// Package vm defines shared types for VM implementations.
// Concrete VM implementations are in subpackages (e.g., qemu).
package vm

import (
	"context"
	"net"

	"github.com/containerd/ttrpc"
)

type NetworkMode int

const (
	NetworkModeUnixgram NetworkMode = iota
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

type StartOpt func(*StartOpts)

func WithInitArgs(args ...string) StartOpt {
	return func(o *StartOpts) {
		o.InitArgs = append(o.InitArgs, args...)
	}
}

func WithNetworkConfig(cfg *NetworkConfig) StartOpt {
	return func(o *StartOpts) {
		o.NetworkConfig = cfg
	}
}

func WithNetworkNamespace(path string) StartOpt {
	return func(o *StartOpts) {
		o.NetworkNamespace = path
	}
}

type MountConfig struct {
	Readonly bool
	Vmdk     bool
}

type MountOpt func(*MountConfig)

func WithReadOnly() MountOpt {
	return func(o *MountConfig) {
		o.Readonly = true
	}
}

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

// Instance represents a VM instance that can run containers.
// This interface abstracts the VMM backend (QEMU)
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
	Client() *ttrpc.Client
	StartStream(ctx context.Context) (uint32, net.Conn, error)

	// Metadata
	VMInfo() VMInfo
}
