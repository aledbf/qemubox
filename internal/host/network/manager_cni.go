//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/log"

	"github.com/aledbf/qemubox/containerd/internal/host/network/cni"
)

// CleanupResult reports what succeeded and what failed during network teardown.
// This allows callers to understand partial failures instead of getting a single opaque error.
type CleanupResult struct {
	CNITeardown   error // Error from CNI DEL operation
	NetNSDelete   error // Error from network namespace deletion
	IPAMVerify    error // Error from IPAM verification (nil = verified clean)
	InMemoryClear bool  // Whether in-memory state was cleared
}

// HasError returns true if any cleanup operation failed.
func (c *CleanupResult) HasError() bool {
	return c.CNITeardown != nil || c.NetNSDelete != nil || c.IPAMVerify != nil
}

// Err returns an error if any cleanup operation failed, nil otherwise.
func (c *CleanupResult) Err() error {
	if !c.HasError() {
		return nil
	}
	return &cleanupError{result: c}
}

// cleanupError wraps CleanupResult as an error type.
type cleanupError struct {
	result *CleanupResult
}

func (c *cleanupError) Error() string {
	var msgs []string
	if c.result.CNITeardown != nil {
		msgs = append(msgs, "CNI teardown: "+c.result.CNITeardown.Error())
	}
	if c.result.NetNSDelete != nil {
		msgs = append(msgs, "netns delete: "+c.result.NetNSDelete.Error())
	}
	if c.result.IPAMVerify != nil {
		msgs = append(msgs, "IPAM verify: "+c.result.IPAMVerify.Error())
	}
	return strings.Join(msgs, "; ")
}

// Result returns the underlying CleanupResult for inspection.
func (c *cleanupError) Result() *CleanupResult {
	return c.result
}

// teardownInFlight tracks an in-progress teardown operation.
// Prevents duplicate teardown attempts for the same container ID.
type teardownInFlight struct {
	done   chan struct{}
	result CleanupResult
}

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
		config:           config,
		cniManager:       cniMgr,
		cniResults:       make(map[string]*cni.CNIResult),
		inFlight:         make(map[string]*setupInFlight),
		teardownInFlight: make(map[string]*teardownInFlight),
		metrics:          &Metrics{},
		ipamDir:          "/var/lib/cni/networks",
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

		// Wait with context cancellation support
		select {
		case <-inflight.done:
			// Setup completed
		case <-ctx.Done():
			return ctx.Err()
		}

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
	start := time.Now()
	result, err := nm.performCNISetup(ctx, env.ID)
	duration := time.Since(start)

	if err != nil {
		conflict := errors.Is(err, cni.ErrResourceConflict)
		nm.metrics.RecordSetup(false, conflict, duration)
		inflight.err = err
		return err
	}

	nm.metrics.RecordSetup(true, false, duration)

	// Store result
	nm.cniMu.Lock()
	nm.cniResults[env.ID] = result
	nm.cniMu.Unlock()

	inflight.result = result
	nm.updateEnvironment(env, result)

	log.G(ctx).WithFields(log.Fields{
		"vmID":     env.ID,
		"tap":      result.TAPDevice,
		"ip":       result.IPAddress,
		"gateway":  result.Gateway,
		"duration": duration,
	}).Info("CNI network configured")

	return nil
}

// performCNISetup executes the actual CNI plugin chain setup.
// This is extracted to a separate function to keep the synchronization logic clear.
func (nm *cniNetworkManager) performCNISetup(ctx context.Context, containerID string) (*cni.CNIResult, error) {
	// Create network namespace for CNI execution
	netnsStart := time.Now()
	netnsPath, err := cni.CreateNetNS(containerID)
	netnsLatency := time.Since(netnsStart)

	if err != nil {
		return nil, fmt.Errorf("create netns for CNI: %w", err)
	}

	log.G(ctx).WithFields(log.Fields{
		"containerID":  containerID,
		"netnsLatency": netnsLatency,
	}).Debug("network namespace created")

	// Execute CNI plugin chain
	cniStart := time.Now()
	result, err := nm.cniManager.Setup(ctx, containerID, netnsPath)
	cniLatency := time.Since(cniStart)

	if err != nil {
		// Clean up netns on failure - log but don't mask original error
		if cleanupErr := cni.DeleteNetNS(containerID); cleanupErr != nil {
			log.G(ctx).WithError(cleanupErr).WithField("containerID", containerID).
				Warn("failed to cleanup netns after CNI setup failure")
		}

		// Check if this is a resource conflict error (veth or IPAM)
		if errors.Is(err, cni.ErrResourceConflict) {
			log.G(ctx).WithError(err).WithFields(log.Fields{
				"containerID": containerID,
				"cniLatency":  cniLatency,
			}).Warn("CNI setup failed due to resource conflict, attempting cleanup")

			nm.attemptOrphanCleanup(ctx, containerID)

			return nil, fmt.Errorf("setup CNI network (resource conflict - orphaned resources from previous run?): %w", err)
		}

		return nil, fmt.Errorf("setup CNI network for %s: %w", containerID, err)
	}

	log.G(ctx).WithFields(log.Fields{
		"containerID": containerID,
		"cniLatency":  cniLatency,
		"tapDevice":   result.TAPDevice,
	}).Debug("CNI plugin chain completed")

	return result, nil
}

