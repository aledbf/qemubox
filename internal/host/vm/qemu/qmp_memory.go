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

// SetPluggedMemory asks the machine's virtio-mem device for a total amount of
// memory beyond the boot size, in bytes, and reports what the device says it has
// after the request.
//
// One call, one number, in both directions. It replaces adding and removing
// pc-dimm devices in fixed slots, which cost this package a backend object and a
// device per step, a slot table to say which of the eight were in use, LIFO
// ordering so that unplug removed the newest, an online RPC into the guest after
// every add and an offline RPC before every remove, and a rollback path for each
// of the four ways that could half-fail. None of it exists here: the guest
// onlines what arrives by itself (memhp_default_state=online), and shrinking is
// the same call with a smaller number.
//
// **The answer is advisory and the request is not a promise.** virtio-mem is a
// negotiation with the guest: it plugs memory in blocks as the guest accepts
// them, and on the way down it can only take back what the guest has released.
// Asking for less than the guest is using is not an error and does not fail — it
// simply does not arrive, which is why the size afterwards is read back rather
// than assumed.
func (q *qmpClient) SetPluggedMemory(ctx context.Context, sizeBytes int64) (int64, error) {
	if sizeBytes < 0 {
		return 0, fmt.Errorf("negative memory request: %d bytes", sizeBytes)
	}

	log.G(ctx).WithFields(log.Fields{
		"requested_bytes": sizeBytes,
		"requested_mb":    sizeBytes / (1024 * 1024),
	}).Debug("qemu: asking virtio-mem for a new size")

	if err := q.QOMSet(ctx, virtioMemQOMPath, "requested-size", sizeBytes); err != nil {
		return 0, fmt.Errorf("requesting %d bytes from virtio-mem: %w", sizeBytes, err)
	}

	summary, err := q.QueryMemorySizeSummary(ctx)
	if err != nil {
		// The request went through; only the confirmation did not. Reporting the
		// requested size here would be inventing a measurement, so the caller is
		// told it does not know rather than told a number.
		return 0, fmt.Errorf("reading back the memory size after requesting %d bytes: %w", sizeBytes, err)
	}

	log.G(ctx).WithFields(log.Fields{
		"requested_mb": sizeBytes / (1024 * 1024),
		"plugged_mb":   summary.PluggedMemory / (1024 * 1024),
		"total_mb":     (summary.BaseMemory + summary.PluggedMemory) / (1024 * 1024),
	}).Info("qemu: virtio-mem size set")

	return summary.PluggedMemory, nil
}

// virtioMemQOMPath is where the machine puts its virtio-mem device. The id comes
// from machine.Spec.Args, which names it vmem0; QEMU exposes anything with an id
// under /machine/peripheral.
const virtioMemQOMPath = "/machine/peripheral/vmem0"
