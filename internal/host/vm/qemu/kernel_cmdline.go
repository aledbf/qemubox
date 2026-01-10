//go:build linux

package qemu

import (
	"fmt"
	"strings"

	"github.com/spin-stack/spinbox/internal/host/vm"
	"github.com/spin-stack/spinbox/internal/vsock"
)

// KernelCmdlineConfig holds the configuration for building a kernel command line.
type KernelCmdlineConfig struct {
	// Console device (e.g., "ttyS0")
	Console string

	// Vsock configuration
	VsockRPCPort    uint32
	VsockStreamPort uint32
	VsockCID        uint32

	// Network configuration (optional)
	Network *vm.NetworkConfig

	// Additional init arguments
	InitArgs []string

	// Quiet boot (reduces kernel messages)
	Quiet bool

	// Log level (0-7, lower is more verbose)
	LogLevel int
}

// DefaultKernelCmdlineConfig returns a default configuration.
func DefaultKernelCmdlineConfig() KernelCmdlineConfig {
	return KernelCmdlineConfig{
		Console:         "ttyS0",
		VsockRPCPort:    vsock.DefaultRPCPort,
		VsockStreamPort: vsock.DefaultStreamPort,
		Quiet:           true,
		LogLevel:        3,
	}
}

// BuildKernelCmdline constructs the kernel command line from the configuration.
func BuildKernelCmdline(cfg KernelCmdlineConfig) string {
	var parts []string

	// Console
	if cfg.Console != "" {
		parts = append(parts, fmt.Sprintf("console=%s", cfg.Console))
	}

	// Boot verbosity
	if cfg.Quiet {
		parts = append(parts, "quiet")
	}
	parts = append(parts, fmt.Sprintf("loglevel=%d", cfg.LogLevel))

	// Systemd options
	parts = append(parts,
		"systemd.show_status=0",
		"systemd.log_level=warning",
	)

	// Panic behavior
	parts = append(parts, "panic=1")

	// Network naming
	parts = append(parts, "net.ifnames=0", "biosdevname=0")

	// Cgroup v2
	parts = append(parts,
		"systemd.unified_cgroup_hierarchy=1",
		"cgroup_no_v1=all",
	)

	// Disable tickless kernel (reduces overhead for short-lived VMs)
	parts = append(parts, "nohz=off")

	// Network configuration
	if netParam := buildNetworkParam(cfg.Network); netParam != "" {
		parts = append(parts, netParam)
	}

	// Init command with vsock args
	initArgs := buildInitArgs(cfg)
	parts = append(parts, fmt.Sprintf("init=/sbin/vminitd -- %s", formatInitArgs(initArgs)))

	return strings.Join(parts, " ")
}

// buildNetworkParam builds the ip= kernel parameter for network configuration.
func buildNetworkParam(netCfg *vm.NetworkConfig) string {
	if netCfg == nil || netCfg.IP == "" {
		return ""
	}

	// IPv4 configuration using kernel ip= parameter format:
	// ip=<client-ip>:<server-ip>:<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>:<dns0-ip>:<dns1-ip>
	var b strings.Builder
	fmt.Fprintf(&b, "ip=%s::%s:%s::eth0:none",
		netCfg.IP,
		netCfg.Gateway,
		netCfg.Netmask)

	// Append DNS servers (kernel supports up to 2)
	for i, dns := range netCfg.DNS {
		if i >= 2 {
			break
		}
		b.WriteString(":")
		b.WriteString(dns)
	}

	return b.String()
}

// buildInitArgs constructs the init arguments list.
func buildInitArgs(cfg KernelCmdlineConfig) []string {
	args := []string{
		fmt.Sprintf("-vsock-rpc-port=%d", cfg.VsockRPCPort),
		fmt.Sprintf("-vsock-stream-port=%d", cfg.VsockStreamPort),
		fmt.Sprintf("-vsock-cid=%d", cfg.VsockCID),
	}
	return append(args, cfg.InitArgs...)
}
