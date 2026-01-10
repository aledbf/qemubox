// Package memhotplug provides memory hotplug control for QEMU VMs.
package memhotplug

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/containerd/log"

	"github.com/spin-stack/spinbox/internal/host/vm/qemu"
)

const (
	// defaultMemorySlots is the default number of memory hotplug slots.
	// This should match the VMResourceConfig.MemorySlots value used when starting QEMU.
	defaultMemorySlots = 8
)

// qmpMemoryClient defines the interface for QMP memory operations.
// This interface exists to enable testing with mocks.
type qmpMemoryClient interface {
	HotplugMemory(ctx context.Context, slotID int, sizeBytes int64) error
	UnplugMemory(ctx context.Context, slotID int) error
	QueryMemorySizeSummary(ctx context.Context) (*qemu.MemorySizeSummary, error)
}

// Controller manages dynamic memory allocation for a VM based on memory usage
type Controller struct {
	containerID   string
	qmpClient     qmpMemoryClient
	stats         StatsProvider
	offlineMemory MemoryOffliner
	onlineMemory  MemoryOnliner

	// Resource limits
	bootMemory int64 // Minimum memory (never go below this)
	maxMemory  int64 // Maximum memory (ceiling)

	// Current state
	currentMemory int64        // Current online memory in bytes
	usedSlots     map[int]bool // Track which memory slots are used

	// Configuration
	config Config

	// Hysteresis tracking
	lastScaleUp          time.Time
	lastScaleDown        time.Time
	consecutiveHighUsage int // Track sustained high usage
	consecutiveLowUsage  int // Track sustained low usage

	// Memory usage sampling
	lastSampleTime  time.Time
	lastMemoryUsage int64

	// State management
	mu        sync.Mutex
	started   bool // Track if Start() has been called
	stopCh    chan struct{}
	stoppedCh chan struct{}
}

// StatsProvider returns cgroup memory usage in bytes
type StatsProvider func(ctx context.Context) (usageBytes int64, err error)

// MemoryOffliner offlines a memory block in the guest before unplug
type MemoryOffliner func(ctx context.Context, memoryID int) error

// MemoryOnliner onlines a memory block in the guest after hotplug
type MemoryOnliner func(ctx context.Context, memoryID int) error

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

	// MaxSlots is the number of memory hotplug slots available.
	// This must match the QEMU configuration (VMResourceConfig.MemorySlots).
	// If zero, defaults to 8.
	MaxSlots int
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
		IncrementSize:      128 * 1024 * 1024, // 128MB (DIMM slot size)
		ScaleUpStability:   3,                 // Need 3 consecutive high readings (30s)
		ScaleDownStability: 6,                 // Need 6 consecutive low readings (60s)
		EnableScaleDown:    false,             // Disabled by default (memory unplug is risky)
		MaxSlots:           defaultMemorySlots,
	}
}

// noopMemoryController is a no-op implementation of MemoryHotplugController.
// Used when memory hotplug is not needed (maxMemory <= bootMemory).
type noopMemoryController struct{}

func (n *noopMemoryController) Start(ctx context.Context) {}
func (n *noopMemoryController) Stop()                     {}

// NewController creates a new memory hotplug controller.
// Returns a no-op controller if hotplug is not needed (maxMemory <= bootMemory).
// In production, qmpClient should be a *qemu.QMPClient.
func NewController(
	containerID string,
	qmpClient qmpMemoryClient,
	stats StatsProvider,
	offliner MemoryOffliner,
	onliner MemoryOnliner,
	bootMemory, maxMemory int64,
	config Config,
) MemoryHotplugController {
	// Return no-op controller if hotplug is not needed
	if maxMemory <= bootMemory {
		return &noopMemoryController{}
	}

	// Apply default for MaxSlots if not set
	if config.MaxSlots < 1 {
		config.MaxSlots = defaultMemorySlots
	}

	// Create channels only when controller will actually run
	return &Controller{
		containerID:   containerID,
		qmpClient:     qmpClient,
		stats:         stats,
		offlineMemory: offliner,
		onlineMemory:  onliner,
		bootMemory:    bootMemory,
		maxMemory:     maxMemory,
		currentMemory: bootMemory,
		usedSlots:     make(map[int]bool),
		config:        config,
		stopCh:        make(chan struct{}),
		stoppedCh:     make(chan struct{}),
	}
}

