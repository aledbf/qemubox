// Package cpuhotplug provides CPU hotplug control for QEMU VMs.
package cpuhotplug

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/containerd/log"

	"github.com/spin-stack/spinbox/internal/host/vm"
)

// Controller manages dynamic vCPU allocation for a VM based on CPU usage
type Controller struct {
	containerID   string
	cpuHotplugger vm.CPUHotplugger
	stats         StatsProvider
	offlineCPU    CPUOffliner
	onlineCPU     CPUOnliner

	// Resource limits
	bootCPUs int // Minimum vCPUs (never go below this)
	maxCPUs  int // Maximum vCPUs (ceiling)

	// Current state
	currentCPUs int // Current online vCPUs

	// Configuration
	config Config

	// Hysteresis tracking
	lastScaleUp          time.Time
	lastScaleDown        time.Time
	consecutiveHighUsage int // Track sustained high usage
	consecutiveLowUsage  int // Track sustained low usage

	// CPU usage sampling
	lastSampleTime    time.Time
	lastUsageUsec     uint64
	lastThrottledUsec uint64

	// State management
	mu        sync.Mutex
	stopCh    chan struct{}
	stoppedCh chan struct{}
}

// StatsProvider returns cgroup CPU usage and throttling in microseconds.
type StatsProvider func(ctx context.Context) (usageUsec, throttledUsec uint64, err error)

// CPUOffliner offlines a CPU in the guest before unplug.
type CPUOffliner func(ctx context.Context, cpuID int) error

// CPUOnliner onlines a CPU in the guest after hotplug.
type CPUOnliner func(ctx context.Context, cpuID int) error

// CPUHotplugController defines the interface for CPU hotplug management.
type CPUHotplugController interface {
	Start(ctx context.Context)
	Stop()
}

// Config holds configuration for the CPU hotplug controller
type Config struct {
	// Monitoring interval
	MonitorInterval time.Duration

	// Cooldown periods
	ScaleUpCooldown   time.Duration
	ScaleDownCooldown time.Duration

	// Thresholds (0-100 percentage)
	ScaleUpThreshold     float64
	ScaleDownThreshold   float64 // Target utilization after removing one vCPU
	ScaleUpThrottleLimit float64

	// Stability requirements (number of consecutive readings)
	ScaleUpStability   int // Need N consecutive high readings before scaling up
	ScaleDownStability int // Need N consecutive low readings before scaling down

	// Enable/disable features
	EnableScaleDown bool // Allow removing CPUs (may fail on some kernels)
}

const (
	// defaultMonitorInterval is how often to check CPU usage.
	// 5 seconds provides responsive scaling without excessive polling.
	defaultMonitorInterval = 5 * time.Second

	// defaultScaleUpCooldown prevents thrashing by rate-limiting scale-up operations.
	// 10 seconds allows new CPU to be utilized before considering another scale-up.
	defaultScaleUpCooldown = 10 * time.Second

	// defaultScaleDownCooldown is more conservative to avoid removing CPUs prematurely.
	// 30 seconds ensures sustained low usage before removing resources.
	defaultScaleDownCooldown = 30 * time.Second

	// defaultScaleUpThreshold is the CPU usage percentage that triggers adding a vCPU.
	// 80% provides headroom before hitting 100% utilization.
	defaultScaleUpThreshold = 80.0

	// defaultScaleDownThreshold is the projected CPU usage after removing one vCPU.
	// 50% ensures removed vCPU was genuinely idle (not causing load redistribution issues).
	defaultScaleDownThreshold = 50.0

	// defaultThrottleLimit prevents scaling when already at CPU quota.
	// If >5% of CPU time is throttled, adding vCPUs won't help (quota-limited, not CPU-limited).
	defaultThrottleLimit = 5.0

	// defaultScaleUpStability requires N consecutive high readings before scaling up.
	// 2 readings = 10 seconds total (2 * 5s interval), filtering brief spikes.
	defaultScaleUpStability = 2

	// defaultScaleDownStability requires more sustained low usage before removing CPUs.
	// 6 readings = 30 seconds total (6 * 5s interval), avoiding premature scale-down.
	defaultScaleDownStability = 6
)

