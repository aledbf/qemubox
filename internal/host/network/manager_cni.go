//go:build linux

package network

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/log"

	"github.com/aledbf/qemubox/containerd/internal/host/network/cni"
	boltstore "github.com/aledbf/qemubox/containerd/internal/host/store"
)

// newCNINetworkManager creates a NetworkManager configured for CNI mode.
func newCNINetworkManager(
	ctx context.Context,
	cancel context.CancelFunc,
	config NetworkConfig,
	networkConfigStore boltstore.Store[NetworkConfig],
) (*NetworkManager, error) {
	if networkConfigStore == nil {
		cancel()
		return nil, fmt.Errorf("NetworkConfigStore is required")
	}

	// Create CNI manager
	cniMgr, err := cni.NewCNIManager(
		config.CNIConfDir,
		config.CNIBinDir,
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create CNI manager: %w", err)
	}

	log.G(ctx).WithFields(log.Fields{
		"confDir": config.CNIConfDir,
		"binDir":  config.CNIBinDir,
	}).Info("CNI mode enabled - auto-discovering network config")

	nm := &NetworkManager{
		config:             config,
		networkConfigStore: networkConfigStore,
		ctx:                ctx,
		cancelFunc:         cancel,
		cniManager:         cniMgr,
		cniResults:         make(map[string]*cni.CNIResult),
	}

	return nm, nil
}

// ensureNetworkResourcesCNI allocates and configures network resources using CNI plugins.
func (nm *NetworkManager) ensureNetworkResourcesCNI(env *Environment) error {
	// Check if already configured
	nm.cniMu.RLock()
	if result, exists := nm.cniResults[env.ID]; exists {
		nm.cniMu.RUnlock()
		log.G(nm.ctx).WithFields(log.Fields{
			"vmID": env.ID,
			"tap":  result.TAPDevice,
		}).Debug("CNI resources already allocated")
		return nil
	}
	nm.cniMu.RUnlock()

	// Create network namespace for CNI execution
	netnsPath, err := cni.CreateNetNS(env.ID)
	if err != nil {
		return fmt.Errorf("failed to create netns for CNI: %w", err)
	}

	// Execute CNI plugin chain
	result, err := nm.cniManager.Setup(nm.ctx, env.ID, netnsPath)
	if err != nil {
		// Clean up netns on failure
		_ = cni.DeleteNetNS(env.ID)

		// Check if this is a veth name conflict error
		if isVethConflictError(err) {
			log.G(nm.ctx).WithError(err).WithField("vmID", env.ID).
				Warn("CNI setup failed due to existing veth pair, attempting cleanup")

			// Try to clean up any orphaned resources for this container ID
			// Create a temporary netns for cleanup
			cleanupNetns, cleanupErr := cni.CreateNetNS(env.ID)
			if cleanupErr == nil {
				_ = nm.cniManager.Teardown(nm.ctx, env.ID, cleanupNetns)
				_ = cni.DeleteNetNS(env.ID)
			}

			return fmt.Errorf("failed to setup CNI network (veth conflict - orphaned resources from previous run?): %w", err)
		}

		return fmt.Errorf("failed to setup CNI network: %w", err)
	}

	log.G(nm.ctx).WithFields(log.Fields{
		"vmID":    env.ID,
		"tap":     result.TAPDevice,
		"ip":      result.IPAddress,
		"gateway": result.Gateway,
	}).Info("CNI network configured")

	// Store CNI result for teardown
	nm.cniMu.Lock()
	nm.cniResults[env.ID] = result
	nm.cniMu.Unlock()

	// Update environment with network info
	env.NetworkInfo = &NetworkInfo{
		TapName: result.TAPDevice,
		MAC:     result.TAPMAC,
		IP:      result.IPAddress,
		Netmask: "255.255.0.0", // Note: Extract from CNI result when available.
		Gateway: result.Gateway,
	}

	return nil
}

