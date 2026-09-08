package memhotplug

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/spin-stack/spinbox/internal/host/vm/qemu"
)

// mockQMPClient simulates QEMU QMP client for testing
type mockQMPClient struct {
	mu              sync.Mutex
	baseMemory      int64
	pluggedMemory   int64
	resizeErr       error
	querySummaryErr error
	resizeCallCount int
	// grantedFloor is the least the guest will give back, in bytes above the boot
	// size. A virtio-mem device can only unplug what the guest has released, so a
	// request below this settles here instead of where it was aimed — which is
	// the case the controller has to survive without believing the number it
	// asked for.
	grantedFloor int64
}

func (m *mockQMPClient) SetPluggedMemory(ctx context.Context, sizeBytes int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resizeCallCount++
	if m.resizeErr != nil {
		return 0, m.resizeErr
	}
	if sizeBytes < m.grantedFloor {
		sizeBytes = m.grantedFloor
	}
	m.pluggedMemory = sizeBytes
	return m.pluggedMemory, nil
}

func (m *mockQMPClient) QueryMemorySizeSummary(ctx context.Context) (*qemu.MemorySizeSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.querySummaryErr != nil {
		return nil, m.querySummaryErr
	}
	return &qemu.MemorySizeSummary{
		BaseMemory:    m.baseMemory,
		PluggedMemory: m.pluggedMemory,
	}, nil
}

// mockStatsProvider simulates cgroup memory stats
type mockStatsProvider struct {
	mu          sync.Mutex
	usageBytes  int64
	returnError error
}

func (m *mockStatsProvider) getStats(ctx context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.returnError != nil {
		return 0, m.returnError
	}
	return m.usageBytes, nil
}

func TestDefaultConfig(t *testing.T) {
	config := DefaultConfig()

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"MonitorInterval", config.MonitorInterval, 10 * time.Second},
		{"ScaleUpThreshold", config.ScaleUpThreshold, 85.0},
		{"ScaleDownThreshold", config.ScaleDownThreshold, 60.0},
		{"OOMSafetyMarginMB", config.OOMSafetyMarginMB, int64(128)},
		{"IncrementSize", config.IncrementSize, int64(128 * 1024 * 1024)},
		{"EnableScaleDown", config.EnableScaleDown, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
			}
		})
	}
}

func TestNewController(t *testing.T) {
	mockQMP := &mockQMPClient{
		baseMemory: 512 * 1024 * 1024,
	}
	mockStats := &mockStatsProvider{}

	config := DefaultConfig()
	config.MonitorInterval = 100 * time.Millisecond // Fast for testing

	controller := NewController(
		"test-container",
		mockQMP,
		mockStats.getStats,
		512*1024*1024,  // boot memory
		1024*1024*1024, // max memory
		config,
	)

	// NewController now returns interface (never nil with Null Object Pattern)
	// Type assert to access internal fields for testing
	ctrl, ok := controller.(*Controller)
	if !ok {
		t.Fatal("NewController returned non-Controller implementation (unexpected for maxMemory > bootMemory)")
	}

	if ctrl.containerID != "test-container" {
		t.Errorf("expected containerID=test-container, got %s", ctrl.containerID)
	}

	if ctrl.bootMemory != 512*1024*1024 {
		t.Errorf("expected bootMemory=512MB, got %d", ctrl.bootMemory)
	}

	if ctrl.maxMemory != 1024*1024*1024 {
		t.Errorf("expected maxMemory=1GB, got %d", ctrl.maxMemory)
	}
}

func TestNewControllerNoopWhenNoHotplug(t *testing.T) {
	mockQMP := &mockQMPClient{
		baseMemory: 512 * 1024 * 1024,
	}
	mockStats := &mockStatsProvider{}

	config := DefaultConfig()

	// Create controller with maxMemory == bootMemory (no room for hotplug)
	controller := NewController(
		"test-container",
		mockQMP,
		mockStats.getStats,
		512*1024*1024, // boot memory
		512*1024*1024, // max memory (same as boot)
		config,
	)

	// Should return no-op implementation (Null Object Pattern)
	_, ok := controller.(*Controller)
	if ok {
		t.Error("expected no-op controller when maxMemory <= bootMemory, got *Controller")
	}

	// Verify it's the no-op implementation
	_, isNoop := controller.(*noopMemoryController)
	if !isNoop {
		t.Error("expected *noopMemoryController, got different type")
	}

	// Should be safe to call Start/Stop on no-op (does nothing)
	ctx := context.Background()
	controller.Start(ctx) // Should not panic or do anything
	controller.Stop()     // Should not panic or do anything
}

