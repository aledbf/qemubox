//go:build linux

// Package qemu implements the QEMU-based VM backend.
//
// # State Machine
//
// The VM instance follows a strict state machine for lifecycle management:
//
//	vmStateNew → vmStateStarting → vmStateRunning → vmStateShutdown
//	    ↑              ↓
//	    └──────────────┘ (on Start failure)
//
// State transitions are atomic (using sync/atomic) and checked at API boundaries:
//   - New: Instance created, not started. AddDisk/AddNIC allowed.
//   - Starting: Start() in progress. No API calls allowed.
//   - Running: VM is running. Client/DialClient/StartStream/Shutdown allowed.
//   - Shutdown: Shutdown() called or completed. No further operations.
//
// # Goroutine Ownership
//
// The Instance owns several goroutines that must be properly tracked:
//
//  1. Console FIFO reader (setupConsoleFIFO in start.go)
//     - Lifecycle: Created in Start(), exits when FIFO is closed in Shutdown()
//     - Reads FIFO → writes to console log file
//     - Terminated by: closing q.consoleFifo in closeClientConnections()
//
//  2. Process monitor (monitorProcess in start.go)
//     - Lifecycle: Created in Start(), exits when QEMU process exits
//     - Waits on q.cmd.Wait() and signals exit via q.waitCh
//     - Terminated by: QEMU process exit (natural or SIGKILL)
//
//  3. QMP event loop (eventLoop in qmp_events.go)
//     - Lifecycle: Created in newQMPClient(), exits when monitor disconnected
//     - Processes asynchronous QMP events (shutdown, panic, etc.)
//     - Terminated by: qmpClient.Close() which disconnects the monitor
//
//  4. Guest RPC monitor (monitorGuestRPC in client.go)
//     - Lifecycle: Created at end of Start(), exits when runCtx is cancelled
//     - Monitors guest vsock RPC health
//     - Terminated by: q.runCancel() in cancelBackgroundMonitors()
//
// # Context Usage
//
// The Instance uses multiple context types:
//   - Caller context: Passed to Start()/Shutdown(), may be cancelled by RPC layer
//   - context.WithoutCancel: Used for QEMU process and QMP to outlive RPC calls
//   - runCtx: Long-lived context for background monitors (cancelled in Shutdown)
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
// See package documentation for the state machine diagram.
type vmState int32

const (
	// vmStateNew: Instance created but Start() not called.
	// Allowed operations: AddDisk(), AddNIC(), AddTAPNIC(), Start()
	vmStateNew vmState = iota

	// vmStateStarting: Start() is executing.
	// No operations allowed (will fail with state error).
	vmStateStarting

	// vmStateRunning: VM is fully initialized and running.
	// Allowed operations: Client(), DialClient(), StartStream(), Shutdown()
	vmStateRunning

	// vmStateShutdown: Shutdown() was called or VM exited.
	// No operations allowed. Terminal state.
	vmStateShutdown
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
//
// Thread safety:
//   - State transitions use atomic operations (vmState)
//   - mu protects mutable fields during concurrent access
//   - Background goroutines coordinate via channels and context cancellation
//
// See package documentation for state machine and goroutine ownership details.
type Instance struct {
	// mu protects fields accessed concurrently:
	// - disks, nets: Written during configuration, read during Start()
	// - cmd, qmpClient, client, vsockConn: Written during Start(), read during operation
	mu sync.Mutex

	// vmState tracks lifecycle (see vmState constants).
	// Accessed atomically, no mutex needed.
	vmState atomic.Uint32

	// streamC is a monotonically increasing stream ID counter.
	// Accessed atomically for unique stream IDs.
	streamC uint32

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
