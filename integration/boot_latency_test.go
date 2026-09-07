//go:build linux && integration

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
}

// measureBootOnce boots one container whose entrypoint prints a readiness token
// immediately, returning both intervals described on TestBootLatency.
func measureBootOnce(t *testing.T, ctx context.Context, client *containerd.Client, cfg testConfig, image containerd.Image, i int) bootSample {
	t.Helper()
	name := fmt.Sprintf("qbx-boot-%d-%s", i, strings.ReplaceAll(time.Now().Format("150405.000"), ".", ""))

	stdoutPath := filepath.Join(t.TempDir(), "stdout.log")
	stdoutFile, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatalf("create stdout file: %v", err)
	}
	defer stdoutFile.Close()

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
	task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStreams(nil, stdoutFile, nil)))
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

	start := time.Now()
	if err := task.Start(ctx); err != nil {
		t.Fatalf("start task %s: %v", name, err)
	}
	waitForOutput(t, stdoutPath, "BOOTED", 60*time.Second)
	done := time.Now()
	return bootSample{createToOutput: done.Sub(tCreate), execToOutput: done.Sub(start)}
}
