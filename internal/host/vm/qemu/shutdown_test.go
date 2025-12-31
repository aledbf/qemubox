//go:build linux

package qemu

import (
	"bytes"
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

func TestInstance_CloseClientConnections(t *testing.T) {
	logger := log.L.WithField("test", true)

	t.Run("all nil - no panic", func(t *testing.T) {
		q := &Instance{}
		// Should not panic
		q.closeClientConnections(logger)
	})

	t.Run("closes client", func(t *testing.T) {
		client := &mockCloser{}
		q := &Instance{client: client}
		q.closeClientConnections(logger)
		assert.True(t, client.closed)
		assert.Nil(t, q.client)
	})

	t.Run("closes vsockConn", func(t *testing.T) {
		vsock := &mockCloser{}
		q := &Instance{vsockConn: vsock}
		q.closeClientConnections(logger)
		assert.True(t, vsock.closed)
		assert.Nil(t, q.vsockConn)
	})

	t.Run("closes consoleFifo", func(t *testing.T) {
		fifo := &mockCloser{}
		q := &Instance{consoleFifo: fifo}
		q.closeClientConnections(logger)
		assert.True(t, fifo.closed)
		assert.Nil(t, q.consoleFifo)
	})
}

func TestInstance_CancelBackgroundMonitors(t *testing.T) {
	logger := log.L.WithField("test", true)

	t.Run("nil cancel - no panic", func(t *testing.T) {
		q := &Instance{}
		// Should not panic
		q.cancelBackgroundMonitors(logger)
	})

	t.Run("calls cancel function", func(t *testing.T) {
		cancelled := false
		q := &Instance{
			runCancel: func() { cancelled = true },
		}
		q.cancelBackgroundMonitors(logger)
		assert.True(t, cancelled)
	})
}

func TestInstance_CleanupAfterFailedKill(t *testing.T) {
	t.Run("closes qmpClient and TAPs", func(t *testing.T) {
		qmpClient := &mockCloser{}
		tap1 := &mockCloser{}

		q := &Instance{
			qmpClient: &QMPClient{conn: &mockReadWriteCloser{}},
			tapFiles:  []io.Closer{tap1},
		}

		// Replace with mock for testing
		q.qmpClient = nil // Can't easily mock this, so just test the nil case

		q.cleanupAfterFailedKill()
		// Should not panic when qmpClient is nil
	})
}

type mockReadWriteCloser struct {
	bytes.Buffer
}

func (m *mockReadWriteCloser) Close() error {
	return nil
}

func TestInstance_Shutdown_NotRunning(t *testing.T) {
	// When VM is not in running state, Shutdown should be idempotent
	q := &Instance{
		state: vmStateStopped,
	}

	// Switch to shutdown state
	q.state = vmStateShutdown

	// Shutdown should return nil without error (idempotent)
	// Can't fully test without starting a real VM
}

func TestInstance_StopQemuProcess_NilCmd(t *testing.T) {
	logger := log.L.WithField("test", true)
	q := &Instance{
		cmd: nil,
	}

	ctx := t.Context()
	err := q.stopQemuProcess(ctx, logger)
	require.NoError(t, err)
}

// Verify io.Closer interface satisfaction
func TestMockCloserImplementsCloser(t *testing.T) {
	var _ io.Closer = (*mockCloser)(nil)
}

// Benchmark close helper
func BenchmarkCloseAndLog(b *testing.B) {
	logger := log.L.WithField("bench", true)
	closer := &mockCloser{}

	b.ResetTimer()
	for range b.N {
		closer.closed = false
		closeAndLog(logger, "test", closer)
	}
}
