//go:build linux

package volume

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// These tests drive the real qemu-img, because what is under test is a chain of
// qcow2 files and every claim about one is that binary's to make. A fake would
// assert that this package can compose strings.
//
// The binary is found by walking up to the repository's own _output/, which is
// where `task machine` puts a release, and not through the shim's configuration.
// Going through the configuration is what the first version did, and it made
// every one of these tests skip on this developer's machine — /etc/spinbox
// points at a state directory only root can write, which has nothing to do with
// whether qemu-img can create a qcow2. A test that skips everywhere passes
// everywhere.
//
// If it is genuinely absent this fails rather than skipping: `task machine`
// fetches it, the tests need nothing else, and a green run that checked nothing
// is worse than a red one that says what to install.
func qemuImg(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("SPINBOX_QEMU_IMG"); p != "" {
		return p
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("looking for the repository root: %v", err)
	}
	for {
		p := filepath.Join(dir, "_output", "bin", "qemu-img")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no qemu-img in _output/bin — run: task machine")
		}
		dir = parent
	}
}

// base writes a qcow2 to stand in for the release's rootfs: a real image of the
// right format, so the chain over it is a real chain.
func base(t *testing.T, qemuImgPath string, sizeBytes int64) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rootfs.qcow2")
	cmd := exec.Command(qemuImgPath, "create", "-f", "qcow2", p, itoa(sizeBytes)) // #nosec G204
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("creating the base image: %v: %s", err, out)
	}
	return p
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

// imageInfo is the part of `qemu-img info` these tests read back.
type imageInfo struct {
	Format              string `json:"format"`
	VirtualSize         int64  `json:"virtual-size"`
	FullBackingFilename string `json:"full-backing-filename"`
}

func inspect(t *testing.T, qemuImgPath, image string) imageInfo {
	t.Helper()
	out, err := exec.Command(qemuImgPath, "info", "--output=json", image).Output() // #nosec G204
	if err != nil {
		t.Fatalf("qemu-img info %s: %v", image, err)
	}
	var info imageInfo
	if err := json.Unmarshal(out, &info); err != nil {
		t.Fatalf("parsing qemu-img info: %v", err)
	}
	return info
}

const gib = 1 << 30

// TestOpenPutsTheVMOnTopOfTheBase is the claim the whole package exists to make.
func TestOpenPutsTheVMOnTopOfTheBase(t *testing.T) {
	qi := qemuImg(t)
	root := t.TempDir()
	baseImage := base(t, qi, gib)

	disk, err := New(root, baseImage, qi).Open(context.Background(), "vm-one", 2*gib)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	info := inspect(t, qi, disk.Image)
	if info.Format != "qcow2" {
		t.Errorf("the tip is %q, want qcow2", info.Format)
	}
	// Backed by the base, so the guest reads the userland it never wrote.
	if info.FullBackingFilename != baseImage {
		t.Errorf("the tip is backed by %q, want the base image %q", info.FullBackingFilename, baseImage)
	}
	// And larger than what backs it: a workspace gets the disk it asked for, and
	// the base contributes the bytes it has.
	if info.VirtualSize != 2*gib {
		t.Errorf("the tip is %d bytes, want %d", info.VirtualSize, 2*gib)
	}

	// The pointer is the contract with whoever launches the VM: one line, the
	// absolute path of the image, no newline.
	b, err := os.ReadFile(disk.Pointer)
	if err != nil {
		t.Fatalf("reading the pointer: %v", err)
	}
	if got := string(b); got != disk.Image {
		t.Errorf("the pointer says %q, the image is %q", got, disk.Image)
	}
	if !filepath.IsAbs(disk.QMPSocket) {
		t.Errorf("the QMP socket path is not absolute: %q", disk.QMPSocket)
	}
}

// TestTheBaseIsNotWritten is the invariant that cannot be recovered from: many
// VMs map this one file, and a write to it corrupts every overlay in existence
// silently, because they keep working until they read a cluster that moved.
func TestTheBaseIsNotWritten(t *testing.T) {
	qi := qemuImg(t)
	root := t.TempDir()
	baseImage := base(t, qi, gib)

	before, err := os.ReadFile(baseImage)
	if err != nil {
		t.Fatal(err)
	}

	s := New(root, baseImage, qi)
	for _, id := range []string{"vm-one", "vm-two", "vm-three"} {
		if _, err := s.Open(context.Background(), id, gib); err != nil {
			t.Fatalf("Open(%s): %v", id, err)
		}
	}

	after, err := os.ReadFile(baseImage)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("preparing three VMs changed the base image")
	}
}

// TestReopenReturnsTheSameLayer is what a restart needs. A second layer for a VM
// that already has one is a disk with none of its writes on it — and the guest
// boots and finds an old world rather than failing.
func TestReopenReturnsTheSameLayer(t *testing.T) {
	qi := qemuImg(t)
	root := t.TempDir()
	s := New(root, base(t, qi, gib), qi)

	first, err := s.Open(context.Background(), "vm-one", gib)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	second, err := s.Open(context.Background(), "vm-one", gib)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if first.Image != second.Image {
		t.Errorf("reopening gave a different layer: %q then %q", first.Image, second.Image)
	}
}

// TestVMsDoNotShareATip: the base is shared on purpose and the tip is not, and
// nothing about the paths should make that an accident.
func TestVMsDoNotShareATip(t *testing.T) {
	qi := qemuImg(t)
	root := t.TempDir()
	s := New(root, base(t, qi, gib), qi)

	one, err := s.Open(context.Background(), "vm-one", gib)
	if err != nil {
		t.Fatal(err)
	}
	two, err := s.Open(context.Background(), "vm-two", gib)
	if err != nil {
		t.Fatal(err)
	}
	if one.Image == two.Image {
		t.Fatalf("two VMs were given the same writable layer: %q", one.Image)
	}
	if one.QMPSocket == two.QMPSocket {
		t.Errorf("two VMs were given the same QMP socket: %q", one.QMPSocket)
	}
}

func TestOpenRefusesWhatItCannotName(t *testing.T) {
	qi := qemuImg(t)
	s := New(t.TempDir(), base(t, qi, gib), qi)

	if _, err := s.Open(context.Background(), "", gib); err == nil {
		t.Error("Open accepted a volume with no id")
	}
	if _, err := s.Open(context.Background(), "vm-one", 0); err == nil {
		t.Error("Open accepted a volume of no size")
	}
}
