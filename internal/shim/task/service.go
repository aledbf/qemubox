//go:build linux

// Package task implements the containerd task service for spinbox runtime.
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
// State Machine (lifecycle/StateMachine):
//   - Manages shim state transitions: Idle -> Creating -> Running -> Deleting -> ShuttingDown
//   - Replaces scattered atomics: creationInProgress, deletionInProgress, intentionalShutdown
//   - Enforces valid state transitions and prevents race conditions
//
// Atomics (lock-free state):
//   - eventsClosed: Set when event channel is closed (shutdown signal)
//   - inflight: Counts in-flight RPC operations for graceful shutdown
//   - initStarted: Tracks if init process has been started
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
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"time"

	eventstypes "github.com/containerd/containerd/api/events"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types"
	tasktypes "github.com/containerd/containerd/api/types/task"
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
	"github.com/containerd/typeurl/v2"

	systemapi "github.com/spin-stack/spinbox/api/spinbox/services/system/v1"
	"github.com/spin-stack/spinbox/api/spinbox/services/vmevents/v1"
	"github.com/spin-stack/spinbox/internal/host/network"
	"github.com/spin-stack/spinbox/internal/shim/cpuhotplug"
	"github.com/spin-stack/spinbox/internal/shim/lifecycle"
	"github.com/spin-stack/spinbox/internal/shim/memhotplug"
	platformMounts "github.com/spin-stack/spinbox/internal/shim/platform/mounts"
	platformNetwork "github.com/spin-stack/spinbox/internal/shim/platform/network"
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
	// taskClientRetryTimeout is how long we retry vsock dial for unary task RPCs.
	// This smooths over transient vsock routing issues (e.g., CID reuse).
	taskClientRetryTimeout = 1 * time.Second

	defaultNamespace = "default"
)

var (
	_ = shim.TTRPCService(&service{})
)

// NewTaskService creates a new instance of a task service.
// containerdAddress is the gRPC address for connecting back to containerd,
// used for updating container labels with VM runtime metadata.
func NewTaskService(ctx context.Context, publisher shim.Publisher, sd shutdown.Service, containerdAddress string) (taskAPI.TTRPCTaskService, error) {
	// Initialize platform managers
	netMgr := platformNetwork.New()
	nm, err := netMgr.InitNetworkManager(ctx)
	if err != nil {
		return nil, fmt.Errorf("initialize network manager: %w", err)
	}

	vmLM := lifecycle.NewManager()
	s := &service{
		stateMachine:             lifecycle.NewStateMachine(),
		events:                   make(chan any, eventChannelBuffer),
		eventsDone:               make(chan struct{}),
		forwardDone:              make(chan struct{}),
		vmExitCh:                 make(chan struct{}),
		cpuHotplugControllers:    make(map[string]cpuhotplug.CPUHotplugController),
		memoryHotplugControllers: make(map[string]memhotplug.MemoryHotplugController),
		networkManager:           nm,
		vmLifecycle:              vmLM,
		platformMounts:           platformMounts.New(),
		platformNetwork:          netMgr,
		initiateShutdown:         sd.Shutdown,
		shutdownSvc:              sd,
		connManager:              NewConnectionManager(vmLM.DialClient, vmLM.DialClientWithRetry),
		containerdAddress:        containerdAddress,
	}
	sd.RegisterCallback(s.shutdown)

	// Register socket cleanup on shutdown if we can determine the socket address.
	// We first try to read from the address file (legacy path), then fall back
	// to computing the address from the containerd address if provided.
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
	pid uint32

	// I/O state for init and exec processes.
	io *taskIO
	// mountCleanup releases host-side mount manager state.
	mountCleanup func(context.Context) error
}

type execIO struct {
	stdin  string
	stdout string
	stderr string
}

type processIOState struct {
	host      execIO
	terminal  bool
	forwarder IOForwarder
}

type taskIO struct {
	init processIOState
	exec map[string]processIOState
}

// containerSnapshot holds immutable copies of container state for use outside locks.
// This enables the "collect-then-execute" pattern: acquire lock, copy data, release lock,
// then operate on the snapshot without risk of data races.
type containerSnapshot struct {
	id     string
	pid    uint32
	initIO *ioSnapshot
	execIO map[string]*ioSnapshot
}

// ioSnapshot holds immutable copies of I/O paths for a process.
type ioSnapshot struct {
	stdin, stdout, stderr string
	terminal              bool
}

// snapshotContainer creates a snapshot of the current container state.
// Must be called with containerMu held.
func (s *service) snapshotContainerLocked() *containerSnapshot {
	if s.container == nil {
		return nil
	}

	snap := &containerSnapshot{
		id:  s.containerID,
		pid: s.container.pid,
	}

	if s.container.io != nil {
		snap.initIO = &ioSnapshot{
			stdin:    s.container.io.init.host.stdin,
			stdout:   s.container.io.init.host.stdout,
			stderr:   s.container.io.init.host.stderr,
			terminal: s.container.io.init.terminal,
		}
		if len(s.container.io.exec) > 0 {
			snap.execIO = make(map[string]*ioSnapshot, len(s.container.io.exec))
			for execID, pio := range s.container.io.exec {
				snap.execIO[execID] = &ioSnapshot{
					stdin:  pio.host.stdin,
					stdout: pio.host.stdout,
					stderr: pio.host.stderr,
				}
			}
		}
	}

	return snap
}

// getContainerSnapshot returns a snapshot of the container state.
// Returns nil if no container exists.
func (s *service) getContainerSnapshot() *containerSnapshot {
	s.containerMu.Lock()
	defer s.containerMu.Unlock()
	return s.snapshotContainerLocked()
}

