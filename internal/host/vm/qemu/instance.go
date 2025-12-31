//go:build linux

package qemu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/containerd/log"

	"github.com/aledbf/qemubox/containerd/internal/config"
	"github.com/aledbf/qemubox/containerd/internal/host/vm"
	"github.com/aledbf/qemubox/containerd/internal/paths"
)

const (
	// defaultMemorySlots is the number of memory hotplug slots.
	// 8 slots * 128MB min increment = 1GB max hotplug capacity
	// Trade-off: More slots = more QEMU overhead, fewer = less flexibility
	defaultMemorySlots = 8

	// minGuestCID is the minimum valid vsock guest CID.
	// CIDs 0-2 are reserved (hypervisor, reserved, host).
	minGuestCID uint32 = 3

	// maxGuestCID is the maximum CID we'll scan to before wrapping.
	// Using a reasonable limit to avoid scanning billions of files.
	maxGuestCID uint32 = 65535

	// cidLockDir is the subdirectory for CID lock files.
	cidLockDir = "cid"
)

// allocateGuestCID allocates a unique vsock CID using lock files.
// Each CID has a corresponding lock file; the caller holds an exclusive flock
// for the lifetime of the VM. When the shim process exits (normally or crashes),
// the kernel automatically releases the lock, making the CID available for reuse.
//
// Returns the allocated CID and the lock file handle. The caller must keep the
// file open for the duration of the VM and close it on shutdown.
func allocateGuestCID() (uint32, *os.File, error) {
	cfg, err := config.Get()
	if err != nil {
		return 0, nil, fmt.Errorf("failed to get config: %w", err)
	}

	lockDir := filepath.Join(cfg.Paths.StateDir, cidLockDir)
	if err := os.MkdirAll(lockDir, 0750); err != nil {
		return 0, nil, fmt.Errorf("failed to create CID lock directory: %w", err)
	}

	// Scan for first available CID
	for cid := minGuestCID; cid <= maxGuestCID; cid++ {
		lockPath := filepath.Join(lockDir, fmt.Sprintf("%d.lock", cid))

		// Try to open/create and lock the file
		f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0600)
		if err != nil {
			continue // Try next CID
		}

		// Try non-blocking exclusive lock
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != nil {
			// Lock held by another process, try next CID
			_ = f.Close()
			continue
		}

		// Successfully acquired lock - this CID is ours
		return cid, f, nil
	}

	return 0, nil, fmt.Errorf("no available vsock CID in range [%d, %d]", minGuestCID, maxGuestCID)
}

func findQemu() (string, error) {
	cfg, err := config.Get()
	if err != nil {
		return "", fmt.Errorf("failed to get config: %w", err)
	}

	path := paths.QemuPath(cfg.Paths)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("qemu-system-x86_64 binary not found at %s", path)
}

// findKernel returns the path to the kernel binary for QEMU
func findKernel() (string, error) {
	cfg, err := config.Get()
	if err != nil {
		return "", fmt.Errorf("failed to get config: %w", err)
	}

	path := paths.KernelPath(cfg.Paths)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("kernel not found at %s (use QEMUBOX_SHARE_DIR to override)", path)
}

// findInitrd returns the path to the initrd for QEMU
func findInitrd() (string, error) {
	cfg, err := config.Get()
	if err != nil {
		return "", fmt.Errorf("failed to get config: %w", err)
	}

	path := paths.InitrdPath(cfg.Paths)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("initrd not found at %s (use QEMUBOX_SHARE_DIR to override)", path)
}

// NewInstance creates a new QEMU VM instance.
func NewInstance(ctx context.Context, containerID, stateDir string, cfg *vm.VMResourceConfig) (vm.Instance, error) {
	binaryPath, err := findQemu()
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(stateDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create state directory: %w", err)
	}

	return newInstance(ctx, containerID, binaryPath, stateDir, cfg)
}

