// Package network provides platform-specific network setup for VMs.
// It handles network manager initialization and network configuration.
package network

import (
	"context"

	"github.com/spin-stack/spinbox/internal/host/network"
	"github.com/spin-stack/spinbox/internal/host/vm"
)

// Manager handles platform-specific network operations.
type Manager interface {
	// InitNetworkManager creates and initializes a NetworkManager for the platform.
	InitNetworkManager(ctx context.Context) (network.NetworkManager, error)

	// Setup configures networking for a VM instance.
	// Returns the network configuration and an error if setup fails.
	Setup(ctx context.Context, nm network.NetworkManager, vmi vm.Instance, containerID, netnsPath string) (*vm.NetworkConfig, error)
}

// New creates a platform-specific network manager.
// Returns the appropriate implementation for the current OS.
func New() Manager {
	return newManager()
}
