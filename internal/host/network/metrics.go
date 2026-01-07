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

// global metrics instance
var globalMetrics = &Metrics{}

// GetMetrics returns the global CNI metrics.
// Safe to call from multiple goroutines.
func GetMetrics() *Metrics {
	return globalMetrics
}

// RecordSetup records a setup attempt result.
func RecordSetup(success bool, conflict bool, duration time.Duration) {
	globalMetrics.SetupAttempts.Add(1)
	globalMetrics.TotalSetupTimeNs.Add(int64(duration))

	if success {
		globalMetrics.SetupSuccesses.Add(1)
	} else {
		globalMetrics.SetupFailures.Add(1)
	}
	if conflict {
		globalMetrics.ResourceConflicts.Add(1)
	}
}

// RecordTeardown records a teardown attempt result.
func RecordTeardown(success bool, duration time.Duration) {
	globalMetrics.TeardownAttempts.Add(1)
	globalMetrics.TotalTeardownTimeNs.Add(int64(duration))

	if success {
		globalMetrics.TeardownSuccesses.Add(1)
	} else {
		globalMetrics.TeardownFailures.Add(1)
	}
}

// RecordIPAMLeak records a detected IPAM leak.
func RecordIPAMLeak() {
	globalMetrics.IPAMLeaksDetected.Add(1)
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

// ResetMetrics resets all metrics to zero. Useful for testing.
func ResetMetrics() {
	globalMetrics.SetupAttempts.Store(0)
	globalMetrics.SetupSuccesses.Store(0)
	globalMetrics.SetupFailures.Store(0)
	globalMetrics.ResourceConflicts.Store(0)
	globalMetrics.TeardownAttempts.Store(0)
	globalMetrics.TeardownSuccesses.Store(0)
	globalMetrics.TeardownFailures.Store(0)
	globalMetrics.IPAMLeaksDetected.Store(0)
	globalMetrics.TotalSetupTimeNs.Store(0)
	globalMetrics.TotalTeardownTimeNs.Store(0)
}
