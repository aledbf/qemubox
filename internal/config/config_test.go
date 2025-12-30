package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testVMM = "qemu"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	// Verify paths
	if cfg.Paths.ShareDir != "/usr/share/qemubox" {
		t.Errorf("expected ShareDir /usr/share/qemubox, got %s", cfg.Paths.ShareDir)
	}
	if cfg.Paths.StateDir != "/var/lib/qemubox" {
		t.Errorf("expected StateDir /var/lib/qemubox, got %s", cfg.Paths.StateDir)
	}
	if cfg.Paths.LogDir != "/var/log/qemubox" {
		t.Errorf("expected LogDir /var/log/qemubox, got %s", cfg.Paths.LogDir)
	}

	// Verify runtime
	if cfg.Runtime.VMM != testVMM {
		t.Errorf("expected VMM %s, got %s", testVMM, cfg.Runtime.VMM)
	}

	// Verify CPU hotplug
	if cfg.CPUHotplug.MonitorInterval != "5s" {
		t.Errorf("expected MonitorInterval 5s, got %s", cfg.CPUHotplug.MonitorInterval)
	}
	if cfg.CPUHotplug.ScaleUpThreshold != 80.0 {
		t.Errorf("expected ScaleUpThreshold 80.0, got %.2f", cfg.CPUHotplug.ScaleUpThreshold)
	}

	// Verify memory hotplug
	if cfg.MemHotplug.MonitorInterval != "10s" {
		t.Errorf("expected MonitorInterval 10s, got %s", cfg.MemHotplug.MonitorInterval)
	}
	if cfg.MemHotplug.OOMSafetyMarginMB != 128 {
		t.Errorf("expected OOMSafetyMarginMB 128, got %d", cfg.MemHotplug.OOMSafetyMarginMB)
	}
}

func TestLoadFrom_MissingFile(t *testing.T) {
	_, err := LoadFrom("/nonexistent/path/config.json")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}

	// Check that error message mentions the file path
	errMsg := err.Error()
	if !strings.Contains(errMsg, "/nonexistent/path/config.json") {
		t.Errorf("error should mention config file path, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "config file not found") {
		t.Errorf("error should mention 'config file not found', got: %s", errMsg)
	}
}

func TestLoadFrom_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	// Write invalid JSON
	if err := os.WriteFile(configPath, []byte("{invalid json}"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadFrom(configPath)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}

	t.Logf("Error message: %s", err)
}

