//go:build !windows

package process

import (
	"context"
	"errors"
	"fmt"

	"github.com/containerd/console"
	google_protobuf "github.com/containerd/containerd/v2/pkg/protobuf/types"
	"github.com/containerd/errdefs"
)

type deletedState struct {
}

func (s *deletedState) Pause(ctx context.Context) error {
	return errors.New("cannot pause a deleted process")
}

func (s *deletedState) Resume(ctx context.Context) error {
	return errors.New("cannot resume a deleted process")
}

func (s *deletedState) Update(context context.Context, r *google_protobuf.Any) error {
	return errors.New("cannot update a deleted process")
}

func (s *deletedState) Checkpoint(ctx context.Context, r *CheckpointConfig) error {
	return errors.New("cannot checkpoint a deleted process")
}

func (s *deletedState) Resize(ws console.WinSize) error {
	return errors.New("cannot resize a deleted process")
}

func (s *deletedState) Start(ctx context.Context) error {
	return errors.New("cannot start a deleted process")
}

func (s *deletedState) Delete(ctx context.Context) error {
	return fmt.Errorf("cannot delete a deleted process: %w", errdefs.ErrNotFound)
}

func (s *deletedState) Kill(ctx context.Context, sig uint32, all bool) error {
	return fmt.Errorf("cannot kill a deleted process: %w", errdefs.ErrNotFound)
}

func (s *deletedState) SetExited(status int) {
	// no op
}

func (s *deletedState) Exec(ctx context.Context, path string, r *ExecConfig) (Process, error) {
	return nil, errors.New("cannot exec in a deleted state")
}

func (s *deletedState) Status(ctx context.Context) (string, error) {
	return stateStopped, nil
}
