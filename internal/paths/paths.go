// Package paths provides standard filesystem paths used by qemubox.
// All paths are now loaded from the centralized configuration file.
package paths

import (
	"os"
	"path/filepath"

	"github.com/aledbf/qemubox/containerd/internal/config"
)

// GetShareDir returns the qemubox share directory from configuration
func GetShareDir() string {
	cfg, err := config.Get()
	if err != nil {
		// This should never happen as config is loaded at startup
		// Return default as fallback
		return "/usr/share/qemubox"
	}
	return cfg.Paths.ShareDir
}

// GetStateDir returns the qemubox state directory from configuration
func GetStateDir() string {
	cfg, err := config.Get()
	if err != nil {
		return "/var/lib/qemubox"
	}
	return cfg.Paths.StateDir
}

// GetLogDir returns the qemubox log directory from configuration
func GetLogDir() string {
	cfg, err := config.Get()
	if err != nil {
		return "/var/log/qemubox"
	}
	return cfg.Paths.LogDir
}

// KernelPath returns the full path to the kernel binary
func KernelPath() string {
	return filepath.Join(GetShareDir(), "kernel", "qemubox-kernel-x86_64")
}

// InitrdPath returns the full path to the initrd binary
func InitrdPath() string {
	return filepath.Join(GetShareDir(), "kernel", "qemubox-initrd")
}

// QemuPath returns the full path to the qemu-system-x86_64 binary
func QemuPath() string {
	cfg, err := config.Get()
	if err != nil {
		return "/usr/bin/qemu-system-x86_64"
	}

	// If explicitly configured, use that path
	if cfg.Paths.QEMUPath != "" {
		return cfg.Paths.QEMUPath
	}

	// Otherwise perform auto-discovery
	return discoverQemuPath(cfg.Paths.ShareDir)
}

// QemuSharePath returns the path to QEMU's share directory containing BIOS files
func QemuSharePath() string {
	cfg, err := config.Get()
	if err != nil {
		return "/usr/share/qemu"
	}

	// If explicitly configured, use that path
	if cfg.Paths.QEMUSharePath != "" {
		return cfg.Paths.QEMUSharePath
	}

	// Otherwise perform auto-discovery
	return discoverQemuSharePath(cfg.Paths.ShareDir)
}

// discoverQemuPath attempts to find qemu-system-x86_64 binary
func discoverQemuPath(shareDir string) string {
	// Check qemubox share directory first
	candidates := []string{
		filepath.Join(shareDir, "bin", "qemu-system-x86_64"),
		"/usr/bin/qemu-system-x86_64",
		"/usr/local/bin/qemu-system-x86_64",
	}

	for _, path := range candidates {
		if fileExists(path) {
			return path
		}
	}

	// Default fallback
	return "/usr/bin/qemu-system-x86_64"
}

// discoverQemuSharePath attempts to find QEMU share directory
func discoverQemuSharePath(shareDir string) string {
	// Check qemubox share directory first
	candidates := []string{
		filepath.Join(shareDir, "qemu"),
		"/usr/share/qemu",
		"/usr/local/share/qemu",
	}

	for _, path := range candidates {
		if dirExists(path) {
			return path
		}
	}

	// Default fallback
	return "/usr/share/qemu"
}

// fileExists checks if a file exists
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// dirExists checks if a directory exists
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