// service is the shim implementation of a remote shim over TTRPC.
type service struct {
	// === State Machine ===
	// Manages shim lifecycle state transitions (Idle -> Creating -> Running -> Deleting -> ShuttingDown)
	// Replaces scattered atomics: creationInProgress, deletionInProgress, intentionalShutdown
	stateMachine *lifecycle.StateMachine

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
	// spinbox enforces 1 container per VM per shim - Create() rejects if already set
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
	initiateShutdown  func()        // Callback to trigger shutdown service
	eventsClosed      atomic.Bool   // True when events channel is closed
	eventsDone        chan struct{} // Closed before events channel to signal send() to stop
	eventsCloseOnce   sync.Once     // Ensures events channel closed exactly once
	forwardDone       chan struct{} // Closed when the event forwarder goroutine exits
	shutdownOnce      sync.Once     // Ensures shutdown/exit sequencing runs once
	shutdownRequested atomic.Bool
	shutdownSvc       shutdown.Service
	inflight          atomic.Int64   // Count of in-flight RPC calls for graceful shutdown
	exitFunc          func(code int) // Exit function (default: os.Exit), injectable for testing

	// paused is set while the VM is suspended via QMP stop (Pause). While paused
	// the whole VM is frozen, including vminitd, so the guest cannot answer State
	// queries; State short-circuits to PAUSED using cached process state.
	paused atomic.Bool

	// === VM Exit Watchdog ===
	// vmExitCh is closed when the QEMU process exits.
	// This provides a reliable signal for event stream goroutine to exit
	// even when vsock doesn't return EOF on VM death.
	vmExitCh   chan struct{}
	vmExitOnce sync.Once

	// Note: initStarted atomic has been replaced by the state machine.
	// Use s.stateMachine.IsCreatedNotStarted() to check if Start() hasn't been called yet.

	connManager *ConnectionManager

	// === Containerd Communication ===
	// containerdAddress is the gRPC address for connecting back to containerd.
	// Used for updating container labels with VM runtime metadata (CID, IP, etc.)
	// after VM creation. Passed from plugin initialization.
	containerdAddress string
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	taskAPI.RegisterTTRPCTaskService(server, s)
	return nil
}

func (s *service) isShuttingDown() bool {
	return s.shutdownRequested.Load() || s.stateMachine.IsShuttingDown()
}

func (s *service) beginRPC(allowDuringShutdown bool) (func(), error) {
	if !allowDuringShutdown && s.isShuttingDown() {
		return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "shim is shutting down")
	}

	s.inflight.Add(1)
	if !allowDuringShutdown && s.isShuttingDown() {
		s.inflight.Add(-1)
		return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "shim is shutting down")
	}

	return func() {
		s.inflight.Add(-1)
	}, nil
}

func (s *service) shutdown(ctx context.Context) error {
	s.shutdownRequested.Store(true)

	// Transition to ShuttingDown state
	s.stateMachine.ForceTransition(lifecycle.StateShuttingDown)

	// Get container ID before cleanup (briefly hold lock)
	s.containerMu.Lock()
	containerID := s.containerID
	s.containerMu.Unlock()

	// Build and execute cleanup using the orchestrator
	// This ensures proper ordering: hotplug -> io -> connection -> vm -> network -> mounts -> events
	phases := s.buildCleanupPhases(containerID)
	orchestrator := lifecycle.NewCleanupOrchestrator(phases)
	result := orchestrator.Execute(ctx)

	// Note: No need to reset initStarted - state machine already transitioned to ShuttingDown

	// Close network manager (separate from ReleaseNetworkResources)
	if s.networkManager != nil {
		if err := s.networkManager.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close network manager")
		}
	}

	return result.AsError()
}

// =============================================================================
// Cleanup Helper Methods (collect-then-execute pattern)
// =============================================================================
//
// These methods follow the collect-then-execute pattern to avoid holding locks
// during slow operations. Each method:
// 1. Acquires lock briefly to extract resources
// 2. Releases lock immediately
// 3. Performs slow cleanup operations outside the lock
//
// This prevents deadlocks and reduces lock contention during shutdown.

// stopAllHotplugControllers stops all CPU and memory hotplug controllers.
// It collects controllers under lock and stops them outside the lock.
func (s *service) stopAllHotplugControllers(_ context.Context) error {
	// Collect controllers under lock
	s.controllerMu.Lock()
	cpuControllers := make([]cpuhotplug.CPUHotplugController, 0, len(s.cpuHotplugControllers))
	memControllers := make([]memhotplug.MemoryHotplugController, 0, len(s.memoryHotplugControllers))
	for id, c := range s.cpuHotplugControllers {
		cpuControllers = append(cpuControllers, c)
		delete(s.cpuHotplugControllers, id)
	}
	for id, c := range s.memoryHotplugControllers {
		memControllers = append(memControllers, c)
		delete(s.memoryHotplugControllers, id)
	}
	s.controllerMu.Unlock()

	// Stop controllers outside lock (non-blocking, just signals goroutines to exit)
	for _, c := range cpuControllers {
		c.Stop()
	}
	for _, c := range memControllers {
		c.Stop()
	}
	return nil
}

