package task

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/runtime"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	ptypes "github.com/containerd/containerd/v2/pkg/protobuf/types"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"

	"github.com/aledbf/beacon/containerd/paths"
	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	bundleAPI "github.com/aledbf/beacon/containerd/api/services/bundle/v1"
	"github.com/aledbf/beacon/containerd/api/services/vmevents/v1"
	"github.com/aledbf/beacon/containerd/network"
	"github.com/aledbf/beacon/containerd/network/ipallocator"
	"github.com/aledbf/beacon/containerd/shim/bundle"
	"github.com/aledbf/beacon/containerd/store"
	"github.com/aledbf/beacon/containerd/vm"

	"github.com/aledbf/beacon/containerd/shim/cpuhotplug"
	"github.com/aledbf/beacon/containerd/vm/qemu"

	// Import VMM implementations to register factories
	_ "github.com/aledbf/beacon/containerd/vm/qemu"
)

const (
	// eventChannelBuffer is the size of the event channel buffer
	eventChannelBuffer = 128
)

var (
	_ = shim.TTRPCService(&service{})
)

// NewTaskService creates a new instance of a task service
func NewTaskService(ctx context.Context, publisher shim.Publisher, sd shutdown.Service) (taskAPI.TTRPCTaskService, error) {
	// Initialize network manager for this service
	nm, err := initNetworkManager(ctx)
	if err != nil {
		return nil, fmt.Errorf("initialize network manager: %w", err)
	}

	s := &service{
		context:          ctx,
		events:           make(chan any, eventChannelBuffer),
		containers:       make(map[string]*container),
		networkManager:   nm,
		initiateShutdown: sd.Shutdown,
	}
	sd.RegisterCallback(s.shutdown)

	if address, err := shim.ReadAddress("address"); err == nil {
		sd.RegisterCallback(func(context.Context) error {
			return shim.RemoveSocket(address)
		})
	}

	go s.forward(ctx, publisher)

	return s, nil
}

type container struct {
	ioShutdown func(context.Context) error

	execShutdowns map[string]func(context.Context) error
}

// service is the shim implementation of a remote shim over GRPC
type service struct {
	mu sync.Mutex

	// vm is the VM instance used to run the container
	vm vm.Instance

	// networkManager handles network resource allocation and cleanup
	networkManager network.NetworkManagerInterface

	// cpuHotplugController manages dynamic vCPU allocation (QEMU only)
	cpuHotplugController *cpuhotplug.Controller

	context context.Context
	events  chan any

	containers map[string]*container

	initiateShutdown func()
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	taskAPI.RegisterTTRPCTaskService(server, s)
	return nil
}

const (
	// KVM ioctl obtained by running: printf("KVM_GET_API_VERSION: 0x%llX\n", KVM_GET_API_VERSION);
	ioctlKVMGetAPIVersion = 0xAE00
	expectedKVMAPIVersion = 12
)

// checkKVM verifies that KVM is available on the system
func checkKVM() error {
	fd, err := unix.Open("/dev/kvm", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("failed to open /dev/kvm: %w. Your system may lack KVM support or you may have insufficient permissions", err)
	}
	defer syscall.Close(fd)

	// Kernel docs says:
	//     Applications should refuse to run if KVM_GET_API_VERSION returns a value other than 12.
	// See https://docs.kernel.org/virt/kvm/api.html#kvm-get-api-version
	apiVersion, _, errno := unix.RawSyscall(unix.SYS_IOCTL, uintptr(fd), ioctlKVMGetAPIVersion, 0)
	if errno != 0 {
		return fmt.Errorf("failed to get KVM API version: %w. You may have insufficient permissions", errno)
	}
	if apiVersion != expectedKVMAPIVersion {
		return fmt.Errorf("KVM API version mismatch; expected %d, got %d", expectedKVMAPIVersion, apiVersion)
	}

	return nil
}

