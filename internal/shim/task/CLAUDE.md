# Task Service - Containerd Shim Implementation

**Technology**: Go, containerd TTRPC, vsock
**Entry Point**: `service.go`
**Parent Context**: This extends [../../../CLAUDE.md](../../../CLAUDE.md)

---

## Quick Overview

**What this package does**:
The task service implements containerd's `TTRPCTaskService` interface, acting as the bridge between containerd on the host and container workloads running inside QEMU VMs. It orchestrates VM lifecycle, container creation, I/O streams, and event forwarding.

**Key responsibilities**:
- Implement containerd task API (Create, Start, Delete, Exec, State, etc.)
- Manage VM lifecycle via `lifecycle.Manager`
- Forward I/O between host FIFOs and guest processes
- Coordinate CPU/memory hotplug controllers
- Forward events from guest to containerd

**Dependencies**:
- Internal: `lifecycle`, `cpuhotplug`, `memhotplug`, `network`, `vm`
- External: `containerd/ttrpc`, `containerd/containerd/v2`

---

## Development Commands

### From This Directory

```bash
# Run tests
go test ./...

# Run with race detection
go test -race ./...

# Run specific test
go test -run TestService_Create ./...
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
task/
├── service.go       # Main service struct, lifecycle methods
├── create.go        # Create() implementation (if split)
├── io.go            # I/O forwarding logic
├── connmanager.go   # TTRPC client connection management
└── *_test.go        # Unit tests
```

### Core Type: `service` struct

The `service` struct is the central coordinator. Understanding its fields is essential:

```go
type service struct {
    // State Machine - manages lifecycle transitions
    stateMachine *lifecycle.StateMachine

    // Locks (MUST acquire in this order)
    containerMu  sync.Mutex  // Protects: container, containerID
    controllerMu sync.Mutex  // Protects: hotplug controller maps

    // Dependency Managers (thread-safe, injected)
    vmLifecycle     *lifecycle.Manager
    networkManager  network.NetworkManager
    platformMounts  platformMounts.Manager
    platformNetwork platformNetwork.Manager

    // Container State (protected by containerMu)
    container   *container
    containerID string

    // Hotplug Controllers (protected by controllerMu)
    cpuHotplugControllers    map[string]cpuhotplug.CPUHotplugController
    memoryHotplugControllers map[string]memhotplug.MemoryHotplugController

    // Event Channel (multi-producer, single-consumer)
    events chan any

    // Shutdown Coordination
    eventsClosed     atomic.Bool
    inflight         atomic.Int64
    initStarted      atomic.Bool
    connManager      *ConnectionManager
}
```

---

## Code Patterns

### Lock Ordering [CRITICAL]

**MUST acquire locks in this order to prevent deadlocks**:
1. `containerMu` first
2. `controllerMu` second

```go
// CORRECT: Acquire containerMu before controllerMu
s.containerMu.Lock()
// ... access container state ...
s.controllerMu.Lock()
// ... access controllers ...
s.controllerMu.Unlock()
s.containerMu.Unlock()

// WRONG: Never acquire controllerMu first if you need both
s.controllerMu.Lock()
s.containerMu.Lock()  // DEADLOCK RISK
```

**NEVER hold locks during slow operations**:
```go
// CORRECT: Collect-then-execute pattern
s.containerMu.Lock()
forwarders := collectForwarders(s.container)  // Fast: just copy references
s.containerMu.Unlock()

// Now do slow operations outside lock
for _, f := range forwarders {
    f.Shutdown(ctx)  // Slow: network I/O
}

// WRONG: Holding lock during slow operations
s.containerMu.Lock()
for _, f := range s.container.io.forwarders {
    f.Shutdown(ctx)  // BLOCKS OTHER GOROUTINES
}
s.containerMu.Unlock()
```

### State Machine Usage

Use `stateMachine` for all lifecycle transitions:

```go
// Starting creation
if !s.stateMachine.TryStartCreating() {
    return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition,
        "cannot create in state: %s", s.stateMachine.State())
}
defer func() {
    if err != nil {
        s.stateMachine.ForceTransition(lifecycle.StateIdle)
    }
}()

// Mark creation complete
s.stateMachine.MarkCreated()

// Check intentional shutdown
if s.stateMachine.IsIntentionalShutdown() {
    return  // Don't process, we're shutting down
}
```

### Event Sending

Always use `send()` method, never write directly to channel:

```go
// CORRECT
s.send(&eventstypes.TaskCreate{
    ContainerID: id,
    // ...
})

// WRONG: Direct channel write can panic during shutdown
s.events <- event
```

The `send()` method handles the race between checking `eventsClosed` and the channel being closed.

### Connection Management

Use `connManager` for TTRPC clients:

```go
// Get cached client for task RPCs
vmc, cleanup, err := s.getTaskClient(ctx)
if err != nil {
    return nil, err
}
defer cleanup()

// Use client
tc := taskAPI.NewTTRPCTaskClient(vmc)
resp, err := tc.Start(ctx, r)
```

