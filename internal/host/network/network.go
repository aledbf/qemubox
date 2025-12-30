//go:build linux

// Package network provides host networking orchestration.
package network

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/containerd/log"

	"github.com/aledbf/qemubox/containerd/internal/host/network/cni"
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

// LoadNetworkConfig returns the standard CNI network configuration.
//
// Uses standard CNI paths:
//   - CNI config directory: /etc/cni/net.d (configs loaded lexicographically)
//   - CNI plugin binary directory: /opt/cni/bin
//
// Network configuration is auto-discovered from the first .conflist file
// in the CNI config directory (sorted alphabetically by filename).
func LoadNetworkConfig() NetworkConfig {
	if dir := os.Getenv("QEMUBOX_CNI_CONF_DIR"); dir != "" {
		return NetworkConfig{
			CNIConfDir: dir,
			CNIBinDir:  os.Getenv("QEMUBOX_CNI_BIN_DIR"),
		}
	}

	qemuboxConfDir := filepath.Join("/usr/share/qemubox", "config", "cni", "net.d")
	qemuboxBinDir := filepath.Join("/usr/share/qemubox", "libexec", "cni")
	if _, err := os.Stat(qemuboxConfDir); err == nil {
		return NetworkConfig{
			CNIConfDir: qemuboxConfDir,
			CNIBinDir:  qemuboxBinDir,
		}
	}

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
}

// setupInFlight tracks an in-progress CNI setup operation.
// Multiple goroutines attempting to setup the same container ID will coordinate
// through this struct - the first one does the work, others wait on the channel.
type setupInFlight struct {
	done   chan struct{} // closed when setup completes (success or failure)
	result *cni.CNIResult
	err    error
}

// cniNetworkManager manages lifecycle of host networking resources using CNI.
type cniNetworkManager struct {
	config NetworkConfig

	// CNI manager for network configuration
	cniManager *cni.CNIManager

	// CNI state storage (maps VM ID to CNI result for cleanup)
	cniResults map[string]*cni.CNIResult
	cniMu      sync.RWMutex

	// Tracks in-flight setup operations to avoid duplicate work
	// Multiple concurrent calls for the same ID will coordinate through this map
	inFlight   map[string]*setupInFlight
	inflightMu sync.Mutex
}

// NewNetworkManager creates a network manager for the configured mode.
func NewNetworkManager(
	ctx context.Context,
	config NetworkConfig,
) (NetworkManager, error) {
	// Log the network mode
	log.G(ctx).Info("Initializing CNI network manager")

	return newCNINetworkManager(config)
}

// Close stops the network manager and releases internal resources.
func (nm *cniNetworkManager) Close() error {
	// CNI resources are cleaned up per-VM via ReleaseNetworkResources
	// No global cleanup needed for CNI mode
	return nil
}

// EnsureNetworkResources allocates and configures network resources for an environment using CNI.
func (nm *cniNetworkManager) EnsureNetworkResources(ctx context.Context, env *Environment) error {
	return nm.ensureNetworkResourcesCNI(ctx, env)
}

// ReleaseNetworkResources releases network resources for an environment using CNI.
func (nm *cniNetworkManager) ReleaseNetworkResources(ctx context.Context, env *Environment) error {
	return nm.releaseNetworkResourcesCNI(ctx, env)
}
