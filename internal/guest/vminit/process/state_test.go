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
		setupState    func(*Init) initState
		expectedFinal string
	}{
		{"created → stopped", func(p *Init) initState { return &createdState{p: p} }, stateStopped},
		{"running → stopped", func(p *Init) initState { return &runningState{p: p} }, stateStopped},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := &Init{}
			state := tt.setupState(proc)
			proc.initState = state

			state.SetExited(0)

			if got := stateName(proc.initState); got != tt.expectedFinal {
				t.Errorf("Expected state %s, got %s", tt.expectedFinal, got)
			}
		})
	}
}

// TestInitInvalidStateTransitions tests that invalid operations return errors
func TestInitInvalidStateTransitions(t *testing.T) {
	tests := []struct {
		name          string
		state         initState
		operation     func(initState) error
		expectedError string
	}{
		{
			name:          "stopped: cannot start",
			state:         &stoppedState{p: &Init{}},
			operation:     func(s initState) error { return s.Start(context.Background()) },
			expectedError: "cannot start a stopped process",
		},
		{
			name:          "deleted: cannot start",
			state:         &deletedState{},
			operation:     func(s initState) error { return s.Start(context.Background()) },
			expectedError: "cannot start a deleted process",
		},
		{
			name:          "deleted: cannot delete",
			state:         &deletedState{},
			operation:     func(s initState) error { return s.Delete(context.Background()) },
			expectedError: "cannot delete a deleted process",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.operation(tt.state)

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
		setupState    func(*execProcess) execState
		expectedFinal string
	}{
		{"execCreated → stopped", func(p *execProcess) execState { return &execCreatedState{p: p} }, stateStopped},
		{"execRunning → stopped", func(p *execProcess) execState { return &execRunningState{p: p} }, stateStopped},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := &execProcess{}
			state := tt.setupState(proc)
			proc.execState = state

			state.SetExited(0)

			if got := stateName(proc.execState); got != tt.expectedFinal {
				t.Errorf("Expected state %s, got %s", tt.expectedFinal, got)
			}
		})
	}
}

// TestExecInvalidStateTransitions tests invalid exec operations
func TestExecInvalidStateTransitions(t *testing.T) {
	tests := []struct {
		name          string
		state         execState
		operation     func(execState) error
		expectedError string
	}{
		{
			name:          "execStopped: cannot start",
			state:         &execStoppedState{p: &execProcess{}},
			operation:     func(s execState) error { return s.Start(context.Background()) },
			expectedError: "cannot start a stopped process",
		},
		{
			name:          "deleted: cannot start",
			state:         &deletedState{},
			operation:     func(s execState) error { return s.Start(context.Background()) },
			expectedError: "cannot start a deleted process",
		},
		{
			name:          "deleted: cannot delete",
			state:         &deletedState{},
			operation:     func(s execState) error { return s.Delete(context.Background()) },
			expectedError: "cannot delete a deleted process",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.operation(tt.state)

			if err == nil {
				t.Fatal("Expected error but got nil")
			}

			if !strings.Contains(err.Error(), tt.expectedError) {
				t.Errorf("Expected error containing %q, got %q", tt.expectedError, err.Error())
			}
		})
	}
}

// TestStateNameFunction tests the stateName helper
func TestStateNameFunction(t *testing.T) {
	tests := []struct {
		name         string
		state        interface{}
		expectedName string
	}{
		{"createdState", &createdState{}, stateCreated},
		{"runningState", &runningState{}, stateRunning},
		{"stoppedState", &stoppedState{}, stateStopped},
		{"deletedState", &deletedState{}, stateDeleted},
		{"execCreatedState", &execCreatedState{}, stateCreated},
		{"execRunningState", &execRunningState{}, stateRunning},
		{"execStoppedState", &execStoppedState{}, stateStopped},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := stateName(tt.state)
			if name != tt.expectedName {
				t.Errorf("Expected state name %s, got %s", tt.expectedName, name)
			}
		})
	}
}

// TestDeletedStateSetExitedNoOp tests that SetExited on deleted state is a no-op
func TestDeletedStateSetExitedNoOp(t *testing.T) {
	// SetExited on deleted state should be a no-op (not panic)
	state := &deletedState{}

	// Should not panic
	state.SetExited(0)

	// Status should still be deleted
	status, err := state.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() returned error: %v", err)
	}
	if status != stateDeleted {
		t.Errorf("Expected status %s, got %s", stateDeleted, status)
	}
}

// TestStatusMethod tests that Status returns correct state name
func TestStatusMethod(t *testing.T) {
	tests := []struct {
		name           string
		state          initState
		expectedStatus string
	}{
		{"created", &createdState{p: &Init{}}, stateCreated},
		{"running", &runningState{p: &Init{}}, stateRunning},
		{"stopped", &stoppedState{p: &Init{}}, stateStopped},
		{"deleted", &deletedState{}, stateDeleted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, err := tt.state.Status(context.Background())
			if err != nil {
				t.Fatalf("Status() returned error: %v", err)
			}
			if status != tt.expectedStatus {
				t.Errorf("Expected status %s, got %s", tt.expectedStatus, status)
			}
		})
	}
}

