package cpuhotplug

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/spin-stack/spinbox/internal/host/vm"
)

// mockCPUHotplugger implements vm.CPUHotplugger for testing
type mockCPUHotplugger struct {
	cpus          []vm.CPUInfo
	hotplugErr    error
	unplugErr     error
	queryCalls    atomic.Int32
	hotplugCalls  atomic.Int32
	unplugCalls   atomic.Int32
	lastHotplugID atomic.Int32
	lastUnplugID  atomic.Int32
}

func (m *mockCPUHotplugger) QueryCPUs(_ context.Context) ([]vm.CPUInfo, error) {
	m.queryCalls.Add(1)
	return m.cpus, nil
}

func (m *mockCPUHotplugger) HotplugCPU(_ context.Context, cpuID int) error {
	m.hotplugCalls.Add(1)
	m.lastHotplugID.Store(int32(cpuID))
	if m.hotplugErr != nil {
		return m.hotplugErr
	}
	m.cpus = append(m.cpus, vm.CPUInfo{CPUIndex: cpuID})
	return nil
}

func (m *mockCPUHotplugger) UnplugCPU(_ context.Context, cpuID int) error {
	m.unplugCalls.Add(1)
	m.lastUnplugID.Store(int32(cpuID))
	if m.unplugErr != nil {
		return m.unplugErr
	}
	// Remove CPU from list
	var newCPUs []vm.CPUInfo
	for _, cpu := range m.cpus {
		if cpu.CPUIndex != cpuID {
			newCPUs = append(newCPUs, cpu)
		}
	}
	m.cpus = newCPUs
	return nil
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	assert.Equal(t, 5*time.Second, cfg.MonitorInterval)
	assert.Equal(t, 10*time.Second, cfg.ScaleUpCooldown)
	assert.Equal(t, 30*time.Second, cfg.ScaleDownCooldown)
	assert.InDelta(t, 80.0, cfg.ScaleUpThreshold, 0.001)
	assert.InDelta(t, 50.0, cfg.ScaleDownThreshold, 0.001)
	assert.InDelta(t, 5.0, cfg.ScaleUpThrottleLimit, 0.001)
	assert.Equal(t, 2, cfg.ScaleUpStability)
	assert.Equal(t, 6, cfg.ScaleDownStability)
	assert.True(t, cfg.EnableScaleDown)
}

func TestNewController(t *testing.T) {
	t.Run("returns noop controller when maxCPUs <= bootCPUs", func(t *testing.T) {
		mock := &mockCPUHotplugger{}
		cfg := DefaultConfig()

		ctrl := NewController("test", mock, nil, nil, nil, 2, 2, cfg)

		// Should be a noop controller
		_, isNoop := ctrl.(*noopCPUController)
		assert.True(t, isNoop)
	})

	t.Run("returns noop controller when maxCPUs < bootCPUs", func(t *testing.T) {
		mock := &mockCPUHotplugger{}
		cfg := DefaultConfig()

		ctrl := NewController("test", mock, nil, nil, nil, 4, 2, cfg)

		_, isNoop := ctrl.(*noopCPUController)
		assert.True(t, isNoop)
	})

	t.Run("returns real controller when maxCPUs > bootCPUs", func(t *testing.T) {
		mock := &mockCPUHotplugger{}
		cfg := DefaultConfig()

		ctrl := NewController("test", mock, nil, nil, nil, 1, 4, cfg)

		realCtrl, isReal := ctrl.(*Controller)
		require.True(t, isReal)
		assert.Equal(t, "test", realCtrl.containerID)
		assert.Equal(t, 1, realCtrl.bootCPUs)
		assert.Equal(t, 4, realCtrl.maxCPUs)
		assert.Equal(t, 1, realCtrl.currentCPUs)
	})
}

func TestNoopController(t *testing.T) {
	ctrl := &noopCPUController{}

	// Should not panic
	ctx := context.Background()
	ctrl.Start(ctx)
	ctrl.Stop()
}

