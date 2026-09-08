//go:build linux

package qemu

import (
	"os"
	"path/filepath"
	"testing"
)

// resolvePointer is the only thing in this package that needs no qemu-img, so it is
// the only thing tested without one. Everything else drives the binary and lives
// behind the integration tag, in the lane that has a machine.

func TestResolveReadsThePointer(t *testing.T) {
	dir := t.TempDir()
	pointer := filepath.Join(dir, "current")
	image := filepath.Join(dir, "layers", "vm-one.qcow2")

	if err := os.WriteFile(pointer, []byte(image), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePointer(pointer)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != image {
		t.Errorf("Resolve returned %q, want %q", got, image)
	}

	// And it moves when the pointer moves, which is the whole reason a launcher
	// reads it instead of keeping the path it was handed: a rotation replaces the
	// tip under a running guest.
	next := filepath.Join(dir, "layers", "vm-one-next.qcow2")
	if err := os.WriteFile(pointer, []byte(next+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := resolvePointer(pointer); err != nil || got != next {
		t.Errorf("after the pointer moved, Resolve returned %q (err %v), want %q", got, err, next)
	}
}

// TestResolveRefusesWhatALauncherCannotUse. Each of these would otherwise reach
// QEMU as a complaint about a file rather than about a pointer.
func TestResolveRefusesWhatALauncherCannotUse(t *testing.T) {
	dir := t.TempDir()

	for _, c := range []struct {
		name    string
		content string
	}{
		{"empty", "  \n"},
		// A relative path resolves against the reader's working directory, and the
		// reader that matters is QEMU's.
		{"relative", "layers/vm-one.qcow2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(dir, c.name)
			if err := os.WriteFile(p, []byte(c.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := resolvePointer(p); err == nil {
				t.Errorf("Resolve accepted a pointer whose content is %q", c.content)
			}
		})
	}

	if _, err := resolvePointer(filepath.Join(dir, "absent")); err == nil {
		t.Error("Resolve accepted a pointer that is not there")
	}
}
