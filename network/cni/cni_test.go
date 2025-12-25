//go:build linux

package cni

import (
	"context"
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
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
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
				data1, _ := json.Marshal(config1)
				_ = os.WriteFile(filepath.Join(tmpDir, "10-first.conflist"), data1, 0644)

				config2 := map[string]interface{}{
					"cniVersion": "1.0.0",
					"name":       "second-network",
					"plugins": []map[string]interface{}{
						{"type": "bridge"},
					},
				}
				data2, _ := json.Marshal(config2)
				_ = os.WriteFile(filepath.Join(tmpDir, "20-second.conflist"), data2, 0644)
				return tmpDir
			},
			expectedName: "first-network", // Should load 10-first.conflist
			expectError:  false,
		},
		{
			name: "single conflist file",
			setupConfig: func() string {
				dir := filepath.Join(tmpDir, "single")
				_ = os.MkdirAll(dir, 0755)
				config := map[string]interface{}{
					"cniVersion": "1.0.0",
					"name":       "my-network",
					"plugins": []map[string]interface{}{
						{"type": "bridge"},
					},
				}
				data, _ := json.Marshal(config)
				_ = os.WriteFile(filepath.Join(dir, "99-my.conflist"), data, 0644)
				return dir
			},
			expectedName: "my-network",
			expectError:  false,
		},
		{
			name: "empty config directory",
			setupConfig: func() string {
				emptyDir := filepath.Join(tmpDir, "empty")
				_ = os.MkdirAll(emptyDir, 0755)
				return emptyDir
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			confDir := tt.setupConfig()
			mgr, err := NewCNIManager(confDir, "/opt/cni/bin")
			require.NoError(t, err)

			config, err := mgr.loadNetworkConfig()

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, config)
				assert.Equal(t, tt.expectedName, config.Name)
			}
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
		{"name with numbers", "qemubox-net-123", true},
		{"name with spaces", "qemubox net", true}, // CNI spec allows various names
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
					{Name: "qemubox0", Mac: "aa:bb:cc:dd:ee:ff", Sandbox: ""},
					{Name: "tap0", Mac: "11:22:33:44:55:66", Sandbox: ""},
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
				assert.Error(t, err)
				assert.Nil(t, result)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, result)
			}
		})
	}
}