// shutdownAllIO shuts down all I/O forwarders for the container and execs.
// It collects forwarders under lock and shuts them down outside the lock.
func (s *service) shutdownAllIO(ctx context.Context) error {
	// Collect forwarders under lock
	s.containerMu.Lock()
	var forwarders []IOForwarder
	if s.container != nil && s.container.io != nil {
		forwarders = append(forwarders, s.container.io.init.forwarder)
		for _, pio := range s.container.io.exec {
			forwarders = append(forwarders, pio.forwarder)
		}
	}
	s.containerMu.Unlock()

	// Shutdown forwarders outside lock
	var errs []error
	for _, f := range forwarders {
		if err := f.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// extractMountCleanup extracts the mount cleanup function under lock.
// Returns nil if no cleanup function is registered.
func (s *service) extractMountCleanup() func(context.Context) error {
	s.containerMu.Lock()
	defer s.containerMu.Unlock()

	if s.container == nil {
		return nil
	}
	cleanup := s.container.mountCleanup
	s.container.mountCleanup = nil
	return cleanup
}

// closeEvents safely closes the events channel.
// This signals the event forwarder goroutine to exit.
// The eventsDone channel is closed first to signal send() to stop before
// the events channel is closed, preventing the send-on-closed-channel race.
func (s *service) closeEvents() error {
	s.eventsCloseOnce.Do(func() {
		s.eventsClosed.Store(true)
		close(s.eventsDone) // Signal send() first
		close(s.events)     // Then close the channel
	})
	return nil
}

// buildCleanupPhases constructs the cleanup phases for the CleanupOrchestrator.
// The containerID is used for network cleanup.
func (s *service) buildCleanupPhases(containerID string) lifecycle.CleanupPhases {
	mountCleanup := s.extractMountCleanup()

	return lifecycle.CleanupPhases{
		HotplugStop: s.stopAllHotplugControllers,
		IOShutdown:  s.shutdownAllIO,
		ConnClose: func(ctx context.Context) error {
			return s.connManager.Close()
		},
		VMShutdown: func(ctx context.Context) error {
			return s.vmLifecycle.Shutdown(ctx)
		},
		NetworkCleanup: func(ctx context.Context) error {
			if containerID == "" {
				return nil
			}
			env := &network.Environment{ID: containerID}
			return s.networkManager.ReleaseNetworkResources(ctx, env)
		},
		MountCleanup: func(ctx context.Context) error {
			if mountCleanup == nil {
				return nil
			}
			return mountCleanup(ctx)
		},
		EventClose: func(_ context.Context) error {
			return s.closeEvents()
		},
	}
}

// getTaskClient returns the cached TTRPC client for task RPC calls.
//
// The ConnectionManager maintains a single cached connection for all unary task RPCs
// (State, Start, Delete, etc.), avoiding repeated vsock dials that can fail with ENODEV.
//
// The cached client from vmLifecycle.Client() is dedicated to the event stream.
// Sharing a TTRPC connection between streaming RPCs (event stream) and unary RPCs
// causes vsock write errors ("no such device") because the vsock layer doesn't
// handle mixed traffic well on a single connection.
//
// The returned cleanup function is a no-op - the ConnectionManager owns the client lifecycle.
func (s *service) getTaskClient(ctx context.Context) (*ttrpc.Client, func(), error) {
	vmc, err := s.connManager.GetClient(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Error("getTaskClient: failed to get client from connection manager")
		return nil, nil, errgrpc.ToGRPC(err)
	}
	return vmc, func() {}, nil
}

// eventRecvResult holds the result of an event stream Recv() call.
type eventRecvResult struct {
	event *types.Envelope
	err   error
}

func (s *service) startEventForwarder(ctx context.Context, vmc *ttrpc.Client) error {
	currentClient := vmc
	sc, err := vmevents.NewTTRPCEventsServiceClient(currentClient).Stream(ctx, &vmevents.StreamRequest{})
	if err != nil {
		return err
	}

	// Create a channel for receiving events from the blocking Recv() call.
	// This allows us to use select with vmExitCh for reliable shutdown detection.
	recvCh := make(chan eventRecvResult, 1)

	// Start a goroutine to read from the stream and send results to recvCh.
	// The stream carries a StreamResponse wrapping the envelope; everything
	// downstream of here wants the envelope, so it is unwrapped once, here.
	startRecvGoroutine := func(stream vmevents.TTRPCEventsService_StreamClient) {
		go func() {
			resp, err := stream.Recv()
			if err != nil {
				recvCh <- eventRecvResult{err: err}
				return
			}
			recvCh <- eventRecvResult{event: resp.GetEnvelope()}
		}()
	}

	go func() {
		// Start the first receive
		startRecvGoroutine(sc)

		for {
			select {
			case <-s.vmExitCh:
				// VM process exited - this is the reliable signal for VM death.
				// Even if vsock doesn't return EOF, we know the VM is gone.
				log.G(ctx).Info("vm process exit detected, initiating shim shutdown")
				s.requestShutdownAndExit(ctx, "vm process exit")
				return

			case result := <-recvCh:
				if result.err != nil {
					// Check intentional shutdown first to avoid spurious warnings during normal shutdown
					if s.stateMachine.IsIntentionalShutdown() {
						log.G(ctx).Debug("vm event stream closed (intentional shutdown)")
						return
					}

					log.G(ctx).WithError(result.err).WithFields(log.Fields{
						"error_type":      fmt.Sprintf("%T", result.err),
						"is_eof":          errors.Is(result.err, io.EOF),
						"is_shutdown":     errors.Is(result.err, shutdown.ErrShutdown),
						"is_ttrpc_closed": errors.Is(result.err, ttrpc.ErrClosed),
					}).Warn("event stream recv error")

					if errors.Is(result.err, io.EOF) || errors.Is(result.err, shutdown.ErrShutdown) || errors.Is(result.err, ttrpc.ErrClosed) {
						log.G(ctx).WithError(result.err).Info("vm event stream closed unexpectedly, attempting reconnect")

						// Try to reconnect
						newClient, newStream, reconnected := s.reconnectEventStream(ctx, currentClient)
						if reconnected {
							currentClient = newClient
							sc = newStream
							log.G(ctx).Info("vm event stream reconnected")
							startRecvGoroutine(sc) // Start receiving on the new stream
							continue               // Continue the select loop
						}

						log.G(ctx).WithError(result.err).Info("vm event stream closed unexpectedly, initiating shim shutdown")
					} else {
						log.G(ctx).WithError(result.err).Error("vm event stream error, initiating shim shutdown")
					}
					if eventStreamShutdownDelay > 0 {
						log.G(ctx).WithField("delay", eventStreamShutdownDelay).Info("delaying shim shutdown after event stream close")
						time.Sleep(eventStreamShutdownDelay)
					}
					s.requestShutdownAndExit(ctx, "vm event stream closed")
					return
				}

				// A message with no envelope carries nothing to forward. The guest
				// never sends one; a future field added beside it could.
				if result.event == nil {
					startRecvGoroutine(sc)
					continue
				}

				// For TaskExit events, wait for I/O forwarder to complete before forwarding.
				// This ensures all stdout/stderr data is written to FIFOs before containerd
				// receives the exit event, preventing a race where the exit arrives before output.
				if result.event.Topic == runtime.TaskExitEventTopic {
					s.waitForIOBeforeExit(ctx, result.event)
				}

				s.send(result.event)

				// Start the next receive
				startRecvGoroutine(sc)
			}
		}
	}()

	return nil
}

// reconnectEventStream attempts to reconnect the event stream within a deadline.
// Uses exponential backoff with jitter to avoid thundering herd on reconnection.
// Returns the new client, stream, and whether reconnection succeeded.
// Note: The caller is responsible for closing the old client if needed. We don't close
// it here because it might be the cached client from the VM instance, which is shared.
func (s *service) reconnectEventStream(ctx context.Context, oldClient *ttrpc.Client) (*ttrpc.Client, vmevents.TTRPCEventsService_StreamClient, bool) {
	const (
		initialBackoff = 50 * time.Millisecond
		maxBackoff     = 500 * time.Millisecond
		jitterFraction = 0.2 // 20% jitter
	)

	reconnectDeadline := time.Now().Add(eventStreamReconnectTimeout)
	backoff := initialBackoff

	for time.Now().Before(reconnectDeadline) {
		if s.stateMachine.IsIntentionalShutdown() {
			log.G(ctx).Info("vm event stream reconnect aborted (intentional shutdown)")
			return nil, nil, false
		}

		newClient, dialErr := s.vmLifecycle.DialClientWithRetry(ctx, eventStreamReconnectTimeout)
		if dialErr != nil {
			log.G(ctx).WithError(dialErr).Debug("event stream reconnect: dial failed")
			sleepWithJitter(backoff, jitterFraction)
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		newStream, streamErr := vmevents.NewTTRPCEventsServiceClient(newClient).Stream(ctx, &vmevents.StreamRequest{})
		if streamErr != nil {
			_ = newClient.Close()
			log.G(ctx).WithError(streamErr).Debug("event stream reconnect: stream failed")
			sleepWithJitter(backoff, jitterFraction)
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		// Success! Don't close the old client here - it might be the cached client
		// from the VM instance which is shared. The event forwarder will track
		// which clients it owns and can close them when appropriate.
		log.G(ctx).Info("event stream reconnect: success")
		return newClient, newStream, true
	}

	log.G(ctx).WithField("timeout", eventStreamReconnectTimeout).Warn("event stream reconnect: timeout expired")
	return nil, nil, false
}

// sleepWithJitter sleeps for the base duration plus/minus a random jitter.
// jitterFraction is the maximum fraction of base to add/subtract (e.g., 0.2 = ±20%).
func sleepWithJitter(base time.Duration, jitterFraction float64) {
	// #nosec G404 -- weak RNG is acceptable for jitter timing (not security-critical)
	jitter := time.Duration(float64(base) * jitterFraction * (2*rand.Float64() - 1))
	time.Sleep(base + jitter)
}

// Start a process.
func (s *service) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	done, err := s.beginRPC(false)
	if err != nil {
		return nil, err
	}
	defer done()

	startTime := time.Now()
	log.G(ctx).WithFields(log.Fields{
		"id":                   r.ID,
		fieldExec:              r.ExecID,
		"has_cached_client":    s.connManager.HasClient(),
		"intentional_shutdown": s.stateMachine.IsIntentionalShutdown(),
	}).Info("start: request received")

	clientStart := time.Now()
	vmc, cleanup, err := s.getTaskClient(ctx)
	clientDuration := time.Since(clientStart)
	if err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{
			"client_duration": clientDuration,
			"total_duration":  time.Since(startTime),
		}).Error("start: failed to get client")
		return nil, err
	}
	defer cleanup()

	log.G(ctx).WithFields(log.Fields{
		"id":              r.ID,
		fieldExec:         r.ExecID,
		"client_duration": clientDuration,
	}).Info("start: got client, calling guest")

	rpcStart := time.Now()
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	resp, err := tc.Start(ctx, r)
	rpcDuration := time.Since(rpcStart)
	if err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{
			"id":             r.ID,
			fieldExec:        r.ExecID,
			"rpc_duration":   rpcDuration,
			"total_duration": time.Since(startTime),
		}).Error("start: guest start failed")
		return nil, errgrpc.ToGRPC(err)
	}
	log.G(ctx).WithFields(log.Fields{
		"id":             r.ID,
		fieldExec:        r.ExecID,
		"pid":            resp.Pid,
		"rpc_duration":   rpcDuration,
		"total_duration": time.Since(startTime),
	}).Info("task start completed")
	if r.ExecID == "" {
		// Transition from Created to Running state.
		// This replaces the previous initStarted atomic.
		if err := s.stateMachine.MarkStarted(); err != nil {
			log.G(ctx).WithError(err).Warn("start: failed to transition to running state")
		} else {
			log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("start: state transitioned to running")
		}
	}
	return resp, nil
}

