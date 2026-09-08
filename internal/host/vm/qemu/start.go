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
	"time"

	"github.com/containerd/log"
	"github.com/containerd/ttrpc"

	"github.com/spin-stack/spinbox/internal/host/vm"
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
	q.mu.Lock()
	q.consoleFile = consoleFile
	q.mu.Unlock()

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

		// Store FIFO handle so Shutdown() can close it to cancel this goroutine.
		// Under the lock: this goroutine outlives Start, and Shutdown reads the
		// same field to close it.
		q.mu.Lock()
		q.consoleFifo = fifo
		q.mu.Unlock()

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
func (q *Instance) validateConfiguration(noNetwork bool) error {
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

	// Validate all disk paths exist. For a disk attached by pointer it is the
	// pointer that has to be there; what it names is read when the command line
	// is built, and is checked then, because between here and there it can change.
	for _, disk := range q.disks {
		path, what := disk.Path, "disk"
		if disk.Pointer != "" {
			path, what = disk.Pointer, "disk pointer"
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("%s not found at %s: %w", what, path, err)
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

	// A VM without a NIC has to have said so.
	//
	// The rule was written when the guest read its address from the kernel
	// command line during initialization, so a VM without one was unrecoverable.
	// It is not any more: system.Apply skips networking when it is given no
	// address, which is what the VM a template is made from has always relied on
	// — it does not know which container it will become, and the NIC arrives with
	// the VM restored from it.
	//
	// So what is left to catch is a caller that *meant* to have a network and
	// dropped it, and that is worth catching: it is a container with no address
	// and no error. A caller that meant the opposite says WithoutNetwork, because
	// from in here the two look identical.
	if len(q.nets) == 0 && !q.buildsTemplate() && !noNetwork {
		return fmt.Errorf("no network interface configured: call AddNetwork() before Start(), or say vm.WithoutNetwork()")
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
			q.mu.Lock()
			q.closeTAPFilesLocked()
			q.mu.Unlock()
			return fmt.Errorf("failed to open tap %s in netns: %w", nic.TapName, err)
		}
		// Store the file descriptor
		q.mu.Lock()
		nic.TapFile = tapFile
		q.mu.Unlock()
	}
	q.mu.Lock()
	q.tapNetns = netns
	q.mu.Unlock()
	return nil
}

// closeTAPFilesLocked closes all TAP file descriptors and resets the netns
// tracking. This centralizes TAP FD cleanup logic used in multiple error paths.
//
// The caller must hold q.mu. Every caller is a cleanup path that is already
// mutating fields under it, which is why this does not take it: taking it here
// would deadlock against them, loudly under the race detector and silently
// otherwise. See vmMutex.
func (q *Instance) closeTAPFilesLocked() {
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
	cmd := exec.CommandContext(context.WithoutCancel(ctx), q.binaryPath, qemuArgs...)
	cmd.Stdout = qemuLogFile
	cmd.Stderr = qemuLogFile
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	waitCh := make(chan error, 1)

	// Pass TAP file descriptors to QEMU via ExtraFiles
	// These will be available to QEMU as FD 3, 4, 5, ... (0,1,2 are stdin/stdout/stderr)
	var extraFiles []*os.File
	for _, nic := range q.nets {
		if nic.TapFile != nil {
			extraFiles = append(extraFiles, nic.TapFile)
		}
	}
	if len(extraFiles) > 0 {
		cmd.ExtraFiles = extraFiles
		log.G(ctx).WithField("fd_count", len(extraFiles)).Debug("passing TAP file descriptors to QEMU")
	}

	if err := cmd.Start(); err != nil {
		// Clean up TAP FDs on start failure
		for _, f := range extraFiles {
			_ = f.Close()
		}
		return fmt.Errorf("failed to start qemu: %w", err)
	}

	// Published once the process exists, so a reader never sees a cmd that has
	// not been started.
	q.mu.Lock()
	q.cmd = cmd
	q.waitCh = waitCh
	q.mu.Unlock()

	log.G(ctx).Info("qemu: process started, waiting for QMP socket...")

	// cmd and waitCh are handed over rather than read back out of the instance:
	// the goroutine outlives Start and would otherwise race with Shutdown's
	// teardown of the same fields.
	q.monitorProcess(ctx, cmd, waitCh)
	return nil
}

func (q *Instance) monitorProcess(ctx context.Context, cmd *exec.Cmd, waitCh chan error) {
	// Monitor QEMU process in background
	// Process monitor: detects when QEMU exits (poweroff, reboot, crash)
	// This goroutine only signals exit - cleanup is handled by Shutdown()
	go func() {
		exitErr := cmd.Wait()

		if exitErr != nil {
			log.G(ctx).WithError(exitErr).Debug("qemu: process exited")
		}

		// Signal Shutdown() that process exited
		select {
		case waitCh <- exitErr:
		default:
			// Channel may be closed if Shutdown() already completed
		}

		// Both fields are written by Start, which is still running when this
		// goroutine is created - startQemuProcess launches it, and runCancel is
		// set twenty-seven lines later. Reading them without the lock is a race,
		// and the losing side is this one: a QEMU that exits early leaves the
		// background monitors running because runCancel still looked nil.
		//
		// They are copied out under the lock and called outside it, because
		// onProcessExit is the shim's and calling into it while holding this
		// instance's lock invites the deadlock vmMutex exists to report.
		q.mu.Lock()
		onExit, cancel := q.onProcessExit, q.runCancel
		q.mu.Unlock()

		// Invoke process exit callback if registered.
		// This provides a reliable signal for the shim to detect VM death
		// even when vsock connections don't receive EOF cleanly.
		if onExit != nil {
			onExit()
		}

		// Cancel background monitors if still running
		if cancel != nil {
			cancel()
		}

		// Don't close clients/TAP here - Shutdown() owns cleanup
		// This goroutine just detects process exit
	}()
}

// connectQMP waits for QEMU to expose its monitor socket and completes the QMP
// capabilities handshake. It reports when the socket appeared, which splits the
// launch cost in two: how long QEMU takes to reach chardev init, and how long it
// then takes to reach a main loop that answers. See BOOT_TIMELINE.
func (q *Instance) connectQMP(ctx context.Context) (time.Time, error) {
	qmpClient, tSocket, err := newQMPClient(ctx, q.qmpSocketPath)
	if err != nil {
		// Check if QEMU process is still running
		if q.cmd.Process != nil {
			_ = q.cmd.Process.Kill()
		}
		return tSocket, fmt.Errorf("failed to connect to QMP: %w", err)
	}
	q.mu.Lock()
	q.qmpClient = qmpClient
	q.mu.Unlock()
	return tSocket, nil
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

	q.mu.Lock()
	q.vsockConn = conn
	q.client = ttrpc.NewClient(conn)
	q.mu.Unlock()
	return nil
}

func (q *Instance) rollbackStart(success *bool) {
	if success != nil && *success {
		return
	}
	q.setState(vmStateNew)

	// Under the lock throughout: this undoes what Start published, and Start no
	// longer holds the lock for the caller. Everything here closes a handle or
	// kills a process that is already dying, so nothing waits.
	q.mu.Lock()
	defer q.mu.Unlock()

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
	q.closeTAPFilesLocked()

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
	// Read the options first: filling a struct has no side effects, and
	// validateConfiguration has to know whether this VM was *meant* to have no
	// network. They used to be parsed further down, which meant the check ran
	// before the caller's answer existed.
	startOpts := vm.StartOpts{}
	for _, o := range opts {
		o(&startOpts)
	}

	// Check and update state atomically
	if !q.compareAndSwapState(vmStateNew, vmStateStarting) {
		currentState := q.getState()
		return fmt.Errorf("cannot start VM in state %d", currentState)
	}

	// Validate configuration before starting
	if err := q.validateConfiguration(startOpts.NoNetwork); err != nil {
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

	// No lock is held across this function, and that is deliberate.
	//
	// It used to hold q.mu for its whole body - a hundred and twelve lines, over
	// launching QEMU, loading a migration stream and connecting vsock with
	// retries, seconds of I/O. That is what the root CLAUDE.md says not to do,
	// and it made every helper called from here unable to take the lock, which
	// deadlocked this package twice.
	//
	// It was never needed for the thing a lock is for. Start is entered through a
	// compare-and-swap from vmStateNew to vmStateStarting, so only one goroutine
	// is ever inside it; there is no second writer to exclude. What the lock
	// protects is readers, and every reader that matters - Client, DialClient,
	// CPUHotplugger, StartStream - refuses to do anything unless the state is
	// vmStateRunning, which this function only sets on its last line.
	//
	// So the fields are published under the lock where they are written, by the
	// helper that writes them, and nothing holds it while waiting for a VM.

	// Remove old socket files if they exist
	if err := os.Remove(q.qmpSocketPath); err != nil && !os.IsNotExist(err) {
		log.G(ctx).WithError(err).Debug("qemu: failed to remove QMP socket")
	}
	if err := os.Remove(q.vsockPath); err != nil && !os.IsNotExist(err) {
		log.G(ctx).WithError(err).Debug("qemu: failed to remove vsock path")
	}

	// Store network configuration
	q.mu.Lock()
	q.networkCfg = startOpts.NetworkConfig
	q.mu.Unlock()

	// Open TAP file descriptors in the network namespace.
	// QEMU (running in init netns for vhost-vsock) will use these FDs to attach to
	// TAP devices that stay in their sandbox namespaces. This is the Kata Containers approach:
	// FDs are namespace-agnostic, so no need to move TAPs between namespaces.
	if err := q.openTapFiles(ctx, startOpts.NetworkNamespace); err != nil {
		return err
	}

	// Build kernel command line
	cmdlineArgs := q.buildKernelCommandLine(startOpts)

	// The machine, and its command line. Both come from one machine.Spec, so the
	// thing QEMU is given and the thing a template's fingerprint describes cannot
	// be different machines. See spec.go.
	spec, err := q.spec(cmdlineArgs)
	if err != nil {
		return err
	}
	qemuArgs, err := spec.Args()
	if err != nil {
		return err
	}

	// Print full command for manual testing
	log.G(ctx).WithFields(log.Fields{
		"binary":  q.binaryPath,
		"cmdline": strings.Join(qemuArgs, " "),
	}).Debug("qemu: starting VM process")

	// tExec marks the QEMU process launch; the deltas below isolate where a
	// normal (non-debug) boot spends its cold-start time. See BOOT_TIMELINE below.
	tExec := time.Now()
	if err := q.startQemuProcess(ctx, qemuArgs); err != nil {
		return err
	}

	// Connect to QMP for control
	tSocket, err := q.connectQMP(ctx)
	if err != nil {
		return err
	}
	tQMP := time.Now()

	// A restored VM has no state until it is told where to read it from; it
	// cannot answer on vsock before that.
	if q.restoreStatePath != "" {
		if err := q.loadTemplate(ctx); err != nil {
			return err
		}
	}

	log.G(ctx).Info("qemu: QMP connected, waiting for vsock...")

	// Create long-lived context for background monitors; Start ctx may be cancelled by callers.
	// We use context.Background() here because the background monitors need to outlive
	// the Start() call and continue running until explicit Shutdown().
	runCtx, runCancel := context.WithCancel(context.WithoutCancel(ctx))
	q.mu.Lock()
	q.runCtx = runCtx
	q.runCancel = runCancel
	q.mu.Unlock()

	// Connect to vsock RPC server
	if err := q.connectVsockClient(ctx); err != nil {
		return err
	}
	tVsock := time.Now()

	// Monitor liveness of the guest RPC server; if it goes away (guest reboot/poweroff)
	// ensure QEMU exits so the shim can clean up.
	go q.monitorGuestRPC(runCtx)

	// Mark as successfully started
	success = true
	q.setState(vmStateRunning)

	// BOOT_TIMELINE isolates VM cold-start on a normal boot, the half that the
	// initcall/userspace profiles do not cover:
	//   qemu_launch = exec + machine/firmware init until QMP responds, split into
	//                 qmp_socket (exec until the monitor socket exists) and
	//                 qmp_handshake (the capabilities negotiation on it)
	//   guest_boot  = kernel boot + vminitd init until its vsock RPC accepts
	// Container create/start happen afterwards over RPC and are logged separately.
	// Always on (one line per VM start) so plain boots emit it - no debug mode.
	log.G(ctx).Infof("BOOT_TIMELINE qemu_launch_us=%d qmp_socket_us=%d qmp_handshake_us=%d guest_boot_us=%d total_us=%d",
		tQMP.Sub(tExec).Microseconds(),
		tSocket.Sub(tExec).Microseconds(),
		tQMP.Sub(tSocket).Microseconds(),
		tVsock.Sub(tQMP).Microseconds(),
		tVsock.Sub(tExec).Microseconds())

	log.G(ctx).Info("qemu: VM fully initialized")

	return nil
}

// buildKernelCommandLine constructs the kernel command line
func (q *Instance) buildKernelCommandLine(startOpts vm.StartOpts) string {
	cfg := DefaultKernelCmdlineConfig()
	cfg.VsockCID = q.guestCID
	cfg.Network = startOpts.NetworkConfig
	cfg.InitArgs = startOpts.InitArgs
	cfg.DiskCount = len(q.disks)
	cfg.ExtrasDiskIndex = startOpts.ExtrasDiskIndex
	// Boot profiling is enabled per-VM (via the debug-boot annotation) or
	// host-wide (SPINBOX_DEBUG_BOOT).
	cfg.Debug = startOpts.DebugBoot || bootDebugEnabled()
	// The root port only exists on a VM that deals in templates, and the guest
	// has to be told to look past bus 0 to find it.
	return BuildKernelCmdline(cfg)
}

// bootDebugEnabled reports whether boot profiling was requested host-wide via
// SPINBOX_DEBUG_BOOT (initcall_debug + verbose kernel output).
func bootDebugEnabled() bool {
	switch os.Getenv("SPINBOX_DEBUG_BOOT") {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// Client returns the long-lived TTRPC client for communicating with the guest.
// This is used for the event stream and should not be shared for concurrent RPCs.
