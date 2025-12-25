// Package qemu implements the QEMU-based VM backend.
package qemu

import (
	"context"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aledbf/qemubox/containerd/vm"
	"github.com/containerd/ttrpc"
)

// vmState represents the lifecycle state of a VM instance.
type vmState uint32

const (
	vmStateNew      vmState = iota // Instance created, not started
	vmStateStarting                // Start() in progress
	vmStateRunning                 // VM is running
	vmStateShutdown                // Shutdown() called
)

const (
	vsockCID            = 3                  // Guest context ID for vsock
	vsockRPCPort        = 1025               // Port for TTRPC RPC communication
	vsockStreamPort     = 1026               // Port for streaming I/O
	defaultBootCPUs     = 1                  // Default number of boot vCPUs
	defaultMaxCPUs      = 2                  // Default maximum vCPUs (set equal to boot for lean mode)
	defaultMemorySize   = 512 * 1024 * 1024  // 512 MiB
	defaultMemoryMax    = 1024 * 1024 * 1024 // 1 GiB (reduced from 2 GiB for leaner defaults)
	vmStartTimeout      = 10 * time.Second
	connectRetryTimeout = 10 * time.Second
)

// Instance represents a QEMU microvm instance.
type Instance struct {
	// mu protects fields that are read/written concurrently during configuration
	// and startup: disks, nets, cmd, qmpClient, client, vsockConn.
	// State transitions are managed via vmState atomic.
	mu      sync.Mutex
	vmState atomic.Uint32 // Current VM state (vmState)
	streamC uint32        // Stream ID counter

	// Configuration
	binaryPath  string
	stateDir    string
	logDir      string
	kernelPath  string
	initrdPath  string
	resourceCfg *vm.VMResourceConfig

	// Runtime paths
	qmpSocketPath   string   // QMP control socket
	vsockPath       string   // Vsock socket
	consolePath     string   // Persistent console log file (logDir) - receives console output from FIFO reader
	consoleFifoPath string   // Ephemeral FIFO pipe (stateDir) - QEMU writes here, prevents blocking on slow disk I/O
	qemuLogPath     string   // QEMU stderr log
	consoleFile     *os.File // Console log file handle

	// Runtime state
	cmd       *exec.Cmd
	waitCh    chan error
	qmpClient *QMPClient
	client    *ttrpc.Client
	vsockConn net.Conn

	// Long-lived context for background monitors started after the VM boots.
	// This is a valid exception to the "no context in struct" rule because:
	// 1. The context represents the VM instance's lifetime, not a single operation
	// 2. The struct manages both the context and its cancellation
	// 3. Multiple background goroutines share this context for coordinated shutdown
	runCtx    context.Context //nolint:containedctx // Manages VM lifetime for background monitors
	runCancel context.CancelFunc

	// Netns where TAPs were originally created (CNI); used to move them back on shutdown.
	tapNetns string

	// Device tracking (configured before Start)
	disks      []*DiskConfig
	nets       []*NetConfig
	networkCfg *vm.NetworkConfig // CNI network configuration
}

// DiskConfig represents a virtio-blk device
type DiskConfig struct {
	ID       string
	Path     string
	Readonly bool
}

// NetConfig represents a virtio-net device
type NetConfig struct {
	ID      string
	TapName string   // TAP device name (stays in sandbox netns)
	TapFile *os.File // TAP device file descriptor (opened in sandbox netns)
	MAC     string
}
