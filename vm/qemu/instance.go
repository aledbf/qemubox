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
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	"github.com/mdlayher/vsock"

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

	qemuSharePath, err := findQemuShare()
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
		qemuSharePath: qemuSharePath,
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

	if err := q.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start qemu: %w", err)
	}

	log.G(ctx).Info("qemu: process started, waiting for QMP socket...")

	// Monitor QEMU process in background
	go func() {
		if err := q.cmd.Wait(); err != nil {
			log.G(ctx).WithError(err).Error("qemu: process exited unexpectedly")
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

	// Connect to vsock RPC server
	conn, err := q.connectVsockRPC(ctx)
	if err != nil {
		q.cmd.Process.Kill()
		q.qmpClient.Close()
		return err
	}

	q.vsockConn = conn
	q.client = ttrpc.NewClient(conn)

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
	}
	initArgs = append(initArgs, startOpts.InitArgs...)

	// Build network configuration
	var netConfigs []string
	if startOpts.NetworkConfig != nil && startOpts.NetworkConfig.IP != "" {
		cfg := startOpts.NetworkConfig
		// IPv4 configuration
		netConfigs = append(netConfigs, fmt.Sprintf("ip=%s::%s:%s::eth0:none",
			cfg.IP,
			cfg.Gateway,
			cfg.Netmask))

		// Add DNS servers
		for _, dns := range cfg.DNS {
			netConfigs = append(netConfigs, fmt.Sprintf("nameserver=%s", dns))
		}
	}

	// Build kernel command line
	cmdlineParts := []string{
		"console=ttyS0",
		"net.ifnames=0", "biosdevname=0",
		"systemd.unified_cgroup_hierarchy=1", // Force cgroup v2
		"cgroup_no_v1=all",                   // Disable cgroup v1
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

	args := []string{
		// BIOS/firmware path - must come first
		"-L", q.qemuSharePath,

		"-machine", "q35",
		"-enable-kvm",

		"-smp", fmt.Sprintf("%d,maxcpus=%d", q.resourceCfg.BootCPUs, q.resourceCfg.MaxCPUs),
		"-m", fmt.Sprintf("%d,slots=16,maxmem=%dM", memoryMB, memoryMaxMB),
		"-cpu", "host,migratable=on",

		// Kernel boot
		"-kernel", q.kernelPath,
		"-initrd", q.initrdPath,
		"-append", cmdlineArgs,

		// Defaults
		"-nodefaults", "-no-user-config", "-nographic",

		// Serial console - redirect to log file
		"-serial", fmt.Sprintf("file:%s", q.consolePath),

		// Vsock for guest communication (using vhost-vsock kernel module)
		"-device", fmt.Sprintf("vhost-vsock-pci,guest-cid=%d", vsockCID),

		// QMP for VM control
		"-qmp", fmt.Sprintf("unix:%s,server=on,wait=off", q.qmpSocketPath),

		// RNG device
		"-device", "virtio-rng-pci",
	}

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

	// Kill QEMU process
	if q.cmd != nil && q.cmd.Process != nil {
		// Wait a bit for graceful shutdown
		time.Sleep(500 * time.Millisecond)

		if err := q.cmd.Process.Kill(); err != nil {
			errs = append(errs, fmt.Errorf("kill process: %w", err))
		}
		q.cmd.Wait()
		q.cmd = nil
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

// logDebugInfo logs console and QEMU logs for debugging when VM startup fails
func (q *Instance) logDebugInfo(ctx context.Context) {
	log.G(ctx).Error("qemu: timeout waiting for VM to start")

	// Try to read QEMU logs
	if qemuLogData, err := os.ReadFile(q.qemuLogPath); err == nil && len(qemuLogData) > 0 {
		if len(qemuLogData) > maxLogBytes {
			qemuLogData = qemuLogData[len(qemuLogData)-maxLogBytes:]
		}
		log.G(ctx).WithField("qemu_log", string(qemuLogData)).Error("qemu: QEMU stderr output (last 4KB)")
	} else {
		log.G(ctx).WithFields(log.Fields{
			"qemu_log_path": q.qemuLogPath,
			"error":         err,
		}).Error("qemu: no QEMU log output")
	}

	// Try to read VM console output
	if consoleData, err := os.ReadFile(q.consolePath); err == nil && len(consoleData) > 0 {
		if len(consoleData) > maxLogBytes {
			consoleData = consoleData[len(consoleData)-maxLogBytes:]
		}
		log.G(ctx).WithField("console", string(consoleData)).Error("qemu: VM console output (last 4KB)")
	} else {
		log.G(ctx).WithFields(log.Fields{
			"console_path": q.consolePath,
			"error":        err,
		}).Error("qemu: no console output (vminitd may not be starting)")
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
