//go:build !windows

package process

import (
	"context"
	"fmt"

	"github.com/containerd/console"
	google_protobuf "github.com/containerd/containerd/v2/pkg/protobuf/types"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
)

// validTransitions defines which state transitions are allowed.
var validTransitions = map[State][]State{
	StateCreated: {StateRunning, StateStopped, StateDeleted},
	StateRunning: {StateStopped},
	StateStopped: {StateDeleted},
	StateDeleted: {},
}

// canTransition checks if a state transition is valid.
func canTransition(from, to State) bool {
	for _, valid := range validTransitions[from] {
		if valid == to {
			return true
		}
	}
	return false
}

// initStateMachine implements initState using a table-driven approach.
type initStateMachine struct {
	p            *Init
	currentState State
}

func newInitStateMachine(p *Init) *initStateMachine {
	return &initStateMachine{p: p, currentState: StateCreated}
}

func (s *initStateMachine) state() State {
	return s.currentState
}

func (s *initStateMachine) transition(to State) error {
	if !canTransition(s.currentState, to) {
		return fmt.Errorf("invalid state transition %s to %s", s.currentState, to)
	}
	s.currentState = to
	return nil
}

func (s *initStateMachine) Start(ctx context.Context) error {
	switch s.currentState {
	case StateCreated:
		if err := s.p.start(ctx); err != nil {
			return err
		}
		return s.transition(StateRunning)
	case StateRunning:
		return fmt.Errorf("cannot start a running process")
	case StateStopped:
		return fmt.Errorf("cannot start a stopped process")
	case StateDeleted:
		return fmt.Errorf("cannot start a deleted process")
	default:
		return fmt.Errorf("unknown state: %s", s.currentState)
	}
}

func (s *initStateMachine) Delete(ctx context.Context) error {
	switch s.currentState {
	case StateCreated, StateStopped:
		if err := s.p.delete(ctx); err != nil {
			return err
		}
		return s.transition(StateDeleted)
	case StateRunning:
		return fmt.Errorf("cannot delete a running process")
	case StateDeleted:
		return fmt.Errorf("cannot delete a deleted process: %w", errdefs.ErrNotFound)
	default:
		return fmt.Errorf("unknown state: %s", s.currentState)
	}
}

func (s *initStateMachine) Kill(ctx context.Context, sig uint32, all bool) error {
	switch s.currentState {
	case StateCreated, StateRunning, StateStopped:
		return s.p.kill(ctx, sig, all)
	case StateDeleted:
		return fmt.Errorf("cannot kill a deleted process: %w", errdefs.ErrNotFound)
	default:
		return fmt.Errorf("unknown state: %s", s.currentState)
	}
}

func (s *initStateMachine) SetExited(status int) {
	switch s.currentState {
	case StateCreated, StateRunning:
		s.p.setExited(status)
		if err := s.transition(StateStopped); err != nil {
			// Log but don't panic - the process has already exited, we must reflect that
			log.L.WithError(err).Error("invalid state transition during exit, forcing to stopped state")
			s.currentState = StateStopped
		}
	case StateStopped, StateDeleted:
		// no-op
	}
}

func (s *initStateMachine) Exec(ctx context.Context, path string, r *ExecConfig) (Process, error) {
	switch s.currentState {
	case StateCreated, StateRunning:
		return s.p.exec(ctx, path, r)
	case StateStopped:
		return nil, fmt.Errorf("cannot exec in a stopped state")
	case StateDeleted:
		return nil, fmt.Errorf("cannot exec in a deleted state")
	default:
		return nil, fmt.Errorf("unknown state: %s", s.currentState)
	}
}

func (s *initStateMachine) Update(ctx context.Context, r *google_protobuf.Any) error {
	switch s.currentState {
	case StateCreated, StateRunning:
		return s.p.update(ctx, r)
	case StateStopped:
		return fmt.Errorf("cannot update a stopped container")
	case StateDeleted:
		return fmt.Errorf("cannot update a deleted process")
	default:
		return fmt.Errorf("unknown state: %s", s.currentState)
	}
}

func (s *initStateMachine) Status(ctx context.Context) (string, error) {
	return s.currentState.String(), nil
}

// execStateMachine implements execState using a table-driven approach.
type execStateMachine struct {
	p            *execProcess
	currentState State
}

func newExecStateMachine(p *execProcess) *execStateMachine {
	return &execStateMachine{p: p, currentState: StateCreated}
}

func (s *execStateMachine) state() State {
	return s.currentState
}

func (s *execStateMachine) transition(to State) error {
	if !canTransition(s.currentState, to) {
		return fmt.Errorf("invalid state transition %s to %s", s.currentState, to)
	}
	s.currentState = to
	return nil
}

func (s *execStateMachine) Start(ctx context.Context) error {
	switch s.currentState {
	case StateCreated:
		if err := s.p.start(ctx); err != nil {
			return err
		}
		return s.transition(StateRunning)
	case StateRunning:
		return fmt.Errorf("cannot start a running process")
	case StateStopped:
		return fmt.Errorf("cannot start a stopped process")
	case StateDeleted:
		return fmt.Errorf("cannot start a deleted process")
	default:
		return fmt.Errorf("unknown state: %s", s.currentState)
	}
}

func (s *execStateMachine) Delete(ctx context.Context) error {
	switch s.currentState {
	case StateCreated, StateStopped:
		if err := s.p.delete(ctx); err != nil {
			return err
		}
		return s.transition(StateDeleted)
	case StateRunning:
		return fmt.Errorf("cannot delete a running process")
	case StateDeleted:
		return fmt.Errorf("cannot delete a deleted process: %w", errdefs.ErrNotFound)
	default:
		return fmt.Errorf("unknown state: %s", s.currentState)
	}
}

func (s *execStateMachine) Kill(ctx context.Context, sig uint32, all bool) error {
	switch s.currentState {
	case StateCreated, StateRunning, StateStopped:
		return s.p.kill(ctx, sig, all)
	case StateDeleted:
		return fmt.Errorf("cannot kill a deleted process: %w", errdefs.ErrNotFound)
	default:
		return fmt.Errorf("unknown state: %s", s.currentState)
	}
}

func (s *execStateMachine) SetExited(status int) {
	switch s.currentState {
	case StateCreated, StateRunning:
		s.p.setExited(status)
		if err := s.transition(StateStopped); err != nil {
			// Log but don't panic - the process has already exited, we must reflect that
			log.L.WithError(err).Error("invalid state transition during exit, forcing to stopped state")
			s.currentState = StateStopped
		}
	case StateStopped, StateDeleted:
		// no-op
	}
}

func (s *execStateMachine) Resize(ws console.WinSize) error {
	switch s.currentState {
	case StateCreated, StateRunning:
		return s.p.resize(ws)
	case StateStopped:
		return fmt.Errorf("cannot resize a stopped container")
	case StateDeleted:
		return fmt.Errorf("cannot resize a deleted process")
	default:
		return fmt.Errorf("unknown state: %s", s.currentState)
	}
}

func (s *execStateMachine) Status(ctx context.Context) (string, error) {
	return s.currentState.String(), nil
}

// initState is the interface that Init uses for state management.
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

// execState is the interface that execProcess uses for state management.
type execState interface {
	state() State
	Resize(ws console.WinSize) error
	Start(ctx context.Context) error
	Delete(ctx context.Context) error
	Kill(ctx context.Context, sig uint32, all bool) error
	SetExited(status int)
	Status(ctx context.Context) (string, error)
}

// Compile-time interface checks
var (
	_ initState = (*initStateMachine)(nil)
	_ execState = (*execStateMachine)(nil)
)