type deleteCleanup struct {
	ioForwarders     []IOForwarder
	cpuController    cpuhotplug.CPUHotplugController
	memController    memhotplug.MemoryHotplugController
	mountCleanup     func(context.Context) error
	needNetworkClean bool
	needVMShutdown   bool
}

func (s *service) cleanupOnDeleteFailure(ctx context.Context, id string) {
	s.stateMachine.SetIntentionalShutdown(true)
	go s.requestShutdownAndExit(ctx, fmt.Sprintf("delete failed for %s", id))
}

func (s *service) collectDeleteCleanup(r *taskAPI.DeleteRequest) deleteCleanup {
	var cleanup deleteCleanup

	s.containerMu.Lock()
	if s.container != nil && s.containerID == r.ID {
		if r.ExecID != "" {
			if s.container.io != nil {
				if pio, ok := s.container.io.exec[r.ExecID]; ok {
					cleanup.ioForwarders = append(cleanup.ioForwarders, pio.forwarder)
					delete(s.container.io.exec, r.ExecID)
				}
			}
		} else {
			if s.container.io != nil {
				cleanup.ioForwarders = append(cleanup.ioForwarders, s.container.io.init.forwarder)
				for _, pio := range s.container.io.exec {
					cleanup.ioForwarders = append(cleanup.ioForwarders, pio.forwarder)
				}
			}
			cleanup.mountCleanup = s.container.mountCleanup
			cleanup.needNetworkClean = true
			cleanup.needVMShutdown = true
			s.container = nil
			s.containerID = ""
		}
	}
	s.containerMu.Unlock()

	if r.ExecID == "" {
		s.controllerMu.Lock()
		cleanup.cpuController = s.cpuHotplugControllers[r.ID]
		delete(s.cpuHotplugControllers, r.ID)
		cleanup.memController = s.memoryHotplugControllers[r.ID]
		delete(s.memoryHotplugControllers, r.ID)
		s.controllerMu.Unlock()
	}

	return cleanup
}

