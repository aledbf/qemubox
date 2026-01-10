// Package vsock provides centralized vsock port and CID constants.
//
// These constants are used by both the host (shim/QEMU) and guest (vminitd)
// components to establish communication over virtio-vsock.
package vsock

const (
	// GuestCID is the guest context ID for vsock communication.
	// CID 0 is reserved for the hypervisor.
	// CID 1 is reserved for the host.
	// CID 2 is the host CID in standard vsock setups (often the QEMU side).
	// CID 3 is assigned to the guest VM by spinbox.
	GuestCID = 3

	// DefaultRPCPort is the vsock port for TTRPC RPC communication
	// between the shim (host) and vminitd (guest).
	DefaultRPCPort = 1025

	// DefaultStreamPort is the vsock port for streaming I/O
	// (stdin/stdout/stderr) between host and guest.
	DefaultStreamPort = 1026
)
