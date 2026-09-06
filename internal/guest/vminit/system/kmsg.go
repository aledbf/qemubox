//go:build linux

package system

import (
	"context"
	"errors"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/containerd/log"
	"golang.org/x/sys/unix"
)

// kmsgInitcallRE matches an initcall_debug completion line embedded in a
// /dev/kmsg record message, e.g.:
//
//	initcall pci_subsys_init+0x0/0x40 returned 0 after 12345 usecs
var kmsgInitcallRE = regexp.MustCompile(`initcall (\S+) returned \S+ after (\d+) usecs`)

// kmsgTopN bounds how many of the slowest initcalls we print.
const kmsgTopN = 25

type kmsgInitcall struct {
	name string
	usec int
}

// kmsgRecord is one kernel log line with the timestamp it carries.
type kmsgRecord struct {
	tsUS int64
	msg  string
}

// kmsgGap is a stretch of boot in which the kernel said nothing, named by the
// lines on either side of it and stamped with where in the boot it falls.
//
// Both sides, because the first run of this reported only `after` and the
// largest gap in the boot came back as "44695 us after: initcall
// late_trace_init returned 0 after 0 usecs" — which says the silence begins once
// the initcalls are done and nothing at all about what it is. The line that ends
// a silence is the one that names it.
type kmsgGap struct {
	usec   int64
	atUS   int64
	after  string
	before string
}

// kmsgGapsN bounds how many silent stretches we print, and kmsgGapFloorUS the
// size below which one is not worth a line.
const (
	kmsgGapsN      = 10
	kmsgGapFloorUS = 500
)

// kmsgCallingRE matches the line initcall_debug prints *before* running an
// initcall. A gap that opens after one of those is the initcall itself, which
// is already reported by name and duration, so it is not a gap worth listing:
// what these are for is the time that belongs to no initcall at all.
var kmsgCallingRE = regexp.MustCompile(`^calling  \S+ @ `)

// DumpKernelBootProfile reads the kernel ring buffer (/dev/kmsg) and emits a
// compact KMSG_PROFILE summary of the boot's initcalls.
//
// Unlike the console-based kernel profile, this sees the *complete* set -
// including the early core/subsys initcalls (acpi_init, pci_subsys_init, ...)
// that run before virtio-console registers and that the hvc0 console therefore
// never replays. /dev/kmsg holds the full kernel log regardless of console, and
// log_buf_len=4M (set in debug-boot) keeps it from wrapping. last_ts_us is the
// timestamp of the last kernel record seen at drain time; it is only a rough
// upper bound on kernel boot - driver/handoff messages emitted after PID1
// starts push it past the real boundary. For the precise kernel-boot number use
// the VMINITD_READY pid1-entry stamp (CLOCK_BOOTTIME at init exec). The gap
// between last_ts_us and sum_us still indicates non-initcall kernel work
// (decompression, mm/SMP bring-up, ...).
//
// No-op unless boot profiling (spin.profile) is enabled.
func DumpKernelBootProfile(ctx context.Context) {
	if !profileEnabled() {
		return
	}

	calls, records, lastTS, err := readKmsgInitcalls()
	if err != nil {
		log.G(ctx).WithError(err).Warn("kmsg boot profile: read failed")
		return
	}
	if len(calls) == 0 {
		log.G(ctx).Warn("kmsg boot profile: no initcall_debug records (is initcall_debug set?)")
		return
	}

	sum := 0
	for _, c := range calls {
		sum += c.usec
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].usec > calls[j].usec })

	log.G(ctx).Infof("KMSG_PROFILE initcalls=%d sum_us=%d last_ts_us=%d", len(calls), sum, lastTS)
	for i, c := range calls {
		if i >= kmsgTopN {
			break
		}
		log.G(ctx).Infof("KMSG_PROFILE %8d us  %s", c.usec, c.name)
	}

	// The initcalls are the half of boot that names itself. The other half - the
	// difference between last_ts_us and sum_us, which is the larger of the two -
	// is time no initcall accounts for, and the only thing the ring buffer can
	// say about it is where the kernel fell silent.
	for _, g := range kmsgGaps(records) {
		log.G(ctx).Infof("KMSG_GAP %8d us  at %8d us  after: %s  ||  before: %s",
			g.usec, g.atUS, g.after, g.before)
	}

	// And, when the initcall tracepoints were enabled at boot, how long each
	// initcall *level* took end to end. Subtracting the initcalls of a level from
	// its wall time splits the silence into "before the level's first initcall"
	// and "between them".
	for _, l := range initcallLevels() {
		log.G(ctx).Infof("KMSG_LEVEL %8d us  %s", l.usec, l.name)
	}
}

// kmsgGaps returns the largest stretches between consecutive kernel records,
// skipping the ones that open right after initcall_debug announced a call: that
// time is the initcall, and it is already reported by name.
func kmsgGaps(records []kmsgRecord) []kmsgGap {
	var gaps []kmsgGap
	for i := 1; i < len(records); i++ {
		d := records[i].tsUS - records[i-1].tsUS
		if d < kmsgGapFloorUS || kmsgCallingRE.MatchString(records[i-1].msg) {
			continue
		}
		gaps = append(gaps, kmsgGap{
			usec:   d,
			atUS:   records[i-1].tsUS,
			after:  records[i-1].msg,
			before: records[i].msg,
		})
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].usec > gaps[j].usec })
	if len(gaps) > kmsgGapsN {
		gaps = gaps[:kmsgGapsN]
	}
	return gaps
}

