# VM Init Daemon (vminitd)

**Technology**: Go, TTRPC, runc, cgroups v2
**Entry Point**: `cmd/vminitd/main.go`, `service/service.go`
**Parent Context**: This extends [../../../../CLAUDE.md](../../../../CLAUDE.md)

---

## Quick Overview

**What this package does**:
The vminitd daemon runs as PID 1 inside the QEMU VM. It provides a TTRPC server over vsock that the host shim uses to manage containers. It wraps runc for OCI container execution and handles container lifecycle, I/O streams, and event forwarding.

**Key responsibilities**:
- Run as PID 1 and manage system initialization inside VM
- Provide TTRPC services over vsock for host communication
- Manage container lifecycle via runc
- Forward container events to host
- Handle I/O streaming between containers and host

**Dependencies**:
- Internal: `config`, `api/services/*`
- External: `containerd/go-runc`, `containerd/ttrpc`, `mdlayher/vsock`

---

## Development Commands

### From This Directory

```bash
# Run tests
go test ./...

# Run with race detection
go test -race ./...

# Build vminitd
task build:vminitd
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
vminit/
├── service/           # TTRPC service registration
│   └── service.go     # Plugin registration, server setup
├── task/              # Task service (container management)
│   ├── service.go     # TTRPCTaskService implementation
│   └── exec.go        # Exec process management
├── events/            # Event streaming
│   ├── service.go     # Event stream TTRPC service
│   └── exchange.go    # Event exchange (pub/sub)
├── streaming/         # I/O streaming
│   └── plugin.go      # Stdio streaming service
├── runc/              # Runc wrapper
│   ├── container.go   # Container abstraction
│   ├── platform.go    # Platform configuration
│   └── cgroup.go      # Cgroup management
├── bundle/            # OCI bundle management
│   └── bundle.go      # Bundle creation
├── config/            # VM configuration
│   └── config.go      # Configuration loading
├── devices/           # Device management
│   └── blockdev.go    # Block device detection
├── system/            # System initialization
│   └── mounts.go      # Mount setup
├── systools/          # System utilities
│   └── proc.go        # Proc filesystem helpers
├── process/           # Process management
│   ├── process.go     # Process abstraction
│   ├── exec.go        # Exec process
│   └── io_util.go     # I/O utilities
├── stream/            # Stream utilities
│   └── stream.go      # Stream helpers
├── types.go           # Type definitions
└── plugin_linux.go    # Linux-specific plugin
```

### Service Architecture

The vminitd provides multiple TTRPC services:

```
vsock listener (CID=2, port=1024)
    │
    ├── Task Service (TTRPCTaskService)
    │   ├── Create, Start, Delete, Kill
    │   ├── Exec, State, Wait, Pids
    │   └── Stats, Update, CloseIO
    │
    ├── Events Service
    │   └── Stream() - bidirectional event streaming
    │
    ├── Streaming Service
    │   └── I/O forwarding for container processes
    │
    └── System Info Service
        └── SystemInfo() - guest capabilities
```

---

## Code Patterns

### Process State Machine

Containers and exec processes follow a state machine:

```
CREATED → RUNNING → STOPPED → DELETED
```

```go
// Check process state before operations
if proc.State() != process.Running {
    return errdefs.ErrNotRunning
}

// State transitions are atomic
proc.SetState(process.Stopped)
```

### Exit Tracking

The exit tracker handles race conditions where processes exit before `Start()` returns:

```go
// Register process for exit tracking
tracker.Register(pid, proc)

// Process exits are captured even if Start() hasn't returned
// The exit status is stored and retrieved later
status := tracker.Wait(pid)
```

### Event Publishing

Events are published through the exchange:

```go
// Publish task exit event
s.events.Publish(ctx, &eventstypes.TaskExit{
    ContainerID: containerID,
    ID:          execID,
    Pid:         pid,
    ExitStatus:  exitStatus,
    ExitedAt:    exitedAt,
})
```

### Container Management via Runc