func TestController_StartStop(t *testing.T) {
	mock := &mockCPUHotplugger{
		cpus: []vm.CPUInfo{{CPUIndex: 0}},
	}
	cfg := DefaultConfig()
	cfg.MonitorInterval = 10 * time.Millisecond

	ctrl := NewController("test", mock, nil, nil, nil, 1, 4, cfg).(*Controller)

	ctx := context.Background()

	// Start the controller
	ctrl.Start(ctx)

	// Wait for at least one monitor tick
	time.Sleep(50 * time.Millisecond)

	// Should have queried CPUs at least once
	assert.GreaterOrEqual(t, mock.queryCalls.Load(), int32(1))

	// Stop the controller
	ctrl.Stop()

	// Note: calling Stop() twice would panic (stopCh is closed twice)
	// The controller should be stopped only once
}

func TestController_StartIdempotent(t *testing.T) {
	mock := &mockCPUHotplugger{
		cpus: []vm.CPUInfo{{CPUIndex: 0}},
	}
	cfg := DefaultConfig()
	cfg.MonitorInterval = 100 * time.Millisecond

	ctrl := NewController("test", mock, nil, nil, nil, 1, 4, cfg).(*Controller)

	ctx := context.Background()

	// Start multiple times should be safe
	ctrl.Start(ctx)
	ctrl.Start(ctx) // Should be ignored
	ctrl.Start(ctx) // Should be ignored

	ctrl.Stop()
}

func TestController_CanScaleUp(t *testing.T) {
	mock := &mockCPUHotplugger{}
	cfg := DefaultConfig()
	cfg.ScaleUpCooldown = 100 * time.Millisecond

	ctrl := &Controller{
		cpuHotplugger: mock,
		config:        cfg,
	}

	// First check should allow scale up (no previous scale up)
	assert.True(t, ctrl.canScaleUp())

	// Set last scale up to now
	ctrl.lastScaleUp = time.Now()

	// Should not allow scale up during cooldown
	assert.False(t, ctrl.canScaleUp())

	// Wait for cooldown to expire
	time.Sleep(150 * time.Millisecond)

	// Should allow scale up again
	assert.True(t, ctrl.canScaleUp())
}

func TestController_CanScaleDown(t *testing.T) {
	mock := &mockCPUHotplugger{}
	cfg := DefaultConfig()
	cfg.ScaleDownCooldown = 100 * time.Millisecond

	ctrl := &Controller{
		cpuHotplugger: mock,
		config:        cfg,
	}

	// First check should allow scale down
	assert.True(t, ctrl.canScaleDown())

	// Set last scale down to now
	ctrl.lastScaleDown = time.Now()

	// Should not allow scale down during cooldown
	assert.False(t, ctrl.canScaleDown())

	// Wait for cooldown to expire
	time.Sleep(150 * time.Millisecond)

	// Should allow scale down again
	assert.True(t, ctrl.canScaleDown())
}

