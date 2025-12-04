package task

import (
	"context"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/containerd/ttrpc"

	"github.com/aledbf/beacon/containerd/vm/cloudhypervisor"
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

func (s *service) vmInstance(ctx context.Context, containerID, state string, resourceCfg *cloudhypervisor.VMResourceConfig) (*cloudhypervisor.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm == nil {
		var err error
		s.vm, err = cloudhypervisor.NewInstance(ctx, containerID, state, resourceCfg)
		if err != nil {
			return nil, err
		}
	}
	return s.vm, nil
}
