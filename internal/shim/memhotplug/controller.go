// Package memhotplug provides memory hotplug control for QEMU VMs.
package memhotplug

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/containerd/log"

	"github.com/spin-stack/spinbox/internal/host/vm/qemu"
	"github.com/spin-stack/spinbox/internal/shim/hotplug"
)

// fieldContainerID is the structured-logging field key for the container ID.
const fieldContainerID = "container_id"

// qmpMemoryClient defines the interface for QMP memory operations.
// This interface exists to enable testing with mocks.
//
// One call in each direction, because the machine grows through a virtio-mem
// device and a virtio-mem device is one number. What this replaced was pc-dimm
// devices in eight fixed slots: a backend object and a device per step, a table
// saying which slots were in use, LIFO ordering so unplug took the newest, an
// RPC into the guest to online what arrived and another to offline what was
// leaving, and a rollback for each of the four ways that could half-fail. The
// guest onlines by itself now (memhp_default_state=online), and shrinking is the
// same call with a smaller number.
type qmpMemoryClient interface {
	// SetPluggedMemory asks for a total amount above the boot size and returns
	// what the device reports afterwards. The answer is what is used: virtio-mem
	// negotiates with the guest and can only take back memory the guest has
	// released, so a request is not a promise and asking is not the same as
	// having.
	SetPluggedMemory(ctx context.Context, sizeBytes int64) (int64, error)
	QueryMemorySizeSummary(ctx context.Context) (*qemu.MemorySizeSummary, error)
}

// StatsProvider returns cgroup memory usage in bytes
type StatsProvider func(ctx context.Context) (usageBytes int64, err error)

// MemoryHotplugController defines the interface for memory hotplug management.
type MemoryHotplugController interface {
	Start(ctx context.Context)
	Stop()
}

// Config holds configuration for the memory hotplug controller
type Config struct {
	// Monitoring interval
	MonitorInterval time.Duration

	// Cooldown periods
	ScaleUpCooldown   time.Duration
	ScaleDownCooldown time.Duration

	// Thresholds (0-100 percentage of current memory)
	ScaleUpThreshold   float64 // Add memory when usage > this %
	ScaleDownThreshold float64 // Remove memory when usage < this %

	// OOM safety margin (keep this much free memory)
	OOMSafetyMarginMB int64

	// Memory increment size (128MB aligned)
	IncrementSize int64

	// Stability requirements (number of consecutive readings)
	ScaleUpStability   int
	ScaleDownStability int

	// Enable/disable features
	EnableScaleDown bool
}

// DefaultConfig returns sensible defaults for memory hotplug
func DefaultConfig() Config {
	return Config{
		MonitorInterval:    10 * time.Second,  // Slower than CPU (memory changes less frequently)
		ScaleUpCooldown:    30 * time.Second,  // Longer cooldown (memory ops are expensive)
		ScaleDownCooldown:  60 * time.Second,  // Very conservative scale-down
		ScaleUpThreshold:   85.0,              // Add memory at 85% usage
		ScaleDownThreshold: 60.0,              // Remove memory below 60% usage
		OOMSafetyMarginMB:  128,               // Always keep 128MB free
		IncrementSize:      128 * 1024 * 1024, // 128MB
		ScaleUpStability:   3,                 // Need 3 consecutive high readings (30s)
		ScaleDownStability: 6,                 // Need 6 consecutive low readings (60s)
		EnableScaleDown:    false,             // Disabled by default (memory unplug is risky)
	}
}

// noopMemoryController is a no-op implementation of MemoryHotplugController.
// Used when memory hotplug is not needed (maxMemory <= bootMemory).
type noopMemoryController struct{}

func (n *noopMemoryController) Start(ctx context.Context) {}
func (n *noopMemoryController) Stop()                     {}

// Controller manages dynamic memory allocation for a VM based on memory usage
type Controller struct {
	containerID string
	qmpClient   qmpMemoryClient
	stats       StatsProvider

	// Resource limits
	bootMemory int64 // Minimum memory (never go below this)
	maxMemory  int64 // Maximum memory (ceiling)

	// Current state (protected by mu)
	mu sync.Mutex
	// currentMemory is what the VM actually has, read back from the device after
	// every request and never assumed from what was asked for: virtio-mem plugs
	// memory as the guest accepts it, and unplugs only what the guest has
	// released, so the number that arrived is the only one worth keeping.
	currentMemory int64

	// Configuration
	config Config

	// Memory usage sampling
	lastSampleTime  time.Time
	lastMemoryUsage int64

	// Shared monitor handles lifecycle and stability tracking
	monitor *hotplug.Monitor
}