// initNetworkManager creates and initializes a new NetworkManager instance
func initNetworkManager(ctx context.Context) (network.NetworkManagerInterface, error) {
	// Create stores using the new boltstore package
	// Multiple stores can share the same database file
	dbPath := paths.NetworkDBPath()

	networkConfigStore, err := boltstore.NewBoltStore[network.NetworkConfig](
		dbPath, "network_configs",
	)
	if err != nil {
		return nil, fmt.Errorf("create network config store: %w", err)
	}

	ipStore, err := boltstore.NewBoltStore[ipallocator.IPAllocation](
		dbPath, "ip_allocations",
	)
	if err != nil {
		networkConfigStore.Close()
		return nil, fmt.Errorf("create IP store: %w", err)
	}

	// Create NetworkManager
	nm, err := network.NewNetworkManager(
		network.NetworkConfig{Subnet: "10.88.0.0/16"},
		networkConfigStore,
		ipStore,
		nil, // Use default module checker
		nil, // Use default network operator
		nil, // Use default NFTables operator
		nil, // No policy change callback
	)
	if err != nil {
		networkConfigStore.Close()
		ipStore.Close()
		return nil, fmt.Errorf("create network manager: %w", err)
	}

	log.G(ctx).Info("NetworkManager initialized")
	return nm, nil
}

func (s *service) shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error

	// Release network resources first
	for id := range s.containers {
		env := &network.Environment{Id: id}
		if err := s.networkManager.ReleaseNetworkResources(env); err != nil {
			log.G(ctx).WithError(err).WithField("id", id).Warn("failed to release network resources")
		}
	}

	// Shutdown IO for all containers and execs
	for id, c := range s.containers {
		if c.ioShutdown != nil {
			if err := c.ioShutdown(ctx); err != nil {
				errs = append(errs, fmt.Errorf("container %q io shutdown: %w", id, err))
			}
		}
		for execID, ioShutdown := range c.execShutdowns {
			if err := ioShutdown(ctx); err != nil {
				errs = append(errs, fmt.Errorf("container %q exec %q io shutdown: %w", id, execID, err))
			}
		}
	}

	// Shutdown VM
	if s.vm != nil {
		if err := s.vm.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("vm shutdown: %w", err))
		}
	}

	// Close network manager (this closes the database)
	if s.networkManager != nil {
		if err := s.networkManager.Close(); err != nil {
			errs = append(errs, fmt.Errorf("network manager close: %w", err))
		}
	}

	// Signal last event and stop forwarding
	s.events <- nil

	return errors.Join(errs...)
}

// transformBindMounts transforms bind mounts
func transformBindMounts(ctx context.Context, b *bundle.Bundle) error {
	for i, m := range b.Spec.Mounts {
		if m.Type == "bind" {
			filename := filepath.Base(m.Source)
			// Check that the bind is from a path with the bundle id
			if filepath.Base(filepath.Dir(m.Source)) != filepath.Base(b.Path) {
				log.G(ctx).WithFields(log.Fields{
					"source": m.Source,
					"name":   filename,
				}).Debug("ignoring bind mount")
				continue
			}

			buf, err := os.ReadFile(m.Source)
			if err != nil {
				return fmt.Errorf("failed to read mount file %q: %w", filename, err)
			}
			b.Spec.Mounts[i].Source = filename
			if err := b.AddExtraFile(filename, buf); err != nil {
				return fmt.Errorf("failed to add extra file %q: %w", filename, err)
			}
		}
	}

	return nil
}

// disableNetworkNamespace removes the network namespace from the OCI spec
func disableNetworkNamespace(ctx context.Context, b *bundle.Bundle) error {
	if b.Spec.Linux == nil {
		return nil
	}

	var namespaces []specs.LinuxNamespace
	for _, ns := range b.Spec.Linux.Namespaces {
		if ns.Type != specs.NetworkNamespace {
			namespaces = append(namespaces, ns)
		}
	}
	b.Spec.Linux.Namespaces = namespaces

	return nil
}

