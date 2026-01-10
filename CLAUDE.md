# spinbox

**CLAUDE.md Version**: 1.0.0
**Last Updated**: 2025-01-09
**Last Review**: @claude

---

## Overview

- **Type**: Containerd shim runtime
- **Primary Language**: Go 1.25
- **Architecture**: Host shim + Guest daemon (vminitd) communicating over vsock/TTRPC
- **Tech Stack**: containerd, QEMU/KVM, CNI networking, TTRPC, protobuf

---

## Purpose of This File

This CLAUDE.md is the **authoritative source** for development guidelines. It is read by Claude Code before every interaction and takes priority over user prompts.

**Hierarchy**:
- This file contains universal project rules
- Subdirectories contain specialized CLAUDE.md files that extend these rules
- See [Directory-Specific Context](#directory-specific-context) for links

**Maintenance**: Update this file as patterns evolve. Use `#` during sessions to add memories organically.

---

## Universal Development Rules

### Code Quality [MUST]

- **MUST** check all errors - no `_ = err` without justification comment
- **MUST** use `context.Context` for cancellation and timeouts in all blocking operations
- **MUST** run `task lint` before committing (enforces 50+ linter rules)
- **MUST** include tests for new functionality (target: >80% coverage)
- **MUST NOT** commit secrets, API keys, or credentials
- **MUST NOT** edit generated files (`*.pb.go`, `*_ttrpc.pb.go`) - regenerate instead

### Go Best Practices [SHOULD]

- **SHOULD** use structured logging via `containerd/log` package
- **SHOULD** prefer interfaces for dependencies (easier testing)
- **SHOULD** use `errors.Is()` and `errors.As()` for error checking
- **SHOULD** wrap errors with context: `fmt.Errorf("operation: %w", err)`
- **SHOULD** use `sync.Mutex` with clear lock ordering to prevent deadlocks
- **SHOULD** document lock ordering in package comments (see `internal/shim/task/service.go`)

### Anti-Patterns [MUST NOT]

- **MUST NOT** ignore errors: `_, err := doThing(); return nil` is forbidden
- **MUST NOT** use `panic()` for error handling (use error returns)
- **MUST NOT** use `unsafe` package without explaining why in comment
- **MUST NOT** hold locks during slow operations (network calls, VM operations)
- **MUST NOT** mutate global state without synchronization

### Concurrency Patterns [CRITICAL]

This project uses sophisticated concurrency. Follow these patterns:

- **State machines** for lifecycle management (see `internal/shim/lifecycle/state.go`)
- **Collect-then-execute** pattern: gather resources under lock, release lock, then operate
- **Atomics** for lock-free counters and flags (`sync/atomic`)
- **Channel ownership**: document producers and consumers in comments

---

## Core Development Commands

### Quick Reference

```bash
# Development
task build:shim          # Build containerd shim (static, Linux x86_64)
task build:vminitd       # Build guest init daemon
task build:initrd        # Build initrd image (requires Docker)
task build:kernel        # Build VM kernel (requires Docker)
task build               # Build all components

# Testing
go test ./...                                    # Unit tests
go test -v -race ./...                           # Unit tests with race detection
go test -v -timeout 10m -tags=integration ./integration/...  # Integration (requires KVM)

# Validation
task lint                # Run golangci-lint (50+ linters)
task validate            # Run all validation checks
task verify:vendor       # Verify go.mod/go.sum are up to date

# Code Generation
task protos              # Generate protobuf files
task check:protos        # Verify protobufs are up to date

# Cleanup
task clean               # Clean build artifacts
```

### Pre-Commit Checklist [CRITICAL]

Run these commands before every commit:

```bash
go fmt ./... && \
goimports -w . && \
task lint && \
go test -race ./...
```

All checks must pass before committing.

---

## Project Structure

### Root Layout

```
.
├── cmd/                           # Application entry points
│   ├── containerd-shim-spinbox-v1/  # Host: containerd shim
│   └── vminitd/                     # Guest: PID 1 inside VM
├── internal/                      # Private application code
│   ├── shim/                        # Shim-side implementation
│   │   ├── task/                    # Task service (Create, Start, Delete)
│   │   ├── lifecycle/               # State machine and cleanup
│   │   ├── cpuhotplug/              # CPU hotplug controller
│   │   └── memhotplug/              # Memory hotplug controller
│   ├── guest/                       # Guest-side implementation
│   │   └── vminit/                  # VM init daemon
│   ├── host/                        # Host-side services
│   │   ├── vm/qemu/                 # QEMU VM lifecycle
│   │   └── network/                 # CNI networking
│   └── config/                      # Configuration loading
├── api/                           # Protobuf definitions
│   └── services/                    # TTRPC service definitions
├── integration/                   # Integration tests
├── docs/                          # Documentation
└── examples/                      # Example configurations
```

### Key Directories

**Applications** (`cmd/`):
- **`cmd/containerd-shim-spinbox-v1/`** - Shim entry point
  - Loads config from `/etc/spinbox/config.json` or `SPINBOX_CONFIG` env var
  - Registers with containerd's shim manager

- **`cmd/vminitd/`** - Guest init daemon
  - Runs as PID 1 inside VM
  - TTRPC server over vsock

**Shim Components** (`internal/shim/`):
- **`internal/shim/task/`** - Core task service ([see CLAUDE.md](internal/shim/task/CLAUDE.md))
  - Implements containerd's `TTRPCTaskService` interface
  - Orchestrates VM lifecycle, container creation, I/O streams

- **`internal/shim/lifecycle/`** - State management ([see CLAUDE.md](internal/shim/lifecycle/CLAUDE.md))
  - State machine: Idle → Creating → Running → Deleting → ShuttingDown
  - Cleanup orchestrator with ordered phases

**Guest Components** (`internal/guest/`):
- **`internal/guest/vminit/`** - Guest daemon ([see CLAUDE.md](internal/guest/vminit/CLAUDE.md))
  - Task service, event streaming, runc container management

**Host Services** (`internal/host/`):
- **`internal/host/vm/qemu/`** - QEMU management ([see CLAUDE.md](internal/host/vm/qemu/CLAUDE.md))
  - VM lifecycle, QMP client, device configuration

- **`internal/host/network/`** - Networking ([see CLAUDE.md](internal/host/network/CLAUDE.md))
  - CNI integration, TAP device management

**API Layer** (`api/`):
- **`api/services/`** - Protobuf definitions ([see CLAUDE.md](api/CLAUDE.md))
  - bundle/v1: OCI bundle creation
  - stdio/v1: I/O streaming
  - vmevents/v1: Event forwarding
  - system/v1: System info queries

---

## Quick Find Commands

### Code Navigation

**Find function/type definitions**:
```bash
rg -n "^func \w+" internal/
rg -n "^type \w+ struct" internal/
```

**Find interface implementations**:
```bash
# Find all TTRPCTaskService implementations
rg -n "TTRPCTaskService" internal/

# Find state machine usage
rg -n "StateMachine" internal/
```

**Find by file type**:
```bash
# Find all test files
find . -name "*_test.go" | grep -v vendor

# Find all proto files
find api/ -name "*.proto"

# Find platform-specific files
find . -name "*_linux.go" -o -name "*_darwin.go"
```

### Dependency Analysis

```bash
# Why is a package included?
go mod why github.com/containerd/containerd/v2

# List all dependencies
go list -m all

# Find outdated dependencies
go list -u -m all
```

### Generated Code

```bash
# Find all generated files
rg -l "Code generated.*DO NOT EDIT" .

# Find proto-generated files
find api/ -name "*.pb.go"
```

---

## Security Guidelines

### Secrets Management [MUST]

- **NEVER** commit tokens, API keys, passwords, or credentials
- **NEVER** log sensitive data (tokens, passwords)
- **NEVER** hardcode secrets in code

**Correct approach**:
- Store secrets in environment variables
- Use config files with proper permissions (not committed to git)

### VM Isolation [CRITICAL]

VM isolation is the **primary security boundary** in spinbox:
- Containers run inside QEMU/KVM VMs
- Network namespaces are NOT used for container isolation
- Each container gets its own VM

**Changes affecting isolation boundaries require security review.**

### Safe Operations

**Before running destructive commands, I will**:
1. Confirm with you explicitly
2. Verify what will be affected

**Commands requiring confirmation**:
- `git push --force`
- `rm -rf` operations on directories
- Database operations (if any)

---

## Git Workflow

### Branching Strategy

- **main**: Protected, production-ready code
- **feature/[name]**: Feature branches
- **fix/[name]**: Bug fix branches

### Commit Messages

Use [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <description>

[optional body]

Co-Authored-By: Claude <noreply@anthropic.com>
```

**Types**: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`

**Scopes**: `shim`, `guest`, `vm`, `network`, `config`, `api`, `lifecycle`

**Examples**:
```bash
feat(shim): add CPU hotplug controller
fix(network): resolve TAP device cleanup race
refactor(lifecycle): extract cleanup orchestrator
test(vm): add QEMU shutdown integration tests
```

### Pull Request Process

1. Create feature branch from main
2. Make changes and commit
3. Ensure CI passes (tests, lint, build)
4. Create PR with clear description
5. Address review feedback
6. Squash and merge

---

## Testing Requirements

### Test Types

**Unit Tests** (required for business logic):
- Location: Colocated with source (`*_test.go`)
- Framework: Go `testing` + `testify`
- Coverage target: >80%

**Integration Tests** (required for VM/network operations):
- Location: `integration/`
- Requirements: KVM support, root privileges
- Run with: `go test -v -timeout 10m -tags=integration ./integration/...`

### Writing Tests

**Test structure** (arrange-act-assert):
```go
func TestSomething(t *testing.T) {
    // Arrange
    input := setupTestData()

    // Act
    result, err := functionUnderTest(input)

    // Assert
    require.NoError(t, err)
    assert.Equal(t, expected, result)
}
```

**Good test characteristics**:
- Tests one thing clearly
- Has descriptive name: `TestStateMachine_TryStartCreating_FromIdle`
- Uses `t.Parallel()` when safe
- Cleans up resources with `t.Cleanup()`

### Testing Commands

```bash
# Run all tests
go test ./...

# Run specific package
go test ./internal/shim/lifecycle/...

# Run with coverage
go test -coverprofile=coverage.txt ./...

# Run with race detection
go test -race ./...
```

---

## Available Tools

### Standard Tools

- **Go toolchain**: go 1.25+
- **Task runner**: task (Taskfile.yml)
- **Linter**: golangci-lint
- **Protobuf**: protobuild (containerd's proto generator)

### Code Generation Tools

- **Protobuf**: `task protos` generates from `api/**/*.proto`
- Generated files: `*.pb.go`, `*_ttrpc.pb.go`

### Tool Permissions

**Allowed Operations**:
- Read any file in the repository
- Write Go code files (except generated)
- Run tests, linters, formatters
- Build the project locally
- Create git branches and commits

**Require Explicit Permission**:
- Edit generated files (regenerate instead)
- Force push to git
- Run integration tests (require KVM)

**Never Allowed** (blocked by hooks):
- `rm -rf /` or `rm -rf .`
- Commit secrets or credentials
- Push directly to main/master

---

## Directory-Specific Context

This root CLAUDE.md provides universal rules. For work in specific directories, refer to their specialized CLAUDE.md files:

### Shim Components

- **[`internal/shim/task/CLAUDE.md`](internal/shim/task/CLAUDE.md)** - Task service implementation
  - Containerd integration, VM lifecycle, I/O forwarding
  - Lock ordering, state machine usage

- **[`internal/shim/lifecycle/CLAUDE.md`](internal/shim/lifecycle/CLAUDE.md)** - Lifecycle management
  - State machine patterns, cleanup orchestration
  - Atomic operations, goroutine coordination

### Host Services

- **[`internal/host/vm/qemu/CLAUDE.md`](internal/host/vm/qemu/CLAUDE.md)** - QEMU management
  - VM lifecycle, QMP protocol, device hotplug
  - Vsock communication, shutdown sequences

- **[`internal/host/network/CLAUDE.md`](internal/host/network/CLAUDE.md)** - Networking
  - CNI integration, TAP devices, IP allocation
  - Error classification, cleanup patterns

### Guest Components

- **[`internal/guest/vminit/CLAUDE.md`](internal/guest/vminit/CLAUDE.md)** - Guest daemon
  - TTRPC services, runc integration
  - Container lifecycle inside VM

### API Layer

- **[`api/CLAUDE.md`](api/CLAUDE.md)** - Protobuf definitions
  - Service definitions, code generation
  - Breaking change management

**These files extend (not replace) this root CLAUDE.md.**

---

## Configuration

### Required Configuration

Config file: `/etc/spinbox/config.json` (or `SPINBOX_CONFIG` env var)

See `examples/config.json` for reference configuration.

### Key Configuration Sections

- **paths**: Filesystem paths for binaries, state, logs
- **runtime**: VMM backend (currently only "qemu")
- **timeouts**: Various lifecycle operation timeouts
- **cpu_hotplug**: CPU scaling thresholds and intervals
- **memory_hotplug**: Memory scaling configuration

---

## CLAUDE.md Maintenance

### Versioning

This file follows semantic versioning:
- **Major** (X.0.0): Fundamental architecture changes
- **Minor** (0.X.0): New sections, significant additions
- **Patch** (0.0.X): Small corrections, updates

Current version: **1.0.0** (Initial creation)

### Changelog

**v1.0.0** (2025-01-09)
- Initial CLAUDE.md creation
- Defined universal development rules
- Mapped project structure
- Created subdirectory CLAUDE.md files

### Keeping Current

**During Development**:
Use `#` to add memories during Claude sessions.

**On Major Changes**:
- New package added → Consider CLAUDE.md
- Architecture change → Update structure maps
- New conventions → Update rules
