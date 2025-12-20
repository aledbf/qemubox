package qemu

import (
	"context"
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

	"github.com/aledbf/beacon/containerd/vm"
)

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
	logDir := filepath.Join("/var/log/beacon", containerID)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	qmpSocketPath := filepath.Join(stateDir, "qmp.sock")
	vsockPath := filepath.Join(stateDir, "vsock.sock")

	// Unix domain sockets have a 108-character path limit on Linux (including null terminator)
	// Validate socket paths to prevent runtime errors
	const maxSocketPathLen = 107
	if len(qmpSocketPath) > maxSocketPathLen {
		return nil, fmt.Errorf("QMP socket path too long (%d > %d): %s", len(qmpSocketPath), maxSocketPathLen, qmpSocketPath)
	}
	if len(vsockPath) > maxSocketPathLen {
		return nil, fmt.Errorf("vsock socket path too long (%d > %d): %s", len(vsockPath), maxSocketPathLen, vsockPath)
	}

	inst := &Instance{
		binaryPath:    binaryPath,
		stateDir:      stateDir,
		logDir:        logDir,
		kernelPath:    kernelPath,
		initrdPath:    initrdPath,
		qmpSocketPath: qmpSocketPath,
		vsockPath:     vsockPath,
		consolePath:   filepath.Join(logDir, "console.log"),
		qemuLogPath:   filepath.Join(logDir, "qemu.log"),
		disks:         []*DiskConfig{},
		nets:          []*NetConfig{},
		resourceCfg:   resourceCfg,
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

// AddDisk schedules a disk to be attached to the VM
func (q *Instance) AddDisk(ctx context.Context, blockID, mountPath string, opts ...vm.MountOpt) error {
	if vmState(q.vmState.Load()) != vmStateNew {
		return errors.New("cannot add disk after VM started")
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	var mc vm.MountConfig
	for _, o := range opts {
		o(&mc)
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
	if vmState(q.vmState.Load()) != vmStateNew {
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

// Start starts the QEMU VM
func (q *Instance) Start(ctx context.Context, opts ...vm.StartOpt) error {
	// Check and update state atomically
	if !q.vmState.CompareAndSwap(uint32(vmStateNew), uint32(vmStateStarting)) {
		currentState := vmState(q.vmState.Load())
		return fmt.Errorf("cannot start VM in state %d", currentState)
	}

	// Ensure we revert to New on failure
	success := false
	defer func() {
		if !success {
			q.vmState.Store(uint32(vmStateNew))
			// Close any opened TAP FDs on failure
			for _, nic := range q.nets {
				if nic.TapFile != nil {
					nic.TapFile.Close()
					nic.TapFile = nil
				}
			}
			q.tapNetns = ""
		}
	}()

	q.mu.Lock()
	defer q.mu.Unlock()

	// Remove old socket files if they exist
	os.Remove(q.qmpSocketPath)
	os.Remove(q.vsockPath)

	// Parse start options
	startOpts := vm.StartOpts{}
	for _, o := range opts {
		o(&startOpts)
	}

	// Store network configuration
	q.networkCfg = startOpts.NetworkConfig

	// NICs require network namespace - enforce this requirement
	if len(q.nets) > 0 && startOpts.NetworkNamespace == "" {
		return fmt.Errorf("network namespace is required when NICs are configured")
	}

	// Open TAP file descriptors in the network namespace.
	// QEMU (running in init netns for vhost-vsock) will use these FDs to attach to
	// TAP devices that stay in their sandbox namespaces. This is the Kata Containers approach:
	// FDs are namespace-agnostic, so no need to move TAPs between namespaces.
	if len(q.nets) > 0 {
		for _, nic := range q.nets {
			tapFile, err := openTAPInNetNS(ctx, nic.TapName, startOpts.NetworkNamespace)
			if err != nil {
				// Clean up any already-opened FDs on failure
				for _, prevNic := range q.nets {
					if prevNic.TapFile != nil {
						prevNic.TapFile.Close()
						prevNic.TapFile = nil
					}
				}
				return fmt.Errorf("failed to open tap %s in netns: %w", nic.TapName, err)
			}
			// Store the file descriptor
			nic.TapFile = tapFile
		}
		q.tapNetns = startOpts.NetworkNamespace
	}

	// Build kernel command line
	cmdlineArgs := q.buildKernelCommandLine(startOpts)

	// Build QEMU command line (now uses the renamed TAP names)
	qemuArgs := q.buildQemuCommandLine(cmdlineArgs)

	// Print full command for manual testing
	log.G(ctx).WithFields(log.Fields{
		"binary":  q.binaryPath,
		"cmdline": strings.Join(qemuArgs, " "),
	}).Debug("qemu: starting vm")

	// Create QEMU log file for stdout/stderr
	qemuLogFile, err := os.Create(q.qemuLogPath)
	if err != nil {
		return fmt.Errorf("failed to create qemu log file: %w", err)
	}

	// Start QEMU
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
			f.Close()
		}
		return fmt.Errorf("failed to start qemu: %w", err)
	}

	log.G(ctx).Info("qemu: process started, waiting for QMP socket...")

	// Monitor QEMU process in background
	// Process monitor: detects when QEMU exits (poweroff, reboot, crash)
	// This goroutine only signals exit - cleanup is handled by Shutdown()
	go func() {
		exitErr := q.cmd.Wait()

		logger := log.G(ctx)
		if exitErr != nil {
			logger.WithError(exitErr).Debug("qemu: process exited")
		} else {
			logger.Debug("qemu: process exited cleanly")
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

	// Connect to QMP for control
	qmpClient, err := NewQMPClient(ctx, q.qmpSocketPath)
	if err != nil {
		// Check if QEMU process is still running
		if q.cmd.Process != nil {
			q.cmd.Process.Kill()
		}
		return fmt.Errorf("failed to connect to QMP: %w", err)
	}
	q.qmpClient = qmpClient

	log.G(ctx).Info("qemu: QMP connected, waiting for vsock...")

	// Create long-lived context for background monitors; Start ctx may be cancelled by callers.
	runCtx, runCancel := context.WithCancel(context.Background())
	// Note: q.mu is already held (locked at line 200), so we can set these fields directly
	q.runCtx = runCtx
	q.runCancel = runCancel

	// Connect to vsock RPC server
	log.G(ctx).Debug("qemu: about to call connectVsockRPC")
	select {
	case <-ctx.Done():
		log.G(ctx).WithError(ctx.Err()).Error("qemu: context cancelled before connectVsockRPC")
		q.cmd.Process.Kill()
		q.qmpClient.Close()
		return ctx.Err()
	default:
	}
	conn, err := q.connectVsockRPC(ctx)
	log.G(ctx).WithError(err).Debug("qemu: connectVsockRPC returned")
	if err != nil {
		q.cmd.Process.Kill()
		q.qmpClient.Close()
		return err
	}

	q.vsockConn = conn
	q.client = ttrpc.NewClient(conn)

	// Monitor liveness of the guest RPC server; if it goes away (guest reboot/poweroff)
	// ensure QEMU exits so the shim can clean up.
	go q.monitorGuestRPC(runCtx)

	// Mark as successfully started
	success = true
	q.vmState.Store(uint32(vmStateRunning))

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
		"-debug",
	}
	initArgs = append(initArgs, startOpts.InitArgs...)

	// Build network configuration
	var netConfigs []string
	if startOpts.NetworkConfig != nil && startOpts.NetworkConfig.IP != "" {
		cfg := startOpts.NetworkConfig
		// IPv4 configuration using kernel ip= parameter format:
		// ip=<client-ip>:<server-ip>:<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>:<dns0-ip>:<dns1-ip>
		ipParam := fmt.Sprintf("ip=%s::%s:%s::eth0:none",
			cfg.IP,
			cfg.Gateway,
			cfg.Netmask)

		// Append DNS servers to ip= parameter (kernel supports up to 2 DNS servers)
		for i, dns := range cfg.DNS {
			if i < 2 {
				ipParam += ":" + dns
			}
		}

		netConfigs = append(netConfigs, ipParam)
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
func (q *Instance) buildQemuCommandLine(cmdlineArgs string) []string {
	// Convert memory from bytes to MB
	memoryMB := q.resourceCfg.MemorySize / (1024 * 1024)
	memoryMaxMB := q.resourceCfg.MemoryHotplugSize / (1024 * 1024)

	// Calculate memory hotplug slots needed (0-16 based on usage)
	memorySlots := 8 // Reduced from 16 - adequate for most workloads
	if q.resourceCfg.MemoryHotplugSize <= q.resourceCfg.MemorySize {
		memorySlots = 0 // No hotplug needed if max equals initial
	}

	args := []string{
		// BIOS/firmware path needed for PVH boot loader (pvh.bin)
		"-L", "/usr/share/qemu",

		"-machine", "q35,accel=kvm,kernel-irqchip=on,hpet=off,acpi=on", // Optimize: use kernel IRQ chip, disable HPET
		"-cpu", "host,migratable=on",

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
		// Serial console - redirect to log file
		"-serial", fmt.Sprintf("file:%s", q.consolePath),

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
			panic(fmt.Sprintf("NIC %s has no TAP file descriptor", nic.TapName))
		}
		fd := 3 + i
		// Note: script= and downscript= are invalid with fd=
		// When using fd=, QEMU expects the TAP to be already configured
		args = append(args,
			"-netdev", fmt.Sprintf("tap,id=net%d,fd=%d", i, fd),
			"-device", fmt.Sprintf("virtio-net-pci,netdev=net%d,mac=%s", i, nic.MAC),
		)
	}

	return args
}

// Client returns the TTRPC client for communicating with the guest
func (q *Instance) Client() *ttrpc.Client {
	// Return nil if VM is shutdown
	if vmState(q.vmState.Load()) == vmStateShutdown {
		return nil
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	return q.client
}

// QMPClient returns the QMP client for controlling the VM
func (q *Instance) QMPClient() *QMPClient {
	// Return nil if VM is shutdown
	if vmState(q.vmState.Load()) == vmStateShutdown {
		return nil
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	return q.qmpClient
}

// Shutdown gracefully shuts down the VM
func (q *Instance) Shutdown(ctx context.Context) error {
	logger := log.G(ctx)
	logger.Info("qemu: Shutdown() called, initiating VM shutdown")

	// Mark VM as shutting down (atomic flag prevents re-entry)
	if !q.vmState.CompareAndSwap(uint32(vmStateRunning), uint32(vmStateShutdown)) {
		currentState := vmState(q.vmState.Load())
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

	// Send ACPI powerdown to guest OS
	if q.qmpClient != nil {
		logger.Info("qemu: sending ACPI powerdown via QMP")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := q.qmpClient.Shutdown(shutdownCtx); err != nil {
			logger.WithError(err).Warning("qemu: failed to send ACPI powerdown")
		}
		cancel()
	}

	// Brief wait to let guest start shutdown, then send quit
	// QEMU won't exit on its own - it always needs an explicit quit command
	if q.cmd != nil && q.cmd.Process != nil {
		logger.WithField("pid", q.cmd.Process.Pid).Debug("qemu: waiting briefly for guest shutdown to start")

		// Wait up to 500ms for guest to receive ACPI signal
		select {
		case exitErr := <-q.waitCh:
			// Unexpected early exit - shouldn't happen but handle it
			logger.WithError(exitErr).Debug("qemu: process exited during ACPI wait")
			q.cmd = nil
			goto cleanup
		case <-time.After(500 * time.Millisecond):
			// Expected - continue to quit command
		}

		// Send quit command to tell QEMU to exit
		if q.qmpClient != nil {
			logger.Debug("qemu: sending quit command to QEMU")
			quitCtx, quitCancel := context.WithTimeout(context.Background(), 1*time.Second)
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
					goto cleanup
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
			// Clean up QMP and TAPs before returning error
			if q.qmpClient != nil {
				_ = q.qmpClient.Close()
				q.qmpClient = nil
			}
			for _, nic := range q.nets {
				if nic.TapFile != nil {
					_ = nic.TapFile.Close()
				}
			}
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
			// Clean up QMP and TAPs before returning error
			if q.qmpClient != nil {
				_ = q.qmpClient.Close()
				q.qmpClient = nil
			}
			for _, nic := range q.nets {
				if nic.TapFile != nil {
					_ = nic.TapFile.Close()
				}
			}
			return fmt.Errorf("process did not exit after SIGKILL")
		}
		q.cmd = nil
	}

cleanup:
	// Close QMP client
	if q.qmpClient != nil {
		logger.Debug("qemu: closing QMP client")
		if err := q.qmpClient.Close(); err != nil {
			logger.WithError(err).Debug("qemu: error closing QMP client")
		}
		q.qmpClient = nil
	}

	// Close TAP file descriptors
	for _, nic := range q.nets {
		if nic.TapFile != nil {
			if err := nic.TapFile.Close(); err != nil {
				logger.WithError(err).WithField("tap", nic.TapName).Debug("error closing TAP file descriptor")
			}
			nic.TapFile = nil
			logger.WithField("tap", nic.TapName).Debug("closed TAP file descriptor")
		}
	}
	q.tapNetns = ""

	return nil
}

// StartStream creates a new stream connection to the VM for I/O operations.
func (q *Instance) StartStream(ctx context.Context) (uint32, net.Conn, error) {
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
				conn.Close()
				return 0, nil, fmt.Errorf("failed to write stream id: %w", err)
			}

			// Wait for stream ID acknowledgment from vminitd
			var streamAck [4]byte
			if _, err := io.ReadFull(conn, streamAck[:]); err != nil {
				conn.Close()
				return 0, nil, fmt.Errorf("failed to read stream ack: %w", err)
			}

			if binary.BigEndian.Uint32(streamAck[:]) != sid {
				conn.Close()
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
	log.G(ctx).Debug("qemu: sleeping 500ms before vsock dial")
	time.Sleep(500 * time.Millisecond)
	log.G(ctx).Debug("qemu: starting vsock dial loop")

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
		conn.SetReadDeadline(time.Now().Add(pingDeadline))
		if err := pingTTRPC(conn); err != nil {
			log.G(ctx).WithError(err).WithField("deadline", pingDeadline).Debug("qemu: TTRPC ping failed, retrying")
			conn.Close()
			pingDeadline += 10 * time.Millisecond
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// Clear the deadline and verify connection is still alive
		conn.SetReadDeadline(time.Time{})
		if err := pingTTRPC(conn); err != nil {
			log.G(ctx).WithError(err).Debug("qemu: TTRPC ping failed after clearing deadline, retrying")
			conn.Close()
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
	log.G(ctx).Debug("qemu: starting guest RPC monitor")
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()

	failures := 0
	for {
		if vmState(q.vmState.Load()) == vmStateShutdown {
			log.G(ctx).Debug("qemu: guest RPC monitor exiting (state shutdown)")
			return
		}

		select {
		case <-ctx.Done():
			log.G(ctx).Debug("qemu: guest RPC monitor exiting (context done)")
			return
		case <-t.C:
		}

		conn, err := vsock.Dial(vsockCID, vsockRPCPort, nil)
		if err == nil {
			conn.SetDeadline(time.Now().Add(200 * time.Millisecond))
			if err := pingTTRPC(conn); err != nil {
				failures++
				log.G(ctx).WithError(err).WithField("failures", failures).Debug("qemu: guest RPC ping failed")
			} else {
				failures = 0
			}
			conn.Close()
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
	defer targetNS.Close()

	origNS, err := netns.Get()
	if err != nil {
		return nil, fmt.Errorf("get current netns: %w", err)
	}
	defer origNS.Close()

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
		TUNSETIFF    = 0x400454ca
		IFF_TAP      = 0x0002
		IFF_NO_PI    = 0x1000
		IFF_VNET_HDR = 0x4000
	)

	type ifReq struct {
		Name  [16]byte
		Flags uint16
		_     [22]byte // padding
	}

	var req ifReq
	copy(req.Name[:], tapName)
	req.Flags = IFF_TAP | IFF_NO_PI | IFF_VNET_HDR

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, tunFile.Fd(), TUNSETIFF, uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		tunFile.Close()
		return nil, fmt.Errorf("TUNSETIFF ioctl failed: %v", errno)
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
