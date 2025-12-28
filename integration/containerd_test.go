//go:build linux

package integration

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	ctrapp "github.com/containerd/containerd/v2/cmd/ctr/app"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/urfave/cli/v2"
)

func TestContainerdRunQemubox(t *testing.T) {
	socket := getenvDefault("QEMUBOX_CONTAINERD_SOCKET", "/var/run/qemubox/containerd.sock")
	imageRef := getenvDefault("QEMUBOX_IMAGE", "docker.io/aledbf/beacon-workspace:test")
	runtime := getenvDefault("QEMUBOX_RUNTIME", "io.containerd.qemubox.v1")
	snapshotter := getenvDefault("QEMUBOX_SNAPSHOTTER", "erofs")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client, err := containerd.New(socket)
	if err != nil {
		t.Fatalf("connect containerd: %v", err)
	}
	defer client.Close()

	pullCtx := namespaces.WithNamespace(ctx, namespaces.Default)
	_, err = client.Pull(
		pullCtx,
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

	args := []string{
		"ctr",
		"--address", socket,
		"--debug",
		"run",
		"--snapshotter", snapshotter,
		"--runtime", runtime,
		"--privileged",
		"--rm",
	}

	// if setupCTRPTY(t) {
	// 	args = append(args, "--tty")
	// }

	args = append(args, imageRef, containerName, "/bin/echo", "OK_FROM_QEMUBOX")

	ctr := ctrapp.New()
	ctr.ExitErrHandler = func(*cli.Context, error) {}
	stdoutBuf, restoreStdout := captureStdout(t)
	if err := ctr.RunContext(ctx, args); err != nil {
		if !strings.Contains(err.Error(), "ttrpc: closed") {
			t.Fatalf("ctr run: %v", err)
		}
		t.Logf("ctr run: ignoring cleanup error: %v", err)
	}
	restoreStdout()
	if !strings.Contains(stdoutBuf.String(), "OK_FROM_QEMUBOX") {
		t.Fatalf("missing echo output, got: %q", stdoutBuf.String())
	}

	t.Log("ctr run completed")
}

func getenvDefault(key, def string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return def
}

func captureStdout(t *testing.T) (*strings.Builder, func()) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	origStdout := os.Stdout
	origStderr := os.Stderr
	os.Stdout = w
	os.Stderr = w

	var buf strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(&buf, r)
	}()

	return &buf, func() {
		_ = w.Close()
		os.Stdout = origStdout
		os.Stderr = origStderr
		_ = r.Close()
		<-done
	}
}

/*
func setupCTRPTY(t *testing.T) bool {
	if os.Getenv("QEMUBOX_CTR_TTY") == "0" {
		return false
	}
	if !isTerminal(os.Stdin) {
		master, slavePath, err := console.NewPty()
		if err != nil {
			t.Logf("failed to allocate PTY, running without --tty: %v", err)
			return false
		}
		ttyFile, err := os.OpenFile(slavePath, os.O_RDWR, 0)
		if err != nil {
			_ = master.Close()
			t.Logf("failed to open PTY slave, running without --tty: %v", err)
			return false
		}
		t.Cleanup(func() {
			_ = master.Close()
			_ = ttyFile.Close()
		})

		restoreStdin := os.Stdin
		restoreStdout := os.Stdout
		restoreStderr := os.Stderr
		os.Stdin = ttyFile
		os.Stdout = ttyFile
		os.Stderr = ttyFile
		t.Cleanup(func() {
			os.Stdin = restoreStdin
			os.Stdout = restoreStdout
			os.Stderr = restoreStderr
		})

		go func() {
			scanner := bufio.NewScanner(master)
			for scanner.Scan() {
				t.Logf("ctr: %s", scanner.Text())
			}
			if err := scanner.Err(); err != nil {
				t.Logf("ctr: pty read error: %v", err)
			}
		}()
	}
	return true
}

func isTerminal(f *os.File) bool {
	_, err := console.ConsoleFromFile(f)
	return err == nil
}
*/