func TestControllerScaleUp(t *testing.T) {
	mockQMP := &mockQMPClient{
		baseMemory: 512 * 1024 * 1024,
	}
	mockStats := &mockStatsProvider{
		usageBytes: 450 * 1024 * 1024, // 450MB of 512MB = 87.9% usage
	}

	config := DefaultConfig()
	config.MonitorInterval = 50 * time.Millisecond
	config.ScaleUpStability = 2 // Need 2 consecutive high readings
	config.ScaleUpCooldown = 50 * time.Millisecond

	controller := NewController(
		"test-container",
		mockQMP,
		mockStats.getStats,
		512*1024*1024,  // boot memory
		1024*1024*1024, // max memory
		config,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	controller.Start(ctx)

	// Wait for scale-up to occur (need 2 samples at 50ms each + processing time)
	time.Sleep(300 * time.Millisecond)

	controller.Stop()

	mockQMP.mu.Lock()
	hotplugCalls := mockQMP.resizeCallCount
	mockQMP.mu.Unlock()

	if hotplugCalls == 0 {
		t.Error("expected at least one hotplug call due to high memory usage")
	}
}

func TestControllerNoScaleUpBelowThreshold(t *testing.T) {
	mockQMP := &mockQMPClient{
		baseMemory: 512 * 1024 * 1024,
	}
	mockStats := &mockStatsProvider{
		usageBytes: 300 * 1024 * 1024, // 300MB of 512MB = 58.6% usage (below 85%)
	}

	config := DefaultConfig()
	config.MonitorInterval = 50 * time.Millisecond

	controller := NewController(
		"test-container",
		mockQMP,
		mockStats.getStats,
		512*1024*1024,  // boot memory
		1024*1024*1024, // max memory
		config,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	controller.Start(ctx)
	time.Sleep(300 * time.Millisecond)
	controller.Stop()

	mockQMP.mu.Lock()
	hotplugCalls := mockQMP.resizeCallCount
	mockQMP.mu.Unlock()

	if hotplugCalls > 0 {
		t.Errorf("expected no hotplug calls below threshold, got %d", hotplugCalls)
	}
}

func TestControllerScaleDown(t *testing.T) {
	mockQMP := &mockQMPClient{
		baseMemory:    512 * 1024 * 1024,
		pluggedMemory: 128 * 1024 * 1024, // Already has extra memory
	}
	mockStats := &mockStatsProvider{
		usageBytes: 200 * 1024 * 1024, // 200MB of 640MB = 31.25% usage (below 60%)
	}

	config := DefaultConfig()
	config.MonitorInterval = 50 * time.Millisecond
	config.ScaleDownStability = 2 // Need 2 consecutive low readings
	config.ScaleDownCooldown = 50 * time.Millisecond
	config.EnableScaleDown = true // Enable scale-down

	controller := NewController(
		"test-container",
		mockQMP,
		mockStats.getStats,
		512*1024*1024, // boot memory
		768*1024*1024, // max memory
		config,
	)

	// Type assert to access internal fields for testing
	ctrl, ok := controller.(*Controller)
	if !ok {
		t.Fatal("NewController returned non-Controller implementation")
	}
	ctrl.currentMemory = 640 * 1024 * 1024 // Set current memory to include plugged

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	controller.Start(ctx)

	// Wait for scale-down to occur
	time.Sleep(300 * time.Millisecond)

	controller.Stop()

	mockQMP.mu.Lock()
	unplugCalls := mockQMP.resizeCallCount
	mockQMP.mu.Unlock()

	if unplugCalls == 0 {
		t.Error("expected at least one unplug call due to low memory usage")
	}
}

func TestControllerScaleDownDisabled(t *testing.T) {
	mockQMP := &mockQMPClient{
		baseMemory:    512 * 1024 * 1024,
		pluggedMemory: 128 * 1024 * 1024,
	}
	mockStats := &mockStatsProvider{
		usageBytes: 200 * 1024 * 1024, // Low usage
	}

	config := DefaultConfig()
	config.MonitorInterval = 50 * time.Millisecond
	config.EnableScaleDown = false // Disabled by default

	controller := NewController(
		"test-container",
		mockQMP,
		mockStats.getStats,
		512*1024*1024,
		768*1024*1024,
		config,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	controller.Start(ctx)
	time.Sleep(300 * time.Millisecond)
	controller.Stop()

	mockQMP.mu.Lock()
	unplugCalls := mockQMP.resizeCallCount
	mockQMP.mu.Unlock()

	if unplugCalls > 0 {
		t.Errorf("expected no unplug calls when scale-down disabled, got %d", unplugCalls)
	}
}

func TestControllerOOMSafetyMargin(t *testing.T) {
	mockQMP := &mockQMPClient{
		baseMemory: 512 * 1024 * 1024,
	}
	// 450MB usage, 62MB free (below 128MB safety margin) - should trigger scale-up
	mockStats := &mockStatsProvider{
		usageBytes: 450 * 1024 * 1024,
	}

	config := DefaultConfig()
	config.MonitorInterval = 50 * time.Millisecond
	config.OOMSafetyMarginMB = 128 // 128MB safety margin
	config.ScaleUpStability = 2

	controller := NewController(
		"test-container",
		mockQMP,
		mockStats.getStats,
		512*1024*1024,
		1024*1024*1024,
		config,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	controller.Start(ctx)
	time.Sleep(300 * time.Millisecond)
	controller.Stop()

	mockQMP.mu.Lock()
	hotplugCalls := mockQMP.resizeCallCount
	mockQMP.mu.Unlock()

	if hotplugCalls == 0 {
		t.Error("expected hotplug call when free memory below safety margin")
	}
}

func TestControllerMaxMemoryLimit(t *testing.T) {
	mockQMP := &mockQMPClient{
		baseMemory: 512 * 1024 * 1024,
	}
	mockStats := &mockStatsProvider{
		usageBytes: 500 * 1024 * 1024, // Very high usage
	}

	config := DefaultConfig()
	config.MonitorInterval = 50 * time.Millisecond
	config.ScaleUpStability = 1 // Fast for testing

	// Max memory = boot memory, no room to scale
	controller := NewController(
		"test-container",
		mockQMP,
		mockStats.getStats,
		512*1024*1024, // boot memory
		512*1024*1024, // max memory (same as boot, no hotplug possible)
		config,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	controller.Start(ctx)
	time.Sleep(300 * time.Millisecond)
	controller.Stop()

	mockQMP.mu.Lock()
	hotplugCalls := mockQMP.resizeCallCount
	mockQMP.mu.Unlock()

	if hotplugCalls > 0 {
		t.Errorf("expected no hotplug when already at max memory, got %d calls", hotplugCalls)
	}
}

func TestControllerErrorHandling(t *testing.T) {
	mockQMP := &mockQMPClient{
		baseMemory: 512 * 1024 * 1024,
		resizeErr:  errors.New("simulated resize error"),
	}
	mockStats := &mockStatsProvider{
		usageBytes: 450 * 1024 * 1024, // High usage to trigger scale-up
	}

	config := DefaultConfig()
	config.MonitorInterval = 50 * time.Millisecond
	config.ScaleUpStability = 1

	controller := NewController(
		"test-container",
		mockQMP,
		mockStats.getStats,
		512*1024*1024,
		1024*1024*1024,
		config,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Controller should not crash on errors
	controller.Start(ctx)
	time.Sleep(300 * time.Millisecond)
	controller.Stop()

	// Test should pass if controller does not crash
}

// TestResizeBelievesTheDeviceAndNotTheRequest is the test the virtio-mem switch
// exists for.
//
// A request is a negotiation: the device plugs memory as the guest accepts it,
// and unplugs only what the guest has already released. So asking for a size and
// recording it is wrong in a way nothing reports — the controller would go on
// believing in memory the VM does not have, and every threshold after that is
// computed against a number that is not real.
func TestResizeBelievesTheDeviceAndNotTheRequest(t *testing.T) {
	const boot = 512 * 1024 * 1024
	const granted = 256 * 1024 * 1024

	// The guest will not give back below 256 MB of the 512 that were plugged.
	mockQMP := &mockQMPClient{baseMemory: boot, pluggedMemory: 512 * 1024 * 1024, grantedFloor: granted}
	c := &Controller{
		containerID:   "test-container",
		qmpClient:     mockQMP,
		bootMemory:    boot,
		maxMemory:     4 * boot,
		currentMemory: boot + 512*1024*1024,
		config:        DefaultConfig(),
	}

	// Aim all the way back at the boot size, which the guest will not allow.
	if err := c.resize(context.Background(), boot, "down"); err != nil {
		t.Fatalf("resize: %v", err)
	}

	if want := int64(boot + granted); c.currentMemory != want {
		t.Errorf("currentMemory = %d, want %d — the controller recorded what it asked for, not what it got",
			c.currentMemory, want)
	}
}
