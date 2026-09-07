//go:build linux

package system

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/log"
	"golang.org/x/sys/unix"
)

// The guest clock after a restore.
//
// A VM restored from a template resumes the clock the template was frozen with.
// Nothing in the guest notices: from its point of view no time passed between
// the freeze and the resume, because for the guest none did. Measured on a
// restore five seconds after the template was saved, the guest still reported
// zero seconds since its own start.
//
// That is fine for five seconds and ruinous for a template built at install time
// and used for weeks. A guest hours or days in the past rejects every TLS
// certificate as not yet valid, writes logs that arrive before the events that
// caused them, and computes every expiry wrongly.
//
// So a restored VM is told the time. It reads it from the host directly rather
// than being sent it, through the KVM PTP clock: a paravirtual clock device the
// host answers from its own CLOCK_REALTIME, so there is no round trip to be
// wrong by and nothing for the caller to get right. CONFIG_PTP_1588_CLOCK_KVM
// is built into the guest kernel and devtmpfs creates the node.

const (
	// ptpClassDir is where the kernel lists PTP clock devices.
	ptpClassDir = "/sys/class/ptp"

	// kvmPTPClockName is what the ptp_kvm driver calls itself in
	// /sys/class/ptp/ptpN/clock_name. Other PTP clocks can exist - a NIC with
	// hardware timestamping is one - and they do not track the host's wall
	// clock, so the right device is chosen by name rather than by being first.
	kvmPTPClockName = "KVM virtual PTP"

	// clockCorrectionThreshold is the skew below which the clock is left alone.
	// A restore is not a time source: correcting a few milliseconds would step
	// the clock backwards as often as forwards, for no benefit to anything.
	clockCorrectionThreshold = time.Second
)

// errNoKVMPTP means the host clock is not readable from here.
var errNoKVMPTP = errors.New("no KVM PTP clock device")

// CorrectClock sets CLOCK_REALTIME from the host, and reports the skew it
// removed.
//
// It is a no-op on a VM that booted normally, where the clock is already right
// and the skew is microseconds. It is the entire point on a restored one.
func CorrectClock(ctx context.Context) (time.Duration, error) {
	hostNow, err := hostTime()
	if err != nil {
		return 0, err
	}

	skew := time.Until(hostNow)
	if skew.Abs() < clockCorrectionThreshold {
		log.G(ctx).WithField("skew_us", skew.Microseconds()).
			Debug("guest clock is close enough to the host's; leaving it alone")
		return skew, nil
	}

	ts := unix.NsecToTimespec(hostNow.UnixNano())
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
		return skew, fmt.Errorf("setting the guest clock forward by %s: %w", skew, err)
	}

	log.G(ctx).WithFields(log.Fields{
		"skew_ms": skew.Milliseconds(),
		"now":     hostNow.Format(time.RFC3339Nano),
	}).Info("corrected the guest clock from the host")
	return skew, nil
}

// hostTime reads the host's wall clock through the KVM PTP device.
func hostTime() (time.Time, error) {
	path, err := kvmPTPDevice()
	if err != nil {
		return time.Time{}, err
	}

	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	var ts unix.Timespec
	if err := unix.ClockGettime(fdToClockID(f.Fd()), &ts); err != nil {
		return time.Time{}, fmt.Errorf("reading the host clock from %s: %w", path, err)
	}
	return time.Unix(ts.Sec, ts.Nsec), nil
}

// kvmPTPDevice finds the /dev node of the KVM PTP clock.
func kvmPTPDevice() (string, error) {
	entries, err := os.ReadDir(ptpClassDir)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errNoKVMPTP, err)
	}
	for _, e := range entries {
		name, err := os.ReadFile(filepath.Join(ptpClassDir, e.Name(), "clock_name"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(name)) == kvmPTPClockName {
			return filepath.Join("/dev", e.Name()), nil
		}
	}
	return "", fmt.Errorf("%w: no device in %s is named %q", errNoKVMPTP, ptpClassDir, kvmPTPClockName)
}

// fdToClockID turns a PTP device's file descriptor into the dynamic clock id
// clock_gettime takes for it.
//
// This is the kernel's FD_TO_CLOCKID macro: ((~(clockid_t)(fd) << 3) | 3). It is
// how a PTP character device is read as a POSIX clock, and there is no other
// spelling of it.
func fdToClockID(fd uintptr) int32 {
	return (^int32(fd) << 3) | 3
}
