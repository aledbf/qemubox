//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
)

// bootLatencyIterations is how many VMs to boot when sampling boot latency.
const bootLatencyIterations = 5

// bootLatencyCeiling is the regression guard on the FASTEST boot observed.
// It is intentionally generous (gross-regression detector, not a benchmark):
// normal CI variance must not trip it, but a hang or a multi-x slowdown must.
// Override with SPINBOX_BOOT_LATENCY_MAX_MS.
func bootLatencyCeiling() time.Duration {
	if v := os.Getenv("SPINBOX_BOOT_LATENCY_MAX_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return 5 * time.Second
}

// TestBootLatency measures cold start across several iterations, logs
// min/median/max as BOOT_METRIC lines (grep-able for tracking over time), and
// fails if the typical boot exceeds a generous ceiling - a regression guard that
// does not depend on manual timing.
//
// Two intervals are reported, because they are not the same thing and only one
// of them is cold start:
//
//	create_to_output - client.NewContainer's task creation through the
//	                   entrypoint's first output. NewTask is where the shim runs
//	                   Create, and Create is where the VM boots, so this is the
//	                   number a cold-start change has to move. ~209 ms here.
//	exec_to_output   - task.Start() through that same output: crun exec inside a
//	                   guest that is already up. ~12 ms here.
//
// The second used to be reported alone, under the name boot_to_output_ms and
// documented as "VM boot + vminitd + crun + exec". It never contained the VM
// boot: the timer started after NewTask had already returned. Paired with the
// 100 ms poll in waitForOutput it read 105-108 ms for everything, and no boot
// improvement of this year - jitterentropy (-3.7 ms), the guest console writes,
// the driver trim (-4.4 ms) - was visible in it.
//
// Absolute numbers reflect the active shim/containerd log level and host load.
func TestBootLatency(t *testing.T) {
	requireQuietHost(t)

	cfg := loadTestConfig()

	client := setupContainerdClient(t, cfg)
	defer client.Close()

	ensureImagePulled(t, client, cfg)

	ctx := namespaces.WithNamespace(t.Context(), cfg.Namespace)

	image, err := client.GetImage(ctx, cfg.Image)
	if err != nil {
		t.Fatalf("get image %s: %v", cfg.Image, err)
	}

	createSamples := make([]time.Duration, 0, bootLatencyIterations)
	execSamples := make([]time.Duration, 0, bootLatencyIterations)
	for i := range bootLatencyIterations {
		s := measureBootOnce(t, ctx, client, cfg, image, i)
		createSamples = append(createSamples, s.createToOutput)
		execSamples = append(execSamples, s.execToOutput)
		// The fault count is logged next to the interval because the two move
		// together and neither explains anything alone. Measured on this host:
		// booting takes 9 ms and 5 faults, restoring takes 28 ms and 4008.
		t.Logf("BOOT_METRIC iteration=%d exec_ms=%d exec_minor_faults=%d",
			i, s.execToOutput.Milliseconds(), s.execFaults)
	}

	createMin, createMed, createMax := summarize(createSamples)
	execMin, execMed, execMax := summarize(execSamples)

	// One grep-able line per interval, so each keeps its own history.
	t.Logf("BOOT_METRIC create_to_output_ms min=%d median=%d max=%d iterations=%d",
		createMin.Milliseconds(), createMed.Milliseconds(), createMax.Milliseconds(), bootLatencyIterations)
	t.Logf("BOOT_METRIC exec_to_output_ms min=%d median=%d max=%d iterations=%d",
		execMin.Milliseconds(), execMed.Milliseconds(), execMax.Milliseconds(), bootLatencyIterations)

	// Guard on the MEDIAN of cold start, not the min: an individual boot can
	// finish unusually fast and mask a real regression. The median tracks the
	// typical boot and shifts up if boots get slower.
	if ceiling := bootLatencyCeiling(); createMed > ceiling {
		t.Fatalf("boot latency regression: median cold start %v exceeds ceiling %v (min %v, max %v)",
			createMed, ceiling, createMin, createMax)
	}
}

// tokenWaiter is an io.Writer that stamps the moment a token first appears in
// the stream containerd copies out of the container.
//
// The alternative is what this test used to do: write the stream to a file and
// poll it. A poll cannot report a time it did not sample at, so its interval
// becomes the metric's resolution - at 100 ms it reported 105-108 ms for
// everything - and shortening it does not fix the shape of the problem, it only
// moves it: a 1 ms poll still quantises at 1 ms, against a run-to-run noise
// floor of about 0.6 ms, and it wakes ~200 times inside the interval it is
// measuring. On this host, CPU contention during boot is not hypothetical - it
// is 29 of the 35 ms of qemu_launch - so an instrument that competes with what
// it measures is not a neutral choice.
//
// Writing is done by one containerd goroutine, but Seen may be read from
// another, so the mutex is not decoration.
type tokenWaiter struct {
	want []byte

	mu   sync.Mutex
	buf  []byte
	at   time.Time
	seen chan struct{}
}

func newTokenWaiter(want string) *tokenWaiter {
	return &tokenWaiter{want: []byte(want), seen: make(chan struct{})}
}

func (w *tokenWaiter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.at.IsZero() {
		// Keep only enough of the tail for a token split across two writes.
		w.buf = append(w.buf, p...)
		if bytes.Contains(w.buf, w.want) {
			w.at = time.Now()
			close(w.seen)
		} else if n := len(w.want); len(w.buf) > n {
			w.buf = w.buf[len(w.buf)-n:]
		}
	}
	return len(p), nil
}

// wait blocks until the token appears, and returns when it did.
func (w *tokenWaiter) wait(t *testing.T, timeout time.Duration) time.Time {
	t.Helper()
	select {
	case <-w.seen:
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.at
	case <-time.After(timeout):
		t.Fatalf("timed out after %s waiting for %q on the container's stdout", timeout, w.want)
		return time.Time{}
	}
}

// summarize sorts samples in place and returns min, median and max.
func summarize(samples []time.Duration) (minD, medD, maxD time.Duration) {
	sort.Slice(samples, func(a, b int) bool { return samples[a] < samples[b] })
	return samples[0], samples[len(samples)/2], samples[len(samples)-1]
}

// bootSample is one boot timed at both boundaries; see TestBootLatency for what
// each interval contains.
type bootSample struct {
	createToOutput time.Duration
	execToOutput   time.Duration

	// execFaults is how many minor page faults the QEMU process took while the
	// container process was starting. See qemuMinorFaults.
	execFaults uint64
}

// measureBootOnce boots one container whose entrypoint prints a readiness token
// immediately, returning both intervals described on TestBootLatency.
func measureBootOnce(t *testing.T, ctx context.Context, client *containerd.Client, cfg testConfig, image containerd.Image, i int) bootSample {
	t.Helper()
	name := fmt.Sprintf("qbx-boot-%d-%s", i, strings.ReplaceAll(time.Now().Format("150405.000"), ".", ""))

	waiter := newTokenWaiter("BOOTED")

	container, err := client.NewContainer(ctx, name,
		containerd.WithSnapshotter(cfg.Snapshotter),
		containerd.WithImage(image),
		containerd.WithNewSnapshot(name+"-snapshot", image),
		containerd.WithRuntime(cfg.Runtime, nil),
		containerd.WithNewSpec(
			oci.WithImageConfig(image),
			// Print the token as the very first thing, then exit.
			oci.WithProcessArgs("/bin/echo", "BOOTED"),
		),
	)
	if err != nil {
		t.Fatalf("create container %s: %v", name, err)
	}
	defer func() {
		if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
			t.Logf("cleanup container %s: %v", name, err)
		}
	}()

	// Before NewTask, not after: the shim runs Create from here, and Create is
	// where QEMU is launched and the guest boots.
	tCreate := time.Now()
	task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStreams(nil, waiter, nil)))
	if err != nil {
		t.Fatalf("create task for %s: %v", name, err)
	}
	defer func() {
		if _, err := task.Delete(ctx, containerd.WithProcessKill); err != nil {
			if !strings.Contains(err.Error(), "ttrpc: closed") {
				t.Logf("cleanup task for %s: %v", name, err)
			}
		}
	}()

	if _, err := task.Wait(ctx); err != nil {
		t.Fatalf("wait for task %s: %v", name, err)
	}

	faultsBefore := qemuMinorFaults(name)
	start := time.Now()
	if err := task.Start(ctx); err != nil {
		t.Fatalf("start task %s: %v", name, err)
	}
	done := waiter.wait(t, 60*time.Second)
	faults := qemuMinorFaults(name) - faultsBefore

	return bootSample{
		createToOutput: done.Sub(tCreate),
		execToOutput:   done.Sub(start),
		execFaults:     faults,
	}
}

