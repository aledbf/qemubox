//go:build linux

package qemu

import (
	"strings"
	"testing"
)

// The command line is where the snapshot design is either right or wrong: the
// template and the restore have to agree on the RAM object's name and disagree
// on how it is mapped, and a VM that does neither must come out unchanged.
func TestSnapshotCommandLine(t *testing.T) {
	t.Parallel()

	build := func(memFile, restore string) string {
		b := newQemuCommandBuilder().
			setMachine("q35", "accel=kvm", machineMemoryBackend(memFile)).
			setMemory(512, 0, 0)
		if memFile != "" {
			b.setMemoryBackendFile(memFile, 512, restore == "")
		}
		if restore != "" {
			b.setIncomingDefer()
		}
		return strings.Join(b.build(), " ")
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
		if !strings.Contains(got, "-machine q35,accel=kvm ") {
			t.Errorf("machine option list should have no trailing comma: %s", got)
		}
	})

	t.Run("template shares its RAM file", func(t *testing.T) {
		t.Parallel()
		got := build("/tmp/ram.img", "")
		for _, want := range []string{
			"memory-backend-file,id=pc.ram,size=512M,mem-path=/tmp/ram.img,share=on",
			"-machine q35,accel=kvm,memory-backend=pc.ram",
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