func (s *service) runDeleteCleanup(ctx context.Context, r *taskAPI.DeleteRequest, cleanup deleteCleanup) {
	// Shutdown I/O forwarders (already extracted from state)
	for i, forwarder := range cleanup.ioForwarders {
		if err := forwarder.Shutdown(ctx); err != nil {
			if i == 0 && r.ExecID == "" {
				log.G(ctx).WithError(err).Error("failed to shutdown io after delete")
			} else {
				log.G(ctx).WithError(err).WithField(fieldExec, r.ExecID).Error("failed to shutdown exec io after delete")
			}
		}
	}

	// Stop hotplug controllers (already extracted from state)
	if cleanup.cpuController != nil {
		cleanup.cpuController.Stop()
	}
	if cleanup.memController != nil {
		cleanup.memController.Stop()
	}

	// For container deletion, use orchestrator for VM/network/mount cleanup
	if cleanup.needVMShutdown {
		log.G(ctx).Info("container deleted, shutting down VM")
		s.stateMachine.SetIntentionalShutdown(true)

		// Build phases with pre-extracted mount cleanup
		phases := lifecycle.CleanupPhases{
			VMShutdown: func(ctx context.Context) error {
				return s.vmLifecycle.Shutdown(ctx)
			},
			NetworkCleanup: func(ctx context.Context) error {
				if !cleanup.needNetworkClean {
					return nil
				}
				env := &network.Environment{ID: r.ID}
				return s.networkManager.ReleaseNetworkResources(ctx, env)
			},
			MountCleanup: func(ctx context.Context) error {
				if cleanup.mountCleanup == nil {
					return nil
				}
				return cleanup.mountCleanup(ctx)
			},
		}
		orchestrator := lifecycle.NewCleanupOrchestrator(phases)
		result := orchestrator.ExecutePartial(ctx, lifecycle.PhaseVMShutdown)

		if result.HasErrors() {
			log.G(ctx).WithField("failed_phases", result.FailedPhases()).Warn("delete cleanup had errors")
		}

		log.G(ctx).Info("VM and network cleanup complete, scheduling shim exit")
		go s.requestShutdownAndExit(ctx, "container deleted")
	}
}

// Delete the initial process and container.
func (s *service) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	done, err := s.beginRPC(false)
	if err != nil {
		return nil, err
	}
	defer done()
	log.G(ctx).WithFields(log.Fields{"id": r.ID, fieldExec: r.ExecID}).Debug("delete task request")

	// Mark deletion in progress (only for container, not exec)
	if r.ExecID == "" {
		if !s.stateMachine.TryStartDeleting() {
			state := s.stateMachine.State()
			if state == lifecycle.StateCreating {
				return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "cannot delete while container creation is in progress")
			}
			if state == lifecycle.StateShuttingDown {
				return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "shim is shutting down")
			}
			if state == lifecycle.StateDeleting {
				return nil, errgrpc.ToGRPCf(errdefs.ErrAlreadyExists, "delete already in progress")
			}
			return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "cannot delete in state: %s", state)
		}
	}

	vmc, cleanup, err := s.getTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	tc := taskAPI.NewTTRPCTaskClient(vmc)
	resp, err := tc.Delete(ctx, r)
	if err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{"id": r.ID, fieldExec: r.ExecID}).Warn("delete task failed")
		if r.ExecID == "" {
			s.cleanupOnDeleteFailure(ctx, r.ID)
		}
		return resp, err
	}

	// Publish TaskDelete event directly from the shim.
	// We cannot rely on the guest's event stream because the shim may shut down
	// before the event propagates through the stream. This ensures containerd
	// receives the delete event before the shim exits.
	if r.ExecID == "" {
		s.send(&eventstypes.TaskDelete{
			ContainerID: r.ID,
			Pid:         resp.Pid,
			ExitStatus:  resp.ExitStatus,
			ExitedAt:    resp.ExitedAt,
		})
	}

	delCleanup := s.collectDeleteCleanup(r)
	s.runDeleteCleanup(ctx, r, delCleanup)

	log.G(ctx).WithFields(log.Fields{"id": r.ID, fieldExec: r.ExecID}).Debug("delete task completed")
	return resp, nil
}

