//go:build linux

package qemu

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	"github.com/mdlayher/vsock"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/aledbf/qemubox/containerd/internal/host/vm"
	"github.com/aledbf/qemubox/containerd/internal/paths"
)

// findQemu returns the path to the qemu-system-x86_64 binary
func findQemu() (string, error) {
	path := paths.QemuPath()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("qemu-system-x86_64 binary not found at %s", path)
}

// findKernel returns the path to the kernel binary for QEMU
func findKernel() (string, error) {
	path := paths.KernelPath()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("kernel not found at %s (use QEMUBOX_SHARE_DIR to override)", path)
}

// findInitrd returns the path to the initrd for QEMU
func findInitrd() (string, error) {
	path := paths.InitrdPath()
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

	// Use dedicated log directory per container
	logDir := filepath.Join("/var/log/qemubox", containerID)
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
	}

	log.G(ctx).WithFields(log.Fields{
		"containerID":   containerID,
		"bootCPUs":      resourceCfg.BootCPUs,
		"maxCPUs":       resourceCfg.MaxCPUs,
		"memorySize":    resourceCfg.MemorySize,
		"memoryHotplug": resourceCfg.MemoryHotplugSize,
	}).Debug("qemu: instance configured")

	return inst, nil
}

// AddFS adds a filesystem to the VM.
// Note: QEMU microvm can use virtio-fs, but we use block devices for simplicity.
func (q *Instance) AddFS(ctx context.Context, tag, mountPath string, opts ...vm.MountOpt) error {
	log.G(ctx).WithFields(log.Fields{
		"tag":  tag,
		"path": mountPath,
	}).Warn("qemu: AddFS not supported, use disk-based approach instead")

	return fmt.Errorf("AddFS not implemented for QEMU: use EROFS or block devices")
}

// generateStableDiskID generates a stable device ID based on file metadata.
// This ensures consistent device naming across VM reboots and reduces issues
// with device enumeration order. Uses inode and device number as stable identifiers.
func generateStableDiskID(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("failed to stat disk path: %w", err)
	}

	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		// Fallback: use base64-encoded path (stable, no hash collisions)
		// Using RawURLEncoding makes it filesystem-safe (no padding '=' characters)
		encoded := base64.RawURLEncoding.EncodeToString([]byte(path))
		return "disk-" + encoded, nil
	}

	// Generate stable ID from device and inode numbers
	// Format: disk-<dev>-<inode>
	return fmt.Sprintf("disk-%x-%x", stat.Dev, stat.Ino), nil
}

