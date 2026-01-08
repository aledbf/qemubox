//go:build !windows

package process

import (
	"context"
	"strings"
	"testing"
)

// TestInitStateSetExited tests SetExited transitions for Init process
func TestInitStateSetExited(t *testing.T) {
	tests := []struct {
		name          string
		initialState  State
		expectedFinal State
	}{
		{"created → stopped", StateCreated, StateStopped},
		{"running → stopped", StateRunning, StateStopped},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := &Init{waitBlock: make(chan struct{})}
			sm := newInitStateMachine(proc)

			// Manually set state for testing
			sm.currentState = tt.initialState
			proc.initState = sm

			sm.SetExited(0)

			if got := sm.state(); got != tt.expectedFinal {
				t.Errorf("Expected state %s, got %s", tt.expectedFinal, got)
			}
		})
	}
}

// TestInitInvalidStateTransitions tests that invalid operations return errors
func TestInitInvalidStateTransitions(t *testing.T) {
	tests := []struct {
		name          string
		initialState  State
		operation     func(*initStateMachine) error
		expectedError string
	}{
		{
			name:          "stopped: cannot start",
			initialState:  StateStopped,
			operation:     func(s *initStateMachine) error { return s.Start(context.Background()) },
			expectedError: "cannot start a stopped process",
		},
		{
			name:          "deleted: cannot start",
			initialState:  StateDeleted,
			operation:     func(s *initStateMachine) error { return s.Start(context.Background()) },
			expectedError: "cannot start a deleted process",
		},
		{
			name:          "deleted: cannot delete",
			initialState:  StateDeleted,
			operation:     func(s *initStateMachine) error { return s.Delete(context.Background()) },
			expectedError: "cannot delete a deleted process",
		},
		{
			name:          "running: cannot delete",
			initialState:  StateRunning,
			operation:     func(s *initStateMachine) error { return s.Delete(context.Background()) },
			expectedError: "cannot delete a running process",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := &Init{}
			sm := newInitStateMachine(proc)
			sm.currentState = tt.initialState

			err := tt.operation(sm)

			if err == nil {
				t.Fatal("Expected error but got nil")
			}

			if !strings.Contains(err.Error(), tt.expectedError) {
				t.Errorf("Expected error containing %q, got %q", tt.expectedError, err.Error())
			}
		})
	}
}

// TestExecStateSetExited tests SetExited transitions for exec process
func TestExecStateSetExited(t *testing.T) {
	tests := []struct {
		name          string
		initialState  State
		expectedFinal State
	}{
		{"created → stopped", StateCreated, StateStopped},
		{"running → stopped", StateRunning, StateStopped},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := &execProcess{waitBlock: make(chan struct{})}
			sm := newExecStateMachine(proc)
			sm.currentState = tt.initialState
			proc.execState = sm

			sm.SetExited(0)

			if got := sm.state(); got != tt.expectedFinal {
				t.Errorf("Expected state %s, got %s", tt.expectedFinal, got)
			}
		})
	}
}

// TestExecInvalidStateTransitions tests invalid exec operations
func TestExecInvalidStateTransitions(t *testing.T) {
	tests := []struct {
		name          string
		initialState  State
		operation     func(*execStateMachine) error
		expectedError string
	}{
		{
			name:          "stopped: cannot start",
			initialState:  StateStopped,
			operation:     func(s *execStateMachine) error { return s.Start(context.Background()) },
			expectedError: "cannot start a stopped process",
		},
		{
			name:          "deleted: cannot start",
			initialState:  StateDeleted,
			operation:     func(s *execStateMachine) error { return s.Start(context.Background()) },
			expectedError: "cannot start a deleted process",
		},
		{
			name:          "deleted: cannot delete",
			initialState:  StateDeleted,
			operation:     func(s *execStateMachine) error { return s.Delete(context.Background()) },
			expectedError: "cannot delete a deleted process",
		},
		{
			name:          "running: cannot delete",
			initialState:  StateRunning,
			operation:     func(s *execStateMachine) error { return s.Delete(context.Background()) },
			expectedError: "cannot delete a running process",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := &execProcess{}
			sm := newExecStateMachine(proc)
			sm.currentState = tt.initialState

			err := tt.operation(sm)

			if err == nil {
				t.Fatal("Expected error but got nil")
			}

			if !strings.Contains(err.Error(), tt.expectedError) {
				t.Errorf("Expected error containing %q, got %q", tt.expectedError, err.Error())
			}
		})
	}
}