// readKmsgInitcalls reads all currently-buffered /dev/kmsg records and returns
// the initcall timings plus the highest record timestamp seen (microseconds
// since boot).
func readKmsgInitcalls() ([]kmsgInitcall, []kmsgRecord, int64, error) {
	// O_NONBLOCK so Read returns EAGAIN once we have drained the buffer instead
	// of blocking for future messages. Raw unix.Read (not os.File) avoids the Go
	// runtime poller, which would otherwise wait on EAGAIN.
	fd, err := unix.Open("/dev/kmsg", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, 0, err
	}
	defer func() { _ = unix.Close(fd) }()

	var (
		calls   []kmsgInitcall
		records []kmsgRecord
		lastTS  int64
		buf     = make([]byte, 8192) // the kernel returns one record per read
	)

	for {
		n, err := unix.Read(fd, buf)
		if err != nil {
			switch {
			case errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EWOULDBLOCK):
				// Drained: caught up to the end of the buffer.
				return calls, records, lastTS, nil
			case errors.Is(err, unix.EINTR):
				continue
			case errors.Is(err, unix.EPIPE):
				// A record was overwritten between reads; the position has moved
				// on, so keep reading.
				continue
			default:
				return calls, records, lastTS, err
			}
		}
		if n == 0 {
			return calls, records, lastTS, nil
		}

		ts, msg, ok := parseKmsgRecord(string(buf[:n]))
		if !ok {
			continue
		}
		if ts > lastTS {
			lastTS = ts
		}
		records = append(records, kmsgRecord{tsUS: ts, msg: msg})
		if name, usec, ok := extractInitcall(msg); ok {
			calls = append(calls, kmsgInitcall{name: name, usec: usec})
		}
	}
}

// parseKmsgRecord splits one /dev/kmsg record into its timestamp (microseconds)
// and message text. The record format is:
//
//	<priority>,<seq>,<timestamp_us>,<flags>[,...];<message>\n[ \t<continuation>...]
//
// ok is false only when there is no ';' separating header from message.
func parseKmsgRecord(record string) (tsUS int64, msg string, ok bool) {
	semi := strings.IndexByte(record, ';')
	if semi < 0 {
		return 0, "", false
	}

	msg = record[semi+1:]
	if nl := strings.IndexByte(msg, '\n'); nl >= 0 {
		msg = msg[:nl] // drop the trailing newline and any continuation lines
	}

	fields := strings.Split(record[:semi], ",")
	if len(fields) >= 3 {
		if ts, err := strconv.ParseInt(fields[2], 10, 64); err == nil {
			tsUS = ts
		}
	}
	return tsUS, msg, true
}

// extractInitcall pulls the initcall name and duration from a record message.
func extractInitcall(msg string) (name string, usec int, ok bool) {
	m := kmsgInitcallRE.FindStringSubmatch(msg)
	if m == nil {
		return "", 0, false
	}
	usec, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, false
	}
	return m[1], usec, true
}

// tracefsTrace is where the initcall tracepoints land when the kernel was
// booted with `trace_event=initcall:*` (see BuildKernelCmdline's debug branch).
const (
	tracefsDir   = "/sys/kernel/tracing"
	tracefsTrace = tracefsDir + "/trace"
)

// traceLevelRE matches an initcall_level event in the ftrace text format:
//
//	<idle>-1  [000] .....  0.089303: initcall_level: level=early
var traceLevelRE = regexp.MustCompile(`\s(\d+)\.(\d{6}): initcall_level: level=(\S+)`)

// kmsgLevel is one initcall level and the wall time it took end to end.
type kmsgLevel struct {
	name string
	usec int64
}

// initcallLevels reports how long each initcall level took, read from the
// ftrace buffer.
//
// This is the one number /dev/kmsg cannot give: the kernel prints per-initcall
// durations but says nothing about the boundaries between levels, so the time a
// level spends outside its own initcalls - the larger half of boot - has no name
// in the log. The initcall tracepoints have been compiled in all along
// (CONFIG_EVENT_TRACING=y); all that was missing was asking for them at boot.
//
// Empty when the tracepoints were not enabled or tracefs is not there, which is
// every non-profiling boot: this must add nothing to a VM that did not ask for
// it.
func initcallLevels() []kmsgLevel {
	data, err := os.ReadFile(tracefsTrace)
	if err != nil {
		// tracefs is not mounted on a normal boot; mount it once, here, rather
		// than making every VM carry a mount it will not read.
		if err := unix.Mount("tracefs", tracefsDir, "tracefs", 0, ""); err != nil {
			return nil
		}
		if data, err = os.ReadFile(tracefsTrace); err != nil {
			return nil
		}
	}

	type mark struct {
		name string
		usec int64
	}
	var marks []mark
	for _, line := range strings.Split(string(data), "\n") {
		m := traceLevelRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		sec, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		frac, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			continue
		}
		marks = append(marks, mark{name: m[3], usec: sec*1_000_000 + frac})
	}
	if len(marks) < 2 {
		return nil
	}

	// A level runs until the next one starts. The last one has no successor in
	// the trace, so it is left out rather than guessed at.
	levels := make([]kmsgLevel, 0, len(marks)-1)
	for i := 0; i+1 < len(marks); i++ {
		levels = append(levels, kmsgLevel{name: marks[i].name, usec: marks[i+1].usec - marks[i].usec})
	}
	return levels
}
