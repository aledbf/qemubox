//go:build linux

package qemu

import (
	"io"
	"testing"
	"time"

	"github.com/containerd/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShutdownConstants(t *testing.T) {
	// Verify shutdown timing constants are reasonable
	assert.Equal(t, 2*time.Second, shutdownQMPTimeout)
	assert.Equal(t, 500*time.Millisecond, shutdownACPIWait)
	assert.Equal(t, 1*time.Second, shutdownQuitTimeout)
	assert.Equal(t, 2*time.Second, shutdownQuitWait)
	assert.Equal(t, 2*time.Second, shutdownKillWait)

	// Total shutdown time should not exceed reasonable limit
	totalTimeout := shutdownQMPTimeout + shutdownACPIWait + shutdownQuitTimeout + shutdownQuitWait + shutdownKillWait
	assert.LessOrEqual(t, totalTimeout, 10*time.Second, "total shutdown timeout should not exceed 10 seconds")
}

func TestCloseAndLog(t *testing.T) {
	logger := log.L.WithField("test", true)

	t.Run("nil closer is no-op", func(t *testing.T) {
		// Should not panic
		closeAndLog(logger, "test", nil)
	})

	t.Run("successful close", func(t *testing.T) {
		closer := &mockCloser{}
		closeAndLog(logger, "test", closer)
		assert.True(t, closer.closed)
	})

	t.Run("close with error logs but doesn't panic", func(t *testing.T) {
		closer := &mockCloser{err: assert.AnError}
		closeAndLog(logger, "test", closer)
		assert.True(t, closer.closed)
	})
}

type mockCloser struct {
	closed bool
	err    error
}

func (m *mockCloser) Close() error {
	m.closed = true
	return m.err
}

func TestInstance_NilFieldsSafety(t *testing.T) {
	logger := log.L.WithField("test", true)

	t.Run("closeClientConnections with nil fields", func(t *testing.T) {
		q := &Instance{}
		q.closeClientConnections(logger) // Should not panic
	})

	t.Run("cancelBackgroundMonitors with nil cancel", func(t *testing.T) {
		q := &Instance{}
		q.cancelBackgroundMonitors(logger) // Should not panic
	})

	t.Run("cleanupAfterFailedKill with nil qmpClient", func(t *testing.T) {
		q := &Instance{}
		q.cleanupAfterFailedKill() // Should not panic
	})

	t.Run("stopQemuProcess with nil cmd", func(t *testing.T) {
		q := &Instance{cmd: nil}
		err := q.stopQemuProcess(t.Context(), logger)
		require.NoError(t, err)
	})
}

func TestInstance_CancelBackgroundMonitors(t *testing.T) {
	t.Run("calls cancel function when set", func(t *testing.T) {
		cancelled := false
		q := &Instance{
			runCancel: func() { cancelled = true },
		}
		q.cancelBackgroundMonitors(log.L.WithField("test", true))
		assert.True(t, cancelled)
	})
}

func TestInstance_Shutdown_NotRunning(t *testing.T) {
	q := &Instance{}
	q.setState(vmStateShutdown)
	assert.Equal(t, vmStateShutdown, q.getState())
}

// Verify io.Closer interface satisfaction
func TestMockCloserImplementsCloser(t *testing.T) {
	var _ io.Closer = (*mockCloser)(nil)
}

// Benchmark close helper
func BenchmarkCloseAndLog(b *testing.B) {
	logger := log.L.WithField("bench", true)
	closer := &mockCloser{}

	for b.Loop() {
		closer.closed = false
		closeAndLog(logger, "test", closer)
	}
}
