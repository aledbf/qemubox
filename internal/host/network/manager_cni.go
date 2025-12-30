//go:build linux

package network

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/log"

	"github.com/aledbf/qemubox/containerd/internal/host/network/cni"
)

// newCNINetworkManager creates a cniNetworkManager configured for CNI mode.
func newCNINetworkManager(config NetworkConfig) (*cniNetworkManager, error) {
	// Create CNI manager
	cniMgr, err := cni.NewCNIManager(
		config.CNIConfDir,
		config.CNIBinDir,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create CNI manager: %w", err)
	}

	nm := &cniNetworkManager{
		config:     config,
		cniManager: cniMgr,
		cniResults: make(map[string]*cni.CNIResult),
		inFlight:   make(map[string]*setupInFlight),
	}

	return nm, nil
}

// ensureNetworkResourcesCNI allocates and configures network resources using CNI plugins.
// If multiple goroutines call this concurrently for the same container ID, only one will
// perform the actual CNI setup. The others will block until setup completes, then return
// the same result or error.
func (nm *cniNetworkManager) ensureNetworkResourcesCNI(ctx context.Context, env *Environment) error {
	// Fast path: check if already configured
	nm.cniMu.RLock()
	if result, exists := nm.cniResults[env.ID]; exists {
		nm.cniMu.RUnlock()
		log.G(ctx).WithFields(log.Fields{
			"vmID": env.ID,
			"tap":  result.TAPDevice,
		}).Debug("CNI resources already allocated")
		nm.updateEnvironment(env, result)
		return nil
	}
	nm.cniMu.RUnlock()

	// Check if another goroutine is already setting up this container
	nm.inflightMu.Lock()
	if inflight, exists := nm.inFlight[env.ID]; exists {
		// Another goroutine is doing the setup - wait for it
		nm.inflightMu.Unlock()
		log.G(ctx).WithField("vmID", env.ID).Debug("waiting for in-flight CNI setup to complete")
		<-inflight.done

		// Check the result
		if inflight.err != nil {
			return inflight.err
		}
		nm.updateEnvironment(env, inflight.result)
		return nil
	}

	// We're the first - create an in-flight tracker so others will wait
	inflight := &setupInFlight{
		done: make(chan struct{}),
	}
	nm.inFlight[env.ID] = inflight
	nm.inflightMu.Unlock()

	// Ensure we clean up the in-flight tracker and wake waiters when done
	defer func() {
		close(inflight.done)
		nm.inflightMu.Lock()
		delete(nm.inFlight, env.ID)
		nm.inflightMu.Unlock()
	}()

	// Perform the actual CNI setup (without holding locks)
	result, err := nm.performCNISetup(ctx, env.ID)
	if err != nil {
		inflight.err = err
		return err
	}

	// Store result
	nm.cniMu.Lock()
	nm.cniResults[env.ID] = result
	nm.cniMu.Unlock()

	inflight.result = result
	nm.updateEnvironment(env, result)

	log.G(ctx).WithFields(log.Fields{
		"vmID":    env.ID,
		"tap":     result.TAPDevice,
		"ip":      result.IPAddress,
		"gateway": result.Gateway,
	}).Info("CNI network configured")

	return nil
}

// performCNISetup executes the actual CNI plugin chain setup.
// This is extracted to a separate function to keep the synchronization logic clear.
func (nm *cniNetworkManager) performCNISetup(ctx context.Context, containerID string) (*cni.CNIResult, error) {
	// Create network namespace for CNI execution
	netnsPath, err := cni.CreateNetNS(containerID)
	if err != nil {
		return nil, fmt.Errorf("create netns for CNI: %w", err)
	}

	// Execute CNI plugin chain
	result, err := nm.cniManager.Setup(ctx, containerID, netnsPath)
	if err != nil {
		// Clean up netns on failure
		_ = cni.DeleteNetNS(containerID)

		// Check if this is a veth name conflict error
		if isVethConflictError(err) {
			log.G(ctx).WithError(err).WithField("containerID", containerID).
				Warn("CNI setup failed due to veth conflict, attempting cleanup")

			// Try to clean up orphaned resources
			cleanupNetns, cleanupErr := cni.CreateNetNS(containerID)
			if cleanupErr == nil {
				_ = nm.cniManager.Teardown(ctx, containerID, cleanupNetns)
				_ = cni.DeleteNetNS(containerID)
			}

			return nil, fmt.Errorf("setup CNI network (veth conflict - orphaned resources from previous run?): %w", err)
		}

		return nil, fmt.Errorf("setup CNI network: %w", err)
	}

	return result, nil
}

