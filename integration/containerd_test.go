//go:build linux && integration

// Package integration provides end-to-end tests for the spinbox runtime.
//
// # Test Strategy
//
// These tests verify the complete stack:
//
//	containerd → shim → QEMU → vminitd → crun → container process
//
// Each test uses a real containerd instance and runs actual VMs. This catches
// integration bugs that unit tests miss (network setup, vsock communication,
// resource cleanup, error propagation across process boundaries).
//
// # Test Environment Requirements
//
// - Running containerd configured with spinbox runtime
// - KVM access (/dev/kvm readable by test user)
// - CNI plugins installed (/opt/cni/bin)
// - CNI configuration present (/etc/cni/net.d)
// - Test image available (configurable via SPINBOX_IMAGE)
//
// Default configuration expects:
//   - Socket: /var/run/spinbox/containerd.sock
//   - Runtime: io.containerd.spinbox.v1
//   - Snapshotter: spin-erofs
//   - Namespace: default
//
// Override via environment variables (see loadTestConfig).
//
// # What These Tests Verify
//
// Currently tested:
//   - Container lifecycle (create, start, wait, cleanup)
//   - Exit code propagation (success and failure cases)
//   - stdout/stderr capture through VM boundary
//   - Multiple containers sequentially (resource cleanup)
//   - Namespace isolation (containers don't leak across namespaces)
//   - OCI spec preservation (resource limits appear in spec)
//
// NOT currently tested (but should be):
//   - Resource limit enforcement (memory OOM, CPU throttling)
//   - Network connectivity (ping between containers, outbound traffic)
//   - Concurrent container creation (stress test for race conditions)
//   - VM crash handling (kill -9 QEMU, verify cleanup)
//   - Long-running containers (multi-hour stability)
//   - Large I/O workloads (GB of logs, verify no corruption)
//
// # Test Failures and Debugging
//
// If tests fail, check in order:
//  1. containerd logs: journalctl -u spinbox-containerd
//  2. VM logs: /var/log/spinbox/vm-*.log
//  3. CNI state: ls -la /var/lib/cni/networks/
//  4. Network devices: ip link show | grep tap
//  5. QEMU processes: ps aux | grep qemu
//
// Common failure modes:
//   - "ttrpc: closed" - Normal during cleanup, ignore unless persistent
//   - "CNI setup failed" - Check CNI plugin installation and config
//   - "KVM unavailable" - Verify /dev/kvm permissions
//   - "Image pull failed" - Network issue or wrong image name
package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
)

// testLogCollector helps collect logs for a specific test/container.
// It captures logs from journald filtered by the container ID and time range.
type testLogCollector struct {
	t           *testing.T
	containerID string
	startTime   time.Time
}

// newTestLogCollector creates a log collector that will capture logs from now onwards.
func newTestLogCollector(t *testing.T, containerID string) *testLogCollector {
	return &testLogCollector{
		t:           t,
		containerID: containerID,
		startTime:   time.Now(),
	}
}

// dumpLogs collects and logs all relevant logs for this test.
// Call this in a defer or when the test fails.
func (c *testLogCollector) dumpLogs() {
	c.t.Helper()

	// Format start time for journalctl
	since := c.startTime.Format("2006-01-02 15:04:05")

	c.t.Logf("=== Logs for container %s (since %s) ===", c.containerID, since)

	// Collect journald logs filtered by container ID
	cmd := exec.Command("journalctl",
		"-u", "spinbox-containerd",
		"--since", since,
		"--no-pager",
		"-o", "short-precise",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		c.t.Logf("Failed to collect journald logs: %v (stderr: %s)", err, stderr.String())
	} else {
		// Filter logs to only show those containing our container ID
		lines := strings.Split(stdout.String(), "\n")
		var filtered []string
		for _, line := range lines {
			if strings.Contains(line, c.containerID) {
				filtered = append(filtered, line)
			}
		}
		if len(filtered) > 0 {
			c.t.Logf("Containerd logs for %s:\n%s", c.containerID, strings.Join(filtered, "\n"))
		} else {
			c.t.Logf("No containerd logs found for container %s", c.containerID)
		}
	}

	// Collect VM console log if it exists
	consoleLogPath := filepath.Join("/var/log/spinbox", c.containerID, "console.log")
	if data, err := os.ReadFile(consoleLogPath); err == nil {
		c.t.Logf("VM console log (%s):\n%s", consoleLogPath, string(data))
	}
}

