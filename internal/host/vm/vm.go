// Package vm defines shared types for VM implementations.
// Concrete VM implementations are in subpackages (e.g., qemu).
package vm

import (
	"context"
	"net"

	"github.com/containerd/ttrpc"
)

// NetworkMode describes how the VM networking is wired.
type NetworkMode int

const (
	// NetworkModeUnixgram uses unixgram for VM networking.
	NetworkModeUnixgram NetworkMode = iota
	// NetworkModeUnixstream uses unixstream for VM networking.
	NetworkModeUnixstream
)

// NetworkConfig holds the network settings to be applied to the VM.
type NetworkConfig struct {
	InterfaceName string   // Interface name in VM (e.g., "eth0")
	IP            string   // IPv4 address (e.g., "10.88.0.5")
	Gateway       string   // Gateway IP (e.g., "10.88.0.1")
	Netmask       string   // Netmask (e.g., "255.255.255.0")
	DNS           []string // DNS servers
}

// VMResourceConfig defines VM resource limits (shared across all VMM backends).
type VMResourceConfig struct {
	BootCPUs          int   // Initial vCPUs (default: 1)
	MaxCPUs           int   // Max vCPUs for hotplug (default: 2)
	MemorySize        int64 // Initial memory in bytes (default: 512 MiB)
	MemoryHotplugSize int64 // Max memory for hotplug in bytes (default: 2 GiB)
}

// StartOpts defines configuration options for starting a VM.
type StartOpts struct {
	InitArgs         []string
	NetworkConfig    *NetworkConfig
	NetworkNamespace string // Path to network namespace (e.g., "/var/run/netns/cni-xxx")
}

// StartOpt configures VM start options.
type StartOpt func(*StartOpts)

// WithInitArgs sets init arguments for the VM.
func WithInitArgs(args ...string) StartOpt {
	return func(o *StartOpts) {
		o.InitArgs = append(o.InitArgs, args...)
	}
}

// WithNetworkConfig sets the network configuration for the VM.
func WithNetworkConfig(cfg *NetworkConfig) StartOpt {
	return func(o *StartOpts) {
		o.NetworkConfig = cfg
	}
}

// WithNetworkNamespace sets the network namespace path for the VM.
func WithNetworkNamespace(path string) StartOpt {
	return func(o *StartOpts) {
		o.NetworkNamespace = path
	}
}

// MountConfig defines configuration for mounting disks into the VM.
type MountConfig struct {
	Readonly bool
	Vmdk     bool
}

// MountOpt configures mount options.
type MountOpt func(*MountConfig)

// WithReadOnly mounts the disk read-only.
func WithReadOnly() MountOpt {
	return func(o *MountConfig) {
		o.Readonly = true
	}
}

// WithVmdk mounts the disk using VMDK format.
func WithVmdk() MountOpt {
	return func(o *MountConfig) {
		o.Vmdk = true
	}
}

// VMInfo contains metadata about the VMM backend
type VMInfo struct {
	// Type identifies the VMM backend (e.g., "qemu")
	Type string

	// SupportsTAP indicates whether the VMM supports TAP device networking
	SupportsTAP bool

	// SupportsVSOCK indicates whether the VMM supports vsock for communication
	SupportsVSOCK bool
}

// CPUHotplugger provides CPU hotplug operations.
type CPUHotplugger interface {
	QueryCPUs(ctx context.Context) ([]CPUInfo, error)
	HotplugCPU(ctx context.Context, cpuID int) error
	UnplugCPU(ctx context.Context, cpuID int) error
}

// CPUInfo represents information about a single vCPU.
type CPUInfo struct {
	CPUIndex int    `json:"cpu-index"`
	QOMPath  string `json:"qom-path"`
	Thread   int    `json:"thread-id"`
	Target   string `json:"target"`
}

// Instance represents a VM instance that can run containers.
// This interface abstracts the VMM backend (QEMU)
//
//nolint:interfacebloat // VM interface naturally has many methods for different operations
type Instance interface {
	// Device configuration (called before Start)
	AddDisk(ctx context.Context, blockID, mountPath string, opts ...MountOpt) error
	AddTAPNIC(ctx context.Context, tapName string, mac net.HardwareAddr) error
	AddFS(ctx context.Context, tag, mountPath string, opts ...MountOpt) error
	AddNIC(ctx context.Context, endpoint string, mac net.HardwareAddr, mode NetworkMode, features, flags uint32) error

	// Lifecycle management
	Start(ctx context.Context, opts ...StartOpt) error
	Shutdown(ctx context.Context) error

	// Communication with guest
	Client() (*ttrpc.Client, error)
	// DialClient creates a new, short-lived TTRPC client connection to the guest.
	// Callers must close the returned client when done.
	DialClient(ctx context.Context) (*ttrpc.Client, error)
	StartStream(ctx context.Context) (uint32, net.Conn, error)

	// Resource management
	CPUHotplugger() (CPUHotplugger, error)

	// Metadata
	VMInfo() VMInfo
}
