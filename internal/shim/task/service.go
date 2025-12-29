// Package task implements the containerd task service for qemubox runtime.
// It orchestrates VM lifecycle, container creation, and I/O streams within the shim.
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
	// eventChannelBuffer is the size of the event channel buffer
	eventChannelBuffer = 128
	// eventStreamShutdownDelay gives the guest time to emit logs before we tear down the VM.
	eventStreamShutdownDelay = 2 * time.Second
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
	// Separate mutexes for different concerns to avoid contention
	containerMu  sync.Mutex // Protects container field
	controllerMu sync.Mutex // Protects hotplug controllers

	// Dependency managers (injected)
	vmLifecycle     *lifecycle.Manager
	networkManager  network.NetworkManager
	platformMounts  platformMounts.Manager
	platformNetwork platformNetwork.Manager

	// Single container (qemubox enforces 1 container per VM per shim)
	container   *container
	containerID string

	// Per-container hotplug controllers (QEMU only)
	cpuHotplugControllers    map[string]cpuhotplug.CPUHotplugController
	memoryHotplugControllers map[string]memhotplug.MemoryHotplugController

	events chan any

	initiateShutdown    func()
	eventsClosed        atomic.Bool
	eventsCloseOnce     sync.Once
	intentionalShutdown atomic.Bool
	deletionInProgress  atomic.Bool
	shutdownSvc         shutdown.Service
	inflight            atomic.Int64
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

// Create a new initial process and container with the underlying OCI runtime.
func (s *service) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	log.G(ctx).WithFields(log.Fields{
		"id":     r.ID,
		"bundle": r.Bundle,
	}).Debug("creating container task")

	if r.Checkpoint != "" || r.ParentCheckpoint != "" {
		return nil, errgrpc.ToGRPC(fmt.Errorf("checkpoints not supported: %w", errdefs.ErrNotImplemented))
	}

	// Check if deletion is in progress
	if s.deletionInProgress.Load() {
		return nil, errgrpc.ToGRPCf(errdefs.ErrAlreadyExists, "shim is deleting container; requires fresh shim per container")
	}

	s.containerMu.Lock()
	hasContainer := s.container != nil
	s.containerMu.Unlock()

	// Check if VM already exists
	if _, err := s.vmLifecycle.Instance(); err == nil || hasContainer {
		return nil, errgrpc.ToGRPCf(errdefs.ErrAlreadyExists, "shim already running a container; requires fresh shim per container")
	}

	// Double-check deletion flag (prevent TOCTOU race)
	if s.deletionInProgress.Load() {
		return nil, errgrpc.ToGRPCf(errdefs.ErrAlreadyExists, "shim is deleting container; requires fresh shim per container")
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

	// Cleanup helper for network resources on failure
	var networkCleanupDone bool
	cleanupNetwork := func() {
		if networkCleanupDone {
			return
		}
		env := &network.Environment{ID: r.ID}
		if err := s.networkManager.ReleaseNetworkResources(ctx, env); err != nil {
			log.G(ctx).WithError(err).WithField("id", r.ID).Warn("failed to cleanup network resources after failure")
		}
		networkCleanupDone = true
	}
	defer func() {
		if !networkCleanupDone {
			cleanupNetwork()
		}
	}()

	// Start VM
	prestart := time.Now()
	startOpts := []vm.StartOpt{
		vm.WithNetworkConfig(netCfg),
		vm.WithNetworkNamespace(netnsPath),
	}
	if err := vmi.Start(ctx, startOpts...); err != nil {
		cleanupNetwork()
		return nil, errgrpc.ToGRPC(err)
	}
	bootTime := time.Since(prestart)
	log.G(ctx).WithField("bootTime", bootTime).Debug("VM started")
	s.intentionalShutdown.Store(false)

	// Get VM client
	vmc, err := s.vmLifecycle.Client()
	if err != nil {
		cleanupNetwork()
		return nil, errgrpc.ToGRPC(err)
	}

	// Start forwarding events
	ns, _ := namespaces.Namespace(ctx)
	eventCtx := namespaces.WithNamespace(context.WithoutCancel(ctx), ns)
	if err := s.startEventForwarder(eventCtx, vmc); err != nil {
		cleanupNetwork()
		return nil, errgrpc.ToGRPC(err)
	}

	// Dial TTRPC client
	rpcClient, err := s.vmLifecycle.DialClient(ctx)
	if err != nil {
		cleanupNetwork()
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := rpcClient.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close rpc client in Create")
		}
	}()

	// Create bundle in VM
	bundleFiles, err := b.Files()
	if err != nil {
		cleanupNetwork()
		return nil, errgrpc.ToGRPC(err)
	}

	bundleService := bundleAPI.NewTTRPCBundleClient(rpcClient)
	br, err := bundleService.Create(ctx, &bundleAPI.CreateRequest{
		ID:    r.ID,
		Files: bundleFiles,
	})
	if err != nil {
		cleanupNetwork()
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
		cleanupNetwork()
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
		cleanupNetwork()
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

	// Mark network cleanup as done since we succeeded
	networkCleanupDone = true

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
	recvLoop:
		for {
			ev, err := sc.Recv()
			if err != nil {
				if s.intentionalShutdown.Load() {
					log.G(ctx).Info("vm event stream closed (intentional shutdown)")
					return
				}

				if errors.Is(err, io.EOF) || errors.Is(err, shutdown.ErrShutdown) || errors.Is(err, ttrpc.ErrClosed) {
					log.G(ctx).WithError(err).Info("vm event stream closed unexpectedly, attempting reconnect")
					reconnectDeadline := time.Now().Add(2 * time.Second)
					for time.Now().Before(reconnectDeadline) {
						if s.intentionalShutdown.Load() {
							log.G(ctx).Info("vm event stream closed (intentional shutdown)")
							return
						}
						newClient, dialErr := s.vmLifecycle.DialClientWithRetry(ctx, 2*time.Second)
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
						if currentClient != nil {
							_ = currentClient.Close()
						}
						currentClient = newClient
						sc = newStream
						log.G(ctx).Info("vm event stream reconnected")
						continue recvLoop
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

// Start a process.
func (s *service) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	vmc, err := s.vmLifecycle.DialClientWithRetry(ctx, 30*time.Second)
	if err != nil {
		log.G(ctx).WithError(err).Error("start: failed to get client")
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in Start")
		}
	}()
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	resp, err := tc.Start(ctx, r)
	if err != nil {
		log.G(ctx).WithError(err).Error("start: guest start failed")
		return nil, err
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
	}

	vmc, err := s.vmLifecycle.DialClient(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in Delete")
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
			log.G(ctx).WithError(err).Warn("failed to close client in Exec")
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

	vmc, err := s.vmLifecycle.DialClient(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in ResizePty")
		}
	}()
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.ResizePty(ctx, r)
}

// State returns runtime state information for a process.
func (s *service) State(ctx context.Context, r *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	vmc, err := s.vmLifecycle.DialClient(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Error("state: failed to get client")
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in State")
		}
	}()
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	st, err := tc.State(ctx, r)
	if err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Error("state: guest state failed")
		return nil, errgrpc.ToGRPC(err)
	}

	return st, nil
}

// Pause the container.
func (s *service) Pause(ctx context.Context, r *taskAPI.PauseRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("pause")

	vmc, err := s.vmLifecycle.DialClient(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in Pause")
		}
	}()
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Pause(ctx, r)
}

