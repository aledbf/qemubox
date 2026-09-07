//go:build integration

package integration

import "testing"

// TestParseBootTimeline pins the parser against a real journal line.
//
// It exists because the shim gained two fields in the middle of its
// BOOT_TIMELINE line and the parser, which spelled the fields out positionally,
// stopped matching. The failure was reported as "no BOOT_TIMELINE line in the
// spinbox journal" against a journal that was full of them, and nothing about it
// pointed at the format.
func TestParseBootTimeline(t *testing.T) {
	t.Parallel()

	t.Run("current format", func(t *testing.T) {
		t.Parallel()
		const line = `level=info msg="BOOT_TIMELINE qemu_launch_us=26505 qmp_socket_us=7726 ` +
			`qmp_handshake_us=18778 guest_boot_us=133103 total_us=159608" runtime=io.containerd.spinbox.v1`

		tl, ok := parseLastBootTimeline(line)
		if !ok {
			t.Fatal("did not match a line the shim actually emits")
		}
		want := bootTimeline{
			qemuLaunchUS:   26505,
			qmpSocketUS:    7726,
			qmpHandshakeUS: 18778,
			guestBootUS:    133103,
			totalUS:        159608,
		}
		if tl != want {
			t.Errorf("parsed %+v, want %+v", tl, want)
		}
	})

	t.Run("a line missing the breakdown still parses", func(t *testing.T) {
		t.Parallel()
		const line = `msg="BOOT_TIMELINE qemu_launch_us=8123 guest_boot_us=146210 total_us=154333"`

		tl, ok := parseLastBootTimeline(line)
		if !ok {
			t.Fatal("a BOOT_TIMELINE line carrying fewer fields should still parse")
		}
		if tl.totalUS != 154333 || tl.qemuLaunchUS != 8123 {
			t.Errorf("parsed %+v", tl)
		}
		if tl.qmpSocketUS != 0 {
			t.Errorf("absent fields should be zero, got %+v", tl)
		}
	})

	t.Run("the last line wins", func(t *testing.T) {
		t.Parallel()
		const lines = `BOOT_TIMELINE qemu_launch_us=1 guest_boot_us=2 total_us=3
BOOT_TIMELINE qemu_launch_us=4 guest_boot_us=5 total_us=6`

		tl, ok := parseLastBootTimeline(lines)
		if !ok || tl.totalUS != 6 {
			t.Errorf("parsed %+v ok=%v, want the most recent boot", tl, ok)
		}
	})
}
