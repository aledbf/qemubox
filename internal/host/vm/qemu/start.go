//go:build linux

package qemu

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/containerd/log"
	"github.com/containerd/ttrpc"

	"github.com/aledbf/qemubox/containerd/internal/config"
	"github.com/aledbf/qemubox/containerd/internal/host/vm"
	"github.com/aledbf/qemubox/containerd/internal/paths"
)

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
	// Goroutine lifecycle: Exits when FIFO is closed (either by QEMU shutdown or explicit
	// close in Shutdown()). This allows proper cancellation during abnormal VM termination.
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

		// Store FIFO handle so Shutdown() can close it to cancel this goroutine
		q.consoleFifo = fifo

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

	// Require at least one network interface from CNI.
	// qemubox currently assumes a configured NIC during guest initialization.
	if len(q.nets) == 0 {
		return fmt.Errorf("no network interface configured: call AddNetwork() before Start()")
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
	// CRITICAL: Use context.WithoutCancel to prevent the RPC request context from
	// killing QEMU when it's canceled. The QEMU process must outlive the Create()
	// RPC call - it runs until explicit Shutdown(). Without this, when Create()
	// returns and the TTRPC layer cancels the context, Go's exec.CommandContext
	// would SIGKILL the QEMU process.
	//nolint:gosec // QEMU path and args are controlled by VM configuration.
	q.cmd = exec.CommandContext(context.WithoutCancel(ctx), q.binaryPath, qemuArgs...)
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

		if exitErr != nil {
			log.G(ctx).WithError(exitErr).Debug("qemu: process exited")
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

	// Release CID lease on failure (allows CID reuse)
	if q.cidLease != nil {
		_ = q.cidLease.Release()
		q.cidLease = nil
	}

	// Remove state directory on failure
	if q.stateDir != "" {
		_ = os.RemoveAll(q.stateDir)
	}
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
	}).Debug("qemu: starting VM process")

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
	// Note: q.mu is already held, so we can set these fields directly
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
	cfg := DefaultKernelCmdlineConfig()
	cfg.VsockCID = q.guestCID
	cfg.Network = startOpts.NetworkConfig
	cfg.InitArgs = startOpts.InitArgs
	return BuildKernelCmdline(cfg)
}

// buildQemuCommandLine constructs the QEMU command line arguments
func (q *Instance) buildQemuCommandLine(cmdlineArgs string) ([]string, error) {
	cfg, err := config.Get()
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}

	// Convert memory from bytes to MB
	memoryMB := int(q.resourceCfg.MemorySize / (1024 * 1024))
	memoryMaxMB := int(q.resourceCfg.MemoryHotplugSize / (1024 * 1024))

	// Calculate memory hotplug slots needed
	memorySlots := defaultMemorySlots
	if q.resourceCfg.MemoryHotplugSize <= q.resourceCfg.MemorySize {
		memorySlots = 0 // No hotplug needed if max equals initial
	}

	// Build QEMU command using fluent builder pattern
	builder := newQemuCommandBuilder().
		setNoDefaults(). // Disable default devices (prevents e1000e NIC needing ROM files)
		setBIOSPath(paths.QemuSharePath(cfg.Paths)).
		// Optimize: use kernel IRQ chip, disable HPET
		setMachine("q35", "accel=kvm", "kernel-irqchip=on", "hpet=off", "acpi=on").
		setCPU("host", "migratable=on").
		// CPU configuration for hotplug:
		// Simple topology: just specify initial CPUs and max CPUs, let QEMU handle the rest
		// This creates a single socket with enough capacity for maxcpus
		setSMP(q.resourceCfg.BootCPUs, q.resourceCfg.MaxCPUs).
		// Memory configuration - optimize slots based on hotplug needs
		setMemory(memoryMB, memorySlots, memoryMaxMB).
		setKernel(q.kernelPath).
		setInitrd(q.initrdPath).
		setKernelArgs(cmdlineArgs).
		setNoGraphic().
		// Serial console → FIFO pipe (producer side)
		// QEMU writes VM console output here; background goroutine reads and streams to log file
		// See setupConsoleFIFO() for the producer-consumer pipeline details
		setSerial(fmt.Sprintf("file:%s", q.consoleFifoPath)).
		// Vsock for guest communication (using vhost-vsock kernel module)
		addVsockDevice(int(q.guestCID)).
		// QMP for VM control
		setQMPUnixSocket(q.qmpSocketPath).
		// RNG device for entropy
		addVirtioRNG()

	// Add disks
	for i, disk := range q.disks {
		builder.addDisk(fmt.Sprintf("blk%d", i), disk)
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
		builder.addNIC(fmt.Sprintf("net%d", i), NICConfig{
			TapFD: fd,
			MAC:   nic.MAC,
		})
	}

	return builder.build(), nil
}

// Client returns the long-lived TTRPC client for communicating with the guest.
// This is used for the event stream and should not be shared for concurrent RPCs.
