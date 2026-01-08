//go:build linux

package qemu

import (
	"context"
	"fmt"

	"github.com/containerd/log"
)

// MemoryDeviceInfo represents a hotplugged memory device.
type MemoryDeviceInfo struct {
	Type string         `json:"type"` // "dimm" or "virtio-mem"
	Data map[string]any `json:"data"`
}

// QueryMemoryDevices returns all hotplugged memory devices.
func (q *qmpClient) QueryMemoryDevices(ctx context.Context) ([]MemoryDeviceInfo, error) {
	return qmpQuery[[]MemoryDeviceInfo](q, ctx, "query-memory-devices")
}

// QueryMemorySizeSummary returns memory usage summary.
func (q *qmpClient) QueryMemorySizeSummary(ctx context.Context) (*MemorySizeSummary, error) {
	return qmpQuery[*MemorySizeSummary](q, ctx, "query-memory-size-summary")
}

// HotplugMemory adds memory to the VM using pc-dimm.
// slotID: memory slot index (0-7 based on -m slots=8)
// sizeBytes: memory size in bytes (must be 128MB aligned)
func (q *qmpClient) HotplugMemory(ctx context.Context, slotID int, sizeBytes int64) error {
	// Validate 128MB alignment
	const alignmentMB = 128
	const alignmentBytes = alignmentMB * 1024 * 1024
	if sizeBytes%alignmentBytes != 0 {
		return fmt.Errorf("memory size must be %dMB aligned, got %d bytes", alignmentMB, sizeBytes)
	}

	backendID := fmt.Sprintf("mem%d", slotID)
	dimmID := fmt.Sprintf("dimm%d", slotID)

	// Query current state before adding
	beforeSummary, err := q.QueryMemorySizeSummary(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Warn("qemu: failed to query memory before hotplug")
	}

	// Step 1: Create memory backend object
	backendArgs := map[string]any{
		"size": sizeBytes,
	}

	log.G(ctx).WithFields(log.Fields{
		"slot_id":    slotID,
		"size_bytes": sizeBytes,
		"size_mb":    sizeBytes / (1024 * 1024),
		"backend_id": backendID,
	}).Debug("qemu: creating memory backend")

	if err := q.ObjectAdd(ctx, "memory-backend-ram", backendID, backendArgs); err != nil {
		return fmt.Errorf("failed to create memory backend: %w", err)
	}

	// Step 2: Hotplug pc-dimm device
	dimmArgs := map[string]any{
		"id":     dimmID,
		"memdev": backendID,
	}

	log.G(ctx).WithFields(log.Fields{
		"slot_id": slotID,
		"dimm_id": dimmID,
	}).Debug("qemu: hotplugging memory device")

	if err := q.DeviceAdd(ctx, "pc-dimm", dimmArgs); err != nil {
		// Cleanup backend on failure
		if delErr := q.ObjectDel(ctx, backendID); delErr != nil {
			log.G(ctx).WithError(delErr).Warn("qemu: failed to cleanup memory backend after device_add failure")
		}
		return fmt.Errorf("failed to hotplug memory device: %w", err)
	}

	// Verify memory was added
	if beforeSummary != nil {
		afterSummary, err := q.QueryMemorySizeSummary(ctx)
		if err == nil {
			if afterSummary.BaseMemory+afterSummary.PluggedMemory <= beforeSummary.BaseMemory+beforeSummary.PluggedMemory {
				log.G(ctx).WithFields(log.Fields{
					"slot_id":       slotID,
					"before_total":  beforeSummary.BaseMemory + beforeSummary.PluggedMemory,
					"after_total":   afterSummary.BaseMemory + afterSummary.PluggedMemory,
					"expected_size": sizeBytes,
				}).Warn("qemu: device_add did not increase memory size")
				return fmt.Errorf("device_add did not increase memory size")
			}
			log.G(ctx).WithFields(log.Fields{
				"slot_id":    slotID,
				"added_mb":   sizeBytes / (1024 * 1024),
				"total_mb":   (afterSummary.BaseMemory + afterSummary.PluggedMemory) / (1024 * 1024),
				"plugged_mb": afterSummary.PluggedMemory / (1024 * 1024),
			}).Info("qemu: memory hotplug successful")
		}
	}

	return nil
}

// UnplugMemory removes memory from the VM.
// slotID: memory slot to remove
// Note: Memory hot-unplug requires guest kernel support (CONFIG_MEMORY_HOTREMOVE=y)
// and the memory must be offline in the guest before removal.
func (q *qmpClient) UnplugMemory(ctx context.Context, slotID int) error {
	dimmID := fmt.Sprintf("dimm%d", slotID)
	backendID := fmt.Sprintf("mem%d", slotID)

	log.G(ctx).WithFields(log.Fields{
		"slot_id": slotID,
		"dimm_id": dimmID,
	}).Debug("qemu: unplugging memory device")

	// Step 1: Remove device
	if err := q.DeviceDelete(ctx, dimmID); err != nil {
		return fmt.Errorf("failed to unplug memory device: %w", err)
	}

	// Step 2: Remove backend object
	// Note: QEMU may need time to complete device removal before backend deletion
	// We'll attempt to delete the backend, but it's not critical if it fails
	if err := q.ObjectDel(ctx, backendID); err != nil {
		log.G(ctx).WithError(err).WithField("backend_id", backendID).
			Warn("qemu: failed to delete memory backend (non-fatal)")
	}

	return nil
}
