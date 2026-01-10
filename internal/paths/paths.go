// Package paths provides standard filesystem paths used by spinbox.
// These helpers take configuration as input to avoid global config coupling.
// QemuPath and QemuSharePath may probe the filesystem when auto-discovering paths.
package paths

import (
	"os"
	"path/filepath"

	"github.com/spin-stack/spinbox/internal/config"
)

// KernelPath returns the full path to the kernel binary based on the provided configuration
func KernelPath(pathsCfg config.PathsConfig) string {
	return filepath.Join(pathsCfg.ShareDir, "kernel", "spinbox-kernel-x86_64")
}

// InitrdPath returns the full path to the initrd binary based on the provided configuration
func InitrdPath(pathsCfg config.PathsConfig) string {
	return filepath.Join(pathsCfg.ShareDir, "kernel", "spinbox-initrd")
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
	// Check spinbox share directory first
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
	// Check spinbox share directory first
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
// This surfaces the real target but does not prevent TOCTOU issues.
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
// This surfaces the real target but does not prevent TOCTOU issues.
func dirExists(path string) bool {
	// Resolve symlinks to get the real path
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	info, err := os.Stat(resolved)
	return err == nil && info.IsDir()
}
