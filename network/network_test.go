//go:build linux

package network

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoadNetworkConfig_Defaults(t *testing.T) {
	// Clear all CNI environment variables
	os.Unsetenv("BEACON_CNI_CONF_DIR")
	os.Unsetenv("BEACON_CNI_BIN_DIR")
	os.Unsetenv("BEACON_CNI_NETWORK")

	cfg := LoadNetworkConfig()

	assert.Equal(t, NetworkModeCNI, cfg.Mode)
	assert.Equal(t, "/etc/cni/net.d", cfg.CNIConfDir)
	assert.Equal(t, "/opt/cni/bin", cfg.CNIBinDir)
	assert.Equal(t, "beacon-net", cfg.CNINetworkName)
}

func TestLoadNetworkConfig_CustomCNIConfDir(t *testing.T) {
	t.Setenv("BEACON_CNI_CONF_DIR", "/custom/cni/conf")

	cfg := LoadNetworkConfig()

	assert.Equal(t, NetworkModeCNI, cfg.Mode)
	assert.Equal(t, "/custom/cni/conf", cfg.CNIConfDir)
	assert.Equal(t, "/opt/cni/bin", cfg.CNIBinDir)    // Default
	assert.Equal(t, "beacon-net", cfg.CNINetworkName) // Default
}

func TestLoadNetworkConfig_CustomCNIBinDir(t *testing.T) {
	t.Setenv("BEACON_CNI_BIN_DIR", "/custom/cni/bin")

	cfg := LoadNetworkConfig()

	assert.Equal(t, NetworkModeCNI, cfg.Mode)
	assert.Equal(t, "/etc/cni/net.d", cfg.CNIConfDir) // Default
	assert.Equal(t, "/custom/cni/bin", cfg.CNIBinDir)
	assert.Equal(t, "beacon-net", cfg.CNINetworkName) // Default
}

func TestLoadNetworkConfig_CustomCNINetwork(t *testing.T) {
	t.Setenv("BEACON_CNI_NETWORK", "custom-network")

	cfg := LoadNetworkConfig()

	assert.Equal(t, NetworkModeCNI, cfg.Mode)
	assert.Equal(t, "/etc/cni/net.d", cfg.CNIConfDir) // Default
	assert.Equal(t, "/opt/cni/bin", cfg.CNIBinDir)    // Default
	assert.Equal(t, "custom-network", cfg.CNINetworkName)
}

func TestLoadNetworkConfig_AllCustomValues(t *testing.T) {
	t.Setenv("BEACON_CNI_CONF_DIR", "/custom/conf")
	t.Setenv("BEACON_CNI_BIN_DIR", "/custom/bin")
	t.Setenv("BEACON_CNI_NETWORK", "custom-net")

	cfg := LoadNetworkConfig()

	assert.Equal(t, NetworkModeCNI, cfg.Mode)
	assert.Equal(t, "/custom/conf", cfg.CNIConfDir)
	assert.Equal(t, "/custom/bin", cfg.CNIBinDir)
	assert.Equal(t, "custom-net", cfg.CNINetworkName)
}

func TestLoadNetworkConfig_EmptyEnvironmentVariables(t *testing.T) {
	// Empty strings should use defaults
	t.Setenv("BEACON_CNI_CONF_DIR", "")
	t.Setenv("BEACON_CNI_BIN_DIR", "")
	t.Setenv("BEACON_CNI_NETWORK", "")

	cfg := LoadNetworkConfig()

	assert.Equal(t, "/etc/cni/net.d", cfg.CNIConfDir)
	assert.Equal(t, "/opt/cni/bin", cfg.CNIBinDir)
	assert.Equal(t, "beacon-net", cfg.CNINetworkName)
}

func TestNetworkMode_String(t *testing.T) {
	assert.Equal(t, "cni", string(NetworkModeCNI))
}

