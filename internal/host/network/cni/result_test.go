//go:build linux

package cni

import (
	"net"
	"testing"

	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCNIResult_Success(t *testing.T) {
	tests := []struct {
		name        string
		result      *current.Result
		expectedTAP string
		expectedMAC string
		expectedIP  string
		expectedGW  string
	}{
		{
			name: "valid result with TAP device",
			result: &current.Result{
				CNIVersion: "1.0.0",
				Interfaces: []*current.Interface{
					{Name: "spinbox0", Mac: "aa:bb:cc:dd:ee:ff"},
					{Name: "tap123", Mac: "11:22:33:44:55:66", Sandbox: "/var/run/netns/test"},
				},
				IPs: []*current.IPConfig{
					{
						Address: net.IPNet{
							IP:   net.ParseIP("10.88.0.5"),
							Mask: net.CIDRMask(16, 32),
						},
						Gateway:   net.ParseIP("10.88.0.1"),
						Interface: intPtr(1),
					},
				},
			},
			expectedTAP: "tap123",
			expectedMAC: "11:22:33:44:55:66",
			expectedIP:  "10.88.0.5",
			expectedGW:  "10.88.0.1",
		},
		{
			name: "result with tapXYZ device",
			result: &current.Result{
				CNIVersion: "1.0.0",
				Interfaces: []*current.Interface{
					{Name: "spinbox0", Mac: "aa:bb:cc:dd:ee:ff"},
					{Name: "tapABC123", Mac: "11:22:33:44:55:66", Sandbox: "/var/run/netns/test"},
				},
				IPs: []*current.IPConfig{
					{
						Address: net.IPNet{
							IP:   net.ParseIP("10.88.0.10"),
							Mask: net.CIDRMask(16, 32),
						},
						Gateway:   net.ParseIP("10.88.0.1"),
						Interface: intPtr(1),
					},
				},
			},
			expectedTAP: "tapABC123",
			expectedMAC: "11:22:33:44:55:66",
			expectedIP:  "10.88.0.10",
			expectedGW:  "10.88.0.1",
		},
		{
			name: "result with multiple IPs (uses first)",
			result: &current.Result{
				CNIVersion: "1.0.0",
				Interfaces: []*current.Interface{
					{Name: "tap456", Mac: "22:33:44:55:66:77", Sandbox: "/var/run/netns/test"},
				},
				IPs: []*current.IPConfig{
					{
						Address: net.IPNet{
							IP:   net.ParseIP("10.88.0.20"),
							Mask: net.CIDRMask(16, 32),
						},
						Gateway: net.ParseIP("10.88.0.1"),
					},
					{
						Address: net.IPNet{
							IP:   net.ParseIP("fd00::1"),
							Mask: net.CIDRMask(64, 128),
						},
					},
				},
			},
			expectedTAP: "tap456",
			expectedIP:  "10.88.0.20",
			expectedGW:  "10.88.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseCNIResult(tt.result)
			require.NoError(t, err)
			assert.NotNil(t, result)

			assert.Equal(t, tt.expectedTAP, result.TAPDevice)
			if tt.expectedMAC != "" {
				assert.Equal(t, tt.expectedMAC, result.TAPMAC)
			}
			assert.Equal(t, tt.expectedIP, result.IPAddress.String())

			if tt.expectedGW != "" {
				assert.Equal(t, tt.expectedGW, result.Gateway.String())
			}
		})
	}
}

func TestParseCNIResult_NoIPs(t *testing.T) {
	result := &current.Result{
		CNIVersion: "1.0.0",
		Interfaces: []*current.Interface{
			{Name: "tap123", Mac: "11:22:33:44:55:66", Sandbox: "/var/run/netns/test"},
		},
		IPs: []*current.IPConfig{},
	}

	_, err := ParseCNIResult(result)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no IP addresses")
}

func TestParseCNIResult_TAPMACRequired(t *testing.T) {
	result := &current.Result{
		CNIVersion: "1.0.0",
		Interfaces: []*current.Interface{
			{Name: "tap123", Sandbox: "/var/run/netns/test"},
		},
		IPs: []*current.IPConfig{
			{
				Address: net.IPNet{
					IP:   net.ParseIP("10.88.0.5"),
					Mask: net.CIDRMask(16, 32),
				},
				Gateway: net.ParseIP("10.88.0.1"),
			},
		},
	}

	_, err := ParseCNIResult(result)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty MAC")
}

func TestParseCNIResult_NoTAPDevice(t *testing.T) {
	result := &current.Result{
		CNIVersion: "1.0.0",
		Interfaces: []*current.Interface{
			{Name: "spinbox0", Mac: "aa:bb:cc:dd:ee:ff"},
			{Name: "veth123", Mac: "11:22:33:44:55:66", Sandbox: "/var/run/netns/test"},
		},
		IPs: []*current.IPConfig{
			{
				Address: net.IPNet{
					IP:   net.ParseIP("10.88.0.5"),
					Mask: net.CIDRMask(16, 32),
				},
			},
		},
	}

	_, err := ParseCNIResult(result)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no TAP device found")
}

func TestParseCNIResult_NilResult(t *testing.T) {
	_, err := ParseCNIResult(nil)
	require.Error(t, err)
}

func TestParseCNIResult_GatewayOptional(t *testing.T) {
	result := &current.Result{
		CNIVersion: "1.0.0",
		Interfaces: []*current.Interface{
			{Name: "tap789", Mac: "11:22:33:44:55:66", Sandbox: "/var/run/netns/test"},
		},
		IPs: []*current.IPConfig{
			{
				Address: net.IPNet{
					IP:   net.ParseIP("10.88.0.30"),
					Mask: net.CIDRMask(16, 32),
				},
				Gateway: nil, // No gateway
			},
		},
	}

	cniResult, err := ParseCNIResult(result)
	require.NoError(t, err)
	assert.Nil(t, cniResult.Gateway)
}

// intPtr returns a pointer to an int
func intPtr(i int) *int {
	return &i
}
