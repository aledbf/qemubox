// Package task implements the containerd task service for qemubox runtime.
// It manages VM lifecycle, container creation, and I/O streams within the shim.
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
	"sync/atomic"
	"syscall"
	"time"

	cgroup2stats "github.com/containerd/cgroups/v3/cgroup2/stats"
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
	"github.com/containerd/typeurl/v2"
	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	bundleAPI "github.com/aledbf/qemubox/containerd/api/services/bundle/v1"
	systemAPI "github.com/aledbf/qemubox/containerd/api/services/system/v1"
	"github.com/aledbf/qemubox/containerd/api/services/vmevents/v1"
	"github.com/aledbf/qemubox/containerd/internal/config"
	"github.com/aledbf/qemubox/containerd/internal/host/network"
	"github.com/aledbf/qemubox/containerd/internal/host/vm"
	"github.com/aledbf/qemubox/containerd/internal/host/vm/qemu"
	"github.com/aledbf/qemubox/containerd/internal/shim/bundle"
	"github.com/aledbf/qemubox/containerd/internal/shim/cpuhotplug"
	"github.com/aledbf/qemubox/containerd/internal/shim/memhotplug"
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

// NewTaskService creates a new instance of a task service
func NewTaskService(ctx context.Context, publisher shim.Publisher, sd shutdown.Service) (taskAPI.TTRPCTaskService, error) {
	// Initialize network manager for this service
	nm, err := initNetworkManager(ctx)
	if err != nil {
		return nil, fmt.Errorf("initialize network manager: %w", err)
	}

	s := &service{
		events:                   make(chan any, eventChannelBuffer),
		cpuHotplugControllers:    make(map[string]cpuhotplug.CPUHotplugController),
		memoryHotplugControllers: make(map[string]memhotplug.MemoryHotplugController),
		networkManager:           nm,
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
	ioShutdown func(context.Context) error

	execShutdowns map[string]func(context.Context) error
	pid           uint32
}

// service is the shim implementation of a remote shim over GRPC
type service struct {
	// Separate mutexes for different concerns to avoid contention
	containerMu  sync.Mutex // Protects container field
	controllerMu sync.Mutex // Protects hotplug controllers
	vmMu         sync.Mutex // Protects VM instance

	// vm is the VM instance used to run the container
	vm vm.Instance

	// networkManager handles network resource allocation and cleanup
	networkManager network.NetworkManager

	// Single container (qemubox enforces 1 container per VM per shim)
	container   *container
	containerID string

	// Per-container hotplug controllers (QEMU only)
	// Using maps prevents race conditions when multiple VMs are created concurrently
	cpuHotplugControllers    map[string]cpuhotplug.CPUHotplugController    // key: containerID
	memoryHotplugControllers map[string]memhotplug.MemoryHotplugController // key: containerID

	events chan any

	initiateShutdown    func()
	eventsClosed        atomic.Bool
	eventsCloseOnce     sync.Once   // Ensures events channel is only closed once
	intentionalShutdown atomic.Bool // Set when we intentionally close VM (not a crash)
	deletionInProgress  atomic.Bool // Set during Delete to prevent Create requests
	shutdownSvc         shutdown.Service
	inflight            atomic.Int64
}

// vmClient encapsulates VM communication for controller callbacks.
// This eliminates closures that capture the entire service, making dependencies explicit.
type vmClient struct {
	dialClient func(context.Context) (*ttrpc.Client, error)
}

// getCPUStats retrieves CPU usage statistics from the container via TTRPC.
func (v *vmClient) getCPUStats(ctx context.Context, containerID string) (uint64, uint64, error) {
	vmc, err := v.dialClient(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in CPU stats")
		}
	}()
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	resp, err := tc.Stats(ctx, &taskAPI.StatsRequest{ID: containerID})
	if err != nil {
		return 0, 0, err
	}
	if resp.GetStats() == nil {
		return 0, 0, fmt.Errorf("missing stats payload")
	}

	var metrics cgroup2stats.Metrics
	if err := typeurl.UnmarshalTo(resp.Stats, &metrics); err != nil {
		return 0, 0, err
	}

	cpu := metrics.GetCPU()
	if cpu == nil {
		return 0, 0, fmt.Errorf("missing CPU stats")
	}

	return cpu.GetUsageUsec(), cpu.GetThrottledUsec(), nil
}

