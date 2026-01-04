//go:build linux

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

	"github.com/aledbf/qemubox/containerd/internal/host/vm"
	vsockalloc "github.com/aledbf/qemubox/containerd/internal/vsock"
	"github.com/containerd/ttrpc"
)

// vmState represents the lifecycle state of a VM instance.
type vmState int32

const (
	vmStateNew      vmState = iota // Instance created, not started
	vmStateStarting                // Start() in progress
	vmStateRunning                 // VM is running
	vmStateShutdown                // Shutdown() called
)

// State management helper methods

// getState returns the current VM state
func (q *Instance) getState() vmState {
	return vmState(q.vmState.Load())
}

// setState atomically sets the VM state
func (q *Instance) setState(state vmState) {
	q.vmState.Store(uint32(state))
}

// compareAndSwapState atomically compares and swaps the VM state
func (q *Instance) compareAndSwapState(old, new vmState) bool {
	return q.vmState.CompareAndSwap(uint32(old), uint32(new))
}

const (
	defaultBootCPUs     = 1                  // Default number of boot vCPUs
	defaultMaxCPUs      = 2                  // Default maximum vCPUs (set equal to boot for lean mode)
	defaultMemorySize   = 512 * 1024 * 1024  // 512 MiB
	defaultMemoryMax    = 1024 * 1024 * 1024 // 1 GiB (reduced from 2 GiB for leaner defaults)
	vmStartTimeout      = 10 * time.Second
	connectRetryTimeout = 10 * time.Second

	// Unix socket and buffer size limits
	maxUnixSocketPath = 107             // UNIX_PATH_MAX on Linux
	consoleBufferSize = 8 * 1024        // Console FIFO read buffer
	qmpDefaultTimeout = 5 * time.Second // Default QMP command timeout
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
	guestCID    uint32            // Unique vsock CID for this VM (3+)
	cidLease    *vsockalloc.Lease // CID reservation (released on close)

	// Runtime paths
	qmpSocketPath   string   // QMP control socket
	vsockPath       string   // Vsock socket
	consolePath     string   // Persistent console log file (logDir) - receives console output from FIFO reader
	consoleFifoPath string   // Ephemeral FIFO pipe (stateDir) - QEMU writes here, prevents blocking on slow disk I/O
	qemuLogPath     string   // QEMU stderr log
	consoleFile     *os.File // Console log file handle
	consoleFifo     *os.File // FIFO reader handle (closed on shutdown to cancel console goroutine)

	// Runtime state
	cmd       *exec.Cmd
	waitCh    chan error
	qmpClient *qmpClient
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
