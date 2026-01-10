//go:build linux

package qemu

import (
	"context"
	"fmt"
	"maps"

	"github.com/containerd/log"

	"github.com/spin-stack/spinbox/internal/host/vm"
)

// HotpluggableCPU describes an available CPU hotplug slot.
type HotpluggableCPU struct {
	Type       string         `json:"type"`
	QOMPath    string         `json:"qom-path"`
	Props      map[string]any `json:"props"`
	VCPUsCount int            `json:"vcpus-count"`
}

// QueryCPUs returns information about all vCPUs in the VM.
// Uses query-cpus-fast which is more efficient than query-cpus.
func (q *qmpClient) QueryCPUs(ctx context.Context) ([]vm.CPUInfo, error) {
	return qmpQuery[[]vm.CPUInfo](q, ctx, "query-cpus-fast")
}

// QueryHotpluggableCPUs returns available CPU hotplug slots.
func (q *qmpClient) QueryHotpluggableCPUs(ctx context.Context) ([]HotpluggableCPU, error) {
	return qmpQuery[[]HotpluggableCPU](q, ctx, "query-hotpluggable-cpus")
}

// HotplugCPU adds a new vCPU to the running VM.
// cpuID should be the next available CPU index (e.g., if you have CPUs 0-1, use cpuID=2).
func (q *qmpClient) HotplugCPU(ctx context.Context, cpuID int) error {
	beforeCount := -1
	if cpus, err := q.QueryCPUs(ctx); err == nil {
		beforeCount = len(cpus)
	}

	driver := "host-x86_64-cpu"
	args := map[string]any{
		"id":        fmt.Sprintf("cpu%d", cpuID),
		"socket-id": 0,
		"core-id":   cpuID,
		"thread-id": 0,
	}

	if cpus, err := q.QueryHotpluggableCPUs(ctx); err == nil {
		if len(cpus) == 0 {
			log.G(ctx).WithField("cpu_id", cpuID).
				Warn("qemu: no hotpluggable CPU slots reported by QEMU")
			return fmt.Errorf("no hotpluggable CPU slots reported by QEMU")
		}
		if match := matchHotpluggableCPU(cpus, cpuID); match != nil {
			driver = match.Type
			args = map[string]any{
				"id": fmt.Sprintf("cpu%d", cpuID),
			}
			maps.Copy(args, match.Props)
			log.G(ctx).WithFields(log.Fields{
				"cpu_id":   cpuID,
				"driver":   driver,
				"props":    match.Props,
				"qom_path": match.QOMPath,
			}).Debug("qemu: using hotpluggable CPU slot")
		} else {
			log.G(ctx).WithFields(log.Fields{
				"cpu_id": cpuID,
				"count":  len(cpus),
			}).Debug("qemu: no matching hotpluggable CPU slot; using default props")
		}
	} else {
		log.G(ctx).WithFields(log.Fields{
			"cpu_id": cpuID,
			"error":  err,
		}).Debug("qemu: failed to query hotpluggable CPUs; using default props")
	}

	log.G(ctx).WithFields(log.Fields{
		"cpu_id":    cpuID,
		"driver":    driver,
		"socket_id": args["socket-id"],
		"core_id":   args["core-id"],
		"thread_id": args["thread-id"],
	}).Debug("qemu: hotplugging vCPU")

	if err := q.DeviceAdd(ctx, driver, args); err != nil {
		return err
	}

	if beforeCount >= 0 {
		if cpus, err := q.QueryCPUs(ctx); err == nil {
			if len(cpus) <= beforeCount {
				log.G(ctx).WithFields(log.Fields{
					"cpu_id":       cpuID,
					"before_count": beforeCount,
					"after_count":  len(cpus),
				}).Warn("qemu: device_add did not increase CPU count")
				return fmt.Errorf("device_add did not increase CPU count")
			}
		}
	}

	return nil
}

// matchHotpluggableCPU finds a matching CPU slot for the given cpuID.
func matchHotpluggableCPU(cpus []HotpluggableCPU, cpuID int) *HotpluggableCPU {
	var fallback *HotpluggableCPU
	for i := range cpus {
		if cpus[i].QOMPath != "" {
			continue
		}
		props := cpus[i].Props
		if props == nil {
			continue
		}
		coreID, ok := intFromProp(props["core-id"])
		if ok && coreID == cpuID {
			return &cpus[i]
		}
		if fallback == nil {
			fallback = &cpus[i]
		}
	}
	return fallback
}

// intFromProp extracts an integer from a JSON-decoded value.
func intFromProp(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case uint32:
		return int(v), true
	case uint64:
		return int(v), true
	default:
		return 0, false
	}
}

// UnplugCPU removes a vCPU from the running VM.
// Note: CPU hot-unplug requires guest kernel support (CONFIG_HOTPLUG_CPU=y)
// and the CPU must be offline in the guest before removal.
func (q *qmpClient) UnplugCPU(ctx context.Context, cpuID int) error {
	deviceID := fmt.Sprintf("cpu%d", cpuID)

	log.G(ctx).WithFields(log.Fields{
		"cpu_id":    cpuID,
		"device_id": deviceID,
	}).Debug("qemu: unplugging vCPU")

	return q.DeviceDelete(ctx, deviceID)
}
