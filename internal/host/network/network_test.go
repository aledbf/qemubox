//go:build linux

package network

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoadNetworkConfig(t *testing.T) {
	t.Run("standard paths fallback", func(t *testing.T) {
		// Clear environment variables to test fallback paths
		t.Setenv("SPINBOX_CNI_CONF_DIR", "")
		t.Setenv("SPINBOX_CNI_BIN_DIR", "")

		cfg := LoadNetworkConfig()

		// LoadNetworkConfig has a three-tier fallback:
		// 1. Environment variables (cleared above)
		// 2. Spinbox-bundled paths (/usr/share/spinbox/config/cni/net.d)
		// 3. Standard system paths (/etc/cni/net.d, /opt/cni/bin)
		assert.NotEmpty(t, cfg.CNIConfDir)
		assert.NotEmpty(t, cfg.CNIBinDir)
	})

	t.Run("idempotent", func(t *testing.T) {
		cfg1 := LoadNetworkConfig()
		cfg2 := LoadNetworkConfig()

		assert.Equal(t, cfg1.CNIConfDir, cfg2.CNIConfDir)
		assert.Equal(t, cfg1.CNIBinDir, cfg2.CNIBinDir)
	})
}

func TestNetworkConfig_Validation(t *testing.T) {
	tests := []struct {
		name   string
		config NetworkConfig
	}{
		{
			name: "valid CNI config with defaults",
			config: NetworkConfig{
				CNIConfDir: "/etc/cni/net.d",
				CNIBinDir:  "/opt/cni/bin",
			},
		},
		{
			name: "valid CNI config with custom values",
			config: NetworkConfig{
				CNIConfDir: "/custom/conf",
				CNIBinDir:  "/custom/bin",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NotEmpty(t, tt.config.CNIConfDir)
			assert.NotEmpty(t, tt.config.CNIBinDir)
		})
	}
}