```go
// Create container
container, err := runc.NewContainer(ctx, runc.ContainerConfig{
    ID:         containerID,
    BundlePath: bundlePath,
    Runtime:    runcBinary,
})
if err != nil {
    return nil, fmt.Errorf("create container: %w", err)
}

// Start container
if err := container.Start(ctx); err != nil {
    return nil, fmt.Errorf("start container: %w", err)
}

// Delete container
if err := container.Delete(ctx); err != nil {
    log.G(ctx).WithError(err).Warn("delete container failed")
}
```

---

## Key Files

### Core Files (understand these first)

- **`service/service.go`** - TTRPC server setup and plugin registration
  - Initializes all services
  - Listens on vsock

- **`task/service.go`** - Task service implementation
  - Implements containerd's `TTRPCTaskService`
  - Container lifecycle management

- **`events/exchange.go`** - Event pub/sub system
  - In-memory event exchange
  - Subscriber management

### Container Management

- **`runc/container.go`** - Container abstraction over runc
  - Create, start, delete operations
  - State management

- **`process/process.go`** - Process abstraction
  - Init and exec processes
  - I/O stream management

- **`bundle/bundle.go`** - OCI bundle handling
  - Bundle creation from host
  - Spec modification

### System Setup

- **`system/mounts.go`** - Mount initialization
  - `/proc`, `/sys`, `/dev` setup
  - Cgroup v2 mounting

- **`devices/blockdev.go`** - Block device detection
  - Virtio disk discovery
  - Device node creation

---

## Quick Search Commands

### Find in This Package

```bash
# Find TTRPC service methods
rg -n "^func \(s \*service\)" internal/guest/vminit/task/

# Find event publishing
rg -n "\.Publish\(" internal/guest/vminit/

# Find runc operations
rg -n "runc\." internal/guest/vminit/

# Find state transitions
rg -n "SetState|State\(\)" internal/guest/vminit/
```

### Find Process Management

```bash
# Find process creation
rg -n "NewProcess|NewExec" internal/guest/vminit/

# Find exit handling
rg -n "ExitStatus|exitTracker" internal/guest/vminit/
```

---

## Common Gotchas

**PID 1 responsibilities**:
- Problem: vminitd is PID 1, must handle orphaned processes
- Solution: Reap zombie processes, handle SIGCHLD properly

**Early process exit**:
- Problem: Process exits before `Start()` returns
- Solution: Use exit tracker to capture early exits

**Cgroup v2 setup**:
- Problem: Cgroup controllers not available
- Solution: Mount cgroup2 filesystem, enable controllers

**Block device detection**:
- Problem: Virtio devices not immediately available
- Solution: Wait for device nodes with timeout

**Vsock connection from guest**:
- Problem: Guest CID is always 2 (VMADDR_CID_HOST connects to host)
- Solution: Listen on guest CID, host dials to VM CID

---

## Testing

### Test Organization

- Unit tests: Colocated (`*_test.go`)
- Integration requires running inside VM

### Testing Patterns

**Testing task service**:
```go
func TestTaskService_Create(t *testing.T) {
    // Setup mock runc, bundle
    svc := NewTaskService(...)

    resp, err := svc.Create(ctx, &taskAPI.CreateTaskRequest{...})

    require.NoError(t, err)
    assert.NotEmpty(t, resp.Pid)
}
```

### Running Tests

```bash
# All tests
go test ./internal/guest/vminit/...

# With race detection
go test -race ./internal/guest/vminit/...
```

---

## Package-Specific Rules

### PID 1 Rules

- **MUST** handle SIGCHLD for zombie reaping
- **MUST** forward signals to child processes appropriately
- **MUST NOT** exit on transient errors (would kill VM)

### Container Rules

- **MUST** clean up containers on deletion
- **MUST** track exit status for all processes
- **SHOULD** log container operations with container ID

### Event Rules

- **MUST** publish events for all lifecycle transitions
- **MUST NOT** block on slow event subscribers
- **SHOULD** buffer events to handle subscriber lag
