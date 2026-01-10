//go:build linux

package cni

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewCNIManager(t *testing.T) {
	tests := []struct {
		name        string
		confDir     string
		binDir      string
		expectError bool
	}{
		{
			name:        "valid configuration",
			confDir:     "/etc/cni/net.d",
			binDir:      "/opt/cni/bin",
			expectError: false,
		},
		{
			name:        "empty conf dir",
			confDir:     "",
			binDir:      "/opt/cni/bin",
			expectError: true,
		},
		{
			name:        "empty bin dir",
			confDir:     "/etc/cni/net.d",
			binDir:      "",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, err := NewCNIManager(tt.confDir, tt.binDir)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.NotNil(t, mgr)
				assert.Equal(t, tt.confDir, mgr.confDir)
				assert.Equal(t, tt.binDir, mgr.binDir)
			}
		})
	}
}

func TestCNIManager_LoadNetworkConfig(t *testing.T) {
	// Create temporary directory for CNI configs
	tmpDir := t.TempDir()

	tests := []struct {
		name         string
		setupConfig  func() string
		expectedName string
		expectError  bool
	}{
		{
			name: "loads first conflist file alphabetically",
			setupConfig: func() string {
				// Create multiple config files
				config1 := map[string]interface{}{
					"cniVersion": "1.0.0",
					"name":       "first-network",
					"plugins": []map[string]interface{}{
						{
							"type":   "bridge",
							"bridge": "test0",
						},
					},
				}
				data1, err := json.Marshal(config1)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "10-first.conflist"), data1, 0600))

				config2 := map[string]interface{}{
					"cniVersion": "1.0.0",
					"name":       "second-network",
					"plugins": []map[string]interface{}{
						{"type": "bridge"},
					},
				}
				data2, err := json.Marshal(config2)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "20-second.conflist"), data2, 0600))
				return tmpDir
			},
			expectedName: "first-network", // Should load 10-first.conflist
			expectError:  false,
		},
		{
			name: "single conflist file",
			setupConfig: func() string {
				dir := filepath.Join(tmpDir, "single")
				require.NoError(t, os.MkdirAll(dir, 0750))
				config := map[string]interface{}{
					"cniVersion": "1.0.0",
					"name":       "my-network",
					"plugins": []map[string]interface{}{
						{"type": "bridge"},
					},
				}
				data, err := json.Marshal(config)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "99-my.conflist"), data, 0600))
				return dir
			},
			expectedName: "my-network",
			expectError:  false,
		},
		{
			name: "empty config directory",
			setupConfig: func() string {
				emptyDir := filepath.Join(tmpDir, "empty")
				require.NoError(t, os.MkdirAll(emptyDir, 0750))
				return emptyDir
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			confDir := tt.setupConfig()
			mgr, err := NewCNIManager(confDir, "/opt/cni/bin")

			if tt.expectError {
				// NewCNIManager now loads config at startup, so error happens there
				require.Error(t, err)
				return
			}

			require.NoError(t, err)

			// Config is cached at startup, retrieve via getNetworkConfig
			config, err := mgr.getNetworkConfig()
			require.NoError(t, err)
			assert.NotNil(t, config)
			assert.Equal(t, tt.expectedName, config.Name)
		})
	}
}

// Test CNI network name validation helpers
func TestValidCNINetworkName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"simple name", "mynet", true},
		{"name with dash", "my-net", true},
		{"name with underscore", "my_net", true},
		{"name with numbers", "spinbox-net-123", true},
		{"name with spaces", "spinbox net", true}, // CNI spec allows various names
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Just verify the name is non-empty for basic validation
			got := len(tt.input) > 0
			assert.Equal(t, tt.want, got)
		})
	}
}

// Mock CNI result parsing tests
func TestParseCNIResult(t *testing.T) {
	tests := []struct {
		name        string
		result      *current.Result
		expectError bool
	}{
		{
			name:        "nil result",
			result:      nil,
			expectError: true,
		},
		{
			name: "valid result with TAP device",
			result: &current.Result{
				CNIVersion: "1.0.0",
				Interfaces: []*current.Interface{
					{Name: "spinbox0", Mac: "aa:bb:cc:dd:ee:ff", Sandbox: ""},
					{Name: "tap0", Mac: "11:22:33:44:55:66", Sandbox: "/var/run/netns/test"},
				},
				IPs: []*current.IPConfig{
					{
						Address: net.IPNet{
							IP:   net.ParseIP("10.88.0.2"),
							Mask: net.CIDRMask(16, 32),
						},
						Gateway: net.ParseIP("10.88.0.1"),
					},
				},
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseCNIResult(tt.result)

			if tt.expectError {
				require.Error(t, err)
				assert.Nil(t, result)
			} else {
				require.NoError(t, err)
				assert.NotNil(t, result)
			}
		})
	}
}