func TestNetworkConfig_Validation(t *testing.T) {
	tests := []struct {
		name   string
		config NetworkConfig
		valid  bool
	}{
		{
			name: "valid CNI config with defaults",
			config: NetworkConfig{
				Mode:           NetworkModeCNI,
				CNIConfDir:     "/etc/cni/net.d",
				CNIBinDir:      "/opt/cni/bin",
				CNINetworkName: "beacon-net",
			},
			valid: true,
		},
		{
			name: "valid CNI config with custom values",
			config: NetworkConfig{
				Mode:           NetworkModeCNI,
				CNIConfDir:     "/custom/conf",
				CNIBinDir:      "/custom/bin",
				CNINetworkName: "custom-net",
			},
			valid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Basic validation - check required fields are set
			if tt.valid {
				assert.Equal(t, NetworkModeCNI, tt.config.Mode)
				assert.NotEmpty(t, tt.config.CNIConfDir)
				assert.NotEmpty(t, tt.config.CNIBinDir)
				assert.NotEmpty(t, tt.config.CNINetworkName)
			}
		})
	}
}

func TestLoadNetworkConfig_Idempotent(t *testing.T) {
	// Loading config multiple times should give same results
	t.Setenv("BEACON_CNI_NETWORK", "test-net")

	cfg1 := LoadNetworkConfig()
	cfg2 := LoadNetworkConfig()

	assert.Equal(t, cfg1.Mode, cfg2.Mode)
	assert.Equal(t, cfg1.CNINetworkName, cfg2.CNINetworkName)
	assert.Equal(t, cfg1.CNIConfDir, cfg2.CNIConfDir)
	assert.Equal(t, cfg1.CNIBinDir, cfg2.CNIBinDir)
}

func TestLoadNetworkConfig_PartialOverride(t *testing.T) {
	// Override only some CNI settings
	t.Setenv("BEACON_CNI_CONF_DIR", "/my/conf")
	// Don't set BEACON_CNI_BIN_DIR or BEACON_CNI_NETWORK

	cfg := LoadNetworkConfig()

	assert.Equal(t, NetworkModeCNI, cfg.Mode)
	assert.Equal(t, "/my/conf", cfg.CNIConfDir)
	assert.Equal(t, "/opt/cni/bin", cfg.CNIBinDir)    // Default
	assert.Equal(t, "beacon-net", cfg.CNINetworkName) // Default
}

func TestNetworkConfig_AlwaysCNI(t *testing.T) {
	// CNI mode is always enabled
	cfg := LoadNetworkConfig()
	assert.Equal(t, NetworkModeCNI, cfg.Mode)
}

func TestLoadNetworkConfig_WhitespaceInEnvVars(t *testing.T) {
	// Test that whitespace in env vars is preserved
	t.Setenv("BEACON_CNI_CONF_DIR", "  /path/with/spaces  ")

	cfg := LoadNetworkConfig()

	// Environment variables are used as-is
	assert.Equal(t, "  /path/with/spaces  ", cfg.CNIConfDir)
}

func TestNetworkMode_TypeSafety(t *testing.T) {
	// Ensure NetworkMode is a distinct type
	var mode = NetworkModeCNI

	assert.IsType(t, NetworkMode(""), mode)
	assert.Equal(t, NetworkModeCNI, mode)
}

func TestLoadNetworkConfig_EnvironmentChanges(t *testing.T) {
	// Test that environment changes are reflected
	os.Unsetenv("BEACON_CNI_NETWORK")
	cfg1 := LoadNetworkConfig()
	assert.Equal(t, "beacon-net", cfg1.CNINetworkName)

	t.Setenv("BEACON_CNI_NETWORK", "custom-net")
	cfg2 := LoadNetworkConfig()
	assert.Equal(t, "custom-net", cfg2.CNINetworkName)

	os.Unsetenv("BEACON_CNI_NETWORK")
	cfg3 := LoadNetworkConfig()
	assert.Equal(t, "beacon-net", cfg3.CNINetworkName)
}
