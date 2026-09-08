//go:build linux

package qemu

import (
	"fmt"
	"strings"

	"github.com/spin-stack/spin-machine/machine"

	"github.com/spin-stack/spinbox/internal/host/vm"
	"github.com/spin-stack/spinbox/internal/vsock"
)

// The kernel command line, which is two things stuck together.
//
// Most of it is a statement about the hardware — that the PCI bus stops at 0,
// that the TSC can be trusted, that there is no timer to probe for — and none of
// that is this repository's to decide. It comes from machine.Cmdline, next to the
// kernel it was written for, and getting one of those wrong shows up as boot time
// rather than as an error.
//
// What is here is the rest: the contract with vminitd. Which vsock ports to
// answer on, how many disks to wait for, where the extras disk is, what address
// the guest has. The kernel ignores every one of them and passes them through to
// /proc/cmdline, which is exactly why they work — see system.FromCmdline.

// KernelCmdlineConfig is what this repository adds to the machine's own command
// line.
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

	// DiskCount is how many virtio block devices this VM is given.
	//
	// The guest waits for exactly that many instead of waiting to see what turns
	// up: waiting to see costs the whole 5 s devices.BlockDeviceTimeout whenever a
	// VM has fewer disks than the guess, and all of it for a VM that has none.
	DiskCount int

	// ExtrasDiskIndex is the 0-based index of the extras disk (nil if none).
	// The guest parses spin.extras_disk=N to locate the block device.
	ExtrasDiskIndex *int

	// Debug enables boot profiling: initcall_debug, a printk ring big enough to
	// hold the result, and a console that stays silent so measuring costs
	// nothing. vminitd dumps the ring afterwards. Off by default.
	Debug bool
}

// DefaultKernelCmdlineConfig returns a default configuration.
func DefaultKernelCmdlineConfig() KernelCmdlineConfig {
	d := machine.DefaultCmdline()
	return KernelCmdlineConfig{
		Console:         d.Console,
		VsockRPCPort:    vsock.DefaultRPCPort,
		VsockStreamPort: vsock.DefaultStreamPort,
		Quiet:           d.Quiet,
		LogLevel:        d.LogLevel,
	}
}

// BuildKernelCmdline constructs the kernel command line from the configuration.
func BuildKernelCmdline(cfg KernelCmdlineConfig) string {
	c := machine.Cmdline{
		Console:  cfg.Console,
		Quiet:    cfg.Quiet,
		LogLevel: cfg.LogLevel,
		Init:     "/sbin/vminitd",
		InitArgs: buildInitArgs(cfg),
		Extra:    guestParams(cfg),
	}
	if cfg.Debug {
		// Silent console, full ring buffer, per-initcall timings and the initcall
		// tracepoints. vminitd reads both afterwards; see
		// system.DumpKernelBootProfile.
		c = c.Profiling()
		// The userspace half, and this one is ours: vminitd emits VMINITD_PROFILE
		// lines for its own boot phases when it finds this marker. A separate
		// token so the kernel ignores it and it still reaches /proc/cmdline.
		c.Extra = append(c.Extra, "spin.profile")
	}
	return c.String()
}

// guestParams is everything vminitd reads out of /proc/cmdline.
func guestParams(cfg KernelCmdlineConfig) []string {
	// systemd is not PID 1 in this guest — vminitd is — so these two do nothing
	// here. They are kept because a guest that does run systemd, which the base
	// image can, should not print a boot status nobody reads onto the console the
	// kernel log shares.
	parts := []string{
		"systemd.show_status=0",
		"systemd.log_level=warning",
	}

	if netParam := buildNetworkParam(cfg.Network); netParam != "" {
		parts = append(parts, netParam)
	}

	parts = append(parts, fmt.Sprintf("spin.disks=%d", cfg.DiskCount))
	if cfg.ExtrasDiskIndex != nil {
		parts = append(parts, fmt.Sprintf("spin.extras_disk=%d", *cfg.ExtrasDiskIndex))
	}
	return parts
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