// qemuMinorFaults returns the minor fault count of the QEMU process serving this
// container, or 0 if it cannot be found.
//
// It exists to test one explanation of a measured regression. A VM restored from
// a template maps the template's memory copy-on-write, so every page the guest
// writes faults and is copied; starting a container process writes a lot of
// pages at once. If that is where exec_to_output's extra milliseconds go, the
// fault count is where it shows.
func qemuMinorFaults(containerID string) uint64 {
	procs, err := filepath.Glob("/proc/[0-9]*")
	if err != nil {
		return 0
	}
	for _, p := range procs {
		cmdline, err := os.ReadFile(filepath.Join(p, "cmdline"))
		if err != nil {
			continue
		}
		// Arguments are NUL-separated; the container id appears in the paths
		// QEMU was given for its console and monitor socket.
		if !bytes.Contains(cmdline, []byte("qemu-system")) || !bytes.Contains(cmdline, []byte(containerID)) {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(p, "stat"))
		if err != nil {
			return 0
		}
		// Field 10 is minflt, counting from 1. The command name in field 2 can
		// contain spaces, so the fields are counted from after its closing
		// parenthesis rather than from the start of the line.
		tail := stat[bytes.LastIndexByte(stat, ')')+1:]
		fields := strings.Fields(string(tail))
		const minfltOffsetAfterComm = 7 // state, ppid, pgrp, session, tty, tpgid, flags, minflt
		if len(fields) <= minfltOffsetAfterComm {
			return 0
		}
		n, err := strconv.ParseUint(fields[minfltOffsetAfterComm], 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}