// DefaultConfig returns sensible defaults for CPU hotplug
func DefaultConfig() Config {
	return Config{
		MonitorInterval:      defaultMonitorInterval,
		ScaleUpCooldown:      defaultScaleUpCooldown,
		ScaleDownCooldown:    defaultScaleDownCooldown,
		ScaleUpThreshold:     defaultScaleUpThreshold,
		ScaleDownThreshold:   defaultScaleDownThreshold,
		ScaleUpThrottleLimit: defaultThrottleLimit,
		ScaleUpStability:     defaultScaleUpStability,
		ScaleDownStability:   defaultScaleDownStability,
		EnableScaleDown:      true, // Enabled by default (some kernels may not support CPU unplug)
	}
}

// noopCPUController is a no-op implementation of CPUHotplugController.
// Used when CPU hotplug is not needed (maxCPUs <= bootCPUs).
type noopCPUController struct{}

func (n *noopCPUController) Start(ctx context.Context) {}
func (n *noopCPUController) Stop()                     {}

// NewController creates a new CPU hotplug controller.
// Returns a no-op controller if hotplug is not needed (maxCPUs <= bootCPUs).
func NewController(containerID string, cpuHotplugger vm.CPUHotplugger, stats StatsProvider, offliner CPUOffliner, onliner CPUOnliner, bootCPUs, maxCPUs int, config Config) CPUHotplugController {
	// Return no-op controller if hotplug is not needed
	if maxCPUs <= bootCPUs {
		return &noopCPUController{}
	}

	return &Controller{
		containerID:   containerID,
		cpuHotplugger: cpuHotplugger,
		stats:         stats,
		offlineCPU:    offliner,
		onlineCPU:     onliner,
		bootCPUs:      bootCPUs,
		maxCPUs:       maxCPUs,
		currentCPUs:   bootCPUs, // Start with boot CPUs
		config:        config,
	}
}

// Start begins the monitoring loop
func (c *Controller) Start(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopCh != nil {
		return // Already started
	}
	c.stopCh = make(chan struct{})
	c.stoppedCh = make(chan struct{})

	log.G(ctx).WithFields(log.Fields{
		"container_id":       c.containerID,
		"boot_cpus":          c.bootCPUs,
		"max_cpus":           c.maxCPUs,
		"monitor_interval":   c.config.MonitorInterval,
		"scale_up_threshold": c.config.ScaleUpThreshold,
		"scale_up_throttle":  c.config.ScaleUpThrottleLimit,
	}).Info("cpu-hotplug: controller started")

	go c.monitorLoop(ctx)
}

// Stop gracefully stops the controller
func (c *Controller) Stop() {
	c.mu.Lock()
	if c.stopCh == nil {
		c.mu.Unlock()
		return // Not started
	}
	close(c.stopCh)
	c.mu.Unlock()

	// Wait for monitor loop to finish
	<-c.stoppedCh
}

// monitorLoop runs the periodic CPU usage check
func (c *Controller) monitorLoop(ctx context.Context) {
	defer close(c.stoppedCh)

	ticker := time.NewTicker(c.config.MonitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			log.G(ctx).WithField("container_id", c.containerID).Info("cpu-hotplug: controller stopped")
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.checkAndAdjust(ctx); err != nil {
				log.G(ctx).WithError(err).WithField("container_id", c.containerID).
					Warn("cpu-hotplug: failed to check/adjust vCPUs")
			}
		}
	}
}

