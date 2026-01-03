// Package config provides centralized configuration management for qemubox.
// All configuration is loaded from a JSON file at /etc/qemubox/config.json
// (overridable via QEMUBOX_CONFIG environment variable).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

const (
	// DefaultConfigPath is the default location for the config file
	DefaultConfigPath = "/etc/qemubox/config.json"

	// ConfigEnvVar is the environment variable to override config file location
	ConfigEnvVar = "QEMUBOX_CONFIG"
)

// Config is the root configuration structure
type Config struct {
	Paths      PathsConfig      `json:"paths"`
	Runtime    RuntimeConfig    `json:"runtime"`
	CPUHotplug CPUHotplugConfig `json:"cpu_hotplug"`
	MemHotplug MemHotplugConfig `json:"memory_hotplug"`
}

// PathsConfig defines filesystem paths for qemubox components
type PathsConfig struct {
	ShareDir      string `json:"share_dir"`       // Binaries and config directory
	StateDir      string `json:"state_dir"`       // State files directory
	LogDir        string `json:"log_dir"`         // Logs directory
	QEMUPath      string `json:"qemu_path"`       // QEMU binary location (auto-discovered if empty)
	QEMUSharePath string `json:"qemu_share_path"` // QEMU firmware/BIOS directory (auto-discovered if empty)
}

// RuntimeConfig defines runtime behavior settings
type RuntimeConfig struct {
	VMM string `json:"vmm"` // VMM backend (currently only "qemu" supported)
}

// CPUHotplugConfig defines CPU hotplug controller settings
type CPUHotplugConfig struct {
	MonitorInterval      string  `json:"monitor_interval"`        // Monitoring interval (e.g., "5s")
	ScaleUpCooldown      string  `json:"scale_up_cooldown"`       // Cooldown after scale-up
	ScaleDownCooldown    string  `json:"scale_down_cooldown"`     // Cooldown after scale-down
	ScaleUpThreshold     float64 `json:"scale_up_threshold"`      // CPU usage % to trigger scale-up
	ScaleDownThreshold   float64 `json:"scale_down_threshold"`    // CPU usage % to trigger scale-down
	ScaleUpThrottleLimit float64 `json:"scale_up_throttle_limit"` // Don't scale up if throttling exceeds this %
	ScaleUpStability     int     `json:"scale_up_stability"`      // Consecutive high readings before scale-up
	ScaleDownStability   int     `json:"scale_down_stability"`    // Consecutive low readings before scale-down
	EnableScaleDown      bool    `json:"enable_scale_down"`       // Allow removing CPUs
}

// MemHotplugConfig defines memory hotplug controller settings
type MemHotplugConfig struct {
	MonitorInterval    string  `json:"monitor_interval"`     // Monitoring interval (e.g., "10s")
	ScaleUpCooldown    string  `json:"scale_up_cooldown"`    // Cooldown after scale-up
	ScaleDownCooldown  string  `json:"scale_down_cooldown"`  // Cooldown after scale-down
	ScaleUpThreshold   float64 `json:"scale_up_threshold"`   // Memory usage % to trigger scale-up
	ScaleDownThreshold float64 `json:"scale_down_threshold"` // Memory usage % to trigger scale-down
	OOMSafetyMarginMB  int64   `json:"oom_safety_margin_mb"` // Keep this much memory free (OOM protection)
	IncrementSizeMB    int64   `json:"increment_size_mb"`    // Memory increment size (must be 128MB-aligned)
	ScaleUpStability   int     `json:"scale_up_stability"`   // Consecutive high readings before scale-up
	ScaleDownStability int     `json:"scale_down_stability"` // Consecutive low readings before scale-down
	EnableScaleDown    bool    `json:"enable_scale_down"`    // Allow removing memory
}

var (
	globalConfig *Config
	configOnce   sync.Once
	configMu     sync.Mutex
	errConfig    error
)

// Reset clears the cached global config, forcing the next Get() call to reload.
// This is intended for testing only. Callers must ensure no concurrent Get() calls
// are in progress when calling Reset().
func Reset() {
	configMu.Lock()
	defer configMu.Unlock()
	globalConfig = nil
	errConfig = nil
	configOnce = sync.Once{}
}

// Get returns the global config, loading it on first call.
// This is the primary way to access configuration throughout the codebase.
func Get() (*Config, error) {
	configOnce.Do(func() {
		globalConfig, errConfig = Load()
	})
	return globalConfig, errConfig
}

// Load loads configuration from QEMUBOX_CONFIG env var or /etc/qemubox/config.json.
// Missing files return an error for the caller to handle.
func Load() (*Config, error) {
	configPath := os.Getenv(ConfigEnvVar)
	if configPath == "" {
		configPath = DefaultConfigPath
	}

	return LoadFrom(configPath)
}

// LoadFrom loads configuration from a specific path.
// Returns error if file doesn't exist or is invalid.
func LoadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config file not found at %s. Please create a config file (see examples/config.json) or set %s environment variable", path, ConfigEnvVar)
		}
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w (ensure it's valid JSON)", path, err)
	}

	// Apply defaults for empty fields
	cfg.applyDefaults()

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration in %s: %w", path, err)
	}

	return &cfg, nil
}

