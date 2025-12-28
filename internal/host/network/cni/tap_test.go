//go:build linux

package cni

import (
	"testing"

	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractTAPDevice_Success(t *testing.T) {
	tests := []struct {
		name        string
		result      *current.Result
		expectedTAP string
		expectError bool
	}{
		{
			name: "tap device with tap prefix",
			result: &current.Result{
				Interfaces: []*current.Interface{
					{Name: "qemubox0", Mac: "aa:bb:cc:dd:ee:ff", Sandbox: ""},
					{Name: "tap123", Mac: "11:22:33:44:55:66", Sandbox: ""},
				},
			},
			expectedTAP: "tap123",
			expectError: false,
		},
		{
			name: "tap device with tap prefix and hex suffix",
			result: &current.Result{
				Interfaces: []*current.Interface{
					{Name: "tapABC123", Mac: "11:22:33:44:55:66", Sandbox: ""},
				},
			},
			expectedTAP: "tapABC123",
			expectError: false,
		},
		{
			name: "multiple interfaces, tap is second",
			result: &current.Result{
				Interfaces: []*current.Interface{
					{Name: "qemubox0", Sandbox: ""},
					{Name: "tap999", Sandbox: ""},
					{Name: "veth0", Sandbox: "/var/run/netns/test"},
				},
			},
			expectedTAP: "tap999",
			expectError: false,
		},
		{
			name: "no tap device - only veth in sandbox",
			result: &current.Result{
				Interfaces: []*current.Interface{
					{Name: "veth123", Sandbox: "/var/run/netns/test"},
				},
			},
			expectError: true,
		},
		{
			name: "tap device in sandbox",
			result: &current.Result{
				Interfaces: []*current.Interface{
					{Name: "tap456", Sandbox: "/var/run/netns/test"},
				},
			},
			expectedTAP: "tap456",
			expectError: false,
		},
		{
			name: "no interfaces",
			result: &current.Result{
				Interfaces: []*current.Interface{},
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tapDevice, err := ExtractTAPDevice(tt.result)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectedTAP, tapDevice)
			}
		})
	}
}

func TestExtractTAPDevice_NilResult(t *testing.T) {
	_, err := ExtractTAPDevice(nil)
	require.Error(t, err)
}

func TestExtractTAPDevice_PrefersTCRedirectTapPattern(t *testing.T) {
	// When multiple patterns match, first matching TAP device should be returned
	result := &current.Result{
		Interfaces: []*current.Interface{
			{Name: "tap123", Sandbox: ""},
			{Name: "tap456", Sandbox: ""},
		},
	}

	tapDevice, err := ExtractTAPDevice(result)
	require.NoError(t, err)
	// Should return first matching TAP device
	assert.Equal(t, "tap123", tapDevice)
}

func TestExtractTAPDevice_ErrorMessages(t *testing.T) {
	tests := []struct {
		name          string
		result        *current.Result
		errorContains string
	}{
		{
			name: "no interfaces at all",
			result: &current.Result{
				Interfaces: []*current.Interface{},
			},
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
			errorContains: "no TAP device found",
		},
		{
			name: "bridge interface only",
			result: &current.Result{
				Interfaces: []*current.Interface{
					{Name: "qemubox0", Sandbox: ""},
				},
			},
			errorContains: "no TAP device found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ExtractTAPDevice(tt.result)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errorContains)
		})
	}
}

func TestExtractTAPDevice_MultipleMatches(t *testing.T) {
	// When multiple TAP devices exist, return the first one
	result := &current.Result{
		Interfaces: []*current.Interface{
			{Name: "qemubox0", Sandbox: ""},
			{Name: "tap111", Sandbox: ""},
			{Name: "tap222", Sandbox: ""},
			{Name: "tap333", Sandbox: ""},
		},
	}

	tapDevice, err := ExtractTAPDevice(result)
	require.NoError(t, err)
	assert.Equal(t, "tap111", tapDevice)
}

func TestExtractTAPDevice_CaseSensitive(t *testing.T) {
	// TAP prefix is case-sensitive (must be lowercase "tap")
	result := &current.Result{
		Interfaces: []*current.Interface{
			{Name: "TAP123", Sandbox: ""}, // Wrong case
			{Name: "Tap456", Sandbox: ""}, // Wrong case
			{Name: "tap789", Sandbox: ""}, // Correct
		},
	}

	tapDevice, err := ExtractTAPDevice(result)
	require.NoError(t, err)
	assert.Equal(t, "tap789", tapDevice)
}