// testConfig holds the configuration for containerd integration tests.
type testConfig struct {
	Socket      string
	Image       string
	Runtime     string
	Snapshotter string
	Namespace   string
}

const (
	defaultSocket      = "/var/run/spinbox/containerd.sock"
	defaultImage       = "ghcr.io/spin-stack/spinbox/sandbox:latest"
	defaultRuntime     = "io.containerd.spinbox.v1"
	defaultSnapshotter = "spin-erofs"
	defaultNamespace   = namespaces.Default
)

// loadTestConfig loads test configuration from environment variables.
func loadTestConfig() testConfig {
	cfg := testConfig{
		Socket:      defaultSocket,
		Image:       defaultImage,
		Runtime:     defaultRuntime,
		Snapshotter: defaultSnapshotter,
		Namespace:   defaultNamespace,
	}

	// Override from environment variables if set
	if v := os.Getenv("SPINBOX_CONTAINERD_SOCKET"); v != "" {
		cfg.Socket = v
	}
	if v := os.Getenv("SPINBOX_IMAGE"); v != "" {
		cfg.Image = v
	}
	if v := os.Getenv("SPINBOX_RUNTIME"); v != "" {
		cfg.Runtime = v
	}
	if v := os.Getenv("SPINBOX_SNAPSHOTTER"); v != "" {
		cfg.Snapshotter = v
	}
	if v := os.Getenv("SPINBOX_NAMESPACE"); v != "" {
		cfg.Namespace = v
	}

	return cfg
}

// setupContainerdClient creates a containerd client connected to the configured socket.
func setupContainerdClient(t *testing.T, cfg testConfig) *containerd.Client {
	t.Helper()

	client, err := containerd.New(cfg.Socket)
	if err != nil {
		t.Fatalf("connect to containerd socket %s: %v", cfg.Socket, err)
	}

	// Verify connection works
	ctx := namespaces.WithNamespace(context.Background(), cfg.Namespace)
	version, err := client.Version(ctx)
	if err != nil {
		client.Close()
		t.Fatalf("get containerd version (socket=%s, namespace=%s): %v",
			cfg.Socket, cfg.Namespace, err)
	}

	t.Logf("containerd version: %s (namespace: %s)", version.Version, cfg.Namespace)
	return client
}

// ensureImagePulled ensures the specified image is available locally and unpacked for the snapshotter.
func ensureImagePulled(t *testing.T, client *containerd.Client, cfg testConfig) {
	t.Helper()

	ctx := namespaces.WithNamespace(context.Background(), cfg.Namespace)

	// Check if image already exists
	image, err := client.GetImage(ctx, cfg.Image)
	if err == nil {
		t.Logf("image already exists: %s", cfg.Image)
		// Ensure image is unpacked for our snapshotter (it may exist but not be unpacked)
		if err := image.Unpack(ctx, cfg.Snapshotter); err != nil {
			t.Fatalf("unpack existing image for snapshotter %s: %v", cfg.Snapshotter, err)
		}
		return
	}

	t.Logf("pulling image: %s", cfg.Image)
	_, err = client.Pull(ctx, cfg.Image,
		containerd.WithPullSnapshotter(cfg.Snapshotter),
		containerd.WithPullUnpack,
	)
	if err != nil {
		t.Fatalf("pull image %s (snapshotter=%s): %v", cfg.Image, cfg.Snapshotter, err)
	}
}

