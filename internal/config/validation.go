package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// Validate validates the entire configuration.
func (c *Config) Validate() error {
	if err := c.validatePaths(); err != nil {
		return fmt.Errorf("paths: %w", err)
	}
	if err := c.validateRuntime(); err != nil {
		return fmt.Errorf("runtime: %w", err)
	}
	if err := c.validateTimeouts(); err != nil {
		return fmt.Errorf("timeouts: %w", err)
	}
	if err := c.validateCPUHotplug(); err != nil {
		return fmt.Errorf("cpu_hotplug: %w", err)
	}
	if err := c.validateMemHotplug(); err != nil {
		return fmt.Errorf("memory_hotplug: %w", err)
	}
	return nil
}

func (c *Config) validatePaths() error {
	if c.Paths.ShareDir == "" {
		return fmt.Errorf("share_dir cannot be empty")
	}
	if err := validateDirExists(c.Paths.ShareDir, "share_dir"); err != nil {
		return err
	}

	// Check kernel and initrd exist
	kernelPath := filepath.Join(c.Paths.ShareDir, "kernel", "qemubox-kernel-x86_64")
	initrdPath := filepath.Join(c.Paths.ShareDir, "kernel", "qemubox-initrd")

	if _, err := os.Stat(kernelPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("kernel not found at %s (run 'task build:kernel')", kernelPath)
		}
		return fmt.Errorf("cannot access kernel: %w", err)
	}
	if _, err := os.Stat(initrdPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("initrd not found at %s (run 'task build:initrd')", initrdPath)
		}
		return fmt.Errorf("cannot access initrd: %w", err)
	}

	if c.Paths.StateDir == "" {
		return fmt.Errorf("state_dir cannot be empty")
	}
	if err := ensureDirWritable(c.Paths.StateDir, "state_dir"); err != nil {
		return err
	}

	if c.Paths.LogDir == "" {
		return fmt.Errorf("log_dir cannot be empty")
	}
	if err := ensureDirWritable(c.Paths.LogDir, "log_dir"); err != nil {
		return err
	}

	if c.Paths.QEMUPath != "" {
		if err := validateExecutable(c.Paths.QEMUPath, "qemu_path"); err != nil {
			return err
		}
	}
	if c.Paths.QEMUSharePath != "" {
		if err := validateDirExists(c.Paths.QEMUSharePath, "qemu_share_path"); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) validateRuntime() error {
	if c.Runtime.VMM != "qemu" {
		return fmt.Errorf("vmm must be \"qemu\", got %q", c.Runtime.VMM)
	}
	return nil
}

func (c *Config) validateTimeouts() error {
	fields := map[string]string{
		"vm_start":          c.Timeouts.VMStart,
		"device_detection":  c.Timeouts.DeviceDetection,
		"shutdown_grace":    c.Timeouts.ShutdownGrace,
		"event_reconnect":   c.Timeouts.EventReconnect,
		"task_client_retry": c.Timeouts.TaskClientRetry,
		"io_wait":           c.Timeouts.IOWait,
		"qmp_command":       c.Timeouts.QMPCommand,
	}

	for name, val := range fields {
		d, err := time.ParseDuration(val)
		if err != nil {
			return fmt.Errorf("%s: invalid duration %q", name, val)
		}
		if d <= 0 {
			return fmt.Errorf("%s: must be positive, got %s", name, d)
		}
		if d > time.Hour {
			return fmt.Errorf("%s: too large (%s), max is 1h", name, d)
		}
	}
	return nil
}