// Create a new initial process and container with the underlying OCI runtime.
// This involves:
// 1. Verifying KVM availability.
// 2. Loading and transforming the OCI bundle (e.g., removing network namespace).
// 3. Configuring VM resources (CPU, memory) based on the OCI spec.
// 4. Creating and booting the QEMU VM.
// 5. Setting up networking (IP allocation, TAP device).
// 6. Connecting to the VM via vsock and creating the container task inside.
func (s *service) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (_ *taskAPI.CreateTaskResponse, err error) {
	log.G(ctx).WithFields(log.Fields{
		"id":     r.ID,
		"bundle": r.Bundle,
		"rootfs": r.Rootfs,
		"stdin":  r.Stdin,
		"stdout": r.Stdout,
		"stderr": r.Stderr,
	}).Info("creating container task")

	if r.Checkpoint != "" || r.ParentCheckpoint != "" {
		return nil, errgrpc.ToGRPC(fmt.Errorf("checkpoints not supported: %w", errdefs.ErrNotImplemented))
	}

	presetup := time.Now()

	// QEMU requires KVM - check if available
	if err := checkKVM(); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// Load the OCI bundle and apply transformers to get the bundle that'll be
	// set up on the VM side.
	// We remove the network namespace to allow the container to share the VM's
	// network namespace (where eth0 is configured).
	b, err := bundle.Load(ctx, r.Bundle, transformBindMounts, disableNetworkNamespace)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// Extract resource requests from OCI spec
	cpuRequest := extractCPURequest(&b.Spec)
	memoryRequest := extractMemoryRequest(&b.Spec)

	// Get host resource limits
	hostCPUs := getHostCPUCount()
	hostMemory, err := getHostMemoryTotal()
	if err != nil {
		log.G(ctx).WithError(err).Warn("failed to get host memory total, using 256GB default")
		hostMemory = 256 * 1024 * 1024 * 1024 // 256GB default
	}

	// Align memory values to 128MB for virtio-mem requirement
	const virtioMemAlignment = 128 * 1024 * 1024 // 128MB
	memoryRequest = alignMemory(memoryRequest, virtioMemAlignment)
	hostMemory = alignMemory(hostMemory, virtioMemAlignment)

	// Calculate smart resource limits for better overcommit:
	// - If container has explicit CPU limit, use that for MaxCPUs (capped at host)
	// - If no limit, allow access to all host CPUs for maximum flexibility
	// - If container has explicit memory limit, use 2x for hotplug headroom (capped at host)
	// - If no limit, allow access to all host memory for maximum flexibility
	maxCPUs := hostCPUs
	memoryHotplugSize := hostMemory

	// Check if explicit limits were set (vs defaults)
	hasExplicitCPULimit := b.Spec.Linux != nil &&
		b.Spec.Linux.Resources != nil &&
		b.Spec.Linux.Resources.CPU != nil &&
		(b.Spec.Linux.Resources.CPU.Quota != nil || b.Spec.Linux.Resources.CPU.Cpus != "")

	hasExplicitMemoryLimit := b.Spec.Linux != nil &&
		b.Spec.Linux.Resources != nil &&
		b.Spec.Linux.Resources.Memory != nil &&
		b.Spec.Linux.Resources.Memory.Limit != nil

	if hasExplicitCPULimit {
		// Container has explicit CPU limit - cap MaxCPUs to the request
		// This prevents wasting CPU scheduling slots on containers that don't need them
		maxCPUs = min(cpuRequest, hostCPUs)
	}

	if hasExplicitMemoryLimit {
		// Container has explicit memory limit - set hotplug to 2x for headroom
		// This allows some burst capacity while preventing unlimited growth
		memoryHotplugSize = min(memoryRequest*2, hostMemory)
		memoryHotplugSize = alignMemory(memoryHotplugSize, virtioMemAlignment)
	}

	// Build VM resource configuration
	resourceCfg := &vm.VMResourceConfig{
		BootCPUs:          cpuRequest,
		MaxCPUs:           maxCPUs,
		MemorySize:        memoryRequest,
		MemoryHotplugSize: memoryHotplugSize,
	}

	log.G(ctx).WithFields(log.Fields{
		"boot_cpus":              resourceCfg.BootCPUs,
		"max_cpus":               resourceCfg.MaxCPUs,
		"memory_size":            resourceCfg.MemorySize,
		"memory_hotplug_size":    resourceCfg.MemoryHotplugSize,
		"has_explicit_cpu_limit": hasExplicitCPULimit,
		"has_explicit_mem_limit": hasExplicitMemoryLimit,
		"host_cpus":              hostCPUs,
		"host_memory":            hostMemory,
	}).Info("VM resource configuration")

	vmState := filepath.Join(r.Bundle, "vm")
	if err := os.Mkdir(vmState, 0700); err != nil {
		return nil, errgrpc.ToGRPCf(err, "failed to create vm state directory %q", vmState)
	}
	vmi, err := s.vmInstance(ctx, r.ID, vmState, resourceCfg)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	m, err := setupMounts(ctx, vmi, r.ID, r.Rootfs, b.Rootfs, filepath.Join(r.Bundle, "mounts"))
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// Network setup: Allocate IP and create TAP device via NetworkManager
	// Note: netnsPath is provided for future CNI integration but currently unused
	netnsPath := filepath.Join("/var/run/netns", r.ID)
	netCfg, err := setupNetworking(ctx, s.networkManager, vmi, r.ID, netnsPath)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// Cleanup helper for network resources on failure
	cleanupNetwork := func() {
		env := &network.Environment{Id: r.ID}
		if err := s.networkManager.ReleaseNetworkResources(env); err != nil {
			log.G(ctx).WithError(err).WithField("id", r.ID).Warn("failed to cleanup network resources after failure")
		}
	}

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

	vmc, err := s.client()
	if err != nil {
		cleanupNetwork()
		return nil, errgrpc.ToGRPC(err)
	}
	// Start forwarding events
	sc, err := vmevents.NewTTRPCEventsClient(vmc).Stream(ctx, &ptypes.Empty{})
	if err != nil {
		cleanupNetwork()
		return nil, errgrpc.ToGRPC(err)
	}
	ns, _ := namespaces.Namespace(ctx)
	go func(ns string) {
		for {
			ev, err := sc.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, shutdown.ErrShutdown) || errors.Is(err, ttrpc.ErrClosed) {
					log.G(ctx).Info("vm event stream closed")
				} else {
					log.G(ctx).WithError(err).Error("vm event stream error")
				}
				return
			}
			s.send(ev)
		}
	}(ns)

	bundleFiles, err := b.Files()
	if err != nil {
		cleanupNetwork()
		return nil, errgrpc.ToGRPC(err)
	}

	bundleService := bundleAPI.NewTTRPCBundleClient(vmc)
	br, err := bundleService.Create(ctx, &bundleAPI.CreateRequest{
		ID:    r.ID,
		Files: bundleFiles,
	})
	if err != nil {
		cleanupNetwork()
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
		cleanupNetwork()
		return nil, errgrpc.ToGRPC(err)
	}

	// setupTime is the total time to setup the VM and everything needed
	// to proxy the create task request. This measures the overall
	// overhead of creating the container inside the VM.
	setupTime := time.Since(presetup)

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
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	resp, err := tc.Create(ctx, vr)
	if err != nil {
		log.G(ctx).WithError(err).Error("failed to create task")
		// Cleanup in reverse order: IO, then network
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

	s.mu.Lock()
	s.containers[r.ID] = c
	s.mu.Unlock()

	// Start CPU hotplug controller if conditions are met
	s.startCPUHotplugController(ctx, r.ID, vmi, resourceCfg)

	return &taskAPI.CreateTaskResponse{
		Pid: resp.Pid,
	}, nil
}

