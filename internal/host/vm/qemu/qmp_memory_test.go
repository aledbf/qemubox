//go:build linux

package qemu

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const qmpTestSocketPath = "/tmp/test-qemu-qmp.sock"

// TestMemoryAlignmentValidation tests the 128MB alignment requirement.
// This is a unit test that doesn't require QEMU.
func TestMemoryAlignmentValidation(t *testing.T) {
	tests := []struct {
		name      string
		sizeBytes int64
		wantErr   bool
		errMsg    string
	}{
		{
			name:      "128MB aligned",
			sizeBytes: 128 * 1024 * 1024,
			wantErr:   false,
		},
		{
			name:      "256MB aligned",
			sizeBytes: 256 * 1024 * 1024,
			wantErr:   false,
		},
		{
			name:      "512MB aligned",
			sizeBytes: 512 * 1024 * 1024,
			wantErr:   false,
		},
		{
			name:      "1GB aligned",
			sizeBytes: 1024 * 1024 * 1024,
			wantErr:   false,
		},
		{
			name:      "64MB unaligned",
			sizeBytes: 64 * 1024 * 1024,
			wantErr:   true,
			errMsg:    "128MB aligned",
		},
		{
			name:      "100MB unaligned",
			sizeBytes: 100 * 1024 * 1024,
			wantErr:   true,
			errMsg:    "128MB aligned",
		},
		{
			name:      "200MB unaligned",
			sizeBytes: 200 * 1024 * 1024,
			wantErr:   true,
			errMsg:    "128MB aligned",
		},
		{
			name:      "1 byte unaligned",
			sizeBytes: 1,
			wantErr:   true,
			errMsg:    "128MB aligned",
		},
	}

	// Create a mock client that will fail on any actual QMP operation
	// The alignment check happens before any QMP call
	client := &qmpClient{}
	client.closed.Store(true) // Mark as closed so it fails fast if we reach QMP calls

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := client.HotplugMemory(context.Background(), 0, tt.sizeBytes)

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				// For aligned sizes, we expect to fail at the QMP call (client closed)
				// not at alignment validation
				if err != nil {
					assert.NotContains(t, err.Error(), "aligned")
				}
			}
		})
	}
}

// TestQMPMemoryHotplug tests the memory hotplug functionality via QMP
// This is an integration test that requires a running QEMU VM
func TestQMPMemoryHotplug(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Note: This test requires manual setup of a QEMU VM with QMP socket
	// For automated testing, use the full VM integration tests
	t.Skip("manual integration test - requires running QEMU VM")

	ctx := context.Background()

	// Connect to QMP (socket path from running VM)
	qmp, err := newQMPClient(ctx, qmpTestSocketPath)
	if err != nil {
		t.Fatalf("failed to connect to QMP: %v", err)
	}
	defer qmp.Close()

	// Query initial memory state
	initialSummary, err := qmp.QueryMemorySizeSummary(ctx)
	if err != nil {
		t.Fatalf("failed to query memory summary: %v", err)
	}

	initialTotalMB := (initialSummary.BaseMemory + initialSummary.PluggedMemory) / (1024 * 1024)
	t.Logf("Initial memory: base=%dMB, plugged=%dMB, total=%dMB",
		initialSummary.BaseMemory/(1024*1024),
		initialSummary.PluggedMemory/(1024*1024),
		initialTotalMB)

	// Hotplug 128MB memory
	const memoryToAdd = 128 * 1024 * 1024 // 128MB
	slotID := 0

	if err := qmp.HotplugMemory(ctx, slotID, memoryToAdd); err != nil {
		t.Fatalf("failed to hotplug memory: %v", err)
	}

	t.Logf("Hotplugged 128MB to slot %d", slotID)

	// Verify memory was added
	afterSummary, err := qmp.QueryMemorySizeSummary(ctx)
	if err != nil {
		t.Fatalf("failed to query memory summary after hotplug: %v", err)
	}

	afterTotalMB := (afterSummary.BaseMemory + afterSummary.PluggedMemory) / (1024 * 1024)
	expectedTotalMB := initialTotalMB + 128

	t.Logf("After hotplug: base=%dMB, plugged=%dMB, total=%dMB",
		afterSummary.BaseMemory/(1024*1024),
		afterSummary.PluggedMemory/(1024*1024),
		afterTotalMB)

	if afterTotalMB != expectedTotalMB {
		t.Errorf("expected %dMB total memory after hotplug, got %dMB", expectedTotalMB, afterTotalMB)
	}

	// Query memory devices
	devices, err := qmp.QueryMemoryDevices(ctx)
	if err != nil {
		t.Fatalf("failed to query memory devices: %v", err)
	}

	t.Logf("Memory devices: %d", len(devices))
	for i, dev := range devices {
		t.Logf("  Device %d: type=%s, data=%v", i, dev.Type, dev.Data)
	}

	// Try to unplug the memory (may not work if memory is in use)
	if err := qmp.UnplugMemory(ctx, slotID); err != nil {
		t.Logf("Memory unplug failed (expected if memory in use): %v", err)
		// Don't fail the test - memory hot-unplug may fail if pages are in use
	} else {
		t.Logf("Successfully unplugged memory from slot %d", slotID)

		// Verify memory was removed
		finalSummary, err := qmp.QueryMemorySizeSummary(ctx)
		if err != nil {
			t.Fatalf("failed to query memory summary after unplug: %v", err)
		}

		finalTotalMB := (finalSummary.BaseMemory + finalSummary.PluggedMemory) / (1024 * 1024)
		if finalTotalMB != initialTotalMB {
			t.Logf("Warning: expected %dMB after unplug, got %dMB", initialTotalMB, finalTotalMB)
		}
	}
}