// offlineCPU takes a CPU offline in the guest VM.
func (v *vmClient) offlineCPU(ctx context.Context, cpuID int) error {
	vmc, err := v.dialClient(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in CPU offline")
		}
	}()
	client := systemAPI.NewTTRPCSystemClient(vmc)
	_, err = client.OfflineCPU(ctx, &systemAPI.OfflineCPURequest{CpuID: uint32(cpuID)})
	return err
}

// onlineCPU brings a CPU online in the guest VM.
func (v *vmClient) onlineCPU(ctx context.Context, cpuID int) error {
	vmc, err := v.dialClient(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in CPU online")
		}
	}()
	client := systemAPI.NewTTRPCSystemClient(vmc)
	_, err = client.OnlineCPU(ctx, &systemAPI.OnlineCPURequest{CpuID: uint32(cpuID)})
	return err
}

// getMemoryStats retrieves memory usage statistics from the container via TTRPC.
func (v *vmClient) getMemoryStats(ctx context.Context, containerID string) (int64, error) {
	vmc, err := v.dialClient(ctx)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in memory stats")
		}
	}()
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	resp, err := tc.Stats(ctx, &taskAPI.StatsRequest{ID: containerID})
	if err != nil {
		return 0, err
	}
	if resp.GetStats() == nil {
		return 0, fmt.Errorf("missing stats payload")
	}

	var metrics cgroup2stats.Metrics
	if err := typeurl.UnmarshalTo(resp.Stats, &metrics); err != nil {
		return 0, err
	}

	mem := metrics.GetMemory()
	if mem == nil {
		return 0, fmt.Errorf("missing memory stats")
	}

	return int64(mem.GetUsage()), nil
}

// offlineMemory takes memory offline in the guest VM.
func (v *vmClient) offlineMemory(ctx context.Context, memoryID int) error {
	vmc, err := v.dialClient(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in memory offline")
		}
	}()
	client := systemAPI.NewTTRPCSystemClient(vmc)
	_, err = client.OfflineMemory(ctx, &systemAPI.OfflineMemoryRequest{MemoryID: uint32(memoryID)})
	return err
}

// onlineMemory brings memory online in the guest VM.
func (v *vmClient) onlineMemory(ctx context.Context, memoryID int) error {
	vmc, err := v.dialClient(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in memory online")
		}
	}()
	client := systemAPI.NewTTRPCSystemClient(vmc)
	_, err = client.OnlineMemory(ctx, &systemAPI.OnlineMemoryRequest{MemoryID: uint32(memoryID)})
	return err
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
	defer func() {
		if err := syscall.Close(fd); err != nil {
			log.L.WithError(err).Warn("failed to close /dev/kvm fd")
		}
	}()

	// Kernel docs says:
	//     Applications should refuse to run if KVM_GET_API_VERSION returns a value other than 12.
	// See https://docs.kernel.org/virt/kvm/api.html#kvm-get-api-version
	// Note: This is Linux-specific KVM functionality. unix.SYS_IOCTL is deprecated on macOS,
	// but this code only runs on Linux systems with KVM support.
	// Linux-only KVM check using ioctl API version.
	apiVersion, _, errno := unix.RawSyscall(unix.SYS_IOCTL, uintptr(fd), ioctlKVMGetAPIVersion, 0)
	if errno != 0 {
		return fmt.Errorf("failed to get KVM API version: %w. You may have insufficient permissions", errno)
	}
	if apiVersion != expectedKVMAPIVersion {
		return fmt.Errorf("KVM API version mismatch; expected %d, got %d", expectedKVMAPIVersion, apiVersion)
	}

	return nil
}

