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

// TestBootLatency measures the wall-clock time from task.Start() to the
// container's entrypoint producing output (i.e. VM boot + vminitd + crun +
// exec) across several iterations, logs min/median/max as a BOOT_METRIC line
// (grep-able for tracking over time), and fails if the FASTEST boot exceeds a
// generous ceiling - a regression guard that does not depend on manual timing.
//
// Absolute numbers reflect the active shim/containerd log level and host load;
// the MIN over N iterations is the most stable signal, so the guard is on it.
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

	samples := make([]time.Duration, 0, bootLatencyIterations)
	for i := range bootLatencyIterations {
		samples = append(samples, measureBootOnce(t, ctx, client, cfg, image, i))
	}

	sort.Slice(samples, func(a, b int) bool { return samples[a] < samples[b] })
	minD := samples[0]
	maxD := samples[len(samples)-1]
	medD := samples[len(samples)/2]

	// Single grep-able metrics line for tracking across runs.
	t.Logf("BOOT_METRIC boot_to_output_ms min=%d median=%d max=%d iterations=%d",
		minD.Milliseconds(), medD.Milliseconds(), maxD.Milliseconds(), bootLatencyIterations)

	// Guard on the MEDIAN, not the min: task.Start can occasionally return
	// before the boot fully overlaps the measured window, producing an
	// unrealistically fast outlier that would mask a real regression. The
	// median tracks the typical boot and shifts up if boots get slower.
	if ceiling := bootLatencyCeiling(); medD > ceiling {
		t.Fatalf("boot latency regression: median boot %v exceeds ceiling %v (min %v, max %v)",
			medD, ceiling, minD, maxD)
	}
}

// measureBootOnce boots one container whose entrypoint prints a readiness token
// immediately, returning the time from Start() to that token appearing.
func measureBootOnce(t *testing.T, ctx context.Context, client *containerd.Client, cfg testConfig, image containerd.Image, i int) time.Duration {
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
	return time.Since(start)
}
