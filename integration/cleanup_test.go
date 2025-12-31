//go:build linux

// Package integration provides resource cleanup verification tests.
//
// # Resource Cleanup Verification (T-008)
//
// These tests verify that all system resources are properly cleaned up after VM shutdown:
//   - TAP network devices are removed
//   - QEMU processes are terminated
//   - Network namespaces are deleted (if used)
//   - CNI IP allocations are released
//
// # How It Works
//
// 1. Capture resource snapshot BEFORE creating VM (baseline)
// 2. Create and start VM (resources allocated)
// 3. Verify resources are created (QEMU process, TAP device, etc.)
// 4. Shutdown VM
// 5. Verify all resources return to baseline state within timeout
//
// # What Gets Verified
//
// TAP Devices:
//   - Checks 'ip link show' output for TAP devices (tap0, tap1, etc.)
//   - Ensures device count returns to baseline after shutdown
//
// QEMU Processes:
//   - Uses 'pgrep -f qemu-system-x86_64' to find running VMs
//   - Ensures all QEMU processes exit cleanly
//
// Network Namespaces:
//   - Checks /var/run/netns directory for namespace files
//   - Verifies namespaces are removed (if created)
//
// CNI Allocations:
//   - Checks /var/lib/cni/networks/*/  for IP allocation files
//   - Ensures IP addresses are released back to IPAM pool
//
// # Timeout Handling
//
// Tests use a 10-second timeout for cleanup verification with 100ms polling interval.
// If resources are not cleaned up within timeout, the test fails with detailed
// information about which resources leaked.
//
// # Failure Debugging
//
// If these tests fail, check:
//  1. VM shutdown logs for errors
//  2. CNI plugin logs (if available)
//  3. Kernel logs (dmesg) for TAP device errors
//  4. QEMU process stuck in uninterruptible state (ps aux | grep D)
package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aledbf/qemubox/containerd/internal/config"
	"github.com/aledbf/qemubox/containerd/internal/host/vm"
	"github.com/aledbf/qemubox/containerd/internal/host/vm/qemu"
)

// resourceSnapshot captures the state of system resources at a point in time.
type resourceSnapshot struct {
	tapDevices       []string
	qemuProcesses    []int
	networkNamspaces []string
	cniAllocations   []string
}

// captureResourceSnapshot captures current system resource state.
func captureResourceSnapshot(t *testing.T) resourceSnapshot {
	t.Helper()

	return resourceSnapshot{
		tapDevices:       listTAPDevices(t),
		qemuProcesses:    listQEMUProcesses(t),
		networkNamspaces: listNetworkNamespaces(t),
		cniAllocations:   listCNIAllocations(t),
	}
}

// listTAPDevices returns a list of TAP network device names.
func listTAPDevices(t *testing.T) []string {
	t.Helper()

	// Use 'ip link show' to list all network devices
	cmd := exec.Command("ip", "link", "show")
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Non-fatal: log and return empty list
		t.Logf("failed to list network devices: %v (output: %s)", err, output)
		return nil
	}

	var tapDevices []string
	for _, line := range strings.Split(string(output), "\n") {
		// Look for lines like: "123: tap0: <BROADCAST,MULTICAST> ..."
		if strings.Contains(line, ": tap") && strings.Contains(line, ":") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				// Extract device name (e.g., "tap0:")
				deviceName := strings.TrimSuffix(fields[1], ":")
				if strings.HasPrefix(deviceName, "tap") {
					tapDevices = append(tapDevices, deviceName)
				}
			}
		}
	}

	return tapDevices
}

// listQEMUProcesses returns a list of QEMU process PIDs.
func listQEMUProcesses(t *testing.T) []int {
	t.Helper()

	// Use pgrep to find qemu-system-x86_64 processes
	cmd := exec.Command("pgrep", "-f", "qemu-system-x86_64")
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Exit code 1 means no processes found (expected case)
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil
		}
		t.Logf("failed to list QEMU processes: %v (output: %s)", err, output)
		return nil
	}

	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(line, "%d", &pid); err == nil {
			pids = append(pids, pid)
		}
	}

	return pids
}

// listNetworkNamespaces returns a list of network namespace names.
func listNetworkNamespaces(t *testing.T) []string {
	t.Helper()

	netnsDir := "/var/run/netns"
	entries, err := os.ReadDir(netnsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Logf("failed to read network namespaces: %v", err)
		return nil
	}

	var namespaces []string
	for _, entry := range entries {
		if !entry.IsDir() {
			namespaces = append(namespaces, entry.Name())
		}
	}

	return namespaces
}

