// Package vm defines shared types for VM implementations.
// Concrete VM implementations are in subpackages (e.g., cloudhypervisor).
package vm

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
}

type MountOpt func(*MountConfig)

func WithReadOnly() MountOpt {
	return func(o *MountConfig) {
		o.Readonly = true
	}
}

// VMInfo contains metadata about the VMM backend
type VMInfo struct {
	// Type identifies the VMM backend (e.g., "libkrun", "cloudhypervisor")
	Type string

	// SupportsTAP indicates whether the VMM supports TAP device networking
	SupportsTAP bool

	// SupportsVSOCK indicates whether the VMM supports vsock for communication
	SupportsVSOCK bool
}
