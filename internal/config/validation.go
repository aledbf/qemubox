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

	// EnableScaleDown is boolean, always valid

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

	// EnableScaleDown is boolean, always valid

	return nil
}

// Helper functions

func validateDirectoryExists(path, fieldName string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s directory does not exist: %s (please create it or check your config)", fieldName, path)
		}
		return fmt.Errorf("%s cannot access directory %s: %w", fieldName, path, err)
	}

	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory: %s", fieldName, path)
	}

	return nil
}

// ensureDirectoryWritable ensures a directory exists and is writable.
// If the directory doesn't exist, it creates it with 0750 permissions.
func ensureDirectoryWritable(path, fieldName string) error {
	// Check if directory exists
	if err := validateDirectoryExists(path, fieldName); err != nil {
		// If directory doesn't exist, try to create it
		if os.IsNotExist(err) {
			if err := os.MkdirAll(path, 0750); err != nil {
				return fmt.Errorf("%s directory does not exist and cannot be created: %s (%w)", fieldName, path, err)
			}
		} else {
			return err
		}
	}

	// Check write permission
	if err := unix.Access(path, unix.W_OK); err != nil {
		return fmt.Errorf("%s directory is not writable: %s", fieldName, path)
	}

	return nil
}

func validateExecutable(path, fieldName string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s file does not exist: %s", fieldName, path)
		}
		return fmt.Errorf("%s cannot access file %s: %w", fieldName, path, err)
	}

	if info.IsDir() {
		return fmt.Errorf("%s is a directory, not an executable: %s", fieldName, path)
	}

	// Check if file is executable
	if info.Mode()&0111 == 0 {
		return fmt.Errorf("%s file is not executable: %s (try: chmod +x %s)", fieldName, path, path)
	}

	return nil
}