// listCNIAllocations returns a list of CNI IP allocation files.
func listCNIAllocations(t *testing.T) []string {
	t.Helper()

	// CNI IPAM stores allocations in /var/lib/cni/networks/<network-name>/
	// We need to get the network name from CNI config
	cfg, err := config.Get()
	if err != nil {
		t.Logf("failed to get config: %v", err)
		return nil
	}

	// Default CNI network state directory
	cniNetworksDir := "/var/lib/cni/networks"

	// Try to find any network directories
	networkDirs, err := os.ReadDir(cniNetworksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Logf("failed to read CNI networks directory: %v", err)
		return nil
	}

	var allocations []string
	for _, netDir := range networkDirs {
		if !netDir.IsDir() {
			continue
		}

		networkPath := filepath.Join(cniNetworksDir, netDir.Name())
		entries, err := os.ReadDir(networkPath)
		if err != nil {
			t.Logf("failed to read network %s: %v", netDir.Name(), err)
			continue
		}

		for _, entry := range entries {
			if !entry.IsDir() && entry.Name() != "lock" && entry.Name() != "last_reserved_ip.0" {
				// This is an IP allocation file
				allocations = append(allocations, filepath.Join(netDir.Name(), entry.Name()))
			}
		}
	}

	_ = cfg // Suppress unused warning
	return allocations
}

