//go:build linux

package integration

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/containerd/errdefs"

	systemapi "github.com/aledbf/qemubox/containerd/api/services/system/v1"
	"github.com/aledbf/qemubox/containerd/internal/host/vm"
)

func TestSystemInfo(t *testing.T) {
	runWithVM(t, func(ctx context.Context, t *testing.T, instance vm.Instance) {
		t.Helper()

		client, err := instance.Client()
		if err != nil {
			t.Fatalf("get client: %v", err)
		}
		ss := systemapi.NewTTRPCSystemClient(client)

		resp, err := ss.Info(ctx, nil)
		if err != nil {
			t.Fatalf("get system info: %v", err)
		}

		// Verify version is not empty
		if resp.Version == "" {
			t.Fatal("system version must not be empty")
		}

		// Verify kernel version is populated
		if resp.KernelVersion == "" {
			t.Fatal("kernel version must not be empty")
		}

		t.Logf("system version: %s", resp.Version)
		t.Logf("kernel version: %s", resp.KernelVersion)

		// If QEMUBOX_VERSION is set, verify it matches
		if expectedVersion := os.Getenv("QEMUBOX_VERSION"); expectedVersion != "" {
			if resp.Version != expectedVersion {
				t.Fatalf("version mismatch: got %q, want %q", resp.Version, expectedVersion)
			}
		}
	})
}

func TestStreamInitialization(t *testing.T) {
	runWithVM(t, func(ctx context.Context, t *testing.T, instance vm.Instance) {
		t.Helper()

		// Start first stream
		sid1, conn, err := instance.StartStream(ctx)
		if err != nil {
			if errors.Is(err, errdefs.ErrNotImplemented) {
				t.Skip("streaming not implemented")
			}
			t.Fatalf("start first stream: %v", err)
		}

		if sid1 == 0 {
			t.Fatal("stream ID must be non-zero")
		}

		if err := conn.Close(); err != nil {
			t.Fatalf("close first stream connection: %v", err)
		}

		// Start second stream
		sid2, conn, err := instance.StartStream(ctx)
		if err != nil {
			t.Fatalf("start second stream: %v", err)
		}

		if sid2 <= sid1 {
			t.Fatalf("stream IDs must increase: got %d after %d", sid2, sid1)
		}

		if err := conn.Close(); err != nil {
			t.Fatalf("close second stream connection: %v", err)
		}

		t.Logf("stream IDs allocated correctly: %d -> %d", sid1, sid2)
	})
}

// TestStreamConcurrent tests that multiple concurrent streams can be created.
func TestStreamConcurrent(t *testing.T) {
	runWithVM(t, func(ctx context.Context, t *testing.T, instance vm.Instance) {
		t.Helper()

		const numStreams = 5
		streams := make([]struct {
			id   uint32
			conn interface{ Close() error }
		}, numStreams)

		// Create multiple streams concurrently
		for i := range numStreams {
			sid, conn, err := instance.StartStream(ctx)
			if err != nil {
				if errors.Is(err, errdefs.ErrNotImplemented) {
					t.Skip("streaming not implemented")
				}
				t.Fatalf("start stream %d: %v", i, err)
			}

			if sid == 0 {
				t.Fatalf("stream %d: ID must be non-zero", i)
			}

			streams[i].id = sid
			streams[i].conn = conn

			t.Logf("stream %d: ID %d", i, sid)
		}

		// Verify all IDs are unique
		seen := make(map[uint32]bool)
		for i, s := range streams {
			if seen[s.id] {
				t.Fatalf("duplicate stream ID %d at index %d", s.id, i)
			}
			seen[s.id] = true
		}

		// Close all connections
		for i, s := range streams {
			if err := s.conn.Close(); err != nil {
				t.Errorf("close stream %d (ID %d): %v", i, s.id, err)
			}
		}

		t.Logf("created %d concurrent streams with unique IDs", numStreams)
	})
}

// TestVMResourceConfig tests VM creation with custom resource configuration.
func TestVMResourceConfig(t *testing.T) {
	cfg := vmTestConfig{
		BootCPUs:          2,
		MaxCPUs:           4,
		MemorySize:        1024 * 1024 * 1024, // 1 GiB
		MemoryHotplugSize: 2048 * 1024 * 1024, // 2 GiB
	}

	runWithVMConfig(t, cfg, func(ctx context.Context, t *testing.T, instance vm.Instance) {
		t.Helper()

		client, err := instance.Client()
		if err != nil {
			t.Fatalf("get client: %v", err)
		}
		ss := systemapi.NewTTRPCSystemClient(client)

		resp, err := ss.Info(ctx, nil)
		if err != nil {
			t.Fatalf("get system info: %v", err)
		}

		t.Logf("VM started with custom resources: version=%s kernel=%s",
			resp.Version, resp.KernelVersion)
	})
}
