//go:build darwin

package mounts

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/api/types"

	"github.com/aledbf/qemubox/containerd/internal/host/vm"
)

type darwinManager struct{}

func newManager() Manager {
	return &darwinManager{}
}

func (m *darwinManager) Setup(ctx context.Context, vmi vm.Instance, id string, rootfs []*types.Mount, bundleRootfs string, mountDir string) ([]*types.Mount, error) {
	return nil, fmt.Errorf("mounts not supported on darwin")
}