// Exec an additional process inside the container.
func (s *service) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*ptypes.Empty, error) {
	done, err := s.beginRPC(false)
	if err != nil {
		return nil, err
	}
	defer done()

	log.G(ctx).WithFields(log.Fields{"id": r.ID, fieldExec: r.ExecID}).Debug("exec request")

	vmc, cleanup, err := s.getTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

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

	// Use forwardIOWithIDs to enable RPC-based I/O for non-TTY mode (supports task attach)
	// The forwarder must be started AFTER the guest creates the exec process.
	cio, execForwarder, err := s.forwardIOWithIDs(ctx, vmi, r.ID, r.ExecID, rio)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	s.containerMu.Lock()
	if s.container != nil && s.containerID == r.ID {
		if s.container.io == nil {
			s.container.io = &taskIO{exec: make(map[string]processIOState)}
		}
		if s.container.io.exec == nil {
			s.container.io.exec = make(map[string]processIOState)
		}
		s.container.io.exec[r.ExecID] = processIOState{
			host: execIO{
				stdin:  rio.Stdin,
				stdout: rio.Stdout,
				stderr: rio.Stderr,
			},
			forwarder: execForwarder,
		}
	} else {
		// Forwarder is always non-nil (noopForwarder for passthrough mode)
		if shutdownErr := execForwarder.Shutdown(ctx); shutdownErr != nil {
			log.G(ctx).WithError(shutdownErr).Error("failed to shutdown exec io after container not found")
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
		if s.container != nil && s.containerID == r.ID && s.container.io != nil {
			// Forwarder is always non-nil when exec io is set (noopForwarder for passthrough mode)
			if pio, ok := s.container.io.exec[r.ExecID]; ok {
				if shutdownErr := pio.forwarder.Shutdown(ctx); shutdownErr != nil {
					log.G(ctx).WithError(shutdownErr).Error("failed to shutdown exec io after exec failure")
				}
				delete(s.container.io.exec, r.ExecID)
			}
		}
		s.containerMu.Unlock()
		return nil, errgrpc.ToGRPC(err)
	}

	// Start the I/O forwarder AFTER guest has created the exec process and registered with I/O manager.
	// This ensures the RPC I/O subscriber can find the process.
	// Note: forwarder is always non-nil (noopForwarder for passthrough mode), so Start() is always safe.
	if err := execForwarder.Start(ctx); err != nil {
		log.G(ctx).WithError(err).Error("failed to start exec I/O forwarder")
		// Don't fail the exec, just log the error - I/O may not work but exec is created
	}

	return resp, nil
}

// ResizePty of a process.
func (s *service) ResizePty(ctx context.Context, r *taskAPI.ResizePtyRequest) (*ptypes.Empty, error) {
	done, err := s.beginRPC(false)
	if err != nil {
		return nil, err
	}
	defer done()

	log.G(ctx).WithFields(log.Fields{"id": r.ID, fieldExec: r.ExecID}).Debug("resize pty request")
	vmc, cleanup, err := s.getTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return taskAPI.NewTTRPCTaskClient(vmc).ResizePty(ctx, r)
}

// State returns runtime state information for a process.
func (s *service) State(ctx context.Context, r *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	done, err := s.beginRPC(false)
	if err != nil {
		return nil, err
	}
	defer done()

	// Use snapshot pattern to safely access container state outside lock.
	// This prevents data races when container fields are modified concurrently.
	snap := s.getContainerSnapshot()

	// If this is an init process state request and we're in Created state (not yet Started),
	// return CREATED status without calling the guest. This replaces the initStarted check.
	if r.ExecID == "" && s.stateMachine.IsCreatedNotStarted() {
		if snap != nil {
			log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("state: short-circuit created state (init not started)")
			st := &taskAPI.StateResponse{
				Status: tasktypes.Status_CREATED,
				Pid:    snap.pid,
			}
			if snap.initIO != nil {
				st.Stdin = snap.initIO.stdin
				st.Stdout = snap.initIO.stdout
				st.Stderr = snap.initIO.stderr
				st.Terminal = snap.initIO.terminal
			}
			return st, nil
		}
	}

	// While paused, the entire VM (including vminitd) is frozen by QMP stop, so
	// the guest cannot answer a State query. Report PAUSED from cached state
	// instead of blocking on the frozen guest.
	if r.ExecID == "" && s.paused.Load() {
		if snap != nil {
			log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("state: short-circuit paused state")
			st := &taskAPI.StateResponse{
				Status: tasktypes.Status_PAUSED,
				Pid:    snap.pid,
			}
			if snap.initIO != nil {
				st.Stdin = snap.initIO.stdin
				st.Stdout = snap.initIO.stdout
				st.Stderr = snap.initIO.stderr
				st.Terminal = snap.initIO.terminal
			}
			return st, nil
		}
	}

	vmc, cleanup, err := s.getTaskClient(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Error("state: failed to get client")
		return nil, err
	}
	defer cleanup()

	st, err := taskAPI.NewTTRPCTaskClient(vmc).State(ctx, r)
	if err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{"id": r.ID, fieldExec: r.ExecID}).Error("state: guest state failed")
		return nil, errgrpc.ToGRPC(err)
	}
	// Replace guest's I/O paths (rpcio:// URIs) with host FIFO paths for attach support.
	// The guest uses rpcio:// URIs internally, but containerd's attach expects host FIFOs.
	// Re-fetch snapshot in case container state changed during RPC.
	snap = s.getContainerSnapshot()

	if snap != nil {
		if r.ExecID == "" {
			// Container I/O paths
			if snap.initIO != nil {
				st.Stdin = snap.initIO.stdin
				st.Stdout = snap.initIO.stdout
				st.Stderr = snap.initIO.stderr
				st.Terminal = snap.initIO.terminal
			}
		} else {
			// Exec process I/O paths
			if pio, ok := snap.execIO[r.ExecID]; ok {
				st.Stdin = pio.stdin
				st.Stdout = pio.stdout
				st.Stderr = pio.stderr
			}
		}
	}

	return st, nil
}

// Pause the container.
func (s *service) Pause(ctx context.Context, r *taskAPI.PauseRequest) (*ptypes.Empty, error) {
	done, err := s.beginRPC(false)
	if err != nil {
		return nil, err
	}
	defer done()

	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("pause request")

	// spinbox runs one container per VM, so suspending the VM's vCPUs (QMP
	// "stop") is equivalent to pausing the container. Execution halts while
	// memory and device state are retained; Resume restarts it.
	vmi, err := s.vmLifecycle.Instance()
	if err != nil {
		return nil, errgrpc.ToGRPC(fmt.Errorf("pause: %w", err))
	}

	// Freeze the writable filesystem before suspending vCPUs (FIFREEZE must run
	// in the live guest). This flushes the page cache and quiesces the on-disk
	// rwlayer, so a paused container has a consistent backing image that a
	// commit can read. FIFREEZE alone keeps the VM running; the QMP stop below
	// then halts execution.
	if err := s.freezeGuestFilesystems(ctx); err != nil {
		return nil, errgrpc.ToGRPC(fmt.Errorf("pause: freeze guest filesystems: %w", err))
	}
	if err := vmi.Pause(ctx); err != nil {
		// Roll back the freeze so the container is not left with a frozen fs.
		if thawErr := s.thawGuestFilesystems(ctx); thawErr != nil {
			log.G(ctx).WithError(thawErr).Warn("pause: thaw rollback failed after vm pause error")
		}
		return nil, errgrpc.ToGRPC(fmt.Errorf("pause vm: %w", err))
	}
	s.paused.Store(true)

	s.send(&eventstypes.TaskPaused{ContainerID: r.ID})
	return &ptypes.Empty{}, nil
}

// Resume the container.
func (s *service) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (*ptypes.Empty, error) {
	done, err := s.beginRPC(false)
	if err != nil {
		return nil, err
	}
	defer done()

	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("resume request")

	vmi, err := s.vmLifecycle.Instance()
	if err != nil {
		return nil, errgrpc.ToGRPC(fmt.Errorf("resume: %w", err))
	}
	if err := vmi.Resume(ctx); err != nil {
		return nil, errgrpc.ToGRPC(fmt.Errorf("resume vm: %w", err))
	}
	s.paused.Store(false)

	// Thaw after vCPUs are running again (FITHAW must run in the live guest).
	if err := s.thawGuestFilesystems(ctx); err != nil {
		return nil, errgrpc.ToGRPC(fmt.Errorf("resume: thaw guest filesystems: %w", err))
	}

	s.send(&eventstypes.TaskResumed{ContainerID: r.ID})
	return &ptypes.Empty{}, nil
}

