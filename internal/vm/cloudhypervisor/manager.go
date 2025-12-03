package cloudhypervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/aledbf/beacon/containerd/internal/paths"
)

// NewInstance creates a new Cloud Hypervisor VM instance.
// The state parameter is the directory where VM state files will be stored.
// The resourceCfg parameter specifies CPU and memory configuration.
func NewInstance(ctx context.Context, state string, resourceCfg *VMResourceConfig) (*Instance, error) {
	// Locate cloud-hypervisor binary
	binaryPath, err := findCloudHypervisor()
	if err != nil {
		return nil, err
	}

	// Ensure state directory exists
	if err := os.MkdirAll(state, 0755); err != nil {
		return nil, fmt.Errorf("failed to create state directory: %w", err)
	}

	return newInstance(ctx, binaryPath, state, resourceCfg)
}

// findCloudHypervisor locates the cloud-hypervisor binary
func findCloudHypervisor() (string, error) {
	// 1. Check environment variable
	if path := os.Getenv("CLOUD_HYPERVISOR_PATH"); path != "" {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
		return "", fmt.Errorf("CLOUD_HYPERVISOR_PATH set to %q but file not found", path)
	}

	// 2. Search in PATH
	if path, err := exec.LookPath("cloud-hypervisor"); err == nil {
		return path, nil
	}

	// 3. Check common installation locations
	commonPaths := []string{
		"/usr/local/bin/cloud-hypervisor",
		"/usr/bin/cloud-hypervisor",
		"/opt/cloud-hypervisor/bin/cloud-hypervisor",
	}

	for _, path := range commonPaths {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("cloud-hypervisor binary not found in PATH or common locations; install cloud-hypervisor or set CLOUD_HYPERVISOR_PATH")
}

// findKernel locates the kernel binary for Cloud Hypervisor
func findKernel() (string, error) {
	kernelName := paths.KernelName()

	// Search through all configured paths
	for _, dir := range paths.KernelSearchPaths() {
		if dir == "" {
			dir = "."
		}
		path := filepath.Join(dir, kernelName)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("kernel %q not found in search paths (use BEACON_SHARE_DIR or install to %s)", kernelName, paths.ShareDir)
}

// findInitrd locates the initrd for Cloud Hypervisor
func findInitrd() (string, error) {
	initrdName := paths.InitrdName()

	// Search through all configured paths
	for _, dir := range paths.KernelSearchPaths() {
		if dir == "" {
			dir = "."
		}
		path := filepath.Join(dir, initrdName)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("initrd %q not found in search paths (use BEACON_SHARE_DIR or install to %s)", initrdName, paths.ShareDir)
}