// checkAndAdjust queries CPU count and decides if adjustment is needed
func (c *Controller) checkAndAdjust(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Query current vCPU count from QEMU
	cpus, err := c.cpuHotplugger.QueryCPUs(ctx)
	if err != nil {
		return fmt.Errorf("query vCPUs: %w", err)
	}

	actualCPUs := len(cpus)
	if actualCPUs != c.currentCPUs {
		log.G(ctx).WithFields(log.Fields{
			"container_id": c.containerID,
			"expected":     c.currentCPUs,
			"actual":       actualCPUs,
		}).Info("cpu-hotplug: CPU count mismatch, updating")
		c.currentCPUs = actualCPUs
	}

	// Calculate target vCPU count based on actual CPU usage from cgroup stats
	targetCPUs := c.calculateTargetCPUs(ctx)

	// Check if we should adjust
	if targetCPUs > c.currentCPUs {
		// Scale up
		if !c.canScaleUp() {
			log.G(ctx).WithFields(log.Fields{
				"container_id": c.containerID,
				"cooldown":     time.Since(c.lastScaleUp),
				"required":     c.config.ScaleUpCooldown,
			}).Debug("cpu-hotplug: scale-up cooldown active")
			return nil
		}

		c.consecutiveHighUsage++
		c.consecutiveLowUsage = 0

		if c.consecutiveHighUsage < c.config.ScaleUpStability {
			log.G(ctx).WithFields(log.Fields{
				"container_id": c.containerID,
				"consecutive":  c.consecutiveHighUsage,
				"required":     c.config.ScaleUpStability,
			}).Debug("cpu-hotplug: waiting for stable high usage")
			return nil
		}

		return c.scaleUp(ctx, targetCPUs)
	} else if targetCPUs < c.currentCPUs && c.config.EnableScaleDown {
		// Scale down
		if !c.canScaleDown() {
			log.G(ctx).WithFields(log.Fields{
				"container_id": c.containerID,
				"cooldown":     time.Since(c.lastScaleDown),
				"required":     c.config.ScaleDownCooldown,
			}).Debug("cpu-hotplug: scale-down cooldown active")
			return nil
		}

		c.consecutiveLowUsage++
		c.consecutiveHighUsage = 0

		if c.consecutiveLowUsage < c.config.ScaleDownStability {
			log.G(ctx).WithFields(log.Fields{
				"container_id": c.containerID,
				"consecutive":  c.consecutiveLowUsage,
				"required":     c.config.ScaleDownStability,
			}).Debug("cpu-hotplug: waiting for stable low usage")
			return nil
		}

		if err := c.scaleDown(ctx, targetCPUs); err != nil {
			return fmt.Errorf("failed to scale down CPUs: %w", err)
		}
		return nil
	}

	// No change needed, reset counters
	c.consecutiveHighUsage = 0
	c.consecutiveLowUsage = 0

	return nil
}

// calculateTargetCPUs determines ideal vCPU count
// Strategy: Scale towards maxCPUs gradually to enable workload
// Future enhancement: Read actual CPU usage from cgroup stats via TTRPC
func (c *Controller) calculateTargetCPUs(ctx context.Context) int {
	usagePct, throttledPct, ok, err := c.sampleCPU(ctx)
	if err != nil {
		log.G(ctx).WithError(err).WithField("container_id", c.containerID).
			Warn("cpu-hotplug: failed to sample CPU usage")
		return c.currentCPUs
	}
	if !ok {
		log.G(ctx).WithField("container_id", c.containerID).
			Debug("cpu-hotplug: skipping scale decision (insufficient samples)")
		return c.currentCPUs
	}

	log.G(ctx).WithFields(log.Fields{
		"container_id":  c.containerID,
		"usage_pct":     fmt.Sprintf("%.2f", usagePct),
		"throttled_pct": fmt.Sprintf("%.2f", throttledPct),
	}).Debug("cpu-hotplug: CPU usage sample")

	if throttledPct >= c.config.ScaleUpThrottleLimit {
		log.G(ctx).WithFields(log.Fields{
			"container_id":  c.containerID,
			"throttled_pct": fmt.Sprintf("%.2f", throttledPct),
			"limit_pct":     c.config.ScaleUpThrottleLimit,
		}).Debug("cpu-hotplug: throttled at CPU limit; not scaling")
		return c.currentCPUs
	}

	if usagePct >= c.config.ScaleUpThreshold && c.currentCPUs < c.maxCPUs {
		return c.currentCPUs + 1
	}

	if c.config.EnableScaleDown && c.currentCPUs > c.bootCPUs {
		// Only scale down if the projected utilization after removal stays under the target.
		if c.currentCPUs > 1 {
			projectedUsage := usagePct * float64(c.currentCPUs) / float64(c.currentCPUs-1)
			if projectedUsage <= c.config.ScaleDownThreshold {
				return c.currentCPUs - 1
			}
		}
	}

	return c.currentCPUs
}

