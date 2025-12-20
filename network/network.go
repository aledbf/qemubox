//go:build linux

package network

import (
	"context"
	"net"
	"os"
	"sync"

	"github.com/containerd/log"

	"github.com/aledbf/beacon/containerd/network/cni"
	"github.com/aledbf/beacon/containerd/store"
)

const (
	// Bridge name used by CNI (for compatibility)
	bridgeName = "beacon0"
)

// NetworkMode represents the network management mode.
type NetworkMode string

const (
	// NetworkModeCNI uses CNI plugin chains for network configuration.
	NetworkModeCNI NetworkMode = "cni"
)

type NetworkConfig struct {
	// Mode is always CNI (kept for compatibility)
	Mode NetworkMode

	// CNIConfDir is the directory containing CNI network configuration files.
	// Default: /etc/cni/net.d
	CNIConfDir string

	// CNIBinDir is the directory containing CNI plugin binaries.
	// Default: /opt/cni/bin
	CNIBinDir string

	// CNINetworkName is the name of the CNI network to use.
	// Default: beacon-net
	CNINetworkName string
}

// LoadNetworkConfig loads network configuration from environment variables.
//
// Environment variables:
//   - BEACON_CNI_CONF_DIR: CNI configuration directory (default: /etc/cni/net.d)
//   - BEACON_CNI_BIN_DIR: CNI plugin binary directory (default: /opt/cni/bin)
//   - BEACON_CNI_NETWORK: CNI network name (default: beacon-net)
//
// The subnet is determined by the CNI IPAM plugin configuration.
func LoadNetworkConfig() NetworkConfig {
	cfg := NetworkConfig{
		Mode:           NetworkModeCNI,
		CNIConfDir:     "/etc/cni/net.d",
		CNIBinDir:      "/opt/cni/bin",
		CNINetworkName: "beacon-net",
	}

	// Override CNI config directory if specified
	if confDir := os.Getenv("BEACON_CNI_CONF_DIR"); confDir != "" {
		cfg.CNIConfDir = confDir
	}

	// Override CNI bin directory if specified
	if binDir := os.Getenv("BEACON_CNI_BIN_DIR"); binDir != "" {
		cfg.CNIBinDir = binDir
	}

	// Override CNI network name if specified
	if networkName := os.Getenv("BEACON_CNI_NETWORK"); networkName != "" {
		cfg.CNINetworkName = networkName
	}

	return cfg
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
	// ID is the unique identifier (container ID or VM ID)
	ID string

	// NetworkInfo contains allocated network configuration
	// Set after EnsureNetworkResources() succeeds
	NetworkInfo *NetworkInfo
}

// NetworkManagerInterface defines the interface for network management operations
// that can be implemented by NetworkManager or mocked for testing
type NetworkManagerInterface interface {
	// Core lifecycle methods
	Close() error

	// Network resource management
	EnsureNetworkResources(env *Environment) error
	ReleaseNetworkResources(env *Environment) error
}

type NetworkManager struct {
	config             NetworkConfig
	networkConfigStore boltstore.Store[NetworkConfig]

	ctx        context.Context
	cancelFunc context.CancelFunc

	// CNI manager for network configuration
	cniManager *cni.CNIManager

	// CNI state storage (maps VM ID to CNI result for cleanup)
	cniResults map[string]*cni.CNIResult
	cniMu      sync.RWMutex
}

func NewNetworkManager(
	config NetworkConfig,
	networkConfigStore boltstore.Store[NetworkConfig],
) (NetworkManagerInterface, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// Log the network mode
	log.G(ctx).Info("Initializing CNI network manager")

	return newCNINetworkManager(ctx, cancel, config, networkConfigStore)
}

func (nm *NetworkManager) Close() error {
	if nm.cancelFunc != nil {
		nm.cancelFunc()
	}

	// CNI resources are cleaned up per-VM via ReleaseNetworkResources
	// No global cleanup needed for CNI mode
	return nil
}

// EnsureNetworkResources allocates and configures network resources for an environment using CNI.
func (nm *NetworkManager) EnsureNetworkResources(env *Environment) error {
	return nm.ensureNetworkResourcesCNI(env)
}

// ReleaseNetworkResources releases network resources for an environment using CNI.
func (nm *NetworkManager) ReleaseNetworkResources(env *Environment) error {
	return nm.releaseNetworkResourcesCNI(env)
}
