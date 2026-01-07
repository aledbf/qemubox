//go:build linux

package runc

import (
	"context"
	"os"
	"testing"

	"github.com/containerd/cgroups/v3/cgroup2/stats"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isCgroupV2Available checks if cgroup v2 (unified hierarchy) is available
func isCgroupV2Available() bool {
	_, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	return err == nil
}

func TestNewCgroupManager(t *testing.T) {
	// NewCgroupManager wraps the v2 manager
	// We can't create a real manager without cgroup2 setup, but we test nil handling
	mgr := NewCgroupManager(nil)
	require.NotNil(t, mgr)

	cm, ok := mgr.(*cgroupManager)
	require.True(t, ok)
	assert.Nil(t, cm.manager) // Underlying manager is nil
}

func TestCgroupManager_Stats_NilManager(t *testing.T) {
	// Test that Stats handles nil manager gracefully (panics)
	// This documents expected behavior - callers must provide valid manager
	mgr := &cgroupManager{manager: nil}

	ctx := context.Background()

	// This will panic because we can't call methods on nil manager
	defer func() {
		r := recover()
		assert.NotNil(t, r, "calling Stats on nil manager should panic")
	}()

	_, _ = mgr.Stats(ctx)
}

func TestLoadProcessCgroup(t *testing.T) {
	t.Run("invalid PID", func(t *testing.T) {
		ctx := context.Background()
		_, err := LoadProcessCgroup(ctx, -1)
		assert.Error(t, err, "invalid PID should return error")
	})

	t.Run("non-existent PID", func(t *testing.T) {
		ctx := context.Background()
		_, err := LoadProcessCgroup(ctx, 999999999)
		assert.Error(t, err, "non-existent PID should return error")
	})

	t.Run("PID 1 (requires cgroup v2)", func(t *testing.T) {
		if !isCgroupV2Available() {
			t.Skip("cgroup v2 (unified mode) not available")
		}

		ctx := context.Background()
		mgr, err := LoadProcessCgroup(ctx, 1)

		if err != nil {
			t.Skipf("cannot load cgroup for PID 1: %v", err)
		}

		require.NotNil(t, mgr)
	})
}

// MockCgroupManager provides a test implementation of CgroupManager
type MockCgroupManager struct {
	StatsResult           *stats.Metrics
	StatsError            error
	EnableControllersErr  error
	StatsCalls            int
	EnableControllerCalls int
}

func (m *MockCgroupManager) Stats(ctx context.Context) (*stats.Metrics, error) {
	m.StatsCalls++
	return m.StatsResult, m.StatsError
}

func (m *MockCgroupManager) EnableControllers(ctx context.Context) error {
	m.EnableControllerCalls++
	return m.EnableControllersErr
}

func TestMockCgroupManager(t *testing.T) {
	// Test that MockCgroupManager implements CgroupManager interface
	var _ CgroupManager = (*MockCgroupManager)(nil)

	t.Run("returns configured stats", func(t *testing.T) {
		expectedStats := &stats.Metrics{
			CPU: &stats.CPUStat{
				UsageUsec: 1000,
			},
		}

		mock := &MockCgroupManager{
			StatsResult: expectedStats,
		}

		ctx := context.Background()
		result, err := mock.Stats(ctx)

		require.NoError(t, err)
		assert.Equal(t, expectedStats, result)
		assert.Equal(t, 1, mock.StatsCalls)
	})

	t.Run("returns configured error", func(t *testing.T) {
		mock := &MockCgroupManager{
			StatsError: assert.AnError,
		}

		ctx := context.Background()
		_, err := mock.Stats(ctx)

		require.Error(t, err)
		assert.Equal(t, assert.AnError, err)
	})

	t.Run("enable controllers", func(t *testing.T) {
		mock := &MockCgroupManager{}

		ctx := context.Background()
		err := mock.EnableControllers(ctx)

		require.NoError(t, err)
		assert.Equal(t, 1, mock.EnableControllerCalls)
	})
}