// releaseNetworkResourcesCNI releases network resources using CNI plugins.
func (nm *NetworkManager) releaseNetworkResourcesCNI(env *Environment) error {
	// Get CNI result for this VM
	nm.cniMu.RLock()
	result, exists := nm.cniResults[env.ID]
	nm.cniMu.RUnlock()

	if !exists {
		log.G(nm.ctx).WithField("vmID", env.ID).
			Warn("No CNI result found for VM, attempting cleanup anyway")
		// Even if we don't have the result in memory, try to clean up
		// This handles the case where the shim restarted and lost the in-memory state
	}

	// Get the netns path that was created during setup
	// The netns should still exist from when we called ensureNetworkResourcesCNI
	netnsPath := cni.GetNetNSPath(env.ID)

	// Check if the netns exists
	if !cni.NetNSExists(env.ID) {
		log.G(nm.ctx).WithField("vmID", env.ID).
			Warn("Netns does not exist, creating temporary one for CNI teardown")
		// Create a temporary netns for cleanup if the original is gone
		// This can happen if the host was rebooted or the netns was manually deleted
		tmpNetns, err := cni.CreateNetNS(env.ID)
		if err != nil {
			log.G(nm.ctx).WithError(err).WithField("vmID", env.ID).
				Warn("Failed to create temporary netns for teardown")
			// Try teardown without netns - some CNI plugins may still clean up host-side resources
			netnsPath = ""
		} else {
			netnsPath = tmpNetns
		}
	}

	// Execute CNI DEL operation
	// This will clean up veth pairs, IP allocations, firewall rules, etc.
	// IMPORTANT: Always attempt teardown even without netns, as some CNI plugins
	// (especially IPAM plugins) can clean up IP allocations without netns
	if err := nm.cniManager.Teardown(nm.ctx, env.ID, netnsPath); err != nil {
		if netnsPath == "" {
			// Expected to have some errors without netns, but IPAM cleanup might still work
			log.G(nm.ctx).WithError(err).WithField("vmID", env.ID).
				Debug("CNI teardown failed without netns (expected), but IPAM cleanup may have succeeded")
		} else {
			log.G(nm.ctx).WithError(err).WithField("vmID", env.ID).
				Warn("Failed to teardown CNI network")
		}
		// Continue with cleanup - we still want to remove state
	} else if netnsPath == "" {
		log.G(nm.ctx).WithField("vmID", env.ID).
			Info("CNI teardown succeeded without netns (IPAM cleanup likely successful)")
	}

	// Clean up netns (whether it's the original or temporary)
	if err := cni.DeleteNetNS(env.ID); err != nil {
		log.G(nm.ctx).WithError(err).WithField("vmID", env.ID).
			Warn("Failed to delete netns")
	}

	// Remove from CNI results map
	nm.cniMu.Lock()
	delete(nm.cniResults, env.ID)
	nm.cniMu.Unlock()

	// Also remove from network config store (persistent state)
	if err := nm.networkConfigStore.Delete(nm.ctx, env.ID); err != nil {
		log.G(nm.ctx).WithError(err).WithField("vmID", env.ID).
			Warn("Failed to delete network config from store")
	}

	if exists {
		log.G(nm.ctx).WithFields(log.Fields{
			"vmID": env.ID,
			"tap":  result.TAPDevice,
		}).Info("CNI network released")
	} else {
		log.G(nm.ctx).WithField("vmID", env.ID).
			Info("CNI network cleanup attempted")
	}

	// Clear environment network info
	env.NetworkInfo = nil

	return nil
}

// isVethConflictError checks if the error is due to a veth pair name conflict.
// This typically happens when CNI tries to create a veth pair but the name already exists
// from a previous run that wasn't properly cleaned up.
func isVethConflictError(err error) bool {
	if err == nil {
		return false
	}
	errMsg := strings.ToLower(err.Error())
	return strings.Contains(errMsg, "already exists") &&
		(strings.Contains(errMsg, "veth") || strings.Contains(errMsg, "peer"))
}