// Start begins monitoring memory usage and managing hotplug
func (c *Controller) Start(ctx context.Context) {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return // Already started
	}
	c.started = true
	c.mu.Unlock()

	go func() {
		defer close(c.stoppedCh)

		log.G(ctx).WithFields(log.Fields{
			"container_id":       c.containerID,
			"boot_memory_mb":     c.bootMemory / (1024 * 1024),
			"max_memory_mb":      c.maxMemory / (1024 * 1024),
			"monitor_interval":   c.config.MonitorInterval,
			"scale_up_threshold": c.config.ScaleUpThreshold,
			"scale_down_enabled": c.config.EnableScaleDown,
		}).Info("memory-hotplug: controller started")

		c.monitorLoop(ctx)

		log.G(ctx).WithField("container_id", c.containerID).
			Info("memory-hotplug: controller stopped")
	}()
}

// Stop terminates the monitoring loop
func (c *Controller) Stop() {
	select {
	case <-c.stopCh:
		// Already stopped
		return
	default:
		close(c.stopCh)
	}

	// Wait for monitoring loop to finish
	<-c.stoppedCh
}

// monitorLoop periodically checks memory usage and adjusts allocation
func (c *Controller) monitorLoop(ctx context.Context) {
	ticker := time.NewTicker(c.config.MonitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.checkAndAdjust(ctx); err != nil {
				log.G(ctx).WithError(err).WithField("container_id", c.containerID).
					Warn("memory-hotplug: failed to check and adjust memory")
			}
		}
	}
}

// checkAndAdjust checks current memory usage and adjusts if needed
func (c *Controller) checkAndAdjust(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Query current memory state from QEMU
	summary, err := c.qmpClient.QueryMemorySizeSummary(ctx)
	if err != nil {
		return fmt.Errorf("failed to query memory summary: %w", err)
	}

	totalMemory := summary.BaseMemory + summary.PluggedMemory
	c.currentMemory = totalMemory

	// Calculate target memory
	targetMemory, err := c.calculateTargetMemory(ctx)
	if err != nil {
		return fmt.Errorf("failed to calculate target memory: %w", err)
	}

	if targetMemory == c.currentMemory {
		return nil
	}

	// Scale up
	if targetMemory > c.currentMemory {
		return c.scaleUp(ctx, targetMemory)
	}

	// Scale down
	if targetMemory < c.currentMemory && c.config.EnableScaleDown {
		return c.scaleDown(ctx, targetMemory)
	}

	return nil
}

// calculateTargetMemory determines the target memory size based on usage
func (c *Controller) calculateTargetMemory(ctx context.Context) (int64, error) {
	usagePct, ok, err := c.sampleMemory(ctx)
	if err != nil {
		return c.currentMemory, err
	}
	if !ok {
		// Not enough data yet
		return c.currentMemory, nil
	}

	// Calculate free memory
	usedMemory := int64(float64(c.currentMemory) * usagePct / 100.0)
	freeMemory := c.currentMemory - usedMemory
	safetyMargin := c.config.OOMSafetyMarginMB * 1024 * 1024

	switch {
	// Scale up: usage > threshold AND free memory < safety margin
	case usagePct >= c.config.ScaleUpThreshold && freeMemory < safetyMargin:
		// Check cooldown
		if time.Since(c.lastScaleUp) < c.config.ScaleUpCooldown {
			return c.currentMemory, nil
		}

		c.consecutiveHighUsage++
		c.consecutiveLowUsage = 0

		if c.consecutiveHighUsage >= c.config.ScaleUpStability {
			newMemory := c.currentMemory + c.config.IncrementSize
			if newMemory > c.maxMemory {
				newMemory = c.maxMemory
			}
			if newMemory > c.currentMemory {
				return newMemory, nil
			}
		}
	case usagePct < c.config.ScaleDownThreshold:
		// Scale down: usage < threshold
		// Check cooldown
		if time.Since(c.lastScaleDown) < c.config.ScaleDownCooldown {
			return c.currentMemory, nil
		}

		c.consecutiveLowUsage++
		c.consecutiveHighUsage = 0

		if c.consecutiveLowUsage >= c.config.ScaleDownStability {
			newMemory := c.currentMemory - c.config.IncrementSize
			if newMemory < c.bootMemory {
				newMemory = c.bootMemory
			}

			// Ensure we still have safety margin after removal
			projectedUsed := usedMemory
			projectedFree := newMemory - projectedUsed
			if projectedFree >= safetyMargin && newMemory < c.currentMemory {
				return newMemory, nil
			}
		}
	default:
		// Reset counters
		c.consecutiveHighUsage = 0
		c.consecutiveLowUsage = 0
	}

	return c.currentMemory, nil
}

