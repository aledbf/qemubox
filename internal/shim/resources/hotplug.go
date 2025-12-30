package resources

import (
	"context"
	"fmt"
	"time"

	"github.com/containerd/log"
	"github.com/containerd/ttrpc"

	"github.com/aledbf/qemubox/containerd/internal/config"
	"github.com/aledbf/qemubox/containerd/internal/host/vm"
	"github.com/aledbf/qemubox/containerd/internal/host/vm/qemu"
	"github.com/aledbf/qemubox/containerd/internal/shim/cpuhotplug"
	"github.com/aledbf/qemubox/containerd/internal/shim/memhotplug"
)

// HotplugCallbacks provides callbacks for hotplug controllers to interact with the VM.
type HotplugCallbacks struct {
	GetCPUStats    func(ctx context.Context, containerID string) (uint64, uint64, error)
	OfflineCPU     func(ctx context.Context, cpuID int) error
	OnlineCPU      func(ctx context.Context, cpuID int) error
	GetMemoryStats func(ctx context.Context, containerID string) (int64, error)
	OfflineMemory  func(ctx context.Context, memoryID int) error
	OnlineMemory   func(ctx context.Context, memoryID int) error
}

// StartCPUHotplug starts the CPU hotplug controller for QEMU VMs.
// It only starts if:
// - VM is a QEMU instance
// - MaxCPUs > BootCPUs (hotplug is possible)
// Returns the controller if started, nil otherwise.
func StartCPUHotplug(
	ctx context.Context,
	containerID string,
	vmi vm.Instance,
	resourceCfg *vm.VMResourceConfig,
	callbacks HotplugCallbacks,
) cpuhotplug.CPUHotplugController {
	// Only enable for VMs that support hotplug (MaxCPUs > BootCPUs)
	if resourceCfg.MaxCPUs <= resourceCfg.BootCPUs {
		log.G(ctx).WithFields(log.Fields{
			"boot_cpus": resourceCfg.BootCPUs,
			"max_cpus":  resourceCfg.MaxCPUs,
		}).Debug("cpu-hotplug: not enabled (MaxCPUs == BootCPUs, no room for scaling)")
		return nil
	}

	// Type assert to check if this is a QEMU instance (only QEMU supports CPU hotplug currently)
	qemuVM, ok := vmi.(*qemu.Instance)
	if !ok {
		return nil
	}

	// Get QMP client
	qmpClient := qemuVM.QMPClient()
	if qmpClient == nil {
		log.G(ctx).Warn("cpu-hotplug: QMP client not available")
		return nil
	}

	if cpus, err := qmpClient.QueryHotpluggableCPUs(ctx); err != nil {
		log.G(ctx).WithError(err).Warn("cpu-hotplug: failed to query hotpluggable CPUs")
	} else {
		sample := ""
		if len(cpus) > 0 {
			sample = fmt.Sprintf("type=%s props=%v", cpus[0].Type, cpus[0].Props)
		}
		log.G(ctx).WithFields(log.Fields{
			"count":  len(cpus),
			"sample": sample,
		}).Debug("cpu-hotplug: hotpluggable CPU slots")
	}

	// Create controller configuration from config file
	var cpuConfig cpuhotplug.Config
	cfg, err := config.Get()
	if err != nil {
		log.G(ctx).WithError(err).Error("cpu-hotplug: failed to load config, using defaults")
		cpuConfig = cpuhotplug.DefaultConfig()
	} else {
		// Parse durations from config
		monitorInterval, err := time.ParseDuration(cfg.CPUHotplug.MonitorInterval)
		if err != nil {
			log.G(ctx).WithError(err).Error("cpu-hotplug: invalid monitor_interval, using defaults")
			cpuConfig = cpuhotplug.DefaultConfig()
		} else {
			scaleUpCooldown, err1 := time.ParseDuration(cfg.CPUHotplug.ScaleUpCooldown)
			scaleDownCooldown, err2 := time.ParseDuration(cfg.CPUHotplug.ScaleDownCooldown)

			if err1 != nil || err2 != nil {
				log.G(ctx).WithFields(log.Fields{
					"scale_up_err":   err1,
					"scale_down_err": err2,
				}).Error("cpu-hotplug: invalid cooldown durations, using defaults")
				cpuConfig = cpuhotplug.DefaultConfig()
			} else {
				cpuConfig = cpuhotplug.Config{
					MonitorInterval:      monitorInterval,
					ScaleUpCooldown:      scaleUpCooldown,
					ScaleDownCooldown:    scaleDownCooldown,
					ScaleUpThreshold:     cfg.CPUHotplug.ScaleUpThreshold,
					ScaleDownThreshold:   cfg.CPUHotplug.ScaleDownThreshold,
					ScaleUpThrottleLimit: cfg.CPUHotplug.ScaleUpThrottleLimit,
					ScaleUpStability:     cfg.CPUHotplug.ScaleUpStability,
					ScaleDownStability:   cfg.CPUHotplug.ScaleDownStability,
					EnableScaleDown:      cfg.CPUHotplug.EnableScaleDown,
				}
			}
		}
	}

	hotplugger, err := vmi.CPUHotplugger()
	if err != nil {
		log.G(ctx).WithError(err).Warn("cpu-hotplug: failed to get CPU hotplugger")
		return nil
	}

	controller := cpuhotplug.NewController(
		containerID,
		hotplugger,
		func(ctx context.Context) (uint64, uint64, error) {
			return callbacks.GetCPUStats(ctx, containerID)
		},
		callbacks.OfflineCPU,
		callbacks.OnlineCPU,
		resourceCfg.BootCPUs,
		resourceCfg.MaxCPUs,
		cpuConfig,
	)

	// Start monitoring loop with detached context (not request context)
	// The controller needs to run for the lifetime of the container, not just the CreateTask RPC.
	// Cleanup happens via controller.Stop() in shutdown/delete, not via context cancellation.
	// Note: controller.Start() is non-blocking and launches its own goroutine internally.
	// The controller manages its own lifecycle and will log any errors in its monitor loop.
	controller.Start(context.WithoutCancel(ctx))

	log.G(ctx).WithFields(log.Fields{
		"container_id": containerID,
		"boot_cpus":    resourceCfg.BootCPUs,
		"max_cpus":     resourceCfg.MaxCPUs,
	}).Info("cpu-hotplug: controller started (monitoring in background)")

	return controller
}

