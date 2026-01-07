//go:build linux

package network

import (
	"sync/atomic"
	"time"
)

// Metrics tracks CNI operation statistics.
// All fields are safe for concurrent access.
type Metrics struct {
	// Setup metrics
	SetupAttempts     atomic.Int64
	SetupSuccesses    atomic.Int64
	SetupFailures     atomic.Int64
	ResourceConflicts atomic.Int64

	// Teardown metrics
	TeardownAttempts  atomic.Int64
	TeardownSuccesses atomic.Int64
	TeardownFailures  atomic.Int64

	// IPAM metrics
	IPAMLeaksDetected atomic.Int64

	// Timing (nanoseconds, use time.Duration for display)
	TotalSetupTimeNs    atomic.Int64
	TotalTeardownTimeNs atomic.Int64
}

// RecordSetup records a setup attempt result.
func (m *Metrics) RecordSetup(success bool, conflict bool, duration time.Duration) {
	m.SetupAttempts.Add(1)
	m.TotalSetupTimeNs.Add(int64(duration))

	if success {
		m.SetupSuccesses.Add(1)
	} else {
		m.SetupFailures.Add(1)
	}
	if conflict {
		m.ResourceConflicts.Add(1)
	}
}

// RecordTeardown records a teardown attempt result.
func (m *Metrics) RecordTeardown(success bool, duration time.Duration) {
	m.TeardownAttempts.Add(1)
	m.TotalTeardownTimeNs.Add(int64(duration))

	if success {
		m.TeardownSuccesses.Add(1)
	} else {
		m.TeardownFailures.Add(1)
	}
}

// RecordIPAMLeak records a detected IPAM leak.
func (m *Metrics) RecordIPAMLeak() {
	m.IPAMLeaksDetected.Add(1)
}

// Reset resets all metrics to zero. Useful for testing.
func (m *Metrics) Reset() {
	m.SetupAttempts.Store(0)
	m.SetupSuccesses.Store(0)
	m.SetupFailures.Store(0)
	m.ResourceConflicts.Store(0)
	m.TeardownAttempts.Store(0)
	m.TeardownSuccesses.Store(0)
	m.TeardownFailures.Store(0)
	m.IPAMLeaksDetected.Store(0)
	m.TotalSetupTimeNs.Store(0)
	m.TotalTeardownTimeNs.Store(0)
}

// MetricsSnapshot is a point-in-time copy of metrics values.
// Useful for logging or exporting metrics.
type MetricsSnapshot struct {
	SetupAttempts     int64
	SetupSuccesses    int64
	SetupFailures     int64
	ResourceConflicts int64
	TeardownAttempts  int64
	TeardownSuccesses int64
	TeardownFailures  int64
	IPAMLeaksDetected int64
	AvgSetupTimeMs    float64
	AvgTeardownTimeMs float64
}

// Snapshot returns a point-in-time copy of metrics.
func (m *Metrics) Snapshot() MetricsSnapshot {
	setupAttempts := m.SetupAttempts.Load()
	teardownAttempts := m.TeardownAttempts.Load()

	snap := MetricsSnapshot{
		SetupAttempts:     setupAttempts,
		SetupSuccesses:    m.SetupSuccesses.Load(),
		SetupFailures:     m.SetupFailures.Load(),
		ResourceConflicts: m.ResourceConflicts.Load(),
		TeardownAttempts:  teardownAttempts,
		TeardownSuccesses: m.TeardownSuccesses.Load(),
		TeardownFailures:  m.TeardownFailures.Load(),
		IPAMLeaksDetected: m.IPAMLeaksDetected.Load(),
	}

	// Calculate averages
	if setupAttempts > 0 {
		snap.AvgSetupTimeMs = float64(m.TotalSetupTimeNs.Load()) / float64(setupAttempts) / 1e6
	}
	if teardownAttempts > 0 {
		snap.AvgTeardownTimeMs = float64(m.TotalTeardownTimeNs.Load()) / float64(teardownAttempts) / 1e6
	}

	return snap
}