// newInstance creates a new QEMU microvm instance
func newInstance(ctx context.Context, containerID, binaryPath, stateDir string, resourceCfg *vm.VMResourceConfig) (*Instance, error) {
	kernelPath, err := findKernel()
	if err != nil {
		return nil, err
	}

	initrdPath, err := findInitrd()
	if err != nil {
		return nil, err
	}

	// Provide default resource configuration if none specified
	if resourceCfg == nil {
		resourceCfg = &vm.VMResourceConfig{
			BootCPUs:          defaultBootCPUs,
			MaxCPUs:           defaultMaxCPUs,
			MemorySize:        defaultMemorySize,
			MemoryHotplugSize: defaultMemoryMax,
			MemorySlots:       defaultMemorySlots,
		}
	}

	// Validate resource configuration
	if resourceCfg.BootCPUs < 1 {
		resourceCfg.BootCPUs = defaultBootCPUs
	}
	if resourceCfg.MaxCPUs < resourceCfg.BootCPUs {
		resourceCfg.MaxCPUs = resourceCfg.BootCPUs
	}
	if resourceCfg.MemorySize < 1 {
		resourceCfg.MemorySize = defaultMemorySize
	}
	if resourceCfg.MemoryHotplugSize < resourceCfg.MemorySize {
		resourceCfg.MemoryHotplugSize = resourceCfg.MemorySize
	}
	if resourceCfg.MemorySlots < 1 {
		resourceCfg.MemorySlots = defaultMemorySlots
	}

	// Use dedicated log directory per container from config
	cfg, err := config.Get()
	if err != nil {
		return nil, fmt.Errorf("failed to get config for log directory: %w", err)
	}
	logDir := filepath.Join(cfg.Paths.LogDir, containerID)
	if err := os.MkdirAll(logDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	qmpSocketPath := filepath.Join(stateDir, "qmp.sock")
	vsockPath := filepath.Join(stateDir, "vsock.sock")

	// Unix domain sockets have a 108-character path limit on Linux (including null terminator)
	// Validate socket paths to prevent runtime errors
	if len(qmpSocketPath) > maxUnixSocketPath {
		return nil, fmt.Errorf("QMP socket path too long (%d > %d): %s", len(qmpSocketPath), maxUnixSocketPath, qmpSocketPath)
	}
	if len(vsockPath) > maxUnixSocketPath {
		return nil, fmt.Errorf("vsock socket path too long (%d > %d): %s", len(vsockPath), maxUnixSocketPath, vsockPath)
	}

	// Allocate unique vsock CID for this VM
	guestCID, cidLockFile, err := allocateGuestCID()
	if err != nil {
		return nil, fmt.Errorf("failed to allocate vsock CID: %w", err)
	}

	inst := &Instance{
		binaryPath:      binaryPath,
		stateDir:        stateDir,
		logDir:          logDir,
		kernelPath:      kernelPath,
		initrdPath:      initrdPath,
		qmpSocketPath:   qmpSocketPath,
		vsockPath:       vsockPath,
		consolePath:     filepath.Join(logDir, "console.log"),
		consoleFifoPath: filepath.Join(stateDir, "console.fifo"),
		qemuLogPath:     filepath.Join(logDir, "qemu.log"),
		disks:           []*DiskConfig{},
		nets:            []*NetConfig{},
		resourceCfg:     resourceCfg,
		guestCID:        guestCID,
		cidLockFile:     cidLockFile,
	}

	log.G(ctx).WithFields(log.Fields{
		"containerID":   containerID,
		"guestCID":      guestCID,
		"bootCPUs":      resourceCfg.BootCPUs,
		"maxCPUs":       resourceCfg.MaxCPUs,
		"memorySize":    resourceCfg.MemorySize,
		"memoryHotplug": resourceCfg.MemoryHotplugSize,
	}).Debug("qemu: instance configured")

	return inst, nil
}

// AddFS adds a filesystem to the VM.
// Note: QEMU microvm can use virtio-fs, but we use block devices for simplicity.
