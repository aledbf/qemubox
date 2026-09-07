//go:build linux && integration

package integration

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
)

// vminitdReadyRE matches a VMINITD_READY line, e.g.:
//
//	VMINITD_READY uptime_us=118233 phase=pid1-entry
var vminitdReadyRE = regexp.MustCompile(`VMINITD_READY uptime_us=(\d+) phase=(\S+)`)

// readyPhaseOrder is the expected emission order; used to print a decomposition.
var readyPhaseOrder = []string{"pid1-entry", "system-init", "vsock-listen", "serve", "first-accept"}

// TestVminitdReady boots a VM on the normal path and reports the VMINITD_READY
// CLOCK_BOOTTIME milestones, which decompose the guest-boot half of cold-start:
//
//	pid1-entry                 -> kernel boot (kernel start -> init exec)
//	system-init  - pid1-entry  -> vminitd system.Initialize
//	vsock-listen - system-init -> listener bind
//	serve        - vsock-listen-> plugin loading
//	first-accept - serve       -> host-readiness lag (guest ready -> host connects)
//
// This is the clean split BOOT_TIMELINE's guest_boot (tVsock - tQMP) could not
// give, since that lumps kernel + vminitd + handshake together.
func TestVminitdReady(t *testing.T) {
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

	name := "qbx-ready-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")

	container, err := client.NewContainer(ctx, name,
		containerd.WithSnapshotter(cfg.Snapshotter),
		containerd.WithImage(image),
		containerd.WithNewSnapshot(name+"-snapshot", image),
		containerd.WithRuntime(cfg.Runtime, nil),
		containerd.WithNewSpec(
			oci.WithImageConfig(image),
			oci.WithProcessArgs("/bin/echo", "BOOTED"),
			// This test reads the phases of a boot, so it has to ask for one.
			// A VM restored from a template - the normal path - executes no
			// initcalls and runs no vminitd startup, and emits none of these
			// lines, which is the point of restoring.
			oci.WithAnnotations(map[string]string{annotationDebugBoot: "true"}),
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

	consolePath := filepath.Join(logDirBase(), name, "console.log")
	phases := waitForVminitdReady(t, consolePath, 30*time.Second)

	for _, p := range readyPhaseOrder {
		if us, ok := phases[p]; ok {
			t.Logf("VMINITD_READY %-12s uptime=%6d us (%.1f ms)", p, us, float64(us)/1000)
		}
	}
	// Deltas attribute each segment.
	prev := "pid1-entry"
	for _, p := range readyPhaseOrder[1:] {
		cur, ok := phases[p]
		pv, okPrev := phases[prev]
		if ok && okPrev {
			t.Logf("VMINITD_READY delta %-24s = %6d us (%.1f ms)", prev+"->"+p, cur-pv, float64(cur-pv)/1000)
			prev = p
		}
	}
}

// waitForVminitdReady polls the console log until the pid1-entry stamp appears
// and returns phase->uptime_us.
func waitForVminitdReady(t *testing.T, consolePath string, timeout time.Duration) map[string]int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		phases := parseVminitdReady(consolePath)
		if _, ok := phases["pid1-entry"]; ok {
			return phases
		}
		if time.Now().After(deadline) {
			data, _ := os.ReadFile(consolePath) //nolint:errcheck // best-effort diagnostics
			t.Fatalf("no VMINITD_READY output in %s after %v; console tail:\n%s",
				consolePath, timeout, lastLines(string(data), 20))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// parseVminitdReady returns the first uptime seen per phase.
func parseVminitdReady(consolePath string) map[string]int {
	data, err := os.ReadFile(consolePath)
	if err != nil {
		return nil
	}
	phases := make(map[string]int)
	for _, m := range vminitdReadyRE.FindAllStringSubmatch(string(data), -1) {
		us, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if _, seen := phases[m[2]]; !seen {
			phases[m[2]] = us
		}
	}
	return phases
}
