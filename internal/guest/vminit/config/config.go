//go:build linux

// Package config provides configuration loading and merging for vminitd.
package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/containerd/containerd/v2/pkg/shutdown"

	"github.com/spin-stack/spinbox/internal/vsock"
)

// ServiceConfig holds the configuration for the vminitd service.
type ServiceConfig struct {
	VSockContextID  int                       `json:"vsock_context_id,omitempty"`
	RPCPort         int                       `json:"rpc_port,omitempty"`
	StreamPort      int                       `json:"stream_port,omitempty"`
	Shutdown        shutdown.Service          `json:"-"`
	Debug           bool                      `json:"debug,omitempty"`
	DisabledPlugins []string                  `json:"disabled_plugins,omitempty"`
	PluginConfigs   map[string]map[string]any `json:"plugin_configs,omitempty"`
}

// LoadFromFile loads configuration from a JSON file and merges it with the provided config.
// Command-line flags take precedence over file configuration.
func LoadFromFile(path string, config *ServiceConfig, setFlags map[string]bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	// Store flag values before unmarshaling
	flagDebug := config.Debug
	flagRPCPort := config.RPCPort
	flagStreamPort := config.StreamPort
	flagVSockContextID := config.VSockContextID

	if err := json.Unmarshal(data, config); err != nil {
		return fmt.Errorf("failed to parse config file: %w", err)
	}

	// Restore flag values that were explicitly set by the user
	// This ensures flags override config file
	if setFlags["debug"] {
		config.Debug = flagDebug
	}
	if setFlags["vsock-rpc-port"] {
		config.RPCPort = flagRPCPort
	}
	if setFlags["vsock-stream-port"] {
		config.StreamPort = flagStreamPort
	}
	if setFlags["vsock-cid"] {
		config.VSockContextID = flagVSockContextID
	}

	return nil
}

// ApplyPluginConfig applies configuration map to a plugin config struct.
//
// This uses JSON marshal/unmarshal as a type-safe conversion mechanism:
//   - Handles type conversions (string to int, etc.) via JSON codec
//   - Validates field types and values during unmarshal
//   - Works with any plugin config struct without reflection complexity
//   - Respects JSON tags for field mapping
//
// Requirements:
//   - pluginConfig must be a non-nil pointer to a struct
//   - configMap keys should match JSON field names/tags in the struct
//
// Type mismatch behavior: If a configMap value cannot be converted to the
// target field type (e.g., "abc" to int), unmarshal will return an error
// with details about the failed field.
//
// Trade-off: Slightly slower than direct assignment, but safer and more maintainable.
func ApplyPluginConfig(pluginConfig any, configMap map[string]any) error {
	if pluginConfig == nil {
		return fmt.Errorf("plugin config is nil")
	}

	// Empty config map is valid - just a no-op
	if len(configMap) == 0 {
		return nil
	}

	// Marshal the config map to JSON
	data, err := json.Marshal(configMap)
	if err != nil {
		return fmt.Errorf("failed to marshal config map to JSON: %w", err)
	}

	// Unmarshal into the plugin config struct
	// Note: json.Unmarshal returns a detailed error if types don't match
	if err := json.Unmarshal(data, pluginConfig); err != nil {
		return fmt.Errorf("failed to apply config to plugin (check field types): %w", err)
	}

	return nil
}

// ParseFlags parses command-line flags and returns the config and set flags map.
func ParseFlags(args []string) (*ServiceConfig, map[string]bool, string, error) {
	var (
		config     ServiceConfig
		configFile string
	)

	fs := flag.NewFlagSet("vminitd", flag.ContinueOnError)
	fs.StringVar(&configFile, "config", "", "Path to configuration file")
	fs.BoolVar(&config.Debug, "debug", false, "Debug log level")
	fs.IntVar(&config.RPCPort, "vsock-rpc-port", vsock.DefaultRPCPort, "vsock port to listen for rpc on")
	fs.IntVar(&config.StreamPort, "vsock-stream-port", vsock.DefaultStreamPort, "vsock port to listen for streams on")
	fs.IntVar(&config.VSockContextID, "vsock-cid", vsock.GuestCID, "vsock context ID for vsock listen")

	if err := fs.Parse(args); err != nil {
		return nil, nil, "", err
	}

	// Track which flags were explicitly set by the user
	setFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		setFlags[f.Name] = true
	})

	return &config, setFlags, configFile, nil
}