func TestController_ScaleUp(t *testing.T) {
	t.Run("adds vCPU successfully", func(t *testing.T) {
		mock := &mockCPUHotplugger{
			cpus: []vm.CPUInfo{{CPUIndex: 0}},
		}
		cfg := DefaultConfig()

		ctrl := &Controller{
			containerID:   "test",
			cpuHotplugger: mock,
			config:        cfg,
			currentCPUs:   1,
			maxCPUs:       4,
		}

		ctx := context.Background()
		err := ctrl.scaleUp(ctx, 2)

		require.NoError(t, err)
		assert.Equal(t, 2, ctrl.currentCPUs)
		assert.Equal(t, int32(1), mock.hotplugCalls.Load())
		assert.Equal(t, int32(1), mock.lastHotplugID.Load()) // Should add CPU 1 (0 already exists)
		assert.False(t, ctrl.lastScaleUp.IsZero())
	})

	t.Run("handles hotplug error", func(t *testing.T) {
		mock := &mockCPUHotplugger{
			cpus:       []vm.CPUInfo{{CPUIndex: 0}},
			hotplugErr: errors.New("hotplug failed"),
		}
		cfg := DefaultConfig()

		ctrl := &Controller{
			containerID:   "test",
			cpuHotplugger: mock,
			config:        cfg,
			currentCPUs:   1,
			maxCPUs:       4,
		}

		ctx := context.Background()
		err := ctrl.scaleUp(ctx, 2)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "hotplug CPU")
		assert.Equal(t, 1, ctrl.currentCPUs) // Should remain unchanged
	})

	t.Run("calls onliner callback", func(t *testing.T) {
		mock := &mockCPUHotplugger{
			cpus: []vm.CPUInfo{{CPUIndex: 0}},
		}
		cfg := DefaultConfig()

		onlineCalls := 0
		onliner := func(ctx context.Context, cpuID int) error {
			onlineCalls++
			return nil
		}

		ctrl := &Controller{
			containerID:   "test",
			cpuHotplugger: mock,
			onlineCPU:     onliner,
			config:        cfg,
			currentCPUs:   1,
			maxCPUs:       4,
		}

		ctx := context.Background()
		err := ctrl.scaleUp(ctx, 2)

		require.NoError(t, err)
		assert.Equal(t, 1, onlineCalls)
	})
}

func TestController_ScaleDown(t *testing.T) {
	t.Run("removes vCPU successfully", func(t *testing.T) {
		mock := &mockCPUHotplugger{
			cpus: []vm.CPUInfo{{CPUIndex: 0}, {CPUIndex: 1}},
		}
		cfg := DefaultConfig()

		ctrl := &Controller{
			containerID:   "test",
			cpuHotplugger: mock,
			config:        cfg,
			currentCPUs:   2,
			bootCPUs:      1,
		}

		ctx := context.Background()
		err := ctrl.scaleDown(ctx, 1)

		require.NoError(t, err)
		assert.Equal(t, 1, ctrl.currentCPUs)
		assert.Equal(t, int32(1), mock.unplugCalls.Load())
		assert.Equal(t, int32(1), mock.lastUnplugID.Load()) // Should remove CPU 1 (never remove CPU 0)
		assert.False(t, ctrl.lastScaleDown.IsZero())
	})

	t.Run("does not remove CPU 0", func(t *testing.T) {
		mock := &mockCPUHotplugger{
			cpus: []vm.CPUInfo{{CPUIndex: 0}},
		}
		cfg := DefaultConfig()

		ctrl := &Controller{
			containerID:   "test",
			cpuHotplugger: mock,
			config:        cfg,
			currentCPUs:   1,
			bootCPUs:      1,
		}

		ctx := context.Background()
		err := ctrl.scaleDown(ctx, 0)

		require.NoError(t, err)
		assert.Equal(t, int32(0), mock.unplugCalls.Load()) // Should not attempt to unplug CPU 0
	})

	t.Run("continues on unplug error", func(t *testing.T) {
		mock := &mockCPUHotplugger{
			cpus:      []vm.CPUInfo{{CPUIndex: 0}, {CPUIndex: 1}, {CPUIndex: 2}},
			unplugErr: errors.New("unplug failed"),
		}
		cfg := DefaultConfig()

		ctrl := &Controller{
			containerID:   "test",
			cpuHotplugger: mock,
			config:        cfg,
			currentCPUs:   3,
			bootCPUs:      1,
		}

		ctx := context.Background()
		err := ctrl.scaleDown(ctx, 1)

		require.NoError(t, err) // Should not fail (best-effort)
		assert.Equal(t, int32(2), mock.unplugCalls.Load())
	})

	t.Run("calls offliner callback", func(t *testing.T) {
		mock := &mockCPUHotplugger{
			cpus: []vm.CPUInfo{{CPUIndex: 0}, {CPUIndex: 1}},
		}
		cfg := DefaultConfig()

		offlineCalls := 0
		offliner := func(ctx context.Context, cpuID int) error {
			offlineCalls++
			return nil
		}

		ctrl := &Controller{
			containerID:   "test",
			cpuHotplugger: mock,
			offlineCPU:    offliner,
			config:        cfg,
			currentCPUs:   2,
			bootCPUs:      1,
		}

		ctx := context.Background()
		err := ctrl.scaleDown(ctx, 1)

		require.NoError(t, err)
		assert.Equal(t, 1, offlineCalls)
	})
}

