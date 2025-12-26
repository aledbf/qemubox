//go:build linux

package integration

import (
	"context"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
)

func TestContainerdRunQemubox(t *testing.T) {
	socket := getenvDefault("QEMUBOX_CONTAINERD_SOCKET", "/var/run/qemubox/containerd.sock")
	imageRef := getenvDefault("QEMUBOX_IMAGE", "docker.io/aledbf/beacon-workspace:test")
	runtime := getenvDefault("QEMUBOX_RUNTIME", "io.containerd.qemubox.v1")
	snapshotter := getenvDefault("QEMUBOX_SNAPSHOTTER", "erofs")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	ctx = namespaces.WithNamespace(ctx, "qemubox-ci")

	client, err := containerd.New(socket)
	if err != nil {
		t.Fatalf("connect containerd: %v", err)
	}
	defer client.Close()

	img, err := client.Pull(
		ctx,
		imageRef,
		containerd.WithPullSnapshotter(snapshotter),
		containerd.WithPullUnpack,
	)
	if err != nil {
		t.Fatalf("pull image: %v", err)
	}

	containerName := "qbx-ci-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	container, err := client.NewContainer(
		ctx,
		containerName,
		containerd.WithImage(img),
		containerd.WithSnapshotter(snapshotter),
		containerd.WithNewSnapshot(containerName+"-snapshot", img),
		containerd.WithNewSpec(
			oci.WithImageConfig(img),
			// Keep the process running until the test explicitly stops it.
			oci.WithProcessArgs("/bin/sh", "-c", "while true; do sleep 60; done"),
		),
		containerd.WithRuntime(runtime, nil),
	)
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	defer func() {
		if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
			t.Logf("cleanup container: %v", err)
		}
	}()

	task, err := container.NewTask(ctx, cio.NullIO)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	defer func() {
		_ = task.Kill(ctx, syscall.SIGKILL)
		_, _ = task.Delete(ctx)
	}()

	statusCh, err := task.Wait(ctx)
	if err != nil {
		t.Fatalf("wait task: %v", err)
	}
	if err := task.Start(ctx); err != nil {
		t.Fatalf("start task: %v", err)
	}

	select {
	case status := <-statusCh:
		code, _, err := status.Result()
		if err != nil {
			t.Fatalf("task exited early: %v", err)
		}
		t.Fatalf("task exited early with code %d", code)
	case <-time.After(2 * time.Second):
		// Task stayed running; proceed with controlled shutdown.
	}

	if err := task.Kill(ctx, syscall.SIGKILL); err != nil {
		t.Fatalf("kill task: %v", err)
	}

	select {
	case status := <-statusCh:
		code, _, err := status.Result()
		if err != nil {
			t.Fatalf("task result: %v", err)
		}
		if code != 0 {
			t.Fatalf("unexpected exit code: %d", code)
		}
	case <-ctx.Done():
		t.Fatalf("task timeout: %v", ctx.Err())
	}
}

func getenvDefault(key, def string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return def
}
