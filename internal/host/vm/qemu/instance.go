//go:build linux

package qemu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/containerd/log"

	"github.com/spin-stack/spinbox/internal/config"
	"github.com/spin-stack/spinbox/internal/host/vm"
	"github.com/spin-stack/spinbox/internal/paths"
	vsockalloc "github.com/spin-stack/spinbox/internal/vsock"
)

const (
	// defaultMemorySlots is the number of memory hotplug slots.
	// 8 slots * 128MB min increment = 1GB max hotplug capacity
	// Trade-off: More slots = more QEMU overhead, fewer = less flexibility
	defaultMemorySlots = 8

	// minGuestCID is the minimum valid vsock guest CID.
	// CIDs 0-2 are reserved (hypervisor, reserved, host).
	// CID 3 is avoided due to observed transient routing issues in some environments.
	minGuestCID uint32 = 4

	// maxGuestCID is the maximum CID we'll scan to before wrapping.
	// Using a reasonable limit to avoid scanning billions of files.
	maxGuestCID uint32 = 65535

	// cidLockDir is the subdirectory for CID lock files.
	cidLockDir = "cid"

	// cidCooldownPeriod is the minimum time to wait before reusing a CID.
	// This allows the kernel's vsock routing table to clean up stale entries
	// after a VM exits. Without this cooldown, a new VM using the same CID
	// might experience vsock connection issues due to stale kernel state.
	cidCooldownPeriod = 2 * time.Second
)

// instancePaths groups all filesystem paths used by an Instance.
// This is a helper struct used during instance construction.
type instancePaths struct {
	stateDir        string // Ephemeral state (sockets, FIFOs) - deleted on shutdown
	logDir          string // Persistent logs (console, QEMU stderr)
	qmpSocketPath   string // QMP control socket
	vsockPath       string // Vsock socket for guest RPC
	consolePath     string // Persistent console log file
	consoleFifoPath string // Ephemeral FIFO for console streaming
	qemuLogPath     string // QEMU stderr log
}

// newInstancePaths creates paths for a new instance.
// Returns an error if any socket path exceeds the Unix socket path limit.
func newInstancePaths(stateDir, logDir string) (*instancePaths, error) {
	p := &instancePaths{
		stateDir:        stateDir,
		logDir:          logDir,
		qmpSocketPath:   filepath.Join(stateDir, "qmp.sock"),
		vsockPath:       filepath.Join(stateDir, "vsock.sock"),
		consolePath:     filepath.Join(logDir, "console.log"),
		consoleFifoPath: filepath.Join(stateDir, "console.fifo"),
		qemuLogPath:     filepath.Join(logDir, "qemu.log"),
	}

	// Unix domain sockets have a 108-character path limit on Linux
	if len(p.qmpSocketPath) > maxUnixSocketPath {
		return nil, fmt.Errorf("QMP socket path too long (%d > %d): %s",
			len(p.qmpSocketPath), maxUnixSocketPath, p.qmpSocketPath)
	}
	if len(p.vsockPath) > maxUnixSocketPath {
		return nil, fmt.Errorf("vsock socket path too long (%d > %d): %s",
			len(p.vsockPath), maxUnixSocketPath, p.vsockPath)
	}

	return p, nil
}

// validateResourceConfig validates and applies defaults to resource configuration.
func validateResourceConfig(cfg *vm.VMResourceConfig) *vm.VMResourceConfig {
	if cfg == nil {
		return &vm.VMResourceConfig{
			BootCPUs:          defaultBootCPUs,
			MaxCPUs:           defaultMaxCPUs,
			MemorySize:        defaultMemorySize,
			MemoryHotplugSize: defaultMemoryMax,
			MemorySlots:       defaultMemorySlots,
		}
	}

	// Apply defaults for invalid values
	result := *cfg
	if result.BootCPUs < 1 {
		result.BootCPUs = defaultBootCPUs
	}
	if result.MaxCPUs < result.BootCPUs {
		result.MaxCPUs = result.BootCPUs
	}
	if result.MemorySize < 1 {
		result.MemorySize = defaultMemorySize
	}
	if result.MemoryHotplugSize < result.MemorySize {
		result.MemoryHotplugSize = result.MemorySize
	}
	if result.MemorySlots < 1 {
		result.MemorySlots = defaultMemorySlots
	}

	return &result
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
	return "", fmt.Errorf("kernel not found at %s (use SPINBOX_SHARE_DIR to override)", path)
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
	return "", fmt.Errorf("initrd not found at %s (use SPINBOX_SHARE_DIR to override)", path)
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

	// Validate and apply defaults to resource configuration
	resourceCfg = validateResourceConfig(resourceCfg)

	// Get config for log directory
	cfg, err := config.Get()
	if err != nil {
		return nil, fmt.Errorf("failed to get config for log directory: %w", err)
	}

	// Create log directory
	logDir := filepath.Join(cfg.Paths.LogDir, containerID)
	if err := os.MkdirAll(logDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	// Create and validate paths
	p, err := newInstancePaths(stateDir, logDir)
	if err != nil {
		return nil, err
	}

	// Allocate unique vsock CID for this VM
	lockDir := filepath.Join(cfg.Paths.StateDir, cidLockDir)
	allocator := vsockalloc.NewAllocator(lockDir, minGuestCID, maxGuestCID, cidCooldownPeriod)
	lease, err := allocator.Allocate()
	if err != nil {
		return nil, fmt.Errorf("failed to allocate vsock CID: %w", err)
	}

	inst := &Instance{
		binaryPath:      binaryPath,
		stateDir:        p.stateDir,
		logDir:          p.logDir,
		kernelPath:      kernelPath,
		initrdPath:      initrdPath,
		qmpSocketPath:   p.qmpSocketPath,
		vsockPath:       p.vsockPath,
		consolePath:     p.consolePath,
		consoleFifoPath: p.consoleFifoPath,
		qemuLogPath:     p.qemuLogPath,
		disks:           []*DiskConfig{},
		nets:            []*NetConfig{},
		resourceCfg:     resourceCfg,
		guestCID:        lease.CID,
		cidLease:        lease,
	}

	log.G(ctx).WithFields(log.Fields{
		"containerID":   containerID,
		"guestCID":      lease.CID,
		"bootCPUs":      resourceCfg.BootCPUs,
		"maxCPUs":       resourceCfg.MaxCPUs,
		"memorySize":    resourceCfg.MemorySize,
		"memoryHotplug": resourceCfg.MemoryHotplugSize,
	}).Debug("qemu: instance configured")

	return inst, nil
}
