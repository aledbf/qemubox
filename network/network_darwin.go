//go:build darwin

package network

import (
	"fmt"
	"net"

	"github.com/aledbf/beacon/containerd/network/ipallocator"
	boltstore "github.com/aledbf/beacon/containerd/store"
)

// NetworkConfig defines network configuration
type NetworkConfig struct {
	Subnet string
}

// NetworkInfo holds internal network configuration
type NetworkInfo struct {
	TapName    string `json:"tap_name"`
	BridgeName string `json:"bridge_name"`
	IP         net.IP `json:"ip"`
	Netmask    string `json:"netmask"`
	Gateway    net.IP `json:"gateway"`
}

// Environment represents a VM/container network environment
type Environment struct {
	Id          string
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
	ipStore boltstore.Store[ipallocator.IPAllocation],
	moduleChecker ModuleChecker,
	netOp NetworkOperator,
	nftOp NFTablesOperator,
	onPolicyChange func(policyChangeType),
) (NetworkManagerInterface, error) {
	// Reference unused parameters to avoid compiler errors
	_ = networkConfigStore
	_ = ipStore
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

// ModuleChecker is a function type that checks for loaded kernel modules
type ModuleChecker func() ([]string, error)

// DefaultModuleChecker is a stub for Darwin
func DefaultModuleChecker() ([]string, error) {
	return nil, fmt.Errorf("not supported on darwin")
}

type policyChangeType int