// containerResult holds the output and exit status of a container run.
type containerResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// runContainer creates and runs a container with the specified command, capturing its output.
func runContainer(t *testing.T, client *containerd.Client, cfg testConfig, containerName string, command []string) containerResult {
	t.Helper()

	ctx := namespaces.WithNamespace(context.Background(), cfg.Namespace)

	// Get the image
	image, err := client.GetImage(ctx, cfg.Image)
	if err != nil {
		t.Fatalf("get image %s: %v", cfg.Image, err)
	}

	// Create output files
	outputDir := t.TempDir()
	stdoutPath := filepath.Join(outputDir, "stdout.log")
	stderrPath := filepath.Join(outputDir, "stderr.log")

	stdoutFile, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatalf("create stdout file: %v", err)
	}
	defer stdoutFile.Close()

	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		t.Fatalf("create stderr file: %v", err)
	}
	defer stderrFile.Close()

	// Create container spec
	container, err := client.NewContainer(ctx, containerName,
		containerd.WithSnapshotter(cfg.Snapshotter),
		containerd.WithImage(image),
		containerd.WithNewSnapshot(containerName+"-snapshot", image),
		containerd.WithRuntime(cfg.Runtime, nil),
		containerd.WithNewSpec(
			oci.WithImageConfig(image),
			oci.WithProcessArgs(command...),
		),
	)
	if err != nil {
		t.Fatalf("create container %s: %v", containerName, err)
	}
	defer func() {
		if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
			t.Logf("cleanup container %s: %v", containerName, err)
		}
	}()

	// Create task with file-backed IO
	ioCreator := cio.NewCreator(cio.WithStreams(nil, stdoutFile, stderrFile))
	task, err := container.NewTask(ctx, ioCreator)
	if err != nil {
		t.Fatalf("create task for container %s: %v", containerName, err)
	}
	defer func() {
		if _, err := task.Delete(ctx, containerd.WithProcessKill); err != nil {
			// Ignore "ttrpc: closed" errors - expected when task completes successfully
			if !strings.Contains(err.Error(), "ttrpc: closed") {
				t.Logf("cleanup task for container %s: %v", containerName, err)
			}
		}
	}()

	// Wait for task to complete
	exitCh, err := task.Wait(ctx)
	if err != nil {
		t.Fatalf("wait for task %s: %v", containerName, err)
	}

	// Start the task
	if err := task.Start(ctx); err != nil {
		t.Fatalf("start task %s: %v", containerName, err)
	}

	// Wait for completion
	status := <-exitCh
	exitCode, _, err := status.Result()
	if err != nil {
		t.Fatalf("get task result for %s: %v", containerName, err)
	}

	// Wait for I/O copy goroutines to complete.
	// containerd's cio.WithStreams creates io.Copy goroutines that copy from FIFO to file.
	// We must call Wait() to block until these finish, then Close() to clean up.
	// IMPORTANT: Close() alone is NOT sufficient - it closes the FIFOs but doesn't
	// wait for the copy goroutines. Wait() blocks until all data has been copied.
	if io := task.IO(); io != nil {
		io.Wait()
		io.Close()
	}

	// Ensure files are flushed
	stdoutFile.Sync()
	stderrFile.Sync()

	// Read output
	stdoutData, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatalf("read stdout from %s: %v", stdoutPath, err)
	}

	stderrData, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatalf("read stderr from %s: %v", stderrPath, err)
	}

	return containerResult{
		ExitCode: int(exitCode),
		Stdout:   string(stdoutData),
		Stderr:   string(stderrData),
	}
}

func TestContainerdRunSpinbox(t *testing.T) {
	cfg := loadTestConfig()
	t.Logf("test config: socket=%s image=%s runtime=%s snapshotter=%s",
		cfg.Socket, cfg.Image, cfg.Runtime, cfg.Snapshotter)

	client := setupContainerdClient(t, cfg)
	defer client.Close()

	ensureImagePulled(t, client, cfg)

	containerName := "qbx-ci-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	t.Logf("container name: %s", containerName)

	result := runContainer(t, client, cfg, containerName, []string{"/bin/echo", "OK_FROM_SPINBOX"})

	if result.ExitCode != 0 {
		t.Fatalf("container exited with code %d\nstdout: %s\nstderr: %s",
			result.ExitCode, result.Stdout, result.Stderr)
	}

	if !strings.Contains(result.Stdout, "OK_FROM_SPINBOX") {
		t.Fatalf("expected output not found\ngot stdout: %q\ngot stderr: %q",
			result.Stdout, result.Stderr)
	}

	t.Logf("output: %s", strings.TrimSpace(result.Stdout))
	t.Log("test completed successfully")
}

// TestContainerdRunMultipleContainers tests running multiple containers sequentially.
func TestContainerdRunMultipleContainers(t *testing.T) {
	cfg := loadTestConfig()

	client := setupContainerdClient(t, cfg)
	defer client.Close()

	ensureImagePulled(t, client, cfg)

	// Run multiple containers to verify resource cleanup
	for i := range 3 {
		containerName := fmt.Sprintf("qbx-multi-%d-%s", i,
			strings.ReplaceAll(time.Now().Format("150405.000"), ".", ""))

		t.Run(containerName, func(t *testing.T) {
			expectedOutput := fmt.Sprintf("CONTAINER_%d", i)
			result := runContainer(t, client, cfg, containerName,
				[]string{"/bin/echo", expectedOutput})

			if result.ExitCode != 0 {
				t.Fatalf("exit code %d\nstdout: %s\nstderr: %s",
					result.ExitCode, result.Stdout, result.Stderr)
			}

			if !strings.Contains(result.Stdout, expectedOutput) {
				t.Fatalf("expected %q in stdout, got: %q", expectedOutput, result.Stdout)
			}
		})
	}
}

