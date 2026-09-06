//go:build linux && integration

package integration

import (
	"os"
	"strings"
)

// Helpers shared by the boot-profiling tests.
//
// What used to live here was TestKernelBootProfile, which scraped `initcall ...
// returned ... after N usecs` lines out of the console log. It is gone:
// TestKernelBootProfileComplete reads the same lines from /dev/kmsg, which is
// where all of them are — the console only ever carried the late ones, as that
// test's own comment said. Keeping it meant the profiling boot had to print the
// whole verbose stream to a console, and registering that console replayed the
// printk ring inside an initcall, which the profile then charged to
// virtio_console_init: 24 ms of measuring the measurement.

// annotationDebugBoot enables per-VM kernel boot profiling (initcall_debug).
const annotationDebugBoot = "io.spin.debug.boot"

// logDirBase returns the host directory that holds per-container console logs.
func logDirBase() string {
	if v := os.Getenv("SPINBOX_LOG_DIR"); v != "" {
		return v
	}
	return "/var/log/spin-stack"
}

// lastLines returns the final n lines of s, for failure messages that would
// otherwise print a whole boot log.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