// Resume the container.
func (s *service) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("resume")

	vmc, err := s.vmLifecycle.DialClient(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in Resume")
		}
	}()
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Resume(ctx, r)
}

// Kill a process with the provided signal.
func (s *service) Kill(ctx context.Context, r *taskAPI.KillRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Debug("kill")

	vmc, err := s.vmLifecycle.DialClient(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in Kill")
		}
	}()
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Kill(ctx, r)
}

// Pids returns all pids inside the container.
func (s *service) Pids(ctx context.Context, r *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("pids")

	vmc, err := s.vmLifecycle.DialClient(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in Pids")
		}
	}()
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Pids(ctx, r)
}

// CloseIO of a process.
func (s *service) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID, "stdin": r.Stdin}).Debug("close io")

	vmc, err := s.vmLifecycle.DialClient(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in CloseIO")
		}
	}()
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.CloseIO(ctx, r)
}

// Checkpoint the container.
func (s *service) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("checkpoint")
	return &ptypes.Empty{}, nil
}

// Update a running container.
func (s *service) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("update")

	vmc, err := s.vmLifecycle.DialClient(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in Update")
		}
	}()
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Update(ctx, r)
}

// Wait for a process to exit.
func (s *service) Wait(ctx context.Context, r *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Debug("wait")

	vmc, err := s.vmLifecycle.DialClient(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in Wait")
		}
	}()
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Wait(ctx, r)
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

	vmc, err := s.vmLifecycle.DialClient(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in Connect")
		}
	}()

	tc := taskAPI.NewTTRPCTaskClient(vmc)
	vr, err := tc.Connect(ctx, r)
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

	vmc, err := s.vmLifecycle.DialClient(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in Stats")
		}
	}()
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Stats(ctx, r)
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
	for e := range s.events {
		log.G(ctx).WithField("event", e).Error("ignored event after shutdown")
	}
}