func (c *Controller) sampleCPU(ctx context.Context) (float64, float64, bool, error) {
	if c.stats == nil {
		return 0, 0, false, nil
	}

	usageUsec, throttledUsec, err := c.stats(ctx)
	if err != nil {
		return 0, 0, false, err
	}

	now := time.Now()
	if c.lastSampleTime.IsZero() {
		c.lastSampleTime = now
		c.lastUsageUsec = usageUsec
		c.lastThrottledUsec = throttledUsec
		return 0, 0, false, nil
	}

	elapsed := now.Sub(c.lastSampleTime)
	if elapsed <= 0 {
		log.G(ctx).WithFields(log.Fields{
			"container_id": c.containerID,
			"elapsed":      elapsed,
		}).Warn("cpu-hotplug: time went backward, resetting CPU stats baseline")
		c.lastSampleTime = now
		c.lastUsageUsec = usageUsec
		c.lastThrottledUsec = throttledUsec
		return 0, 0, false, nil
	}

	if usageUsec < c.lastUsageUsec || throttledUsec < c.lastThrottledUsec {
		log.G(ctx).WithFields(log.Fields{
			"container_id":        c.containerID,
			"usage_usec":          usageUsec,
			"last_usage_usec":     c.lastUsageUsec,
			"throttled_usec":      throttledUsec,
			"last_throttled_usec": c.lastThrottledUsec,
		}).Warn("cpu-hotplug: CPU usage counters decreased (possible counter overflow or stats bug), resetting baseline")
		c.lastSampleTime = now
		c.lastUsageUsec = usageUsec
		c.lastThrottledUsec = throttledUsec
		return 0, 0, false, nil
	}

	deltaUsage := usageUsec - c.lastUsageUsec
	deltaThrottled := throttledUsec - c.lastThrottledUsec

	c.lastSampleTime = now
	c.lastUsageUsec = usageUsec
	c.lastThrottledUsec = throttledUsec

	elapsedUsec := float64(elapsed.Microseconds())
	if elapsedUsec <= 0 || c.currentCPUs <= 0 {
		return 0, 0, false, nil
	}

	usagePct := (float64(deltaUsage) / (elapsedUsec * float64(c.currentCPUs))) * 100.0
	if usagePct < 0 {
		usagePct = 0
	}

	throttledPct := (float64(deltaThrottled) / elapsedUsec) * 100.0
	if throttledPct < 0 {
		throttledPct = 0
	}

	return usagePct, throttledPct, true, nil
}

// canScaleUp checks if scale-up cooldown has elapsed
func (c *Controller) canScaleUp() bool {
	if c.lastScaleUp.IsZero() {
		return true
	}
	return time.Since(c.lastScaleUp) >= c.config.ScaleUpCooldown
}

// canScaleDown checks if scale-down cooldown has elapsed
func (c *Controller) canScaleDown() bool {
	if c.lastScaleDown.IsZero() {
		return true
	}
	return time.Since(c.lastScaleDown) >= c.config.ScaleDownCooldown
}

