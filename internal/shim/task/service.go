// Package task implements the containerd task service for qemubox runtime.
// It orchestrates VM lifecycle, container creation, and I/O streams within the shim.
//
// # Synchronization Model
//
// The service struct coordinates multiple concurrent operations (container creation,
// exec processes, shutdown, hotplug operations). Thread safety is achieved through
// a combination of mutexes and atomics:
//
// Locks (must be acquired in this order to prevent deadlocks):
//  1. containerMu - Protects container field and containerID
//  2. controllerMu - Protects hotplug controller maps
//
// Lock Usage:
//   - containerMu: Hold when reading/writing container or containerID
//   - controllerMu: Hold when reading/writing hotplug controller maps
//   - NEVER hold both locks simultaneously unless absolutely necessary
//   - NEVER hold locks during slow operations (VM start, network setup, TTRPC calls)
//
// Atomics (lock-free state):
//   - eventsClosed: Set when event channel is closed (shutdown signal)
//   - intentionalShutdown: Distinguishes clean shutdown from VM crash
//   - deletionInProgress: Prevents new Create() calls during Delete()
//   - inflight: Counts in-flight RPC operations for graceful shutdown
//
// Goroutine Ownership:
//   - Main goroutine: Handles TTRPC service calls (Create, Start, Delete, etc.)
//   - Event forwarder: Single goroutine forwards events from VM to containerd
//     (spawned in NewTaskService, runs until events channel is closed)
//   - Hotplug controllers: Each container gets CPU and memory hotplug goroutines
//     (spawned on Create, stopped on Delete or shutdown)
//
// Channel Usage:
//   - events channel (buffered, size 128):
//   - Producers: VM event stream, task service operations
//   - Consumer: Event forwarder goroutine
//   - Closed during shutdown to signal forwarder to exit
//   - Buffer size prevents event loss during brief containerd unavailability
//
// Resource Lifecycle:
//   - VM instance: Created in Create(), started in Create(), stopped in shutdown()
//   - Network resources: Allocated in Create(), released in shutdown()
//   - Hotplug controllers: Started after VM boot, stopped in shutdown()
//   - Event forwarder: Started in NewTaskService(), stopped when events channel closes
//   - I/O streams: Created per container/exec, closed in shutdown()
//
// Shutdown Sequence:
//  1. shutdown() called (via Delete or process exit)
//  2. Lock containerMu and controllerMu
//  3. Stop all hotplug controllers (graceful stop)
//  4. Close all I/O streams (exec processes, then container)
//  5. Shutdown VM (sends SIGTERM to QEMU, waits for exit)
//  6. Release network resources (CNI teardown)
//  7. Close network manager
//  8. Close events channel (signals forwarder to exit)
//
// Concurrency Invariants:
//   - Only one container per service (enforced in Create)
//   - Create() and Delete() never run concurrently (enforced by containerd)
//   - Multiple Exec() calls may run concurrently (each gets unique exec ID)
//   - shutdown() may run concurrently with other operations (uses locks)
//   - Event forwarder runs independently, tolerates slow consumers
package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/runtime"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	ptypes "github.com/containerd/containerd/v2/pkg/protobuf/types"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"

	bundleAPI "github.com/aledbf/qemubox/containerd/api/services/bundle/v1"
	"github.com/aledbf/qemubox/containerd/api/services/vmevents/v1"
	"github.com/aledbf/qemubox/containerd/internal/host/network"
	"github.com/aledbf/qemubox/containerd/internal/host/vm"
	"github.com/aledbf/qemubox/containerd/internal/shim/cpuhotplug"
	"github.com/aledbf/qemubox/containerd/internal/shim/lifecycle"
	"github.com/aledbf/qemubox/containerd/internal/shim/memhotplug"
	platformMounts "github.com/aledbf/qemubox/containerd/internal/shim/platform/mounts"
	platformNetwork "github.com/aledbf/qemubox/containerd/internal/shim/platform/network"
	"github.com/aledbf/qemubox/containerd/internal/shim/resources"
	"github.com/aledbf/qemubox/containerd/internal/shim/transform"
)