// TestQueryMemorySizeSummary tests the query-memory-size-summary command
func TestQueryMemorySizeSummary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Skip("manual integration test - requires running QEMU VM")

	ctx := context.Background()

	qmp, err := newQMPClient(ctx, qmpTestSocketPath)
	if err != nil {
		t.Fatalf("failed to connect to QMP: %v", err)
	}
	defer qmp.Close()

	summary, err := qmp.QueryMemorySizeSummary(ctx)
	if err != nil {
		t.Fatalf("failed to query memory summary: %v", err)
	}

	t.Logf("Memory summary:")
	t.Logf("  Base memory: %d bytes (%d MB)", summary.BaseMemory, summary.BaseMemory/(1024*1024))
	t.Logf("  Plugged memory: %d bytes (%d MB)", summary.PluggedMemory, summary.PluggedMemory/(1024*1024))
	t.Logf("  Total: %d bytes (%d MB)",
		summary.BaseMemory+summary.PluggedMemory,
		(summary.BaseMemory+summary.PluggedMemory)/(1024*1024))

	if summary.BaseMemory <= 0 {
		t.Errorf("expected positive base memory, got %d", summary.BaseMemory)
	}
}

// TestQueryMemoryDevices tests the query-memory-devices command
func TestQueryMemoryDevices(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Skip("manual integration test - requires running QEMU VM")

	ctx := context.Background()

	qmp, err := newQMPClient(ctx, qmpTestSocketPath)
	if err != nil {
		t.Fatalf("failed to connect to QMP: %v", err)
	}
	defer qmp.Close()

	devices, err := qmp.QueryMemoryDevices(ctx)
	if err != nil {
		t.Fatalf("failed to query memory devices: %v", err)
	}

	t.Logf("Found %d memory devices:", len(devices))
	for i, dev := range devices {
		t.Logf("  Device %d:", i)
		t.Logf("    Type: %s", dev.Type)
		t.Logf("    Data: %v", dev.Data)
	}
}

// TestMemoryHotplugAlignment tests that memory size validation works
func TestMemoryHotplugAlignment(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Skip("manual integration test - requires running QEMU VM")

	ctx := context.Background()

	qmp, err := newQMPClient(ctx, qmpTestSocketPath)
	if err != nil {
		t.Fatalf("failed to connect to QMP: %v", err)
	}
	defer qmp.Close()

	// Test cases for memory alignment
	testCases := []struct {
		name      string
		sizeBytes int64
		shouldErr bool
	}{
		{
			name:      "128MB aligned - valid",
			sizeBytes: 128 * 1024 * 1024,
			shouldErr: false,
		},
		{
			name:      "256MB aligned - valid",
			sizeBytes: 256 * 1024 * 1024,
			shouldErr: false,
		},
		{
			name:      "100MB unaligned - invalid",
			sizeBytes: 100 * 1024 * 1024,
			shouldErr: true,
		},
		{
			name:      "64MB unaligned - invalid",
			sizeBytes: 64 * 1024 * 1024,
			shouldErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := qmp.HotplugMemory(ctx, 0, tc.sizeBytes)
			if tc.shouldErr {
				if err == nil {
					t.Errorf("expected error for %dB memory, got nil", tc.sizeBytes)
				} else {
					t.Logf("Got expected error: %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("expected no error for %dB memory, got: %v", tc.sizeBytes, err)
				}
			}
		})
	}
}

// TestObjectAddDel tests QMP object-add and object-del commands
func TestObjectAddDel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Skip("manual integration test - requires running QEMU VM")

	ctx := context.Background()

	qmp, err := newQMPClient(ctx, qmpTestSocketPath)
	if err != nil {
		t.Fatalf("failed to connect to QMP: %v", err)
	}
	defer qmp.Close()

	// Add a memory backend
	backendID := "test-mem-backend"
	args := map[string]any{
		"size": int64(128 * 1024 * 1024), // 128MB
	}

	if err := qmp.ObjectAdd(ctx, "memory-backend-ram", backendID, args); err != nil {
		t.Fatalf("failed to add memory backend: %v", err)
	}

	t.Logf("Added memory backend: %s", backendID)

	// Delete the memory backend
	if err := qmp.ObjectDel(ctx, backendID); err != nil {
		t.Fatalf("failed to delete memory backend: %v", err)
	}

	t.Logf("Deleted memory backend: %s", backendID)
}
