//go:build linux

// Package task implements the containerd task service for qemubox runtime.
package task

import (
	"context"
	"fmt"
	"strings"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"

	bundleAPI "github.com/aledbf/qemubox/containerd/api/services/bundle/v1"
	"github.com/aledbf/qemubox/containerd/internal/host/network"
	"github.com/aledbf/qemubox/containerd/internal/host/vm"
	"github.com/aledbf/qemubox/containerd/internal/shim/bundle"
	"github.com/aledbf/qemubox/containerd/internal/shim/lifecycle"
	"github.com/aledbf/qemubox/containerd/internal/shim/resources"
	"github.com/aledbf/qemubox/containerd/internal/shim/transform"
)

// createCleanup tracks resources that need cleanup on failure.
// Cleanups are executed in reverse order (LIFO) when rollback is called.
type createCleanup struct {
	steps []cleanupStep
}

type cleanupStep struct {
	name string
	fn   func(context.Context) error
}

func (c *createCleanup) add(name string, fn func(context.Context) error) {
	c.steps = append(c.steps, cleanupStep{name: name, fn: fn})
}

func (c *createCleanup) rollback(ctx context.Context) {
	// Execute in reverse order (LIFO)
	for i := len(c.steps) - 1; i >= 0; i-- {
		step := c.steps[i]
		if err := step.fn(ctx); err != nil {
			log.G(ctx).WithError(err).WithField("step", step.name).Warn("cleanup failed")
		}
	}
}

// createState holds intermediate state during container creation.
// This avoids passing many parameters between helper functions.
type createState struct {
	request      *taskAPI.CreateTaskRequest
	bundle       *bundle.Bundle
	resourceCfg  *vm.VMResourceConfig
	vmInstance   vm.Instance
	mountCleanup func(context.Context) error
	mounts       []*types.Mount
	netConfig    *vm.NetworkConfig
	netnsPath    string
	ioForwarder  IOForwarder
	containerIO  stdio.Stdio
	guestIO      stdio.Stdio
	cleanup      createCleanup
}

// validateCreateRequest performs all pre-creation validation.
// Returns an error if the request is invalid or the shim is not ready.
func (s *service) validateCreateRequest(ctx context.Context, r *taskAPI.CreateTaskRequest) error {
	if r.Checkpoint != "" || r.ParentCheckpoint != "" {
		return errgrpc.ToGRPC(fmt.Errorf("checkpoints not supported: %w", errdefs.ErrNotImplemented))
	}

	// Atomically transition to Creating state to prevent concurrent Create() or Delete()
	if !s.stateMachine.TryStartCreating() {
		state := s.stateMachine.State()
		if state == lifecycle.StateDeleting {
			return errgrpc.ToGRPCf(errdefs.ErrAlreadyExists, "shim is deleting container; requires fresh shim per container")
		}
		if state == lifecycle.StateShuttingDown {
			return errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "shim is shutting down")
		}
		return errgrpc.ToGRPCf(errdefs.ErrAlreadyExists, "container creation already in progress")
	}

	// Check if container already exists
	s.containerMu.Lock()
	hasContainer := s.container != nil
	s.containerMu.Unlock()

	if _, err := s.vmLifecycle.Instance(); err == nil || hasContainer {
		_ = s.stateMachine.MarkCreationFailed()
		return errgrpc.ToGRPCf(errdefs.ErrAlreadyExists, "shim already running a container; requires fresh shim per container")
	}

	// Security: prevent path traversal via container ID
	if strings.ContainsAny(r.ID, "/\\") {
		_ = s.stateMachine.MarkCreationFailed()
		return errgrpc.ToGRPCf(errdefs.ErrInvalidArgument, "container ID contains invalid path separators: %q", r.ID)
	}

	return nil
}

