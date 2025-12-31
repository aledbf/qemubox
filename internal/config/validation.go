package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// Validate validates the entire configuration.
// Returns detailed error if any validation fails.
func (c *Config) Validate() error {
	if err := c.validatePaths(); err != nil {
		return fmt.Errorf("paths validation failed: %w", err)
	}

	if err := c.validateRuntime(); err != nil {
		return fmt.Errorf("runtime validation failed: %w", err)
	}

	if err := c.validateCPUHotplug(); err != nil {
		return fmt.Errorf("cpu_hotplug validation failed: %w", err)
	}

	if err := c.validateMemHotplug(); err != nil {
		return fmt.Errorf("memory_hotplug validation failed: %w", err)
	}

	return nil
}

// validatePaths validates path configuration
func (c *Config) validatePaths() error {
	// ShareDir must exist and contain kernel/initrd
	if c.Paths.ShareDir == "" {
		return fmt.Errorf("share_dir cannot be empty")
	}

	if err := validateDirectoryExists(c.Paths.ShareDir, "share_dir"); err != nil {
		return err
	}

	// Check for kernel and initrd
	kernelPath := filepath.Join(c.Paths.ShareDir, "kernel", "qemubox-kernel-x86_64")
	initrdPath := filepath.Join(c.Paths.ShareDir, "kernel", "qemubox-initrd")

	if _, err := os.Stat(kernelPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("kernel not found at %s (check share_dir or run 'task build:kernel')", kernelPath)
		}
		return fmt.Errorf("cannot access kernel at %s: %w", kernelPath, err)
	}

	if _, err := os.Stat(initrdPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("initrd not found at %s (check share_dir or run 'task build:initrd')", initrdPath)
		}
		return fmt.Errorf("cannot access initrd at %s: %w", initrdPath, err)
	}

	// StateDir must be writable (create if it doesn't exist)
	if c.Paths.StateDir == "" {
		return fmt.Errorf("state_dir cannot be empty")
	}

	if err := ensureDirectoryWritable(c.Paths.StateDir, "state_dir"); err != nil {
		return err
	}

	// LogDir must be writable (create if it doesn't exist)
	if c.Paths.LogDir == "" {
		return fmt.Errorf("log_dir cannot be empty")
	}

	if err := ensureDirectoryWritable(c.Paths.LogDir, "log_dir"); err != nil {
		return err
	}

	// QEMUPath (if specified) must be executable
	if c.Paths.QEMUPath != "" {
		if err := validateExecutable(c.Paths.QEMUPath, "qemu_path"); err != nil {
			return err
		}
	}

	// QEMUSharePath (if specified) must exist
	if c.Paths.QEMUSharePath != "" {
		if err := validateDirectoryExists(c.Paths.QEMUSharePath, "qemu_share_path"); err != nil {
			return err
		}
	}

	return nil
}

// validateRuntime validates runtime configuration
func (c *Config) validateRuntime() error {
	// VMM must be "qemu" (only supported backend)
	if c.Runtime.VMM != "qemu" {
		return fmt.Errorf("vmm must be \"qemu\" (only supported backend), got %q", c.Runtime.VMM)
	}

	return nil
}

