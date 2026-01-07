//go:build linux

package network

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestMetricsRecording(t *testing.T) {
	m := &Metrics{}

	// Record some setups
	m.RecordSetup(true, false, 100*time.Millisecond)
	m.RecordSetup(true, false, 200*time.Millisecond)
	m.RecordSetup(false, true, 50*time.Millisecond)  // failure with conflict
	m.RecordSetup(false, false, 75*time.Millisecond) // failure without conflict

	assert.Equal(t, int64(4), m.SetupAttempts.Load())
	assert.Equal(t, int64(2), m.SetupSuccesses.Load())
	assert.Equal(t, int64(2), m.SetupFailures.Load())
	assert.Equal(t, int64(1), m.ResourceConflicts.Load())

	// Record some teardowns
	m.RecordTeardown(true, 50*time.Millisecond)
	m.RecordTeardown(false, 30*time.Millisecond)

	assert.Equal(t, int64(2), m.TeardownAttempts.Load())
	assert.Equal(t, int64(1), m.TeardownSuccesses.Load())
	assert.Equal(t, int64(1), m.TeardownFailures.Load())

	// Record IPAM leak
	m.RecordIPAMLeak()
	assert.Equal(t, int64(1), m.IPAMLeaksDetected.Load())
}

func TestMetricsSnapshot(t *testing.T) {
	m := &Metrics{}

	// Record some operations
	m.RecordSetup(true, false, 100*time.Millisecond)
	m.RecordSetup(true, false, 200*time.Millisecond)
	m.RecordTeardown(true, 50*time.Millisecond)
	m.RecordIPAMLeak()

	snap := m.Snapshot()

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
	m := &Metrics{}

	snap := m.Snapshot()

	assert.Equal(t, int64(0), snap.SetupAttempts)
	assert.InDelta(t, 0.0, snap.AvgSetupTimeMs, 0.001)
	assert.InDelta(t, 0.0, snap.AvgTeardownTimeMs, 0.001)
}

func TestMetricsReset(t *testing.T) {
	m := &Metrics{}

	// Add some data
	m.RecordSetup(true, false, 100*time.Millisecond)
	m.RecordTeardown(true, 50*time.Millisecond)
	m.RecordIPAMLeak()

	// Reset
	m.Reset()

	assert.Equal(t, int64(0), m.SetupAttempts.Load())
	assert.Equal(t, int64(0), m.SetupSuccesses.Load())
	assert.Equal(t, int64(0), m.TeardownAttempts.Load())
	assert.Equal(t, int64(0), m.IPAMLeaksDetected.Load())
}