const (
	// eventChannelBuffer is the size of the event channel buffer.
	//
	// This buffer prevents event loss when containerd briefly can't consume events
	// (e.g., during GC pauses). Size chosen to handle typical burst scenarios:
	//   - Container exit + log flush: ~10-20 events
	//   - Multiple exec processes exiting: ~5 events per process
	//   - Hotplug operations: ~2-5 events per operation
	//
	// 128 provides headroom for multiple concurrent operations. If this buffer
	// fills, the event producer will block, which is acceptable - it means
	// containerd is genuinely overloaded.
	//
	// NOTE: This is not configurable because event loss would break container
	// lifecycle guarantees. If you need to tune this, you have bigger problems.
	eventChannelBuffer = 128

	// eventStreamShutdownDelay gives the guest time to emit final logs before VM teardown.
	//
	// During shutdown, the VM may have buffered logs that haven't been flushed yet.
	// This delay ensures those logs make it to containerd before we kill the VM.
	//
	// 2 seconds is chosen to handle:
	//   - Typical log buffer flush: ~100-500ms
	//   - Application cleanup handlers: ~500ms-1s
	//   - Guest kernel shutdown logging: ~200-500ms
	//
	// This is pure overhead on every container stop, so it should be as short as
	// possible while still catching most logs. Logs lost after this timeout are
	// considered acceptable data loss (VM is shutting down anyway).
	eventStreamShutdownDelay = 2 * time.Second

	// eventStreamReconnectTimeout is how long we try to reconnect the event stream.
	// 2 seconds allows for brief VM pauses (e.g., during snapshot operations)
	// without triggering shim shutdown, while still detecting genuine VM failures quickly.
	eventStreamReconnectTimeout = 2 * time.Second
)

var (
	_ = shim.TTRPCService(&service{})
)

// NewTaskService creates a new instance of a task service.
func NewTaskService(ctx context.Context, publisher shim.Publisher, sd shutdown.Service) (taskAPI.TTRPCTaskService, error) {
	// Initialize platform managers
	netMgr := platformNetwork.New()
	nm, err := netMgr.InitNetworkManager(ctx)
	if err != nil {
		return nil, fmt.Errorf("initialize network manager: %w", err)
	}

	s := &service{
		events:                   make(chan any, eventChannelBuffer),
		cpuHotplugControllers:    make(map[string]cpuhotplug.CPUHotplugController),
		memoryHotplugControllers: make(map[string]memhotplug.MemoryHotplugController),
		networkManager:           nm,
		vmLifecycle:              lifecycle.NewManager(),
		platformMounts:           platformMounts.New(),
		platformNetwork:          netMgr,
		initiateShutdown:         sd.Shutdown,
		shutdownSvc:              sd,
	}
	sd.RegisterCallback(s.shutdown)

	if address, err := shim.ReadAddress("address"); err == nil {
		sd.RegisterCallback(func(context.Context) error {
			return shim.RemoveSocket(address)
		})
	}

	// Start event forwarding goroutine with lifecycle tracking
	forwardReady := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.G(ctx).WithField("panic", r).Error("event forwarding goroutine panicked")
			}
			log.G(ctx).Info("event forwarding goroutine exited")
		}()
		s.forward(ctx, publisher, forwardReady)
	}()

	// Wait for forwarder to signal ready
	<-forwardReady

	return s, nil
}

type container struct {
	ioShutdown    func(context.Context) error
	execShutdowns map[string]func(context.Context) error
	pid           uint32
}

// service is the shim implementation of a remote shim over TTRPC.
type service struct {
	// === Synchronization Primitives ===
	// LOCK ORDER: Always acquire containerMu before controllerMu if you need both

	containerMu  sync.Mutex // Protects: container, containerID
	controllerMu sync.Mutex // Protects: cpuHotplugControllers, memoryHotplugControllers

	// === Dependency Managers (thread-safe, injected at construction) ===
	vmLifecycle     *lifecycle.Manager      // VM process management (internal locking)
	networkManager  network.NetworkManager  // CNI network operations (internal locking)
	platformMounts  platformMounts.Manager  // Mount configuration (stateless)
	platformNetwork platformNetwork.Manager // Network namespace setup (stateless)

	// === Container State (protected by containerMu) ===
	// qemubox enforces 1 container per VM per shim - Create() rejects if already set
	container   *container // Container metadata and I/O shutdown functions
	containerID string     // Container ID (empty string means no container)

	// === Hotplug Controllers (protected by controllerMu) ===
	// Map key is container ID. Controllers are goroutines that monitor and adjust
	// CPU/memory based on usage. Started after VM boot, stopped during shutdown.
	cpuHotplugControllers    map[string]cpuhotplug.CPUHotplugController
	memoryHotplugControllers map[string]memhotplug.MemoryHotplugController

	// === Event Channel (multi-producer, single-consumer) ===
	// Producers: VM event stream, task operations (Create, Start, Delete, etc.)
	// Consumer: forward() goroutine (started in NewTaskService)
	// Closed during shutdown() to signal forward() to exit
	events chan any

	// === Shutdown Coordination ===
	initiateShutdown    func()      // Callback to trigger shutdown service
	eventsClosed        atomic.Bool // True when events channel is closed
	eventsCloseOnce     sync.Once   // Ensures events channel closed exactly once
	intentionalShutdown atomic.Bool // True for clean shutdown, false for VM crash
	deletionInProgress  atomic.Bool // True during Delete() to reject concurrent Create()
	creationInProgress  atomic.Bool // True during Create() to reject concurrent Create() and Delete()
	shutdownSvc         shutdown.Service
	inflight            atomic.Int64 // Count of in-flight RPC calls for graceful shutdown
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	taskAPI.RegisterTTRPCTaskService(server, s)
	return nil
}