// validateCPUHotplug validates CPU hotplug configuration
func (c *Config) validateCPUHotplug() error {
	// Validate duration strings
	if _, err := time.ParseDuration(c.CPUHotplug.MonitorInterval); err != nil {
		return fmt.Errorf("monitor_interval must be a valid duration (e.g., \"5s\", \"1m\"): %w", err)
	}

	if _, err := time.ParseDuration(c.CPUHotplug.ScaleUpCooldown); err != nil {
		return fmt.Errorf("scale_up_cooldown must be a valid duration (e.g., \"10s\"): %w", err)
	}

	if _, err := time.ParseDuration(c.CPUHotplug.ScaleDownCooldown); err != nil {
		return fmt.Errorf("scale_down_cooldown must be a valid duration (e.g., \"30s\"): %w", err)
	}

	// Validate thresholds (0 < value <= 100)
	if c.CPUHotplug.ScaleUpThreshold <= 0 || c.CPUHotplug.ScaleUpThreshold > 100 {
		return fmt.Errorf("scale_up_threshold must be between 0 and 100, got %.2f", c.CPUHotplug.ScaleUpThreshold)
	}

	if c.CPUHotplug.ScaleDownThreshold <= 0 || c.CPUHotplug.ScaleDownThreshold > 100 {
		return fmt.Errorf("scale_down_threshold must be between 0 and 100, got %.2f", c.CPUHotplug.ScaleDownThreshold)
	}

	if c.CPUHotplug.ScaleUpThrottleLimit < 0 || c.CPUHotplug.ScaleUpThrottleLimit > 100 {
		return fmt.Errorf("scale_up_throttle_limit must be between 0 and 100, got %.2f", c.CPUHotplug.ScaleUpThrottleLimit)
	}

	// Validate stability counters (must be > 0)
	if c.CPUHotplug.ScaleUpStability <= 0 {
		return fmt.Errorf("scale_up_stability must be > 0, got %d", c.CPUHotplug.ScaleUpStability)
	}

	if c.CPUHotplug.ScaleDownStability <= 0 {
		return fmt.Errorf("scale_down_stability must be > 0, got %d", c.CPUHotplug.ScaleDownStability)
	}

	// Validate threshold relationship: scale_down must be less than scale_up
	// Otherwise the system would simultaneously want to scale up and down
	if c.CPUHotplug.ScaleDownThreshold >= c.CPUHotplug.ScaleUpThreshold {
		return fmt.Errorf("scale_down_threshold (%.2f) must be less than scale_up_threshold (%.2f)",
			c.CPUHotplug.ScaleDownThreshold, c.CPUHotplug.ScaleUpThreshold)
	}

	return nil
}

// validateMemHotplug validates memory hotplug configuration
func (c *Config) validateMemHotplug() error {
	// Validate duration strings
	if _, err := time.ParseDuration(c.MemHotplug.MonitorInterval); err != nil {
		return fmt.Errorf("monitor_interval must be a valid duration (e.g., \"10s\", \"1m\"): %w", err)
	}

	if _, err := time.ParseDuration(c.MemHotplug.ScaleUpCooldown); err != nil {
		return fmt.Errorf("scale_up_cooldown must be a valid duration (e.g., \"30s\"): %w", err)
	}

	if _, err := time.ParseDuration(c.MemHotplug.ScaleDownCooldown); err != nil {
		return fmt.Errorf("scale_down_cooldown must be a valid duration (e.g., \"60s\"): %w", err)
	}

	// Validate thresholds (0 < value <= 100)
	if c.MemHotplug.ScaleUpThreshold <= 0 || c.MemHotplug.ScaleUpThreshold > 100 {
		return fmt.Errorf("scale_up_threshold must be between 0 and 100, got %.2f", c.MemHotplug.ScaleUpThreshold)
	}

	if c.MemHotplug.ScaleDownThreshold <= 0 || c.MemHotplug.ScaleDownThreshold > 100 {
		return fmt.Errorf("scale_down_threshold must be between 0 and 100, got %.2f", c.MemHotplug.ScaleDownThreshold)
	}

	// Validate OOM safety margin (must be > 0)
	if c.MemHotplug.OOMSafetyMarginMB <= 0 {
		return fmt.Errorf("oom_safety_margin_mb must be > 0, got %d", c.MemHotplug.OOMSafetyMarginMB)
	}

	// Validate increment size (must be 128MB-aligned and > 0)
	if c.MemHotplug.IncrementSizeMB <= 0 {
		return fmt.Errorf("increment_size_mb must be > 0, got %d", c.MemHotplug.IncrementSizeMB)
	}

	if c.MemHotplug.IncrementSizeMB%128 != 0 {
		return fmt.Errorf("increment_size_mb must be 128MB-aligned, got %d (try %d or %d)",
			c.MemHotplug.IncrementSizeMB,
			(c.MemHotplug.IncrementSizeMB/128)*128,
			((c.MemHotplug.IncrementSizeMB/128)+1)*128)
	}

	// Validate stability counters (must be > 0)
	if c.MemHotplug.ScaleUpStability <= 0 {
		return fmt.Errorf("scale_up_stability must be > 0, got %d", c.MemHotplug.ScaleUpStability)
	}

	if c.MemHotplug.ScaleDownStability <= 0 {
		return fmt.Errorf("scale_down_stability must be > 0, got %d", c.MemHotplug.ScaleDownStability)
	}

	// Validate threshold relationship: scale_down must be less than scale_up
	// Otherwise the system would simultaneously want to scale up and down
	if c.MemHotplug.ScaleDownThreshold >= c.MemHotplug.ScaleUpThreshold {
		return fmt.Errorf("scale_down_threshold (%.2f) must be less than scale_up_threshold (%.2f)",
			c.MemHotplug.ScaleDownThreshold, c.MemHotplug.ScaleUpThreshold)
	}

	return nil
}