// freezeGuestFilesystems asks vminitd to FIFREEZE the container's writable
// filesystem(s) so the backing rwlayer image is consistent on disk.
func (s *service) freezeGuestFilesystems(ctx context.Context) error {
	vmc, cleanup, err := s.getTaskClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := systemapi.NewTTRPCSystemServiceClient(vmc).FreezeFilesystems(ctx, &systemapi.FreezeFilesystemsRequest{})
	if err != nil {
		return err
	}
	log.G(ctx).WithField("frozen", resp.GetFrozen()).Debug("froze guest filesystems")
	return nil
}

// thawGuestFilesystems asks vminitd to FITHAW filesystems frozen by a prior
// freeze. It is idempotent on the guest side.
func (s *service) thawGuestFilesystems(ctx context.Context) error {
	vmc, cleanup, err := s.getTaskClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	_, err = systemapi.NewTTRPCSystemServiceClient(vmc).ThawFilesystems(ctx, &systemapi.ThawFilesystemsRequest{})
	return err
}

// Kill a process with the provided signal.
func (s *service) Kill(ctx context.Context, r *taskAPI.KillRequest) (*ptypes.Empty, error) {
	done, err := s.beginRPC(false)
	if err != nil {
		return nil, err
	}
	defer done()

	log.G(ctx).WithFields(log.Fields{"id": r.ID, fieldExec: r.ExecID}).Debug("kill request")
	vmc, cleanup, err := s.getTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return taskAPI.NewTTRPCTaskClient(vmc).Kill(ctx, r)
}

// Pids returns all pids inside the container.
func (s *service) Pids(ctx context.Context, r *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	done, err := s.beginRPC(false)
	if err != nil {
		return nil, err
	}
	defer done()

	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("pids request")
	vmc, cleanup, err := s.getTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return taskAPI.NewTTRPCTaskClient(vmc).Pids(ctx, r)
}

// CloseIO of a process.
func (s *service) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*ptypes.Empty, error) {
	done, err := s.beginRPC(false)
	if err != nil {
		return nil, err
	}
	defer done()

	log.G(ctx).WithFields(log.Fields{"id": r.ID, fieldExec: r.ExecID, "stdin": r.Stdin}).Debug("close io request")

	// If stdin is being closed and we have an RPC forwarder, signal it to close stdin.
	// This allows the forwarder to stop waiting for FIFO writers and propagate the
	// close to the guest via RPC.
	if r.Stdin {
		s.containerMu.Lock()
		if s.container != nil && s.containerID == r.ID && s.container.io != nil {
			// Forwarder is always non-nil when io is set (noopForwarder for passthrough mode)
			if r.ExecID == "" {
				// Container stdin
				s.container.io.init.forwarder.CloseStdin()
			} else {
				// Exec stdin
				if pio, ok := s.container.io.exec[r.ExecID]; ok {
					pio.forwarder.CloseStdin()
				}
			}
		}
		s.containerMu.Unlock()
	}

	vmc, cleanup, err := s.getTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return taskAPI.NewTTRPCTaskClient(vmc).CloseIO(ctx, r)
}

// Checkpoint the container.
func (s *service) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (*ptypes.Empty, error) {
	done, err := s.beginRPC(false)
	if err != nil {
		return nil, err
	}
	defer done()

	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("checkpoint request")
	// Checkpoint is not supported in VM-based runtime.
	// Would require CRIU or QEMU snapshot to save/restore process state.
	return nil, errgrpc.ToGRPCf(errdefs.ErrNotImplemented, "checkpoint is not supported: VM-based runtime cannot snapshot process state")
}

// Update a running container.
func (s *service) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*ptypes.Empty, error) {
	done, err := s.beginRPC(false)
	if err != nil {
		return nil, err
	}
	defer done()

	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("update request")
	vmc, cleanup, err := s.getTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return taskAPI.NewTTRPCTaskClient(vmc).Update(ctx, r)
}

// Wait for a process to exit.
func (s *service) Wait(ctx context.Context, r *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	done, err := s.beginRPC(false)
	if err != nil {
		return nil, err
	}
	defer done()

	log.G(ctx).WithFields(log.Fields{"id": r.ID, fieldExec: r.ExecID}).Debug("wait request")
	vmc, cleanup, err := s.getTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return taskAPI.NewTTRPCTaskClient(vmc).Wait(ctx, r)
}

// Connect returns shim information such as the shim's pid.
func (s *service) Connect(ctx context.Context, r *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	done, err := s.beginRPC(false)
	if err != nil {
		return nil, err
	}
	defer done()

	// Use snapshot pattern to safely access container state outside lock.
	snap := s.getContainerSnapshot()
	hasContainer := snap != nil && snap.id == r.ID
	pid := uint32(0)
	if hasContainer {
		pid = snap.pid
	}
	log.G(ctx).WithFields(log.Fields{
		"id":            r.ID,
		"has_container": hasContainer,
		"task_pid":      pid,
	}).Debug("connect request")
	if hasContainer && pid != 0 {
		return &taskAPI.ConnectResponse{
			ShimPid: uint32(os.Getpid()),
			TaskPid: pid,
		}, nil
	}

	vmc, cleanup, err := s.getTaskClient(ctx)
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
	done, err := s.beginRPC(true)
	if err != nil {
		return nil, err
	}
	defer done()
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("shutdown request")

	s.stateMachine.SetIntentionalShutdown(true)

	if s.shutdownSvc != nil {
		go s.requestShutdownAndExit(ctx, "shutdown rpc")
	} else if s.initiateShutdown != nil {
		s.initiateShutdown()
		s.initiateShutdown = nil
	}

	return &ptypes.Empty{}, nil
}

func (s *service) Stats(ctx context.Context, r *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	done, err := s.beginRPC(false)
	if err != nil {
		return nil, err
	}
	defer done()

	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("stats request")
	vmc, cleanup, err := s.getTaskClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return taskAPI.NewTTRPCTaskClient(vmc).Stats(ctx, r)
}

// getIOForwarder returns the I/O forwarder for the given container/exec ID.
// Returns nil if no forwarder is found.
func (s *service) getIOForwarder(containerID, execID string) IOForwarder {
	s.containerMu.Lock()
	defer s.containerMu.Unlock()

	if s.container == nil || s.container.io == nil {
		return nil
	}

	// Check if container ID matches
	if s.containerID != containerID {
		return nil
	}

	if execID == "" {
		// Init process
		return s.container.io.init.forwarder
	}
	// Exec process
	if pio, ok := s.container.io.exec[execID]; ok {
		return pio.forwarder
	}
	return nil
}

