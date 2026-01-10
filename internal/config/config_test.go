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

// testConfigEnv holds paths for a test configuration environment.
type testConfigEnv struct {
	configFile string
	shareDir   string
	stateDir   string
	logDir     string
}

// createTestConfigEnv creates a complete test configuration environment in the given directory.
// It creates all required directories and dummy kernel/initrd files, then writes a valid config file.
func createTestConfigEnv(t *testing.T, baseDir string) testConfigEnv {
	t.Helper()

	env := testConfigEnv{
		configFile: filepath.Join(baseDir, "config.json"),
		shareDir:   filepath.Join(baseDir, "share"),
		stateDir:   filepath.Join(baseDir, "state"),
		logDir:     filepath.Join(baseDir, "log"),
	}

	// Create required directories
	if err := os.MkdirAll(filepath.Join(env.shareDir, "kernel"), 0755); err != nil {
		t.Fatalf("failed to create share dir: %v", err)
	}
	if err := os.MkdirAll(env.stateDir, 0755); err != nil {
		t.Fatalf("failed to create state dir: %v", err)
	}
	if err := os.MkdirAll(env.logDir, 0755); err != nil {
		t.Fatalf("failed to create log dir: %v", err)
	}

	// Create dummy kernel and initrd files
	kernelPath := filepath.Join(env.shareDir, "kernel", "spinbox-kernel-x86_64")
	initrdPath := filepath.Join(env.shareDir, "kernel", "spinbox-initrd")
	if err := os.WriteFile(kernelPath, []byte("dummy"), 0644); err != nil {
		t.Fatalf("failed to create dummy kernel: %v", err)
	}
	if err := os.WriteFile(initrdPath, []byte("dummy"), 0644); err != nil {
		t.Fatalf("failed to create dummy initrd: %v", err)
	}

	// Create and write config
	cfg := DefaultConfig()
	cfg.Paths.ShareDir = env.shareDir
	cfg.Paths.StateDir = env.stateDir
	cfg.Paths.LogDir = env.logDir

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("failed to marshal config: %v", err)
	}
	if err := os.WriteFile(env.configFile, data, 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	return env
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	// Verify paths
	if cfg.Paths.ShareDir != "/usr/share/spinbox" {
		t.Errorf("expected ShareDir /usr/share/spinbox, got %s", cfg.Paths.ShareDir)
	}
	if cfg.Paths.StateDir != "/var/lib/spinbox" {
		t.Errorf("expected StateDir /var/lib/spinbox, got %s", cfg.Paths.StateDir)
	}
	if cfg.Paths.LogDir != "/var/log/spinbox" {
		t.Errorf("expected LogDir /var/log/spinbox, got %s", cfg.Paths.LogDir)
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
	kernelPath := filepath.Join(kernelDir, "spinbox-kernel-x86_64")
	initrdPath := filepath.Join(kernelDir, "spinbox-initrd")
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
			HotplugConfig: HotplugConfig{
				MonitorInterval:    "10s",
				ScaleUpCooldown:    "20s",
				ScaleDownCooldown:  "40s",
				ScaleUpThreshold:   85.0,
				ScaleDownThreshold: 40.0,
				ScaleUpStability:   3,
				ScaleDownStability: 5,
				EnableScaleDown:    false,
			},
			ScaleUpThrottleLimit: 10.0,
		},
		MemHotplug: MemHotplugConfig{
			HotplugConfig: HotplugConfig{
				MonitorInterval:    "15s",
				ScaleUpCooldown:    "45s",
				ScaleDownCooldown:  "90s",
				ScaleUpThreshold:   90.0,
				ScaleDownThreshold: 50.0,
				ScaleUpStability:   2,
				ScaleDownStability: 4,
				EnableScaleDown:    true,
			},
			OOMSafetyMarginMB: 256,
			IncrementSizeMB:   256,
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

	if cfg.Paths.StateDir != "/var/lib/spinbox" {
		t.Errorf("expected default StateDir, got %s", cfg.Paths.StateDir)
	}

	if cfg.Paths.LogDir != "/var/log/spinbox" {
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

	// Note: This test does not reset global state; it only verifies that
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

func TestValidate_Comprehensive(t *testing.T) {
	// Create a valid base config for testing
	validConfig := func() *Config {
		cfg := DefaultConfig()
		// Ensure paths exist for validation
		tmpDir := t.TempDir()
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
		kernelPath := filepath.Join(kernelDir, "spinbox-kernel-x86_64")
		initrdPath := filepath.Join(kernelDir, "spinbox-initrd")
		if err := os.WriteFile(kernelPath, []byte("dummy"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(initrdPath, []byte("dummy"), 0600); err != nil {
			t.Fatal(err)
		}

		cfg.Paths.ShareDir = shareDir
		cfg.Paths.StateDir = stateDir
		cfg.Paths.LogDir = logDir

		return cfg
	}

	tests := []struct {
		name      string
		setupFunc func(*Config)
		wantErr   bool
	}{
		// CPU Hotplug validation
		{
			name:    "CPU scale_up_threshold too high",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.CPUHotplug.ScaleUpThreshold = 150.0
			},
		},
		{
			name:    "CPU scale_up_threshold too low",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.CPUHotplug.ScaleUpThreshold = 0
			},
		},
		{
			name:    "CPU scale_down_threshold too high",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.CPUHotplug.ScaleDownThreshold = 101.0
			},
		},
		{
			name:    "CPU scale_down_threshold negative",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.CPUHotplug.ScaleDownThreshold = -10.0
			},
		},
		{
			name:    "CPU throttle_limit negative",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.CPUHotplug.ScaleUpThrottleLimit = -5.0
			},
		},
		{
			name:    "CPU throttle_limit too high",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.CPUHotplug.ScaleUpThrottleLimit = 150.0
			},
		},
		{
			name:    "CPU invalid monitor interval",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.CPUHotplug.MonitorInterval = "not-a-duration"
			},
		},
		{
			name:    "CPU invalid scale_up_cooldown",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.CPUHotplug.ScaleUpCooldown = "5x"
			},
		},
		{
			name:    "CPU invalid scale_down_cooldown",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.CPUHotplug.ScaleDownCooldown = "invalid"
			},
		},
		{
			name:    "CPU zero scale_up_stability",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.CPUHotplug.ScaleUpStability = 0
			},
		},
		{
			name:    "CPU negative scale_up_stability",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.CPUHotplug.ScaleUpStability = -1
			},
		},
		{
			name:    "CPU zero scale_down_stability",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.CPUHotplug.ScaleDownStability = 0
			},
		},

		// Memory Hotplug validation
		{
			name:    "Memory scale_up_threshold too high",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.MemHotplug.ScaleUpThreshold = 105.0
			},
		},
		{
			name:    "Memory scale_up_threshold zero",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.MemHotplug.ScaleUpThreshold = 0
			},
		},
		{
			name:    "Memory scale_down_threshold too high",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.MemHotplug.ScaleDownThreshold = 101.0
			},
		},
		{
			name:    "Memory scale_down_threshold negative",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.MemHotplug.ScaleDownThreshold = -20.0
			},
		},
		{
			name:    "Memory invalid monitor interval",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.MemHotplug.MonitorInterval = "bad-format"
			},
		},
		{
			name:    "Memory invalid scale_up_cooldown",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.MemHotplug.ScaleUpCooldown = "10seconds"
			},
		},
		{
			name:    "Memory invalid scale_down_cooldown",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.MemHotplug.ScaleDownCooldown = "1 minute"
			},
		},
		{
			name:    "Memory non-aligned increment size",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.MemHotplug.IncrementSizeMB = 100 // Not 128-aligned
			},
		},
		{
			name:    "Memory zero increment size",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.MemHotplug.IncrementSizeMB = 0
			},
		},
		{
			name:    "Memory negative increment size",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.MemHotplug.IncrementSizeMB = -128
			},
		},
		{
			name:    "Memory zero OOM safety margin",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.MemHotplug.OOMSafetyMarginMB = 0
			},
		},
		{
			name:    "Memory negative OOM safety margin",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.MemHotplug.OOMSafetyMarginMB = -64
			},
		},
		{
			name:    "Memory zero scale_up_stability",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.MemHotplug.ScaleUpStability = 0
			},
		},
		{
			name:    "Memory zero scale_down_stability",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.MemHotplug.ScaleDownStability = 0
			},
		},

		// Runtime validation
		{
			name:    "Invalid VMM type",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.Runtime.VMM = "firecracker"
			},
		},
		{
			name:    "Empty VMM type",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.Runtime.VMM = ""
			},
		},

		// Paths validation
		{
			name:    "Empty share_dir",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.Paths.ShareDir = ""
			},
		},
		{
			name:    "Empty state_dir",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.Paths.StateDir = ""
			},
		},
		{
			name:    "Empty log_dir",
			wantErr: true,
			setupFunc: func(c *Config) {
				c.Paths.LogDir = ""
			},
		},

		// Valid configurations (should not error)
		{
			name:    "Valid default config",
			wantErr: false,
			setupFunc: func(c *Config) {
				// No changes - use valid config as-is
			},
		},
		{
			name:    "Valid edge case - thresholds at boundaries",
			wantErr: false,
			setupFunc: func(c *Config) {
				c.CPUHotplug.ScaleUpThreshold = 100.0
				c.CPUHotplug.ScaleDownThreshold = 0.1
				c.MemHotplug.ScaleUpThreshold = 100.0
				c.MemHotplug.ScaleDownThreshold = 0.1
			},
		},
		{
			name:    "Valid 128MB-aligned increment",
			wantErr: false,
			setupFunc: func(c *Config) {
				c.MemHotplug.IncrementSizeMB = 256
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.setupFunc(cfg)

			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("expected validation error for %s, got nil", tt.name)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected no error for %s, got: %v", tt.name, err)
			}

			if err != nil {
				t.Logf("Error message: %s", err)
			}
		})
	}
}