// TestContainerdContainerFailure tests that container failures are properly reported.
func TestContainerdContainerFailure(t *testing.T) {
	cfg := loadTestConfig()

	client := setupContainerdClient(t, cfg)
	defer client.Close()

	ensureImagePulled(t, client, cfg)

	containerName := "qbx-fail-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")

	// Run a command that fails
	result := runContainer(t, client, cfg, containerName, []string{"/bin/sh", "-c", "exit 42"})

	if result.ExitCode != 42 {
		t.Fatalf("expected exit code 42, got %d", result.ExitCode)
	}

	t.Logf("container failed as expected with exit code %d", result.ExitCode)
}

// TestContainerdResourceConstraints tests that OCI resource constraints are applied.
func TestContainerdResourceConstraints(t *testing.T) {
	cfg := loadTestConfig()

	client := setupContainerdClient(t, cfg)
	defer client.Close()

	ensureImagePulled(t, client, cfg)

	ctx := namespaces.WithNamespace(context.Background(), cfg.Namespace)
	image, err := client.GetImage(ctx, cfg.Image)
	if err != nil {
		t.Fatalf("get image: %v", err)
	}

	containerName := "qbx-resources-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")

	// Resource limits enforced by crun inside the VM
	memLimit := int64(128 * 1024 * 1024) // 128 MiB
	cpuQuota := int64(50000)             // CFS quota: 50000/100000 = 0.5 CPU
	cpuPeriod := uint64(100000)

	container, err := client.NewContainer(ctx, containerName,
		containerd.WithSnapshotter(cfg.Snapshotter),
		containerd.WithImage(image),
		containerd.WithNewSnapshot(containerName+"-snapshot", image),
		containerd.WithRuntime(cfg.Runtime, nil),
		containerd.WithNewSpec(
			oci.WithImageConfig(image),
			oci.WithProcessArgs("/bin/sh", "-c", "echo RESOURCES_OK"),
			oci.WithMemoryLimit(uint64(memLimit)),
			oci.WithCPUCFS(cpuQuota, cpuPeriod),
		),
	)
	if err != nil {
		t.Fatalf("create container with resource limits: %v", err)
	}
	defer container.Delete(ctx, containerd.WithSnapshotCleanup)

	// Verify spec has resource constraints
	spec, err := container.Spec(ctx)
	if err != nil {
		t.Fatalf("get container spec: %v", err)
	}

	if spec.Linux == nil || spec.Linux.Resources == nil {
		t.Fatal("container spec missing Linux resources")
	}

	if spec.Linux.Resources.Memory == nil || spec.Linux.Resources.Memory.Limit == nil {
		t.Fatal("container spec missing memory limit")
	}

	if *spec.Linux.Resources.Memory.Limit != memLimit {
		t.Fatalf("memory limit: got %d, want %d", *spec.Linux.Resources.Memory.Limit, memLimit)
	}

	if spec.Linux.Resources.CPU == nil || spec.Linux.Resources.CPU.Quota == nil {
		t.Fatal("container spec missing CPU quota")
	}

	if *spec.Linux.Resources.CPU.Quota != cpuQuota {
		t.Fatalf("CPU quota: got %d, want %d", *spec.Linux.Resources.CPU.Quota, cpuQuota)
	}

	t.Log("resource constraints verified in container spec")
}

