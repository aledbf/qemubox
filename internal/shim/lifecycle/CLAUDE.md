# Lifecycle Management - State Machine and Cleanup

**Technology**: Go, sync/atomic
**Entry Point**: `state.go`, `cleanup.go`, `vm.go`
**Parent Context**: This extends [../../../../CLAUDE.md](../../../../CLAUDE.md)

---

## Quick Overview

**What this package does**:
Provides lifecycle management primitives for the spinbox shim: a state machine for tracking shim states, a cleanup orchestrator for ordered resource cleanup, and a VM lifecycle manager. These components ensure safe state transitions and proper cleanup ordering.

**Key responsibilities**:
- Enforce valid state transitions (Idle → Creating → Running → Deleting → ShuttingDown)
- Provide atomic state operations to prevent race conditions
- Orchestrate cleanup in correct order (hotplug → IO → connection → VM → network → mounts → events)
- Manage VM instance lifecycle (create, client access, shutdown)

**Dependencies**:
- Internal: None (foundational package)
- External: `sync/atomic` from stdlib

---

## Development Commands

### From This Directory

```bash
# Run tests
go test ./...

# Run with race detection (important for this package!)
go test -race ./...
```

### Pre-PR Checklist

```bash
go fmt ./... && \
goimports -w . && \
go test -race ./... && \
task lint
```

---

## Architecture

### Directory Structure

```
lifecycle/
├── state.go        # State machine implementation
├── state_test.go   # State machine tests
├── cleanup.go      # Cleanup orchestrator
├── cleanup_test.go # Cleanup tests
├── errors.go       # Error types
├── vm.go           # VM lifecycle manager
└── CLAUDE.md       # This file
```

### State Machine

```
StateIdle (0)
    │
    ▼ TryStartCreating()
StateCreating (1)
    │
    ▼ MarkCreated()
StateRunning (2)
    │
    ▼ TryStartDeleting()
StateDeleting (3)
    │
    ▼ ForceTransition(StateShuttingDown)
StateShuttingDown (4)
```

### Cleanup Phases

Cleanup must happen in this order:

```
1. HotplugStop      - Stop CPU/memory hotplug controllers
2. IOShutdown       - Close I/O forwarders
3. ConnClose        - Close TTRPC client connection
4. VMShutdown       - Send SIGTERM/SIGKILL to QEMU
5. NetworkCleanup   - Release CNI allocations
6. MountCleanup     - Unmount host-side resources
7. EventClose       - Close event channel (signals forwarder to exit)
```

---

## Code Patterns

### State Machine Usage

```go
// Create state machine
sm := lifecycle.NewStateMachine()

// Try to start creating (atomic, returns false if invalid state)
if !sm.TryStartCreating() {
    return fmt.Errorf("cannot create in state: %s", sm.State())
}

// Mark creation complete
sm.MarkCreated()

// Check if intentional shutdown
if sm.IsIntentionalShutdown() {
    return // Don't process during shutdown
}

// Set intentional shutdown flag
sm.SetIntentionalShutdown(true)

// Try to start deleting
if !sm.TryStartDeleting() {
    return fmt.Errorf("cannot delete in state: %s", sm.State())
}

// Force transition (for shutdown, doesn't check current state)
sm.ForceTransition(lifecycle.StateShuttingDown)
```

### State Machine Internals

```go
type StateMachine struct {
    state               atomic.Int32  // Current state
    intentionalShutdown atomic.Bool   // Set when shutdown is intentional
}

// TryStartCreating atomically transitions from Idle to Creating
func (sm *StateMachine) TryStartCreating() bool {
    return sm.state.CompareAndSwap(int32(StateIdle), int32(StateCreating))
}

// State returns current state (atomic read)
func (sm *StateMachine) State() State {
    return State(sm.state.Load())
}
```

### Cleanup Orchestrator Usage

```go
// Define cleanup phases
phases := lifecycle.CleanupPhases{
    HotplugStop: func(ctx context.Context) error {
        return stopHotplugControllers()
    },
    IOShutdown: func(ctx context.Context) error {
        return shutdownIO()
    },
    ConnClose: func(ctx context.Context) error {
        return closeConnection()
    },
    VMShutdown: func(ctx context.Context) error {
        return shutdownVM()
    },
    NetworkCleanup: func(ctx context.Context) error {
        return releaseNetwork()
    },
    MountCleanup: func(ctx context.Context) error {
        return unmountResources()
    },
    EventClose: func(ctx context.Context) error {
        return closeEvents()
    },
}

// Execute all phases in order
orchestrator := lifecycle.NewCleanupOrchestrator(phases)
result := orchestrator.Execute(ctx)

// Check for errors
if result.HasErrors() {
    log.Warn("cleanup errors", "failed", result.FailedPhases())
}

// Get combined error
if err := result.AsError(); err != nil {
    return err
}
```

### Partial Cleanup

For failure scenarios, start cleanup from a specific phase:

```go
// Skip hotplug (not running), start from IO
result := orchestrator.ExecutePartial(ctx, lifecycle.PhaseIOShutdown)
```

### VM Lifecycle Manager

