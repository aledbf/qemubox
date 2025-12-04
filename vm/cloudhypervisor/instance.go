package cloudhypervisor

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

	"github.com/aledbf/beacon/containerd/vm"
)

const (
	vmStartTimeout      = 10 * time.Second
	vsockCID            = 3    // Guest context ID
	vsockRPCPort        = 1025 // Port for TTRPC communication
	vsockStreamPort     = 1026 // Port for streaming I/O
	defaultBootCPUs     = 1
	defaultMaxCPUs      = 2
	defaultMemorySize   = 512 * 1024 * 1024      // 512 MiB
	defaultMemoryMax    = 2 * 1024 * 1024 * 1024 // 2 GiB
	connectRetryTimeout = 10 * time.Second
	connectAckTimeout   = 100 * time.Millisecond
	maxLogBytes         = 4096 // Maximum log output to include in errors
)

func newInstance(ctx context.Context, binaryPath, stateDir string, resourceCfg *VMResourceConfig) (*Instance, error) {
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
		resourceCfg = &VMResourceConfig{
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

	// Cloud Hypervisor vsock model:
	// - Cloud Hypervisor creates a single Unix socket at the specified path
	// - Host connects to that socket and sends "CONNECT <port>\n" to specify the vsock port
	// - The same socket is used for all vsock port connections (multiplexed)
	vsockSocketPath := filepath.Join(stateDir, "vsock")

	// Use dedicated log directory for persistent logs
	containerID := filepath.Base(stateDir)
	logDir := filepath.Join("/var/log/beacon", containerID)

	// Create log directory
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	consolePath := filepath.Join(logDir, "console.log")

	inst := &Instance{
		binaryPath:    binaryPath,
		stateDir:      stateDir,
		logDir:        logDir,
		kernelPath:    kernelPath,
		initrdPath:    initrdPath,
		apiSocketPath: filepath.Join(stateDir, "api.sock"),
		vsockRPCPath:  vsockSocketPath, // Single socket for all vsock connections
		streamPath:    vsockSocketPath, // Same socket, different port via CONNECT header
		consolePath:   consolePath,
		disks:         []*DiskConfig{},
		filesystems:   []*FsConfig{},
		nets:          []*NetConfig{},
		resourceCfg:   resourceCfg,
	}

	log.G(ctx).WithFields(log.Fields{
		"containerID":   containerID,
		"bootCPUs":      resourceCfg.BootCPUs,
		"maxCPUs":       resourceCfg.MaxCPUs,
		"memorySize":    resourceCfg.MemorySize,
		"memoryHotplug": resourceCfg.MemoryHotplugSize,
	}).Debug("cloud-hypervisor: instance configured")

	return inst, nil
}

func (ch *Instance) AddFS(ctx context.Context, tag, mountPath string, opts ...vm.MountOpt) error {
	// Cloud Hypervisor: FsConfig (virtio-fs) requires shared memory and virtiofsd
	// Since we don't want either, we return an error to force the caller
	// to use a different approach (e.g., EROFS disk images via AddDisk)
	log.G(ctx).WithFields(log.Fields{
		"tag":  tag,
		"path": mountPath,
	}).Warn("cloud-hypervisor: AddFS not supported, use disk-based approach instead")

	return fmt.Errorf("AddFS not implemented for Cloud Hypervisor: use EROFS or block devices")
}

func (ch *Instance) AddDisk(ctx context.Context, blockID, mountPath string, opts ...vm.MountOpt) error {
	if vmState(ch.vmState.Load()) != vmStateNew {
		return errors.New("cannot add disk after VM started")
	}

	ch.mu.Lock()
	defer ch.mu.Unlock()

	var mc vm.MountConfig
	for _, o := range opts {
		o(&mc)
	}

	ch.disks = append(ch.disks, &DiskConfig{
		Path:     mountPath,
		Readonly: mc.Readonly,
		Id:       blockID,
	})

	log.G(ctx).WithFields(log.Fields{
		"blockID":  blockID,
		"path":     mountPath,
		"readonly": mc.Readonly,
		"vmdk":     mc.Vmdk,
	}).Debug("cloud-hypervisor: scheduled disk")

	return nil
}

func (ch *Instance) AddNIC(ctx context.Context, endpoint string, mac net.HardwareAddr, mode vm.NetworkMode, features, flags uint32) error {
	return fmt.Errorf("UNIX socket networking not supported by Cloud Hypervisor; use AddTAPNIC instead: %w", errdefs.ErrNotImplemented)
}

func (ch *Instance) AddTAPNIC(ctx context.Context, tapName string, mac net.HardwareAddr) error {
	if vmState(ch.vmState.Load()) != vmStateNew {
		return errors.New("cannot add NIC after VM started")
	}

	ch.mu.Lock()
	defer ch.mu.Unlock()

	// Cloud Hypervisor opens TAP device by name via API
	macStr := mac.String()
	ch.nets = append(ch.nets, &NetConfig{
		Tap: tapName,
		Mac: macStr,
	})

	log.G(ctx).WithFields(log.Fields{
		"tap": tapName,
		"mac": macStr,
	}).Debug("cloud-hypervisor: scheduled TAP NIC (will open by name)")

	return nil
}

func (ch *Instance) VMInfo() vm.VMInfo {
	return vm.VMInfo{
		Type:          "cloudhypervisor",
		SupportsTAP:   true,
		SupportsVSOCK: true,
	}
}

func (ch *Instance) Start(ctx context.Context, opts ...vm.StartOpt) error {
	// Check and update state atomically
	if !ch.vmState.CompareAndSwap(uint32(vmStateNew), uint32(vmStateStarting)) {
		currentState := vmState(ch.vmState.Load())
		return fmt.Errorf("cannot start VM in state %d", currentState)
	}

	// Ensure we revert to New on failure
	success := false
	defer func() {
		if !success {
			ch.vmState.Store(uint32(vmStateNew))
		}
	}()

	ch.mu.Lock()
	defer ch.mu.Unlock()

	// Remove old socket files if they exist
	os.Remove(ch.apiSocketPath)
	os.Remove(ch.vsockRPCPath)
	os.Remove(ch.streamPath)

	// Parse start options
	startOpts := vm.StartOpts{}
	for _, o := range opts {
		o(&startOpts)
	}

	// Store network configuration for use in --net flag
	ch.networkCfg = startOpts.NetworkConfig

	// Prepare init arguments for vminitd
	// The kernel command line format is: console=hvc0 init=/sbin/vminitd -- <args>
	// Everything after -- gets passed as argv to init
	initArgs := []string{
		fmt.Sprintf("-vsock-rpc-port=%d", vsockRPCPort),
		fmt.Sprintf("-vsock-stream-port=%d", vsockStreamPort),
		fmt.Sprintf("-vsock-cid=%d", vsockCID),
	}
	initArgs = append(initArgs, startOpts.InitArgs...)

	// Build network configuration for kernel command line
	var netConfigs []string
	if startOpts.NetworkConfig != nil {
		cfg := startOpts.NetworkConfig
		log.G(ctx).WithFields(log.Fields{
			"ip":      cfg.IP,
			"gateway": cfg.Gateway,
			"netmask": cfg.Netmask,
			"dns":     cfg.DNS,
		}).Info("cloud-hypervisor: received network configuration")

		// Only configure network if we have an IP address
		if cfg.IP != "" {
			// IPv4 configuration - base network config without DNS servers
			// Format: ip=<client-ip>::<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>
			// Use eth0 as the device name
			// Use 'none' for autoconf to keep interface up with static configuration
			netConfigs = append(netConfigs, fmt.Sprintf("ip=%s::%s:%s::eth0:none",
				cfg.IP,
				cfg.Gateway,
				cfg.Netmask))

			// Add each DNS server as a separate nameserver argument
			for _, dns := range cfg.DNS {
				netConfigs = append(netConfigs, fmt.Sprintf("nameserver=%s", dns))
			}

			log.G(ctx).WithField("netConfigs", netConfigs).Info("cloud-hypervisor: kernel network parameters configured")
		} else {
			log.G(ctx).Warn("cloud-hypervisor: NetworkConfig provided but IP is empty, network will not be configured")
		}
	} else {
		log.G(ctx).Info("cloud-hypervisor: no NetworkConfig provided, network will not be configured")
	}

	// Build kernel command line
	// Combine network config with init specification
	cmdlineParts := []string{
		"console=hvc0",
		"quiet",
		"loglevel=0",
		"systemd.show_status=0",
		"rd.systemd.show_status=0",
		"no_timer_check",
		"noreplace-smp",
		"rw",
		"net.ifnames=0", "biosdevname=0",
	}

	if len(netConfigs) > 0 {
		cmdlineParts = append(cmdlineParts, netConfigs...)
	}

	// Disable predictable interface names so virtio-net devices show up as eth0, eth1, etc.
	// instead of ens3, enp0s3, etc.
	cmdlineParts = append(cmdlineParts, fmt.Sprintf("init=/sbin/vminitd -- %s", formatInitArgs(initArgs)))

	cmdlineArgs := strings.Join(cmdlineParts, " ")
	log.G(ctx).WithFields(log.Fields{
		"cmdline":    cmdlineArgs,
		"vsockPath":  ch.vsockRPCPath,
		"networkCfg": startOpts.NetworkConfig,
	}).Debug("cloud-hypervisor: kernel command line prepared")

	// Build command-line arguments for Cloud Hypervisor
	chLogPath := filepath.Join(ch.logDir, "cloud-hypervisor.log")

	args := []string{
		"--api-socket", ch.apiSocketPath,
		"--log-file", chLogPath,
		"--seccomp", "false",
		"-v",
	}

	log.G(ctx).WithFields(log.Fields{
		"binary":       ch.binaryPath,
		"api_socket":   ch.apiSocketPath,
		"full_command": fmt.Sprintf("%s %s", ch.binaryPath, strings.Join(args, " ")),
	}).Info("cloud-hypervisor: starting in API-only mode")

	// Start Cloud Hypervisor
	ch.cmd = exec.CommandContext(ctx, ch.binaryPath, args...)

	// Cloud Hypervisor logs to --log-file, so we don't need stdout/stderr
	ch.cmd.Stdout = nil
	ch.cmd.Stderr = os.Stderr
	ch.cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	if err := ch.cmd.Start(); err != nil {
		// Try to get Cloud Hypervisor logs for debugging
		if chLogData, readErr := os.ReadFile(chLogPath); readErr == nil && len(chLogData) > 0 {
			log.G(ctx).WithField("ch_log", string(chLogData)).Error("cloud-hypervisor failed to start, see log")
		}
		return fmt.Errorf("failed to start cloud-hypervisor: %w", err)
	}

	log.G(ctx).Info("cloud-hypervisor: process started, waiting for API...")

	// Wait for API socket to appear (for runtime management)
	if err := ch.waitForAPISocket(ctx); err != nil {
		ch.cmd.Process.Kill()
		return err
	}

	// Create API client for runtime operations (hotplug, etc.)
	ch.api = NewCloudHypervisorClient(ch.apiSocketPath)

	// Test API connection
	if _, err := ch.api.mPing(ctx); err != nil {
		ch.cmd.Process.Kill()
		return fmt.Errorf("cloud-hypervisor API not responding: %w", err)
	}

	log.G(ctx).Info("cloud-hypervisor: API available for runtime operations")

	log.G(ctx).Info("cloud-hypervisor: creating VM configuration")

	// Build VM configuration with network devices included
	vmConfig := &VmConfig{
		Payload: &PayloadConfig{
			Kernel:    ch.kernelPath,
			Initramfs: ch.initrdPath,
			Cmdline:   cmdlineArgs,
		},
		Cpus: &CpusConfig{
			BootVcpus: ch.resourceCfg.BootCPUs,
			MaxVcpus:  ch.resourceCfg.MaxCPUs,
		},
		Memory: &MemoryConfig{
			Size:          ch.resourceCfg.MemorySize,
			HotplugMethod: "VirtioMem",
			HotplugSize:   ch.resourceCfg.MemoryHotplugSize,
			Mergeable:     true,
		},
		Vsock: &VsockConfig{
			Cid:    int64(vsockCID),
			Socket: ch.vsockRPCPath,
		},
		Rng: &RngConfig{
			Src: "/dev/urandom",
		},
		Serial: &ConsoleConfig{
			Mode: "File",
			File: filepath.Join(ch.stateDir, "serial.log"),
		},
		Console: &ConsoleConfig{
			Mode: "File",
			File: ch.consolePath,
		},
		Disks: ch.disks,
		Net:   ch.nets, // Include network devices in API config
	}

	// Create VM
	if err := ch.api.Create(ctx, vmConfig); err != nil {
		ch.cmd.Process.Kill()
		return fmt.Errorf("failed to create VM via API: %w", err)
	}

	log.G(ctx).Info("cloud-hypervisor: VM created, booting...")

	// Boot VM
	if err := ch.api.Boot(ctx); err != nil {
		ch.cmd.Process.Kill()
		return fmt.Errorf("failed to boot VM via API: %w", err)
	}

	log.G(ctx).Info("cloud-hypervisor: VM booted successfully")

	// Wait for vsock socket to appear (guest starts listening)
	if err := ch.waitForVsockSocket(ctx); err != nil {
		ch.cmd.Process.Kill()
		return err
	}

	// Connect to vsock RPC server
	conn, err := ch.connectVsockRPC(ctx)
	if err != nil {
		ch.cmd.Process.Kill()
		return err
	}

	ch.vsockConn = conn
	ch.client = ttrpc.NewClient(conn)

	// Mark as successfully started
	success = true
	ch.vmState.Store(uint32(vmStateRunning))

	log.G(ctx).Info("cloud-hypervisor: VM fully initialized")

	return nil
}

func (ch *Instance) Client() *ttrpc.Client {
	// Return nil if VM is shutdown, preventing use after close
	if vmState(ch.vmState.Load()) == vmStateShutdown {
		return nil
	}

	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.client
}

func (ch *Instance) Shutdown(ctx context.Context) error {
	// Update state
	ch.vmState.Store(uint32(vmStateShutdown))

	ch.mu.Lock()
	defer ch.mu.Unlock()

	var errs []error

	// Close TTRPC client first to stop any in-flight requests
	if ch.client != nil {
		if err := ch.client.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close ttrpc client: %w", err))
		}
		ch.client = nil
	}

	// Close vsock connection before shutting down VM
	if ch.vsockConn != nil {
		if err := ch.vsockConn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close vsock connection: %w", err))
		}
		ch.vsockConn = nil
	}

	// Shutdown VM via API
	if ch.api != nil {
		if err := ch.api.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown VM: %w", err))
		}
	}

	// Kill hypervisor process
	if ch.cmd != nil && ch.cmd.Process != nil {
		if err := ch.cmd.Process.Kill(); err != nil {
			errs = append(errs, fmt.Errorf("kill process: %w", err))
		}
		ch.cmd.Wait()
		ch.cmd = nil
	}

	return errors.Join(errs...)
}

