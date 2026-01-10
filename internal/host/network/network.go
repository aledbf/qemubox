//go:build linux

// Package network provides host networking orchestration.
//
// # Synchronization Model
//
// The cniNetworkManager coordinates CNI network setup across multiple concurrent
// container creation requests. Thread safety is achieved through two separate
// synchronization mechanisms:
//
// 1. Result Cache (cniResults map, protected by cniMu RWMutex):
//   - Stores completed CNI setup results for cleanup
//   - Read lock for checking if already configured (fast path)
//   - Write lock only when storing/removing results
//
// 2. In-Flight Coordination (inFlight map, protected by inflightMu Mutex):
//   - Prevents duplicate CNI setup for the same container ID
//   - First caller becomes the "worker" and performs setup
//   - Subsequent callers block on a channel until worker completes
//   - Worker shares result/error with all waiters via the channel
//
// Locking Order (when holding multiple locks):
//  1. inflightMu (always acquire first if needed)
//  2. cniMu (acquire second)
//     Never hold inflightMu during CNI operations (expensive I/O)
//
// Goroutine Ownership:
//   - Each ensureNetworkResourcesCNI call runs in caller's goroutine
//   - The first caller for a container ID owns the CNI setup operation
//   - Other concurrent callers become waiters (block on done channel)
//   - No background goroutines - all operations are synchronous
//
// Resource Lifecycle:
//   - Network namespace: Created during setup, deleted during teardown
//   - TAP device: Created by CNI plugins, destroyed during teardown
//   - IP allocation: Managed by CNI IPAM plugin, released during teardown
//   - cniResults entry: Stored after successful setup, removed during teardown
//   - inFlight entry: Created when setup starts, removed when setup completes
package network

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/containerd/log"

	"github.com/spin-stack/spinbox/internal/host/network/cni"
)

// LoadNetworkConfig loads CNI network configuration using a three-tier fallback:
//  1. Environment variables (SPINBOX_CNI_CONF_DIR, SPINBOX_CNI_BIN_DIR)
//  2. Spinbox-bundled CNI config (if exists)
//  3. Standard system CNI paths (/etc/cni/net.d, /opt/cni/bin)
//
// Network configuration is auto-discovered from the first .conflist file
// in the CNI config directory (sorted alphabetically by filename).
func LoadNetworkConfig() NetworkConfig {
	// Priority 1: Environment variable override (user-specified paths)
	// Allows users to override CNI config location without changing code
	if confDir := os.Getenv("SPINBOX_CNI_CONF_DIR"); confDir != "" {
		binDir := os.Getenv("SPINBOX_CNI_BIN_DIR")
		if binDir == "" {
			// If only config dir is overridden, use standard bin dir
			binDir = "/opt/cni/bin"
		}
		return NetworkConfig{
			CNIConfDir: confDir,
			CNIBinDir:  binDir,
		}
	}

	// Priority 2: Spinbox-bundled CNI paths (if they exist)
	// Used when spinbox is installed with its own CNI plugins
	spinboxConfDir := filepath.Join("/usr/share/spinbox", "config", "cni", "net.d")
	spinboxBinDir := filepath.Join("/usr/share/spinbox", "libexec", "cni")
	if _, err := os.Stat(spinboxConfDir); err == nil {
		return NetworkConfig{
			CNIConfDir: spinboxConfDir,
			CNIBinDir:  spinboxBinDir,
		}
	}

	// Priority 3: Standard system CNI paths (fallback)
	// Used when neither env vars nor spinbox paths are available
	return NetworkConfig{
		CNIConfDir: "/etc/cni/net.d",
		CNIBinDir:  "/opt/cni/bin",
	}
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

	// Tracks in-flight teardown operations to avoid duplicate cleanup
	// Prevents race conditions when multiple goroutines try to release the same container
	teardownInFlight map[string]*teardownInFlight
	teardownMu       sync.Mutex

	// Metrics for CNI operations - allows per-instance tracking for testing
	metrics *Metrics

	// ipamDir is the directory where IPAM state files are stored.
	// Defaults to /var/lib/cni/networks. Configurable for testing.
	ipamDir string
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

// Metrics returns the CNI operation metrics for this manager instance.
func (nm *cniNetworkManager) Metrics() *Metrics {
	return nm.metrics
}
