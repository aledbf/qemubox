//go:build linux

package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
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

	containerName := getenvDefault("QEMUBOX_TEST_ID", "")
	if containerName == "" {
		containerName = "qbx-ci-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	}
	t.Logf("container name: %s", containerName)

	container, err := client.NewContainer(
		ctx,
		containerName,
		containerd.WithImage(img),
		containerd.WithSnapshotter(snapshotter),
		containerd.WithNewSnapshot(containerName+"-snapshot", img),
		containerd.WithNewSpec(
			oci.WithImageConfig(img),
			oci.WithProcessArgs("/sbin/init"),
			oci.WithPrivileged,
			oci.WithAllDevicesAllowed,
		oci.WithHostDevices,
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

	// Use null IO to avoid stream setup that can close the VM before Start.
	task, err := container.NewTask(ctx, cio.NullIO)
	if err != nil {
		if existing, loadErr := container.Task(ctx, nil); loadErr == nil {
			_ = existing.Kill(ctx, syscall.SIGKILL)
			_, _ = existing.Delete(ctx)
		}
		t.Fatalf("create task: %v", err)
	}
	defer func() {
		_ = task.Kill(ctx, syscall.SIGKILL)
		_, _ = task.Delete(ctx)
	}()

	// Small stabilization delay for your vsock/CID issue.
	time.Sleep(100 * time.Millisecond)

	if err := task.Start(ctx); err != nil {
		t.Fatalf("start task: %v", err)
	}

	statusCh, err := task.Wait(ctx)
	if err != nil {
		t.Fatalf("wait task: %v", err)
	}

	// Ensure it didn't immediately die
	select {
	case status := <-statusCh:
		code, _, err := status.Result()
		if err != nil {
			t.Fatalf("task exited early: %v", err)
		}
		t.Fatalf("task exited early with code %d", code)
	default:
	}

	// Wait for init/boot/userspace to be ready enough to exec.
	// This also validates that exec path is functional (the thing you called out).
	waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer waitCancel()

	if err := waitForExecReady(waitCtx, task); err != nil {
		t.Fatalf("exec never became ready: %v", err)
	}

	// Now run an assertion command and verify output.
	stdout, stderr, exitCode, err := execInTask(ctx, task,
		[]string{"/bin/sh", "-lc", "echo __QEMUBOX_OK__ && id -u && uname -a >/dev/null"},
		30*time.Second,
	)
	if err != nil {
		t.Fatalf("exec failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if exitCode != 0 {
		t.Fatalf("exec exit code = %d (want 0)\nstdout:\n%s\nstderr:\n%s", exitCode, stdout, stderr)
	}
	if !strings.Contains(stdout, "__QEMUBOX_OK__") {
		t.Fatalf("missing marker in stdout\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	// Controlled shutdown (still using SIGKILL here, but now we validated exec first)
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

func waitForExecReady(ctx context.Context, task containerd.Task) error {
	// Retry a cheap exec until it works.
	// This covers boot time, init bringing up namespaces, etc.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("%w; last exec error: %v", ctx.Err(), lastErr)
			}
			return ctx.Err()
		case <-ticker.C:
			_, _, code, err := execInTask(ctx, task, []string{"/bin/sh", "-lc", "echo ready"}, 10*time.Second)
			if err == nil && code == 0 {
				return nil
			}
			if err != nil {
				lastErr = err
			} else {
				lastErr = fmt.Errorf("non-zero exit code: %d", code)
			}
		}
	}
}

func execInTask(ctx context.Context, task containerd.Task, argv []string, timeout time.Duration) (stdout string, stderr string, exitCode uint32, _ error) {
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Unique exec ID
	execID := "exec-" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")

	var outBuf, errBuf bytes.Buffer
	creator := cio.NewCreator(cio.WithStreams(nil, &outBuf, &errBuf))

	proc, err := task.Exec(execCtx, execID, &specs.Process{
		Args: argv,
	}, creator)
	if err != nil {
		return "", "", 0, fmt.Errorf("task exec create: %w", err)
	}
	defer func() {
		// Ensure the exec process is cleaned up. Ignore errors: it may already be gone.
		// Use WithoutCancel to allow cleanup even if parent context is cancelled.
		_, _ = proc.Delete(context.WithoutCancel(execCtx))
	}()

	waitCh, err := proc.Wait(execCtx)
	if err != nil {
		return "", "", 0, fmt.Errorf("task exec wait: %w", err)
	}

	if err := proc.Start(execCtx); err != nil {
		return outBuf.String(), errBuf.String(), 0, fmt.Errorf("task exec start: %w", err)
	}

	select {
	case st := <-waitCh:
		code, _, err := st.Result()
		if err != nil {
			return outBuf.String(), errBuf.String(), 0, fmt.Errorf("task exec result: %w", err)
		}
		return outBuf.String(), errBuf.String(), code, nil
	case <-execCtx.Done():
		_ = proc.Kill(context.WithoutCancel(execCtx), syscall.SIGKILL)
		return outBuf.String(), errBuf.String(), 0, fmt.Errorf("task exec timeout: %w", execCtx.Err())
	}
}

func getenvDefault(key, def string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return def
}
