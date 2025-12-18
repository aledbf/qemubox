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

	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	"github.com/mdlayher/vsock"
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

	inst := &Instance{
		binaryPath:    binaryPath,
		stateDir:      stateDir,
		logDir:        logDir,
		kernelPath:    kernelPath,
		initrdPath:    initrdPath,
		qmpSocketPath: filepath.Join(stateDir, "qmp.sock"),
		vsockPath:     filepath.Join(stateDir, "vsock.sock"),
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

	// Build kernel command line
	cmdlineArgs := q.buildKernelCommandLine(startOpts)

	// Build QEMU command line
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

	// If a network namespace is specified, start QEMU inside it
	// This allows QEMU to access TAP devices created by CNI in that netns
	if startOpts.NetworkNamespace != "" {
		log.G(ctx).WithField("netns", startOpts.NetworkNamespace).Info("qemu: starting inside network namespace")
		if err := q.startInNetNS(ctx, startOpts.NetworkNamespace); err != nil {
			return fmt.Errorf("failed to start qemu in netns: %w", err)
		}
	} else {
		if err := q.cmd.Start(); err != nil {
			return fmt.Errorf("failed to start qemu: %w", err)
		}
	}

	log.G(ctx).Info("qemu: process started, waiting for QMP socket...")

	// Monitor QEMU process in background
	// When QEMU exits (poweroff, reboot, crash), trigger cleanup
	go func() {
		exitErr := q.cmd.Wait()

		// Cancel background monitors
		q.mu.Lock()
		if q.runCancel != nil {
			q.runCancel()
		}
		q.mu.Unlock()

		// Update state to shutdown
		q.vmState.Store(uint32(vmStateShutdown))

		if exitErr != nil {
			log.G(ctx).WithError(exitErr).Warn("qemu: process exited with error")
		} else {
			log.G(ctx).Info("qemu: process exited cleanly")
		}

		// Clean up resources
		q.mu.Lock()
		defer q.mu.Unlock()

		// Close TTRPC client
		if q.client != nil {
			q.client.Close()
			q.client = nil
		}

		// Close vsock connection
		if q.vsockConn != nil {
			q.vsockConn.Close()
			q.vsockConn = nil
		}

		// Close QMP client
		if q.qmpClient != nil {
			q.qmpClient.Close()
			q.qmpClient = nil
		}
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
	q.mu.Lock()
	q.runCtx = runCtx
	q.runCancel = runCancel
	q.mu.Unlock()

	// Monitor QMP status as a fallback to detect guest shutdown even if
	// asynchronous events are missed. If QEMU transitions to a shutdown/paused
	// state, we explicitly send quit to ensure the process exits.
	go q.monitorVMStatus(runCtx)

	// Connect to vsock RPC server
	conn, err := q.connectVsockRPC(ctx)
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

// startInNetNS starts the QEMU process inside a network namespace.
// This allows QEMU to access TAP devices created by CNI in that netns.
func (q *Instance) startInNetNS(ctx context.Context, netnsPath string) error {
	// Get the target network namespace
	targetNS, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return fmt.Errorf("failed to get netns from path %s: %w", netnsPath, err)
	}
	defer targetNS.Close()

	// Get the current network namespace (to restore later)
	origNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("failed to get current netns: %w", err)
	}
	defer origNS.Close()

	// Lock the OS thread to ensure namespace operations work correctly
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Switch to the target network namespace
	if err := netns.Set(targetNS); err != nil {
		return fmt.Errorf("failed to set netns: %w", err)
	}

	// Ensure we always return to the original namespace
	defer func() {
		if err := netns.Set(origNS); err != nil {
			log.G(ctx).WithError(err).Error("failed to restore original netns")
		}
	}()

	// Start QEMU process (it will inherit the current netns)
	if err := q.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start qemu process: %w", err)
	}

	log.G(ctx).WithField("netns", netnsPath).Info("qemu: process started in network namespace")
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
		"reboot=k",                       // Use keyboard controller for reboot (ACPI compatible)
		"panic=1",                        // Reboot 1 second after kernel panic
		"net.ifnames=0", "biosdevname=0", // Predictable network naming
		"systemd.unified_cgroup_hierarchy=1", // Force cgroup v2
		"cgroup_no_v1=all",                   // Disable cgroup v1
		"nohz=off",                           // Disable tickless kernel (reduces overhead for short-lived VMs)
		"nomodules",
		"systemd.journald.forward_to_console",
		"systemd.log_color=false",
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

		"-machine", "q35,accel=kvm,kernel-irqchip=on,hpet=off", // Optimize: use kernel IRQ chip, disable HPET
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
		// Kernel boot - direct kernel boot using PVH loader
		"-kernel", q.kernelPath,
		"-initrd", q.initrdPath,
		"-append", cmdlineArgs,

		// Optimization: disable unnecessary devices
		"-nodefaults",
		"-no-user-config",
		"-nographic",
		"-no-reboot", // Exit QEMU when guest reboots (instead of restarting VM)

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
		args = append(args,
			"-netdev", fmt.Sprintf("tap,id=net%d,ifname=%s,script=no,downscript=no", i, nic.TapName),
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
	// Update state
	q.vmState.Store(uint32(vmStateShutdown))

	q.mu.Lock()
	if q.runCancel != nil {
		q.runCancel()
	}
	q.mu.Unlock()

	q.mu.Lock()
	defer q.mu.Unlock()

	var errs []error

	// Close TTRPC client first
	if q.client != nil {
		if err := q.client.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close ttrpc client: %w", err))
		}
		q.client = nil
	}

	// Close vsock connection
	if q.vsockConn != nil {
		if err := q.vsockConn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close vsock connection: %w", err))
		}
		q.vsockConn = nil
	}

	// Shutdown VM via QMP
	if q.qmpClient != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := q.qmpClient.Shutdown(shutdownCtx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown VM: %w", err))
		}
		q.qmpClient.Close()
		q.qmpClient = nil
	}

	// Wait for QEMU process to exit gracefully after ACPI powerdown
	if q.cmd != nil && q.cmd.Process != nil {
		// Create a channel to signal when process exits
		done := make(chan error, 1)
		go func() {
			done <- q.cmd.Wait()
		}()

		// Wait up to 3 seconds for graceful shutdown
		select {
		case <-done:
			// Process exited cleanly
			q.cmd = nil
		case <-time.After(3 * time.Second):
			// Timeout - force kill
			if err := q.cmd.Process.Kill(); err != nil {
				errs = append(errs, fmt.Errorf("kill process: %w", err))
			}
			<-done // Wait for Wait() to finish
			q.cmd = nil
		}
	}

	return errors.Join(errs...)
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