// setupVMInstance creates the VM and sets up mounts and networking.
// On success, cleanup functions are registered in state.cleanup.
func (s *service) setupVMInstance(ctx context.Context, state *createState) error {
	r := state.request

	// Check KVM availability
	if err := lifecycle.CheckKVM(); err != nil {
		return err
	}

	// Load and transform bundle
	b, err := transform.LoadForCreate(ctx, r.Bundle)
	if err != nil {
		return err
	}
	state.bundle = b

	// Compute resource configuration
	resourceCfg, _ := resources.ComputeConfig(ctx, &b.Spec)
	state.resourceCfg = resourceCfg

	log.G(ctx).WithFields(log.Fields{
		"boot_cpus":  resourceCfg.BootCPUs,
		"max_cpus":   resourceCfg.MaxCPUs,
		"memory_mb":  resourceCfg.MemorySize / (1024 * 1024),
		"hotplug_mb": resourceCfg.MemoryHotplugSize / (1024 * 1024),
	}).Debug("VM resource configuration")

	// Create VM instance
	vmi, err := s.vmLifecycle.CreateVM(ctx, r.ID, r.Bundle, resourceCfg)
	if err != nil {
		return err
	}
	state.vmInstance = vmi

	// Setup mounts
	setupResult, err := s.platformMounts.Setup(ctx, vmi, r.ID, r.Rootfs)
	if err != nil {
		return err
	}
	state.mountCleanup = setupResult.Cleanup
	state.mounts = setupResult.Mounts

	// Register mount cleanup
	state.cleanup.add("mounts", func(ctx context.Context) error {
		if state.mountCleanup != nil {
			return state.mountCleanup(ctx)
		}
		return nil
	})

	// Setup networking
	state.netnsPath = "/var/run/netns/" + r.ID
	netCfg, err := s.platformNetwork.Setup(ctx, s.networkManager, vmi, r.ID, state.netnsPath)
	if err != nil {
		return err
	}
	state.netConfig = netCfg

	// Register network cleanup
	state.cleanup.add("network", func(ctx context.Context) error {
		env := &network.Environment{ID: r.ID}
		return s.networkManager.ReleaseNetworkResources(ctx, env)
	})

	return nil
}

// startVM boots the VM and establishes the event stream connection.
func (s *service) startVM(ctx context.Context, state *createState) error {
	startOpts := []vm.StartOpt{
		vm.WithNetworkConfig(state.netConfig),
		vm.WithNetworkNamespace(state.netnsPath),
	}

	prestart := time.Now()
	if err := state.vmInstance.Start(ctx, startOpts...); err != nil {
		return err
	}

	bootTime := time.Since(prestart)
	log.G(ctx).WithField("bootTime", bootTime).Debug("VM boot completed")
	s.stateMachine.SetIntentionalShutdown(false)

	// Get VM client for event stream
	vmc, err := s.vmLifecycle.Client()
	if err != nil {
		return err
	}

	// Start forwarding events
	ns, _ := namespaces.Namespace(ctx)
	eventCtx := namespaces.WithNamespace(context.WithoutCancel(ctx), ns)
	if err := s.startEventForwarder(eventCtx, vmc); err != nil {
		return err
	}

	return nil
}

// createTaskInVM creates the bundle and task inside the VM.
func (s *service) createTaskInVM(ctx context.Context, state *createState) (*taskAPI.CreateTaskResponse, error) {
	r := state.request

	// Dial TTRPC client for Create RPCs.
	// NOTE: We intentionally do NOT cache this connection because vsock connections
	// can become stale between Create completing and Start being called.
	rpcClient, err := s.vmLifecycle.DialClient(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Error("create: failed to dial RPC client")
		return nil, err
	}

	// Create bundle in VM
	bundleFiles, err := state.bundle.Files()
	if err != nil {
		return nil, err
	}

	bundleService := bundleAPI.NewTTRPCBundleClient(rpcClient)
	br, err := bundleService.Create(ctx, &bundleAPI.CreateRequest{
		ID:    r.ID,
		Files: bundleFiles,
	})
	if err != nil {
		return nil, err
	}

	// Setup I/O forwarding
	state.containerIO = stdio.Stdio{
		Stdin:    r.Stdin,
		Stdout:   r.Stdout,
		Stderr:   r.Stderr,
		Terminal: r.Terminal,
	}

	cio, ioForwarder, err := s.forwardIOWithIDs(ctx, state.vmInstance, r.ID, "", state.containerIO)
	if err != nil {
		return nil, err
	}
	state.guestIO = cio
	state.ioForwarder = ioForwarder

	// Create task in VM
	vr := &taskAPI.CreateTaskRequest{
		ID:       r.ID,
		Bundle:   br.Bundle,
		Rootfs:   state.mounts,
		Terminal: cio.Terminal,
		Stdin:    cio.Stdin,
		Stdout:   cio.Stdout,
		Stderr:   cio.Stderr,
		Options:  r.Options,
	}

	tc := taskAPI.NewTTRPCTaskClient(rpcClient)
	resp, err := tc.Create(ctx, vr)
	if err != nil {
		log.G(ctx).WithError(err).Error("failed to create task")
		if shutdownErr := ioForwarder.Shutdown(ctx); shutdownErr != nil {
			log.G(ctx).WithError(shutdownErr).Error("failed to shutdown io after create failure")
		}
		return nil, err
	}

	// Start the I/O forwarder AFTER guest has created the process
	if err := ioForwarder.Start(ctx); err != nil {
		log.G(ctx).WithError(err).Error("failed to start I/O forwarder")
		// Don't fail the create, just log - I/O may not work but container is created
	}

	return resp, nil
}

