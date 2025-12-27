package task

import (
	"context"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/containerd/ttrpc"

	"github.com/aledbf/qemubox/containerd/internal/host/vm"
	"github.com/aledbf/qemubox/containerd/internal/host/vm/qemu"
)

func (s *service) client() (*ttrpc.Client, error) {
	s.vmMu.Lock()
	defer s.vmMu.Unlock()
	if s.vm == nil {
		return nil, fmt.Errorf("vm not created: %w", errdefs.ErrFailedPrecondition)
	}
	client := s.vm.Client()
	if client == nil {
		return nil, fmt.Errorf("vm not running: %w", errdefs.ErrFailedPrecondition)
	}
	return client, nil
}

func (s *service) dialClient(ctx context.Context) (*ttrpc.Client, error) {
	s.vmMu.Lock()
	defer s.vmMu.Unlock()
	if s.vm == nil {
		return nil, fmt.Errorf("vm not created: %w", errdefs.ErrFailedPrecondition)
	}
	return s.vm.DialClient(ctx)
}

func (s *service) vmInstance(ctx context.Context, containerID, state string, resourceCfg *vm.VMResourceConfig) (vm.Instance, error) {
	s.vmMu.Lock()
	defer s.vmMu.Unlock()
	if s.vm != nil {
		return nil, fmt.Errorf("vm already exists; shim requires one VM per container: %w", errdefs.ErrAlreadyExists)
	}
	// Create instance (QEMU only).
	var err error
	s.vm, err = qemu.NewInstance(ctx, containerID, state, resourceCfg)
	if err != nil {
		return nil, err
	}
	return s.vm, nil
}