func (s *service) shutdown(ctx context.Context) error {
	// Lock all mutexes in consistent order to prevent deadlocks
	s.containerMu.Lock()
	defer s.containerMu.Unlock()

	var errs []error

	// Stop hotplug controllers before VM shutdown
	s.controllerMu.Lock()
	for id, controller := range s.cpuHotplugControllers {
		controller.Stop()
		delete(s.cpuHotplugControllers, id)
	}
	for id, controller := range s.memoryHotplugControllers {
		controller.Stop()
		delete(s.memoryHotplugControllers, id)
	}
	s.controllerMu.Unlock()

	// Shutdown IO for container and execs
	if s.container != nil {
		if s.container.ioShutdown != nil {
			if err := s.container.ioShutdown(ctx); err != nil {
				errs = append(errs, fmt.Errorf("container %q io shutdown: %w", s.containerID, err))
			}
		}
		for execID, ioShutdown := range s.container.execShutdowns {
			if err := ioShutdown(ctx); err != nil {
				errs = append(errs, fmt.Errorf("container %q exec %q io shutdown: %w", s.containerID, execID, err))
			}
		}
	}

	// Shutdown VM (idempotent)
	// IMPORTANT: This must happen BEFORE network cleanup so QEMU can exit cleanly
	if err := s.vmLifecycle.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("vm shutdown: %w", err))
	}

	// Release network resources AFTER VM shutdown
	if s.containerID != "" {
		env := &network.Environment{ID: s.containerID}
		if err := s.networkManager.ReleaseNetworkResources(ctx, env); err != nil {
			log.G(ctx).WithError(err).WithField("id", s.containerID).Warn("failed to release network resources")
		}
	}

	// Close network manager
	if s.networkManager != nil {
		if err := s.networkManager.Close(); err != nil {
			errs = append(errs, fmt.Errorf("network manager close: %w", err))
		}
	}

	// Stop forwarding events
	s.eventsClosed.Store(true)
	s.eventsCloseOnce.Do(func() {
		close(s.events)
	})

	return errors.Join(errs...)
}

// dialTaskClient dials a TTRPC client and returns it with a cleanup function.
// The cleanup function handles closing the client and logging any errors.
// Callers should use taskAPI.NewTTRPCTaskClient() to create a task client from the returned connection.
func (s *service) dialTaskClient(ctx context.Context) (*ttrpc.Client, func(), error) {
	vmc, err := s.vmLifecycle.DialClient(ctx)
	if err != nil {
		return nil, nil, errgrpc.ToGRPC(err)
	}
	cleanup := func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close ttrpc client")
		}
	}
	return vmc, cleanup, nil
}

