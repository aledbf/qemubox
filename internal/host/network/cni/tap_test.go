//go:build linux

package cni

import (
	"testing"

	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractTAPDevice(t *testing.T) {
	tests := []struct {
		name          string
		result        *current.Result
		expectedTAP   string
		expectError   bool
		errorContains string
	}{
		{
			name: "tap device with tap prefix",
			result: &current.Result{
				Interfaces: []*current.Interface{
					{Name: "qemubox0", Mac: "aa:bb:cc:dd:ee:ff", Sandbox: ""},
					{Name: "tap123", Mac: "11:22:33:44:55:66", Sandbox: "/var/run/netns/test"},
				},
			},
			expectedTAP: "tap123",
		},
		{
			name: "tap device with hex suffix",
			result: &current.Result{
				Interfaces: []*current.Interface{
					{Name: "tapABC123", Mac: "11:22:33:44:55:66", Sandbox: "/var/run/netns/test"},
				},
			},
			expectedTAP: "tapABC123",
		},
		{
			name: "multiple interfaces returns first tap",
			result: &current.Result{
				Interfaces: []*current.Interface{
					{Name: "qemubox0", Sandbox: ""},
					{Name: "tap999", Sandbox: "/var/run/netns/test"},
					{Name: "veth0", Sandbox: "/var/run/netns/test"},
				},
			},
			expectedTAP: "tap999",
		},
		{
			name: "tap device in sandbox",
			result: &current.Result{
				Interfaces: []*current.Interface{
					{Name: "tap456", Sandbox: "/var/run/netns/test"},
				},
			},
			expectedTAP: "tap456",
		},
		{
			name: "multiple tap devices returns first",
			result: &current.Result{
				Interfaces: []*current.Interface{
					{Name: "qemubox0", Sandbox: ""},
					{Name: "tap111", Sandbox: "/var/run/netns/test"},
					{Name: "tap222", Sandbox: "/var/run/netns/test"},
					{Name: "tap333", Sandbox: "/var/run/netns/test"},
				},
			},
			expectedTAP: "tap111",
		},
		{
			name: "case sensitive - returns lowercase tap",
			result: &current.Result{
				Interfaces: []*current.Interface{
					{Name: "TAP123", Sandbox: "/var/run/netns/test"},
					{Name: "Tap456", Sandbox: "/var/run/netns/test"},
					{Name: "tap789", Sandbox: "/var/run/netns/test"},
				},
			},
			expectedTAP: "tap789",
		},
		{
			name:          "nil result",
			result:        nil,
			expectError:   true,
			errorContains: "nil",
		},
		{
			name: "no interfaces",
			result: &current.Result{
				Interfaces: []*current.Interface{},
			},
			expectError:   true,
			errorContains: "no TAP device found",
		},
		{
			name: "only veth in sandbox",
			result: &current.Result{
				Interfaces: []*current.Interface{
					{Name: "veth123", Sandbox: "/var/run/netns/test"},
				},
			},
			expectError:   true,
			errorContains: "no TAP device found",
		},
		{
			name: "only veth interfaces",
			result: &current.Result{
				Interfaces: []*current.Interface{
					{Name: "veth0", Sandbox: "/var/run/netns/test"},
					{Name: "veth1", Sandbox: "/var/run/netns/test"},
				},
			},
			expectError:   true,
			errorContains: "no TAP device found",
		},
		{
			name: "bridge interface only",
			result: &current.Result{
				Interfaces: []*current.Interface{
					{Name: "qemubox0", Sandbox: ""},
				},
			},
			expectError:   true,
			errorContains: "no TAP device found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tapDevice, err := ExtractTAPDevice(tt.result)

			if tt.expectError {
				require.Error(t, err)
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains)
				}
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectedTAP, tapDevice)
			}
		})
	}
}