// updateEnvironment updates the environment with network information from a CNI result.
func (nm *cniNetworkManager) updateEnvironment(env *Environment, result *cni.CNIResult) {
	env.NetworkInfo = &NetworkInfo{
		TapName: result.TAPDevice,
		MAC:     result.TAPMAC,
		IP:      result.IPAddress,
		Netmask: result.Netmask,
		Gateway: result.Gateway,
	}
}

// releaseNetworkResourcesCNI releases network resources using CNI plugins.
func (nm *cniNetworkManager) releaseNetworkResourcesCNI(ctx context.Context, env *Environment) error {
	// Get CNI result for this VM
	nm.cniMu.RLock()
	result, exists := nm.cniResults[env.ID]
	nm.cniMu.RUnlock()

	if !exists {
		log.G(ctx).WithField("vmID", env.ID).
			Warn("No CNI result found for VM, attempting cleanup anyway")
		// Even if we don't have the result in memory, try to clean up
		// This handles the case where the shim restarted and lost the in-memory state
	}

	// Get the netns path that was created during setup
	// The netns should still exist from when we called ensureNetworkResourcesCNI
	netnsPath := cni.GetNetNSPath(env.ID)

	// Track if any errors occurred during teardown
	hadErrors := false

	// Check if the netns exists
	if !cni.NetNSExists(env.ID) {
		log.G(ctx).WithField("vmID", env.ID).
			Warn("Netns does not exist, creating temporary one for CNI teardown")
		hadErrors = true
		// Create a temporary netns for cleanup if the original is gone
		// This can happen if the host was rebooted or the netns was manually deleted
		tmpNetns, err := cni.CreateNetNS(env.ID)
		if err != nil {
			log.G(ctx).WithError(err).WithField("vmID", env.ID).
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
	if err := nm.cniManager.Teardown(ctx, env.ID, netnsPath); err != nil {
		hadErrors = true
		if netnsPath == "" {
			// Expected to have some errors without netns, but IPAM cleanup might still work
			log.G(ctx).WithError(err).WithField("vmID", env.ID).
				Debug("CNI teardown failed without netns (expected), but IPAM cleanup may have succeeded")
		} else {
			log.G(ctx).WithError(err).WithField("vmID", env.ID).
				Warn("Failed to teardown CNI network")
		}
		// Continue with cleanup - we still want to remove state
	} else if netnsPath == "" {
		log.G(ctx).WithField("vmID", env.ID).
			Debug("CNI teardown completed without netns (IPAM cleanup may have succeeded)")
	}

	// Clean up netns (whether it's the original or temporary)
	if err := cni.DeleteNetNS(env.ID); err != nil {
		hadErrors = true
		log.G(ctx).WithError(err).WithField("vmID", env.ID).
			Warn("Failed to delete netns")
	}

	// Remove from CNI results map
	nm.cniMu.Lock()
	delete(nm.cniResults, env.ID)
	nm.cniMu.Unlock()

	// Log final status - use Debug level if errors occurred to avoid misleading success messages
	if exists {
		fields := log.Fields{
			"vmID": env.ID,
			"tap":  result.TAPDevice,
		}
		if hadErrors {
			log.G(ctx).WithFields(fields).Debug("CNI network cleanup completed (with errors)")
		} else {
			log.G(ctx).WithFields(fields).Info("CNI network released")
		}
	} else {
		if hadErrors {
			log.G(ctx).WithField("vmID", env.ID).Debug("CNI network cleanup attempted (with errors)")
		} else {
			log.G(ctx).WithField("vmID", env.ID).Info("CNI network cleanup attempted")
		}
	}

	// Clear environment network info
	env.NetworkInfo = nil

	return nil
}

// isVethConflictError detects veth naming conflicts from CNI plugin errors.
//
// CNI plugins don't return structured errors, so we must parse error strings.
// This is inherently fragile but unavoidable given the CNI interface.
//
// This typically happens when CNI tries to create a veth pair but the name
// already exists from a previous run that wasn't properly cleaned up.
//
// Common error patterns from different CNI plugins:
//   - "file exists" or "already exists"
//   - Mentions "veth", "peer", or "interface"
func isVethConflictError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// Check for common error patterns (case-insensitive for robustness)
	return (strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "file exists")) &&
		(strings.Contains(msg, "veth") ||
			strings.Contains(msg, "peer") ||
			strings.Contains(msg, "interface"))
}
