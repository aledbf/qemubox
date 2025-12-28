//go:build linux

package integration

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/cmd/ctr/commands"
	"github.com/containerd/containerd/v2/cmd/ctr/commands/run"
	"github.com/containerd/containerd/v2/cmd/ctr/commands/tasks"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/urfave/cli/v2"
)

func TestContainerdRunQemubox(t *testing.T) {
	socket := getenvDefault("QEMUBOX_CONTAINERD_SOCKET", "/var/run/qemubox/containerd.sock")
	imageRef := getenvDefault("QEMUBOX_IMAGE", "docker.io/aledbf/beacon-workspace:test")
	runtime := getenvDefault("QEMUBOX_RUNTIME", "io.containerd.qemubox.v1")
	snapshotter := getenvDefault("QEMUBOX_SNAPSHOTTER", "erofs")

	containerName := getenvDefault("QEMUBOX_TEST_ID", "")
	if containerName == "" {
		containerName = "qbx-ci-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	}
	t.Logf("container name: %s", containerName)

	// Create a CLI-like context and container using run.NewContainer.
	fifoDir := t.TempDir()
	cliCtx := newRunCLIContext(t, socket, namespaces.Default, snapshotter, runtime, fifoDir, imageRef, containerName, "/bin/echo", "OK_FROM_QEMUBOX")

	cliClient, cliCtxWithNS, cliCancel, err := commands.NewClient(cliCtx)
	if err != nil {
		t.Fatalf("create cli client: %v", err)
	}
	defer cliCancel()
	defer cliClient.Close()

	// Pull image so run.NewContainer can resolve it like the CLI.
	if _, err := cliClient.Pull(
		cliCtxWithNS,
		imageRef,
		containerd.WithPullSnapshotter(snapshotter),
		containerd.WithPullUnpack,
	); err != nil {
		t.Fatalf("pull image: %v", err)
	}

	container, err := run.NewContainer(cliCtxWithNS, cliClient, cliCtx)
	if err != nil {
		t.Fatalf("create container via run.NewContainer: %v", err)
	}

	// Cleanup container on exit
	defer func() {
		if err := container.Delete(cliCtxWithNS, containerd.WithSnapshotCleanup); err != nil {
			t.Logf("failed to cleanup container: %v", err)
		}
	}()

	// Match ctr run IO: use tasks.NewTask with fifo-dir and capture stdout/stderr by swapping os.Stdout/Err.
	stdoutFile := filepath.Join(fifoDir, "stdout.log")
	stderrFile := filepath.Join(fifoDir, "stderr.log")
	stdout, err := os.Create(stdoutFile)
	if err != nil {
		t.Fatalf("create stdout file: %v", err)
	}
	stderr, err := os.Create(stderrFile)
	if err != nil {
		stdout.Close()
		t.Fatalf("create stderr file: %v", err)
	}

	oldStdout := os.Stdout
	oldStderr := os.Stderr
	os.Stdout = stdout
	os.Stderr = stderr
	t.Cleanup(func() {
		os.Stdout = oldStdout
		os.Stderr = oldStderr
		_ = stdout.Close()
		_ = stderr.Close()
	})

	task, err := tasks.NewTask(
		cliCtxWithNS,
		cliClient,
		container,
		"",    // checkpoint
		nil,   // console
		false, // null-io
		"",    // log-uri
		[]cio.Opt{cio.WithFIFODir(fifoDir)},
		tasks.GetNewTaskOpts(cliCtx)...,
	)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Cleanup task on exit
	defer func() {
		if _, err := task.Delete(cliCtxWithNS, containerd.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
			// Ignore "ttrpc: closed" error on cleanup
			if !strings.Contains(err.Error(), "ttrpc: closed") {
				t.Logf("failed to cleanup task: %v", err)
			}
		}
	}()

	// Wait for task completion
	statusC, err := task.Wait(cliCtxWithNS)
	if err != nil {
		t.Fatalf("wait for task: %v", err)
	}

	// Start the task
	if err := task.Start(cliCtxWithNS); err != nil {
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

func newRunCLIContext(t *testing.T, socket, namespace, snapshotter, runtime, fifoDir, imageRef, containerName string, args ...string) *cli.Context {
	t.Helper()

	app := &cli.App{
		Name: "qemubox-ctr",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "address",
				Value: socket,
			},
			&cli.DurationFlag{
				Name: "timeout",
			},
			&cli.DurationFlag{
				Name: "connect-timeout",
			},
			&cli.StringFlag{
				Name:  "namespace",
				Value: namespace,
			},
		},
	}

	set := flag.NewFlagSet("qemubox-ctr", flag.ContinueOnError)
	for _, flg := range app.Flags {
		if err := flg.Apply(set); err != nil {
			t.Fatalf("apply app flag: %v", err)
		}
	}
	for _, flg := range run.Command.Flags {
		if err := flg.Apply(set); err != nil {
			t.Fatalf("apply run flag: %v", err)
		}
	}

	if err := set.Set("address", socket); err != nil {
		t.Fatalf("set address: %v", err)
	}
	if err := set.Set("namespace", namespace); err != nil {
		t.Fatalf("set namespace: %v", err)
	}
	if err := set.Set("snapshotter", snapshotter); err != nil {
		t.Fatalf("set snapshotter: %v", err)
	}
	if err := set.Set("runtime", runtime); err != nil {
		t.Fatalf("set runtime: %v", err)
	}
	if err := set.Set("fifo-dir", fifoDir); err != nil {
		t.Fatalf("set fifo-dir: %v", err)
	}

	argv := append([]string{imageRef, containerName}, args...)
	if err := set.Parse(argv); err != nil {
		t.Fatalf("parse args: %v", err)
	}

	return cli.NewContext(app, set, nil)
}

func getenvDefault(key, def string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return def
}
