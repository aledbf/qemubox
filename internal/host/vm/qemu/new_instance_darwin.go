//go:build darwin

package qemu

import (
	"context"
	"fmt"

	"github.com/spin-stack/spinbox/internal/host/vm"
)

// NewInstance returns an error on Darwin (not supported)
func NewInstance(ctx context.Context, containerID, stateDir string, cfg *vm.VMResourceConfig) (vm.Instance, error) {
	return nil, fmt.Errorf("QEMU VM instances not supported on darwin")
}
