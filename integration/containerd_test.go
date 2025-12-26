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
			oci.WithProcessArgs("/bin/sh", "-c", "echo qemubox-ok"),
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
	if st, err := task.Status(ctx); err == nil {
		t.Logf("task status after start: %s (pid=%d)", st.Status.String(), st.Pid)
	} else {
		t.Logf("task status after start failed: %v", err)
	}

	select {
	case status := <-statusCh:
		code, _, err := status.Result()
		if err != nil {
			t.Fatalf("task result: %v", err)
		}
		if code != 0 {
			if st, err := task.Status(ctx); err == nil {
				t.Logf("task status on non-zero exit: %s (pid=%d)", st.Status.String(), st.Pid)
			} else {
				t.Logf("task status on non-zero exit failed: %v", err)
			}
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
