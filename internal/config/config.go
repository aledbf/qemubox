// Package config provides centralized configuration management for spinbox.
// All configuration is loaded from a JSON file at /etc/spinbox/config.json
// (overridable via SPINBOX_CONFIG environment variable).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	// DefaultConfigPath is the default location for the config file
	DefaultConfigPath = "/etc/spinbox/config.json"

	// ConfigEnvVar is the environment variable to override config file location
	ConfigEnvVar = "SPINBOX_CONFIG"
)

// Config is the root configuration structure
type Config struct {
	Paths      PathsConfig      `json:"paths"`
	Runtime    RuntimeConfig    `json:"runtime"`
	Timeouts   TimeoutsConfig   `json:"timeouts"`
	CPUHotplug CPUHotplugConfig `json:"cpu_hotplug"`
	MemHotplug MemHotplugConfig `json:"memory_hotplug"`
}

// PathsConfig defines filesystem paths for spinbox components
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

// TimeoutsConfig defines timeout durations for various lifecycle operations.
// All values are duration strings (e.g., "5s", "2m", "500ms").
type TimeoutsConfig struct {
	VMStart         string `json:"vm_start"`          // VM boot timeout (default: 10s)
	DeviceDetection string `json:"device_detection"`  // Guest device detection timeout (default: 5s)
	ShutdownGrace   string `json:"shutdown_grace"`    // Grace period before SIGKILL (default: 2s)
	EventReconnect  string `json:"event_reconnect"`   // Event stream reconnection timeout (default: 2s)
	TaskClientRetry string `json:"task_client_retry"` // Vsock dial retry timeout (default: 1s)
	IOWait          string `json:"io_wait"`           // I/O forwarder completion timeout (default: 30s)
	QMPCommand      string `json:"qmp_command"`       // QMP command timeout (default: 5s)
}

// Duration parses and returns a timeout duration by name.
// Panics if the field doesn't exist or the value is invalid (should be caught by validation).
func (t *TimeoutsConfig) Duration(name string) time.Duration {
	var s string
	switch name {
	case "vm_start":
		s = t.VMStart
	case "device_detection":
		s = t.DeviceDetection
	case "shutdown_grace":
		s = t.ShutdownGrace
	case "event_reconnect":
		s = t.EventReconnect
	case "task_client_retry":
		s = t.TaskClientRetry
	case "io_wait":
		s = t.IOWait
	case "qmp_command":
		s = t.QMPCommand
	default:
		panic(fmt.Sprintf("unknown timeout field: %s", name))
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(fmt.Sprintf("invalid duration for %s: %s", name, s))
	}
	return d
}

// HotplugConfig defines common hotplug controller settings (used for CPU)
type HotplugConfig struct {
	MonitorInterval    string  `json:"monitor_interval"`    // Monitoring interval (e.g., "5s")
	ScaleUpCooldown    string  `json:"scale_up_cooldown"`   // Cooldown after scale-up
	ScaleDownCooldown  string  `json:"scale_down_cooldown"` // Cooldown after scale-down
	ScaleUpThreshold   float64 `json:"scale_up_threshold"`  // Usage % to trigger scale-up
	ScaleDownThreshold float64 `json:"scale_down_threshold"`
	ScaleUpStability   int     `json:"scale_up_stability"`   // Consecutive readings before scale-up
	ScaleDownStability int     `json:"scale_down_stability"` // Consecutive readings before scale-down
	EnableScaleDown    bool    `json:"enable_scale_down"`    // Allow scaling down
}

// MemHotplugConfig extends HotplugConfig with memory-specific settings
type MemHotplugConfig struct {
	HotplugConfig
	OOMSafetyMarginMB int64 `json:"oom_safety_margin_mb"` // Keep this much memory free
	IncrementSizeMB   int64 `json:"increment_size_mb"`    // Memory increment (must be 128MB-aligned)
}

// CPUHotplugConfig extends HotplugConfig with CPU-specific settings
type CPUHotplugConfig struct {
	HotplugConfig
	ScaleUpThrottleLimit float64 `json:"scale_up_throttle_limit"` // Don't scale up if throttling exceeds this %
}

var (
	globalConfig *Config
	configOnce   sync.Once
	configMu     sync.Mutex
	errConfig    error
)