func (s *service) shutdown(ctx context.Context) error {
	// Lock all mutexes in consistent order to prevent deadlocks
	s.containerMu.Lock()
	defer s.containerMu.Unlock()
	s.vmMu.Lock()
	defer s.vmMu.Unlock()

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

	// Shutdown VM (idempotent - may be nil if Delete already cleared it)
	// IMPORTANT: This must happen BEFORE network cleanup so QEMU can exit cleanly
	// before CNI tears down the TAP interface
	if s.vm != nil {
		if err := s.vm.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("vm shutdown: %w", err))
		}
		// Clear s.vm if not already nil
		s.vm = nil
	}

	// Release network resources AFTER VM shutdown
	// This ensures QEMU has exited and closed TAP FDs before CNI deletes the TAP interface
	if s.containerID != "" {
		env := &network.Environment{ID: s.containerID}
		if err := s.networkManager.ReleaseNetworkResources(ctx, env); err != nil {
			log.G(ctx).WithError(err).WithField("id", s.containerID).Warn("failed to release network resources")
		}
	}

	// Close network manager (this closes the database)
	if s.networkManager != nil {
		if err := s.networkManager.Close(); err != nil {
			errs = append(errs, fmt.Errorf("network manager close: %w", err))
		}
	}

	// Stop forwarding events without blocking shutdown.
	s.eventsClosed.Store(true)
	s.eventsCloseOnce.Do(func() {
		close(s.events)
	})

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

type resourceConfigInfo struct {
	hasExplicitCPULimit    bool
	hasExplicitMemoryLimit bool
	hostCPUs               int
	hostMemory             int64
}

func loadBundleForCreate(ctx context.Context, bundlePath string) (*bundle.Bundle, error) {
	// QEMU requires KVM - check if available
	if err := checkKVM(); err != nil {
		return nil, err
	}

	// Load the OCI bundle and apply transformers to get the bundle that'll be
	// set up on the VM side.
	// We remove the network namespace to allow the container to share the VM's
	// network namespace (where eth0 is configured).
	return bundle.Load(ctx, bundlePath, transformBindMounts, disableNetworkNamespace)
}

func (s *service) initVMInstance(ctx context.Context, r *taskAPI.CreateTaskRequest, resourceCfg *vm.VMResourceConfig) (vm.Instance, error) {
	vmStateRoot := os.Getenv("QEMUBOX_VM_STATE_DIR")
	vmState := filepath.Join(r.Bundle, "vm")
	if vmStateRoot != "" {
		namespace, ok := namespaces.Namespace(ctx)
		if !ok || namespace == "" {
			namespace = "default"
		}
		vmState = filepath.Join(vmStateRoot, namespace, r.ID)
	}
	if err := os.MkdirAll(vmState, 0700); err != nil {
		return nil, errgrpc.ToGRPCf(err, "failed to create vm state directory %q", vmState)
	}
	return s.vmInstance(ctx, r.ID, vmState, resourceCfg)
}