// waitForIOBeforeExit waits for the I/O forwarder to complete before forwarding a TaskExit event.
// This ensures that all stdout/stderr data is written to FIFOs before containerd receives the exit event.
func (s *service) waitForIOBeforeExit(ctx context.Context, ev *types.Envelope) {
	// Parse the event to get container/exec IDs
	if ev.Event == nil {
		return
	}

	v, err := typeurl.UnmarshalAny(ev.Event)
	if err != nil {
		log.G(ctx).WithError(err).Debug("failed to unmarshal event for I/O wait")
		return
	}

	taskExit, ok := v.(*eventstypes.TaskExit)
	if !ok {
		return
	}

	// Get the exec ID (empty for init process)
	execID := ""
	if taskExit.ID != taskExit.ContainerID {
		execID = taskExit.ID
	}

	forwarder := s.getIOForwarder(taskExit.ContainerID, execID)
	if forwarder == nil {
		log.G(ctx).WithFields(log.Fields{
			fieldContainer: taskExit.ContainerID,
			fieldExec:      execID,
		}).Debug("no I/O forwarder found for TaskExit, proceeding")
		return
	}

	log.G(ctx).WithFields(log.Fields{
		fieldContainer: taskExit.ContainerID,
		fieldExec:      execID,
	}).Debug("waiting for I/O forwarder to complete before forwarding TaskExit")

	// Wait for I/O to complete. With direct stream I/O, the guest closes the stream
	// when the process exits, and we see EOF. The timeout is a safety measure for
	// cases where the stream doesn't close cleanly (e.g., vsock connection lost).
	const ioWaitTimeout = 30 * time.Second

	done := make(chan struct{})
	go func() {
		forwarder.WaitForComplete()
		close(done)
	}()

	select {
	case <-done:
		log.G(ctx).WithFields(log.Fields{
			fieldContainer: taskExit.ContainerID,
			fieldExec:      execID,
		}).Debug("I/O forwarder complete, forwarding TaskExit")
	case <-time.After(ioWaitTimeout):
		log.G(ctx).WithFields(log.Fields{
			fieldContainer: taskExit.ContainerID,
			fieldExec:      execID,
			"timeout":      ioWaitTimeout,
		}).Warn("timeout waiting for I/O forwarder, proceeding with TaskExit")
	}
}

func (s *service) send(evt any) {
	// Use select with eventsDone to safely handle the race between sending
	// and channel close. When eventsDone is closed (which happens before
	// events channel), the send is cancelled without panic.
	select {
	case <-s.eventsDone:
		return // Shutdown in progress, drop event
	case s.events <- evt:
		// Sent successfully
	}
}

func (s *service) requestShutdownAndExit(ctx context.Context, reason string) {
	started := false
	s.shutdownOnce.Do(func() {
		started = true
		s.shutdownRequested.Store(true)
		s.stateMachine.ForceTransition(lifecycle.StateShuttingDown)

		log.G(ctx).WithField("reason", reason).Info("shim shutdown requested")

		// Use injectable exit function, defaulting to os.Exit
		exit := s.exitFunc
		if exit == nil {
			exit = os.Exit
		}

		// Wait for in-flight requests to complete before triggering shutdown callbacks.
		inflightTimeout := time.NewTimer(5 * time.Second)
		inflightTicker := time.NewTicker(10 * time.Millisecond)
		defer inflightTimeout.Stop()
		defer inflightTicker.Stop()

	inflightWait:
		for s.inflight.Load() > 0 {
			select {
			case <-inflightTimeout.C:
				log.G(ctx).WithFields(log.Fields{
					"reason":   reason,
					"inflight": s.inflight.Load(),
				}).Warn("shutdown waiting for in-flight requests timed out")
				break inflightWait
			case <-inflightTicker.C:
				// Check again.
			}
		}

		if s.shutdownSvc == nil {
			log.G(ctx).WithField("reason", reason).Warn("shutdown service missing; running cleanup inline")
			if err := s.shutdown(context.WithoutCancel(ctx)); err != nil {
				log.G(ctx).WithError(err).WithField("reason", reason).Warn("inline shutdown completed with errors")
			}
			s.waitForForwarder(reason)
			log.G(ctx).WithField("reason", reason).Info("exiting shim after inline shutdown")
			exit(0)
			return
		}

		s.shutdownSvc.Shutdown()

		select {
		case <-s.shutdownSvc.Done():
		case <-time.After(5 * time.Second):
			log.G(ctx).WithField("reason", reason).Warn("shutdown timeout; exiting anyway")
		}

		s.waitForForwarder(reason)
		log.G(ctx).WithField("reason", reason).Info("exiting shim after shutdown")
		exit(0)
	})

	if !started {
		log.G(ctx).WithField("reason", reason).Debug("shim shutdown already in progress")
	}
}

func (s *service) waitForForwarder(reason string) {
	if s.forwardDone == nil {
		return
	}

	select {
	case <-s.forwardDone:
	case <-time.After(5 * time.Second):
		log.L.WithField("reason", reason).Warn("timed out waiting for event forwarder to exit")
	}
}

func (s *service) forward(ctx context.Context, publisher shim.Publisher, ready chan<- struct{}) {
	defer func() {
		if s.forwardDone != nil {
			close(s.forwardDone)
		}
	}()

	ns, ok := namespaces.Namespace(ctx)
	if !ok || ns == "" {
		ns = defaultNamespace
	}
	ctx = namespaces.WithNamespace(context.WithoutCancel(ctx), ns)

	close(ready)

	for e := range s.events {
		switch e := e.(type) {
		case *types.Envelope:
			if err := publisher.Publish(ctx, e.Topic, e.Event); err != nil {
				log.G(ctx).WithError(err).Error("forward event")
			}
		default:
			if err := publisher.Publish(ctx, runtime.GetTopic(e), e); err != nil {
				log.G(ctx).WithError(err).Error("post event")
			}
		}
	}
	if err := publisher.Close(); err != nil {
		log.G(ctx).WithError(err).Error("close event publisher")
	}
}
