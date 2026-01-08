//go:build linux

package qemu

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/aledbf/qemubox/containerd/internal/host/vm"
)

func TestBuildKernelCmdline(t *testing.T) {
	tests := []struct {
		name     string
		cfg      KernelCmdlineConfig
		contains []string
		excludes []string
	}{
		{
			name: "default config",
			cfg:  DefaultKernelCmdlineConfig(),
			contains: []string{
				"console=ttyS0",
				"quiet",
				"loglevel=3",
				"panic=1",
				"net.ifnames=0",
				"biosdevname=0",
				"systemd.unified_cgroup_hierarchy=1",
				"cgroup_no_v1=all",
				"nohz=off",
				"init=/sbin/vminitd",
			},
		},
		{
			name: "with network config",
			cfg: KernelCmdlineConfig{
				Console:         "ttyS0",
				VsockRPCPort:    1024,
				VsockStreamPort: 1025,
				VsockCID:        5,
				Quiet:           true,
				LogLevel:        3,
				Network: &vm.NetworkConfig{
					IP:      "10.0.0.5",
					Gateway: "10.0.0.1",
					Netmask: "255.255.255.0",
				},
			},
			contains: []string{
				"ip=10.0.0.5::10.0.0.1:255.255.255.0::eth0:none",
				"-vsock-cid=5",
			},
		},
		{
			name: "with DNS servers",
			cfg: KernelCmdlineConfig{
				Console:  "ttyS0",
				VsockCID: 3,
				Quiet:    true,
				LogLevel: 3,
				Network: &vm.NetworkConfig{
					IP:      "10.0.0.5",
					Gateway: "10.0.0.1",
					Netmask: "255.255.255.0",
					DNS:     []string{"8.8.8.8", "8.8.4.4"},
				},
			},
			contains: []string{
				"ip=10.0.0.5::10.0.0.1:255.255.255.0::eth0:none:8.8.8.8:8.8.4.4",
			},
		},
		{
			name: "with extra DNS servers (truncated to 2)",
			cfg: KernelCmdlineConfig{
				Console:  "ttyS0",
				VsockCID: 3,
				Quiet:    true,
				LogLevel: 3,
				Network: &vm.NetworkConfig{
					IP:      "10.0.0.5",
					Gateway: "10.0.0.1",
					Netmask: "255.255.255.0",
					DNS:     []string{"8.8.8.8", "8.8.4.4", "1.1.1.1"},
				},
			},
			contains: []string{
				"ip=10.0.0.5::10.0.0.1:255.255.255.0::eth0:none:8.8.8.8:8.8.4.4",
			},
			excludes: []string{"1.1.1.1"},
		},
		{
			name: "with custom init args",
			cfg: KernelCmdlineConfig{
				Console:  "ttyS0",
				VsockCID: 3,
				Quiet:    true,
				LogLevel: 3,
				InitArgs: []string{"-debug", "-trace"},
			},
			contains: []string{
				"-debug",
				"-trace",
			},
		},
		{
			name: "not quiet",
			cfg: KernelCmdlineConfig{
				Console:  "ttyS0",
				VsockCID: 3,
				Quiet:    false,
				LogLevel: 7,
			},
			contains: []string{
				"loglevel=7",
			},
			excludes: []string{"quiet"},
		},
		{
			name: "custom console",
			cfg: KernelCmdlineConfig{
				Console:  "hvc0",
				VsockCID: 3,
				Quiet:    true,
				LogLevel: 3,
			},
			contains: []string{"console=hvc0"},
			excludes: []string{"console=ttyS0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmdline := BuildKernelCmdline(tt.cfg)

			for _, s := range tt.contains {
				assert.Contains(t, cmdline, s, "cmdline should contain %q", s)
			}

			for _, s := range tt.excludes {
				assert.NotContains(t, cmdline, s, "cmdline should not contain %q", s)
			}
		})
	}
}

