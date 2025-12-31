// Package paths provides standard filesystem paths used by qemubox.
// All path functions are pure functions that take configuration as a parameter,
// making them easy to test and avoiding tight coupling to the config package.
package paths

import (
	"os"
	"path/filepath"

	"github.com/aledbf/qemubox/containerd/internal/config"
)

// KernelPath returns the full path to the kernel binary based on the provided configuration
func KernelPath(pathsCfg config.PathsConfig) string {
	return filepath.Join(pathsCfg.ShareDir, "kernel", "qemubox-kernel-x86_64")
}

// InitrdPath returns the full path to the initrd binary based on the provided configuration
func InitrdPath(pathsCfg config.PathsConfig) string {
	return filepath.Join(pathsCfg.ShareDir, "kernel", "qemubox-initrd")
}

// QemuPath returns the full path to the qemu-system-x86_64 binary based on the provided configuration
func QemuPath(pathsCfg config.PathsConfig) string {
	// If explicitly configured, use that path
	if pathsCfg.QEMUPath != "" {
		return pathsCfg.QEMUPath
	}

	// Otherwise perform auto-discovery
	return discoverQemuPath(pathsCfg.ShareDir)
}

// QemuSharePath returns the path to QEMU's share directory containing BIOS files based on the provided configuration
func QemuSharePath(pathsCfg config.PathsConfig) string {
	// If explicitly configured, use that path
	if pathsCfg.QEMUSharePath != "" {
		return pathsCfg.QEMUSharePath
	}

	// Otherwise perform auto-discovery
	return discoverQemuSharePath(pathsCfg.ShareDir)
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

// fileExists checks if a file exists, resolving symlinks to the real path.
// This prevents symlink attacks by ensuring we check the actual file.
func fileExists(path string) bool {
	// Resolve symlinks to get the real path
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	info, err := os.Stat(resolved)
	return err == nil && !info.IsDir()
}

// dirExists checks if a directory exists, resolving symlinks to the real path.
// This prevents symlink attacks by ensuring we check the actual directory.
func dirExists(path string) bool {
	// Resolve symlinks to get the real path
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	info, err := os.Stat(resolved)
	return err == nil && info.IsDir()
}
