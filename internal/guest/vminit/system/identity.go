//go:build linux

package system

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/containerd/log"

	"github.com/spin-stack/spinbox/internal/guest/vminit/devices"
	"github.com/spin-stack/spinbox/internal/guest/vminit/extras"
)

// Identity is everything about a VM that depends on which container it runs:
// its address on the network, the disks it should find, the extras archive to
// unpack. It is the half of startup that Initialize deliberately does not do.
//
// A VM that boots reads all of it from its kernel command line, which the host
// wrote for it. A VM restored from a template has a kernel command line too -
// the template's - and it describes a different VM: another address, another
// CID, disks that are not there. So a restore is told instead, over RPC, and
// FromCmdline is what the boot path uses to say the same thing from the source
// it does have.
type Identity struct {
	// Network is the address to take, or nil to leave networking alone - which
	// is what a workload with no NIC wants.
	Network *NetworkIdentity

	// BlockDevices is how many virtio block devices to expect.
	//
	// Negative means the count is not known and the guest should wait and see,
	// which is what a kernel command line without spin.disks implies. Knowing
	// the number is worth a great deal: waiting for a stated count returns as
	// soon as the last disk arrives, while waiting to see costs the whole
	// devices.BlockDeviceTimeout whenever a VM has fewer disks than the guess -
	// and a template, which has none at all, pays all 5 s of it.
	BlockDevices int

	// ExtrasDisk is the block device holding the extras archive, empty when this
	// container has none.
	ExtrasDisk string

	// ExtrasForce re-extracts over the marker that makes extraction idempotent.
	// A debug knob, set from spin.extras_force; a restore never asks for it.
	ExtrasForce bool

	// Restored says this VM came from a template rather than a boot.
	//
	// The guest cannot tell from the inside - that is what a restore is - and two
	// things it cannot see depend on it: its clock resumed at the time the
	// template was frozen, and its random pool is the template's until vmgenid
	// reseeds it. Both are put right either way; this decides whether failing to
	// is an error or simply does not apply.
	Restored bool
}

// unknownDiskCount asks Apply to wait and see rather than for a stated number.
const unknownDiskCount = -1

// Apply gives a VM its identity. It is idempotent: applying it to a VM that is
// already configured that way sets what is already set, which is what lets the
// host call it after a restore without knowing how the VM came up.
//
// The network steps log and continue rather than fail, as they did when this
// read the kernel command line: a workload that does not need networking should
// still run. The disks do not get that treatment - a container whose rootfs
// never appeared is not going to work.
func Apply(ctx context.Context, id Identity) error {
	applyRestoreCorrections(ctx, id.Restored)

	if err := applyDisks(ctx, id.BlockDevices); err != nil {
		return err
	}

	if id.ExtrasDisk != "" {
		if err := extras.ExtractDevice(ctx, id.ExtrasDisk, id.ExtrasForce); err != nil {
			log.G(ctx).WithError(err).Warn("failed to extract extras disk, continuing anyway")
		}
	}

	if id.Network == nil {
		log.G(ctx).Debug("no network identity, skipping network configuration")
		return nil
	}
	if err := configureNetwork(ctx, *id.Network); err != nil {
		log.G(ctx).WithError(err).Warn("failed to configure network interface, continuing anyway")
	}
	if err := configureDNS(ctx, *id.Network); err != nil {
		log.G(ctx).WithError(err).Warn("failed to configure DNS, continuing anyway")
	}
	if err := configureMetadataRoute(ctx, *id.Network); err != nil {
		log.G(ctx).WithError(err).Warn("failed to configure metadata route, continuing anyway")
	}
	return nil
}

