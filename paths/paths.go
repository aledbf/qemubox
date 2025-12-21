// Package paths provides standard filesystem paths used by beacon.
package paths

import (
	"os"
	"path/filepath"
)

const (
	// ShareDir is the binaries and config directory.
	ShareDir = "/usr/share/beacon"

	// StateDir is the state files directory.
	StateDir = "/var/lib/beacon"

	// LogDir is the logs directory.
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

// CNIConfigDBPath returns the path to the CNI network configuration database.
// This database stores CNI network metadata, not IP allocations.
// IP allocation is handled by CNI IPAM plugins (typically in /var/lib/cni/networks/).
func CNIConfigDBPath() string {
	return filepath.Join(GetStateDir(), "cni-config.db")
}

// KernelPath returns the full path to the kernel binary
func KernelPath() string {
	return filepath.Join(GetShareDir(), "kernel", "beacon-kernel-x86_64")
}

// InitrdPath returns the full path to the initrd binary
func InitrdPath() string {
	return filepath.Join(GetShareDir(), "kernel", "beacon-initrd")
}

// QemuPath returns the full path to the qemu-system-x86_64 binary
func QemuPath() string {
	// Check custom path first
	if path := os.Getenv("BEACON_QEMU_PATH"); path != "" {
		return path
	}

	// Check beacon share directory
	customPath := filepath.Join(GetShareDir(), "bin", "qemu-system-x86_64")
	if _, err := os.Stat(customPath); err == nil {
		return customPath
	}

	// Fall back to system QEMU
	return "/usr/bin/qemu-system-x86_64"
}

// QemuSharePath returns the path to QEMU's share directory containing BIOS files
func QemuSharePath() string {
	// Check custom path first
	if path := os.Getenv("BEACON_QEMU_SHARE_PATH"); path != "" {
		return path
	}

	// Check beacon share directory
	customPath := filepath.Join(GetShareDir(), "qemu")
	if _, err := os.Stat(customPath); err == nil {
		return customPath
	}

	// Fall back to system QEMU share path
	return "/usr/share/qemu"
}