// Helper functions

// canonicalizePath resolves symlinks and cleans the path to prevent symlink attacks.
// This ensures paths are resolved to their real location before validation.
// If the path doesn't exist yet (for directories we'll create), we clean it
// and resolve as much of the path as possible.
func canonicalizePath(path string) (string, error) {
	// First clean the path to normalize it (remove . and .. where possible)
	cleaned := filepath.Clean(path)

	// Try to resolve symlinks for the full path
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err == nil {
		return resolved, nil
	}

	// If path doesn't exist, resolve parent directories that do exist
	// This handles the case where we need to create a directory
	if os.IsNotExist(err) {
		// Walk up the path to find the first existing parent
		dir := cleaned
		var nonExistent []string
		for {
			parent := filepath.Dir(dir)
			if parent == dir {
				// Reached root, nothing exists
				break
			}

			resolved, err := filepath.EvalSymlinks(parent)
			if err == nil {
				// Found an existing parent, reconstruct the path
				for i := len(nonExistent) - 1; i >= 0; i-- {
					resolved = filepath.Join(resolved, nonExistent[i])
				}
				resolved = filepath.Join(resolved, filepath.Base(dir))
				return resolved, nil
			}

			if !os.IsNotExist(err) {
				return "", fmt.Errorf("failed to resolve path %s: %w", path, err)
			}

			nonExistent = append(nonExistent, filepath.Base(dir))
			dir = parent
		}
		// Nothing exists, just return the cleaned path
		return cleaned, nil
	}

	return "", fmt.Errorf("failed to resolve path %s: %w", path, err)
}

func validateDirectoryExists(path, fieldName string) error {
	// Canonicalize the path to prevent symlink attacks
	canonical, err := canonicalizePath(path)
	if err != nil {
		return fmt.Errorf("%s path resolution failed: %w", fieldName, err)
	}

	info, err := os.Stat(canonical)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s directory does not exist: %s (resolved from %s)", fieldName, canonical, path)
		}
		return fmt.Errorf("%s cannot access directory %s: %w", fieldName, canonical, err)
	}

	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory: %s", fieldName, canonical)
	}

	return nil
}

// ensureDirectoryWritable ensures a directory exists and is writable.
// If the directory doesn't exist, it creates it with 0750 permissions.
// Paths are canonicalized to prevent symlink attacks.
func ensureDirectoryWritable(path, fieldName string) error {
	// Canonicalize the path first to prevent symlink attacks
	// This resolves existing parent directories to their real paths
	canonical, err := canonicalizePath(path)
	if err != nil {
		return fmt.Errorf("%s path resolution failed: %w", fieldName, err)
	}

	// Check if directory exists
	info, statErr := os.Stat(canonical)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			// Directory doesn't exist, try to create it at the canonical path
			if err := os.MkdirAll(canonical, 0750); err != nil {
				return fmt.Errorf("%s directory does not exist and cannot be created: %s (%w)", fieldName, canonical, err)
			}
		} else {
			return fmt.Errorf("%s cannot access directory %s: %w", fieldName, canonical, statErr)
		}
	} else if !info.IsDir() {
		return fmt.Errorf("%s is not a directory: %s", fieldName, canonical)
	}

	// Check write permission on the canonical path
	if err := unix.Access(canonical, unix.W_OK); err != nil {
		return fmt.Errorf("%s directory is not writable: %s", fieldName, canonical)
	}

	return nil
}

func validateExecutable(path, fieldName string) error {
	// Canonicalize the path to prevent symlink attacks
	canonical, err := canonicalizePath(path)
	if err != nil {
		return fmt.Errorf("%s path resolution failed: %w", fieldName, err)
	}

	info, err := os.Stat(canonical)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s file does not exist: %s", fieldName, canonical)
		}
		return fmt.Errorf("%s cannot access file %s: %w", fieldName, canonical, err)
	}

	if info.IsDir() {
		return fmt.Errorf("%s is a directory, not an executable: %s", fieldName, canonical)
	}

	// Check if file is executable
	if info.Mode()&0111 == 0 {
		return fmt.Errorf("%s file is not executable: %s (try: chmod +x %s)", fieldName, canonical, canonical)
	}

	return nil
}
