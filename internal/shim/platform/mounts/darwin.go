//go:build darwin

package mounts

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/api/types"

	"github.com/spin-stack/spinbox/internal/host/vm"
)

type darwinManager struct{}

func newManager() Manager {
	return &darwinManager{}
}

func (m *darwinManager) Setup(_ context.Context, _ vm.Instance, _ string, _ []*types.Mount) (SetupResult, error) {
	return SetupResult{}, fmt.Errorf("mounts not supported on darwin")
}