// sampleMemory samples current memory usage and returns percentage
// Returns (usagePercentage, dataValid, error)
func (c *Controller) sampleMemory(ctx context.Context) (float64, bool, error) {
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

// scaleUp adds memory to the VM
func (c *Controller) scaleUp(ctx context.Context, targetMemory int64) error {
	amountToAdd := targetMemory - c.currentMemory
	if amountToAdd <= 0 {
		return nil
	}

	// Find available slot
	slotID := c.findFreeSlot()
	if slotID < 0 {
		log.G(ctx).WithField("container_id", c.containerID).
			Warn("memory-hotplug: no free memory slots available")
		return fmt.Errorf("no free memory slots available")
	}

	usagePct := float64(c.lastMemoryUsage) / float64(c.currentMemory) * 100.0
	freeMemory := c.currentMemory - c.lastMemoryUsage

	log.G(ctx).WithFields(log.Fields{
		"container_id":      c.containerID,
		"current_memory_mb": c.currentMemory / (1024 * 1024),
		"target_memory_mb":  targetMemory / (1024 * 1024),
		"add_mb":            amountToAdd / (1024 * 1024),
		"slot_id":           slotID,
		"usage_pct":         fmt.Sprintf("%.2f", usagePct),
		"free_mb":           freeMemory / (1024 * 1024),
	}).Info("memory-hotplug: scaling up memory")

	// Hotplug memory via QMP
	if err := c.qmpClient.HotplugMemory(ctx, slotID, amountToAdd); err != nil {
		return fmt.Errorf("failed to hotplug memory: %w", err)
	}

	// Mark slot as used
	c.usedSlots[slotID] = true

	// Online memory in guest - required for memory to be usable
	if err := c.onlineMemory(ctx, slotID); err != nil {
		log.G(ctx).WithError(err).WithField("slot_id", slotID).
			Error("memory-hotplug: failed to online memory in guest")
		// Memory was allocated via QMP but is not usable by guest
		// Try to unplug it to avoid wasting resources
		if unplugErr := c.qmpClient.UnplugMemory(ctx, slotID); unplugErr != nil {
			log.G(ctx).WithError(unplugErr).WithField("slot_id", slotID).
				Warn("memory-hotplug: failed to unplug unusable memory")
		}
		delete(c.usedSlots, slotID)
		return fmt.Errorf("memory allocated but failed to online in guest: %w", err)
	}

	c.lastScaleUp = time.Now()
	c.consecutiveHighUsage = 0
	c.currentMemory = targetMemory

	return nil
}

// scaleDown removes memory from the VM
func (c *Controller) scaleDown(ctx context.Context, targetMemory int64) error {
	amountToRemove := c.currentMemory - targetMemory
	if amountToRemove <= 0 {
		return nil
	}

	// Find used slot to remove (last added, LIFO)
	slotID := c.findUsedSlot()
	if slotID < 0 {
		return fmt.Errorf("no used memory slots to remove")
	}

	usagePct := float64(c.lastMemoryUsage) / float64(c.currentMemory) * 100.0
	projectedFree := targetMemory - c.lastMemoryUsage

	log.G(ctx).WithFields(log.Fields{
		"container_id":      c.containerID,
		"current_memory_mb": c.currentMemory / (1024 * 1024),
		"target_memory_mb":  targetMemory / (1024 * 1024),
		"remove_mb":         amountToRemove / (1024 * 1024),
		"slot_id":           slotID,
		"usage_pct":         fmt.Sprintf("%.2f", usagePct),
		"projected_free_mb": projectedFree / (1024 * 1024),
	}).Info("memory-hotplug: scaling down memory")

	// Offline memory in guest first
	if err := c.offlineMemory(ctx, slotID); err != nil {
		log.G(ctx).WithError(err).WithField("slot_id", slotID).
			Warn("memory-hotplug: failed to offline memory in guest")
		return fmt.Errorf("failed to offline memory: %w", err)
	}

	// Unplug memory via QMP
	if err := c.qmpClient.UnplugMemory(ctx, slotID); err != nil {
		// Try to bring memory back online if unplug failed
		if onlineErr := c.onlineMemory(ctx, slotID); onlineErr != nil {
			log.G(ctx).WithError(onlineErr).WithField("slot_id", slotID).
				Error("memory-hotplug: CRITICAL - failed to re-online memory after unplug failure, guest may have offline memory")
		}
		return fmt.Errorf("failed to unplug memory: %w", err)
	}

	// Mark slot as free
	delete(c.usedSlots, slotID)

	c.lastScaleDown = time.Now()
	c.consecutiveLowUsage = 0
	c.currentMemory = targetMemory

	return nil
}

// findFreeSlot finds the first available memory slot
func (c *Controller) findFreeSlot() int {
	for i := range c.config.MaxSlots {
		if !c.usedSlots[i] {
			return i
		}
	}
	return -1
}

// findUsedSlot finds a used memory slot (LIFO - last added first)
func (c *Controller) findUsedSlot() int {
	for i := c.config.MaxSlots - 1; i >= 0; i-- {
		if c.usedSlots[i] {
			return i
		}
	}
	return -1
}