// TestStateString tests the State.String() method
func TestStateString(t *testing.T) {
	tests := []struct {
		state    State
		expected string
	}{
		{StateCreated, "created"},
		{StateRunning, "running"},
		{StateStopped, "stopped"},
		{StateDeleted, "deleted"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if got := tt.state.String(); got != tt.expected {
				t.Errorf("Expected %s, got %s", tt.expected, got)
			}
		})
	}
}

// TestDeletedStateSetExitedNoOp tests that SetExited on deleted state is a no-op
func TestDeletedStateSetExitedNoOp(t *testing.T) {
	proc := &Init{}
	sm := newInitStateMachine(proc)
	sm.currentState = StateDeleted

	// Should not panic
	sm.SetExited(0)

	// Status should still be deleted
	status, err := sm.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() returned error: %v", err)
	}
	if status != StateDeleted.String() {
		t.Errorf("Expected status %s, got %s", StateDeleted.String(), status)
	}
}

// TestStatusMethod tests that Status returns correct state name
func TestStatusMethod(t *testing.T) {
	tests := []struct {
		name           string
		state          State
		expectedStatus string
	}{
		{"created", StateCreated, "created"},
		{"running", StateRunning, "running"},
		{"stopped", StateStopped, "stopped"},
		{"deleted", StateDeleted, "deleted"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := &Init{}
			sm := newInitStateMachine(proc)
			sm.currentState = tt.state

			status, err := sm.Status(context.Background())
			if err != nil {
				t.Fatalf("Status() returned error: %v", err)
			}
			if status != tt.expectedStatus {
				t.Errorf("Expected status %s, got %s", tt.expectedStatus, status)
			}
		})
	}
}

// TestKillAllowedInMostStates tests that Kill is allowed in most states
func TestKillAllowedInMostStates(t *testing.T) {
	states := []struct {
		name  string
		state State
	}{
		{"created", StateCreated},
		{"running", StateRunning},
		{"stopped", StateStopped},
	}

	for _, tt := range states {
		t.Run(tt.name, func(t *testing.T) {
			proc := &Init{}
			sm := newInitStateMachine(proc)
			sm.currentState = tt.state

			// Kill should not return "invalid state" errors for these states
			// It may fail for other reasons (no process, etc.) but not state
			err := sm.Kill(context.Background(), 9, false)

			// We expect errors (no actual process), but not state-related errors
			if err != nil && strings.Contains(err.Error(), "invalid state") {
				t.Errorf("Kill should be allowed in %s state, got: %v", tt.name, err)
			}
		})
	}
}

// TestDeletedStateAllOperationsFail tests that deleted state rejects all operations
func TestDeletedStateAllOperationsFail(t *testing.T) {
	proc := &Init{}
	sm := newInitStateMachine(proc)
	sm.currentState = StateDeleted

	operations := []struct {
		name      string
		operation func() error
	}{
		{"Start", func() error { return sm.Start(context.Background()) }},
		{"Delete", func() error { return sm.Delete(context.Background()) }},
		{"Kill", func() error { return sm.Kill(context.Background(), 9, false) }},
		{"Update", func() error { return sm.Update(context.Background(), nil) }},
	}

	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			err := op.operation()
			if err == nil {
				t.Errorf("Expected error for %s on deleted state", op.name)
			}
			if !strings.Contains(err.Error(), "deleted") {
				t.Errorf("Expected error mentioning 'deleted', got: %v", err)
			}
		})
	}
}