func TestReset(t *testing.T) {
	// This test demonstrates that Reset allows testing different
	// configurations in the same test run by resetting the global singleton state

	// Reset any cached config from previous tests
	Reset()

	// Create first config environment
	env1 := createTestConfigEnv(t, t.TempDir())

	// Load first config
	t.Setenv("SPINBOX_CONFIG", env1.configFile)
	loadedCfg1, err := Get()
	if err != nil {
		t.Fatalf("failed to load first config: %v", err)
	}

	if loadedCfg1.Paths.ShareDir != env1.shareDir {
		t.Errorf("first config: expected ShareDir %s, got %s", env1.shareDir, loadedCfg1.Paths.ShareDir)
	}
	if loadedCfg1.Paths.StateDir != env1.stateDir {
		t.Errorf("first config: expected StateDir %s, got %s", env1.stateDir, loadedCfg1.Paths.StateDir)
	}

	// Without Reset, Get() would return the cached first config
	// even after changing SPINBOX_CONFIG. Verify this:
	t.Setenv("SPINBOX_CONFIG", "/this/does/not/exist")
	cachedCfg, _ := Get()
	if cachedCfg.Paths.ShareDir != env1.shareDir {
		t.Error("expected Get() to return cached config without Reset")
	}

	// Now reset and load second config
	Reset()

	// Create second config environment
	env2 := createTestConfigEnv(t, t.TempDir())

	t.Setenv("SPINBOX_CONFIG", env2.configFile)
	loadedCfg2, err := Get()
	if err != nil {
		t.Fatalf("failed to load second config: %v", err)
	}

	if loadedCfg2.Paths.ShareDir != env2.shareDir {
		t.Errorf("second config: expected ShareDir %s, got %s", env2.shareDir, loadedCfg2.Paths.ShareDir)
	}
	if loadedCfg2.Paths.StateDir != env2.stateDir {
		t.Errorf("second config: expected StateDir %s, got %s", env2.stateDir, loadedCfg2.Paths.StateDir)
	}

	// Verify configs are actually different
	if loadedCfg1.Paths.ShareDir == loadedCfg2.Paths.ShareDir {
		t.Error("Reset did not allow loading different config")
	}
	if env1.shareDir == env2.shareDir {
		t.Fatal("test setup error: directories should be different")
	}
}
