package paths

import (
	"os"
	"path/filepath"
)

const (
	// Binaries and config directory
	ShareDir = "/usr/share/beacon"

	// State files directory
	StateDir = "/var/lib/beacon"

	// Logs directory
	LogDir = "/var/log/beacon"
)

// GetShareDir returns the beacon share directory, checking environment variables first
func GetShareDir() string {
	if dir := os.Getenv("BEACON_SHARE_DIR"); dir != "" {
		return dir
	}
	return ShareDir
}

// GetStateDir returns the beacon state directory, checking environment variables first
func GetStateDir() string {
	if dir := os.Getenv("BEACON_STATE_DIR"); dir != "" {
		return dir
	}
	return StateDir
}

// GetLogDir returns the beacon log directory, checking environment variables first
func GetLogDir() string {
	if dir := os.Getenv("BEACON_LOG_DIR"); dir != "" {
		return dir
	}
	return LogDir
}

// KernelSearchPaths returns the list of directories to search for kernel binaries
func KernelSearchPaths() []string {
	paths := []string{}

	// 1. Add PATH environment variable
	if pathEnv := os.Getenv("PATH"); pathEnv != "" {
		paths = append(paths, filepath.SplitList(pathEnv)...)
	}

	// 2. Add beacon share directories
	paths = append(paths,
		GetShareDir(),
		"/usr/share/beacon",
		"/usr/local/share/beacon",
		// Legacy beaconbox paths for compatibility
		"/usr/share/beaconbox",
		"/usr/local/share/beaconbox",
	)

	return paths
}

// NetworkDBPath returns the path to the network allocation database
func NetworkDBPath() string {
	return filepath.Join(GetStateDir(), "network.db")
}

// KernelName returns the kernel binary name for the current architecture
func KernelName() string {
	return "beacon-kernel-x86_64"
}

// InitrdName returns the initrd binary name
func InitrdName() string {
	return "beacon-initrd"
}
