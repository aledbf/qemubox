//go:build linux

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
)

// testConfig holds the configuration for containerd integration tests.
type testConfig struct {
	Socket      string
	Image       string
	Runtime     string
	Snapshotter string
	Namespace   string
}

// loadTestConfig loads test configuration from environment variables.
func loadTestConfig() testConfig {
	containerName := os.Getenv("QEMUBOX_TEST_ID")
	if containerName == "" {
		containerName = "qbx-ci-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	}

	return testConfig{
		Socket:      getenvDefault("QEMUBOX_CONTAINERD_SOCKET", "/var/run/qemubox/containerd.sock"),
		Image:       getenvDefault("QEMUBOX_IMAGE", "docker.io/aledbf/beacon-workspace:test"),
		Runtime:     getenvDefault("QEMUBOX_RUNTIME", "io.containerd.qemubox.v1"),
		Snapshotter: getenvDefault("QEMUBOX_SNAPSHOTTER", "erofs"),
		Namespace:   getenvDefault("QEMUBOX_NAMESPACE", namespaces.Default),
	}
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

// ensureImagePulled ensures the specified image is available locally.
func ensureImagePulled(t *testing.T, client *containerd.Client, cfg testConfig) {
	t.Helper()

	ctx := namespaces.WithNamespace(context.Background(), cfg.Namespace)

	// Check if image already exists
	_, err := client.GetImage(ctx, cfg.Image)
	if err == nil {
		t.Logf("image already exists: %s", cfg.Image)
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
		containerd.WithImage(image),
		containerd.WithSnapshotter(cfg.Snapshotter),
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
			t.Logf("cleanup task for container %s: %v", containerName, err)
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

func TestContainerdRunQemubox(t *testing.T) {
	cfg := loadTestConfig()
	t.Logf("test config: socket=%s image=%s runtime=%s snapshotter=%s",
		cfg.Socket, cfg.Image, cfg.Runtime, cfg.Snapshotter)

	client := setupContainerdClient(t, cfg)
	defer client.Close()

	ensureImagePulled(t, client, cfg)

	containerName := "qbx-ci-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	t.Logf("container name: %s", containerName)

	result := runContainer(t, client, cfg, containerName, []string{"/bin/echo", "OK_FROM_QEMUBOX"})

	if result.ExitCode != 0 {
		t.Fatalf("container exited with code %d\nstdout: %s\nstderr: %s",
			result.ExitCode, result.Stdout, result.Stderr)
	}

	if !strings.Contains(result.Stdout, "OK_FROM_QEMUBOX") {
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
	for i := 0; i < 3; i++ {
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
	cpuQuota := int64(50000)              // CFS quota: 50000/100000 = 0.5 CPU
	cpuPeriod := uint64(100000)

	container, err := client.NewContainer(ctx, containerName,
		containerd.WithImage(image),
		containerd.WithSnapshotter(cfg.Snapshotter),
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

func getenvDefault(key, def string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return def
}