// defaults
var defaultConfig = Config{
	Paths: PathsConfig{
		ShareDir: "/usr/share/spinbox",
		StateDir: "/var/lib/spinbox",
		LogDir:   "/var/log/spinbox",
	},
	Runtime: RuntimeConfig{
		VMM: "qemu",
	},
	Timeouts: TimeoutsConfig{
		VMStart:         "10s",
		DeviceDetection: "5s",
		ShutdownGrace:   "2s",
		EventReconnect:  "2s",
		TaskClientRetry: "1s",
		IOWait:          "30s",
		QMPCommand:      "5s",
	},
	CPUHotplug: CPUHotplugConfig{
		HotplugConfig: HotplugConfig{
			MonitorInterval:    "5s",
			ScaleUpCooldown:    "10s",
			ScaleDownCooldown:  "30s",
			ScaleUpThreshold:   80.0,
			ScaleDownThreshold: 50.0,
			ScaleUpStability:   2,
			ScaleDownStability: 6,
			EnableScaleDown:    true,
		},
		ScaleUpThrottleLimit: 5.0,
	},
	MemHotplug: MemHotplugConfig{
		HotplugConfig: HotplugConfig{
			MonitorInterval:    "10s",
			ScaleUpCooldown:    "30s",
			ScaleDownCooldown:  "60s",
			ScaleUpThreshold:   85.0,
			ScaleDownThreshold: 60.0,
			ScaleUpStability:   3,
			ScaleDownStability: 6,
			EnableScaleDown:    false,
		},
		OOMSafetyMarginMB: 128,
		IncrementSizeMB:   128,
	},
}

// Reset clears the cached global config, forcing the next Get() call to reload.
// This is intended for testing only.
func Reset() {
	configMu.Lock()
	defer configMu.Unlock()
	globalConfig = nil
	errConfig = nil
	configOnce = sync.Once{}
}

// Get returns the global config, loading it on first call.
func Get() (*Config, error) {
	configOnce.Do(func() {
		globalConfig, errConfig = Load()
	})
	return globalConfig, errConfig
}

// Load loads configuration from SPINBOX_CONFIG env var or /etc/spinbox/config.json.
func Load() (*Config, error) {
	configPath := os.Getenv(ConfigEnvVar)
	if configPath == "" {
		configPath = DefaultConfigPath
	}
	return LoadFrom(configPath)
}

// LoadFrom loads configuration from a specific path.
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

	cfg.applyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration in %s: %w", path, err)
	}

	return &cfg, nil
}

// DefaultConfig returns a copy of the default configuration.
func DefaultConfig() *Config {
	cfg := defaultConfig // copy
	cfg.MemHotplug.HotplugConfig = defaultConfig.MemHotplug.HotplugConfig
	return &cfg
}

// setDefault sets *ptr to dflt if *ptr is the zero value
func setDefault[T comparable](ptr *T, dflt T) {
	var zero T
	if *ptr == zero {
		*ptr = dflt
	}
}

func (c *Config) applyDefaults() {
	d := &defaultConfig

	// Paths
	setDefault(&c.Paths.ShareDir, d.Paths.ShareDir)
	setDefault(&c.Paths.StateDir, d.Paths.StateDir)
	setDefault(&c.Paths.LogDir, d.Paths.LogDir)

	// Runtime
	setDefault(&c.Runtime.VMM, d.Runtime.VMM)

	// Timeouts
	setDefault(&c.Timeouts.VMStart, d.Timeouts.VMStart)
	setDefault(&c.Timeouts.DeviceDetection, d.Timeouts.DeviceDetection)
	setDefault(&c.Timeouts.ShutdownGrace, d.Timeouts.ShutdownGrace)
	setDefault(&c.Timeouts.EventReconnect, d.Timeouts.EventReconnect)
	setDefault(&c.Timeouts.TaskClientRetry, d.Timeouts.TaskClientRetry)
	setDefault(&c.Timeouts.IOWait, d.Timeouts.IOWait)
	setDefault(&c.Timeouts.QMPCommand, d.Timeouts.QMPCommand)

	// CPU Hotplug
	applyHotplugDefaults(&c.CPUHotplug.HotplugConfig, &d.CPUHotplug.HotplugConfig)
	setDefault(&c.CPUHotplug.ScaleUpThrottleLimit, d.CPUHotplug.ScaleUpThrottleLimit)

	// Memory Hotplug
	applyHotplugDefaults(&c.MemHotplug.HotplugConfig, &d.MemHotplug.HotplugConfig)
	setDefault(&c.MemHotplug.OOMSafetyMarginMB, d.MemHotplug.OOMSafetyMarginMB)
	setDefault(&c.MemHotplug.IncrementSizeMB, d.MemHotplug.IncrementSizeMB)
}

func applyHotplugDefaults(c, d *HotplugConfig) {
	setDefault(&c.MonitorInterval, d.MonitorInterval)
	setDefault(&c.ScaleUpCooldown, d.ScaleUpCooldown)
	setDefault(&c.ScaleDownCooldown, d.ScaleDownCooldown)
	setDefault(&c.ScaleUpThreshold, d.ScaleUpThreshold)
	setDefault(&c.ScaleDownThreshold, d.ScaleDownThreshold)
	setDefault(&c.ScaleUpStability, d.ScaleUpStability)
	setDefault(&c.ScaleDownStability, d.ScaleDownStability)
}
