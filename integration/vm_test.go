//go:build linux

package integration

import (
	"errors"
	"testing"

	"github.com/containerd/errdefs"

	systemapi "github.com/aledbf/qemubox/containerd/api/services/system/v1"
	"github.com/aledbf/qemubox/containerd/internal/host/vm"
)

func TestSystemInfo(t *testing.T) {
	runWithVM(t, func(t *testing.T, i vm.Instance) {
		t.Helper()
		client := i.Client()

		ss := systemapi.NewTTRPCSystemClient(client)

		resp, err := ss.Info(t.Context(), nil)
		if err != nil {
			t.Fatal("failed to get system info:", err)
		}
		if resp.Version != "dev" {
			t.Fatalf("unexpected version: %s, expected: dev", resp.Version)
		}
		t.Log("Kernel Version:", resp.KernelVersion)
	})
}

func TestStreamInitialization(t *testing.T) {
	runWithVM(t, func(t *testing.T, i vm.Instance) {
		t.Helper()
		sid1, conn, err := i.StartStream(t.Context())
		if err != nil {
			if errors.Is(err, errdefs.ErrNotImplemented) {
				t.Skip("streaming not implemented")
			}
			t.Fatal("failed to start stream client:", err)
		}

		if sid1 == 0 {
			t.Fatal("expected non-zero stream id")
		}

		if err := conn.Close(); err != nil {
			t.Fatal("failed to close stream connection:", err)
		}

		sid2, conn, err := i.StartStream(t.Context())
		if err != nil {
			t.Fatal("failed to start stream client:", err)
		}

		if sid2 <= sid1 {
			t.Fatalf("expected stream id %d, previous was %d", sid2, sid1)
		}

		if err := conn.Close(); err != nil {
			t.Fatal("failed to close stream connection:", err)
		}
	})
}