func TestBuildNetworkParam(t *testing.T) {
	tests := []struct {
		name string
		cfg  *vm.NetworkConfig
		want string
	}{
		{
			name: "nil config",
			cfg:  nil,
			want: "",
		},
		{
			name: "empty IP",
			cfg:  &vm.NetworkConfig{IP: ""},
			want: "",
		},
		{
			name: "basic config",
			cfg: &vm.NetworkConfig{
				IP:      "192.168.1.10",
				Gateway: "192.168.1.1",
				Netmask: "255.255.255.0",
			},
			want: "ip=192.168.1.10::192.168.1.1:255.255.255.0::eth0:none",
		},
		{
			name: "with one DNS",
			cfg: &vm.NetworkConfig{
				IP:      "192.168.1.10",
				Gateway: "192.168.1.1",
				Netmask: "255.255.255.0",
				DNS:     []string{"8.8.8.8"},
			},
			want: "ip=192.168.1.10::192.168.1.1:255.255.255.0::eth0:none:8.8.8.8",
		},
		{
			name: "with two DNS",
			cfg: &vm.NetworkConfig{
				IP:      "192.168.1.10",
				Gateway: "192.168.1.1",
				Netmask: "255.255.255.0",
				DNS:     []string{"8.8.8.8", "8.8.4.4"},
			},
			want: "ip=192.168.1.10::192.168.1.1:255.255.255.0::eth0:none:8.8.8.8:8.8.4.4",
		},
		{
			name: "DNS truncated to two",
			cfg: &vm.NetworkConfig{
				IP:      "192.168.1.10",
				Gateway: "192.168.1.1",
				Netmask: "255.255.255.0",
				DNS:     []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"},
			},
			want: "ip=192.168.1.10::192.168.1.1:255.255.255.0::eth0:none:1.1.1.1:8.8.8.8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildNetworkParam(tt.cfg)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuildInitArgs(t *testing.T) {
	tests := []struct {
		name string
		cfg  KernelCmdlineConfig
		want []string
	}{
		{
			name: "basic config",
			cfg: KernelCmdlineConfig{
				VsockRPCPort:    1024,
				VsockStreamPort: 1025,
				VsockCID:        5,
			},
			want: []string{
				"-vsock-rpc-port=1024",
				"-vsock-stream-port=1025",
				"-vsock-cid=5",
			},
		},
		{
			name: "with extra args",
			cfg: KernelCmdlineConfig{
				VsockRPCPort:    1024,
				VsockStreamPort: 1025,
				VsockCID:        5,
				InitArgs:        []string{"-debug", "-verbose"},
			},
			want: []string{
				"-vsock-rpc-port=1024",
				"-vsock-stream-port=1025",
				"-vsock-cid=5",
				"-debug",
				"-verbose",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildInitArgs(tt.cfg)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFormatInitArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "empty",
			args: []string{},
			want: "",
		},
		{
			name: "single arg",
			args: []string{"-debug"},
			want: "-debug",
		},
		{
			name: "multiple args",
			args: []string{"-port=1024", "-cid=5"},
			want: "-port=1024 -cid=5",
		},
		{
			name: "args with spaces need quoting",
			args: []string{"-msg=hello world"},
			want: "\"-msg=hello world\"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatInitArgs(tt.args)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNeedsQuoting(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"hello", false},
		{"hello world", true},
		{"hello\tworld", true},
		{"hello\nworld", true},
		{"", false},
		{"-flag=value", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := needsQuoting(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

// BenchmarkBuildKernelCmdline benchmarks cmdline construction.
func BenchmarkBuildKernelCmdline(b *testing.B) {
	cfg := KernelCmdlineConfig{
		Console:         "ttyS0",
		VsockRPCPort:    1024,
		VsockStreamPort: 1025,
		VsockCID:        5,
		Quiet:           true,
		LogLevel:        3,
		Network: &vm.NetworkConfig{
			IP:      "10.0.0.5",
			Gateway: "10.0.0.1",
			Netmask: "255.255.255.0",
			DNS:     []string{"8.8.8.8", "8.8.4.4"},
		},
		InitArgs: []string{"-debug"},
	}

	for b.Loop() {
		result := BuildKernelCmdline(cfg)
		// Prevent compiler optimization
		if len(result) == 0 {
			b.Fatal("empty result")
		}
	}
}

// TestKernelCmdlineFormat verifies the overall format is correct.
func TestKernelCmdlineFormat(t *testing.T) {
	cfg := DefaultKernelCmdlineConfig()
	cfg.VsockCID = 42

	cmdline := BuildKernelCmdline(cfg)

	// Should not have leading/trailing spaces
	assert.Equal(t, strings.TrimSpace(cmdline), cmdline)

	// Should not have double spaces
	assert.NotContains(t, cmdline, "  ")

	// Should end with init command
	assert.Contains(t, cmdline, "init=/sbin/vminitd")
}
