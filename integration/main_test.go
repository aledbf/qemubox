//go:build linux

package integration

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/aledbf/qemubox/containerd/internal/host/vm"
	"github.com/aledbf/qemubox/containerd/internal/host/vm/qemu"
)

func TestMain(m *testing.M) {
	// Package-level validation only
	if !haveKVM() {
		log.Print("skipping integration tests: /dev/kvm not available")
		os.Exit(0)
	}

	if !haveBuildDir() {
		log.Print("skipping integration tests: ../build directory not found")
		os.Exit(0)
	}

	os.Exit(m.Run())
}

// haveKVM checks if KVM is available for VM tests.
func haveKVM() bool {
	_, err := os.Stat("/dev/kvm")
	return err == nil
}

// haveBuildDir checks if the build directory exists.
func haveBuildDir() bool {
	buildPath, err := filepath.Abs("../build")
	if err != nil {
		return false
	}
	info, err := os.Stat(buildPath)
	return err == nil && info.IsDir()
}

// setupTestPath adds the build directory to PATH for the test.
// This must be called by each test that needs the qemubox binaries.
func setupTestPath(t *testing.T) {
	t.Helper()

	buildPath, err := filepath.Abs("../build")
	if err != nil {
		t.Fatalf("resolve build path: %v", err)
	}

	currentPath := os.Getenv("PATH")
	t.Setenv("PATH", buildPath+":"+currentPath)
}

// vmTestConfig holds configuration for VM-based tests.
type vmTestConfig struct {
	BootCPUs          int
	MaxCPUs           int
	MemorySize        int64
	MemoryHotplugSize int64
}

// defaultVMTestConfig returns the default VM configuration for testing.
func defaultVMTestConfig() vmTestConfig {
	return vmTestConfig{
		BootCPUs:          1,
		MaxCPUs:           2,
		MemorySize:        512 * 1024 * 1024,  // 512 MiB
		MemoryHotplugSize: 1024 * 1024 * 1024, // 1 GiB
	}
}

// runWithVM creates a VM instance and runs the test function with it.
// The VM is automatically cleaned up after the test completes.
func runWithVM(t *testing.T, testFn func(context.Context, *testing.T, vm.Instance)) {
	t.Helper()

	setupTestPath(t)

	cfg := defaultVMTestConfig()
	runWithVMConfig(t, cfg, testFn)
}

// runWithVMConfig creates a VM instance with custom configuration and runs the test function.
func runWithVMConfig(t *testing.T, cfg vmTestConfig, testFn func(context.Context, *testing.T, vm.Instance)) {
	t.Helper()

	ctx := t.Context()
	stateDir := filepath.Join(t.TempDir(), "vm-state")

	resourceCfg := &vm.VMResourceConfig{
		BootCPUs:          cfg.BootCPUs,
		MaxCPUs:           cfg.MaxCPUs,
		MemorySize:        cfg.MemorySize,
		MemoryHotplugSize: cfg.MemoryHotplugSize,
	}

	instance, err := qemu.NewInstance(ctx, t.Name(), stateDir, resourceCfg)
	if err != nil {
		t.Fatalf("create VM instance: %v", err)
	}

	if err := instance.Start(ctx); err != nil {
		t.Fatalf("start VM instance: %v", err)
	}

	// Ensure VM is cleaned up even if test panics
	t.Cleanup(func() {
		if err := instance.Shutdown(context.Background()); err != nil {
			t.Logf("shutdown VM: %v", err)
		}
	})

	testFn(ctx, t, instance)
}
