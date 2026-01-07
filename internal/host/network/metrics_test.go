//go:build linux

package network

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestMetricsRecording(t *testing.T) {
	// Reset metrics before test
	ResetMetrics()

	// Record some setups
	RecordSetup(true, false, 100*time.Millisecond)
	RecordSetup(true, false, 200*time.Millisecond)
	RecordSetup(false, true, 50*time.Millisecond)  // failure with conflict
	RecordSetup(false, false, 75*time.Millisecond) // failure without conflict

	m := GetMetrics()
	assert.Equal(t, int64(4), m.SetupAttempts.Load())
	assert.Equal(t, int64(2), m.SetupSuccesses.Load())
	assert.Equal(t, int64(2), m.SetupFailures.Load())
	assert.Equal(t, int64(1), m.ResourceConflicts.Load())

	// Record some teardowns
	RecordTeardown(true, 50*time.Millisecond)
	RecordTeardown(false, 30*time.Millisecond)

	assert.Equal(t, int64(2), m.TeardownAttempts.Load())
	assert.Equal(t, int64(1), m.TeardownSuccesses.Load())
	assert.Equal(t, int64(1), m.TeardownFailures.Load())

	// Record IPAM leak
	RecordIPAMLeak()
	assert.Equal(t, int64(1), m.IPAMLeaksDetected.Load())
}

func TestMetricsSnapshot(t *testing.T) {
	ResetMetrics()

	// Record some operations
	RecordSetup(true, false, 100*time.Millisecond)
	RecordSetup(true, false, 200*time.Millisecond)
	RecordTeardown(true, 50*time.Millisecond)
	RecordIPAMLeak()

	snap := GetMetrics().Snapshot()

	assert.Equal(t, int64(2), snap.SetupAttempts)
	assert.Equal(t, int64(2), snap.SetupSuccesses)
	assert.Equal(t, int64(0), snap.SetupFailures)
	assert.Equal(t, int64(1), snap.TeardownAttempts)
	assert.Equal(t, int64(1), snap.IPAMLeaksDetected)

	// Average should be 150ms = 150.0
	assert.InDelta(t, 150.0, snap.AvgSetupTimeMs, 1.0)
	assert.InDelta(t, 50.0, snap.AvgTeardownTimeMs, 1.0)
}

func TestMetricsSnapshotEmpty(t *testing.T) {
	ResetMetrics()

	snap := GetMetrics().Snapshot()

	assert.Equal(t, int64(0), snap.SetupAttempts)
	assert.InDelta(t, 0.0, snap.AvgSetupTimeMs, 0.001)
	assert.InDelta(t, 0.0, snap.AvgTeardownTimeMs, 0.001)
}

func TestResetMetrics(t *testing.T) {
	// Add some data
	RecordSetup(true, false, 100*time.Millisecond)
	RecordTeardown(true, 50*time.Millisecond)
	RecordIPAMLeak()

	// Reset
	ResetMetrics()

	m := GetMetrics()
	assert.Equal(t, int64(0), m.SetupAttempts.Load())
	assert.Equal(t, int64(0), m.SetupSuccesses.Load())
	assert.Equal(t, int64(0), m.TeardownAttempts.Load())
	assert.Equal(t, int64(0), m.IPAMLeaksDetected.Load())
}