// Start a process.
// This forwards the Start request to the vminitd process running inside the VM via TTRPC.
func (s *service) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Info("starting container task")
	vmc, err := s.client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Start(ctx, r)
}

// Delete the initial process and container.
// This cleans up resources in the following order:
// 1. Deletes the task inside the VM.
// 2. Shuts down IO forwarding.
// 3. Releases network resources (IPs, TAP devices).
// 4. Removes the container from the shim's state.
func (s *service) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Info("deleting task")
	vmc, err := s.client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	resp, err := tc.Delete(ctx, r)
	if err == nil {
		s.mu.Lock()
		if c, ok := s.containers[r.ID]; ok {
			if r.ExecID != "" {
				if ioShutdown, ok := c.execShutdowns[r.ExecID]; ok {
					if err := ioShutdown(ctx); err != nil {
						log.G(ctx).WithError(err).WithField("exec", r.ExecID).Error("failed to shutdown exec io after delete")
					}
					delete(c.execShutdowns, r.ExecID)
				}
			} else {
				if c.ioShutdown != nil {
					if err := c.ioShutdown(ctx); err != nil {
						log.G(ctx).WithError(err).Error("failed to shutdown io after delete")
					}
				}
				for execID, ioShutdown := range c.execShutdowns {
					if err := ioShutdown(ctx); err != nil {
						log.G(ctx).WithError(err).WithField("exec", execID).Error("failed to shutdown exec io after delete")
					}
				}

				// Release network resources before deleting from map
				env := &network.Environment{Id: r.ID}
				if err := s.networkManager.ReleaseNetworkResources(env); err != nil {
					log.G(ctx).WithError(err).WithField("id", r.ID).Warn("failed to release network resources during delete")
				}

				// Stop CPU hotplug controller if running
				s.stopCPUHotplugController(ctx)

				delete(s.containers, r.ID)
			}
		}
		s.mu.Unlock()

	}
	return resp, err
}

