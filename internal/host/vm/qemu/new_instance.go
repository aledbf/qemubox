package qemu

import (
	"context"
	"fmt"
	"os"

	"github.com/aledbf/qemubox/containerd/internal/host/vm"
)

// NewInstance creates a new QEMU VM instance.
func NewInstance(ctx context.Context, containerID, stateDir string, cfg *vm.VMResourceConfig) (vm.Instance, error) {
	binaryPath, err := findQemu()
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(stateDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create state directory: %w", err)
	}

	return newInstance(ctx, containerID, binaryPath, stateDir, cfg)
}
