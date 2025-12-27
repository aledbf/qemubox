package task

import (
	"context"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/containerd/ttrpc"

	"github.com/aledbf/qemubox/containerd/internal/host/vm"
)

func (s *service) client() (*ttrpc.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm == nil {
		return nil, fmt.Errorf("vm not created: %w", errdefs.ErrFailedPrecondition)
	}
	return s.vm.DialClient(ctx)
}

func (s *service) vmInstance(ctx context.Context, containerID, state string, resourceCfg *vm.VMResourceConfig) (vm.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm == nil {
		// Get VMM type from environment or default
		vmmType := vm.GetVMType()

		// Create factory for selected VMM
		factory, err := vm.NewFactory(ctx, vmmType)
		if err != nil {
			return nil, err
		}

		// Create instance
		s.vm, err = factory.NewInstance(ctx, containerID, state, resourceCfg)
		if err != nil {
			return nil, err
		}
	}
	return s.vm, nil
}