// finalizeCreate stores the container state and starts hotplug controllers.
func (s *service) finalizeCreate(ctx context.Context, state *createState, resp *taskAPI.CreateTaskResponse) {
	r := state.request

	c := &container{
		pid: resp.Pid,
		io: &taskIO{
			init: processIOState{
				host: execIO{
					stdin:  state.containerIO.Stdin,
					stdout: state.containerIO.Stdout,
					stderr: state.containerIO.Stderr,
				},
				terminal:  state.containerIO.Terminal,
				forwarder: state.ioForwarder,
			},
			exec: make(map[string]processIOState),
		},
		mountCleanup: state.mountCleanup,
	}

	s.containerMu.Lock()
	s.container = c
	s.containerID = r.ID
	s.containerMu.Unlock()

	// Start hotplug controllers
	callbacks := resources.CreateVMClientCallbacks(s.connManager.GetClient)
	if cpuCtrl := resources.StartCPUHotplug(ctx, r.ID, state.vmInstance, state.resourceCfg, callbacks); cpuCtrl != nil {
		s.controllerMu.Lock()
		s.cpuHotplugControllers[r.ID] = cpuCtrl
		s.controllerMu.Unlock()
	}
	if memCtrl := resources.StartMemoryHotplug(ctx, r.ID, state.vmInstance, state.resourceCfg, callbacks); memCtrl != nil {
		s.controllerMu.Lock()
		s.memoryHotplugControllers[r.ID] = memCtrl
		s.controllerMu.Unlock()
	}
}

// Create creates a new initial process and container with the underlying OCI runtime.
//
// The creation process is split into phases with explicit cleanup handling:
//  1. Validation - check request and shim state
//  2. VM Setup - create VM instance, configure mounts and networking
//  3. VM Start - boot VM and establish event stream
//  4. Task Creation - create bundle and task inside VM
//  5. Finalization - store container state and start hotplug controllers
//
// On failure, cleanup.rollback() releases resources in reverse order (LIFO).
func (s *service) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	log.G(ctx).WithFields(log.Fields{
		"id":     r.ID,
		"bundle": r.Bundle,
	}).Debug("creating container task")

	// Phase 1: Validation (transitions state to Creating)
	if err := s.validateCreateRequest(ctx, r); err != nil {
		return nil, err
	}

	// On any failure, mark creation as failed (transitions Creating -> Idle)
	var createSucceeded bool
	defer func() {
		if !createSucceeded {
			_ = s.stateMachine.MarkCreationFailed()
		}
	}()

	presetup := time.Now()
	state := &createState{request: r}

	// Phase 2: Setup VM, mounts, and networking
	if err := s.setupVMInstance(ctx, state); err != nil {
		state.cleanup.rollback(ctx)
		return nil, errgrpc.ToGRPC(err)
	}

	// Phase 3: Start VM and event stream
	if err := s.startVM(ctx, state); err != nil {
		state.cleanup.rollback(ctx)
		return nil, errgrpc.ToGRPC(err)
	}

	// Phase 4: Create task in VM
	resp, err := s.createTaskInVM(ctx, state)
	if err != nil {
		state.cleanup.rollback(ctx)
		return nil, errgrpc.ToGRPC(err)
	}

	setupTime := time.Since(presetup)
	log.G(ctx).WithField("t_setup", setupTime).Info("task successfully created")

	// Phase 5: Finalize (store state, start controllers)
	// No rollback after this - container is considered created
	s.finalizeCreate(ctx, state, resp)

	// Transition to Running state
	if err := s.stateMachine.MarkCreated(); err != nil {
		log.G(ctx).WithError(err).Error("failed to transition to running state")
	}
	createSucceeded = true

	return &taskAPI.CreateTaskResponse{Pid: resp.Pid}, nil
}