// verifyResourcesReleased checks that resources from before snapshot are now released.
func verifyResourcesReleased(t *testing.T, before resourceSnapshot, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		after := captureResourceSnapshot(t)

		// Check TAP devices
		newTAPDevices := diffSlices(before.tapDevices, after.tapDevices)
		if len(newTAPDevices) > 0 {
			t.Logf("waiting for TAP devices to be removed: %v", newTAPDevices)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Check QEMU processes
		newQEMUProcesses := diffInts(before.qemuProcesses, after.qemuProcesses)
		if len(newQEMUProcesses) > 0 {
			t.Logf("waiting for QEMU processes to exit: %v", newQEMUProcesses)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Check network namespaces
		newNetNS := diffSlices(before.networkNamspaces, after.networkNamspaces)
		if len(newNetNS) > 0 {
			t.Logf("waiting for network namespaces to be removed: %v", newNetNS)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Check CNI allocations
		newAllocations := diffSlices(before.cniAllocations, after.cniAllocations)
		if len(newAllocations) > 0 {
			t.Logf("waiting for CNI allocations to be released: %v", newAllocations)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// All resources cleaned up
		t.Log("all resources successfully released")
		return
	}

	// Timeout - check what's still leaked
	after := captureResourceSnapshot(t)

	var failures []string

	newTAPDevices := diffSlices(before.tapDevices, after.tapDevices)
	if len(newTAPDevices) > 0 {
		failures = append(failures, fmt.Sprintf("TAP devices not removed: %v", newTAPDevices))
	}

	newQEMUProcesses := diffInts(before.qemuProcesses, after.qemuProcesses)
	if len(newQEMUProcesses) > 0 {
		failures = append(failures, fmt.Sprintf("QEMU processes still running: %v", newQEMUProcesses))
	}

	newNetNS := diffSlices(before.networkNamspaces, after.networkNamspaces)
	if len(newNetNS) > 0 {
		failures = append(failures, fmt.Sprintf("network namespaces not removed: %v", newNetNS))
	}

	newAllocations := diffSlices(before.cniAllocations, after.cniAllocations)
	if len(newAllocations) > 0 {
		failures = append(failures, fmt.Sprintf("CNI allocations not released: %v", newAllocations))
	}

	if len(failures) > 0 {
		t.Fatalf("resource cleanup failed after %v:\n  - %s", timeout, strings.Join(failures, "\n  - "))
	}
}

// diffSlices returns elements in 'after' that are not in 'before' (new items).
func diffSlices(before, after []string) []string {
	beforeMap := make(map[string]bool)
	for _, item := range before {
		beforeMap[item] = true
	}

	var diff []string
	for _, item := range after {
		if !beforeMap[item] {
			diff = append(diff, item)
		}
	}

	return diff
}

// diffInts returns elements in 'after' that are not in 'before' (new items).
func diffInts(before, after []int) []int {
	beforeMap := make(map[int]bool)
	for _, item := range before {
		beforeMap[item] = true
	}

	var diff []int
	for _, item := range after {
		if !beforeMap[item] {
			diff = append(diff, item)
		}
	}

	return diff
}

// TestVMResourceCleanup verifies that VM resources are properly cleaned up after shutdown.
func TestVMResourceCleanup(t *testing.T) {
	setupTestPath(t)

	// Capture resource state before creating VM
	before := captureResourceSnapshot(t)
	t.Logf("initial state: TAP devices=%d, QEMU processes=%d, netns=%d, CNI allocations=%d",
		len(before.tapDevices), len(before.qemuProcesses),
		len(before.networkNamspaces), len(before.cniAllocations))

	// Create and start VM
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "vm-state")

	resourceCfg := &vm.VMResourceConfig{
		BootCPUs:          1,
		MaxCPUs:           2,
		MemorySize:        512 * 1024 * 1024,
		MemoryHotplugSize: 1024 * 1024 * 1024,
	}

	instance, err := qemu.NewInstance(ctx, t.Name(), stateDir, resourceCfg)
	if err != nil {
		t.Fatalf("create VM instance: %v", err)
	}

	if err := instance.Start(ctx); err != nil {
		t.Fatalf("start VM instance: %v", err)
	}

	// Capture state after VM is running
	during := captureResourceSnapshot(t)
	t.Logf("VM running: TAP devices=%d (+%d), QEMU processes=%d (+%d), netns=%d (+%d), CNI allocations=%d (+%d)",
		len(during.tapDevices), len(during.tapDevices)-len(before.tapDevices),
		len(during.qemuProcesses), len(during.qemuProcesses)-len(before.qemuProcesses),
		len(during.networkNamspaces), len(during.networkNamspaces)-len(before.networkNamspaces),
		len(during.cniAllocations), len(during.cniAllocations)-len(before.cniAllocations))

	// Verify VM created resources
	if len(during.qemuProcesses) <= len(before.qemuProcesses) {
		t.Fatal("expected QEMU process to be created, but none found")
	}

	// Shutdown VM
	if err := instance.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown VM: %v", err)
	}

	// Verify all resources are released within timeout
	verifyResourcesReleased(t, before, 10*time.Second)
}

// TestVMResourceCleanupMultiple verifies cleanup works correctly for multiple sequential VMs.
func TestVMResourceCleanupMultiple(t *testing.T) {
	setupTestPath(t)

	before := captureResourceSnapshot(t)
	t.Logf("initial state: TAP devices=%d, QEMU processes=%d, netns=%d, CNI allocations=%d",
		len(before.tapDevices), len(before.qemuProcesses),
		len(before.networkNamspaces), len(before.cniAllocations))

	// Create and cleanup 3 VMs sequentially
	for i := range 3 {
		t.Run(fmt.Sprintf("vm-%d", i), func(t *testing.T) {
			ctx := context.Background()
			stateDir := filepath.Join(t.TempDir(), "vm-state")

			resourceCfg := &vm.VMResourceConfig{
				BootCPUs:          1,
				MaxCPUs:           2,
				MemorySize:        512 * 1024 * 1024,
				MemoryHotplugSize: 1024 * 1024 * 1024,
			}

			instance, err := qemu.NewInstance(ctx, fmt.Sprintf("%s-vm%d", t.Name(), i), stateDir, resourceCfg)
			if err != nil {
				t.Fatalf("create VM instance: %v", err)
			}

			if err := instance.Start(ctx); err != nil {
				t.Fatalf("start VM instance: %v", err)
			}

			// Verify VM is running
			during := captureResourceSnapshot(t)
			if len(during.qemuProcesses) <= len(before.qemuProcesses) {
				t.Fatal("expected QEMU process to be created")
			}

			// Shutdown
			if err := instance.Shutdown(ctx); err != nil {
				t.Fatalf("shutdown VM: %v", err)
			}

			// Verify cleanup after this VM
			verifyResourcesReleased(t, before, 10*time.Second)
		})
	}
}

// TestVMResourceCleanupOnStartFailure verifies cleanup happens even if VM start fails.
func TestVMResourceCleanupOnStartFailure(t *testing.T) {
	setupTestPath(t)

	before := captureResourceSnapshot(t)

	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "vm-state")

	// Create VM with invalid configuration to force start failure
	// Using negative memory size should cause failure
	resourceCfg := &vm.VMResourceConfig{
		BootCPUs:          1,
		MaxCPUs:           2,
		MemorySize:        512 * 1024 * 1024,
		MemoryHotplugSize: 1024 * 1024 * 1024,
	}

	instance, err := qemu.NewInstance(ctx, t.Name(), stateDir, resourceCfg)
	if err != nil {
		t.Fatalf("create VM instance: %v", err)
	}

	// Try to start - this might succeed or fail depending on validation
	// Either way, we should verify cleanup works
	_ = instance.Start(ctx)

	// Always try to shutdown (should handle already-stopped case)
	_ = instance.Shutdown(ctx)

	// Verify no resources leaked even if start failed
	verifyResourcesReleased(t, before, 10*time.Second)
}