// AddDisk schedules a disk to be attached to the VM
func (q *Instance) AddDisk(ctx context.Context, blockID, mountPath string, opts ...vm.MountOpt) error {
	if q.getState() != vmStateNew {
		return errors.New("cannot add disk after VM started")
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	var mc vm.MountConfig
	for _, o := range opts {
		o(&mc)
	}

	// Generate stable device ID if not provided
	if blockID == "" {
		stableID, err := generateStableDiskID(mountPath)
		if err != nil {
			return fmt.Errorf("failed to generate stable disk ID: %w", err)
		}
		blockID = stableID
	}

	q.disks = append(q.disks, &DiskConfig{
		Path:     mountPath,
		Readonly: mc.Readonly,
		ID:       blockID,
	})

	log.G(ctx).WithFields(log.Fields{
		"blockID":  blockID,
		"path":     mountPath,
		"readonly": mc.Readonly,
	}).Debug("qemu: scheduled disk")

	return nil
}

// AddNIC adds a network interface (not supported for QEMU microvm, use TAP)
func (q *Instance) AddNIC(ctx context.Context, endpoint string, mac net.HardwareAddr, mode vm.NetworkMode, features, flags uint32) error {
	return fmt.Errorf("UNIX socket networking not supported by QEMU microvm; use AddTAPNIC instead: %w", errdefs.ErrNotImplemented)
}

// AddTAPNIC schedules a TAP network interface to be attached to the VM
func (q *Instance) AddTAPNIC(ctx context.Context, tapName string, mac net.HardwareAddr) error {
	if q.getState() != vmStateNew {
		return errors.New("cannot add NIC after VM started")
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	macStr := mac.String()
	q.nets = append(q.nets, &NetConfig{
		TapName: tapName,
		MAC:     macStr,
		ID:      fmt.Sprintf("net%d", len(q.nets)),
	})

	log.G(ctx).WithFields(log.Fields{
		"tap": tapName,
		"mac": macStr,
	}).Debug("qemu: scheduled TAP NIC")

	return nil
}

// VMInfo returns metadata about the QEMU backend
func (q *Instance) VMInfo() vm.VMInfo {
	return vm.VMInfo{
		Type:          "qemu",
		SupportsTAP:   true,
		SupportsVSOCK: true,
	}
}

// setupConsoleFIFO creates a producer-consumer pipeline for VM console output:
//
// Data flow: VM serial console → QEMU → FIFO pipe → goroutine → persistent log file
//
// Why two files instead of QEMU writing directly to console.log?
//  1. Non-blocking writes: QEMU writes to FIFO never block (kernel buffering)
//  2. Async I/O: Slow disk writes don't stall VM console output
//  3. Separation: FIFO (ephemeral, deleted on cleanup) vs log (persistent for debugging)
//
// Files created:
//   - consoleFifoPath (stateDir/console.fifo): Named pipe, QEMU writes here via -serial file:
//   - consolePath (logDir/console.log): Regular file, goroutine streams FIFO data here
func (q *Instance) setupConsoleFIFO(ctx context.Context) error {
	// Remove old FIFO if it exists (ignore errors)
	_ = os.Remove(q.consoleFifoPath)

	// Create FIFO pipe for QEMU to write to
	if err := syscall.Mkfifo(q.consoleFifoPath, 0600); err != nil {
		return fmt.Errorf("failed to create console FIFO: %w", err)
	}

	// Create persistent console log file
	consoleFile, err := os.Create(q.consolePath)
	if err != nil {
		_ = os.Remove(q.consoleFifoPath)
		return fmt.Errorf("failed to create console log file: %w", err)
	}
	q.consoleFile = consoleFile

	// Start background goroutine to stream FIFO → log file
	// This prevents QEMU from blocking on slow disk I/O
	//
	// Goroutine lifecycle: This goroutine exits when QEMU closes the FIFO writer (on VM shutdown).
	// FIFOs don't support read deadlines, so we can't use context for early cancellation.
	// In practice, the goroutine lifetime matches the VM lifetime, which is acceptable.
	go func() {
		defer func() {
			_ = consoleFile.Close()
		}()

		// Consumer side: Open FIFO for reading
		// This blocks until QEMU opens the other end for writing (producer side)
		fifo, err := os.OpenFile(q.consoleFifoPath, os.O_RDONLY, 0)
		if err != nil {
			log.G(ctx).WithError(err).Error("qemu: failed to open console FIFO for reading")
			return
		}
		defer func() {
			_ = fifo.Close()
		}()

		// Continuously stream: FIFO (fast, kernel-buffered) → log file (persistent, may be slow)
		// This decouples QEMU's write speed from disk I/O performance
		buf := make([]byte, consoleBufferSize)
		for {
			n, err := fifo.Read(buf)
			if n > 0 {
				if _, writeErr := consoleFile.Write(buf[:n]); writeErr != nil {
					log.G(ctx).WithError(writeErr).Error("qemu: failed to write console output")
				}
			}
			if err != nil {
				if err != io.EOF {
					log.G(ctx).WithError(err).Debug("qemu: console FIFO read error")
				}
				break
			}
		}
	}()

	return nil
}

// validateConfiguration validates the VM configuration before starting
func (q *Instance) validateConfiguration() error {
	// Validate kernel exists
	if _, err := os.Stat(q.kernelPath); err != nil {
		return fmt.Errorf("kernel not found at %s: %w", q.kernelPath, err)
	}

	// Validate initrd exists
	if _, err := os.Stat(q.initrdPath); err != nil {
		return fmt.Errorf("initrd not found at %s: %w", q.initrdPath, err)
	}

	// Validate QEMU binary exists
	if _, err := os.Stat(q.binaryPath); err != nil {
		return fmt.Errorf("QEMU binary not found at %s: %w", q.binaryPath, err)
	}

	// Validate all disk paths exist
	for _, disk := range q.disks {
		if _, err := os.Stat(disk.Path); err != nil {
			return fmt.Errorf("disk not found at %s: %w", disk.Path, err)
		}
	}

	// Validate resource limits are sane
	const minMemory = 128 * 1024 * 1024 // 128 MiB
	if q.resourceCfg.MemorySize < minMemory {
		return fmt.Errorf("memory too low: %d bytes (minimum %d bytes / 128 MiB)", q.resourceCfg.MemorySize, minMemory)
	}

	if q.resourceCfg.BootCPUs < 1 {
		return fmt.Errorf("boot CPUs must be at least 1, got %d", q.resourceCfg.BootCPUs)
	}

	if q.resourceCfg.MaxCPUs < q.resourceCfg.BootCPUs {
		return fmt.Errorf("max CPUs (%d) cannot be less than boot CPUs (%d)", q.resourceCfg.MaxCPUs, q.resourceCfg.BootCPUs)
	}

	return nil
}

func (q *Instance) openTapFiles(ctx context.Context, netns string) error {
	if len(q.nets) == 0 {
		return nil
	}
	if netns == "" {
		return fmt.Errorf("network namespace is required when NICs are configured")
	}
	for _, nic := range q.nets {
		tapFile, err := openTAPInNetNS(ctx, nic.TapName, netns)
		if err != nil {
			// Clean up any already-opened FDs on failure
			q.closeTAPFiles()
			return fmt.Errorf("failed to open tap %s in netns: %w", nic.TapName, err)
		}
		// Store the file descriptor
		nic.TapFile = tapFile
	}
	q.tapNetns = netns
	return nil
}

// closeTAPFiles closes all TAP file descriptors and resets the netns tracking.
// This centralizes TAP FD cleanup logic used in multiple error paths.
func (q *Instance) closeTAPFiles() {
	for _, nic := range q.nets {
		if nic.TapFile != nil {
			_ = nic.TapFile.Close()
			nic.TapFile = nil
		}
	}
	q.tapNetns = ""
}

func (q *Instance) startQemuProcess(ctx context.Context, qemuArgs []string) error {
	// Create QEMU log file for stdout/stderr
	qemuLogFile, err := os.Create(q.qemuLogPath)
	if err != nil {
		return fmt.Errorf("failed to create qemu log file: %w", err)
	}

	// Start QEMU
	//nolint:gosec // QEMU path and args are controlled by VM configuration.
	q.cmd = exec.CommandContext(ctx, q.binaryPath, qemuArgs...)
	q.cmd.Stdout = qemuLogFile
	q.cmd.Stderr = qemuLogFile
	q.cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	q.waitCh = make(chan error, 1)

	// Pass TAP file descriptors to QEMU via ExtraFiles
	// These will be available to QEMU as FD 3, 4, 5, ... (0,1,2 are stdin/stdout/stderr)
	var extraFiles []*os.File
	for _, nic := range q.nets {
		if nic.TapFile != nil {
			extraFiles = append(extraFiles, nic.TapFile)
		}
	}
	if len(extraFiles) > 0 {
		q.cmd.ExtraFiles = extraFiles
		log.G(ctx).WithField("fd_count", len(extraFiles)).Debug("passing TAP file descriptors to QEMU")
	}

	if err := q.cmd.Start(); err != nil {
		// Clean up TAP FDs on start failure
		for _, f := range extraFiles {
			_ = f.Close()
		}
		return fmt.Errorf("failed to start qemu: %w", err)
	}

	log.G(ctx).Info("qemu: process started, waiting for QMP socket...")

	q.monitorProcess(ctx)
	return nil
}

func (q *Instance) monitorProcess(ctx context.Context) {
	// Monitor QEMU process in background
	// Process monitor: detects when QEMU exits (poweroff, reboot, crash)
	// This goroutine only signals exit - cleanup is handled by Shutdown()
	go func() {
		exitErr := q.cmd.Wait()

		logger := log.G(ctx)
		if exitErr != nil {
			logger.WithError(exitErr).Debug("qemu: process exited")
		}

		// Signal Shutdown() that process exited
		select {
		case q.waitCh <- exitErr:
		default:
			// Channel may be closed if Shutdown() already completed
		}

		// Cancel background monitors if still running
		if q.runCancel != nil {
			q.runCancel()
		}

		// Don't close clients/TAP here - Shutdown() owns cleanup
		// This goroutine just detects process exit
	}()
}

func (q *Instance) connectQMP(ctx context.Context) error {
	qmpClient, err := newQMPClient(ctx, q.qmpSocketPath)
	if err != nil {
		// Check if QEMU process is still running
		if q.cmd.Process != nil {
			_ = q.cmd.Process.Kill()
		}
		return fmt.Errorf("failed to connect to QMP: %w", err)
	}
	q.qmpClient = qmpClient
	return nil
}

func (q *Instance) connectVsockClient(ctx context.Context) error {
	select {
	case <-ctx.Done():
		log.G(ctx).WithError(ctx.Err()).Error("qemu: context cancelled before connectVsockRPC")
		if q.cmd != nil && q.cmd.Process != nil {
			_ = q.cmd.Process.Kill()
		}
		if q.qmpClient != nil {
			_ = q.qmpClient.Close()
		}
		return ctx.Err()
	default:
	}
	conn, err := q.connectVsockRPC(ctx)
	if err != nil {
		if q.cmd != nil && q.cmd.Process != nil {
			_ = q.cmd.Process.Kill()
		}
		if q.qmpClient != nil {
			_ = q.qmpClient.Close()
		}
		return err
	}

	q.vsockConn = conn
	q.client = ttrpc.NewClient(conn)
	return nil
}

func (q *Instance) rollbackStart(success *bool) {
	if success != nil && *success {
		return
	}
	q.setState(vmStateNew)

	// Close vsock connection FIRST (before killing QEMU)
	if q.vsockConn != nil {
		_ = q.vsockConn.Close()
		q.vsockConn = nil
	}

	// Close TTRPC client
	if q.client != nil {
		_ = q.client.Close()
		q.client = nil
	}

	// Close QMP client
	if q.qmpClient != nil {
		_ = q.qmpClient.Close()
		q.qmpClient = nil
	}

	// Close console file and remove FIFO on failure
	if q.consoleFile != nil {
		_ = q.consoleFile.Close()
		q.consoleFile = nil
	}
	if q.consoleFifoPath != "" {
		_ = os.Remove(q.consoleFifoPath)
	}

	// Close any opened TAP FDs on failure
	q.closeTAPFiles()
}

// Start starts the QEMU VM
func (q *Instance) Start(ctx context.Context, opts ...vm.StartOpt) error {
	// Check and update state atomically
	if !q.compareAndSwapState(vmStateNew, vmStateStarting) {
		currentState := q.getState()
		return fmt.Errorf("cannot start VM in state %d", currentState)
	}

	// Validate configuration before starting
	if err := q.validateConfiguration(); err != nil {
		q.setState(vmStateNew)
		return fmt.Errorf("configuration validation failed: %w", err)
	}

	// Setup console FIFO for real-time streaming
	if err := q.setupConsoleFIFO(ctx); err != nil {
		q.setState(vmStateNew)
		return fmt.Errorf("failed to setup console FIFO: %w", err)
	}

	// Ensure we revert to New on failure
	success := false
	defer q.rollbackStart(&success)

	q.mu.Lock()
	defer q.mu.Unlock()

	// Remove old socket files if they exist
	if err := os.Remove(q.qmpSocketPath); err != nil && !os.IsNotExist(err) {
		log.G(ctx).WithError(err).Debug("qemu: failed to remove QMP socket")
	}
	if err := os.Remove(q.vsockPath); err != nil && !os.IsNotExist(err) {
		log.G(ctx).WithError(err).Debug("qemu: failed to remove vsock path")
	}

	// Parse start options
	startOpts := vm.StartOpts{}
	for _, o := range opts {
		o(&startOpts)
	}

	// Store network configuration
	q.networkCfg = startOpts.NetworkConfig

	// Open TAP file descriptors in the network namespace.
	// QEMU (running in init netns for vhost-vsock) will use these FDs to attach to
	// TAP devices that stay in their sandbox namespaces. This is the Kata Containers approach:
	// FDs are namespace-agnostic, so no need to move TAPs between namespaces.
	if err := q.openTapFiles(ctx, startOpts.NetworkNamespace); err != nil {
		return err
	}

	// Build kernel command line
	cmdlineArgs := q.buildKernelCommandLine(startOpts)

	// Build QEMU command line (now uses the renamed TAP names)
	qemuArgs, err := q.buildQemuCommandLine(cmdlineArgs)
	if err != nil {
		return err
	}

	// Print full command for manual testing
	log.G(ctx).WithFields(log.Fields{
		"binary":  q.binaryPath,
		"cmdline": strings.Join(qemuArgs, " "),
	}).Debug("qemu: starting vm")

	if err := q.startQemuProcess(ctx, qemuArgs); err != nil {
		return err
	}

	// Connect to QMP for control
	if err := q.connectQMP(ctx); err != nil {
		return err
	}

	log.G(ctx).Info("qemu: QMP connected, waiting for vsock...")

	// Create long-lived context for background monitors; Start ctx may be cancelled by callers.
	// We use context.Background() here because the background monitors need to outlive
	// the Start() call and continue running until explicit Shutdown().
	runCtx, runCancel := context.WithCancel(context.WithoutCancel(ctx))
	// Note: q.mu is already held (locked at line 200), so we can set these fields directly
	q.runCtx = runCtx
	q.runCancel = runCancel

	// Connect to vsock RPC server
	if err := q.connectVsockClient(ctx); err != nil {
		return err
	}

	// Monitor liveness of the guest RPC server; if it goes away (guest reboot/poweroff)
	// ensure QEMU exits so the shim can clean up.
	go q.monitorGuestRPC(runCtx)

	// Mark as successfully started
	success = true
	q.setState(vmStateRunning)

	log.G(ctx).Info("qemu: VM fully initialized")

	return nil
}

// buildKernelCommandLine constructs the kernel command line
func (q *Instance) buildKernelCommandLine(startOpts vm.StartOpts) string {
	// Prepare init arguments for vminitd
	initArgs := []string{
		fmt.Sprintf("-vsock-rpc-port=%d", vsockRPCPort),
		fmt.Sprintf("-vsock-stream-port=%d", vsockStreamPort),
		fmt.Sprintf("-vsock-cid=%d", vsockCID),
	}
	initArgs = append(initArgs, startOpts.InitArgs...)

	// Build network configuration
	var netConfigs []string
	if startOpts.NetworkConfig != nil && startOpts.NetworkConfig.IP != "" {
		cfg := startOpts.NetworkConfig
		// IPv4 configuration using kernel ip= parameter format:
		// ip=<client-ip>:<server-ip>:<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>:<dns0-ip>:<dns1-ip>
		var ipParamBuilder strings.Builder
		fmt.Fprintf(&ipParamBuilder, "ip=%s::%s:%s::eth0:none",
			cfg.IP,
			cfg.Gateway,
			cfg.Netmask)

		// Append DNS servers to ip= parameter (kernel supports up to 2 DNS servers)
		for i, dns := range cfg.DNS {
			if i < 2 {
				ipParamBuilder.WriteString(":")
				ipParamBuilder.WriteString(dns)
			}
		}

		netConfigs = append(netConfigs, ipParamBuilder.String())
	}

	// Build kernel command line
	cmdlineParts := []string{
		"console=ttyS0",
		"quiet",                          // Reduce boot messages for faster boot
		"loglevel=3",                     // Minimal kernel logging (errors only)
		"panic=1",                        // Reboot 1 second after kernel panic
		"net.ifnames=0", "biosdevname=0", // Predictable network naming
		"systemd.unified_cgroup_hierarchy=1", // Force cgroup v2
		"cgroup_no_v1=all",                   // Disable cgroup v1
		"nohz=off",                           // Disable tickless kernel (reduces overhead for short-lived VMs)
	}

	if len(netConfigs) > 0 {
		cmdlineParts = append(cmdlineParts, netConfigs...)
	}

	cmdlineParts = append(cmdlineParts, fmt.Sprintf("init=/sbin/vminitd -- %s", formatInitArgs(initArgs)))

	return strings.Join(cmdlineParts, " ")
}

// buildQemuCommandLine constructs the QEMU command line arguments
func (q *Instance) buildQemuCommandLine(cmdlineArgs string) ([]string, error) {
	// Convert memory from bytes to MB
	memoryMB := q.resourceCfg.MemorySize / (1024 * 1024)
	memoryMaxMB := q.resourceCfg.MemoryHotplugSize / (1024 * 1024)

	// Calculate memory hotplug slots needed (0-16 based on usage)
	memorySlots := 8 // Reduced from 16 - adequate for most workloads
	if q.resourceCfg.MemoryHotplugSize <= q.resourceCfg.MemorySize {
		memorySlots = 0 // No hotplug needed if max equals initial
	}

	args := []string{
		// BIOS/firmware path
		"-L", paths.QemuSharePath(),

		"-machine", "q35,accel=kvm,kernel-irqchip=on,hpet=off,acpi=on", // Optimize: use kernel IRQ chip, disable HPET
		"-cpu", "host,migratable=on",

		// CPU configuration for hotplug:
		// Simple topology: just specify initial CPUs and max CPUs, let QEMU handle the rest
		// This creates a single socket with enough capacity for maxcpus
		"-smp", fmt.Sprintf("%d,maxcpus=%d", q.resourceCfg.BootCPUs, q.resourceCfg.MaxCPUs),
	}

	// Memory configuration - optimize slots based on hotplug needs
	if memorySlots > 0 {
		args = append(args, "-m", fmt.Sprintf("%d,slots=%d,maxmem=%dM", memoryMB, memorySlots, memoryMaxMB))
	} else {
		args = append(args, "-m", fmt.Sprintf("%d", memoryMB))
	}

	args = append(args,
		"-kernel", q.kernelPath,
		"-initrd", q.initrdPath,
		"-append", cmdlineArgs,
		"-nographic",
		// Serial console → FIFO pipe (producer side)
		// QEMU writes VM console output here; background goroutine reads and streams to log file
		// See setupConsoleFIFO() for the producer-consumer pipeline details
		"-serial", fmt.Sprintf("file:%s", q.consoleFifoPath),

		// Vsock for guest communication (using vhost-vsock kernel module)
		"-device", fmt.Sprintf("vhost-vsock-pci,guest-cid=%d", vsockCID),

		// QMP for VM control
		"-qmp", fmt.Sprintf("unix:%s,server=on,wait=off", q.qmpSocketPath),

		// RNG device for entropy
		"-device", "virtio-rng-pci",
	)

	// Add disks
	for i, disk := range q.disks {
		// Detect format based on file extension
		format := "raw"
		if strings.HasSuffix(disk.Path, ".vmdk") {
			format = "vmdk"
		} else if strings.HasSuffix(disk.Path, ".qcow2") {
			format = "qcow2"
		}

		driveArgs := fmt.Sprintf("file=%s,if=none,id=blk%d,format=%s", disk.Path, i, format)
		if disk.Readonly {
			driveArgs += ",readonly=on"
		}
		args = append(args, "-drive", driveArgs)
		args = append(args, "-device", fmt.Sprintf("virtio-blk-pci,drive=blk%d", i))
	}

	// Add NICs
	for i, nic := range q.nets {
		// Use Kata Containers approach: pass TAP via file descriptor
		// FD will be passed via ExtraFiles, which start at FD 3
		// (FDs 0,1,2 are stdin/stdout/stderr)
		if nic.TapFile == nil {
			// This should never happen - TAP FD must be opened before Start()
			return nil, fmt.Errorf("internal error: NIC %s has no TAP file descriptor (openTapFiles not called?)", nic.TapName)
		}
		fd := 3 + i
		// Note: script= and downscript= are invalid with fd=
		// When using fd=, QEMU expects the TAP to be already configured
		args = append(args,
			"-netdev", fmt.Sprintf("tap,id=net%d,fd=%d", i, fd),
			// Disable option ROM loading (e.g., efi-virtio.rom) to avoid firmware dependency.
			"-device", fmt.Sprintf("virtio-net-pci,netdev=net%d,mac=%s,romfile=", i, nic.MAC),
		)
	}

	return args, nil
}

// Client returns the long-lived TTRPC client for communicating with the guest.
// This is used for the event stream and should not be shared for concurrent RPCs.
func (q *Instance) Client() *ttrpc.Client {
	// Return nil if VM is shutdown
	if q.getState() != vmStateRunning {
		return nil
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	return q.client
}

// DialClient creates a short-lived TTRPC client for one-off RPCs.
// The caller must close the returned client.
func (q *Instance) DialClient(ctx context.Context) (*ttrpc.Client, error) {
	if q.getState() != vmStateRunning {
		return nil, fmt.Errorf("vm not running: %w", errdefs.ErrFailedPrecondition)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	conn, err := vsock.Dial(vsockCID, vsockRPCPort, nil)
	if err != nil {
		return nil, err
	}
	log.G(ctx).Debug("qemu: vsock dialed for TTRPC")

	if err := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := pingTTRPC(conn); err != nil {
		log.G(ctx).WithError(err).Debug("qemu: TTRPC ping failed")
		_ = conn.Close()
		return nil, err
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}

	log.G(ctx).Debug("qemu: TTRPC ping ok, client ready")
	return ttrpc.NewClient(conn), nil
}

// QMPClient returns the QMP client for controlling the VM
func (q *Instance) QMPClient() *qmpClient {
	// Return nil if VM is shutdown
	if q.getState() == vmStateShutdown {
		return nil
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	return q.qmpClient
}

// CPUHotplugger returns an interface for CPU hotplug operations
func (q *Instance) CPUHotplugger() vm.CPUHotplugger {
	// Return nil if VM is shutdown
	if q.getState() == vmStateShutdown {
		return nil
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	return q.qmpClient
}

func (q *Instance) shutdownGuest(ctx context.Context, logger *log.Entry) {
	// Send graceful shutdown to guest OS
	// Try CTRL+ALT+DELETE first (more reliable for some distributions), then ACPI powerdown
	// We use a fresh context here because the caller's context might be cancelled/expired,
	// but we still need time to properly shut down the VM.
	if q.qmpClient != nil {
		logger.Info("qemu: sending CTRL+ALT+DELETE via QMP")
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		if err := q.qmpClient.SendCtrlAltDelete(shutdownCtx); err != nil {
			logger.WithError(err).Debug("qemu: failed to send CTRL+ALT+DELETE, trying ACPI powerdown")
			// Fall back to ACPI powerdown
			if err := q.qmpClient.Shutdown(shutdownCtx); err != nil {
				logger.WithError(err).Warning("qemu: failed to send ACPI powerdown")
			}
		}
		cancel()
	}
}

func (q *Instance) cleanupAfterFailedKill() {
	// Clean up QMP and TAPs before returning error
	if q.qmpClient != nil {
		_ = q.qmpClient.Close()
		q.qmpClient = nil
	}
	q.closeTAPFiles()
}

func (q *Instance) stopQemuProcess(ctx context.Context, logger *log.Entry) error {
	// Brief wait to let guest start shutdown, then send quit
	// QEMU won't exit on its own - it always needs an explicit quit command
	if q.cmd == nil || q.cmd.Process == nil {
		return nil
	}

	// Wait up to 500ms for guest to receive ACPI signal
	select {
	case exitErr := <-q.waitCh:
		// Unexpected early exit - shouldn't happen but handle it
		logger.WithError(exitErr).Debug("qemu: process exited during ACPI wait")
		q.cmd = nil
		return nil
	case <-time.After(500 * time.Millisecond):
		// Expected - continue to quit command
	}

	// Send quit command to tell QEMU to exit
	if q.qmpClient != nil {
		logger.Debug("qemu: sending quit command to QEMU")
		quitCtx, quitCancel := context.WithTimeout(context.WithoutCancel(ctx), 1*time.Second)
		if err := q.qmpClient.Quit(quitCtx); err != nil {
			logger.WithError(err).Debug("qemu: failed to send quit command")
			quitCancel()
			// Fall through to SIGKILL
		} else {
			quitCancel()
			// Wait for quit to complete (should be fast - ~50ms)
			select {
			case exitErr := <-q.waitCh:
				if exitErr != nil && exitErr.Error() != "signal: killed" {
					logger.WithError(exitErr).Debug("qemu: process exited with error after quit")
				} else {
					logger.Info("qemu: process exited after quit command")
				}
				q.cmd = nil
				return nil
			case <-time.After(2 * time.Second):
				// Quit didn't work - fall through to SIGKILL
				logger.Warning("qemu: quit command timeout, sending SIGKILL")
			}
		}
	}

	// Still not dead - SIGKILL as last resort
	logger.Warning("qemu: sending SIGKILL to process")
	if err := q.cmd.Process.Kill(); err != nil {
		logger.WithError(err).Error("qemu: failed to send SIGKILL")
		q.cmd = nil
		q.cleanupAfterFailedKill()
		return fmt.Errorf("failed to kill QEMU process: %w", err)
	}
	logger.Info("qemu: sent SIGKILL to process")

	// Wait for SIGKILL to complete (with timeout)
	select {
	case exitErr := <-q.waitCh:
		if exitErr != nil {
			logger.WithError(exitErr).Debug("qemu: process exited after SIGKILL")
		}
	case <-time.After(2 * time.Second):
		logger.Error("qemu: process did not exit after SIGKILL")
		q.cmd = nil
		q.cleanupAfterFailedKill()
		return fmt.Errorf("process did not exit after SIGKILL")
	}
	q.cmd = nil
	return nil
}

func (q *Instance) cleanupResources(logger *log.Entry) {
	// Close QMP client
	if q.qmpClient != nil {
		if err := q.qmpClient.Close(); err != nil {
			logger.WithError(err).Debug("qemu: error closing QMP client")
		}
		q.qmpClient = nil
	}

	// Close console file (this will also stop the FIFO streaming goroutine)
	if q.consoleFile != nil {
		if err := q.consoleFile.Close(); err != nil {
			logger.WithError(err).Debug("qemu: error closing console file")
		}
		q.consoleFile = nil
	}

	// Remove FIFO pipe
	if q.consoleFifoPath != "" {
		if err := os.Remove(q.consoleFifoPath); err != nil && !os.IsNotExist(err) {
			logger.WithError(err).Debug("qemu: error removing console FIFO")
		}
	}

	// Close TAP file descriptors
	q.closeTAPFiles()
}

// Shutdown gracefully shuts down the VM
func (q *Instance) Shutdown(ctx context.Context) error {
	logger := log.G(ctx)
	logger.Info("qemu: Shutdown() called, initiating VM shutdown")

	// Mark VM as shutting down (atomic flag prevents re-entry)
	if !q.compareAndSwapState(vmStateRunning, vmStateShutdown) {
		currentState := q.getState()
		logger.WithField("state", currentState).Debug("qemu: VM not in running state, shutdown may already be in progress")
		// Not an error - idempotent shutdown
		return nil
	}

	// Cancel background monitors (VM status, guest RPC)
	if q.runCancel != nil {
		logger.Debug("qemu: cancelling background monitors")
		q.runCancel()
	}

	// Hold mutex for entire shutdown to prevent races
	q.mu.Lock()
	defer q.mu.Unlock()

	// Close TTRPC client to stop guest communication
	if q.client != nil {
		logger.Debug("qemu: closing TTRPC client")
		if err := q.client.Close(); err != nil {
			logger.WithError(err).Debug("qemu: error closing TTRPC client")
		}
		q.client = nil
	}

	// Close vsock listener
	if q.vsockConn != nil {
		logger.Debug("qemu: closing vsock connection")
		if err := q.vsockConn.Close(); err != nil {
			logger.WithError(err).Debug("qemu: error closing vsock connection")
		}
		q.vsockConn = nil
	}

	q.shutdownGuest(ctx, logger)

	if err := q.stopQemuProcess(ctx, logger); err != nil {
		return err
	}

	q.cleanupResources(logger)
	return nil
}

// StartStream creates a new stream connection to the VM for I/O operations.
func (q *Instance) StartStream(ctx context.Context) (uint32, net.Conn, error) {
	if q.getState() != vmStateRunning {
		return 0, nil, fmt.Errorf("vm not running: %w", errdefs.ErrFailedPrecondition)
	}
	const timeIncrement = 10 * time.Millisecond
	for d := timeIncrement; d < time.Second; d += timeIncrement {
		// Generate unique stream ID
		sid := atomic.AddUint32(&q.streamC, 1)
		if sid == 0 {
			return 0, nil, fmt.Errorf("exhausted stream identifiers: %w", errdefs.ErrUnavailable)
		}

		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		default:
		}

		// Connect directly via vsock stream port
		conn, err := vsock.Dial(vsockCID, vsockStreamPort, nil)
		if err == nil {
			// Send stream ID to vminitd (4 bytes, big-endian)
			var vs [4]byte
			binary.BigEndian.PutUint32(vs[:], sid)
			if _, err := conn.Write(vs[:]); err != nil {
				_ = conn.Close()
				return 0, nil, fmt.Errorf("failed to write stream id: %w", err)
			}

			// Wait for stream ID acknowledgment from vminitd
			var streamAck [4]byte
			if _, err := io.ReadFull(conn, streamAck[:]); err != nil {
				_ = conn.Close()
				return 0, nil, fmt.Errorf("failed to read stream ack: %w", err)
			}

			if binary.BigEndian.Uint32(streamAck[:]) != sid {
				_ = conn.Close()
				return 0, nil, fmt.Errorf("stream ack mismatch")
			}

			return sid, conn, nil
		}

		time.Sleep(timeIncrement)
	}

	return 0, nil, fmt.Errorf("timeout waiting for stream server: %w", errdefs.ErrUnavailable)
}

// connectVsockRPC establishes a connection to the vsock RPC server (vminitd)
func (q *Instance) connectVsockRPC(ctx context.Context) (net.Conn, error) {
	log.G(ctx).WithFields(log.Fields{
		"cid":  vsockCID,
		"port": vsockRPCPort,
	}).Info("qemu: connecting to vsock RPC port")

	// Wait a bit for vminitd to fully initialize
	time.Sleep(500 * time.Millisecond)

	retryStart := time.Now()
	pingDeadline := 50 * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if time.Since(retryStart) > connectRetryTimeout {
			return nil, fmt.Errorf("timeout waiting for vminitd to accept connections")
		}

		// Connect directly via vsock using kernel's vhost-vsock driver
		conn, err := vsock.Dial(vsockCID, vsockRPCPort, nil)
		if err != nil {
			log.G(ctx).WithError(err).Debug("qemu: failed to dial vsock")
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// Try to ping the TTRPC server with a deadline
		if err := conn.SetReadDeadline(time.Now().Add(pingDeadline)); err != nil {
			log.G(ctx).WithError(err).Debug("qemu: failed to set ping deadline")
			_ = conn.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err := pingTTRPC(conn); err != nil {
			log.G(ctx).WithError(err).WithField("deadline", pingDeadline).Debug("qemu: TTRPC ping failed, retrying")
			_ = conn.Close()
			pingDeadline += 10 * time.Millisecond
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// Clear the deadline and verify connection is still alive
		if err := conn.SetReadDeadline(time.Time{}); err != nil {
			log.G(ctx).WithError(err).Debug("qemu: failed to clear ping deadline")
			_ = conn.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err := pingTTRPC(conn); err != nil {
			log.G(ctx).WithError(err).Debug("qemu: TTRPC ping failed after clearing deadline, retrying")
			_ = conn.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// Connection is ready
		log.G(ctx).WithField("retry_time", time.Since(retryStart)).Info("qemu: TTRPC connection established")
		return conn, nil
	}
}

// monitorGuestRPC periodically checks if the in-guest vminitd RPC server is reachable.
// If the server disappears (e.g., guest reboot/poweroff), log a warning for debugging.
// Shutdown() is responsible for coordinating all shutdown actions.
func (q *Instance) monitorGuestRPC(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()

	failures := 0
	for {
		if q.getState() == vmStateShutdown {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		conn, err := vsock.Dial(vsockCID, vsockRPCPort, nil)
		if err == nil {
			if err := conn.SetDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
				log.G(ctx).WithError(err).Debug("qemu: failed to set guest RPC deadline")
				_ = conn.Close()
				continue
			}
			if err := pingTTRPC(conn); err != nil {
				failures++
				log.G(ctx).WithError(err).WithField("failures", failures).Debug("qemu: guest RPC ping failed")
			} else {
				failures = 0
			}
			_ = conn.Close()
		} else {
			failures++
			log.G(ctx).WithError(err).WithField("failures", failures).Debug("qemu: guest RPC dial failed")
		}

		// Log when guest becomes unreachable (may indicate reboot or hang)
		if failures >= 2 {
			log.G(ctx).WithField("failures", failures).Warning("qemu: guest RPC unreachable for 1 second (may be rebooting or hung)")
			// Don't force quit - Shutdown() will handle timeouts
		}
	}
}

// Helper functions

// openTAPInNetNS opens a TAP device in the specified network namespace and returns
// its file descriptor. This allows QEMU (running in init netns for vhost-vsock) to
// attach to TAP devices that live in sandbox namespaces.
//
// This approach is inspired by Kata Containers and is cleaner than moving TAPs between
// namespaces: file descriptors are namespace-agnostic, so once opened, the FD can be
// used from any namespace.
func openTAPInNetNS(ctx context.Context, tapName, netnsPath string) (*os.File, error) {
	targetNS, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return nil, fmt.Errorf("get target netns: %w", err)
	}
	defer func() { _ = targetNS.Close() }()

	origNS, err := netns.Get()
	if err != nil {
		return nil, fmt.Errorf("get current netns: %w", err)
	}
	defer func() { _ = origNS.Close() }()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Switch to target namespace
	if err := netns.Set(targetNS); err != nil {
		return nil, fmt.Errorf("set target netns: %w", err)
	}

	// Ensure we restore original namespace
	defer func() {
		if err := netns.Set(origNS); err != nil {
			log.G(ctx).WithError(err).Error("failed to restore original netns")
		}
	}()

	// Get the TAP device and ensure it's UP
	link, err := netlink.LinkByName(tapName)
	if err != nil {
		return nil, fmt.Errorf("lookup tap %s: %w", tapName, err)
	}

	// Bring the TAP UP if it's not already
	if link.Attrs().Flags&net.FlagUp == 0 {
		if err := netlink.LinkSetUp(link); err != nil {
			return nil, fmt.Errorf("bring tap %s up: %w", tapName, err)
		}
		log.G(ctx).WithField("tap", tapName).Debug("brought tap device up")
	}

	// Open /dev/net/tun and attach to the existing TAP device using TUNSETIFF ioctl
	tunFile, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w", err)
	}

	// Use syscall to attach to the existing TAP device
	// We need to use the TUNSETIFF ioctl with IFF_TAP | IFF_NO_PI flags
	// and set the device name
	const (
		tunSetIFF  = 0x400454ca
		iffTap     = 0x0002
		iffNoPI    = 0x1000
		iffVNetHdr = 0x4000
	)

	type ifReq struct {
		Name  [16]byte
		Flags uint16
		_     [22]byte // padding
	}

	var req ifReq
	copy(req.Name[:], tapName)
	req.Flags = iffTap | iffNoPI | iffVNetHdr

	//nolint:gosec // Required ioctl to attach to existing TAP device.
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, tunFile.Fd(), tunSetIFF, uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		_ = tunFile.Close()
		return nil, fmt.Errorf("TUNSETIFF ioctl failed: %w", errno)
	}

	log.G(ctx).WithFields(log.Fields{
		"tap":   tapName,
		"netns": netnsPath,
		"fd":    tunFile.Fd(),
	}).Info("opened TAP device FD in netns")

	return tunFile, nil
}

// waitForSocket waits for a Unix socket to appear
func waitForSocket(ctx context.Context, socketPath string, timeout time.Duration) error {
	startedAt := time.Now()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		if time.Since(startedAt) > timeout {
			return fmt.Errorf("timeout waiting for socket: %s", socketPath)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := os.Stat(socketPath); err == nil {
				return nil
			}
		}
	}
}

// formatInitArgs formats init arguments as a kernel command line string
func formatInitArgs(args []string) string {
	var result strings.Builder
	for i, arg := range args {
		if i > 0 {
			result.WriteString(" ")
		}
		// Quote arguments that contain spaces
		if len(arg) > 0 && (arg[0] == '-' || !needsQuoting(arg)) {
			result.WriteString(arg)
		} else {
			fmt.Fprintf(&result, "\"%s\"", arg)
		}
	}
	return result.String()
}

func needsQuoting(s string) bool {
	for _, c := range s {
		if c == ' ' || c == '\t' || c == '\n' {
			return true
		}
	}
	return false
}

// pingTTRPC sends an invalid request to a TTRPC server to check for a response
func pingTTRPC(rw net.Conn) error {
	n, err := rw.Write([]byte{
		0, 0, 0, 0, // Zero length
		0, 0, 0, 0, // Zero stream ID to force rejection response
		0, 0, // No type or flags
	})
	if err != nil {
		return fmt.Errorf("failed to write to TTRPC server: %w", err)
	} else if n != 10 {
		return fmt.Errorf("short write: %d bytes written", n)
	}
	p := make([]byte, 10)
	_, err = io.ReadFull(rw, p)
	if err != nil {
		return err
	}
	length := binary.BigEndian.Uint32(p[:4])
	sid := binary.BigEndian.Uint32(p[4:8])
	if sid != 0 {
		return fmt.Errorf("unexpected stream ID %d, expected 0", sid)
	}

	if length == 0 {
		return fmt.Errorf("expected error response, but got length 0")
	}

	_, err = io.Copy(io.Discard, io.LimitReader(rw, int64(length)))
	return err
}