func TestController_CalculateTargetCPUs(t *testing.T) {
	t.Run("maintains current when no stats provider", func(t *testing.T) {
		ctrl := &Controller{
			containerID: "test",
			currentCPUs: 2,
			maxCPUs:     4,
			bootCPUs:    1,
			config:      DefaultConfig(),
		}

		ctx := context.Background()
		target := ctrl.calculateTargetCPUs(ctx)

		assert.Equal(t, 2, target) // Should maintain current
	})

	t.Run("scales up on high usage", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.ScaleUpThreshold = 80.0

		// Mock stats provider that returns high usage
		var callCount int32
		statsProvider := func(ctx context.Context) (uint64, uint64, error) {
			count := atomic.AddInt32(&callCount, 1)
			// First call initializes baseline, second call calculates usage
			if count == 1 {
				return 0, 0, nil
			}
			// Return high CPU usage (90% over 1 CPU for 100ms = 90000 usec)
			return 90000, 0, nil
		}

		ctrl := &Controller{
			containerID: "test",
			currentCPUs: 1,
			maxCPUs:     4,
			bootCPUs:    1,
			config:      cfg,
			stats:       statsProvider,
		}

		ctx := context.Background()

		// First call initializes baseline
		_ = ctrl.calculateTargetCPUs(ctx)

		// Simulate time passing
		ctrl.lastSampleTime = time.Now().Add(-100 * time.Millisecond)
		ctrl.lastUsageUsec = 0

		// Second call should trigger scale up
		target := ctrl.calculateTargetCPUs(ctx)

		assert.Equal(t, 2, target) // Should want to scale up
	})

	t.Run("scales down on low usage", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.ScaleDownThreshold = 50.0
		cfg.EnableScaleDown = true

		// Mock stats provider that returns low usage
		var callCount int32
		statsProvider := func(ctx context.Context) (uint64, uint64, error) {
			count := atomic.AddInt32(&callCount, 1)
			if count == 1 {
				return 0, 0, nil
			}
			// Return low CPU usage (20% over 2 CPUs for 100ms = 40000 usec total, 20% each)
			return 40000, 0, nil
		}

		ctrl := &Controller{
			containerID: "test",
			currentCPUs: 2,
			maxCPUs:     4,
			bootCPUs:    1,
			config:      cfg,
			stats:       statsProvider,
		}

		ctx := context.Background()

		// First call initializes baseline
		_ = ctrl.calculateTargetCPUs(ctx)

		// Simulate time passing
		ctrl.lastSampleTime = time.Now().Add(-100 * time.Millisecond)
		ctrl.lastUsageUsec = 0

		// Second call should suggest scale down
		target := ctrl.calculateTargetCPUs(ctx)

		assert.Equal(t, 1, target) // Should want to scale down
	})

	t.Run("no scale up when throttled", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.ScaleUpThreshold = 80.0
		cfg.ScaleUpThrottleLimit = 5.0

		// Mock stats provider that returns high usage but also throttled
		var callCount int32
		statsProvider := func(ctx context.Context) (uint64, uint64, error) {
			count := atomic.AddInt32(&callCount, 1)
			if count == 1 {
				return 0, 0, nil
			}
			// High usage but >5% throttled
			return 90000, 10000, nil // 10% throttled
		}

		ctrl := &Controller{
			containerID: "test",
			currentCPUs: 1,
			maxCPUs:     4,
			bootCPUs:    1,
			config:      cfg,
			stats:       statsProvider,
		}

		ctx := context.Background()

		// First call initializes baseline
		_ = ctrl.calculateTargetCPUs(ctx)

		// Simulate time passing
		ctrl.lastSampleTime = time.Now().Add(-100 * time.Millisecond)
		ctrl.lastUsageUsec = 0
		ctrl.lastThrottledUsec = 0

		// Second call should NOT scale up due to throttling
		target := ctrl.calculateTargetCPUs(ctx)

		assert.Equal(t, 1, target) // Should maintain current due to throttling
	})
}

