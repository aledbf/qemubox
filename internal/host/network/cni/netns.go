//go:build linux

package cni

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/containerd/log"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const (
	// netnsBasePath is the base path for network namespaces.
	netnsBasePath = "/var/run/netns"
)

// CreateNetNS creates a new network namespace for CNI plugin execution.
//
// CNI plugins expect to run in a network namespace, even though Qemubox VMs
// don't use network namespaces for the actual containers. This function
// creates a temporary network namespace that CNI plugins can use, which
// will be cleaned up after TAP device extraction.
//
// Returns the path to the network namespace.
func CreateNetNS(vmID string) (string, error) {
	// Ensure netns directory exists
	if err := os.MkdirAll(netnsBasePath, 0750); err != nil {
		return "", fmt.Errorf("failed to create netns directory: %w", err)
	}

	// Construct netns path
	netnsPath := filepath.Join(netnsBasePath, vmID)

	// Check if netns already exists (from previous run)
	if NetNSExists(vmID) {
		log.L.WithField("vmID", vmID).Warn("netns already exists from previous run, attempting cleanup")
		if err := DeleteNetNS(vmID); err != nil {
			return "", fmt.Errorf("failed to clean up existing netns: %w", err)
		}
	}

	// Lock OS thread to ensure namespace operations work correctly
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Get current namespace to restore later
	origNS, err := netns.Get()
	if err != nil {
		return "", fmt.Errorf("failed to get current netns: %w", err)
	}
	defer func() {
		if err := origNS.Close(); err != nil {
			log.L.WithError(err).Warn("failed to close original netns handle")
		}
	}()

	// Create new network namespace
	newNS, err := netns.New()
	if err != nil {
		return "", fmt.Errorf("failed to create new netns: %w", err)
	}
	defer func() {
		if err := newNS.Close(); err != nil {
			log.L.WithError(err).Warn("failed to close new netns handle")
		}
	}()

	// Bind mount the namespace to make it persistent
	// Use the file descriptor directly instead of /proc/self/ns/net
	// to ensure we're mounting the correct namespace
	nsFdPath := fmt.Sprintf("/proc/self/fd/%d", newNS)
	if err := bindMountNetNS(nsFdPath, netnsPath); err != nil {
		if restoreErr := netns.Set(origNS); restoreErr != nil {
			// Best effort cleanup of netns file on catastrophic failure
			_ = os.Remove(netnsPath)
			return "", fmt.Errorf("failed to restore original netns after bind mount error: %w", restoreErr)
		}
		return "", fmt.Errorf("failed to bind mount netns: %w", err)
	}

	// Return to original namespace
	if err := netns.Set(origNS); err != nil {
		// Clean up on failure
		_ = DeleteNetNS(vmID)
		return "", fmt.Errorf("failed to restore original netns: %w", err)
	}

	// Verify the netns was created correctly and is different from host
	if err := verifyNetNS(netnsPath, origNS); err != nil {
		_ = DeleteNetNS(vmID)
		return "", fmt.Errorf("netns verification failed: %w", err)
	}

	return netnsPath, nil
}

// verifyNetNS verifies that the created netns is valid and different from the host netns.
func verifyNetNS(netnsPath string, hostNS netns.NsHandle) error {
	// Check if the file exists
	if _, err := os.Stat(netnsPath); err != nil {
		return fmt.Errorf("netns file does not exist: %w", err)
	}

	// Open the netns file
	nsFile, err := os.Open(netnsPath)
	if err != nil {
		return fmt.Errorf("failed to open netns file: %w", err)
	}
	defer func() {
		if err := nsFile.Close(); err != nil {
			log.L.WithError(err).Warn("failed to close netns file")
		}
	}()

	// Get the inode of the netns file
	var nsFileStat, hostNSStat unix.Stat_t
	if err := unix.Fstat(int(nsFile.Fd()), &nsFileStat); err != nil {
		return fmt.Errorf("failed to stat netns file: %w", err)
	}

	// Get the inode of the host netns
	if err := unix.Fstat(int(hostNS), &hostNSStat); err != nil {
		return fmt.Errorf("failed to stat host netns: %w", err)
	}

	// Compare inodes - they should be different
	if nsFileStat.Ino == hostNSStat.Ino {
		return fmt.Errorf("created netns is the same as host netns (inode: %d)", nsFileStat.Ino)
	}

	return nil
}

// DeleteNetNS deletes a network namespace.
func DeleteNetNS(vmID string) error {
	netnsPath := filepath.Join(netnsBasePath, vmID)

	// Unmount the namespace
	if err := unmountNetNS(netnsPath); err != nil {
		// Continue with deletion even if unmount fails
		_ = err // Ignore unmount errors
	}

	// Remove the file
	if err := os.Remove(netnsPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove netns file: %w", err)
	}

	return nil
}

// GetNetNSPath returns the path to a network namespace.
func GetNetNSPath(vmID string) string {
	return filepath.Join(netnsBasePath, vmID)
}

// NetNSExists checks if a network namespace exists.
func NetNSExists(vmID string) bool {
	netnsPath := GetNetNSPath(vmID)
	_, err := os.Stat(netnsPath)
	return err == nil
}

// bindMountNetNS creates a bind mount for a network namespace.
func bindMountNetNS(source, target string) error {
	// Create target file if it doesn't exist
	f, err := os.Create(target)
	if err != nil {
		return fmt.Errorf("failed to create netns file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close netns file: %w", err)
	}

	// Bind mount the namespace using MS_BIND | MS_REC flags
	if err := unix.Mount(source, target, "", uintptr(unix.MS_BIND|unix.MS_REC), ""); err != nil {
		if removeErr := os.Remove(target); removeErr != nil && !os.IsNotExist(removeErr) {
			log.L.WithError(removeErr).Warn("failed to remove netns file after mount error")
		}
		return fmt.Errorf("failed to bind mount: %w", err)
	}

	return nil
}

// unmountNetNS unmounts a network namespace.
func unmountNetNS(target string) error {
	// Use MNT_DETACH for lazy unmount
	if err := unix.Unmount(target, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("failed to unmount netns: %w", err)
	}

	return nil
}