// StartMemoryHotplug starts the memory hotplug controller for QEMU VMs.
// Returns the controller if started, nil otherwise.
func StartMemoryHotplug(
	ctx context.Context,
	containerID string,
	vmi vm.Instance,
	resourceCfg *vm.VMResourceConfig,
	callbacks HotplugCallbacks,
) memhotplug.MemoryHotplugController {
	// Type assert to check if this is a QEMU instance (only QEMU supports memory hotplug currently)
	qemuVM, ok := vmi.(*qemu.Instance)
	if !ok {
		return nil
	}

	// Get QMP client
	qmpClient := qemuVM.QMPClient()
	if qmpClient == nil {
		log.G(ctx).Warn("memory-hotplug: QMP client not available")
		return nil
	}

	// Query current memory configuration
	if summary, err := qmpClient.QueryMemorySizeSummary(ctx); err != nil {
		log.G(ctx).WithError(err).Warn("memory-hotplug: failed to query memory size")
	} else {
		log.G(ctx).WithFields(log.Fields{
			"base_memory_mb":    summary.BaseMemory / (1024 * 1024),
			"plugged_memory_mb": summary.PluggedMemory / (1024 * 1024),
		}).Debug("memory-hotplug: initial memory state")
	}

	// Create controller configuration from config file
	var memConfig memhotplug.Config
	cfg, err := config.Get()
	if err != nil {
		log.G(ctx).WithError(err).Error("memory-hotplug: failed to load config, using defaults")
		memConfig = memhotplug.DefaultConfig()
	} else {
		// Parse durations from config
		monitorInterval, err := time.ParseDuration(cfg.MemHotplug.MonitorInterval)
		if err != nil {
			log.G(ctx).WithError(err).Error("memory-hotplug: invalid monitor_interval, using defaults")
			memConfig = memhotplug.DefaultConfig()
		} else {
			scaleUpCooldown, _ := time.ParseDuration(cfg.MemHotplug.ScaleUpCooldown)
			scaleDownCooldown, _ := time.ParseDuration(cfg.MemHotplug.ScaleDownCooldown)

			memConfig = memhotplug.Config{
				MonitorInterval:    monitorInterval,
				ScaleUpCooldown:    scaleUpCooldown,
				ScaleDownCooldown:  scaleDownCooldown,
				ScaleUpThreshold:   cfg.MemHotplug.ScaleUpThreshold,
				ScaleDownThreshold: cfg.MemHotplug.ScaleDownThreshold,
				OOMSafetyMarginMB:  cfg.MemHotplug.OOMSafetyMarginMB,
				IncrementSize:      cfg.MemHotplug.IncrementSizeMB * 1024 * 1024, // Convert MB to bytes
				ScaleUpStability:   cfg.MemHotplug.ScaleUpStability,
				ScaleDownStability: cfg.MemHotplug.ScaleDownStability,
				EnableScaleDown:    cfg.MemHotplug.EnableScaleDown,
			}
		}
	}

	if memConfig.EnableScaleDown {
		log.G(ctx).Warn("memory-hotplug: scale-down enabled (EXPERIMENTAL)")
	}

	controller := memhotplug.NewController(
		containerID,
		qmpClient,
		func(ctx context.Context) (int64, error) {
			return callbacks.GetMemoryStats(ctx, containerID)
		},
		callbacks.OfflineMemory,
		callbacks.OnlineMemory,
		resourceCfg.MemorySize,
		resourceCfg.MemoryHotplugSize,
		memConfig,
	)

	// Start monitoring loop with detached context (not request context)
	// The controller needs to run for the lifetime of the container, not just the CreateTask RPC.
	// Cleanup happens via controller.Stop() in shutdown/delete, not via context cancellation.
	// Note: controller.Start() is non-blocking and launches its own goroutine internally.
	// The controller manages its own lifecycle and will log any errors in its monitor loop.
	controller.Start(context.WithoutCancel(ctx))

	log.G(ctx).WithFields(log.Fields{
		"container_id":   containerID,
		"boot_memory_mb": resourceCfg.MemorySize / (1024 * 1024),
		"max_memory_mb":  resourceCfg.MemoryHotplugSize / (1024 * 1024),
	}).Info("memory-hotplug: controller started (monitoring in background)")

	return controller
}

// CreateVMClientCallbacks creates hotplug callbacks from a TTRPC dial function.
// This is a helper to create the callbacks needed by the hotplug controllers.
func CreateVMClientCallbacks(dialClient func(context.Context) (*ttrpc.Client, error)) HotplugCallbacks {
	return HotplugCallbacks{
		GetCPUStats: func(ctx context.Context, containerID string) (uint64, uint64, error) {
			return getCPUStats(ctx, dialClient, containerID)
		},
		OfflineCPU: func(ctx context.Context, cpuID int) error {
			return offlineCPU(ctx, dialClient, cpuID)
		},
		OnlineCPU: func(ctx context.Context, cpuID int) error {
			return onlineCPU(ctx, dialClient, cpuID)
		},
		GetMemoryStats: func(ctx context.Context, containerID string) (int64, error) {
			return getMemoryStats(ctx, dialClient, containerID)
		},
		OfflineMemory: func(ctx context.Context, memoryID int) error {
			return offlineMemory(ctx, dialClient, memoryID)
		},
		OnlineMemory: func(ctx context.Context, memoryID int) error {
			return onlineMemory(ctx, dialClient, memoryID)
		},
	}
}