// StartStream creates a new stream connection to the VM for I/O operations.
//
// Cloud Hypervisor vsock stream protocol:
// 1. Connect to the vsock Unix socket (multiplexed for all vsock ports)
// 2. Send "CONNECT <port>\n" to Cloud Hypervisor to specify vsock stream port
// 3. Wait for "OK <port>\n" acknowledgment from Cloud Hypervisor
// 4. Send 4-byte stream ID (big-endian uint32) to vminitd
// 5. Wait for 4-byte stream ID acknowledgment from vminitd
// 6. Connection is ready for bidirectional streaming
//
// Returns a unique stream ID and the connection, or an error if setup fails.
func (ch *Instance) StartStream(ctx context.Context) (uint32, net.Conn, error) {
	const timeIncrement = 10 * time.Millisecond
	for d := timeIncrement; d < time.Second; d += timeIncrement {
		// Generate unique stream ID (wraps at uint32 max, checked for exhaustion)
		sid := atomic.AddUint32(&ch.streamC, 1)
		if sid == 0 {
			return 0, nil, fmt.Errorf("exhausted stream identifiers: %w", errdefs.ErrUnavailable)
		}

		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		default:
		}

		if _, err := os.Stat(ch.streamPath); err == nil {
			conn, err := net.Dial("unix", ch.streamPath)
			if err != nil {
				return 0, nil, fmt.Errorf("failed to connect to stream server: %w", err)
			}

			// Step 1: Send CONNECT header to Cloud Hypervisor vsock muxer
			connectHeader := fmt.Sprintf("CONNECT %d\n", vsockStreamPort)
			if _, err := conn.Write([]byte(connectHeader)); err != nil {
				conn.Close()
				return 0, nil, fmt.Errorf("failed to send CONNECT header: %w", err)
			}

			// Step 2: Read "OK <port>\n" acknowledgment from Cloud Hypervisor
			connectAckBuf := make([]byte, 32)
			conn.SetReadDeadline(time.Now().Add(connectAckTimeout))
			n, err := conn.Read(connectAckBuf)
			if err != nil {
				conn.Close()
				return 0, nil, fmt.Errorf("failed to read CONNECT acknowledgment: %w", err)
			}
			connectAck := string(connectAckBuf[:n])
			if !strings.HasPrefix(connectAck, "OK ") || !strings.HasSuffix(connectAck, "\n") {
				conn.Close()
				return 0, nil, fmt.Errorf("invalid CONNECT acknowledgment: %s", connectAck)
			}
			conn.SetReadDeadline(time.Time{}) // Clear deadline

			// Step 3: Send stream ID to vminitd (4 bytes, big-endian)
			var vs [4]byte
			binary.BigEndian.PutUint32(vs[:], sid)
			if _, err := conn.Write(vs[:]); err != nil {
				conn.Close()
				return 0, nil, fmt.Errorf("failed to write stream id: %w", err)
			}

			// Step 4: Wait for stream ID acknowledgment from vminitd
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

// Helper functions

// waitForVsockSocket waits for the vsock RPC socket to appear.
// Cloud Hypervisor creates this socket when the guest starts listening on the vsock port.
func (ch *Instance) waitForVsockSocket(ctx context.Context) error {
	log.G(ctx).WithField("socket", ch.vsockRPCPath).Info("cloud-hypervisor: waiting for guest to start listening on vsock port")

	startedAt := time.Now()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		if time.Since(startedAt) > vmStartTimeout {
			ch.logDebugInfo(ctx)
			return fmt.Errorf("timeout waiting for vsock RPC socket")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := os.Stat(ch.vsockRPCPath); err == nil {
				log.G(ctx).WithField("socket", ch.vsockRPCPath).Info("cloud-hypervisor: vsock RPC socket appeared")
				return nil
			}
		}
	}
}

// logDebugInfo logs console and hypervisor logs for debugging when VM startup fails.
func (ch *Instance) logDebugInfo(ctx context.Context) {
	log.G(ctx).Error("cloud-hypervisor: timeout waiting for VM to start")

	// Try to read VM console output
	if consoleData, err := os.ReadFile(ch.consolePath); err == nil && len(consoleData) > 0 {
		if len(consoleData) > maxLogBytes {
			consoleData = consoleData[len(consoleData)-maxLogBytes:]
		}
		log.G(ctx).WithField("console", string(consoleData)).Error("cloud-hypervisor: VM console output (last 4KB)")
	} else {
		log.G(ctx).WithFields(log.Fields{
			"console_path": ch.consolePath,
			"error":        err,
		}).Error("cloud-hypervisor: no console output (vminitd may not be starting)")
	}

	// Try to read Cloud Hypervisor log
	chLogPath := filepath.Join(ch.logDir, "cloud-hypervisor.log")
	if chLogData, err := os.ReadFile(chLogPath); err == nil && len(chLogData) > 0 {
		if len(chLogData) > maxLogBytes {
			chLogData = chLogData[len(chLogData)-maxLogBytes:]
		}
		log.G(ctx).WithField("ch_log", string(chLogData)).Error("cloud-hypervisor: hypervisor log (last 4KB)")
	}
}

// connectVsockRPC establishes a connection to the vsock RPC server (vminitd).
// It handles the Cloud Hypervisor vsock protocol handshake and TTRPC ping.
func (ch *Instance) connectVsockRPC(ctx context.Context) (net.Conn, error) {
	log.G(ctx).WithField("socket", ch.vsockRPCPath).Info("cloud-hypervisor: connecting to vsock RPC socket")

	// Wait a bit for vminitd to fully initialize
	time.Sleep(100 * time.Millisecond)

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

		conn, err := net.Dial("unix", ch.vsockRPCPath)
		if err != nil {
			log.G(ctx).WithError(err).Debug("cloud-hypervisor: failed to dial vsock socket")
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// Send CONNECT header required by Cloud Hypervisor vsock protocol
		connectHeader := fmt.Sprintf("CONNECT %d\n", vsockRPCPort)
		if _, err := conn.Write([]byte(connectHeader)); err != nil {
			log.G(ctx).WithError(err).Debug("cloud-hypervisor: failed to send CONNECT header")
			conn.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// Read the "OK <port>\n" acknowledgment from Cloud Hypervisor muxer
		ackBuf := make([]byte, 32)
		conn.SetReadDeadline(time.Now().Add(connectAckTimeout))
		n, err := conn.Read(ackBuf)
		if err != nil {
			log.G(ctx).WithError(err).Debug("cloud-hypervisor: failed to read CONNECT acknowledgment")
			conn.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		ack := string(ackBuf[:n])
		if !strings.HasPrefix(ack, "OK ") || !strings.HasSuffix(ack, "\n") {
			log.G(ctx).WithField("ack", ack).Debug("cloud-hypervisor: invalid CONNECT acknowledgment")
			conn.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		log.G(ctx).WithField("ack", strings.TrimSpace(ack)).Debug("cloud-hypervisor: received CONNECT acknowledgment")

		// Try to ping the TTRPC server with a deadline
		conn.SetReadDeadline(time.Now().Add(pingDeadline))
		if err := pingTTRPC(conn); err != nil {
			log.G(ctx).WithError(err).WithField("deadline", pingDeadline).Debug("cloud-hypervisor: TTRPC ping failed, retrying")
			conn.Close()
			pingDeadline += 10 * time.Millisecond
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// Clear the deadline and verify connection is still alive
		conn.SetReadDeadline(time.Time{})
		if err := pingTTRPC(conn); err != nil {
			log.G(ctx).WithError(err).Debug("cloud-hypervisor: TTRPC ping failed after clearing deadline, retrying")
			conn.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// Connection is ready
		log.G(ctx).WithField("retry_time", time.Since(retryStart)).Info("cloud-hypervisor: TTRPC connection established")
		return conn, nil
	}
}

func (ch *Instance) waitForAPISocket(ctx context.Context) error {
	timeout := time.After(vmStartTimeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for API socket")
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := os.Stat(ch.apiSocketPath); err == nil {
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

// pingTTRPC sends an invalid request to a TTRPC server to check
// for a response, indicating the server is alive and accepting
// requests.
func pingTTRPC(rw net.Conn) error {
	n, err := rw.Write([]byte{
		0, 0, 0, 0, // Zero length
		0, 0, 0, 0, // Zero stream ID to force rejection response (must be odd)
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