**Why separate clients?**
- Task client: For unary RPCs (State, Start, Delete)
- Event client: For streaming RPC (event stream)
- Mixing streaming and unary on one connection causes vsock errors

### I/O Forwarding

Two modes based on terminal setting:

```go
// Terminal mode (TTY): Passthrough via FIFOs
if rio.Terminal {
    return s.forwardIOPassthrough(ctx, vmi, rio)
}

// Non-terminal mode: RPC-based streaming
return s.forwardIOWithIDs(ctx, vmi, containerID, execID, rio)
```

The forwarder must be started AFTER the guest creates the process:
```go
// Create process in guest first
resp, err := tc.Exec(ctx, vr)
if err != nil {
    execForwarder.Shutdown(ctx)  // Cleanup on failure
    return nil, err
}

// Then start I/O forwarding
if err := execForwarder.Start(ctx); err != nil {
    log.G(ctx).WithError(err).Error("failed to start I/O forwarder")
}
```

---

## Key Files

### Core Files (understand these first)

- **`service.go`** - Main service struct, all TTRPCTaskService methods
  - `NewTaskService()`: Constructor, starts event forwarder goroutine
  - `shutdown()`: Cleanup orchestration
  - `Create()`, `Start()`, `Delete()`: Lifecycle methods

- **`io.go`** - I/O forwarding between host and guest
  - `IOForwarder` interface
  - `forwardIOWithIDs()`: RPC-based I/O for attach support
  - `forwardIOPassthrough()`: FIFO-based for TTY mode

- **`connmanager.go`** - TTRPC client connection pooling
  - Caches connections to avoid vsock dial issues
  - Handles transient ENODEV errors

### Supporting Files

- **`create.go`** (if exists) - `Create()` implementation details
- **`*_test.go`** - Unit tests

---

## Quick Search Commands

### Find in This Package

```bash
# Find all service methods
rg -n "^func \(s \*service\)" internal/shim/task/

# Find state transitions
rg -n "stateMachine\.(Try|Mark|Force)" internal/shim/task/

# Find lock acquisitions
rg -n "containerMu\.Lock|controllerMu\.Lock" internal/shim/task/

# Find error handling
rg -n "errgrpc\.ToGRPC" internal/shim/task/
```

### Find I/O Patterns

```bash
# Find forwarder usage
rg -n "IOForwarder|forwarder\." internal/shim/task/

# Find event sending
rg -n "\.send\(" internal/shim/task/
```

---

## Common Gotchas

**Lock ordering violation**:
- Problem: Deadlock when acquiring `controllerMu` before `containerMu`
- Solution: Always follow the documented lock order

**Holding locks during slow ops**:
- Problem: Blocks other goroutines waiting for locks
- Solution: Use collect-then-execute pattern (see above)

**Direct channel write**:
- Problem: Panic if channel is closed during shutdown race
- Solution: Always use `send()` method

**Starting I/O forwarder too early**:
- Problem: Guest process doesn't exist yet, RPC fails
- Solution: Start forwarder AFTER guest confirms process creation

**Connection sharing between stream and unary**:
- Problem: Vsock write errors ("no such device")
- Solution: Use separate connections for event stream vs task RPCs

---

## Testing

### Test Organization

- Unit tests: Colocated (`*_test.go`)
- Focus on state machine transitions and lock ordering

### Testing Patterns

**Testing state transitions**:
```go
func TestService_TryStartCreating_FromIdle(t *testing.T) {
    s := &service{
        stateMachine: lifecycle.NewStateMachine(),
    }

    ok := s.stateMachine.TryStartCreating()
    require.True(t, ok)
    assert.Equal(t, lifecycle.StateCreating, s.stateMachine.State())
}
```

**Testing with mocks**:
- Mock `network.NetworkManager` for network tests
- Mock `lifecycle.Manager` for VM tests

### Running Tests

```bash
# All tests
go test ./internal/shim/task/...

# With race detection
go test -race ./internal/shim/task/...

# Specific test
go test -run TestService_Create ./internal/shim/task/...
```

---

## Package-Specific Rules

### Synchronization Rules

- **MUST** follow lock ordering: `containerMu` → `controllerMu`
- **MUST NOT** hold locks during slow operations (network, VM, I/O)
- **MUST** use `send()` for event channel writes
- **MUST** check `stateMachine.IsIntentionalShutdown()` before processing

### Error Handling Rules

- **MUST** convert errors to gRPC format: `errgrpc.ToGRPC(err)`
- **MUST** wrap errors with context: `fmt.Errorf("operation: %w", err)`
- **SHOULD** log errors with structured fields before returning

### Lifecycle Rules

- **MUST** use state machine for all lifecycle transitions
- **MUST** cleanup resources on error (use defer or explicit cleanup)
- **MUST** wait for I/O forwarder before forwarding TaskExit event