func validateHotplug(h *HotplugConfig, prefix string) error {
	// Validate durations
	for name, val := range map[string]string{
		"monitor_interval":    h.MonitorInterval,
		"scale_up_cooldown":   h.ScaleUpCooldown,
		"scale_down_cooldown": h.ScaleDownCooldown,
	} {
		if _, err := time.ParseDuration(val); err != nil {
			return fmt.Errorf("%s.%s: invalid duration %q", prefix, name, val)
		}
	}

	// Validate thresholds (0 < val <= 100)
	if h.ScaleUpThreshold <= 0 || h.ScaleUpThreshold > 100 {
		return fmt.Errorf("%s.scale_up_threshold: must be 0-100, got %.2f", prefix, h.ScaleUpThreshold)
	}
	if h.ScaleDownThreshold <= 0 || h.ScaleDownThreshold > 100 {
		return fmt.Errorf("%s.scale_down_threshold: must be 0-100, got %.2f", prefix, h.ScaleDownThreshold)
	}
	if h.ScaleDownThreshold >= h.ScaleUpThreshold {
		return fmt.Errorf("%s: scale_down_threshold (%.2f) must be < scale_up_threshold (%.2f)",
			prefix, h.ScaleDownThreshold, h.ScaleUpThreshold)
	}

	// Validate stability counters
	if h.ScaleUpStability <= 0 {
		return fmt.Errorf("%s.scale_up_stability: must be > 0, got %d", prefix, h.ScaleUpStability)
	}
	if h.ScaleDownStability <= 0 {
		return fmt.Errorf("%s.scale_down_stability: must be > 0, got %d", prefix, h.ScaleDownStability)
	}

	return nil
}

func (c *Config) validateCPUHotplug() error {
	if err := validateHotplug(&c.CPUHotplug.HotplugConfig, "cpu_hotplug"); err != nil {
		return err
	}
	if c.CPUHotplug.ScaleUpThrottleLimit < 0 || c.CPUHotplug.ScaleUpThrottleLimit > 100 {
		return fmt.Errorf("scale_up_throttle_limit: must be 0-100, got %.2f", c.CPUHotplug.ScaleUpThrottleLimit)
	}
	return nil
}

func (c *Config) validateMemHotplug() error {
	if err := validateHotplug(&c.MemHotplug.HotplugConfig, "memory_hotplug"); err != nil {
		return err
	}

	if c.MemHotplug.OOMSafetyMarginMB <= 0 {
		return fmt.Errorf("oom_safety_margin_mb: must be > 0, got %d", c.MemHotplug.OOMSafetyMarginMB)
	}
	if c.MemHotplug.IncrementSizeMB <= 0 {
		return fmt.Errorf("increment_size_mb: must be > 0, got %d", c.MemHotplug.IncrementSizeMB)
	}
	if c.MemHotplug.IncrementSizeMB%128 != 0 {
		return fmt.Errorf("increment_size_mb: must be 128MB-aligned, got %d", c.MemHotplug.IncrementSizeMB)
	}
	return nil
}

// Helper functions

func canonicalizePath(path string) (string, error) {
	cleaned := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err == nil {
		return resolved, nil
	}
	if os.IsNotExist(err) {
		return cleaned, nil
	}
	return "", fmt.Errorf("failed to resolve path %s: %w", path, err)
}

func validateDirExists(path, name string) error {
	canonical, err := canonicalizePath(path)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	info, err := os.Stat(canonical)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s: directory does not exist: %s", name, canonical)
		}
		return fmt.Errorf("%s: cannot access: %w", name, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: not a directory: %s", name, canonical)
	}
	return nil
}

func ensureDirWritable(path, name string) error {
	canonical, err := canonicalizePath(path)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	info, statErr := os.Stat(canonical)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			if err := os.MkdirAll(canonical, 0750); err != nil {
				return fmt.Errorf("%s: cannot create directory %s: %w", name, canonical, err)
			}
		} else {
			return fmt.Errorf("%s: cannot access %s: %w", name, canonical, statErr)
		}
	} else if !info.IsDir() {
		return fmt.Errorf("%s: not a directory: %s", name, canonical)
	}

	if err := unix.Access(canonical, unix.W_OK); err != nil {
		return fmt.Errorf("%s: not writable: %s", name, canonical)
	}
	return nil
}

func validateExecutable(path, name string) error {
	canonical, err := canonicalizePath(path)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	info, err := os.Stat(canonical)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s: file not found: %s", name, canonical)
		}
		return fmt.Errorf("%s: cannot access: %w", name, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s: is a directory, not executable: %s", name, canonical)
	}
	if info.Mode()&0111 == 0 {
		return fmt.Errorf("%s: not executable: %s", name, canonical)
	}
	return nil
}