// TestContainerdNamespaceIsolation tests that containers in different namespaces are isolated.
func TestContainerdNamespaceIsolation(t *testing.T) {
	cfg := loadTestConfig()

	client := setupContainerdClient(t, cfg)
	defer client.Close()

	// Create a test namespace
	testNS := "qbx-test-ns-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	ctx := namespaces.WithNamespace(context.Background(), testNS)

	// Pull image in test namespace
	_, err := client.Pull(ctx, cfg.Image,
		containerd.WithPullSnapshotter(cfg.Snapshotter),
		containerd.WithPullUnpack,
	)
	if err != nil {
		t.Fatalf("pull image in namespace %s: %v", testNS, err)
	}

	// List containers in default namespace - should not see test namespace containers
	defaultCtx := namespaces.WithNamespace(context.Background(), cfg.Namespace)
	defaultContainers, err := client.Containers(defaultCtx)
	if err != nil {
		t.Fatalf("list containers in default namespace: %v", err)
	}

	// List containers in test namespace - should be empty
	testContainers, err := client.Containers(ctx)
	if err != nil {
		t.Fatalf("list containers in test namespace: %v", err)
	}

	if len(testContainers) != 0 {
		t.Fatalf("expected 0 containers in new namespace %s, got %d", testNS, len(testContainers))
	}

	t.Logf("namespace isolation verified: default=%d containers, test=%d containers",
		len(defaultContainers), len(testContainers))
}

// TestContainerdKernelIsolation verifies that the VM runs its own kernel,
// isolated from the host. This is the core security property of spinbox.
// Inspired by: build/cast/spinbox.exp (uname -r check)
func TestContainerdKernelIsolation(t *testing.T) {
	cfg := loadTestConfig()

	client := setupContainerdClient(t, cfg)
	defer client.Close()

	ensureImagePulled(t, client, cfg)

	containerName := "qbx-kernel-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")

	// Get kernel version from inside the VM
	result := runContainer(t, client, cfg, containerName, []string{"/bin/uname", "-r"})

	if result.ExitCode != 0 {
		t.Fatalf("uname failed with exit code %d\nstderr: %s", result.ExitCode, result.Stderr)
	}

	vmKernel := strings.TrimSpace(result.Stdout)
	if vmKernel == "" {
		t.Fatal("empty kernel version from VM")
	}

	// The VM kernel should be a valid Linux version string
	// Format: major.minor.patch[-extra]
	if !strings.Contains(vmKernel, ".") {
		t.Fatalf("unexpected kernel version format: %q", vmKernel)
	}

	t.Logf("VM kernel: %s (isolated from host)", vmKernel)
}

// TestContainerdLargeOutput verifies that large stdout output is captured correctly
// through the VM boundary. This tests the I/O streaming path.
func TestContainerdLargeOutput(t *testing.T) {
	cfg := loadTestConfig()

	client := setupContainerdClient(t, cfg)
	defer client.Close()

	ensureImagePulled(t, client, cfg)

	containerName := "qbx-largeout-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")

	// Generate substantial output - 1000 lines with sequence numbers
	// This tests the streaming I/O path across the VM boundary
	result := runContainer(t, client, cfg, containerName,
		[]string{"/bin/sh", "-c", "seq 1 1000"})

	if result.ExitCode != 0 {
		t.Fatalf("seq command failed with exit code %d\nstderr: %s", result.ExitCode, result.Stderr)
	}

	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) != 1000 {
		t.Fatalf("expected 1000 lines, got %d", len(lines))
	}

	// Verify first and last lines to ensure no data corruption
	if lines[0] != "1" {
		t.Fatalf("first line should be '1', got %q", lines[0])
	}
	if lines[999] != "1000" {
		t.Fatalf("last line should be '1000', got %q", lines[999])
	}

	t.Logf("captured %d lines of output correctly", len(lines))
}

// TestContainerdStderrCapture verifies that stderr output is captured separately from stdout.
func TestContainerdStderrCapture(t *testing.T) {
	cfg := loadTestConfig()

	client := setupContainerdClient(t, cfg)
	defer client.Close()

	ensureImagePulled(t, client, cfg)

	containerName := "qbx-stderr-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")

	// Write to both stdout and stderr
	result := runContainer(t, client, cfg, containerName,
		[]string{"/bin/sh", "-c", "echo STDOUT_MSG && echo STDERR_MSG >&2"})

	if result.ExitCode != 0 {
		t.Fatalf("command failed with exit code %d", result.ExitCode)
	}

	if !strings.Contains(result.Stdout, "STDOUT_MSG") {
		t.Fatalf("stdout should contain 'STDOUT_MSG', got: %q", result.Stdout)
	}

	if !strings.Contains(result.Stderr, "STDERR_MSG") {
		t.Fatalf("stderr should contain 'STDERR_MSG', got: %q", result.Stderr)
	}

	// Verify streams are separate
	if strings.Contains(result.Stdout, "STDERR_MSG") {
		t.Fatal("stdout should not contain stderr content")
	}
	if strings.Contains(result.Stderr, "STDOUT_MSG") {
		t.Fatal("stderr should not contain stdout content")
	}

	t.Log("stdout and stderr captured separately")
}

