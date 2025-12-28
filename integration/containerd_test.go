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
	"github.com/containerd/containerd/v2/cmd/ctr/commands/tasks"
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

	// Create log file for capturing output
	logDir := t.TempDir()
	logFile := filepath.Join(logDir, "output.log")
	logURI := "file://" + logFile

	// Create task using the same helper function as the working CLI
	// This handles all the I/O setup complexity correctly
	ioOpts := []cio.Opt{cio.WithFIFODir(logDir)}

	// Task options - empty slice since we don't need any special options
	taskOpts := []containerd.NewTaskOpts{}

	task, err := tasks.NewTask(
		ctx,
		client,
		container,
		"",          // checkpoint
		nil,         // con (console)
		false,       // nullIO
		logURI,      // logURI
		ioOpts,      // ioOpts
		taskOpts..., // task options (required parameter)
	)
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
		// Try to read log file for error details
		logData, _ := os.ReadFile(logFile)
		t.Fatalf("task exited with code %d, output: %s", code, string(logData))
	}

	// Read and check output from log file
	output, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log file: %v", err)
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
