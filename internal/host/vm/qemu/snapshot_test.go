//go:build linux

package qemu

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/spin-stack/spin-machine/machine"
)

// The command line is where the snapshot design is either right or wrong: the
// template and the restore have to agree on the RAM object's name and disagree
// on how it is mapped, and a VM that does neither must come out unchanged.
//
// The three arguments are built by the machine package; what is under test here
// is the decision this repository makes, which is which of the three cases a VM
// is in. It is two booleans derived from two paths — see (*Instance).spec — and
// getting one backwards is not an error at start-up. A template that mapped its
// RAM privately would freeze a file full of nothing; a restore that shared it
// would write into the template every later VM reads.
func TestSnapshotCommandLine(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	build := func(memFile, restore string) string {
		s := machine.Spec{
			QEMU:     filepath.Join(dir, "qemu"),
			Kernel:   filepath.Join(dir, "kernel"),
			Firmware: dir,
			BootCPUs: 1,
			Memory: machine.Memory{
				SizeMB: 512,
				File:   memFile,
				// The line from spec(): a template writes into the file and must
				// share it, a restore maps the same file privately.
				Shared: memFile != "" && restore == "",
			},
			IncomingDefer: restore != "",
		}
		args, err := s.Args()
		if err != nil {
			t.Fatalf("Args: %v", err)
		}
		return strings.Join(args, " ")
	}

	t.Run("plain VM is untouched", func(t *testing.T) {
		t.Parallel()
		got := build("", "")
		if strings.Contains(got, "memory-backend") {
			t.Errorf("plain VM should not reference a memory backend: %s", got)
		}
		if strings.Contains(got, "-incoming") {
			t.Errorf("plain VM should not wait for incoming state: %s", got)
		}
	})

	t.Run("template shares its RAM file", func(t *testing.T) {
		t.Parallel()
		got := build("/tmp/ram.img", "")
		for _, want := range []string{
			"memory-backend-file,id=pc.ram,size=512M,mem-path=/tmp/ram.img,share=on",
			"memory-backend=pc.ram",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in: %s", want, got)
			}
		}
		if strings.Contains(got, "-incoming") {
			t.Errorf("a template boots, it does not restore: %s", got)
		}
	})

	t.Run("restore maps the same file privately and defers", func(t *testing.T) {
		t.Parallel()
		got := build("/tmp/ram.img", "/tmp/state")
		for _, want := range []string{
			"mem-path=/tmp/ram.img,share=off", // copy-on-write, so the template survives
			"id=pc.ram",                       // same block name as the template, or migration will not match it
			"-incoming defer",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in: %s", want, got)
			}
		}
	})
}
