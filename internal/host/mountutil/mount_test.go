//go:build linux

package mountutil

import (
	"context"
	"os"
	"strings"
	"testing"

	types "github.com/containerd/containerd/api/types"
)

func TestAllCleanupUnmounts(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to perform bind mounts")
	}

	ctx := context.Background()
	rootfs := t.TempDir()
	source := t.TempDir()
	mountDir := t.TempDir()

	if isMountPoint(rootfs) {
		t.Fatalf("expected %s to not be a mountpoint before test", rootfs)
	}

	cleanup, err := All(ctx, rootfs, mountDir, []*types.Mount{{
		Type:    "bind",
		Source:  source,
		Options: []string{"rbind", "rw"},
	}})
	if err != nil {
		t.Fatalf("All() failed: %v", err)
	}
	if cleanup == nil {
		t.Fatal("expected cleanup function, got nil")
	}

	if !isMountPoint(rootfs) {
		t.Fatalf("expected %s to be a mountpoint after mount", rootfs)
	}

	if err := cleanup(ctx); err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}

	if isMountPoint(rootfs) {
		t.Fatalf("expected %s to be unmounted after cleanup", rootfs)
	}
}

func isMountPoint(path string) bool {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if fields[4] == path {
			return true
		}
	}
	return false
}