// TestKillAllowedInAllStates tests that Kill is allowed in any state
func TestKillAllowedInAllStates(t *testing.T) {
	states := []struct {
		name  string
		state initState
	}{
		{"created", &createdState{p: &Init{}}},
		{"running", &runningState{p: &Init{}}},
		{"stopped", &stoppedState{p: &Init{}}},
		// Note: deleted state returns specific error
	}

	for _, tt := range states {
		t.Run(tt.name, func(t *testing.T) {
			// Kill should not return "invalid state" errors for these states
			// It may fail for other reasons (no process, etc.) but not state
			err := tt.state.Kill(context.Background(), 9, false)

			// We expect errors (no actual process), but not state-related errors
			if err != nil && strings.Contains(err.Error(), "invalid state") {
				t.Errorf("Kill should be allowed in %s state, got: %v", tt.name, err)
			}
		})
	}
}

// TestDeletedStateAllOperationsFail tests that deleted state rejects all operations
func TestDeletedStateAllOperationsFail(t *testing.T) {
	state := &deletedState{}

	operations := []struct {
		name      string
		operation func() error
	}{
		{"Start", func() error { return state.Start(context.Background()) }},
		{"Delete", func() error { return state.Delete(context.Background()) }},
		{"Kill", func() error { return state.Kill(context.Background(), 9, false) }},
		{"Update", func() error { return state.Update(context.Background(), nil) }},
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
		state          execState
		expectedStatus string
	}{
		{"execCreated", &execCreatedState{p: &execProcess{}}, stateCreated},
		{"execRunning", &execRunningState{p: &execProcess{}}, stateRunning},
		{"execStopped", &execStoppedState{p: &execProcess{}}, stateStopped},
		{"deleted", &deletedState{}, stateDeleted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, err := tt.state.Status(context.Background())
			if err != nil {
				t.Fatalf("Status() returned error: %v", err)
			}
			if status != tt.expectedStatus {
				t.Errorf("Expected status %s, got %s", tt.expectedStatus, status)
			}
		})
	}
}

// TestTransitionValidation tests the transition() method validation
func TestTransitionValidation(t *testing.T) {
	tests := []struct {
		name          string
		fromState     initState
		toState       string
		shouldSucceed bool
	}{
		// Valid transitions from created
		{"created→running", &createdState{p: &Init{}}, stateRunning, true},
		{"created→stopped", &createdState{p: &Init{}}, stateStopped, true},
		{"created→deleted", &createdState{p: &Init{}}, stateDeleted, true},

		// Valid transitions from running
		{"running→stopped", &runningState{p: &Init{}}, stateStopped, true},
		// Invalid transitions from running
		{"running→created", &runningState{p: &Init{}}, stateCreated, false},
		{"running→deleted", &runningState{p: &Init{}}, stateDeleted, false},

		// Valid transitions from stopped
		{"stopped→deleted", &stoppedState{p: &Init{}}, stateDeleted, true},
		// Invalid transitions from stopped
		{"stopped→created", &stoppedState{p: &Init{}}, stateCreated, false},
		{"stopped→running", &stoppedState{p: &Init{}}, stateRunning, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error

			switch s := tt.fromState.(type) {
			case *createdState:
				err = s.transition(tt.toState)
			case *runningState:
				err = s.transition(tt.toState)
			case *stoppedState:
				err = s.transition(tt.toState)
			}

			if tt.shouldSucceed && err != nil {
				t.Errorf("Expected transition to succeed, got error: %v", err)
			}
			if !tt.shouldSucceed && err == nil {
				t.Error("Expected transition to fail, but it succeeded")
			}
			if !tt.shouldSucceed && err != nil && !strings.Contains(err.Error(), "invalid state transition") {
				t.Errorf("Expected 'invalid state transition' error, got: %v", err)
			}
		})
	}
}

// TestSetExitedIdempotent tests that SetExited can be called multiple times safely
func TestSetExitedIdempotent(t *testing.T) {
	proc := &Init{}
	proc.initState = &createdState{p: proc}

	// First SetExited should transition to stopped
	proc.initState.SetExited(0)

	status, _ := proc.initState.Status(context.Background())
	if status != stateStopped {
		t.Errorf("Expected stopped state after first SetExited, got %s", status)
	}

	// Second SetExited should not panic (already in stopped state)
	// Note: This calls SetExited on stoppedState which is a no-op
	proc.initState.SetExited(1)

	// Should still be stopped
	status, _ = proc.initState.Status(context.Background())
	if status != stateStopped {
		t.Errorf("Expected stopped state after second SetExited, got %s", status)
	}
}

// TestStateTransitionThreadSafety tests concurrent status checks
// Note: Full thread-safety requires the parent process's mutex
func TestStateTransitionStatusCheck(t *testing.T) {
	proc := &Init{}
	proc.initState = &runningState{p: proc}

	// Status should be callable concurrently
	// Collect errors instead of calling t.Errorf from goroutines (not goroutine-safe)
	type result struct {
		status string
		err    error
	}
	results := make(chan result, 10)

	for range 10 {
		go func() {
			status, err := proc.initState.Status(context.Background())
			results <- result{status: status, err: err}
		}()
	}

	// Wait for all goroutines and check results
	for range 10 {
		r := <-results
		if r.err != nil {
			t.Errorf("Status() error: %v", r.err)
		}
		if r.status != stateRunning {
			t.Errorf("Expected running, got %s", r.status)
		}
	}
}