func computeResourceConfig(ctx context.Context, spec *specs.Spec) (*vm.VMResourceConfig, resourceConfigInfo) {
	// Extract resource requests from OCI spec
	cpuRequest := extractCPURequest(spec)
	memoryRequest := extractMemoryRequest(spec)

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
	hasExplicitCPULimit := spec.Linux != nil &&
		spec.Linux.Resources != nil &&
		spec.Linux.Resources.CPU != nil &&
		(spec.Linux.Resources.CPU.Quota != nil || spec.Linux.Resources.CPU.Cpus != "")

	hasExplicitMemoryLimit := spec.Linux != nil &&
		spec.Linux.Resources != nil &&
		spec.Linux.Resources.Memory != nil &&
		spec.Linux.Resources.Memory.Limit != nil

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

	resourceCfg := &vm.VMResourceConfig{
		BootCPUs:          cpuRequest,
		MaxCPUs:           maxCPUs,
		MemorySize:        memoryRequest,
		MemoryHotplugSize: memoryHotplugSize,
	}

	return resourceCfg, resourceConfigInfo{
		hasExplicitCPULimit:    hasExplicitCPULimit,
		hasExplicitMemoryLimit: hasExplicitMemoryLimit,
		hostCPUs:               hostCPUs,
		hostMemory:             hostMemory,
	}
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
				// Check if this was an intentional shutdown (Delete/Shutdown called)
				// vs unexpected VM crash
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
						newClient, dialErr := s.dialClientWithRetry(ctx, 2*time.Second)
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
				// VM died unexpectedly - trigger shim shutdown to clean up and exit
				s.requestShutdownAndExit(ctx, "vm event stream closed")
				return
			}
			s.send(ev)
		}
	}()

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
	s.vmMu.Lock()
	hasVM := s.vm != nil
	s.vmMu.Unlock()

	// Double-check deletion flag (prevent TOCTOU race)
	if s.deletionInProgress.Load() {
		return nil, errgrpc.ToGRPCf(errdefs.ErrAlreadyExists, "shim is deleting container; requires fresh shim per container")
	}

	if hasContainer || hasVM {
		return nil, errgrpc.ToGRPCf(errdefs.ErrAlreadyExists, "shim already running a container; requires fresh shim per container")
	}

	presetup := time.Now()

	b, err := loadBundleForCreate(ctx, r.Bundle)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	resourceCfg, _ := computeResourceConfig(ctx, &b.Spec)

	log.G(ctx).WithFields(log.Fields{
		"boot_cpus":  resourceCfg.BootCPUs,
		"max_cpus":   resourceCfg.MaxCPUs,
		"memory_mb":  resourceCfg.MemorySize / (1024 * 1024),
		"hotplug_mb": resourceCfg.MemoryHotplugSize / (1024 * 1024),
	}).Debug("VM resource configuration")

	vmi, err := s.initVMInstance(ctx, r, resourceCfg)
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

	// Cleanup helper for network resources on failure - defined immediately after allocation
	// Use defer pattern to ensure cleanup on any error path
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
	// Defer cleanup - will be called on error, but skipped if we succeed
	defer func() {
		if !networkCleanupDone {
			cleanupNetwork()
		}
	}()

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

	vmc, err := s.client()
	if err != nil {
		cleanupNetwork()
		return nil, errgrpc.ToGRPC(err)
	}
	// Start forwarding events using a detached context (not request-scoped)
	// Event forwarding must continue for the service lifetime, not tied to this RPC.
	// Cleanup happens in shutdown() callback, not via context cancellation.
	ns, _ := namespaces.Namespace(ctx)
	eventCtx := namespaces.WithNamespace(context.WithoutCancel(ctx), ns)
	if err := s.startEventForwarder(eventCtx, vmc); err != nil {
		cleanupNetwork()
		return nil, errgrpc.ToGRPC(err)
	}

	rpcClient, err := s.dialClient(ctx)
	if err != nil {
		cleanupNetwork()
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := rpcClient.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close rpc client in Create")
		}
	}()

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
	tc := taskAPI.NewTTRPCTaskClient(rpcClient)
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

	c.pid = resp.Pid
	s.containerMu.Lock()
	s.container = c
	s.containerID = r.ID
	s.containerMu.Unlock()

	// Start CPU hotplug controller if conditions are met
	s.startCPUHotplugController(ctx, r.ID, vmi, resourceCfg)

	// Start memory hotplug controller if conditions are met
	s.startMemoryHotplugController(ctx, r.ID, vmi, resourceCfg)

	// Mark network cleanup as done since we succeeded
	networkCleanupDone = true

	return &taskAPI.CreateTaskResponse{
		Pid: resp.Pid,
	}, nil
}

// Start a process.
// This forwards the Start request to the vminitd process running inside the VM via TTRPC.
func (s *service) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	vmc, err := s.dialClientWithRetry(ctx, 30*time.Second)
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

