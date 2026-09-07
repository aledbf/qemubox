//go:build linux && integration

package integration

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The measurement tests in this package time a VM boot to the millisecond on a
// host they do not own. This machine is also the repository's self-hosted CI
// runner, so a `task build:kernel` from an unrelated push takes all 20 cores for
// minutes at a time - and a boot measured underneath that is not a slow boot, it
// is a meaningless number. One such run showed up here as a pid1-entry of 78 ms
// against a median of 51, the only outlier in twelve.
//
// The gate is on measured CPU use rather than on the runner's process, for two
// reasons: it catches anything heavy (a manual build, another benchmark, an
// image pull), and it does not fire merely because these tests are themselves
// running inside a CI job, where a Runner.Worker always exists and is idle.
const (
	// quietHostBusyPct is the share of host CPU that may be in use before a
	// measurement is considered untrustworthy. A settled host sits near zero; a
	// kernel build pins every core.
	quietHostBusyPct = 25.0

	// quietHostWindow is how long CPU use is sampled for. Long enough to look
	// past a scheduling hiccup, short enough not to pad every test.
	quietHostWindow = 200 * time.Millisecond

	// quietHostTimeout is how long to wait for a busy host to settle before
	// giving up and skipping.
	quietHostTimeout = 30 * time.Second
)

// requireQuietHost blocks until ambient host CPU use falls below
// quietHostBusyPct, and skips the test if it has not settled within
// quietHostTimeout. The measured value is always logged: a number just under the
// threshold is worth seeing next to the timing it produced.
//
// A host whose CPU use cannot be read is treated as quiet - failing to measure
// the machine is not a reason to refuse to measure the boot.
func requireQuietHost(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(quietHostTimeout)
	for {
		busy, err := hostBusyPct(quietHostWindow)
		if err != nil {
			t.Logf("HOST_QUIET could not read /proc/stat (%v); measuring anyway", err)
			return
		}
		if busy <= quietHostBusyPct {
			t.Logf("HOST_QUIET host %.1f%% busy (threshold %.0f%%)", busy, quietHostBusyPct)
			return
		}
		if time.Now().After(deadline) {
			t.Skipf("HOST_QUIET host %.1f%% busy after %s, above the %.0f%% threshold: "+
				"a boot measured under this load is not a measurement (CI build? concurrent benchmark?)",
				busy, quietHostTimeout, quietHostBusyPct)
		}
		time.Sleep(quietHostWindow)
	}
}

// hostBusyPct samples /proc/stat twice, window apart, and returns the percentage
// of aggregate CPU time that was not idle in between.
func hostBusyPct(window time.Duration) (float64, error) {
	total1, idle1, err := cpuTimes()
	if err != nil {
		return 0, err
	}
	time.Sleep(window)
	total2, idle2, err := cpuTimes()
	if err != nil {
		return 0, err
	}

	dTotal := total2 - total1
	if dTotal == 0 {
		return 0, fmt.Errorf("no CPU time elapsed between samples")
	}
	return 100 * float64(dTotal-(idle2-idle1)) / float64(dTotal), nil
}

// cpuTimes returns the aggregate and idle jiffy counters from the first line of
// /proc/stat. Idle counts both idle and iowait, matching what tools like top
// treat as time not spent on anyone's work.
func cpuTimes() (total, idle uint64, err error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	line, _, _ := strings.Cut(string(data), "\n")
	fields := strings.Fields(line)
	if len(fields) < 6 || fields[0] != "cpu" {
		return 0, 0, fmt.Errorf("unexpected /proc/stat first line: %q", line)
	}
	for i, f := range fields[1:] {
		v, parseErr := strconv.ParseUint(f, 10, 64)
		if parseErr != nil {
			return 0, 0, fmt.Errorf("parsing /proc/stat field %d: %w", i, parseErr)
		}
		total += v
		// user nice system idle iowait ...: index 3 is idle, 4 is iowait.
		if i == 3 || i == 4 {
			idle += v
		}
	}
	return total, idle, nil
}