// TestExecStatusMethod tests exec state status reporting
func TestExecStatusMethod(t *testing.T) {
	tests := []struct {
		name           string
		state          State
		expectedStatus string
	}{
		{"created", StateCreated, "created"},
		{"running", StateRunning, "running"},
		{"stopped", StateStopped, "stopped"},
		{"deleted", StateDeleted, "deleted"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := &execProcess{}
			sm := newExecStateMachine(proc)
			sm.currentState = tt.state

			status, err := sm.Status(context.Background())
			if err != nil {
				t.Fatalf("Status() returned error: %v", err)
			}
			if status != tt.expectedStatus {
				t.Errorf("Expected status %s, got %s", tt.expectedStatus, status)
			}
		})
	}
}

// TestTransitionValidation tests the canTransition function
func TestTransitionValidation(t *testing.T) {
	tests := []struct {
		name          string
		from          State
		to            State
		shouldSucceed bool
	}{
		// Valid transitions from created
		{"created→running", StateCreated, StateRunning, true},
		{"created→stopped", StateCreated, StateStopped, true},
		{"created→deleted", StateCreated, StateDeleted, true},

		// Valid transitions from running
		{"running→stopped", StateRunning, StateStopped, true},
		// Invalid transitions from running
		{"running→created", StateRunning, StateCreated, false},
		{"running→deleted", StateRunning, StateDeleted, false},

		// Valid transitions from stopped
		{"stopped→deleted", StateStopped, StateDeleted, true},
		// Invalid transitions from stopped
		{"stopped→created", StateStopped, StateCreated, false},
		{"stopped→running", StateStopped, StateRunning, false},

		// No transitions from deleted
		{"deleted→created", StateDeleted, StateCreated, false},
		{"deleted→running", StateDeleted, StateRunning, false},
		{"deleted→stopped", StateDeleted, StateStopped, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := canTransition(tt.from, tt.to)

			if tt.shouldSucceed && !result {
				t.Errorf("Expected transition %s → %s to be valid", tt.from, tt.to)
			}
			if !tt.shouldSucceed && result {
				t.Errorf("Expected transition %s → %s to be invalid", tt.from, tt.to)
			}
		})
	}
}

// TestSetExitedIdempotent tests that SetExited can be called multiple times safely
func TestSetExitedIdempotent(t *testing.T) {
	proc := &Init{waitBlock: make(chan struct{})}
	sm := newInitStateMachine(proc)
	proc.initState = sm

	// First SetExited should transition to stopped
	sm.SetExited(0)

	status, _ := sm.Status(context.Background())
	if status != StateStopped.String() {
		t.Errorf("Expected stopped state after first SetExited, got %s", status)
	}

	// Second SetExited should not panic (already in stopped state)
	sm.SetExited(1)

	// Should still be stopped
	status, _ = sm.Status(context.Background())
	if status != StateStopped.String() {
		t.Errorf("Expected stopped state after second SetExited, got %s", status)
	}
}

// TestStateTransitionStatusCheck tests concurrent status checks
func TestStateTransitionStatusCheck(t *testing.T) {
	proc := &Init{}
	sm := newInitStateMachine(proc)
	sm.currentState = StateRunning
	proc.initState = sm

	// Status should be callable concurrently
	type result struct {
		status string
		err    error
	}
	results := make(chan result, 10)

	for range 10 {
		go func() {
			status, err := sm.Status(context.Background())
			results <- result{status: status, err: err}
		}()
	}

	// Wait for all goroutines and check results
	for range 10 {
		r := <-results
		if r.err != nil {
			t.Errorf("Status() error: %v", r.err)
		}
		if r.status != StateRunning.String() {
			t.Errorf("Expected running, got %s", r.status)
		}
	}
}

// TestInitStateMachineInterface verifies initStateMachine implements initState
func TestInitStateMachineInterface(t *testing.T) {
	var _ initState = (*initStateMachine)(nil)
}

// TestExecStateMachineInterface verifies execStateMachine implements execState
func TestExecStateMachineInterface(t *testing.T) {
	var _ execState = (*execStateMachine)(nil)
}