func (s *service) dialClientWithRetry(ctx context.Context, maxWait time.Duration) (*ttrpc.Client, error) {
	deadline := time.Now().Add(maxWait)
	for {
		vmc, err := s.dialClient(ctx)
		if err == nil {
			return vmc, nil
		}
		if !isTransientVsockError(err) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func isTransientVsockError(err error) bool {
	if err == nil {
		return false
	}
	if errdefs.IsFailedPrecondition(err) || errdefs.IsUnavailable(err) {
		return true
	}
	if errors.Is(err, ttrpc.ErrClosed) || errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	msg := err.Error()
	if strings.Contains(msg, "ttrpc: closed") || strings.Contains(msg, "broken pipe") {
		return true
	}
	if !strings.Contains(msg, "dial vsock") {
		return false
	}
	return strings.Contains(msg, "no such device") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "connection refused")
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
// This cleans up resources in the following order:
// 1. Deletes the task inside the VM.
// 2. Shuts down IO forwarding.
// 3. Releases network resources (IPs, TAP devices).
// 4. Removes the container from the shim's state.
func (s *service) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	s.inflight.Add(1)
	defer s.inflight.Add(-1)
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Debug("deleting task")

	// Mark deletion in progress to prevent new Create requests
	// Only do this for container deletion (not exec)
	if r.ExecID == "" {
		if !s.deletionInProgress.CompareAndSwap(false, true) {
			return nil, errgrpc.ToGRPCf(errdefs.ErrAlreadyExists, "delete already in progress")
		}
		// Note: Don't clear this flag - keep it true until shim exits
	}

	vmc, err := s.dialClient(ctx)
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
		// On error, attempt cleanup for container (not exec) deletion
		if r.ExecID == "" && s.vm != nil {
			// Clean up IO before VM shutdown to prevent FD leaks
			s.containerMu.Lock()
			if s.container != nil && s.container.ioShutdown != nil {
				if ioErr := s.container.ioShutdown(ctx); ioErr != nil {
					log.G(ctx).WithError(ioErr).Warn("failed to shutdown io after delete failure")
				}
			}
			s.containerMu.Unlock()

			s.intentionalShutdown.Store(true)
			if shutdownErr := s.vm.Shutdown(ctx); shutdownErr != nil {
				if !isProcessAlreadyFinished(shutdownErr) {
					log.G(ctx).WithError(shutdownErr).Warn("failed to shutdown VM after delete error")
				}
			}
		}
		return resp, err
	}

	// Delete succeeded - clean up resources
	// Extract cleanup targets while holding appropriate locks
	var ioShutdowns []func(context.Context) error
	var cpuController cpuhotplug.CPUHotplugController
	var memController memhotplug.MemoryHotplugController
	var needNetworkClean, needVMShutdown bool

	s.containerMu.Lock()
	if s.container != nil && s.containerID == r.ID {
		if r.ExecID != "" {
			// Exec process cleanup
			if ioShutdown, ok := s.container.execShutdowns[r.ExecID]; ok {
				ioShutdowns = append(ioShutdowns, ioShutdown)
				delete(s.container.execShutdowns, r.ExecID)
			}
		} else {
			// Container cleanup
			if s.container.ioShutdown != nil {
				ioShutdowns = append(ioShutdowns, s.container.ioShutdown)
			}
			for _, ioShutdown := range s.container.execShutdowns {
				ioShutdowns = append(ioShutdowns, ioShutdown)
			}
			needNetworkClean = true
			s.container = nil
			s.containerID = ""
		}
	}
	s.containerMu.Unlock()

	// Get controllers and VM state (only for container, not exec)
	if r.ExecID == "" {
		s.controllerMu.Lock()
		cpuController = s.cpuHotplugControllers[r.ID]
		delete(s.cpuHotplugControllers, r.ID)
		memController = s.memoryHotplugControllers[r.ID]
		delete(s.memoryHotplugControllers, r.ID)
		s.controllerMu.Unlock()

		s.vmMu.Lock()
		needVMShutdown = s.vm != nil
		s.vmMu.Unlock()
	}

	// Execute cleanup operations (no locks held)
	// Shutdown I/O streams
	for i, ioShutdown := range ioShutdowns {
		if err := ioShutdown(ctx); err != nil {
			if i == 0 && r.ExecID == "" {
				log.G(ctx).WithError(err).Error("failed to shutdown io after delete")
			} else {
				log.G(ctx).WithError(err).WithField("exec", r.ExecID).Error("failed to shutdown exec io after delete")
			}
		}
	}

	// Stop CPU hotplug controller
	if cpuController != nil {
		cpuController.Stop()
	}

	// Stop memory hotplug controller
	if memController != nil {
		memController.Stop()
	}

	// Shutdown VM if needed (one VM per container)
	// IMPORTANT: This must happen BEFORE network cleanup so QEMU can exit cleanly
	// before CNI tears down the TAP interface
	if needVMShutdown {
		log.G(ctx).Info("container deleted, shutting down VM")
		s.intentionalShutdown.Store(true)
		if err := s.vm.Shutdown(ctx); err != nil {
			if !isProcessAlreadyFinished(err) {
				log.G(ctx).WithError(err).Warn("failed to shutdown VM after container deleted")
			}
		}

		// CRITICAL: Clear s.vm synchronously BEFORE returning
		// This ensures Create cannot see s.vm != nil after Delete returns
		s.vmMu.Lock()
		s.vm = nil
		s.vmMu.Unlock()
		log.G(ctx).Info("VM state cleared, scheduling shim exit")

		// Schedule async exit (non-blocking, allows Delete to return)
		go s.requestShutdownAndExit(ctx, "container deleted")
	}

	// Release network resources AFTER VM shutdown
	// This ensures QEMU has exited and closed TAP FDs before CNI deletes the TAP interface
	if needNetworkClean {
		env := &network.Environment{ID: r.ID}
		if err := s.networkManager.ReleaseNetworkResources(ctx, env); err != nil {
			log.G(ctx).WithError(err).WithField("id", r.ID).Warn("failed to release network resources during delete")
		}
	}

	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Debug("task deleted successfully")
	return resp, nil
}

