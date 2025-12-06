package qemu

import (
	"net"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aledbf/beacon/containerd/vm"
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
	vsockCID            = 3      // Guest context ID for vsock
	vsockRPCPort        = 1025   // Port for TTRPC RPC communication
	vsockStreamPort     = 1026   // Port for streaming I/O
	defaultBootCPUs     = 1      // Default number of boot vCPUs
	defaultMaxCPUs      = 2      // Default maximum vCPUs
	defaultMemorySize   = 512 * 1024 * 1024      // 512 MiB
	defaultMemoryMax    = 2 * 1024 * 1024 * 1024 // 2 GiB
	vmStartTimeout      = 10 * time.Second
	connectRetryTimeout = 10 * time.Second
	maxLogBytes         = 4096 // Maximum log output to include in errors
)

// Instance represents a QEMU microvm instance.
type Instance struct {
	// mu protects fields that are read/written concurrently during configuration
	// and startup: disks, nets, cmd, qmpClient, client, vsockConn.
	// State transitions are managed via vmState atomic.
	mu        sync.Mutex
	vmState   atomic.Uint32 // Current VM state (vmState)
	streamC   uint32        // Stream ID counter

	// Configuration
	binaryPath    string
	stateDir      string
	logDir        string
	kernelPath    string
	initrdPath    string
	qemuSharePath string // Path to QEMU share directory (BIOS files)
	resourceCfg   *vm.VMResourceConfig

	// Runtime paths
	qmpSocketPath string // QMP control socket
	vsockPath     string // Vsock socket
	consolePath   string // Console log
	qemuLogPath   string // QEMU stderr log

	// Runtime state
	cmd       *exec.Cmd
	qmpClient *QMPClient
	client    *ttrpc.Client
	vsockConn net.Conn

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
	TapName string
	MAC     string
}