// monitorVMStatus polls QMP status to detect guest-initiated shutdown when
// QMP events are missed. If the status shows shutdown or paused, it forces
// QEMU to exit via a quit command.
func (q *Instance) monitorVMStatus(ctx context.Context) {
	log.G(ctx).Debug("qemu: starting VM status monitor")
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()

	for {
		if vmState(q.vmState.Load()) == vmStateShutdown {
			log.G(ctx).Debug("qemu: VM status monitor exiting (state shutdown)")
			return
		}

		select {
		case <-ctx.Done():
			log.G(ctx).Debug("qemu: VM status monitor exiting (context done)")
			return
		case <-t.C:
		}

		q.mu.Lock()
		qmp := q.qmpClient
		q.mu.Unlock()

		if qmp == nil {
			continue
		}

		status, err := qmp.QueryStatus(context.Background())
		if err != nil {
			log.G(ctx).WithError(err).Debug("qemu: query-status failed")
			continue
		}

		if status.Status == "shutdown" || status.Status == "paused" {
			log.G(ctx).WithField("status", status.Status).Info("qemu: VM no longer running, sending quit command")
			if err := qmp.execute(context.Background(), "quit", nil); err != nil {
				log.G(ctx).WithError(err).Warn("qemu: failed to send quit command after status change")
			}
			return
		}
	}
}

// monitorGuestRPC periodically checks if the in-guest vminitd RPC server is reachable.
// If the server disappears (e.g., guest reboot/poweroff), we force the VMM to exit.
func (q *Instance) monitorGuestRPC(ctx context.Context) {
	log.G(ctx).Debug("qemu: starting guest RPC monitor")
	t := time.NewTicker(1 * time.Second)
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
			} else {
				failures = 0
			}
			conn.Close()
		} else {
			failures++
		}

		if failures >= 3 {
			log.G(ctx).WithError(err).Warn("qemu: guest RPC unreachable, forcing VM exit")
			q.forceQuit(ctx)
			return
		}
	}
}

// forceQuit attempts to terminate the VM process via QMP quit, falling back to SIGKILL.
func (q *Instance) forceQuit(ctx context.Context) {
	q.mu.Lock()
	qmp := q.qmpClient
	cmd := q.cmd
	q.mu.Unlock()

	if qmp != nil {
		if err := qmp.execute(context.Background(), "quit", nil); err != nil {
			log.G(ctx).WithError(err).Warn("qemu: failed to send quit command during force quit")
		}
	}
	if cmd != nil && cmd.Process != nil {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			log.G(ctx).WithError(err).Warn("qemu: failed to kill process during force quit")
		}
	}
}

// Helper functions

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
	result := ""
	for i, arg := range args {
		if i > 0 {
			result += " "
		}
		// Quote arguments that contain spaces
		if len(arg) > 0 && (arg[0] == '-' || !needsQuoting(arg)) {
			result += arg
		} else {
			result += fmt.Sprintf("\"%s\"", arg)
		}
	}
	return result
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