// Create a new initial process and container with the underlying OCI runtime.
func (s *service) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (_ *taskAPI.CreateTaskResponse, retErr error) {
	log.G(ctx).WithFields(log.Fields{
		"id":     r.ID,
		"bundle": r.Bundle,
	}).Debug("creating container task")

	if r.Checkpoint != "" || r.ParentCheckpoint != "" {
		return nil, errgrpc.ToGRPC(fmt.Errorf("checkpoints not supported: %w", errdefs.ErrNotImplemented))
	}

	// Atomically mark creation in progress to prevent concurrent Create() or Delete()
	// Uses CompareAndSwap to avoid TOCTOU race between check and VM creation
	if !s.creationInProgress.CompareAndSwap(false, true) {
		return nil, errgrpc.ToGRPCf(errdefs.ErrAlreadyExists, "container creation already in progress")
	}
	defer s.creationInProgress.Store(false)

	// Check if deletion is in progress
	if s.deletionInProgress.Load() {
		return nil, errgrpc.ToGRPCf(errdefs.ErrAlreadyExists, "shim is deleting container; requires fresh shim per container")
	}

	// Check if container already exists
	s.containerMu.Lock()
	hasContainer := s.container != nil
	s.containerMu.Unlock()

	// Check if VM already exists
	if _, err := s.vmLifecycle.Instance(); err == nil || hasContainer {
		return nil, errgrpc.ToGRPCf(errdefs.ErrAlreadyExists, "shim already running a container; requires fresh shim per container")
	}

	presetup := time.Now()

	// Check KVM availability
	if err := lifecycle.CheckKVM(); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// Load and transform bundle
	b, err := transform.LoadForCreate(ctx, r.Bundle)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// Compute resource configuration
	resourceCfg, _ := resources.ComputeConfig(ctx, &b.Spec)

	log.G(ctx).WithFields(log.Fields{
		"boot_cpus":  resourceCfg.BootCPUs,
		"max_cpus":   resourceCfg.MaxCPUs,
		"memory_mb":  resourceCfg.MemorySize / (1024 * 1024),
		"hotplug_mb": resourceCfg.MemoryHotplugSize / (1024 * 1024),
	}).Debug("VM resource configuration")

	// Create VM instance
	vmi, err := s.vmLifecycle.CreateVM(ctx, r.ID, r.Bundle, resourceCfg)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// Setup mounts
	m, err := s.platformMounts.Setup(ctx, vmi, r.ID, r.Rootfs, b.Rootfs, r.Bundle+"/mounts")
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// Setup networking
	netnsPath := "/var/run/netns/" + r.ID
	netCfg, err := s.platformNetwork.Setup(ctx, s.networkManager, vmi, r.ID, netnsPath)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// Cleanup network resources on any error
	defer func() {
		if retErr != nil {
			env := &network.Environment{ID: r.ID}
			if err := s.networkManager.ReleaseNetworkResources(ctx, env); err != nil {
				log.G(ctx).WithError(err).WithField("id", r.ID).Warn("failed to cleanup network resources after failure")
			}
		}
	}()

	// Start VM
	prestart := time.Now()
	startOpts := []vm.StartOpt{
		vm.WithNetworkConfig(netCfg),
		vm.WithNetworkNamespace(netnsPath),
	}
	if err := vmi.Start(ctx, startOpts...); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	bootTime := time.Since(prestart)
	log.G(ctx).WithField("bootTime", bootTime).Debug("VM started")
	s.intentionalShutdown.Store(false)

	// Get VM client
	vmc, err := s.vmLifecycle.Client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// Start forwarding events
	ns, _ := namespaces.Namespace(ctx)
	eventCtx := namespaces.WithNamespace(context.WithoutCancel(ctx), ns)
	if err := s.startEventForwarder(eventCtx, vmc); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// Dial TTRPC client
	rpcClient, err := s.vmLifecycle.DialClient(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := rpcClient.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close ttrpc client")
		}
	}()

	// Create bundle in VM
	bundleFiles, err := b.Files()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	bundleService := bundleAPI.NewTTRPCBundleClient(rpcClient)
	br, err := bundleService.Create(ctx, &bundleAPI.CreateRequest{
		ID:    r.ID,
		Files: bundleFiles,
	})
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// Setup I/O forwarding
	rio := stdio.Stdio{
		Stdin:    r.Stdin,
		Stdout:   r.Stdout,
		Stderr:   r.Stderr,
		Terminal: r.Terminal,
	}

	cio, ioShutdown, err := s.forwardIO(ctx, vmi, rio)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	setupTime := time.Since(presetup)

	// Create task in VM
	vr := &taskAPI.CreateTaskRequest{
		ID:       r.ID,
		Bundle:   br.Bundle,
		Rootfs:   m,
		Terminal: cio.Terminal,
		Stdin:    cio.Stdin,
		Stdout:   cio.Stdout,
		Stderr:   cio.Stderr,
		Options:  r.Options,
	}

	preCreate := time.Now()
	c := &container{
		ioShutdown:    ioShutdown,
		execShutdowns: make(map[string]func(context.Context) error),
	}
	tc := taskAPI.NewTTRPCTaskClient(rpcClient)
	resp, err := tc.Create(ctx, vr)
	if err != nil {
		log.G(ctx).WithError(err).Error("failed to create task")
		if c.ioShutdown != nil {
			if err := c.ioShutdown(ctx); err != nil {
				log.G(ctx).WithError(err).Error("failed to shutdown io after create failure")
			}
		}
		return nil, errgrpc.ToGRPC(err)
	}

	log.G(ctx).WithFields(log.Fields{
		"t_boot":   bootTime,
		"t_setup":  setupTime - bootTime,
		"t_create": time.Since(preCreate),
	}).Info("task successfully created")

	c.pid = resp.Pid
	s.containerMu.Lock()
	s.container = c
	s.containerID = r.ID
	s.containerMu.Unlock()

	// Start hotplug controllers
	callbacks := resources.CreateVMClientCallbacks(s.vmLifecycle.DialClient)
	if cpuCtrl := resources.StartCPUHotplug(ctx, r.ID, vmi, resourceCfg, callbacks); cpuCtrl != nil {
		s.controllerMu.Lock()
		s.cpuHotplugControllers[r.ID] = cpuCtrl
		s.controllerMu.Unlock()
	}
	if memCtrl := resources.StartMemoryHotplug(ctx, r.ID, vmi, resourceCfg, callbacks); memCtrl != nil {
		s.controllerMu.Lock()
		s.memoryHotplugControllers[r.ID] = memCtrl
		s.controllerMu.Unlock()
	}

	return &taskAPI.CreateTaskResponse{
		Pid: resp.Pid,
	}, nil
}

