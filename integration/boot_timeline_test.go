//go:build linux && integration

package integration

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
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

// bootTimelineRE matches the shim's BOOT_TIMELINE line, e.g.:
//
//	BOOT_TIMELINE qemu_launch_us=8123 qmp_socket_us=6945 qmp_handshake_us=13943 guest_boot_us=146210 total_us=154333
//
// It captures the whole run of key=value pairs rather than naming them in
// order. The previous regex spelled the three fields out positionally and broke
// the moment two were added between them - reporting "no BOOT_TIMELINE line in
// the journal" about a journal full of them.
var bootTimelineRE = regexp.MustCompile(`BOOT_TIMELINE ((?:[a-z_]+=\d+ ?)+)`)

type bootTimeline struct {
	qemuLaunchUS   int
	qmpSocketUS    int
	qmpHandshakeUS int
	guestBootUS    int
	totalUS        int
}

// TestBootTimeline boots one VM on the normal (non-debug) path and reports the
// shim's BOOT_TIMELINE breakdown from the journal: how cold-start splits between
// QEMU launch (process + firmware/machine init until QMP) and guest boot (kernel
// + vminitd until its vsock RPC accepts). This is the wall-clock companion to the
// kernel/userspace profiles, which only cover slices of the guest side.
func TestBootTimeline(t *testing.T) {
	cfg := loadTestConfig()

	client := setupContainerdClient(t, cfg)
	defer client.Close()

	ensureImagePulled(t, client, cfg)

	ctx := namespaces.WithNamespace(t.Context(), cfg.Namespace)

	image, err := client.GetImage(ctx, cfg.Image)
	if err != nil {
		t.Fatalf("get image %s: %v", cfg.Image, err)
	}

	name := "qbx-timeline-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")

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

	// The VM boots (and emits BOOT_TIMELINE) during NewTask, so capture the
	// journal window from just before it.
	since := time.Now().Add(-1 * time.Second)

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
	if err := task.Start(ctx); err != nil {
		t.Fatalf("start task %s: %v", name, err)
	}
	waitForOutput(t, stdoutPath, "BOOTED", 60*time.Second)

	tl := readBootTimeline(t, since)
	t.Logf("BOOT_TIMELINE %.1f ms total: %.1f ms qemu launch (%.1f ms to the monitor socket, %.1f ms to a QMP that answers) + %.1f ms guest boot",
		float64(tl.totalUS)/1000, float64(tl.qemuLaunchUS)/1000,
		float64(tl.qmpSocketUS)/1000, float64(tl.qmpHandshakeUS)/1000,
		float64(tl.guestBootUS)/1000)
}

// readBootTimeline reads the shim journal since the given time and returns the
// most recent BOOT_TIMELINE entry (the boot this test just triggered).
func readBootTimeline(t *testing.T, since time.Time) bootTimeline {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if tl, ok := parseLastBootTimeline(journalSince(t, since)); ok {
			return tl
		}
		if time.Now().After(deadline) {
			t.Fatalf("no BOOT_TIMELINE line in spinbox journal since %s", since.Format(time.RFC3339))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// journalSince returns the spinbox unit's journal output since the given time.
func journalSince(t *testing.T, since time.Time) string {
	t.Helper()
	cmd := exec.Command("journalctl",
		"-u", "spinbox",
		"--since", since.Format("2006-01-02 15:04:05"),
		"--no-pager",
		"-o", "cat",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Logf("journalctl failed: %v (stderr: %s)", err, stderr.String())
		return ""
	}
	return stdout.String()
}

// parseLastBootTimeline returns the last BOOT_TIMELINE entry in the text.
func parseLastBootTimeline(text string) (bootTimeline, bool) {
	matches := bootTimelineRE.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return bootTimeline{}, false
	}

	fields := map[string]int{}
	for _, pair := range strings.Fields(matches[len(matches)-1][1]) {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		fields[k] = mustAtoi(v)
	}

	// Only the outer two are required. The breakdown between them is there to be
	// read, and a shim that stops emitting part of it should not fail this test.
	if _, ok := fields["total_us"]; !ok {
		return bootTimeline{}, false
	}
	return bootTimeline{
		qemuLaunchUS:   fields["qemu_launch_us"],
		qmpSocketUS:    fields["qmp_socket_us"],
		qmpHandshakeUS: fields["qmp_handshake_us"],
		guestBootUS:    fields["guest_boot_us"],
		totalUS:        fields["total_us"],
	}, true
}

func mustAtoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic(fmt.Sprintf("parse int %q: %v", s, err))
	}
	return n
}
