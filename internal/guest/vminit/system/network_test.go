//go:build linux

package system

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseIPConfig(t *testing.T) {
	tests := []struct {
		name    string
		cmdline string
		want    NetworkIdentity
		ok      bool
	}{
		{
			name:    "no ip param",
			cmdline: "console=hvc0 quiet init=/sbin/vminitd",
			ok:      false,
		},
		{
			name:    "full static config with two DNS",
			cmdline: "console=hvc0 ip=10.88.0.5::10.88.0.1:255.255.255.0::eth0:none:8.8.8.8:8.8.4.4 init=/sbin/vminitd",
			want: NetworkIdentity{
				IP:      "10.88.0.5",
				Gateway: "10.88.0.1",
				Netmask: "255.255.255.0",
				Device:  "eth0",
				DNS:     []string{"8.8.8.8", "8.8.4.4"},
			},
			ok: true,
		},
		{
			name:    "no DNS",
			cmdline: "ip=10.88.0.5::10.88.0.1:255.255.255.0::eth0:none",
			want: NetworkIdentity{
				IP:      "10.88.0.5",
				Gateway: "10.88.0.1",
				Netmask: "255.255.255.0",
				Device:  "eth0",
			},
			ok: true,
		},
		{
			name:    "single DNS",
			cmdline: "ip=10.88.0.5::10.88.0.1:255.255.255.0::eth0:none:1.1.1.1",
			want: NetworkIdentity{
				IP:      "10.88.0.5",
				Gateway: "10.88.0.1",
				Netmask: "255.255.255.0",
				Device:  "eth0",
				DNS:     []string{"1.1.1.1"},
			},
			ok: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseIPConfig(tt.cmdline)
			assert.Equal(t, tt.ok, ok)
			if tt.ok {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestIPConfigDevice(t *testing.T) {
	assert.Equal(t, "eth0", NetworkIdentity{}.device())
	assert.Equal(t, "eth0", NetworkIdentity{Device: "eth0"}.device())
	assert.Equal(t, "ens3", NetworkIdentity{Device: "ens3"}.device())
}

func TestParseNetmask(t *testing.T) {
	t.Run("dotted /24", func(t *testing.T) {
		mask, err := parseNetmask("255.255.255.0")
		require.NoError(t, err)
		ones, bits := mask.Size()
		assert.Equal(t, 24, ones)
		assert.Equal(t, 32, bits)
	})

	t.Run("dotted /16", func(t *testing.T) {
		mask, err := parseNetmask("255.255.0.0")
		require.NoError(t, err)
		ones, _ := mask.Size()
		assert.Equal(t, 16, ones)
	})

	t.Run("empty defaults to /24", func(t *testing.T) {
		mask, err := parseNetmask("")
		require.NoError(t, err)
		ones, _ := mask.Size()
		assert.Equal(t, 24, ones)
	})

	t.Run("invalid", func(t *testing.T) {
		_, err := parseNetmask("not-a-mask")
		assert.Error(t, err)
	})

	t.Run("non-contiguous", func(t *testing.T) {
		_, err := parseNetmask("255.0.255.0")
		assert.Error(t, err)
	})
}

func TestHasParam(t *testing.T) {
	cmdline := "console=hvc0 spin.metadata_addr=169.254.169.254:80 init=/sbin/vminitd"
	assert.True(t, hasParam(cmdline, "spin.metadata_addr="))
	assert.False(t, hasParam(cmdline, "spin.extras_disk="))
}

// metadataServiceIP must stay the IANA link-local metadata address.
func TestMetadataServiceIP(t *testing.T) {
	assert.True(t, metadataServiceIP.Equal(net.IPv4(169, 254, 169, 254)))
}
