//go:build linux

package task

import (
	"context"
	"fmt"

	"github.com/containerd/log"

	"github.com/aledbf/qemubox/containerd/internal/host/network"
	boltstore "github.com/aledbf/qemubox/containerd/internal/host/store"
	"github.com/aledbf/qemubox/containerd/internal/paths"
)

// initNetworkManager creates and initializes a new NetworkManager instance.
// Qemubox uses CNI (Container Network Interface) for all network management.
// This store persists CNI network configuration metadata; IP allocation
// is delegated to CNI IPAM plugins (state stored in /var/lib/cni/networks/).
func initNetworkManager(ctx context.Context) (network.NetworkManagerInterface, error) {
	// Create BoltDB store for CNI network configuration metadata
	dbPath := paths.CNIConfigDBPath()

	networkConfigStore, err := boltstore.NewBoltStore[network.NetworkConfig](
		dbPath, "network_configs",
	)
	if err != nil {
		return nil, fmt.Errorf("create CNI network config store: %w", err)
	}

	// Load CNI network configuration from environment
	netCfg := network.LoadNetworkConfig()

	// Create CNI-based NetworkManager
	nm, err := network.NewNetworkManager(
		netCfg,
		networkConfigStore,
	)
	if err != nil {
		_ = networkConfigStore.Close()
		return nil, fmt.Errorf("create CNI network manager: %w", err)
	}

	log.G(ctx).WithField("mode", netCfg.Mode).Info("NetworkManager initialized")
	return nm, nil
}
