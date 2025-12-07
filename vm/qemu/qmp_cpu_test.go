package qemu

import (
	"context"
	"testing"
)

// TestQMPCPUHotplug tests the CPU hotplug functionality via QMP
// This is an integration test that requires a running QEMU VM
func TestQMPCPUHotplug(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Note: This test requires manual setup of a QEMU VM with QMP socket
	// For automated testing, use the full VM integration tests
	t.Skip("manual integration test - requires running QEMU VM")

	ctx := context.Background()

	// Connect to QMP (socket path from running VM)
	qmpSocketPath := "/tmp/test-qemu-qmp.sock"
	qmp, err := NewQMPClient(ctx, qmpSocketPath)
	if err != nil {
		t.Fatalf("failed to connect to QMP: %v", err)
	}
	defer qmp.Close()

	// Query initial CPU count
	cpus, err := qmp.QueryCPUs(ctx)
	if err != nil {
		t.Fatalf("failed to query CPUs: %v", err)
	}

	initialCount := len(cpus)
	t.Logf("Initial CPU count: %d", initialCount)

	// Hotplug a new CPU
	newCPUID := initialCount
	if err := qmp.HotplugCPU(ctx, newCPUID); err != nil {
		t.Fatalf("failed to hotplug CPU %d: %v", newCPUID, err)
	}

	t.Logf("Hotplugged CPU %d", newCPUID)

	// Verify CPU was added
	cpus, err = qmp.QueryCPUs(ctx)
	if err != nil {
		t.Fatalf("failed to query CPUs after hotplug: %v", err)
	}

	if len(cpus) != initialCount+1 {
		t.Errorf("expected %d CPUs after hotplug, got %d", initialCount+1, len(cpus))
	}

	// Try to unplug the CPU (may not work on all kernels)
	if err := qmp.UnplugCPU(ctx, newCPUID); err != nil {
		t.Logf("CPU unplug failed (expected on many kernels): %v", err)
		// Don't fail the test - CPU hot-unplug is optional
	} else {
		t.Logf("Successfully unplugged CPU %d", newCPUID)

		// Verify CPU was removed
		cpus, err = qmp.QueryCPUs(ctx)
		if err != nil {
			t.Fatalf("failed to query CPUs after unplug: %v", err)
		}

		if len(cpus) != initialCount {
			t.Logf("Warning: expected %d CPUs after unplug, got %d", initialCount, len(cpus))
		}
	}
}

// TestQueryCPUs tests the query-cpus-fast command
func TestQueryCPUs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Skip("manual integration test - requires running QEMU VM")

	ctx := context.Background()

	qmpSocketPath := "/tmp/test-qemu-qmp.sock"
	qmp, err := NewQMPClient(ctx, qmpSocketPath)
	if err != nil {
		t.Fatalf("failed to connect to QMP: %v", err)
	}
	defer qmp.Close()

	cpus, err := qmp.QueryCPUs(ctx)
	if err != nil {
		t.Fatalf("failed to query CPUs: %v", err)
	}

	t.Logf("Found %d CPUs:", len(cpus))
	for _, cpu := range cpus {
		t.Logf("  CPU %d: thread=%d, path=%s, target=%s",
			cpu.CPUIndex, cpu.Thread, cpu.QOMPath, cpu.Target)
	}

	if len(cpus) == 0 {
		t.Error("expected at least 1 CPU")
	}
}