// Exec an additional process inside the container
func (s *service) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Info("exec container")
	vmc, err := s.client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	rio := stdio.Stdio{
		Stdin:    r.Stdin,
		Stdout:   r.Stdout,
		Stderr:   r.Stderr,
		Terminal: r.Terminal,
	}

	cio, ioShutdown, err := s.forwardIO(ctx, s.vm, rio)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	s.mu.Lock()
	if c, ok := s.containers[r.ID]; ok {
		c.execShutdowns[r.ExecID] = ioShutdown
	} else {
		if ioShutdown != nil {
			if err := ioShutdown(ctx); err != nil {
				log.G(ctx).WithError(err).Error("failed to shutdown exec io after container not found")
			}
		}
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "container %q not found", r.ID)
	}
	s.mu.Unlock()

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
		s.mu.Lock()
		if c, ok := s.containers[r.ID]; ok {
			if ioShutdown, ok := c.execShutdowns[r.ExecID]; ok {
				if err := ioShutdown(ctx); err != nil {
					log.G(ctx).WithError(err).Error("failed to shutdown exec io after exec failure")
				}
				delete(c.execShutdowns, r.ExecID)
			}
		}
		s.mu.Unlock()
		return nil, errgrpc.ToGRPC(err)
	}

	return resp, nil
}

// ResizePty of a process
func (s *service) ResizePty(ctx context.Context, r *taskAPI.ResizePtyRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Info("resize pty")
	vmc, err := s.client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.ResizePty(ctx, r)
}

// State returns runtime state information for a process
func (s *service) State(ctx context.Context, r *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	vmc, err := s.client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	st, err := tc.State(ctx, r)
	if err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Info("state")
		return nil, errgrpc.ToGRPC(err)
	}
	log.G(ctx).WithFields(log.Fields{"status": st.Status, "id": r.ID, "exec": r.ExecID}).Info("state")

	return st, nil
}

// Pause the container
func (s *service) Pause(ctx context.Context, r *taskAPI.PauseRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Info("pause")
	vmc, err := s.client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Pause(ctx, r)
}

// Resume the container
func (s *service) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Info("resume")
	vmc, err := s.client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Resume(ctx, r)
}

// Kill a process with the provided signal
func (s *service) Kill(ctx context.Context, r *taskAPI.KillRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Info("kill")
	vmc, err := s.client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Kill(ctx, r)
}

// Pids returns all pids inside the container
func (s *service) Pids(ctx context.Context, r *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Info("all pids")
	vmc, err := s.client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Pids(ctx, r)
}

