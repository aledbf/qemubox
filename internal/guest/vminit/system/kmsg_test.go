//go:build linux

package system

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseKmsgRecord(t *testing.T) {
	tests := []struct {
		name    string
		record  string
		wantTS  int64
		wantMsg string
		wantOK  bool
	}{
		{
			name:    "initcall record",
			record:  "6,123,187940,-;initcall pci_subsys_init+0x0/0x40 returned 0 after 12345 usecs",
			wantTS:  187940,
			wantMsg: "initcall pci_subsys_init+0x0/0x40 returned 0 after 12345 usecs",
			wantOK:  true,
		},
		{
			name:    "trailing newline and continuation dropped",
			record:  "5,80,5085,-;EXT4-fs (vdb): mounted\n SUBSYSTEM=block\n DEVICE=b259:0",
			wantTS:  5085,
			wantMsg: "EXT4-fs (vdb): mounted",
			wantOK:  true,
		},
		{
			name:   "no semicolon",
			record: "garbage without separator",
			wantOK: false,
		},
		{
			name:    "header too short still yields message",
			record:  "6;some message",
			wantTS:  0,
			wantMsg: "some message",
			wantOK:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, msg, ok := parseKmsgRecord(tt.record)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.wantTS, ts)
				assert.Equal(t, tt.wantMsg, msg)
			}
		})
	}
}

func TestExtractInitcall(t *testing.T) {
	t.Run("match", func(t *testing.T) {
		name, usec, ok := extractInitcall("initcall acpi_init+0x0/0x460 returned 0 after 39000 usecs")
		assert.True(t, ok)
		assert.Equal(t, "acpi_init+0x0/0x460", name)
		assert.Equal(t, 39000, usec)
	})

	t.Run("non-initcall message", func(t *testing.T) {
		_, _, ok := extractInitcall("erofs (device vda): mounted with root inode @ nid 60")
		assert.False(t, ok)
	})

	t.Run("calling line is not a completion", func(t *testing.T) {
		_, _, ok := extractInitcall("calling  acpi_init+0x0/0x460 @ 1")
		assert.False(t, ok)
	})
}

// The gaps are the point of the whole dump: the initcalls name half of boot, and
// the other half is only visible as time in which the kernel said nothing.
func TestKmsgGapsReportSilenceThatBelongsToNoInitcall(t *testing.T) {
	records := []kmsgRecord{
		{tsUS: 1_000, msg: "Memory: 498M/512M available"},
		// 20 ms of silence after a message that is not an initcall announcement:
		// this is exactly what the profile cannot otherwise see.
		{tsUS: 21_000, msg: "smp: Bringing up secondary CPUs ..."},
		// A gap that opens after `calling` is the initcall itself, which is
		// already reported by name and duration. Listing it again would fill the
		// output with the entries the profile already has.
		{tsUS: 22_000, msg: "calling  acpi_init+0x0/0x460 @ 1"},
		{tsUS: 40_000, msg: "initcall acpi_init+0x0/0x460 returned 0 after 18000 usecs"},
		// Below the floor: not worth a line.
		{tsUS: 40_100, msg: "clocksource: Switched to clocksource kvm-clock"},
	}

	gaps := kmsgGaps(records)

	// Two silences belong to no initcall: the 20 ms one, and the 1 ms before the
	// call was announced. The 18 ms *inside* acpi_init is not one of them.
	if len(gaps) != 2 {
		t.Fatalf("gaps = %d, want the two silences no initcall explains: %+v", len(gaps), gaps)
	}
	if gaps[0].usec != 20_000 {
		t.Errorf("largest gap = %d us, want 20000", gaps[0].usec)
	}
	if gaps[0].after != "Memory: 498M/512M available" {
		t.Errorf("gap named %q, want the last thing the kernel said before it", gaps[0].after)
	}
	for _, g := range gaps {
		if g.usec == 18_000 {
			t.Errorf("the 18 ms inside acpi_init was listed as a gap; it is already reported as an initcall")
		}
	}
}

// And the ordering, because a top-N list that is not sorted is a list of the
// first N.
func TestKmsgGapsAreLargestFirst(t *testing.T) {
	records := []kmsgRecord{
		{tsUS: 0, msg: "a"},
		{tsUS: 1_000, msg: "b"},
		{tsUS: 11_000, msg: "c"},
		{tsUS: 14_000, msg: "d"},
	}

	gaps := kmsgGaps(records)

	if len(gaps) != 3 {
		t.Fatalf("gaps = %+v, want three", gaps)
	}
	if gaps[0].usec != 10_000 || gaps[1].usec != 3_000 || gaps[2].usec != 1_000 {
		t.Errorf("gaps = %+v, want 10000, 3000, 1000 in that order", gaps)
	}
}