func (s *service) startEventForwarder(ctx context.Context, vmc *ttrpc.Client) error {
	currentClient := vmc
	sc, err := vmevents.NewTTRPCEventsClient(currentClient).Stream(ctx, &ptypes.Empty{})
	if err != nil {
		return err
	}
	go func() {
		for {
			ev, err := sc.Recv()
			if err != nil {
				if s.intentionalShutdown.Load() {
					log.G(ctx).Info("vm event stream closed (intentional shutdown)")
					return
				}

				if errors.Is(err, io.EOF) || errors.Is(err, shutdown.ErrShutdown) || errors.Is(err, ttrpc.ErrClosed) {
					log.G(ctx).WithError(err).Info("vm event stream closed unexpectedly, attempting reconnect")

					// Try to reconnect
					newClient, newStream, reconnected := s.reconnectEventStream(ctx, currentClient)
					if reconnected {
						currentClient = newClient
						sc = newStream
						log.G(ctx).Info("vm event stream reconnected")
						continue // Restart the receive loop with new stream
					}

					log.G(ctx).WithError(err).Info("vm event stream closed unexpectedly, initiating shim shutdown")
				} else {
					log.G(ctx).WithError(err).Error("vm event stream error, initiating shim shutdown")
				}
				if eventStreamShutdownDelay > 0 {
					log.G(ctx).WithField("delay", eventStreamShutdownDelay).Info("delaying shim shutdown after event stream close")
					time.Sleep(eventStreamShutdownDelay)
				}
				s.requestShutdownAndExit(ctx, "vm event stream closed")
				return
			}
			s.send(ev)
		}
	}()

	return nil
}

// reconnectEventStream attempts to reconnect the event stream within a deadline.
// Returns the new client, stream, and whether reconnection succeeded.
func (s *service) reconnectEventStream(ctx context.Context, oldClient *ttrpc.Client) (*ttrpc.Client, vmevents.TTRPCEvents_StreamClient, bool) {
	reconnectDeadline := time.Now().Add(eventStreamReconnectTimeout)

	for time.Now().Before(reconnectDeadline) {
		if s.intentionalShutdown.Load() {
			log.G(ctx).Info("vm event stream reconnect aborted (intentional shutdown)")
			return nil, nil, false
		}

		newClient, dialErr := s.vmLifecycle.DialClientWithRetry(ctx, eventStreamReconnectTimeout)
		if dialErr != nil {
			log.G(ctx).WithError(dialErr).Debug("event stream reconnect: dial failed")
			time.Sleep(200 * time.Millisecond)
			continue
		}

		newStream, streamErr := vmevents.NewTTRPCEventsClient(newClient).Stream(ctx, &ptypes.Empty{})
		if streamErr != nil {
			_ = newClient.Close()
			log.G(ctx).WithError(streamErr).Debug("event stream reconnect: stream failed")
			time.Sleep(200 * time.Millisecond)
			continue
		}

		// Success! Close old client and return new connection
		if oldClient != nil {
			_ = oldClient.Close()
		}
		return newClient, newStream, true
	}

	return nil, nil, false
}

