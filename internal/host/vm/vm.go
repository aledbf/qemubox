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
	MemorySlots       int   // Memory hotplug slots (default: 8, must match VMM config)
}

// StartOpts defines configuration options for starting a VM.
type StartOpts struct {
	InitArgs         []string
	NetworkConfig    *NetworkConfig
	NetworkNamespace string // Path to network namespace (e.g., "/var/run/netns/cni-xxx")
	ExtrasDiskIndex  *int   // Index of extras disk (0-based), nil if none
	DebugBoot        bool   // Enable kernel boot profiling (initcall_debug + verbose)
	// NoNetwork says this VM is meant to have no network interface, as against
	// having lost one to a bug. See WithoutNetwork.
	NoNetwork bool
}

// StartOpt configures VM start options.
type StartOpt func(*StartOpts)

// WithInitArgs sets init arguments for the VM.
func WithInitArgs(args ...string) StartOpt {
	return func(o *StartOpts) {
		o.InitArgs = append(o.InitArgs, args...)
	}
}

// WithDebugBoot enables kernel boot profiling for this VM (initcall_debug and
// verbose kernel output to the console log).
func WithDebugBoot(enabled bool) StartOpt {
	return func(o *StartOpts) {
		o.DebugBoot = enabled
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

// WithoutNetwork says this VM is meant to have no network interface.
//
// Start otherwise refuses a VM with no NIC, and the refusal is worth keeping: it
// catches a caller that forgot AddNetwork, which used to be unrecoverable
// because the guest read its address off the kernel command line. It is not
// unrecoverable any more — the guest skips networking when it is given no
// address, which is what the VM a template is made from has always relied on.
//
// So the rule is not "every VM has a NIC", it is "a VM without one said so".
// Stated by the caller rather than derived, because the two situations it
// separates — a workspace that wants no network, and a shim that dropped one —
// look identical from in here.
func WithoutNetwork() StartOpt {
	return func(o *StartOpts) {
		o.NoNetwork = true
	}
}

// WithExtrasDisk sets the index of the extras disk for kernel cmdline.
// The guest will parse spin.extras_disk=N to find the device.
func WithExtrasDisk(idx int) StartOpt {
	return func(o *StartOpts) {
		o.ExtrasDiskIndex = &idx
	}
}

// MountConfig defines configuration for mounting disks into the VM.
type MountConfig struct {
	Readonly bool
	// Format is what QEMU is told the image is: raw, vmdk, qcow2.
	//
	// Stated by whoever adds the disk, because that is the only party that knows.
	// It used to be worked out from the file extension while the command line was
	// being built — a guess, in the one place nobody looks, made by code that had
	// just been handed the answer and dropped it. A wrong format is not an error:
	// it is a guest that boots and finds a disk full of nothing, and letting QEMU
	// probe the format of a file the guest can write is how an image is talked
	// into being read as another one.
	//
	// Empty means DefaultDiskFormat.
	Format string
	// Pointer says the path is a file naming the image, not the image. See
	// FromPointer.
	Pointer bool
	// Serial is the virtio-blk serial exposed to the guest (max 20 chars).
	// The guest resolves the device by matching this serial, so the
	// layer→device mapping does not depend on PCI enumeration order.
	Serial string
}

// MountOpt configures mount options.
type MountOpt func(*MountConfig)

// WithReadOnly mounts the disk read-only.
func WithReadOnly() MountOpt {
	return func(o *MountConfig) {
		o.Readonly = true
	}
}

// DefaultDiskFormat is what a disk is when nobody says otherwise.
//
// raw, because that is what the snapshotter's rwlayer and the extras disk are.
// It becomes qcow2 when the disks stop coming from a snapshotter and start
// coming from a layer chain, and this constant is the whole of that change.
const DefaultDiskFormat = "raw"

// FromPointer says the path this disk was added with is not an image but a file
// naming one, to be read when the VM is launched rather than now.
//
// It is how a disk backed by a qcow2 chain is attached: the chain's tip can be
// replaced while the guest runs, and the pointer is where the replacement is
// announced. Without this the launcher would open the pointer file itself as if
// it were a disk, which is what QEMU said when this was first wired up — it
// exited before the monitor came up, with nothing about pointers in the message.
func FromPointer() MountOpt {
	return func(o *MountConfig) {
		o.Pointer = true
	}
}

// WithFormat states the image format QEMU is given for this disk.
func WithFormat(format string) MountOpt {
	return func(o *MountConfig) {
		o.Format = format
	}
}

// WithSerial sets the virtio-blk serial used by the guest to identify the device.
func WithSerial(serial string) MountOpt {
	return func(o *MountConfig) {
		o.Serial = serial
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

	// CID is the vsock context ID assigned to this VM (0 if not applicable)
	CID uint32
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

// DeviceConfigurator configures VM devices before startup.
// These methods must be called before Start().
type DeviceConfigurator interface {
	// AddDisk adds a virtio-blk disk device to the VM.
	AddDisk(ctx context.Context, blockID, mountPath string, opts ...MountOpt) error
	// AddTAPNIC adds a TAP-based network interface to the VM.
	AddTAPNIC(ctx context.Context, tapName string, mac net.HardwareAddr) error
	// AddNIC adds a network interface with the specified configuration.
	AddNIC(ctx context.Context, endpoint string, mac net.HardwareAddr, mode NetworkMode, features, flags uint32) error
	// DiskCount returns the number of disks currently configured.
	DiskCount() int
}

// GuestCommunicator provides communication channels with the guest VM.
type GuestCommunicator interface {
	// Client returns the shared TTRPC client for guest communication.
	Client() (*ttrpc.Client, error)
	// DialClient creates a new, short-lived TTRPC client connection to the guest.
	// Callers must close the returned client when done.
	DialClient(ctx context.Context) (*ttrpc.Client, error)
	// StartStream creates a new bidirectional stream for I/O forwarding.
	StartStream(ctx context.Context) (uint32, net.Conn, error)
}

// ResourceManager provides dynamic resource management for the VM.
type ResourceManager interface {
	// CPUHotplugger returns an interface for CPU hotplug operations.
	CPUHotplugger() (CPUHotplugger, error)
}

// Instance represents a VM instance that can run containers.
// This interface abstracts the VMM backend (QEMU) and composes
// focused interfaces for different aspects of VM management.
//
// The interface is organized into logical groups:
//   - DeviceConfigurator: Configure devices before Start()
//   - Lifecycle: Start() and Shutdown()
//   - GuestCommunicator: Communicate with the running guest
//   - ResourceManager: Dynamic resource management
//   - Metadata: VM information
type Instance interface {
	DeviceConfigurator
	GuestCommunicator
	ResourceManager

	// Lifecycle management
	Start(ctx context.Context, opts ...StartOpt) error
	Shutdown(ctx context.Context) error

	// Pause suspends VM execution (all vCPUs); the VM must be running.
	Pause(ctx context.Context) error
	// Resume restarts a paused VM (all vCPUs).
	Resume(ctx context.Context) error

	// Metadata
	VMInfo() VMInfo
}

// Restorer is implemented by VMM backends that can start a VM from a template
// instead of booting it: a guest frozen once, and resumed by every later VM.
//
// It is a separate interface rather than part of Instance because restoring is
// not something every backend can do, and a caller that finds a backend does not
// implement it should boot rather than fail. Both methods must be called before
// Start.
type Restorer interface {
	// UseMemoryFile backs guest RAM with a file, which is what lets a template
	// leave memory out of its state and be mapped copy-on-write by many VMs.
	UseMemoryFile(path string) error

	// RestoreFrom starts this VM from the template's state instead of booting a
	// kernel. The instance must be given the matching memory file too.
	RestoreFrom(statePath string) error
}