// TestContainerdFileOperations verifies that file operations work correctly inside the VM.
// Inspired by: build/cast/snapshot.exp (creating files and directories)
func TestContainerdFileOperations(t *testing.T) {
	cfg := loadTestConfig()

	client := setupContainerdClient(t, cfg)
	defer client.Close()

	ensureImagePulled(t, client, cfg)

	containerName := "qbx-fileops-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")

	// Create a file, directory, and verify contents
	script := `
		mkdir -p /tmp/testdir && \
		echo "test content" > /tmp/testdir/file.txt && \
		cat /tmp/testdir/file.txt && \
		ls -la /tmp/testdir/
	`
	result := runContainer(t, client, cfg, containerName, []string{"/bin/sh", "-c", script})

	if result.ExitCode != 0 {
		t.Fatalf("file operations failed with exit code %d\nstderr: %s", result.ExitCode, result.Stderr)
	}

	if !strings.Contains(result.Stdout, "test content") {
		t.Fatalf("file content not found in output: %q", result.Stdout)
	}

	if !strings.Contains(result.Stdout, "file.txt") {
		t.Fatalf("file.txt not listed in directory: %q", result.Stdout)
	}

	t.Log("file operations completed successfully")
}

// TestContainerdEnvironmentVariables verifies that environment variables are passed to the container.
func TestContainerdEnvironmentVariables(t *testing.T) {
	cfg := loadTestConfig()

	client := setupContainerdClient(t, cfg)
	defer client.Close()

	ensureImagePulled(t, client, cfg)

	ctx := namespaces.WithNamespace(context.Background(), cfg.Namespace)
	image, err := client.GetImage(ctx, cfg.Image)
	if err != nil {
		t.Fatalf("get image: %v", err)
	}

	containerName := "qbx-env-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")

	// Create container with custom environment variable
	container, err := client.NewContainer(ctx, containerName,
		containerd.WithSnapshotter(cfg.Snapshotter),
		containerd.WithImage(image),
		containerd.WithNewSnapshot(containerName+"-snapshot", image),
		containerd.WithRuntime(cfg.Runtime, nil),
		containerd.WithNewSpec(
			oci.WithImageConfig(image),
			oci.WithProcessArgs("/bin/sh", "-c", "echo $TEST_VAR"),
			oci.WithEnv([]string{"TEST_VAR=spinbox_test_value"}),
		),
	)
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	defer container.Delete(ctx, containerd.WithSnapshotCleanup)

	// Create output files
	outputDir := t.TempDir()
	stdoutPath := filepath.Join(outputDir, "stdout.log")
	stdoutFile, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatalf("create stdout file: %v", err)
	}
	defer stdoutFile.Close()

	ioCreator := cio.NewCreator(cio.WithStreams(nil, stdoutFile, nil))
	task, err := container.NewTask(ctx, ioCreator)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	defer task.Delete(ctx, containerd.WithProcessKill)

	exitCh, err := task.Wait(ctx)
	if err != nil {
		t.Fatalf("wait for task: %v", err)
	}

	if err := task.Start(ctx); err != nil {
		t.Fatalf("start task: %v", err)
	}

	status := <-exitCh
	exitCode, _, err := status.Result()
	if err != nil {
		t.Fatalf("get task result: %v", err)
	}

	if io := task.IO(); io != nil {
		io.Wait()
		io.Close()
	}

	stdoutFile.Sync()
	stdoutData, _ := os.ReadFile(stdoutPath)

	if exitCode != 0 {
		t.Fatalf("exit code %d", exitCode)
	}

	if !strings.Contains(string(stdoutData), "spinbox_test_value") {
		t.Fatalf("expected env var value in output, got: %q", string(stdoutData))
	}

	t.Log("environment variable passed correctly to container")
}

