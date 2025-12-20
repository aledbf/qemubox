//go:build darwin

// Package network provides CNI-based network management for beacon VMs.
// On Darwin, all network operations return errors as networking is not supported.
package network

import (
	"fmt"
	"net"

	boltstore "github.com/aledbf/beacon/containerd/store"
)

// NetworkMode represents the network management mode.
type NetworkMode string

const (
	// NetworkModeCNI uses CNI plugin chains (not supported on Darwin).
	NetworkModeCNI NetworkMode = "cni"
)

// NetworkConfig defines network configuration
type NetworkConfig struct {
	Mode NetworkMode

	// CNI fields (not used on Darwin)
	CNIConfDir     string
	CNIBinDir      string
	CNINetworkName string
}

// LoadNetworkConfig loads network configuration.
// On Darwin, returns CNI mode config (though CNI is not supported).
func LoadNetworkConfig() NetworkConfig {
	return NetworkConfig{
		Mode:           NetworkModeCNI,
		CNIConfDir:     "/etc/cni/net.d",
		CNIBinDir:      "/opt/cni/bin",
		CNINetworkName: "beacon-net",
	}
}

// NetworkInfo holds internal network configuration
type NetworkInfo struct {
	TapName    string `json:"tap_name"`
	BridgeName string `json:"bridge_name"`
	MAC        string `json:"mac"`
	IP         net.IP `json:"ip"`
	Netmask    string `json:"netmask"`
	Gateway    net.IP `json:"gateway"`
}

// Environment represents a VM/container network environment
type Environment struct {
	ID          string
	NetworkInfo *NetworkInfo
}

// NetworkManagerInterface defines the interface for network management operations
type NetworkManagerInterface interface {
	Close() error
	EnsureNetworkResources(env *Environment) error
	ReleaseNetworkResources(env *Environment) error
}

// NetworkManager stub for Darwin
type NetworkManager struct{}

// NewNetworkManager creates a stub network manager (Darwin only)
func NewNetworkManager(
	config NetworkConfig,
	networkConfigStore boltstore.Store[NetworkConfig],
) (NetworkManagerInterface, error) {
	// Reference unused parameter to avoid compiler errors
	_ = config
	_ = networkConfigStore
	return nil, fmt.Errorf("network manager not supported on darwin")
}

// Close is a stub for Darwin
func (nm *NetworkManager) Close() error {
	return fmt.Errorf("not supported on darwin")
}

// EnsureNetworkResources is a stub for Darwin
func (nm *NetworkManager) EnsureNetworkResources(env *Environment) error {
	return fmt.Errorf("not supported on darwin")
}

// ReleaseNetworkResources is a stub for Darwin
func (nm *NetworkManager) ReleaseNetworkResources(env *Environment) error {
	return fmt.Errorf("not supported on darwin")
}
