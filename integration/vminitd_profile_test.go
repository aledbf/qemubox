//go:build linux && integration

package integration

import (
	"os"
	"path/filepath"
	"regexp"
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

// vminitdProfileRE matches a VMINITD_PROFILE phase line emitted by the guest,
// e.g.:
//
//	VMINITD_PROFILE mount-filesystems delta_us=1234 total_us=1234
var vminitdProfileRE = regexp.MustCompile(`VMINITD_PROFILE (\S+) delta_us=(\d+) total_us=(\d+)`)

type vminitdPhase struct {
	name    string
	deltaUS int
	totalUS int
}

// TestUserspaceBootProfile boots a VM with the debug-boot annotation, which
// makes vminitd emit per-phase timings (VMINITD_PROFILE) to the console log -
// the userspace companion to TestKernelBootProfile. It parses that log and
// reports where the guest userspace boot time goes (mounts, device wait,
// extras, cgroup, network, service setup), turning the userspace half of boot
// into a CI artifact alongside the kernel initcall profile.
func TestUserspaceBootProfile(t *testing.T) {
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

	name := "qbx-uprofile-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")

	container, err := client.NewContainer(ctx, name,
		containerd.WithSnapshotter(cfg.Snapshotter),
		containerd.WithImage(image),
		containerd.WithNewSnapshot(name+"-snapshot", image),
		containerd.WithRuntime(cfg.Runtime, nil),
		containerd.WithNewSpec(
			oci.WithImageConfig(image),
			oci.WithAnnotations(map[string]string{annotationDebugBoot: "true"}),
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

	task, err := container.NewTask(ctx, cio.NullIO)
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
	if err := task.Start(ctx); err != nil {
		t.Fatalf("start task %s: %v", name, err)
	}
	waitForTaskStatus(t, ctx, task, containerd.Running)

	// vminitd emits its phase timings during VM init, before the container
	// starts, so by the time the task is Running they are in the console log.
	consolePath := filepath.Join(logDirBase(), name, "console.log")
	phases := waitForVminitdProfile(t, consolePath, 30*time.Second)

	// total_us is cumulative; the largest is the whole measured window.
	total := 0
	for _, p := range phases {
		if p.totalUS > total {
			total = p.totalUS
		}
	}

	t.Logf("VMINITD_PROFILE phases=%d total_us=%d (%.1f ms)", len(phases), total, float64(total)/1000)

	// Report phases by cost (slowest first) so a regression stands out.
	byCost := make([]vminitdPhase, len(phases))
	copy(byCost, phases)
	sort.Slice(byCost, func(a, b int) bool { return byCost[a].deltaUS > byCost[b].deltaUS })
	for _, p := range byCost {
		t.Logf("VMINITD_PROFILE   %6d us  %s", p.deltaUS, p.name)
	}
}

// waitForVminitdProfile polls the console log until it contains VMINITD_PROFILE
// lines (vminitd may still be flushing) and returns them in emission order.
func waitForVminitdProfile(t *testing.T, consolePath string, timeout time.Duration) []vminitdPhase {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		phases := parseVminitdProfile(consolePath)
		if len(phases) > 0 {
			return phases
		}
		if time.Now().After(deadline) {
			data, _ := os.ReadFile(consolePath) //nolint:errcheck // best-effort diagnostics
			t.Fatalf("no VMINITD_PROFILE output in %s after %v (is the debug-boot annotation honored and spin.profile set?); console tail:\n%s",
				consolePath, timeout, lastLines(string(data), 20))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// parseVminitdProfile extracts vminitd phase timings from the console log.
func parseVminitdProfile(consolePath string) []vminitdPhase {
	data, err := os.ReadFile(consolePath)
	if err != nil {
		return nil
	}
	var phases []vminitdPhase
	for _, m := range vminitdProfileRE.FindAllStringSubmatch(string(data), -1) {
		delta, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		total, err := strconv.Atoi(m[3])
		if err != nil {
			continue
		}
		phases = append(phases, vminitdPhase{name: m[1], deltaUS: delta, totalUS: total})
	}
	return phases
}