// Start a process.
func (s *service) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	vmc, err := s.vmLifecycle.DialClientWithRetry(ctx, 30*time.Second)
	if err != nil {
		log.G(ctx).WithError(err).Error("start: failed to get client")
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close ttrpc client")
		}
	}()
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	resp, err := tc.Start(ctx, r)
	if err != nil {
		log.G(ctx).WithError(err).Error("start: guest start failed")
		return nil, errgrpc.ToGRPC(err)
	}
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "pid": resp.Pid}).Debug("task started")
	return resp, nil
}

func isProcessAlreadyFinished(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrProcessDone) {
		return true
	}
	return strings.Contains(err.Error(), "process already finished")
}

// Delete the initial process and container.
func (s *service) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	s.inflight.Add(1)
	defer s.inflight.Add(-1)
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Debug("deleting task")

	// Mark deletion in progress (only for container, not exec)
	if r.ExecID == "" {
		if !s.deletionInProgress.CompareAndSwap(false, true) {
			return nil, errgrpc.ToGRPCf(errdefs.ErrAlreadyExists, "delete already in progress")
		}

		// Check if creation is in progress - fail fast to avoid race
		if s.creationInProgress.Load() {
			s.deletionInProgress.Store(false) // Reset deletion flag
			return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "cannot delete while container creation is in progress")
		}
	}

	vmc, err := s.vmLifecycle.DialClient(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close ttrpc client")
		}
	}()

	tc := taskAPI.NewTTRPCTaskClient(vmc)
	resp, err := tc.Delete(ctx, r)
	if err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Warn("delete task failed")
		if r.ExecID == "" {
			s.containerMu.Lock()
			if s.container != nil && s.container.ioShutdown != nil {
				if ioErr := s.container.ioShutdown(ctx); ioErr != nil {
					log.G(ctx).WithError(ioErr).Warn("failed to shutdown io after delete failure")
				}
			}
			s.containerMu.Unlock()

			s.intentionalShutdown.Store(true)
			if shutdownErr := s.vmLifecycle.Shutdown(ctx); shutdownErr != nil {
				if !isProcessAlreadyFinished(shutdownErr) {
					log.G(ctx).WithError(shutdownErr).Warn("failed to shutdown VM after delete error")
				}
			}
		}
		return resp, err
	}

	// Extract cleanup targets
	var ioShutdowns []func(context.Context) error
	var cpuController cpuhotplug.CPUHotplugController
	var memController memhotplug.MemoryHotplugController
	var needNetworkClean, needVMShutdown bool

	s.containerMu.Lock()
	if s.container != nil && s.containerID == r.ID {
		if r.ExecID != "" {
			if ioShutdown, ok := s.container.execShutdowns[r.ExecID]; ok {
				ioShutdowns = append(ioShutdowns, ioShutdown)
				delete(s.container.execShutdowns, r.ExecID)
			}
		} else {
			if s.container.ioShutdown != nil {
				ioShutdowns = append(ioShutdowns, s.container.ioShutdown)
			}
			for _, ioShutdown := range s.container.execShutdowns {
				ioShutdowns = append(ioShutdowns, ioShutdown)
			}
			needNetworkClean = true
			needVMShutdown = true
			s.container = nil
			s.containerID = ""
		}
	}
	s.containerMu.Unlock()

	// Get controllers (only for container, not exec)
	if r.ExecID == "" {
		s.controllerMu.Lock()
		cpuController = s.cpuHotplugControllers[r.ID]
		delete(s.cpuHotplugControllers, r.ID)
		memController = s.memoryHotplugControllers[r.ID]
		delete(s.memoryHotplugControllers, r.ID)
		s.controllerMu.Unlock()
	}

	// Execute cleanup operations
	for i, ioShutdown := range ioShutdowns {
		if err := ioShutdown(ctx); err != nil {
			if i == 0 && r.ExecID == "" {
				log.G(ctx).WithError(err).Error("failed to shutdown io after delete")
			} else {
				log.G(ctx).WithError(err).WithField("exec", r.ExecID).Error("failed to shutdown exec io after delete")
			}
		}
	}

	if cpuController != nil {
		cpuController.Stop()
	}
	if memController != nil {
		memController.Stop()
	}

	// Shutdown VM (BEFORE network cleanup)
	if needVMShutdown {
		log.G(ctx).Info("container deleted, shutting down VM")
		s.intentionalShutdown.Store(true)
		if err := s.vmLifecycle.Shutdown(ctx); err != nil {
			if !isProcessAlreadyFinished(err) {
				log.G(ctx).WithError(err).Warn("failed to shutdown VM after container deleted")
			}
		}
		log.G(ctx).Info("VM state cleared, scheduling shim exit")

		go s.requestShutdownAndExit(ctx, "container deleted")
	}

	// Release network resources AFTER VM shutdown
	if needNetworkClean {
		env := &network.Environment{ID: r.ID}
		if err := s.networkManager.ReleaseNetworkResources(ctx, env); err != nil {
			log.G(ctx).WithError(err).WithField("id", r.ID).Warn("failed to release network resources during delete")
		}
	}

	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Debug("task deleted successfully")
	return resp, nil
}

