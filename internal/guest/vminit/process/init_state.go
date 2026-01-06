//go:build !windows

package process

import (
	"context"
	"errors"
	"fmt"

	google_protobuf "github.com/containerd/containerd/v2/pkg/protobuf/types"
	"github.com/containerd/log"
)

type initState interface {
	state() State
	Start(ctx context.Context) error
	Delete(ctx context.Context) error
	Update(ctx context.Context, r *google_protobuf.Any) error
	Exec(ctx context.Context, id string, r *ExecConfig) (Process, error)
	Kill(ctx context.Context, sig uint32, all bool) error
	SetExited(status int)
	Status(ctx context.Context) (string, error)
}

type createdState struct {
	p *Init
}

func (s *createdState) state() State {
	return StateCreated
}

func (s *createdState) transition(name string) error {
	switch name {
	case stateRunning:
		s.p.initState = &runningState{p: s.p}
	case stateStopped:
		s.p.initState = &stoppedState{p: s.p}
	case stateDeleted:
		s.p.initState = &deletedState{}
	default:
		return fmt.Errorf("invalid state transition %q to %q", stateName(s), name)
	}
	return nil
}

func (s *createdState) Update(ctx context.Context, r *google_protobuf.Any) error {
	return s.p.update(ctx, r)
}

func (s *createdState) Start(ctx context.Context) error {
	if err := s.p.start(ctx); err != nil {
		return err
	}
	return s.transition(stateRunning)
}

func (s *createdState) Delete(ctx context.Context) error {
	if err := s.p.delete(ctx); err != nil {
		return err
	}
	return s.transition(stateDeleted)
}

func (s *createdState) Kill(ctx context.Context, sig uint32, all bool) error {
	return s.p.kill(ctx, sig, all)
}

func (s *createdState) SetExited(status int) {
	s.p.setExited(status)

	if err := s.transition(stateStopped); err != nil {
		// Log but don't panic - the process has already exited, we must reflect that
		log.L.WithError(err).Error("invalid state transition during exit, forcing to stopped state")
		s.p.initState = &stoppedState{p: s.p}
	}
}

func (s *createdState) Exec(ctx context.Context, path string, r *ExecConfig) (Process, error) {
	return s.p.exec(ctx, path, r)
}

func (s *createdState) Status(ctx context.Context) (string, error) {
	return stateCreated, nil
}

type runningState struct {
	p *Init
}

func (s *runningState) state() State {
	return StateRunning
}

func (s *runningState) transition(name string) error {
	switch name {
	case stateStopped:
		s.p.initState = &stoppedState{p: s.p}
	default:
		return fmt.Errorf("invalid state transition %q to %q", stateName(s), name)
	}
	return nil
}

func (s *runningState) Update(ctx context.Context, r *google_protobuf.Any) error {
	return s.p.update(ctx, r)
}

func (s *runningState) Start(ctx context.Context) error {
	return errors.New("cannot start a running process")
}

func (s *runningState) Delete(ctx context.Context) error {
	return errors.New("cannot delete a running process")
}

func (s *runningState) Kill(ctx context.Context, sig uint32, all bool) error {
	return s.p.kill(ctx, sig, all)
}

func (s *runningState) SetExited(status int) {
	s.p.setExited(status)

	if err := s.transition(stateStopped); err != nil {
		// Log but don't panic - the process has already exited, we must reflect that
		log.L.WithError(err).Error("invalid state transition during exit, forcing to stopped state")
		s.p.initState = &stoppedState{p: s.p}
	}
}

func (s *runningState) Exec(ctx context.Context, path string, r *ExecConfig) (Process, error) {
	return s.p.exec(ctx, path, r)
}

func (s *runningState) Status(ctx context.Context) (string, error) {
	return stateRunning, nil
}

type stoppedState struct {
	p *Init
}

func (s *stoppedState) state() State {
	return StateStopped
}

func (s *stoppedState) transition(name string) error {
	switch name {
	case stateDeleted:
		s.p.initState = &deletedState{}
	default:
		return fmt.Errorf("invalid state transition %q to %q", stateName(s), name)
	}
	return nil
}

func (s *stoppedState) Update(ctx context.Context, r *google_protobuf.Any) error {
	return errors.New("cannot update a stopped container")
}

func (s *stoppedState) Start(ctx context.Context) error {
	return errors.New("cannot start a stopped process")
}

func (s *stoppedState) Delete(ctx context.Context) error {
	if err := s.p.delete(ctx); err != nil {
		return err
	}
	return s.transition(stateDeleted)
}

func (s *stoppedState) Kill(ctx context.Context, sig uint32, all bool) error {
	return s.p.kill(ctx, sig, all)
}

func (s *stoppedState) SetExited(status int) {
	// no op
}

func (s *stoppedState) Exec(ctx context.Context, path string, r *ExecConfig) (Process, error) {
	return nil, errors.New("cannot exec in a stopped state")
}

func (s *stoppedState) Status(ctx context.Context) (string, error) {
	return stateStopped, nil
}
