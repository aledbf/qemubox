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

// kmsgSummaryRE matches the KMSG_PROFILE header line vminitd emits, e.g.:
//
//	KMSG_PROFILE initcalls=678 sum_us=83210 last_ts_us=152331
var kmsgSummaryRE = regexp.MustCompile(`KMSG_PROFILE initcalls=(\d+) sum_us=(\d+) last_ts_us=(\d+)`)

// kmsgEntryRE matches a per-initcall KMSG_PROFILE line, e.g.:
//
//	KMSG_PROFILE    39000 us  acpi_init+0x0/0x460
var kmsgEntryRE = regexp.MustCompile(`KMSG_PROFILE\s+(\d+) us\s+(\S+)`)

// TestKernelBootProfileComplete boots a VM with the debug-boot annotation and
// reads the COMPLETE kernel initcall profile that vminitd dumps from /dev/kmsg.
// Unlike TestKernelBootProfile (which parses the hvc0 console and so only sees
// the late initcalls), this includes the early core/subsys initcalls - the
// suspected ACPI/PCI cost in the guest-boot half of cold-start. last_ts_us is
// the kernel-side boot duration; sum_us is the initcall total, and the gap
// between them is the non-initcall kernel work.
func TestKernelBootProfileComplete(t *testing.T) {
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

	name := "qbx-kmsg-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")

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

	consolePath := filepath.Join(logDirBase(), name, "console.log")
	header, entries := waitForKmsgProfile(t, consolePath, 30*time.Second)

	t.Logf("KMSG_PROFILE initcalls=%d sum_us=%d (%.1f ms) last_ts_us=%d (%.1f ms kernel-to-init)",
		header.initcalls, header.sumUS, float64(header.sumUS)/1000,
		header.lastTSUS, float64(header.lastTSUS)/1000)
	gap := header.lastTSUS - header.sumUS
	t.Logf("KMSG_PROFILE non-initcall kernel work ~= %d us (%.1f ms)", gap, float64(gap)/1000)
	for _, e := range entries {
		t.Logf("KMSG_PROFILE   %6d us  %s", e.usec, e.name)
	}

	// The initcalls are the half of boot that names itself; sum_us above is
	// smaller than last_ts_us, and the difference is what these two report. The
	// gaps are stretches in which the kernel said nothing and no initcall was
	// running; the levels are how long each initcall level took end to end, from
	// the tracepoints. Logged verbatim: they are what the run is for, and a
	// regexp here would only be a second place to keep their format.
	for _, line := range strings.Split(readFileOrEmpty(consolePath), "\n") {
		if strings.Contains(line, "KMSG_GAP") || strings.Contains(line, "KMSG_LEVEL") {
			t.Log(strings.TrimSpace(line[strings.Index(line, "KMSG_"):]))
		}
	}
}

// readFileOrEmpty reads path, returning "" on any error: the caller is logging
// extra detail about a run that has already succeeded, and a missing file there
// is not a test failure.
func readFileOrEmpty(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

type kmsgHeader struct {
	initcalls int
	sumUS     int
	lastTSUS  int
}

type kmsgEntry struct {
	usec int
	name string
}

// waitForKmsgProfile polls the console log until vminitd's KMSG_PROFILE summary
// appears and returns the header plus the per-initcall entries.
func waitForKmsgProfile(t *testing.T, consolePath string, timeout time.Duration) (kmsgHeader, []kmsgEntry) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		data, _ := os.ReadFile(consolePath) //nolint:errcheck // best-effort poll
		text := string(data)
		if m := kmsgSummaryRE.FindStringSubmatch(text); m != nil {
			h := kmsgHeader{
				initcalls: atoiOr0(m[1]),
				sumUS:     atoiOr0(m[2]),
				lastTSUS:  atoiOr0(m[3]),
			}
			var entries []kmsgEntry
			for _, e := range kmsgEntryRE.FindAllStringSubmatch(text, -1) {
				usec := atoiOr0(e[1])
				// The summary line also starts with KMSG_PROFILE but has no
				// "<n> us" entry shape, so the entry regex naturally skips it.
				entries = append(entries, kmsgEntry{usec: usec, name: e[2]})
			}
			return h, entries
		}
		if time.Now().After(deadline) {
			t.Fatalf("no KMSG_PROFILE output in %s after %v (debug-boot honored? spin.profile set? initcall_debug on?); console tail:\n%s",
				consolePath, timeout, lastLines(text, 20))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func atoiOr0(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
