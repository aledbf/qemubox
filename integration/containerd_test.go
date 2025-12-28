//go:build linux

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
)

func TestContainerdRunQemubox(t *testing.T) {
	socket := getenvDefault("QEMUBOX_CONTAINERD_SOCKET", "/var/run/qemubox/containerd.sock")
	imageRef := getenvDefault("QEMUBOX_IMAGE", "docker.io/aledbf/beacon-workspace:test")
	runtime := getenvDefault("QEMUBOX_RUNTIME", "io.containerd.qemubox.v1")
	snapshotter := getenvDefault("QEMUBOX_SNAPSHOTTER", "erofs")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	ctx = namespaces.WithNamespace(ctx, namespaces.Default)

	client, err := containerd.New(socket)
	if err != nil {
		t.Fatalf("connect containerd: %v", err)
	}
	defer client.Close()

	// Pull image
	image, err := client.Pull(
		ctx,
		imageRef,
		containerd.WithPullSnapshotter(snapshotter),
		containerd.WithPullUnpack,
	)
	if err != nil {
		t.Fatalf("pull image: %v", err)
	}

	containerName := getenvDefault("QEMUBOX_TEST_ID", "")
	if containerName == "" {
		containerName = "qbx-ci-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	}
	t.Logf("container name: %s", containerName)

	// Create container
	container, err := client.NewContainer(
		ctx,
		containerName,
		containerd.WithSnapshotter(snapshotter),
		containerd.WithNewSnapshot(containerName+"-snapshot", image),
		containerd.WithNewSpec(
			oci.WithDefaultSpec(),
			oci.WithDefaultUnixDevices,
			oci.WithImageConfig(image),
			oci.WithProcessArgs("/bin/echo", "OK_FROM_QEMUBOX"),
			oci.Compose(
				oci.WithAllCurrentCapabilities,
				oci.WithMaskedPaths(nil),
				oci.WithReadonlyPaths(nil),
				oci.WithWriteableSysfs,
				oci.WithWriteableCgroupfs,
				oci.WithSelinuxLabel(""),
				oci.WithApparmorProfile(""),
				oci.WithSeccompUnconfined,
			),
		),
		containerd.WithRuntime(runtime, nil),
	)
	if err != nil {
		t.Fatalf("create container: %v", err)
	}

	// Cleanup container on exit
	defer func() {
		if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
			t.Logf("failed to cleanup container: %v", err)
		}
	}()

	// Match ctr run's IO path: FIFO-backed streams with custom stdout/stderr sinks.
	fifoDir := t.TempDir()
	stdoutFile := filepath.Join(fifoDir, "stdout.log")
	stderrFile := filepath.Join(fifoDir, "stderr.log")

	stdout, err := os.Create(stdoutFile)
	if err != nil {
		t.Fatalf("create stdout file: %v", err)
	}
	defer stdout.Close()

	stderr, err := os.Create(stderrFile)
	if err != nil {
		t.Fatalf("create stderr file: %v", err)
	}
	defer stderr.Close()

	spec, err := container.Spec(ctx)
	if err != nil {
		t.Fatalf("get container spec: %v", err)
	}

	taskOpts := []containerd.NewTaskOpts{}
	if spec.Linux != nil {
		if len(spec.Linux.UIDMappings) != 0 {
			taskOpts = append(taskOpts, containerd.WithUIDOwner(spec.Linux.UIDMappings[0].HostID))
		}
		if len(spec.Linux.GIDMappings) != 0 {
			taskOpts = append(taskOpts, containerd.WithGIDOwner(spec.Linux.GIDMappings[0].HostID))
		}
	}

	ioCreator := cio.NewCreator(
		cio.WithStreams(nil, stdout, stderr),
		cio.WithFIFODir(fifoDir),
	)

	task, err := container.NewTask(ctx, ioCreator, taskOpts...)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Cleanup task on exit
	defer func() {
		if _, err := task.Delete(ctx, containerd.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
			// Ignore "ttrpc: closed" error on cleanup
			if !strings.Contains(err.Error(), "ttrpc: closed") {
				t.Logf("failed to cleanup task: %v", err)
			}
		}
	}()

	// Wait for task completion
	statusC, err := task.Wait(ctx)
	if err != nil {
		t.Fatalf("wait for task: %v", err)
	}

	// Start the task
	if err := task.Start(ctx); err != nil {
		t.Fatalf("start task: %v", err)
	}

	// Wait for completion
	status := <-statusC
	code, _, err := status.Result()
	if err != nil {
		t.Fatalf("task result: %v", err)
	}

	if code != 0 {
		// Try to read log files for error details
		stdoutData, _ := os.ReadFile(stdoutFile)
		stderrData, _ := os.ReadFile(stderrFile)
		t.Fatalf("task exited with code %d\nstdout: %s\nstderr: %s", code, string(stdoutData), string(stderrData))
	}

	// Read and check output from stdout file
	output, err := os.ReadFile(stdoutFile)
	if err != nil {
		t.Fatalf("read stdout file: %v", err)
	}

	if !strings.Contains(string(output), "OK_FROM_QEMUBOX") {
		t.Fatalf("missing echo output, got: %q", string(output))
	}

	t.Logf("output: %s", strings.TrimSpace(string(output)))
	t.Log("test completed successfully")
}

func getenvDefault(key, def string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return def
}
