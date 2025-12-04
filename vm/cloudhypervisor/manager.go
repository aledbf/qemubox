package cloudhypervisor

import (
	"context"
	"fmt"
	"os"

	"github.com/aledbf/beacon/containerd/paths"
)

// NewInstance creates a new Cloud Hypervisor VM instance.
// The containerID parameter uniquely identifies this VM for logging purposes.
// The state parameter is the directory where VM state files will be stored.
// The resourceCfg parameter specifies CPU and memory configuration.
func NewInstance(ctx context.Context, containerID, state string, resourceCfg *VMResourceConfig) (*Instance, error) {
	// Locate cloud-hypervisor binary
	binaryPath, err := findCloudHypervisor()
	if err != nil {
		return nil, err
	}

	// Ensure state directory exists
	if err := os.MkdirAll(state, 0755); err != nil {
		return nil, fmt.Errorf("failed to create state directory: %w", err)
	}

	return newInstance(ctx, containerID, binaryPath, state, resourceCfg)
}

// findCloudHypervisor returns the path to the cloud-hypervisor binary
func findCloudHypervisor() (string, error) {
	path := paths.CloudHypervisorPath()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("cloud-hypervisor binary not found at %s", path)
}

// findKernel returns the path to the kernel binary for Cloud Hypervisor
func findKernel() (string, error) {
	path := paths.KernelPath()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("kernel not found at %s (use BEACON_SHARE_DIR to override)", path)
}

// findInitrd returns the path to the initrd for Cloud Hypervisor
func findInitrd() (string, error) {
	path := paths.InitrdPath()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("initrd not found at %s (use BEACON_SHARE_DIR to override)", path)
}