// DefaultConfig returns the default configuration.
// This is primarily for reference and documentation - production code should use Get().
func DefaultConfig() *Config {
	cfg := &Config{
		Paths: PathsConfig{
			ShareDir:      "/usr/share/qemubox",
			StateDir:      "/var/lib/qemubox",
			LogDir:        "/var/log/qemubox",
			QEMUPath:      "", // Auto-discovered
			QEMUSharePath: "", // Auto-discovered
		},
		Runtime: RuntimeConfig{
			VMM: "qemu",
		},
		CPUHotplug: CPUHotplugConfig{
			MonitorInterval:      "5s",
			ScaleUpCooldown:      "10s",
			ScaleDownCooldown:    "30s",
			ScaleUpThreshold:     80.0,
			ScaleDownThreshold:   50.0,
			ScaleUpThrottleLimit: 5.0,
			ScaleUpStability:     2,
			ScaleDownStability:   6,
			EnableScaleDown:      true,
		},
		MemHotplug: MemHotplugConfig{
			MonitorInterval:    "10s",
			ScaleUpCooldown:    "30s",
			ScaleDownCooldown:  "60s",
			ScaleUpThreshold:   85.0,
			ScaleDownThreshold: 60.0,
			OOMSafetyMarginMB:  128,
			IncrementSizeMB:    128,
			ScaleUpStability:   3,
			ScaleDownStability: 6,
			EnableScaleDown:    false,
		},
	}
	return cfg
}

// applyDefaults fills in default values for any empty fields
func (c *Config) applyDefaults() {
	defaults := DefaultConfig()

	c.applyPathDefaults(defaults)
	c.applyRuntimeDefaults(defaults)
	c.applyCPUHotplugDefaults(defaults)
	c.applyMemHotplugDefaults(defaults)
}

func (c *Config) applyPathDefaults(defaults *Config) {
	// Paths
	if c.Paths.ShareDir == "" {
		c.Paths.ShareDir = defaults.Paths.ShareDir
	}
	if c.Paths.StateDir == "" {
		c.Paths.StateDir = defaults.Paths.StateDir
	}
	if c.Paths.LogDir == "" {
		c.Paths.LogDir = defaults.Paths.LogDir
	}
	// QEMUPath and QEMUSharePath are intentionally left empty for auto-discovery
}

func (c *Config) applyRuntimeDefaults(defaults *Config) {
	// Runtime
	if c.Runtime.VMM == "" {
		c.Runtime.VMM = defaults.Runtime.VMM
	}
}

func (c *Config) applyCPUHotplugDefaults(defaults *Config) {
	// CPU Hotplug
	if c.CPUHotplug.MonitorInterval == "" {
		c.CPUHotplug.MonitorInterval = defaults.CPUHotplug.MonitorInterval
	}
	if c.CPUHotplug.ScaleUpCooldown == "" {
		c.CPUHotplug.ScaleUpCooldown = defaults.CPUHotplug.ScaleUpCooldown
	}
	if c.CPUHotplug.ScaleDownCooldown == "" {
		c.CPUHotplug.ScaleDownCooldown = defaults.CPUHotplug.ScaleDownCooldown
	}
	if c.CPUHotplug.ScaleUpThreshold == 0 {
		c.CPUHotplug.ScaleUpThreshold = defaults.CPUHotplug.ScaleUpThreshold
	}
	if c.CPUHotplug.ScaleDownThreshold == 0 {
		c.CPUHotplug.ScaleDownThreshold = defaults.CPUHotplug.ScaleDownThreshold
	}
	if c.CPUHotplug.ScaleUpThrottleLimit == 0 {
		c.CPUHotplug.ScaleUpThrottleLimit = defaults.CPUHotplug.ScaleUpThrottleLimit
	}
	if c.CPUHotplug.ScaleUpStability == 0 {
		c.CPUHotplug.ScaleUpStability = defaults.CPUHotplug.ScaleUpStability
	}
	if c.CPUHotplug.ScaleDownStability == 0 {
		c.CPUHotplug.ScaleDownStability = defaults.CPUHotplug.ScaleDownStability
	}
}

func (c *Config) applyMemHotplugDefaults(defaults *Config) {
	// Memory Hotplug
	if c.MemHotplug.MonitorInterval == "" {
		c.MemHotplug.MonitorInterval = defaults.MemHotplug.MonitorInterval
	}
	if c.MemHotplug.ScaleUpCooldown == "" {
		c.MemHotplug.ScaleUpCooldown = defaults.MemHotplug.ScaleUpCooldown
	}
	if c.MemHotplug.ScaleDownCooldown == "" {
		c.MemHotplug.ScaleDownCooldown = defaults.MemHotplug.ScaleDownCooldown
	}
	if c.MemHotplug.ScaleUpThreshold == 0 {
		c.MemHotplug.ScaleUpThreshold = defaults.MemHotplug.ScaleUpThreshold
	}
	if c.MemHotplug.ScaleDownThreshold == 0 {
		c.MemHotplug.ScaleDownThreshold = defaults.MemHotplug.ScaleDownThreshold
	}
	if c.MemHotplug.OOMSafetyMarginMB == 0 {
		c.MemHotplug.OOMSafetyMarginMB = defaults.MemHotplug.OOMSafetyMarginMB
	}
	if c.MemHotplug.IncrementSizeMB == 0 {
		c.MemHotplug.IncrementSizeMB = defaults.MemHotplug.IncrementSizeMB
	}
	if c.MemHotplug.ScaleUpStability == 0 {
		c.MemHotplug.ScaleUpStability = defaults.MemHotplug.ScaleUpStability
	}
	if c.MemHotplug.ScaleDownStability == 0 {
		c.MemHotplug.ScaleDownStability = defaults.MemHotplug.ScaleDownStability
	}
}
