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

	// Console. The profiling boot uses the same console as a production boot, and
	// says nothing on it: the per-initcall lines are read from /dev/kmsg by vminitd
	// (system.DumpKernelBootProfile), not scraped from the console, so there is no
	// reason for them to be written twice.
	//
	// This used to route the console through virtio-console (hvc0) because the
	// verbose stream over the emulated 8250 - a PIO VMEXIT per byte - inflated the
	// timings being measured. It fixed the symptom and moved the cost: a console
	// registers at the device_initcall phase, and registering it replays the whole
	// printk ring into it, synchronously, inside that initcall. The profile then
	// showed 24 ms in virtio_console_init, which was the measurement writing itself
	// out - the largest single entry in a boot the same run reported as 137 ms,
	// against the ~90 ms a normal boot takes.
	//
	// With loglevel=0 below, nothing reaches the console at all and the question
	// does not arise; the ring buffer still records everything, which is all the
	// profile needs.
	console := cfg.Console
	if console != "" {
		parts = append(parts, fmt.Sprintf("console=%s", console))
	}

	// Boot verbosity. Debug mode forces verbose output so initcall timings
	// are visible; otherwise honor the configured quiet/loglevel.
	quiet, loglevel := cfg.Quiet, cfg.LogLevel
	if cfg.Debug {
		// Silent console, full ring buffer. loglevel is the *console* threshold;
		// every message is still recorded for /dev/kmsg, which is where the
		// profile is read from.
		quiet, loglevel = false, 0
	}
	if quiet {
		parts = append(parts, "quiet")
	}
	parts = append(parts, fmt.Sprintf("loglevel=%d", loglevel))

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

	// Boot-speed tuning for a KVM guest:
	//   no_timer_check            - skip the boot-time timer IRQ delivery probe
	//   tsc=reliable              - trust the TSC and skip the clocksource watchdog
	//   rcupdate.rcu_expedited=1  - expedite RCU grace periods during boot
	//   pci=lastbus=0             - stop PCI enumeration after bus 0. On our q35
	//                               machine every virtio device sits on the root
	//                               bus (pcie.0), so scanning buses 1-255 is pure
	//                               boot-time overhead. If a device is ever placed
	//                               behind a bridge this must be revisited - the
	//                               integration tests (virtio-blk/-net) are the gate.
	parts = append(parts,
		"no_timer_check",
		"tsc=reliable",
		"rcupdate.rcu_expedited=1",
		"pci=lastbus=0",
	)

	// Boot profiling: print per-initcall timings to the console log.
	//
	// log_buf_len enlarges the printk ring buffer for the profiling boot, and it is
	// the one part of this that is not about the console: vminitd reads the ring
	// after boot, so everything initcall_debug emits - two lines per initcall, plus
	// the verbose ACPI/PCI dumps - has to still be in it. The default 256 KiB
	// (CONFIG_LOG_BUF_SHIFT=18) overflows under that load and silently drops the
	// earliest entries, which are exactly the early core/subsys initcalls the
	// profile exists to see. 4 MiB holds the whole boot.
	if cfg.Debug {
		parts = append(parts, "initcall_debug", "printk.time=1", "log_buf_len=4M")
		// The initcall tracepoints, which are the only source for where a level
		// *boundary* falls: initcall_debug times each call and says nothing about
		// the time between them, and that time is the larger half of boot. The
		// events have been compiled in all along (CONFIG_EVENT_TRACING=y), so this
		// costs a cmdline token and a ring; vminitd reads /sys/kernel/tracing/trace
		// after boot, the same way it reads /dev/kmsg, and for the same reason -
		// nothing is printed while the thing being measured is running.
		parts = append(parts, "trace_event=initcall:*", "trace_buf_size=4M")
		// Userspace companion to initcall_debug: vminitd emits VMINITD_PROFILE
		// lines for its boot phases when this marker is present (see
		// system.BootProfiler). Kept as a separate token so the kernel ignores
		// it and it reaches /proc/cmdline for vminitd to read.
		parts = append(parts, "spin.profile")
	}

	// Network configuration
	if netParam := buildNetworkParam(cfg.Network); netParam != "" {
		parts = append(parts, netParam)
	}

	// Extras disk index for guest to locate the extras block device
	if cfg.ExtrasDiskIndex != nil {
		parts = append(parts, fmt.Sprintf("spin.extras_disk=%d", *cfg.ExtrasDiskIndex))
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