// attemptOrphanCleanup tries to clean up orphaned CNI resources from a previous run.
// Uses a unique temporary netns to avoid racing with other processes.
func (nm *cniNetworkManager) attemptOrphanCleanup(ctx context.Context, containerID string) {
	cleanupID := fmt.Sprintf("%s-cleanup-%d", containerID, time.Now().UnixNano())
	cleanupNetns, err := cni.CreateNetNS(cleanupID)
	if err != nil {
		log.G(ctx).WithError(err).WithField("cleanupID", cleanupID).
			Warn("failed to create temp netns for orphaned resource cleanup")
		return
	}

	if teardownErr := nm.cniManager.Teardown(ctx, containerID, cleanupNetns); teardownErr != nil {
		log.G(ctx).WithError(teardownErr).WithField("containerID", containerID).
			Warn("failed to teardown orphaned CNI resources")
	}

	if deleteErr := cni.DeleteNetNS(cleanupID); deleteErr != nil {
		log.G(ctx).WithError(deleteErr).WithField("cleanupID", cleanupID).
			Warn("failed to delete temp cleanup netns")
	}
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
// Uses deduplication to prevent concurrent teardown attempts for the same container.
// Returns an error wrapping CleanupResult with details on what succeeded/failed.
func (nm *cniNetworkManager) releaseNetworkResourcesCNI(ctx context.Context, env *Environment) error {
	// Check if another goroutine is already tearing down this container
	nm.teardownMu.Lock()
	if inflight, exists := nm.teardownInFlight[env.ID]; exists {
		nm.teardownMu.Unlock()
		log.G(ctx).WithField("vmID", env.ID).Debug("waiting for in-flight CNI teardown to complete")

		// Wait with context cancellation support
		select {
		case <-inflight.done:
			// Teardown completed
		case <-ctx.Done():
			return ctx.Err()
		}

		return inflight.result.Err()
	}

	// We're the first - create an in-flight tracker
	inflight := &teardownInFlight{
		done: make(chan struct{}),
	}
	nm.teardownInFlight[env.ID] = inflight
	nm.teardownMu.Unlock()

	// Ensure we clean up the tracker and wake waiters when done
	defer func() {
		close(inflight.done)
		nm.teardownMu.Lock()
		delete(nm.teardownInFlight, env.ID)
		nm.teardownMu.Unlock()
	}()

	// Perform actual teardown
	start := time.Now()
	inflight.result = nm.performCNITeardown(ctx, env)
	duration := time.Since(start)

	nm.metrics.RecordTeardown(!inflight.result.HasError(), duration)

	return inflight.result.Err()
}

// performCNITeardown executes the actual CNI teardown.
// Returns a CleanupResult with details on each step.
func (nm *cniNetworkManager) performCNITeardown(ctx context.Context, env *Environment) CleanupResult {
	result := CleanupResult{}

	// Get CNI result for this VM
	nm.cniMu.RLock()
	cniResult, exists := nm.cniResults[env.ID]
	nm.cniMu.RUnlock()

	if !exists {
		log.G(ctx).WithField("vmID", env.ID).
			Warn("No CNI result found for VM, attempting cleanup anyway")
		// Even if we don't have the result in memory, try to clean up
		// This handles the case where the shim restarted and lost the in-memory state
	}

	// Get the netns path that was created during setup
	netnsPath := cni.GetNetNSPath(env.ID)

	// Check if the netns exists
	if !cni.NetNSExists(env.ID) {
		log.G(ctx).WithField("vmID", env.ID).
			Warn("Netns does not exist, creating temporary one for CNI teardown")
		// Create a temporary netns for cleanup if the original is gone
		// This can happen if the host was rebooted or the netns was manually deleted
		tmpNetns, err := cni.CreateNetNS(env.ID)
		if err != nil {
			log.G(ctx).WithError(err).WithField("vmID", env.ID).
				Warn("Failed to create temporary netns for teardown")
			result.NetNSDelete = fmt.Errorf("create temporary netns: %w", err)
			// Try teardown without netns - some CNI plugins may still clean up host-side resources
			netnsPath = ""
		} else {
			netnsPath = tmpNetns
		}
	}

	// Execute CNI DEL operation
	// This will clean up veth pairs, IP allocations, firewall rules, etc.
	if err := nm.cniManager.Teardown(ctx, env.ID, netnsPath); err != nil {
		if netnsPath == "" {
			// Expected to have some errors without netns, but IPAM cleanup might still work
			log.G(ctx).WithError(err).WithField("vmID", env.ID).
				Debug("CNI teardown failed without netns (expected), but IPAM cleanup may have succeeded")
			// Still record the error - let caller decide if it matters
			result.CNITeardown = err
		} else {
			log.G(ctx).WithError(err).WithField("vmID", env.ID).
				Warn("Failed to teardown CNI network")
			result.CNITeardown = err
		}
		// Continue with cleanup - we still want to remove state
	}

	// Verify IPAM cleanup
	if verifyErr := nm.verifyIPAMCleanup(ctx, env.ID); verifyErr != nil {
		log.G(ctx).WithError(verifyErr).WithField("vmID", env.ID).
			Warn("IPAM cleanup verification failed - IP may be leaked")
		result.IPAMVerify = verifyErr
		nm.metrics.RecordIPAMLeak()
	}

	// Clean up netns (whether it's the original or temporary)
	if err := cni.DeleteNetNS(env.ID); err != nil {
		log.G(ctx).WithError(err).WithField("vmID", env.ID).
			Warn("Failed to delete netns")
		result.NetNSDelete = err
	}

	// Remove from CNI results map
	nm.cniMu.Lock()
	delete(nm.cniResults, env.ID)
	nm.cniMu.Unlock()
	result.InMemoryClear = true

	// Log final status
	if exists {
		fields := log.Fields{
			"vmID": env.ID,
			"tap":  cniResult.TAPDevice,
		}
		if err := result.Err(); err != nil {
			log.G(ctx).WithFields(fields).WithError(err).
				Warn("CNI network cleanup completed with errors")
		} else {
			log.G(ctx).WithFields(fields).Info("CNI network released")
		}
	} else {
		if err := result.Err(); err != nil {
			log.G(ctx).WithField("vmID", env.ID).WithError(err).
				Warn("CNI network cleanup attempted with errors")
		} else {
			log.G(ctx).WithField("vmID", env.ID).Info("CNI network cleanup completed")
		}
	}

	// Clear environment network info
	env.NetworkInfo = nil

	return result
}

// verifyIPAMCleanup checks if the IP allocation was properly released.
// Returns an error if the container ID still has an allocated IP.
func (nm *cniNetworkManager) verifyIPAMCleanup(ctx context.Context, containerID string) error {
	entries, err := os.ReadDir(nm.ipamDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No IPAM state directory - nothing to verify
			return nil
		}
		// Can't read directory - log but don't fail
		log.G(ctx).WithError(err).WithField("ipamDir", nm.ipamDir).
			Debug("Could not read IPAM directory for verification")
		return nil
	}

	for _, netDir := range entries {
		if !netDir.IsDir() {
			continue
		}
		netPath := filepath.Join(nm.ipamDir, netDir.Name())
		ipFiles, err := os.ReadDir(netPath)
		if err != nil {
			continue
		}
		for _, ipFile := range ipFiles {
			if ipFile.IsDir() {
				continue
			}
			// Skip special files like "last_reserved_ip"
			if strings.HasPrefix(ipFile.Name(), "last_") || strings.HasPrefix(ipFile.Name(), ".") {
				continue
			}
			content, err := os.ReadFile(filepath.Join(netPath, ipFile.Name()))
			if err != nil {
				continue
			}
			// host-local IPAM stores container ID in the file
			if strings.TrimSpace(string(content)) == containerID {
				return fmt.Errorf("%w: IP %s in network %s still allocated to %s",
					cni.ErrIPAMLeak, ipFile.Name(), netDir.Name(), containerID)
			}
		}
	}
	return nil
}