// Exec an additional process inside the container
func (s *service) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Debug("exec container")

	vmc, err := s.dialClient(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	defer func() {
		if err := vmc.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("failed to close client in Exec")
		}
	}()

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

// ResizePty of a process
func (s *service) ResizePty(ctx context.Context, r *taskAPI.ResizePtyRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Debug("resize pty")

	vmc, err := s.dialClient(ctx)
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

// State returns runtime state information for a process
func (s *service) State(ctx context.Context, r *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {

	vmc, err := s.dialClient(ctx)
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

// Pause the container
func (s *service) Pause(ctx context.Context, r *taskAPI.PauseRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("pause")

	vmc, err := s.dialClient(ctx)
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

// Resume the container
func (s *service) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("resume")

	vmc, err := s.dialClient(ctx)
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

// Kill a process with the provided signal
func (s *service) Kill(ctx context.Context, r *taskAPI.KillRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Debug("kill")

	vmc, err := s.dialClient(ctx)
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

// Pids returns all pids inside the container
func (s *service) Pids(ctx context.Context, r *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("pids")

	vmc, err := s.dialClient(ctx)
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

// CloseIO of a process
func (s *service) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID, "stdin": r.Stdin}).Debug("close io")

	vmc, err := s.dialClient(ctx)
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

// Checkpoint the container
func (s *service) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("checkpoint")
	return &ptypes.Empty{}, nil
}

// Update a running container
func (s *service) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("update")

	vmc, err := s.dialClient(ctx)
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

// Wait for a process to exit
func (s *service) Wait(ctx context.Context, r *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID, "exec": r.ExecID}).Debug("wait")

	vmc, err := s.dialClient(ctx)
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

// Connect returns shim information such as the shim's pid
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

	vmc, err := s.dialClient(ctx)
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

	// Mark as intentional shutdown
	s.intentionalShutdown.Store(true)

	if s.shutdownSvc != nil {
		go s.requestShutdownAndExit(ctx, "shutdown rpc")
	} else if s.initiateShutdown != nil {
		// Please ensure that temporary resources have been cleaned up or registered
		// for cleanup before calling shutdown
		s.initiateShutdown()
		s.initiateShutdown = nil
	}

	return &ptypes.Empty{}, nil
}

func (s *service) Stats(ctx context.Context, r *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	log.G(ctx).WithFields(log.Fields{"id": r.ID}).Debug("stats")

	vmc, err := s.dialClient(ctx)
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
	// Create a fresh background context with the namespace preserved.
	// This is intentional: event forwarding must continue even if the parent context is cancelled,
	// so we need an independent context that will live until the publisher is closed.
	ctx = namespaces.WithNamespace(context.WithoutCancel(ctx), ns)

	// Signal that we're ready to process events
	close(ready)

	for e := range s.events {
		switch e := e.(type) {
		case *types.Envelope:
			// Note: consider transforming event fields.
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
	const defaultMemory = 512 * 1024 * 1024 // 512MB default

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
	defer func() {
		if err := f.Close(); err != nil {
			log.L.WithError(err).Warn("failed to close /proc/meminfo")
		}
	}()

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
	// Only enable for VMs that support hotplug (MaxCPUs > BootCPUs)
	if resourceCfg.MaxCPUs <= resourceCfg.BootCPUs {
		log.G(ctx).WithFields(log.Fields{
			"boot_cpus": resourceCfg.BootCPUs,
			"max_cpus":  resourceCfg.MaxCPUs,
		}).Debug("cpu-hotplug: not enabled (MaxCPUs == BootCPUs, no room for scaling)")
		return
	}

	// Type assert to check if this is a QEMU instance (only QEMU supports CPU hotplug currently)
	qemuVM, ok := vmi.(*qemu.Instance)
	if !ok {
		return
	}

	// Get QMP client
	qmpClient := qemuVM.QMPClient()
	if qmpClient == nil {
		log.G(ctx).Warn("cpu-hotplug: QMP client not available")
		return
	}

	if cpus, err := qmpClient.QueryHotpluggableCPUs(ctx); err != nil {
		log.G(ctx).WithError(err).Warn("cpu-hotplug: failed to query hotpluggable CPUs")
	} else {
		sample := ""
		if len(cpus) > 0 {
			sample = fmt.Sprintf("type=%s props=%v", cpus[0].Type, cpus[0].Props)
		}
		log.G(ctx).WithFields(log.Fields{
			"count":  len(cpus),
			"sample": sample,
		}).Debug("cpu-hotplug: hotpluggable CPU slots")
	}

	// Create controller configuration from config file
	var cpuConfig cpuhotplug.Config
	cfg, err := config.Get()
	if err != nil {
		log.G(ctx).WithError(err).Error("cpu-hotplug: failed to load config, using defaults")
		cpuConfig = cpuhotplug.DefaultConfig()
	} else {
		// Parse durations from config
		monitorInterval, err := time.ParseDuration(cfg.CPUHotplug.MonitorInterval)
		if err != nil {
			log.G(ctx).WithError(err).Error("cpu-hotplug: invalid monitor_interval, using defaults")
			cpuConfig = cpuhotplug.DefaultConfig()
		} else {
			scaleUpCooldown, _ := time.ParseDuration(cfg.CPUHotplug.ScaleUpCooldown)
			scaleDownCooldown, _ := time.ParseDuration(cfg.CPUHotplug.ScaleDownCooldown)

			cpuConfig = cpuhotplug.Config{
				MonitorInterval:      monitorInterval,
				ScaleUpCooldown:      scaleUpCooldown,
				ScaleDownCooldown:    scaleDownCooldown,
				ScaleUpThreshold:     cfg.CPUHotplug.ScaleUpThreshold,
				ScaleDownThreshold:   cfg.CPUHotplug.ScaleDownThreshold,
				ScaleUpThrottleLimit: cfg.CPUHotplug.ScaleUpThrottleLimit,
				ScaleUpStability:     cfg.CPUHotplug.ScaleUpStability,
				ScaleDownStability:   cfg.CPUHotplug.ScaleDownStability,
				EnableScaleDown:      cfg.CPUHotplug.EnableScaleDown,
			}
		}
	}

	// Create vmClient with explicit dependencies (no closure capturing entire service)
	vmc := &vmClient{dialClient: s.dialClient}

	controller := cpuhotplug.NewController(
		containerID,
		qmpClient,
		func(ctx context.Context) (uint64, uint64, error) {
			return vmc.getCPUStats(ctx, containerID)
		},
		vmc.offlineCPU,
		vmc.onlineCPU,
		resourceCfg.BootCPUs,
		resourceCfg.MaxCPUs,
		cpuConfig,
	)

	// Store controller in service by container ID (prevents race condition)
	// Controller may be a no-op if hotplug not needed (maxCPUs <= bootCPUs)
	s.controllerMu.Lock()
	s.cpuHotplugControllers[containerID] = controller
	s.controllerMu.Unlock()

	// Start monitoring loop with detached context (not request context)
	// The controller needs to run for the lifetime of the container, not just the CreateTask RPC.
	// Cleanup happens via controller.Stop() in shutdown/delete, not via context cancellation.
	// Note: controller.Start() is non-blocking and launches its own goroutine internally.
	// The controller manages its own lifecycle and will log any errors in its monitor loop.
	controller.Start(context.WithoutCancel(ctx))

	log.G(ctx).WithFields(log.Fields{
		"container_id": containerID,
		"boot_cpus":    resourceCfg.BootCPUs,
		"max_cpus":     resourceCfg.MaxCPUs,
	}).Info("cpu-hotplug: controller started (monitoring in background)")
}

func (s *service) startMemoryHotplugController(ctx context.Context, containerID string, vmi vm.Instance, resourceCfg *vm.VMResourceConfig) {
	// Type assert to check if this is a QEMU instance (only QEMU supports memory hotplug currently)
	qemuVM, ok := vmi.(*qemu.Instance)
	if !ok {
		return
	}

	// Get QMP client
	qmpClient := qemuVM.QMPClient()
	if qmpClient == nil {
		log.G(ctx).Warn("memory-hotplug: QMP client not available")
		return
	}

	// Query current memory configuration
	if summary, err := qmpClient.QueryMemorySizeSummary(ctx); err != nil {
		log.G(ctx).WithError(err).Warn("memory-hotplug: failed to query memory size")
	} else {
		log.G(ctx).WithFields(log.Fields{
			"base_memory_mb":    summary.BaseMemory / (1024 * 1024),
			"plugged_memory_mb": summary.PluggedMemory / (1024 * 1024),
		}).Debug("memory-hotplug: initial memory state")
	}

	// Create controller configuration from config file
	var memConfig memhotplug.Config
	cfg, err := config.Get()
	if err != nil {
		log.G(ctx).WithError(err).Error("memory-hotplug: failed to load config, using defaults")
		memConfig = memhotplug.DefaultConfig()
	} else {
		// Parse durations from config
		monitorInterval, err := time.ParseDuration(cfg.MemHotplug.MonitorInterval)
		if err != nil {
			log.G(ctx).WithError(err).Error("memory-hotplug: invalid monitor_interval, using defaults")
			memConfig = memhotplug.DefaultConfig()
		} else {
			scaleUpCooldown, _ := time.ParseDuration(cfg.MemHotplug.ScaleUpCooldown)
			scaleDownCooldown, _ := time.ParseDuration(cfg.MemHotplug.ScaleDownCooldown)

			memConfig = memhotplug.Config{
				MonitorInterval:    monitorInterval,
				ScaleUpCooldown:    scaleUpCooldown,
				ScaleDownCooldown:  scaleDownCooldown,
				ScaleUpThreshold:   cfg.MemHotplug.ScaleUpThreshold,
				ScaleDownThreshold: cfg.MemHotplug.ScaleDownThreshold,
				OOMSafetyMarginMB:  cfg.MemHotplug.OOMSafetyMarginMB,
				IncrementSize:      cfg.MemHotplug.IncrementSizeMB * 1024 * 1024, // Convert MB to bytes
				ScaleUpStability:   cfg.MemHotplug.ScaleUpStability,
				ScaleDownStability: cfg.MemHotplug.ScaleDownStability,
				EnableScaleDown:    cfg.MemHotplug.EnableScaleDown,
			}
		}
	}

	if memConfig.EnableScaleDown {
		log.G(ctx).Warn("memory-hotplug: scale-down enabled (EXPERIMENTAL)")
	}

	// Create vmClient with explicit dependencies (no closure capturing entire service)
	vmc := &vmClient{dialClient: s.dialClient}

	controller := memhotplug.NewController(
		containerID,
		qmpClient,
		func(ctx context.Context) (int64, error) {
			return vmc.getMemoryStats(ctx, containerID)
		},
		vmc.offlineMemory,
		vmc.onlineMemory,
		resourceCfg.MemorySize,
		resourceCfg.MemoryHotplugSize,
		memConfig,
	)

	// Store controller in service by container ID (prevents race condition)
	// Controller may be a no-op if hotplug not needed (maxMemory <= bootMemory)
	s.controllerMu.Lock()
	s.memoryHotplugControllers[containerID] = controller
	s.controllerMu.Unlock()

	// Start monitoring loop with detached context (not request context)
	// The controller needs to run for the lifetime of the container, not just the CreateTask RPC.
	// Cleanup happens via controller.Stop() in shutdown/delete, not via context cancellation.
	// Note: controller.Start() is non-blocking and launches its own goroutine internally.
	// The controller manages its own lifecycle and will log any errors in its monitor loop.
	controller.Start(context.WithoutCancel(ctx))

	log.G(ctx).WithFields(log.Fields{
		"container_id":   containerID,
		"boot_memory_mb": resourceCfg.MemorySize / (1024 * 1024),
		"max_memory_mb":  resourceCfg.MemoryHotplugSize / (1024 * 1024),
	}).Info("memory-hotplug: controller started (monitoring in background)")
}