// Exec an additional process inside the container.
func (s *service) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Debug("exec container")

	vmc, err := s.vmLifecycle.DialClient(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close ttrpc client")
		}
	}()

	vmi, err := s.vmLifecycle.Instance()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	rio := stdio.Stdio{
		Stdin:    r.Stdin,
		Stdout:   r.Stdout,
		Stderr:   r.Stderr,
		Terminal: r.Terminal,
	}

	cio, ioShutdown, err := s.forwardIO(ctx, vmi, rio)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	s.containerMu.Lock()
	if s.container != nil && s.containerID == r.ID {
		s.container.execShutdowns[r.ExecID] = ioShutdown
	} else {
		if ioShutdown != nil {
			if err := ioShutdown(ctx); err != nil {
				log.G(ctx).WithError(err).Error("failed to shutdown exec io after container not found")
			}
		}
		s.containerMu.Unlock()
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "container %q not found", r.ID)
	}
	s.containerMu.Unlock()

	vr := &taskAPI.ExecProcessRequest{
		ID:       r.ID,
		ExecID:   r.ExecID,
		Terminal: cio.Terminal,
		Stdin:    cio.Stdin,
		Stdout:   cio.Stdout,
		Stderr:   cio.Stderr,
		Spec:     r.Spec,
	}

	resp, err := taskAPI.NewTTRPCTaskClient(vmc).Exec(ctx, vr)

	if err != nil {
		s.containerMu.Lock()
		if s.container != nil && s.containerID == r.ID {
			if ioShutdown, ok := s.container.execShutdowns[r.ExecID]; ok {
				if err := ioShutdown(ctx); err != nil {
					log.G(ctx).WithError(err).Error("failed to shutdown exec io after exec failure")
				}
				delete(s.container.execShutdowns, r.ExecID)
			}
		}
		s.containerMu.Unlock()
		return nil, errgrpc.ToGRPC(err)
	}

	return resp, nil
}

// ResizePty of a process.
func (s *service) ResizePty(ctx context.Context, r *taskAPI.ResizePtyRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Debug("resize pty")
	vmc, cleanup, err := s.dialTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return taskAPI.NewTTRPCTaskClient(vmc).ResizePty(ctx, r)
}

// State returns runtime state information for a process.
func (s *service) State(ctx context.Context, r *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	vmc, cleanup, err := s.dialTaskClient(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Error("state: failed to get client")
		return nil, err
	}
	defer cleanup()
	st, err := taskAPI.NewTTRPCTaskClient(vmc).State(ctx, r)
	if err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Error("state: guest state failed")
		return nil, errgrpc.ToGRPC(err)
	}
	return st, nil
}

// Pause the container.
func (s *service) Pause(ctx context.Context, r *taskAPI.PauseRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("pause")
	vmc, cleanup, err := s.dialTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return taskAPI.NewTTRPCTaskClient(vmc).Pause(ctx, r)
}

// Resume the container.
func (s *service) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("resume")
	vmc, cleanup, err := s.dialTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return taskAPI.NewTTRPCTaskClient(vmc).Resume(ctx, r)
}

// Kill a process with the provided signal.
func (s *service) Kill(ctx context.Context, r *taskAPI.KillRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Debug("kill")
	vmc, cleanup, err := s.dialTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return taskAPI.NewTTRPCTaskClient(vmc).Kill(ctx, r)
}

