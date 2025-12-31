//go:build !windows

package process

import (
	"context"
	"errors"
	"fmt"

	"github.com/containerd/console"
	"github.com/containerd/log"
)

type execState interface {
	state() State
	Resize(ws console.WinSize) error
	Start(ctx context.Context) error
	Delete(ctx context.Context) error
	Kill(ctx context.Context, sig uint32, all bool) error
	SetExited(status int)
	Status(ctx context.Context) (string, error)
}

type execCreatedState struct {
	p *execProcess
}

func (s *execCreatedState) state() State {
	return StateCreated
}

func (s *execCreatedState) transition(name string) error {
	switch name {
	case stateRunning:
		s.p.execState = &execRunningState{p: s.p}
	case stateStopped:
		s.p.execState = &execStoppedState{p: s.p}
	case stateDeleted:
		s.p.execState = &deletedState{}
	default:
		return fmt.Errorf("invalid state transition %q to %q", stateName(s), name)
	}
	return nil
}

func (s *execCreatedState) Resize(ws console.WinSize) error {
	return s.p.resize(ws)
}

func (s *execCreatedState) Start(ctx context.Context) error {
	if err := s.p.start(ctx); err != nil {
		return err
	}
	return s.transition(stateRunning)
}

func (s *execCreatedState) Delete(ctx context.Context) error {
	if err := s.p.delete(ctx); err != nil {
		return err
	}

	return s.transition(stateDeleted)
}

func (s *execCreatedState) Kill(ctx context.Context, sig uint32, all bool) error {
	return s.p.kill(ctx, sig, all)
}

func (s *execCreatedState) SetExited(status int) {
	s.p.setExited(status)

	if err := s.transition(stateStopped); err != nil {
		// Log but don't panic - the process has already exited, we must reflect that
		log.L.WithError(err).Error("invalid state transition during exit, forcing to stopped state")
		s.p.execState = &execStoppedState{p: s.p}
	}
}

func (s *execCreatedState) Status(ctx context.Context) (string, error) {
	return stateCreated, nil
}

type execRunningState struct {
	p *execProcess
}

func (s *execRunningState) state() State {
	return StateRunning
}

func (s *execRunningState) transition(name string) error {
	switch name {
	case stateStopped:
		s.p.execState = &execStoppedState{p: s.p}
	default:
		return fmt.Errorf("invalid state transition %q to %q", stateName(s), name)
	}
	return nil
}

func (s *execRunningState) Resize(ws console.WinSize) error {
	return s.p.resize(ws)
}

func (s *execRunningState) Start(ctx context.Context) error {
	return errors.New("cannot start a running process")
}

func (s *execRunningState) Delete(ctx context.Context) error {
	return errors.New("cannot delete a running process")
}

func (s *execRunningState) Kill(ctx context.Context, sig uint32, all bool) error {
	return s.p.kill(ctx, sig, all)
}

func (s *execRunningState) SetExited(status int) {
	s.p.setExited(status)

	if err := s.transition(stateStopped); err != nil {
		// Log but don't panic - the process has already exited, we must reflect that
		log.L.WithError(err).Error("invalid state transition during exit, forcing to stopped state")
		s.p.execState = &execStoppedState{p: s.p}
	}
}

func (s *execRunningState) Status(ctx context.Context) (string, error) {
	return stateRunning, nil
}

type execStoppedState struct {
	p *execProcess
}

func (s *execStoppedState) state() State {
	return StateStopped
}

func (s *execStoppedState) transition(name string) error {
	switch name {
	case stateDeleted:
		s.p.execState = &deletedState{}
	default:
		return fmt.Errorf("invalid state transition %q to %q", stateName(s), name)
	}
	return nil
}

func (s *execStoppedState) Resize(ws console.WinSize) error {
	return errors.New("cannot resize a stopped container")
}

func (s *execStoppedState) Start(ctx context.Context) error {
	return errors.New("cannot start a stopped process")
}

func (s *execStoppedState) Delete(ctx context.Context) error {
	if err := s.p.delete(ctx); err != nil {
		return err
	}

	return s.transition(stateDeleted)
}

func (s *execStoppedState) Kill(ctx context.Context, sig uint32, all bool) error {
	return s.p.kill(ctx, sig, all)
}

func (s *execStoppedState) SetExited(status int) {
	// no op
}

func (s *execStoppedState) Status(ctx context.Context) (string, error) {
	return stateStopped, nil
}