// CloseIO of a process
func (s *service) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID, "stdin": r.Stdin}).Info("close io")
	vmc, err := s.client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.CloseIO(ctx, r)
}

// Checkpoint the container
func (s *service) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Info("checkpoint")
	return &ptypes.Empty{}, nil
}

// Update a running container
func (s *service) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Info("update")
	vmc, err := s.client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Update(ctx, r)
}

// Wait for a process to exit
func (s *service) Wait(ctx context.Context, r *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Info("wait")
	vmc, err := s.client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Wait(ctx, r)
}

// Connect returns shim information such as the shim's pid
func (s *service) Connect(ctx context.Context, r *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Info("connect")
	vmc, err := s.client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
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
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Info("shutdown")

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.initiateShutdown != nil {
		// Please ensure that temporary resources have been cleaned up or registered
		// for cleanup before calling shutdown
		s.initiateShutdown()
		s.initiateShutdown = nil
	}

	return &ptypes.Empty{}, nil
}

func (s *service) Stats(ctx context.Context, r *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Info("stats")
	vmc, err := s.client()
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	return tc.Stats(ctx, r)
}

func (s *service) send(evt interface{}) {
	s.events <- evt
}

func (s *service) forward(ctx context.Context, publisher shim.Publisher) {
	ns, _ := namespaces.Namespace(ctx)
	ctx = namespaces.WithNamespace(context.Background(), ns)
	for e := range s.events {
		if e == nil {
			break
		}
		switch e := e.(type) {
		case *types.Envelope:
			// TODO: Transform event fields?
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
	publisher.Close()
	for e := range s.events {
		log.G(ctx).WithField("event", e).Error("ignored event after shutdown")
	}
}

// extractCPURequest extracts the CPU request from the OCI spec
// Returns the number of vCPUs requested, defaulting to 1 if not specified
func extractCPURequest(spec *specs.Spec) int {
	if spec == nil || spec.Linux == nil || spec.Linux.Resources == nil || spec.Linux.Resources.CPU == nil {
		return 1 // Default to 1 vCPU (improved from 2 for better overcommit)
	}

	cpu := spec.Linux.Resources.CPU

	// CPU.Quota and CPU.Period define CPU limits in microseconds
	// For example: Quota=200000, Period=100000 means 2 CPUs (200000/100000 = 2)
	if cpu.Quota != nil && cpu.Period != nil && *cpu.Period > 0 {
		cpus := int(*cpu.Quota / int64(*cpu.Period))
		if cpus > 0 {
			return cpus
		}
		// If quota is set but results in <1 CPU, give it 1 vCPU
		// (fractional CPU will be enforced by cgroups within the VM)
		return 1
	}

	// Fallback: check CPU.Cpus (cpuset format like "0-3" or "0,1,2,3")
	// This is less common but may be present
	if cpu.Cpus != "" {
		// Simple heuristic: count commas + 1, or parse ranges
		// For now, just return 1 as this requires more complex parsing
		return 1
	}

	return 1 // Default to 1 vCPU
}

// extractMemoryRequest extracts the memory request from the OCI spec
// Returns the memory limit in bytes, defaulting to 512MB if not specified
func extractMemoryRequest(spec *specs.Spec) int64 {
	const defaultMemory = 512 * 1024 * 1024 // 512MB default (was incorrectly commented as 512MB but set to 2GB)

	if spec == nil || spec.Linux == nil || spec.Linux.Resources == nil || spec.Linux.Resources.Memory == nil {
		return defaultMemory
	}

	mem := spec.Linux.Resources.Memory

	// Memory.Limit defines the memory limit in bytes
	if mem.Limit != nil && *mem.Limit > 0 {
		return *mem.Limit
	}

	return defaultMemory
}

// alignMemory rounds up the given memory value to the nearest multiple of alignment.
// This is required for virtio-mem which needs memory sizes aligned to 128MB.
func alignMemory(memory, alignment int64) int64 {
	if memory%alignment == 0 {
		return memory
	}
	return ((memory / alignment) + 1) * alignment
}

// getHostCPUCount returns the total number of CPUs available on the host
func getHostCPUCount() int {
	return goruntime.NumCPU()
}

// getHostMemoryTotal returns the total physical memory available on the host in bytes
// Reads from /proc/meminfo on Linux
func getHostMemoryTotal() (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("failed to open /proc/meminfo: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			// Format: "MemTotal:       16384000 kB"
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseInt(fields[1], 10, 64)
				if err != nil {
					return 0, fmt.Errorf("failed to parse MemTotal value: %w", err)
				}
				// Convert from KB to bytes
				return kb * 1024, nil
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("error reading /proc/meminfo: %w", err)
	}

	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}

// startCPUHotplugController starts the CPU hotplug controller for QEMU VMs
// It only starts if:
// - VM is a QEMU instance
// - MaxCPUs > BootCPUs (hotplug is possible)
// - CPU hotplug is enabled (via environment variable)
func (s *service) startCPUHotplugController(ctx context.Context, containerID string, vmi vm.Instance, resourceCfg *vm.VMResourceConfig) {
	// Check if CPU hotplug is enabled via environment variable
	if os.Getenv("BEACON_CPU_HOTPLUG_ENABLED") != "true" {
		log.G(ctx).Debug("cpu-hotplug: disabled (set BEACON_CPU_HOTPLUG_ENABLED=true to enable)")
		return
	}

	// Only enable for VMs that support hotplug (MaxCPUs > BootCPUs)
	if resourceCfg.MaxCPUs <= resourceCfg.BootCPUs {
		log.G(ctx).WithFields(log.Fields{
			"boot_cpus": resourceCfg.BootCPUs,
			"max_cpus":  resourceCfg.MaxCPUs,
		}).Debug("cpu-hotplug: not enabled (MaxCPUs == BootCPUs, no room for scaling)")
		return
	}

	// Type assert to check if this is a QEMU instance (only QEMU supports CPU hotplug currently)
	type qemuInstance interface {
		QMPClient() interface{}
	}

	qemuVM, ok := vmi.(qemuInstance)
	if !ok {
		log.G(ctx).Debug("cpu-hotplug: not supported (VM type does not support QMP)")
		return
	}

	// Get QMP client - type assert to *qemu.QMPClient directly
	qmpClientInterface := qemuVM.QMPClient()
	if qmpClientInterface == nil {
		log.G(ctx).Warn("cpu-hotplug: QMP client not available")
		return
	}

	qmpClient, ok := qmpClientInterface.(*qemu.QMPClient)
	if !ok {
		log.G(ctx).Warn("cpu-hotplug: QMP client type assertion failed")
		return
	}

	// Create controller configuration from environment variables
	config := cpuhotplug.DefaultConfig()

	// Allow environment variable overrides
	if interval := os.Getenv("BEACON_CPU_HOTPLUG_INTERVAL"); interval != "" {
		if d, err := time.ParseDuration(interval); err == nil {
			config.MonitorInterval = d
		}
	}

	// Create and start the controller
	controller := cpuhotplug.NewController(
		containerID,
		qmpClient,
		resourceCfg.BootCPUs,
		resourceCfg.MaxCPUs,
		config,
	)

	// Store controller in service
	s.mu.Lock()
	s.cpuHotplugController = controller
	s.mu.Unlock()

	// Start monitoring loop
	controller.Start(ctx)

	log.G(ctx).WithFields(log.Fields{
		"container_id": containerID,
		"boot_cpus":    resourceCfg.BootCPUs,
		"max_cpus":     resourceCfg.MaxCPUs,
	}).Info("cpu-hotplug: controller started")
}

// stopCPUHotplugController stops the CPU hotplug controller if running
func (s *service) stopCPUHotplugController(ctx context.Context) {
	s.mu.Lock()
	controller := s.cpuHotplugController
	s.cpuHotplugController = nil
	s.mu.Unlock()

	if controller != nil {
		controller.Stop()
		log.G(ctx).Info("cpu-hotplug: controller stopped")
	}
}
