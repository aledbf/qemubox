// Package network provides host networking orchestration.
package network

import (
	"context"
	"net"
)

// NetworkConfig describes the CNI configuration locations.
type NetworkConfig struct {
	// CNIConfDir is the directory containing CNI network configuration files.
	// Default: /etc/cni/net.d
	CNIConfDir string

	// CNIBinDir is the directory containing CNI plugin binaries.
	// Default: /opt/cni/bin
	CNIBinDir string
}

// NetworkInfo holds internal network configuration
type NetworkInfo struct {
	TapName string `json:"tap_name"`
	MAC     string `json:"mac"`
	IP      net.IP `json:"ip"`
	Netmask string `json:"netmask"`
	Gateway net.IP `json:"gateway"`
}

// Environment represents a VM/container network environment
type Environment struct {
	// ID is the unique identifier (container ID or VM ID)
	ID string

	// NetworkInfo contains allocated network configuration
	// Set after EnsureNetworkResources() succeeds
	NetworkInfo *NetworkInfo
}

// NetworkManager defines the interface for network management operations
type NetworkManager interface {
	// Close stops the network manager and releases internal resources
	Close() error

	// EnsureNetworkResources allocates and configures network resources for an environment
	EnsureNetworkResources(ctx context.Context, env *Environment) error

	// ReleaseNetworkResources releases network resources for an environment
	ReleaseNetworkResources(ctx context.Context, env *Environment) error

	// Metrics returns the CNI operation metrics for this manager instance
	Metrics() *Metrics
}