// NewController creates a new memory hotplug controller.
// Returns a no-op controller if hotplug is not needed (maxMemory <= bootMemory).
// In production, qmpClient should be a *qemu.QMPClient.
func NewController(
	containerID string,
	qmpClient qmpMemoryClient,
	stats StatsProvider,
	bootMemory, maxMemory int64,
	config Config,
) MemoryHotplugController {
	// Return no-op controller if hotplug is not needed
	if maxMemory <= bootMemory {
		return &noopMemoryController{}
	}

	c := &Controller{
		containerID:   containerID,
		qmpClient:     qmpClient,
		stats:         stats,
		bootMemory:    bootMemory,
		maxMemory:     maxMemory,
		currentMemory: bootMemory,
		config:        config,
	}

	// Create shared monitor with memory-specific scaler
	c.monitor = hotplug.NewMonitor(c, hotplug.MonitorConfig{
		MonitorInterval:    config.MonitorInterval,
		ScaleUpCooldown:    config.ScaleUpCooldown,
		ScaleDownCooldown:  config.ScaleDownCooldown,
		ScaleUpStability:   config.ScaleUpStability,
		ScaleDownStability: config.ScaleDownStability,
		EnableScaleDown:    config.EnableScaleDown,
	})

	return c
}

// Start begins monitoring memory usage and managing hotplug
func (c *Controller) Start(ctx context.Context) {
	log.G(ctx).WithFields(log.Fields{
		fieldContainerID:     c.containerID,
		"boot_memory_mb":     c.bootMemory / (1024 * 1024),
		"max_memory_mb":      c.maxMemory / (1024 * 1024),
		"scale_up_threshold": c.config.ScaleUpThreshold,
		"scale_down_enabled": c.config.EnableScaleDown,
	}).Info("memory-hotplug: controller starting")

	c.monitor.Start(ctx)
}

// Stop terminates the monitoring loop
func (c *Controller) Stop() {
	c.monitor.Stop()
}

// Name implements hotplug.ResourceScaler
func (c *Controller) Name() string {
	return "memory"
}

// ContainerID implements hotplug.ResourceScaler
func (c *Controller) ContainerID() string {
	return c.containerID
}

// EvaluateScaling implements hotplug.ResourceScaler
func (c *Controller) EvaluateScaling(ctx context.Context) (hotplug.ScaleDirection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Query current memory state from QEMU
	summary, err := c.qmpClient.QueryMemorySizeSummary(ctx)
	if err != nil {
		return hotplug.ScaleNone, fmt.Errorf("query memory summary: %w", err)
	}

	totalMemory := summary.BaseMemory + summary.PluggedMemory
	c.currentMemory = totalMemory

	// Sample memory usage
	usagePct, ok, err := c.sampleMemory(ctx)
	if err != nil {
		log.G(ctx).WithError(err).WithField(fieldContainerID, c.containerID).
			Warn("memory-hotplug: failed to sample memory usage")
		return hotplug.ScaleNone, nil
	}
	if !ok {
		return hotplug.ScaleNone, nil
	}

	// Calculate free memory
	usedMemory := int64(float64(c.currentMemory) * usagePct / 100.0)
	freeMemory := c.currentMemory - usedMemory
	safetyMargin := c.config.OOMSafetyMarginMB * 1024 * 1024

	log.G(ctx).WithFields(log.Fields{
		fieldContainerID:    c.containerID,
		"usage_pct":         fmt.Sprintf("%.2f", usagePct),
		"current_memory_mb": c.currentMemory / (1024 * 1024),
		"free_mb":           freeMemory / (1024 * 1024),
	}).Debug("memory-hotplug: memory usage sample")

	// Check scale up: usage > threshold AND free memory < safety margin
	if usagePct >= c.config.ScaleUpThreshold && freeMemory < safetyMargin && c.currentMemory < c.maxMemory {
		return hotplug.ScaleUp, nil
	}

	// Check scale down: usage < threshold
	if c.config.EnableScaleDown && c.currentMemory > c.bootMemory {
		if usagePct < c.config.ScaleDownThreshold {
			// Verify we'd still have safety margin after removal
			newMemory := c.currentMemory - c.config.IncrementSize
			if newMemory >= c.bootMemory {
				projectedFree := newMemory - usedMemory
				if projectedFree >= safetyMargin {
					return hotplug.ScaleDown, nil
				}
			}
		}
	}

	return hotplug.ScaleNone, nil
}