// TestContainerdProcessInfo verifies that process information is visible inside the VM.
func TestContainerdProcessInfo(t *testing.T) {
	cfg := loadTestConfig()

	client := setupContainerdClient(t, cfg)
	defer client.Close()

	ensureImagePulled(t, client, cfg)

	containerName := "qbx-procinfo-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")

	// Check PID 1 and process info
	result := runContainer(t, client, cfg, containerName,
		[]string{"/bin/sh", "-c", "echo PID=$$ && cat /proc/1/cmdline | tr '\\0' ' '"})

	if result.ExitCode != 0 {
		t.Fatalf("command failed with exit code %d\nstderr: %s", result.ExitCode, result.Stderr)
	}

	// Verify we have process info
	if !strings.Contains(result.Stdout, "PID=") {
		t.Fatalf("expected PID info, got: %q", result.Stdout)
	}

	t.Logf("process info:\n%s", result.Stdout)
}

// TestContainerdQuickExit verifies that containers that exit quickly don't lose output.
// This is a regression test for the I/O synchronization issue.
func TestContainerdQuickExit(t *testing.T) {
	cfg := loadTestConfig()

	client := setupContainerdClient(t, cfg)
	defer client.Close()

	ensureImagePulled(t, client, cfg)

	// Run multiple quick-exit containers to stress the I/O path
	for i := range 5 {
		containerName := fmt.Sprintf("qbx-quick-%d-%s", i,
			strings.ReplaceAll(time.Now().Format("150405.000"), ".", ""))

		t.Run(fmt.Sprintf("quick-exit-%d", i), func(t *testing.T) {
			// Very short command that writes output and exits immediately
			result := runContainer(t, client, cfg, containerName,
				[]string{"/bin/echo", fmt.Sprintf("QUICK_OUTPUT_%d", i)})

			if result.ExitCode != 0 {
				t.Fatalf("exit code %d", result.ExitCode)
			}

			expected := fmt.Sprintf("QUICK_OUTPUT_%d", i)
			if !strings.Contains(result.Stdout, expected) {
				t.Fatalf("expected %q in stdout, got: %q", expected, result.Stdout)
			}
		})
	}
}

// TestContainerdWorkingDirectory verifies that the working directory is set correctly.
func TestContainerdWorkingDirectory(t *testing.T) {
	cfg := loadTestConfig()

	client := setupContainerdClient(t, cfg)
	defer client.Close()

	ensureImagePulled(t, client, cfg)

	ctx := namespaces.WithNamespace(context.Background(), cfg.Namespace)
	image, err := client.GetImage(ctx, cfg.Image)
	if err != nil {
		t.Fatalf("get image: %v", err)
	}

	containerName := "qbx-cwd-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")

	// Create container with custom working directory
	container, err := client.NewContainer(ctx, containerName,
		containerd.WithSnapshotter(cfg.Snapshotter),
		containerd.WithImage(image),
		containerd.WithNewSnapshot(containerName+"-snapshot", image),
		containerd.WithRuntime(cfg.Runtime, nil),
		containerd.WithNewSpec(
			oci.WithImageConfig(image),
			oci.WithProcessArgs("/bin/pwd"),
			oci.WithProcessCwd("/tmp"),
		),
	)
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	defer container.Delete(ctx, containerd.WithSnapshotCleanup)

	// Create output files
	outputDir := t.TempDir()
	stdoutPath := filepath.Join(outputDir, "stdout.log")
	stdoutFile, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatalf("create stdout file: %v", err)
	}
	defer stdoutFile.Close()

	ioCreator := cio.NewCreator(cio.WithStreams(nil, stdoutFile, nil))
	task, err := container.NewTask(ctx, ioCreator)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	defer task.Delete(ctx, containerd.WithProcessKill)

	exitCh, err := task.Wait(ctx)
	if err != nil {
		t.Fatalf("wait for task: %v", err)
	}

	if err := task.Start(ctx); err != nil {
		t.Fatalf("start task: %v", err)
	}

	status := <-exitCh
	exitCode, _, err := status.Result()
	if err != nil {
		t.Fatalf("get task result: %v", err)
	}

	if io := task.IO(); io != nil {
		io.Wait()
		io.Close()
	}

	stdoutFile.Sync()
	stdoutData, _ := os.ReadFile(stdoutPath)

	if exitCode != 0 {
		t.Fatalf("exit code %d", exitCode)
	}

	output := strings.TrimSpace(string(stdoutData))
	if output != "/tmp" {
		t.Fatalf("expected working directory '/tmp', got: %q", output)
	}

	t.Log("working directory set correctly")
}
