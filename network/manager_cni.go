//go:build linux

package network

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aledbf/beacon/containerd/network/cni"
	boltstore "github.com/aledbf/beacon/containerd/store"
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
		config.CNINetworkName,
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create CNI manager: %w", err)
	}

	slog.InfoContext(ctx, "CNI mode enabled",
		"confDir", config.CNIConfDir,
		"binDir", config.CNIBinDir,
		"network", config.CNINetworkName,
	)

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
	if result, exists := nm.cniResults[env.Id]; exists {
		nm.cniMu.RUnlock()
		slog.DebugContext(nm.ctx, "CNI resources already allocated", "vmID", env.Id, "tap", result.TAPDevice)
		return nil
	}
	nm.cniMu.RUnlock()

	// Create network namespace for CNI execution
	netnsPath, err := cni.CreateNetNS(env.Id)
	if err != nil {
		return fmt.Errorf("failed to create netns for CNI: %w", err)
	}

	// Execute CNI plugin chain
	result, err := nm.cniManager.Setup(nm.ctx, env.Id, netnsPath)
	if err != nil {
		// Clean up netns on failure
		_ = cni.DeleteNetNS(env.Id)

		// Check if this is a veth name conflict error
		if isVethConflictError(err) {
			slog.WarnContext(nm.ctx, "CNI setup failed due to existing veth pair, attempting cleanup",
				"vmID", env.Id, "error", err)

			// Try to clean up any orphaned resources for this container ID
			// Create a temporary netns for cleanup
			cleanupNetns, cleanupErr := cni.CreateNetNS(env.Id)
			if cleanupErr == nil {
				_ = nm.cniManager.Teardown(nm.ctx, env.Id, cleanupNetns)
				_ = cni.DeleteNetNS(env.Id)
			}

			return fmt.Errorf("failed to setup CNI network (veth conflict - orphaned resources from previous run?): %w", err)
		}

		return fmt.Errorf("failed to setup CNI network: %w", err)
	}

	slog.InfoContext(nm.ctx, "CNI network configured",
		"vmID", env.Id,
		"tap", result.TAPDevice,
		"ip", result.IPAddress,
		"gateway", result.Gateway,
	)

	// Store CNI result for teardown
	nm.cniMu.Lock()
	nm.cniResults[env.Id] = result
	nm.cniMu.Unlock()

	// Update environment with network info
	env.NetworkInfo = &NetworkInfo{
		TapName:    result.TAPDevice,
		BridgeName: bridgeName, // CNI may use different bridge, but keep for compatibility
		IP:         result.IPAddress,
		Netmask:    "255.255.0.0", // TODO: Extract from CNI result
		Gateway:    result.Gateway,
	}

	// NOTE: We do NOT delete the netns here!
	// The netns must persist for the lifetime of the VM so that CNI DEL operations
	// can properly clean up network resources. The netns will be deleted during
	// releaseNetworkResourcesCNI().

	return nil
}

// releaseNetworkResourcesCNI releases network resources using CNI plugins.
func (nm *NetworkManager) releaseNetworkResourcesCNI(env *Environment) error {
	// Get CNI result for this VM
	nm.cniMu.RLock()
	result, exists := nm.cniResults[env.Id]
	nm.cniMu.RUnlock()

	if !exists {
		slog.WarnContext(nm.ctx, "No CNI result found for VM, attempting cleanup anyway", "vmID", env.Id)
		// Even if we don't have the result in memory, try to clean up
		// This handles the case where the shim restarted and lost the in-memory state
	}

	// Get the netns path that was created during setup
	// The netns should still exist from when we called ensureNetworkResourcesCNI
	netnsPath := cni.GetNetNSPath(env.Id)

	// Check if the netns exists
	if !cni.NetNSExists(env.Id) {
		slog.WarnContext(nm.ctx, "Netns does not exist, creating temporary one for CNI teardown", "vmID", env.Id)
		// Create a temporary netns for cleanup if the original is gone
		// This can happen if the host was rebooted or the netns was manually deleted
		tmpNetns, err := cni.CreateNetNS(env.Id)
		if err != nil {
			slog.WarnContext(nm.ctx, "Failed to create temporary netns for teardown", "vmID", env.Id, "error", err)
			// Try teardown without netns - some CNI plugins may still clean up host-side resources
			netnsPath = ""
		} else {
			netnsPath = tmpNetns
		}
	}

	// Execute CNI DEL operation
	// This will clean up veth pairs, IP allocations, firewall rules, etc.
	if netnsPath != "" {
		if err := nm.cniManager.Teardown(nm.ctx, env.Id, netnsPath); err != nil {
			slog.WarnContext(nm.ctx, "Failed to teardown CNI network", "vmID", env.Id, "error", err)
			// Continue with cleanup - we still want to remove state
		}
	} else {
		slog.WarnContext(nm.ctx, "Skipping CNI teardown - no valid netns available", "vmID", env.Id)
	}

	// Clean up netns (whether it's the original or temporary)
	if err := cni.DeleteNetNS(env.Id); err != nil {
		slog.WarnContext(nm.ctx, "Failed to delete netns", "vmID", env.Id, "error", err)
	}

	// Remove from CNI results map
	nm.cniMu.Lock()
	delete(nm.cniResults, env.Id)
	nm.cniMu.Unlock()

	// Also remove from network config store (persistent state)
	if err := nm.networkConfigStore.Delete(nm.ctx, env.Id); err != nil {
		slog.WarnContext(nm.ctx, "Failed to delete network config from store", "vmID", env.Id, "error", err)
	}

	if exists {
		slog.InfoContext(nm.ctx, "CNI network released",
			"vmID", env.Id,
			"tap", result.TAPDevice,
		)
	} else {
		slog.InfoContext(nm.ctx, "CNI network cleanup attempted",
			"vmID", env.Id,
		)
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