// scaleUp adds vCPUs to reach target
func (c *Controller) scaleUp(ctx context.Context, targetCPUs int) error {
	log.G(ctx).WithFields(log.Fields{
		"container_id": c.containerID,
		"current":      c.currentCPUs,
		"target":       targetCPUs,
	}).Info("cpu-hotplug: scaling up vCPUs")

	// Query current CPUs to find which IDs are already in use
	cpus, err := c.cpuHotplugger.QueryCPUs(ctx)
	if err != nil {
		return fmt.Errorf("query CPUs for scale-up: %w", err)
	}

	// Build set of existing CPU IDs
	existingCPUs := make(map[int]bool, len(cpus))
	for _, cpu := range cpus {
		existingCPUs[cpu.CPUIndex] = true
	}

	// Add CPUs one at a time to target
	cpusToAdd := targetCPUs - c.currentCPUs
	added := 0
	for nextID := 0; added < cpusToAdd && nextID < c.maxCPUs; nextID++ {
		// Skip if this CPU ID already exists
		if existingCPUs[nextID] {
			continue
		}

		if err := c.cpuHotplugger.HotplugCPU(ctx, nextID); err != nil {
			log.G(ctx).WithError(err).WithFields(log.Fields{
				"container_id": c.containerID,
				"cpu_id":       nextID,
			}).Error("cpu-hotplug: failed to add vCPU")
			return fmt.Errorf("hotplug CPU %d: %w", nextID, err)
		}

		log.G(ctx).WithFields(log.Fields{
			"container_id": c.containerID,
			"cpu_id":       nextID,
		}).Info("cpu-hotplug: added vCPU")

		// Online the CPU in the guest (if onliner callback is provided)
		if c.onlineCPU != nil {
			if err := c.onlineCPU(ctx, nextID); err != nil {
				log.G(ctx).WithError(err).WithFields(log.Fields{
					"container_id": c.containerID,
					"cpu_id":       nextID,
				}).Warn("cpu-hotplug: failed to online vCPU in guest (may auto-online via uevents)")
				// Don't fail the operation - CPU may auto-online via kernel uevents
			} else {
				log.G(ctx).WithFields(log.Fields{
					"container_id": c.containerID,
					"cpu_id":       nextID,
				}).Debug("cpu-hotplug: vCPU onlined in guest")
			}
		}

		existingCPUs[nextID] = true
		added++
	}

	c.currentCPUs += added
	c.lastScaleUp = time.Now()
	c.consecutiveHighUsage = 0

	return nil
}

// scaleDown removes vCPUs to reach target
func (c *Controller) scaleDown(ctx context.Context, targetCPUs int) error {
	log.G(ctx).WithFields(log.Fields{
		"container_id": c.containerID,
		"current":      c.currentCPUs,
		"target":       targetCPUs,
	}).Info("cpu-hotplug: scaling down vCPUs")

	// Query current CPUs to get actual CPU IDs
	cpus, err := c.cpuHotplugger.QueryCPUs(ctx)
	if err != nil {
		return fmt.Errorf("query CPUs for scale-down: %w", err)
	}

	// Collect CPU IDs (excluding CPU 0 which cannot be unplugged)
	var removableCPUs []int
	for _, cpu := range cpus {
		if cpu.CPUIndex > 0 { // Never remove CPU0 (boot processor)
			removableCPUs = append(removableCPUs, cpu.CPUIndex)
		}
	}

	// Sort in descending order to remove highest IDs first (LIFO)
	for i, j := 0, len(removableCPUs)-1; i < j; i, j = i+1, j-1 {
		removableCPUs[i], removableCPUs[j] = removableCPUs[j], removableCPUs[i]
	}

	// Remove CPUs until we reach target (or run out of removable CPUs)
	cpusToRemove := c.currentCPUs - targetCPUs
	removed := 0
	for _, cpuID := range removableCPUs {
		if removed >= cpusToRemove {
			break
		}

		if c.offlineCPU != nil {
			if err := c.offlineCPU(ctx, cpuID); err != nil {
				log.G(ctx).WithError(err).WithFields(log.Fields{
					"container_id": c.containerID,
					"cpu_id":       cpuID,
				}).Warn("cpu-hotplug: failed to offline vCPU in guest (best-effort, continuing)")
				// CPU hot-unplug is best-effort - don't fail the operation
				// Guest may keep the CPU in use, but we proceed with unplug
			}
		}

		if err := c.cpuHotplugger.UnplugCPU(ctx, cpuID); err != nil {
			log.G(ctx).WithError(err).WithFields(log.Fields{
				"container_id": c.containerID,
				"cpu_id":       cpuID,
			}).Warn("cpu-hotplug: failed to remove vCPU (may not be supported by guest kernel, best-effort)")
			// CPU hot-unplug is best-effort - don't fail the operation
			// Continue removing other CPUs even if this one fails
			continue
		}

		log.G(ctx).WithFields(log.Fields{
			"container_id": c.containerID,
			"cpu_id":       cpuID,
		}).Info("cpu-hotplug: removed vCPU")
		removed++
	}

	c.currentCPUs -= removed
	c.lastScaleDown = time.Now()
	c.consecutiveLowUsage = 0
	return nil
}