// ScaleUp implements hotplug.ResourceScaler
func (c *Controller) ScaleUp(ctx context.Context) error {
	return c.resize(ctx, c.clamp(c.currentMemory+c.config.IncrementSize), "up")
}

// ScaleDown implements hotplug.ResourceScaler
func (c *Controller) ScaleDown(ctx context.Context) error {
	return c.resize(ctx, c.clamp(c.currentMemory-c.config.IncrementSize), "down")
}

// clamp keeps a target inside the boot size and the ceiling. Both are hard: the
// boot size is the memory a template was frozen with, and the ceiling is what the
// virtio-mem device was created able to hand out.
func (c *Controller) clamp(target int64) int64 {
	if target > c.maxMemory {
		return c.maxMemory
	}
	if target < c.bootMemory {
		return c.bootMemory
	}
	return target
}

// resize asks the machine for a total size and records what arrived.
//
// Growing and shrinking are the same call, which is the point of virtio-mem, and
// it is why there is one function here where there were two: the device is on the
// command line from the start, its size is a property, and setting that property
// is the whole operation in both directions.
//
// What arrived is read back rather than assumed. A request is a negotiation: the
// guest accepts memory in blocks, and on the way down the device can only take
// back what the guest has released, so asking for less than the guest is using
// is not an error and simply does not happen. Recording the request instead
// would leave this believing in memory the VM does not have — and the next
// decision is made against that number.
func (c *Controller) resize(ctx context.Context, target int64, direction string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if target == c.currentMemory {
		return nil
	}

	usagePct := float64(c.lastMemoryUsage) / float64(c.currentMemory) * 100.0
	log.G(ctx).WithFields(log.Fields{
		fieldContainerID:    c.containerID,
		"current_memory_mb": c.currentMemory / (1024 * 1024),
		"target_memory_mb":  target / (1024 * 1024),
		"usage_pct":         fmt.Sprintf("%.2f", usagePct),
		"free_mb":           (c.currentMemory - c.lastMemoryUsage) / (1024 * 1024),
	}).Info("memory-hotplug: scaling memory " + direction)

	plugged, err := c.qmpClient.SetPluggedMemory(ctx, target-c.bootMemory)
	if err != nil {
		return fmt.Errorf("resizing memory to %d bytes: %w", target, err)
	}

	c.currentMemory = c.bootMemory + plugged
	if c.currentMemory != target {
		// Not a failure. The guest has not released what was asked for yet, or has
		// not taken all of what was offered; the next cycle asks again with the
		// same thresholds against the size that actually exists.
		log.G(ctx).WithFields(log.Fields{
			fieldContainerID:   c.containerID,
			"target_memory_mb": target / (1024 * 1024),
			"actual_memory_mb": c.currentMemory / (1024 * 1024),
		}).Debug("memory-hotplug: the guest has not settled on the requested size")
	}
	return nil
}

// sampleMemory samples current memory usage and returns percentage
// Returns (usagePercentage, dataValid, error)
func (c *Controller) sampleMemory(ctx context.Context) (float64, bool, error) {
	if c.stats == nil {
		return 0, false, nil
	}

	usageBytes, err := c.stats(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("failed to get memory stats: %w", err)
	}

	now := time.Now()
	if c.lastSampleTime.IsZero() {
		// First sample
		c.lastSampleTime = now
		c.lastMemoryUsage = usageBytes
		return 0, false, nil
	}

	// Calculate usage percentage
	usagePct := float64(usageBytes) / float64(c.currentMemory) * 100.0

	c.lastSampleTime = now
	c.lastMemoryUsage = usageBytes

	return usagePct, true, nil
}