```go
// Create manager
vmLM := lifecycle.NewManager()

// Set VM instance
vmLM.SetInstance(vmInstance)

// Get instance
instance, err := vmLM.Instance()
if err != nil {
    return err // No instance set
}

// Get TTRPC client
client, err := vmLM.Client()
if err != nil {
    return err
}

// Dial new client (separate from cached)
client, err := vmLM.DialClient(ctx)
if err != nil {
    return err
}
defer client.Close()

// Dial with retry
client, err := vmLM.DialClientWithRetry(ctx, timeout)

// Shutdown VM
err := vmLM.Shutdown(ctx)
```

---

## Key Files

### Core Files

- **`state.go`** - State machine implementation
  - `StateMachine` struct
  - Atomic state transitions
  - `TryStartCreating()`, `MarkCreated()`, `TryStartDeleting()`

- **`cleanup.go`** - Cleanup orchestrator
  - `CleanupOrchestrator` struct
  - `CleanupPhases` definition
  - `Execute()`, `ExecutePartial()`

- **`vm.go`** - VM lifecycle manager
  - `Manager` struct
  - Instance management
  - Client access methods

- **`errors.go`** - Error types
  - `StateTransitionError`
  - Error wrapping helpers

---

## Quick Search Commands

### Find in This Package

```bash
# Find state transitions
rg -n "TryStart|Mark|ForceTransition" internal/shim/lifecycle/

# Find cleanup phases
rg -n "Phase|Cleanup" internal/shim/lifecycle/

# Find atomic operations
rg -n "atomic\.|CompareAndSwap|Load|Store" internal/shim/lifecycle/
```

### Find Usage in Other Packages

```bash
# Find state machine usage
rg -n "lifecycle\.State|lifecycle\.New" internal/shim/

# Find cleanup orchestrator usage
rg -n "CleanupOrchestrator|CleanupPhases" internal/shim/
```

---

## Common Gotchas

**Race condition in state checks**:
- Problem: Checking state then acting is not atomic
- Solution: Use `TryStart*` methods that atomically check-and-set

```go
// WRONG: Race condition
if sm.State() == StateIdle {
    sm.ForceTransition(StateCreating)  // Another goroutine might have changed state
}

// CORRECT: Atomic check-and-set
if !sm.TryStartCreating() {
    return fmt.Errorf("invalid state: %s", sm.State())
}
```

**Cleanup order violation**:
- Problem: Releasing network before VM shutdown causes issues
- Solution: Use `CleanupOrchestrator` which enforces order

**Missing cleanup phase**:
- Problem: Skipping a phase leaves resources orphaned
- Solution: Always define all phases, use no-op if not applicable

**Partial cleanup confusion**:
- Problem: `ExecutePartial` skips early phases
- Solution: Only use for failure recovery where early phases didn't run

---

## Testing

### Test Organization

- Unit tests: Colocated (`*_test.go`)
- **Race detection is critical** for this package

### Testing Patterns

**Testing state transitions**:
```go
func TestStateMachine_TryStartCreating_FromIdle(t *testing.T) {
    sm := NewStateMachine()

    ok := sm.TryStartCreating()

    assert.True(t, ok)
    assert.Equal(t, StateCreating, sm.State())
}

func TestStateMachine_TryStartCreating_FromRunning(t *testing.T) {
    sm := NewStateMachine()
    sm.TryStartCreating()
    sm.MarkCreated()

    ok := sm.TryStartCreating()

    assert.False(t, ok)  // Can't create from Running
    assert.Equal(t, StateRunning, sm.State())
}
```

**Testing cleanup orchestrator**:
```go
func TestCleanupOrchestrator_Execute(t *testing.T) {
    var order []string
    phases := CleanupPhases{
        HotplugStop: func(ctx context.Context) error {
            order = append(order, "hotplug")
            return nil
        },
        IOShutdown: func(ctx context.Context) error {
            order = append(order, "io")
            return nil
        },
        // ... other phases
    }

    orchestrator := NewCleanupOrchestrator(phases)
    result := orchestrator.Execute(context.Background())

    assert.False(t, result.HasErrors())
    assert.Equal(t, []string{"hotplug", "io", ...}, order)
}
```

### Running Tests

```bash
# All tests with race detection (critical!)
go test -race ./internal/shim/lifecycle/...

# Specific test
go test -run TestStateMachine ./internal/shim/lifecycle/...
```

---

## Package-Specific Rules

### State Machine Rules

- **MUST** use atomic operations for all state access
- **MUST** use `TryStart*` for check-and-set transitions
- **MUST NOT** check state then act separately (race condition)
- **SHOULD** check `IsIntentionalShutdown()` before processing

### Cleanup Rules

- **MUST** define all cleanup phases
- **MUST** execute phases in order (orchestrator handles this)
- **MUST NOT** skip cleanup phases without good reason
- **SHOULD** log cleanup errors but continue cleanup

### Concurrency Rules

- **MUST** use atomic operations from `sync/atomic`
- **MUST** run tests with `-race` flag
- **MUST NOT** use mutexes in this package (atomics only)