// Pids returns all pids inside the container.
func (s *service) Pids(ctx context.Context, r *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("pids")
	vmc, cleanup, err := s.dialTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return taskAPI.NewTTRPCTaskClient(vmc).Pids(ctx, r)
}

// CloseIO of a process.
func (s *service) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID, "stdin": r.Stdin}).Debug("close io")
	vmc, cleanup, err := s.dialTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return taskAPI.NewTTRPCTaskClient(vmc).CloseIO(ctx, r)
}

// Checkpoint the container.
func (s *service) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("checkpoint")
	return &ptypes.Empty{}, nil
}

// Update a running container.
func (s *service) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("update")
	vmc, cleanup, err := s.dialTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return taskAPI.NewTTRPCTaskClient(vmc).Update(ctx, r)
}

// Wait for a process to exit.
func (s *service) Wait(ctx context.Context, r *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Debug("wait")
	vmc, cleanup, err := s.dialTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return taskAPI.NewTTRPCTaskClient(vmc).Wait(ctx, r)
}

// Connect returns shim information such as the shim's pid.
func (s *service) Connect(ctx context.Context, r *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("connect")
	s.containerMu.Lock()
	hasContainer := s.container != nil && s.containerID == r.ID
	pid := uint32(0)
	if hasContainer {
		pid = s.container.pid
	}
	s.containerMu.Unlock()
	if hasContainer && pid != 0 {
		return &taskAPI.ConnectResponse{
			ShimPid: uint32(os.Getpid()),
			TaskPid: pid,
		}, nil
	}

	vmc, cleanup, err := s.dialTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	vr, err := taskAPI.NewTTRPCTaskClient(vmc).Connect(ctx, r)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	return &taskAPI.ConnectResponse{
		ShimPid: uint32(os.Getpid()),
		TaskPid: vr.TaskPid,
	}, nil
}

func (s *service) Shutdown(ctx context.Context, r *taskAPI.ShutdownRequest) (*ptypes.Empty, error) {
	s.inflight.Add(1)
	defer s.inflight.Add(-1)
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("shutdown")

	s.intentionalShutdown.Store(true)

	if s.shutdownSvc != nil {
		go s.requestShutdownAndExit(ctx, "shutdown rpc")
	} else if s.initiateShutdown != nil {
		s.initiateShutdown()
		s.initiateShutdown = nil
	}

	return &ptypes.Empty{}, nil
}

func (s *service) Stats(ctx context.Context, r *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("stats")
	vmc, cleanup, err := s.dialTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return taskAPI.NewTTRPCTaskClient(vmc).Stats(ctx, r)
}

func (s *service) send(evt interface{}) {
	if s.eventsClosed.Load() {
		return
	}
	s.events <- evt
}

func (s *service) requestShutdownAndExit(ctx context.Context, reason string) {
	log.G(ctx).WithField("reason", reason).Info("shim shutdown requested")
	if s.shutdownSvc == nil {
		log.G(ctx).WithField("reason", reason).Warn("shutdown service missing; exiting immediately")
		os.Exit(0)
	}

	s.shutdownSvc.Shutdown()
	deadline := time.Now().Add(5 * time.Second)
	for s.inflight.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if s.inflight.Load() > 0 {
		log.G(ctx).WithFields(log.Fields{
			"reason":   reason,
			"inflight": s.inflight.Load(),
		}).Warn("shutdown waiting for in-flight requests timed out")
	}
	select {
	case <-s.shutdownSvc.Done():
	case <-time.After(5 * time.Second):
		log.G(ctx).WithField("reason", reason).Warn("shutdown timeout; exiting anyway")
	}

	log.G(ctx).WithField("reason", reason).Info("exiting shim after shutdown")
	os.Exit(0)
}

func (s *service) forward(ctx context.Context, publisher shim.Publisher, ready chan<- struct{}) {
	ns, _ := namespaces.Namespace(ctx)
	ctx = namespaces.WithNamespace(context.WithoutCancel(ctx), ns)

	close(ready)

	for e := range s.events {
		switch e := e.(type) {
		case *types.Envelope:
			if err := publisher.Publish(ctx, e.Topic, e.Event); err != nil {
				log.G(ctx).WithError(err).Error("forward event")
			}
		default:
			err := publisher.Publish(ctx, runtime.GetTopic(e), e)
			if err != nil {
				log.G(ctx).WithError(err).Error("post event")
			}
		}
	}
	if err := publisher.Close(); err != nil {
		log.G(ctx).WithError(err).Error("close event publisher")
	}
}
