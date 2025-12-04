package cloudhypervisor

import (
	"net"
	"os/exec"
	"sync"
	"sync/atomic"

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

// Instance represents a Cloud Hypervisor VM instance.
type Instance struct {
	// mu protects fields that are read/written concurrently during configuration
	// and startup: disks, filesystems, nets, cmd, api, client, vsockConn.
	// State transitions are managed via vmState atomic.
	mu            sync.Mutex
	vmState       atomic.Uint32 // Current VM state (vmState)
	binaryPath    string
	stateDir      string // State directory path for sockets and other runtime files
	logDir        string // Log directory path for persistent logs
	kernelPath    string
	initrdPath    string
	apiSocketPath string
	vsockRPCPath  string
	streamPath    string
	consolePath   string

	cmd       *exec.Cmd
	api       *CloudHypervisorClient
	client    *ttrpc.Client
	vsockConn net.Conn // Connection to vsock RPC server
	streamC   uint32

	// Configuration collected before Start()
	disks       []*DiskConfig
	filesystems []*FsConfig
	nets        []*NetConfig
	networkCfg  *vm.NetworkConfig // CNI network configuration (IP, gateway, netmask)
	resourceCfg *VMResourceConfig // CPU and memory resource configuration
}

// VMResourceConfig holds the CPU and memory configuration for a VM instance
type VMResourceConfig struct {
	// BootCPUs is the number of vCPUs to boot with (minimum)
	BootCPUs int
	// MaxCPUs is the maximum number of vCPUs that can be hotplugged
	MaxCPUs int
	// MemorySize is the initial memory size in bytes
	MemorySize int64
	// MemoryHotplugSize is the maximum memory size in bytes (for hotplug)
	MemoryHotplugSize int64
}
