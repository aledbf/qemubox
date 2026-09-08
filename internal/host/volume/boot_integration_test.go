//go:build linux && integration

package volume

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spin-stack/spin-machine/machine"
)

// TestAGuestBootsFromTheChainAndTheBaseSurvivesIt is the claim the unit tests
// cannot make: that what this package prepares is a disk a real Linux kernel
// mounts, and that mounting it read-write leaves the shared base image
// untouched.
//
// Both halves matter and only the second is subtle. A guest that boots proves
// the overlay resolves to its backing file; the base image being byte-for-byte
// identical afterwards proves the sharing is safe, which is the property every
// other VM on the host depends on and the one whose failure has no symptom until
// some unrelated guest reads a cluster that moved.
func TestAGuestBootsFromTheChainAndTheBaseSurvivesIt(t *testing.T) {
	holdVMLane(t)
	rel := release(t)

	qemuImg := filepath.Join(rel, "bin", "qemu-img")
	base := filepath.Join(rel, "image", "rootfs.qcow2")
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("no base image at %s — run: task machine: %v", base, err)
	}

	before := sum(t, base)

	disk, err := New(t.TempDir(), base, qemuImg).Open(context.Background(), "workspace-one", 8<<30)
	if err != nil {
		t.Fatalf("preparing the chain: %v", err)
	}

	out := boot(t, rel, disk.Image)

	// The kernel is asked, not the userland. /bin/true runs as init and exits,
	// which panics the kernel and is how QEMU stops here — but a guest with no
	// initramfs has no /proc, so anything in userland that wants to describe its
	// own mounts cannot. `mount` was tried first and failed on exactly that,
	// which reads like a broken chain and is not.
	//
	// Two lines, and together they are the claim:
	//
	//   EXT4-fs (vda): mounted filesystem ...  the kernel resolved the overlay to
	//                                          its backing file and mounted what
	//                                          it found, read-write
	//   Comm: true                             PID 1 was executed *from that
	//                                          filesystem*, so the userland the
	//                                          base image carries is really there
	if !strings.Contains(out, "EXT4-fs (vda): mounted filesystem") {
		t.Errorf("the kernel did not mount an ext4 root from the chain; it printed:\n%s", out)
	}
	if !strings.Contains(out, "Comm: true") {
		t.Errorf("PID 1 did not come off the chain; it printed:\n%s", out)
	}
	if strings.Contains(out, "(ro)") || strings.Contains(out, "mounting read-only") {
		t.Errorf("the root came up read-only, so the guest cannot write to its own layer:\n%s", out)
	}
	if after := sum(t, base); after != before {
		t.Errorf("booting a guest changed the base image: %s became %s", before, after)
	}
}

// release finds the machine this repository fetched. Fails rather than skips —
// see the note on qemuImg in the unit tests.
func release(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		out := filepath.Join(dir, "_output")
		if _, err := os.Stat(filepath.Join(out, "bin", "qemu-system-x86_64")); err == nil {
			return out
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no machine in _output — run: task machine")
		}
		dir = parent
	}
}

// boot runs the guest and returns everything it printed on the console.
//
// The command line comes from machine.Spec, because the point is that the disk
// this package prepares is handed to the machine this project runs and not to
// some other one assembled for a test.
func boot(t *testing.T, rel, image string) string {
	t.Helper()

	spec := machine.Spec{
		QEMU:     filepath.Join(rel, "bin", "qemu-system-x86_64"),
		Kernel:   filepath.Join(rel, "kernel", "vmlinux"),
		Firmware: filepath.Join(rel, "qemu"),
		BootCPUs: 2,
		Memory:   machine.Memory{SizeMB: 2048},
		Disks: []machine.Disk{{
			Path:    image,
			Format:  "qcow2",
			Locking: true,
		}},
		Serial: "file:/dev/stdout",
	}
	c := machine.DefaultCmdline()
	c.Console = "ttyS0"
	c.Quiet = false
	// Everything the kernel says, because what is being read back is a line it
	// prints at info level. The default is 3, which is errors and worse.
	c.LogLevel = 7
	c.Root = "/dev/vda"
	c.Init = "/bin/true"
	spec.Cmdline = c.String()

	args, err := spec.Args()
	if err != nil {
		t.Fatalf("building the command line: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// CombinedOutput, because the console is on stdout and QEMU's own refusals
	// are on stderr, and a failure here is as likely to be one as the other.
	out, err := exec.CommandContext(ctx, spec.QEMU, args...).CombinedOutput() // #nosec G204
	if ctx.Err() != nil {
		t.Fatalf("the guest did not stop within the timeout; it printed:\n%s", out)
	}
	// A non-zero exit is expected: init exits, the kernel panics, QEMU stops. What
	// is not expected is QEMU refusing the command line, and that prints nothing
	// from the guest at all.
	if err != nil && !strings.Contains(string(out), "Linux version") {
		t.Fatalf("the guest never started: %v\n%s", err, out)
	}
	return string(out)
}

func sum(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("sha256sum", path).Output() // #nosec G204
	if err != nil {
		t.Fatalf("hashing %s: %v", path, err)
	}
	return strings.Fields(string(out))[0]
}

// vmLaneLock is the same file the integration Taskfile target takes. It is
// spelled twice because the two takers are a shell and a Go test and there is
// nowhere they could share a constant; VM_LANE_LOCK in Taskfile.yml is the other
// half, and whoever changes one has to change the other.
const vmLaneLock = "/tmp/spinbox-vm-lane.lock"

// holdVMLane blocks until this process owns the host's VM lane, and holds it for
// the rest of the test.
//
// The self-hosted CI runner is somebody's workstation, so a developer running
// this test and a CI job running the integration lane are two things booting VMs
// on one machine. They do not fail at what they were doing: the loser fails
// during setup, naming a device node, in a message that has nothing to do with
// either change. Taking the same lock the Taskfile takes puts both behind one
// queue.
func holdVMLane(t *testing.T) {
	t.Helper()

	// Read-only, which is all flock(2) needs, and is what makes one lock usable by
	// two different users. CI's job and the developer at this machine are not the
	// same account, and whichever creates the file first makes it unwritable by
	// the other — which is how this failed: "Permission denied" on a path both
	// could read perfectly well.
	//
	// #nosec G302,G304 -- a well-known path, and it must be openable by whoever
	// runs the tests as well as by CI.
	f, err := os.OpenFile(vmLaneLock, os.O_CREATE|os.O_RDONLY, 0o666)
	if err != nil {
		t.Fatalf("opening the VM lane lock %s: %v", vmLaneLock, err)
	}
	t.Cleanup(func() { _ = f.Close() })

	// Non-blocking first, so that waiting is announced rather than looking like a
	// hung test: this lane can be held for many minutes by a CI run.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		return
	}
	t.Logf("waiting for the host's VM lane (%s) — something else is booting VMs here", vmLaneLock)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("waiting for %s: %v", vmLaneLock, err)
	}
}
