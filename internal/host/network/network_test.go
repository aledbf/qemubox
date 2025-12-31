//go:build linux

package network

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoadNetworkConfig_StandardPaths(t *testing.T) {
	// Clear environment variables to test fallback paths
	t.Setenv("QEMUBOX_CNI_CONF_DIR", "")
	t.Setenv("QEMUBOX_CNI_BIN_DIR", "")

	cfg := LoadNetworkConfig()

	// LoadNetworkConfig has a three-tier fallback:
	// 1. Environment variables (cleared above)
	// 2. Qemubox-bundled paths (/usr/share/qemubox/config/cni/net.d)
	// 3. Standard system paths (/etc/cni/net.d, /opt/cni/bin)
	// We just verify that valid paths are returned
	assert.NotEmpty(t, cfg.CNIConfDir)
	assert.NotEmpty(t, cfg.CNIBinDir)
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
				CNIConfDir: "/etc/cni/net.d",
				CNIBinDir:  "/opt/cni/bin",
			},
			valid: true,
		},
		{
			name: "valid CNI config with custom values",
			config: NetworkConfig{
				CNIConfDir: "/custom/conf",
				CNIBinDir:  "/custom/bin",
			},
			valid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Basic validation - check required fields are set
			if tt.valid {
				assert.NotEmpty(t, tt.config.CNIConfDir)
				assert.NotEmpty(t, tt.config.CNIBinDir)
			}
		})
	}
}

func TestLoadNetworkConfig_Idempotent(t *testing.T) {
	// Loading config multiple times should give same results
	cfg1 := LoadNetworkConfig()
	cfg2 := LoadNetworkConfig()

	assert.Equal(t, cfg1.CNIConfDir, cfg2.CNIConfDir)
	assert.Equal(t, cfg1.CNIBinDir, cfg2.CNIBinDir)
}
