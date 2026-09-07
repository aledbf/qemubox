//go:build linux

package system

import (
	"context"
	"errors"
	"strings"

	"github.com/containerd/log"
	"golang.org/x/sys/unix"
)

// Entropy after a restore.
//
// Every VM restored from a template starts with the template's memory, and the
// kernel's random pool is part of that memory. Two containers restored from one
// template would produce the same "random" bytes - the same session keys, the
// same nonces, the same UUIDs - until something reseeded them. That is the kind
// of failure that does not announce itself and is worth a great deal to whoever
// finds it first.
//
// The mechanism is already in place: the VM carries a vmgenid device whose GUID
// QEMU randomises per process and rewrites on the destination of every migration
// (vmgenid_post_load calls vmgenid_update_guest, which raises an ACPI notify).
// The guest's CONFIG_VMGENID driver answers that with add_vmfork_randomness,
// which reseeds the CRNG.
//
// What is not in place is any way to know it happened. The kernel says so, at
// pr_notice level, which loglevel=3 keeps off the console - so the whole chain
// could break silently at any link: a template built without the device, a
// kernel config change, a QEMU that stops notifying. This checks.

// vmforkReseedMessage is what drivers/char/random.c prints from
// add_vmfork_randomness once the CRNG has been reseeded.
const vmforkReseedMessage = "crng reseeded due to virtual machine fork"

// CheckEntropyReseed reports whether this VM's random pool has been reseeded
// since it was forked from a template.
//
// It returns false on a VM that booted normally, which never forked and has
// nothing to reseed for. The caller decides what that means: on a restore it is
// a problem, on a boot it is the expected answer.
func CheckEntropyReseed(ctx context.Context) bool {
	found, err := kmsgContains(vmforkReseedMessage)
	if err != nil {
		log.G(ctx).WithError(err).Debug("cannot read /dev/kmsg to confirm the random pool was reseeded")
		return false
	}
	return found
}

// kmsgContains reports whether any buffered kernel message contains needle.
func kmsgContains(needle string) (bool, error) {
	// O_NONBLOCK so Read returns EAGAIN at the end of the buffer instead of
	// blocking for messages that have not been printed yet. Raw unix.Read avoids
	// the Go runtime poller, which would wait on EAGAIN rather than returning
	// it. Same reasoning as readKmsgInitcalls.
	fd, err := unix.Open("/dev/kmsg", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	defer func() { _ = unix.Close(fd) }()

	buf := make([]byte, 8192) // the kernel returns one record per read
	for {
		n, err := unix.Read(fd, buf)
		if err != nil {
			switch {
			case errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EWOULDBLOCK):
				return false, nil
			case errors.Is(err, unix.EPIPE):
				// Records were overwritten while reading; keep going from
				// wherever the kernel put us.
				continue
			case errors.Is(err, unix.EINTR):
				continue
			default:
				return false, err
			}
		}
		if strings.Contains(string(buf[:n]), needle) {
			return true, nil
		}
	}
}