func TestLoadFrom_ValidConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	// Create necessary directories and files for validation
	shareDir := filepath.Join(tmpDir, "share")
	kernelDir := filepath.Join(shareDir, "kernel")
	stateDir := filepath.Join(tmpDir, "state")
	logDir := filepath.Join(tmpDir, "log")

	if err := os.MkdirAll(kernelDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(logDir, 0750); err != nil {
		t.Fatal(err)
	}

	// Create dummy kernel and initrd
	kernelPath := filepath.Join(kernelDir, "qemubox-kernel-x86_64")
	initrdPath := filepath.Join(kernelDir, "qemubox-initrd")
	if err := os.WriteFile(kernelPath, []byte("dummy"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initrdPath, []byte("dummy"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Paths: PathsConfig{
			ShareDir: shareDir,
			StateDir: stateDir,
			LogDir:   logDir,
		},
		Runtime: RuntimeConfig{
			VMM: testVMM,
		},
		CPUHotplug: CPUHotplugConfig{
			MonitorInterval:      "10s",
			ScaleUpCooldown:      "20s",
			ScaleDownCooldown:    "40s",
			ScaleUpThreshold:     85.0,
			ScaleDownThreshold:   40.0,
			ScaleUpThrottleLimit: 10.0,
			ScaleUpStability:     3,
			ScaleDownStability:   5,
			EnableScaleDown:      false,
		},
		MemHotplug: MemHotplugConfig{
			MonitorInterval:    "15s",
			ScaleUpCooldown:    "45s",
			ScaleDownCooldown:  "90s",
			ScaleUpThreshold:   90.0,
			ScaleDownThreshold: 50.0,
			OOMSafetyMarginMB:  256,
			IncrementSizeMB:    256,
			ScaleUpStability:   2,
			ScaleDownStability: 4,
			EnableScaleDown:    true,
		},
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadFrom(configPath)
	if err != nil {
		t.Fatalf("failed to load valid config: %v", err)
	}

	if loaded.CPUHotplug.ScaleUpThreshold != 85.0 {
		t.Errorf("expected ScaleUpThreshold 85.0, got %.2f", loaded.CPUHotplug.ScaleUpThreshold)
	}
}

func TestApplyDefaults(t *testing.T) {
	// Create config with some empty fields
	cfg := &Config{
		Paths: PathsConfig{
			ShareDir: "/custom/share",
			// StateDir and LogDir empty - should be filled with defaults
		},
		Runtime: RuntimeConfig{
			// VMM empty - should be filled with default
		},
	}

	cfg.applyDefaults()

	if cfg.Paths.ShareDir != "/custom/share" {
		t.Errorf("expected custom ShareDir to be preserved, got %s", cfg.Paths.ShareDir)
	}

	if cfg.Paths.StateDir != "/var/lib/qemubox" {
		t.Errorf("expected default StateDir, got %s", cfg.Paths.StateDir)
	}

	if cfg.Paths.LogDir != "/var/log/qemubox" {
		t.Errorf("expected default LogDir, got %s", cfg.Paths.LogDir)
	}

	if cfg.Runtime.VMM != testVMM {
		t.Errorf("expected default VMM %s, got %s", testVMM, cfg.Runtime.VMM)
	}

	if cfg.CPUHotplug.MonitorInterval != "5s" {
		t.Errorf("expected default CPU MonitorInterval, got %s", cfg.CPUHotplug.MonitorInterval)
	}

	if cfg.MemHotplug.IncrementSizeMB != 128 {
		t.Errorf("expected default IncrementSizeMB, got %d", cfg.MemHotplug.IncrementSizeMB)
	}
}

func TestValidate_InvalidVMM(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Runtime.VMM = "firecracker" // Not supported

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for invalid VMM")
	}

	t.Logf("Error message: %s", err)
}

func TestGet_Singleton(t *testing.T) {
	// Get() should return the same instance on multiple calls (singleton pattern)
	// This test verifies the sync.Once behavior

	// Note: We can't reliably test Get() in isolation because it uses a global
	// sync.Once that can't be reset between tests. However, we can verify that
	// multiple calls within the same test return the same instance.

	cfg1, err1 := Get()
	cfg2, err2 := Get()

	// Both calls should return the same error state
	if (err1 == nil) != (err2 == nil) {
		t.Fatalf("Get() returned different error states: err1=%v, err2=%v", err1, err2)
	}

	// If no error, verify same instance (pointer equality)
	if err1 == nil && err2 == nil {
		if cfg1 != cfg2 {
			t.Errorf("Get() returned different instances: want same pointer, got cfg1=%p cfg2=%p", cfg1, cfg2)
		}
	}

	// Call again to ensure sync.Once doesn't run multiple times
	cfg3, err3 := Get()
	if (err1 == nil) != (err3 == nil) {
		t.Fatalf("Get() returned different error states on third call: err1=%v, err3=%v", err1, err3)
	}

	if err1 == nil && err3 == nil {
		if cfg1 != cfg3 {
			t.Errorf("Get() returned different instance on third call: want same pointer, got cfg1=%p cfg3=%p", cfg1, cfg3)
		}
	}
}

func TestValidate_InvalidThresholds(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*Config)
	}{
		{
			name: "CPU scale_up_threshold too high",
			setupFunc: func(c *Config) {
				c.CPUHotplug.ScaleUpThreshold = 150.0
			},
		},
		{
			name: "CPU scale_up_threshold too low",
			setupFunc: func(c *Config) {
				c.CPUHotplug.ScaleUpThreshold = 0
			},
		},
		{
			name: "Memory scale_down_threshold too high",
			setupFunc: func(c *Config) {
				c.MemHotplug.ScaleDownThreshold = 101.0
			},
		},
		{
			name: "Invalid monitor interval",
			setupFunc: func(c *Config) {
				c.CPUHotplug.MonitorInterval = "invalid"
			},
		},
		{
			name: "Non-aligned increment size",
			setupFunc: func(c *Config) {
				c.MemHotplug.IncrementSizeMB = 100 // Not 128-aligned
			},
		},
		{
			name: "Zero stability counter",
			setupFunc: func(c *Config) {
				c.CPUHotplug.ScaleUpStability = 0
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.setupFunc(cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected validation error for %s", tt.name)
			}

			t.Logf("Error message: %s", err)
		})
	}
}
