package qemu

import (
	"context"
	"fmt"
	"os"

	"github.com/aledbf/beacon/containerd/vm"
)

type factory struct{}

// init registers the QEMU factory
func init() {
	vm.Register(vm.VMTypeQEMU, &factory{})
}

func (f *factory) NewInstance(ctx context.Context, containerID, stateDir string, cfg *vm.VMResourceConfig) (vm.Instance, error) {
	// Locate QEMU binary
	binaryPath, err := findQemu()
	if err != nil {
		return nil, err
	}

	// Ensure state directory exists
	if err := os.MkdirAll(stateDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create state directory: %w", err)
	}

	return newInstance(ctx, containerID, binaryPath, stateDir, cfg)
}
