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
	mu               sync.Mutex
	baseMemory       int64
	pluggedMemory    int64
	hotplugErr       error
	unplugErr        error
	querySummaryErr  error
	hotplugCallCount int
	unplugCallCount  int
}

func (m *mockQMPClient) HotplugMemory(ctx context.Context, slotID int, sizeBytes int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hotplugCallCount++
	if m.hotplugErr != nil {
		return m.hotplugErr
	}
	m.pluggedMemory += sizeBytes
	return nil
}

func (m *mockQMPClient) UnplugMemory(ctx context.Context, slotID int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unplugCallCount++
	if m.unplugErr != nil {
		return m.unplugErr
	}
	// Assume each slot is 128MB
	m.pluggedMemory -= 128 * 1024 * 1024
	if m.pluggedMemory < 0 {
		m.pluggedMemory = 0
	}
	return nil
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

// mockMemoryManager simulates guest memory online/offline
type mockMemoryManager struct {
	mu           sync.Mutex
	offlineErr   error
	onlineErr    error
	offlineCalls int
	onlineCalls  int
	offlineIDs   []int
	onlineIDs    []int
}

func (m *mockMemoryManager) offline(ctx context.Context, memoryID int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.offlineCalls++
	m.offlineIDs = append(m.offlineIDs, memoryID)
	return m.offlineErr
}

func (m *mockMemoryManager) online(ctx context.Context, memoryID int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onlineCalls++
	m.onlineIDs = append(m.onlineIDs, memoryID)
	return m.onlineErr
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
	mockMem := &mockMemoryManager{}

	config := DefaultConfig()
	config.MonitorInterval = 100 * time.Millisecond // Fast for testing

	controller := NewController(
		"test-container",
		mockQMP,
		mockStats.getStats,
		mockMem.offline,
		mockMem.online,
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
	mockMem := &mockMemoryManager{}

	config := DefaultConfig()

	// Create controller with maxMemory == bootMemory (no room for hotplug)
	controller := NewController(
		"test-container",
		mockQMP,
		mockStats.getStats,
		mockMem.offline,
		mockMem.online,
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
	mockMem := &mockMemoryManager{}

	config := DefaultConfig()
	config.MonitorInterval = 50 * time.Millisecond
	config.ScaleUpStability = 2 // Need 2 consecutive high readings
	config.ScaleUpCooldown = 50 * time.Millisecond

	controller := NewController(
		"test-container",
		mockQMP,
		mockStats.getStats,
		mockMem.offline,
		mockMem.online,
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
	hotplugCalls := mockQMP.hotplugCallCount
	mockQMP.mu.Unlock()

	if hotplugCalls == 0 {
		t.Error("expected at least one hotplug call due to high memory usage")
	}

	mockMem.mu.Lock()
	onlineCalls := mockMem.onlineCalls
	mockMem.mu.Unlock()

	if onlineCalls == 0 {
		t.Error("expected at least one online call after hotplug")
	}
}

func TestControllerNoScaleUpBelowThreshold(t *testing.T) {
	mockQMP := &mockQMPClient{
		baseMemory: 512 * 1024 * 1024,
	}
	mockStats := &mockStatsProvider{
		usageBytes: 300 * 1024 * 1024, // 300MB of 512MB = 58.6% usage (below 85%)
	}
	mockMem := &mockMemoryManager{}

	config := DefaultConfig()
	config.MonitorInterval = 50 * time.Millisecond

	controller := NewController(
		"test-container",
		mockQMP,
		mockStats.getStats,
		mockMem.offline,
		mockMem.online,
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
	hotplugCalls := mockQMP.hotplugCallCount
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
	mockMem := &mockMemoryManager{}

	config := DefaultConfig()
	config.MonitorInterval = 50 * time.Millisecond
	config.ScaleDownStability = 2 // Need 2 consecutive low readings
	config.ScaleDownCooldown = 50 * time.Millisecond
	config.EnableScaleDown = true // Enable scale-down

	controller := NewController(
		"test-container",
		mockQMP,
		mockStats.getStats,
		mockMem.offline,
		mockMem.online,
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
	ctrl.usedSlots[0] = true               // Mark slot 0 as used

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	controller.Start(ctx)

	// Wait for scale-down to occur
	time.Sleep(300 * time.Millisecond)

	controller.Stop()

	mockQMP.mu.Lock()
	unplugCalls := mockQMP.unplugCallCount
	mockQMP.mu.Unlock()

	if unplugCalls == 0 {
		t.Error("expected at least one unplug call due to low memory usage")
	}

	mockMem.mu.Lock()
	offlineCalls := mockMem.offlineCalls
	mockMem.mu.Unlock()

	if offlineCalls == 0 {
		t.Error("expected at least one offline call before unplug")
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
	mockMem := &mockMemoryManager{}

	config := DefaultConfig()
	config.MonitorInterval = 50 * time.Millisecond
	config.EnableScaleDown = false // Disabled by default

	controller := NewController(
		"test-container",
		mockQMP,
		mockStats.getStats,
		mockMem.offline,
		mockMem.online,
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
	unplugCalls := mockQMP.unplugCallCount
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
	mockMem := &mockMemoryManager{}

	config := DefaultConfig()
	config.MonitorInterval = 50 * time.Millisecond
	config.OOMSafetyMarginMB = 128 // 128MB safety margin
	config.ScaleUpStability = 2

	controller := NewController(
		"test-container",
		mockQMP,
		mockStats.getStats,
		mockMem.offline,
		mockMem.online,
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
	hotplugCalls := mockQMP.hotplugCallCount
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
	mockMem := &mockMemoryManager{}

	config := DefaultConfig()
	config.MonitorInterval = 50 * time.Millisecond
	config.ScaleUpStability = 1 // Fast for testing

	// Max memory = boot memory, no room to scale
	controller := NewController(
		"test-container",
		mockQMP,
		mockStats.getStats,
		mockMem.offline,
		mockMem.online,
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
	hotplugCalls := mockQMP.hotplugCallCount
	mockQMP.mu.Unlock()

	if hotplugCalls > 0 {
		t.Errorf("expected no hotplug when already at max memory, got %d calls", hotplugCalls)
	}
}

func TestControllerErrorHandling(t *testing.T) {
	mockQMP := &mockQMPClient{
		baseMemory: 512 * 1024 * 1024,
		hotplugErr: errors.New("simulated hotplug error"),
	}
	mockStats := &mockStatsProvider{
		usageBytes: 450 * 1024 * 1024, // High usage to trigger scale-up
	}
	mockMem := &mockMemoryManager{}

	config := DefaultConfig()
	config.MonitorInterval = 50 * time.Millisecond
	config.ScaleUpStability = 1

	controller := NewController(
		"test-container",
		mockQMP,
		mockStats.getStats,
		mockMem.offline,
		mockMem.online,
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

	// Test should pass if controller doesn't crash
}

func TestFindFreeSlot(t *testing.T) {
	tests := []struct {
		name      string
		usedSlots map[int]bool
		want      int
	}{
		{
			name:      "all slots free",
			usedSlots: map[int]bool{},
			want:      0,
		},
		{
			name:      "first slot used",
			usedSlots: map[int]bool{0: true},
			want:      1,
		},
		{
			name:      "first two slots used",
			usedSlots: map[int]bool{0: true, 1: true},
			want:      2,
		},
		{
			name:      "all slots used",
			usedSlots: map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true},
			want:      -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := &Controller{
				usedSlots: tt.usedSlots,
				config:    Config{MaxSlots: 8},
			}
			if got := controller.findFreeSlot(); got != tt.want {
				t.Errorf("findFreeSlot() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFindUsedSlot(t *testing.T) {
	tests := []struct {
		name      string
		usedSlots map[int]bool
		want      int
	}{
		{
			name:      "no slots used",
			usedSlots: map[int]bool{},
			want:      -1,
		},
		{
			name:      "single slot used",
			usedSlots: map[int]bool{3: true},
			want:      3,
		},
		{
			name:      "multiple slots used returns highest",
			usedSlots: map[int]bool{2: true, 5: true},
			want:      5,
		},
		{
			name:      "first slot only",
			usedSlots: map[int]bool{0: true},
			want:      0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := &Controller{
				usedSlots: tt.usedSlots,
				config:    Config{MaxSlots: 8},
			}
			if got := controller.findUsedSlot(); got != tt.want {
				t.Errorf("findUsedSlot() = %d, want %d", got, tt.want)
			}
		})
	}
}