// applyDisks makes the VM's disks visible, by whichever route fits what is
// known about them.
func applyDisks(ctx context.Context, want int) error {
	switch {
	case want == 0:
		// Nothing to wait for. Saying so is the point: a template has no disks,
		// and without a count it would spend devices.BlockDeviceTimeout finding
		// that out.
		return nil
	case want < 0:
		devices.WaitForBlockDevices(ctx)
		return nil
	default:
		// A rescan, not just a wait. On a booted VM the devices are already
		// there and it is a no-op that costs one sysfs write; on a restored one
		// the guest enumerated its bus inside the template, before these disks
		// existed, and this is the only thing that will find them.
		if _, err := devices.RescanPCI(ctx, want); err != nil {
			return fmt.Errorf("making this VM's disks visible: %w", err)
		}
		return nil
	}
}

// FromCmdline reads a VM's identity from the kernel command line, which is
// where the host puts it for a VM that boots.
func FromCmdline(ctx context.Context) (Identity, error) {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return Identity{}, fmt.Errorf("read /proc/cmdline: %w", err)
	}
	cmdline := string(b)

	id := Identity{BlockDevices: unknownDiskCount}

	if net, ok := parseIPConfig(cmdline); ok {
		net.MetadataRoute = hasParam(cmdline, "spin.metadata_addr=")
		// The MAC is left alone on the boot path: the NIC came up with the one
		// the host gave QEMU, and it is already right.
		id.Network = &net
	}

	if v, ok := cmdlineValue(cmdline, "spin.disks="); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			log.G(ctx).WithError(err).Warnf("ignoring unparsable spin.disks=%q", v)
		} else {
			id.BlockDevices = n
		}
	}

	if dev, force, err := extras.DeviceFromCmdline(ctx, cmdline); err != nil {
		log.G(ctx).WithError(err).Warn("ignoring unusable extras disk configuration")
	} else {
		id.ExtrasDisk, id.ExtrasForce = dev, force
	}

	return id, nil
}

// cmdlineValue returns the value of the first parameter with this prefix.
func cmdlineValue(cmdline, prefix string) (string, bool) {
	for param := range strings.FieldsSeq(cmdline) {
		if v, ok := strings.CutPrefix(param, prefix); ok {
			return v, true
		}
	}
	return "", false
}

// applyRestoreCorrections puts right the two things a VM inherits from the
// template it was restored from, neither of which it can see for itself.
//
// Both emit a RESTORE line whether they succeed or fail. That is not noise: a VM
// hours in the past and a VM sharing another's random pool both behave perfectly
// until something depends on them, and a check that only speaks up on failure is
// indistinguishable from a check that is not running. There is a test that
// asserts these lines appear, and it would have passed against no
// implementation at all without them.
func applyRestoreCorrections(ctx context.Context, restored bool) {
	// The clock first, because everything after it is timestamped: a restored VM
	// resumes the time its template was frozen at, and would otherwise extract
	// files, configure an interface and start a container in the past.
	//
	// Best-effort, like the network steps. A VM whose clock is wrong is worth
	// running; a VM that refuses to start because it has no PTP device is not.
	skew, err := CorrectClock(ctx)
	switch {
	case err != nil:
		log.G(ctx).WithError(err).Warn("RESTORE clock could not be read from the host")
	case restored:
		log.G(ctx).WithField("skew_us", skew.Microseconds()).
			Info("RESTORE clock read from the host over ptp_kvm")
	}

	if !restored {
		return
	}

	// A restored VM inherits the template's random pool along with its memory, so
	// two containers from one template would produce the same "random" bytes -
	// the same session keys, the same nonces - until something reseeded them. The
	// vmgenid device is what does it, and this is the only place that would
	// notice if any link in that chain broke.
	if CheckEntropyReseed(ctx) {
		log.G(ctx).Info("RESTORE entropy reseeded=true")
		return
	}
	log.G(ctx).Error("RESTORE entropy reseeded=false: this VM kept the template's " +
		"random pool and shares it with every other VM restored from the same template. " +
		"Check that the VM has a vmgenid device and the guest kernel has CONFIG_VMGENID")
}