func TestController_SampleCPU(t *testing.T) {
	t.Run("returns false when no stats provider", func(t *testing.T) {
		ctrl := &Controller{
			containerID: "test",
			currentCPUs: 1,
		}

		ctx := context.Background()
		_, _, ok, err := ctrl.sampleCPU(ctx)

		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("initializes baseline on first call", func(t *testing.T) {
		statsProvider := func(ctx context.Context) (uint64, uint64, error) {
			return 1000, 100, nil
		}

		ctrl := &Controller{
			containerID: "test",
			currentCPUs: 1,
			stats:       statsProvider,
		}

		ctx := context.Background()
		_, _, ok, err := ctrl.sampleCPU(ctx)

		require.NoError(t, err)
		assert.False(t, ok) // First call returns false
		assert.Equal(t, uint64(1000), ctrl.lastUsageUsec)
		assert.Equal(t, uint64(100), ctrl.lastThrottledUsec)
	})

	t.Run("resets on counter decrease", func(t *testing.T) {
		var usageUsec uint64 = 1000
		statsProvider := func(ctx context.Context) (uint64, uint64, error) {
			return usageUsec, 0, nil
		}

		ctrl := &Controller{
			containerID:    "test",
			currentCPUs:    1,
			stats:          statsProvider,
			lastSampleTime: time.Now().Add(-time.Second),
			lastUsageUsec:  2000, // Higher than what will be returned
		}

		ctx := context.Background()
		_, _, ok, err := ctrl.sampleCPU(ctx)

		require.NoError(t, err)
		assert.False(t, ok) // Should reset baseline
	})

	t.Run("handles stats error", func(t *testing.T) {
		statsProvider := func(ctx context.Context) (uint64, uint64, error) {
			return 0, 0, errors.New("stats error")
		}

		ctrl := &Controller{
			containerID: "test",
			currentCPUs: 1,
			stats:       statsProvider,
		}

		ctx := context.Background()
		_, _, ok, err := ctrl.sampleCPU(ctx)

		require.Error(t, err)
		assert.False(t, ok)
	})
}

func TestController_CheckAndAdjust(t *testing.T) {
	t.Run("syncs CPU count on mismatch", func(t *testing.T) {
		mock := &mockCPUHotplugger{
			cpus: []vm.CPUInfo{{CPUIndex: 0}, {CPUIndex: 1}},
		}
		cfg := DefaultConfig()

		ctrl := &Controller{
			containerID:   "test",
			cpuHotplugger: mock,
			config:        cfg,
			currentCPUs:   1, // Mismatch - we think 1, but QEMU has 2
			maxCPUs:       4,
			bootCPUs:      1,
		}

		ctx := context.Background()
		err := ctrl.checkAndAdjust(ctx)

		require.NoError(t, err)
		assert.Equal(t, 2, ctrl.currentCPUs) // Should sync with actual
	})

	t.Run("respects scale up stability", func(t *testing.T) {
		mock := &mockCPUHotplugger{
			cpus: []vm.CPUInfo{{CPUIndex: 0}},
		}
		cfg := DefaultConfig()
		cfg.ScaleUpStability = 3
		cfg.ScaleUpThreshold = 80.0

		// Create a stats provider that returns incrementing usage values
		// to simulate high CPU usage over time
		baseUsage := uint64(0)
		statsProvider := func(ctx context.Context) (uint64, uint64, error) {
			baseUsage += 90000 // Add 90ms of CPU time each call (90% over 100ms at 1 CPU)
			return baseUsage, 0, nil
		}

		ctrl := &Controller{
			containerID:   "test",
			cpuHotplugger: mock,
			stats:         statsProvider,
			config:        cfg,
			currentCPUs:   1,
			maxCPUs:       4,
			bootCPUs:      1,
		}

		ctx := context.Background()

		// First call: Initialize baseline (no action)
		err := ctrl.checkAndAdjust(ctx)
		require.NoError(t, err)
		assert.Equal(t, 0, ctrl.consecutiveHighUsage)

		// Simulate time passing
		ctrl.lastSampleTime = time.Now().Add(-100 * time.Millisecond)

		// Second call: First high usage reading
		err = ctrl.checkAndAdjust(ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, ctrl.consecutiveHighUsage)
		assert.Equal(t, int32(0), mock.hotplugCalls.Load()) // Should not scale yet

		ctrl.lastSampleTime = time.Now().Add(-100 * time.Millisecond)

		// Third call: Second high usage reading
		err = ctrl.checkAndAdjust(ctx)
		require.NoError(t, err)
		assert.Equal(t, 2, ctrl.consecutiveHighUsage)
		assert.Equal(t, int32(0), mock.hotplugCalls.Load()) // Still not enough

		ctrl.lastSampleTime = time.Now().Add(-100 * time.Millisecond)

		// Fourth call: Third high usage reading - now should scale
		err = ctrl.checkAndAdjust(ctx)
		require.NoError(t, err)
		assert.Equal(t, int32(1), mock.hotplugCalls.Load()) // Should have scaled up now
	})

	t.Run("respects scale down cooldown", func(t *testing.T) {
		mock := &mockCPUHotplugger{
			cpus: []vm.CPUInfo{{CPUIndex: 0}, {CPUIndex: 1}},
		}
		cfg := DefaultConfig()
		cfg.ScaleDownCooldown = 1 * time.Hour // Long cooldown

		// Low usage stats
		var callCount int32
		statsProvider := func(ctx context.Context) (uint64, uint64, error) {
			count := atomic.AddInt32(&callCount, 1)
			if count == 1 {
				return 0, 0, nil
			}
			return 10000, 0, nil // Very low usage
		}

		ctrl := &Controller{
			containerID:   "test",
			cpuHotplugger: mock,
			stats:         statsProvider,
			config:        cfg,
			currentCPUs:   2,
			maxCPUs:       4,
			bootCPUs:      1,
			lastScaleDown: time.Now(), // Just scaled down
		}

		ctx := context.Background()

		// Initialize and check
		_ = ctrl.checkAndAdjust(ctx)
		ctrl.lastSampleTime = time.Now().Add(-100 * time.Millisecond)
		ctrl.lastUsageUsec = 0

		err := ctrl.checkAndAdjust(ctx)

		require.NoError(t, err)
		assert.Equal(t, int32(0), mock.unplugCalls.Load()) // Should not scale down due to cooldown
	})
}

// Benchmarks

func BenchmarkController_SampleCPU(b *testing.B) {
	statsProvider := func(ctx context.Context) (uint64, uint64, error) {
		return 50000, 1000, nil
	}

	ctrl := &Controller{
		containerID:    "bench",
		currentCPUs:    2,
		stats:          statsProvider,
		lastSampleTime: time.Now().Add(-100 * time.Millisecond),
		lastUsageUsec:  40000,
	}

	ctx := context.Background()

	b.ResetTimer()
	for range b.N {
		ctrl.lastSampleTime = time.Now().Add(-100 * time.Millisecond)
		_, _, _, _ = ctrl.sampleCPU(ctx)
	}
}

func BenchmarkController_CheckAndAdjust(b *testing.B) {
	mock := &mockCPUHotplugger{
		cpus: []vm.CPUInfo{{CPUIndex: 0}},
	}

	ctrl := &Controller{
		containerID:   "bench",
		cpuHotplugger: mock,
		config:        DefaultConfig(),
		currentCPUs:   1,
		maxCPUs:       4,
		bootCPUs:      1,
	}

	ctx := context.Background()

	b.ResetTimer()
	for range b.N {
		_ = ctrl.checkAndAdjust(ctx)
	}
}
