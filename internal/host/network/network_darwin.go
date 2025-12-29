//go:build darwin

// Package network provides CNI-based network management for qemubox VMs.
// On Darwin, all network operations return errors as networking is not supported.
package network

import (
	"context"
	"fmt"
	"net"

	boltstore "github.com/aledbf/qemubox/containerd/internal/host/store"
)

// NetworkConfig defines network configuration
type NetworkConfig struct {
	// CNI fields (not used on Darwin)
	CNIConfDir string
	CNIBinDir  string
}

// LoadNetworkConfig loads network configuration.
// On Darwin, returns stub config (networking is not supported).
func LoadNetworkConfig() NetworkConfig {
	return NetworkConfig{
		CNIConfDir: "/etc/cni/net.d",
		CNIBinDir:  "/opt/cni/bin",
	}
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
	ID          string
	NetworkInfo *NetworkInfo
}

// NetworkManager defines the interface for network management operations
type NetworkManager interface {
	Close() error
	EnsureNetworkResources(ctx context.Context, env *Environment) error
	ReleaseNetworkResources(ctx context.Context, env *Environment) error
}

// darwinNetworkManager stub for Darwin
type darwinNetworkManager struct{}

// NewNetworkManager creates a stub network manager (Darwin only)
func NewNetworkManager(
	ctx context.Context,
	config NetworkConfig,
	networkConfigStore boltstore.Store[NetworkConfig],
) (NetworkManager, error) {
	// Reference unused parameter to avoid compiler errors
	_ = ctx
	_ = config
	_ = networkConfigStore
	return nil, fmt.Errorf("network manager not supported on darwin")
}

// Close is a stub for Darwin
func (nm *darwinNetworkManager) Close() error {
	return fmt.Errorf("not supported on darwin")
}

// EnsureNetworkResources is a stub for Darwin
func (nm *darwinNetworkManager) EnsureNetworkResources(ctx context.Context, env *Environment) error {
	return fmt.Errorf("not supported on darwin")
}

// ReleaseNetworkResources is a stub for Darwin
func (nm *darwinNetworkManager) ReleaseNetworkResources(ctx context.Context, env *Environment) error {
	return fmt.Errorf("not supported on darwin")
}
