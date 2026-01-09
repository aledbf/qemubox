# QEMU VM Management

**Technology**: Go, QEMU/KVM, QMP, vsock
**Entry Point**: `qemu.go`, `instance.go`
**Parent Context**: This extends [../../../../CLAUDE.md](../../../../CLAUDE.md)

---

## Quick Overview

**What this package does**:
Manages QEMU virtual machine lifecycle including creation, startup, device configuration (disks, NICs), communication via QMP (QEMU Machine Protocol), and graceful shutdown. Each container runs in its own dedicated VM instance.

**Key responsibilities**:
- Create and configure QEMU instances
- Manage VM state machine (New → Starting → Running → Shutdown)
- Communicate with QEMU via QMP for hotplug and queries
- Handle vsock connections for host↔guest TTRPC communication
- Implement graceful shutdown with SIGTERM/SIGKILL fallback

**Dependencies**:
- Internal: `config`, `vsock`
- External: `digitalocean/go-qemu` (QMP client), `mdlayher/vsock`

---

## Development Commands

### From This Directory

```bash
# Run tests
go test ./...

# Run with race detection
go test -race ./...

# Run specific test
go test -run TestQEMU_Start ./...
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
qemu/
├── qemu.go              # Instance struct and state machine
├── instance.go          # Instance interface implementation
├── start.go             # VM startup sequence
├── shutdown.go          # Graceful shutdown logic
├── client.go            # TTRPC/vsock client setup
├── devices.go           # Virtio disk/NIC device config
├── qemu_command.go      # QEMU command line builder
├── kernel_cmdline.go    # Kernel boot parameters
├── qmp_client.go        # QMP connection management
├── qmp_cpu.go           # CPU hotplug via QMP
├── qmp_memory.go        # Memory hotplug via QMP
├── qmp_events.go        # QMP event handling
├── streaming.go         # Vsock stream management
├── types.go             # Type definitions
├── utils.go             # Utility functions
└── *_test.go            # Unit tests
```

### VM State Machine

```
vmStateNew → vmStateStarting → vmStateRunning → vmStateShutdown
     │              │                │
     └──────────────┴────────────────┴─── (error) → vmStateShutdown
```

State transitions are atomic and enforced:

```go
// Only valid transition: New → Starting
func (i *Instance) transitionToStarting() error {
    if !i.state.CompareAndSwap(vmStateNew, vmStateStarting) {
        return fmt.Errorf("invalid state transition: expected %d, got %d",
            vmStateNew, i.state.Load())
    }
    return nil
}
```

---

## Code Patterns

### Instance Interface

The `Instance` implements multiple interfaces:

```go
type Instance interface {
    DeviceConfigurator  // AddDisk(), AddTAPNIC(), AddNIC()
    GuestCommunicator   // Client(), DialClient(), StartStream()
    ResourceManager     // CPUHotplugger()
    Start(ctx context.Context, opts StartOptions) error
    Shutdown(ctx context.Context) error
    VMInfo() VMInfo
}
```

### Creating a QEMU Instance

```go
// Create instance with configuration
instance, err := qemu.NewInstance(ctx, qemu.InstanceConfig{
    ID:            containerID,
    BundlePath:    bundlePath,
    ResourceConfig: resourceConfig,
    CID:           vsockCID,
    LogDir:        logDir,
})
if err != nil {
    return nil, fmt.Errorf("create VM instance: %w", err)
}

// Add devices before starting
if err := instance.AddDisk(disk); err != nil {
    return nil, fmt.Errorf("add disk: %w", err)
}
if err := instance.AddTAPNIC(nic); err != nil {
    return nil, fmt.Errorf("add NIC: %w", err)
}

// Start the VM
if err := instance.Start(ctx, startOpts); err != nil {
    return nil, fmt.Errorf("start VM: %w", err)
}
```

### QMP Communication

QMP is used for runtime queries and hotplug operations:

```go
// Query current CPU count
cpuInfo, err := instance.QueryCPUs(ctx)
if err != nil {
    return fmt.Errorf("query CPUs: %w", err)
}

// Hotplug a CPU
if err := instance.CPUHotplug(ctx, socketID); err != nil {
    return fmt.Errorf("hotplug CPU: %w", err)
}

// Query memory
memInfo, err := instance.QueryMemorySizeSummary(ctx)
if err != nil {
    return fmt.Errorf("query memory: %w", err)
}
```

### Vsock Connection Pattern

```go
// Get cached TTRPC client (for event stream)
client, err := instance.Client()
if err != nil {
    return nil, err
}

// Dial new connection (for task RPCs)
client, err := instance.DialClient(ctx)
if err != nil {
    return nil, err
}
defer client.Close()

// Dial with retry (handles transient vsock issues)
client, err := instance.DialClientWithRetry(ctx, timeout)
if err != nil {
    return nil, err
}
```

### Graceful Shutdown

```go
// Shutdown sends SIGTERM, waits, then SIGKILL if needed
func (i *Instance) Shutdown(ctx context.Context) error {
    // 1. Transition to shutdown state
    i.transitionToShutdown()

    // 2. Send SIGTERM to QEMU process
    if err := i.process.Signal(syscall.SIGTERM); err != nil {
        // Process may already be dead
    }

    // 3. Wait for graceful exit with timeout
    select {
    case <-i.waitCh:
        return nil  // Clean exit
    case <-time.After(gracePeriod):
        // 4. Force kill
        i.process.Kill()
        <-i.waitCh
    }
    return nil
}
```

---

## Key Files

### Core Files (understand these first)

- **`qemu.go`** - Instance struct definition and state management
  - State machine implementation
  - Resource configuration

- **`instance.go`** - Interface implementations
  - `VMInfo()`, device management

- **`start.go`** - VM startup sequence
  - QEMU process launch
  - Wait for guest readiness

- **`shutdown.go`** - Graceful shutdown
  - SIGTERM → wait → SIGKILL sequence
  - Resource cleanup

### QMP Files

- **`qmp_client.go`** - QMP connection management
  - Unix socket communication with QEMU

- **`qmp_cpu.go`** - CPU hotplug operations
  - `QueryCPUs()`, `CPUHotplug()`, `CPUHotunplug()`

- **`qmp_memory.go`** - Memory hotplug operations
  - `QueryMemorySizeSummary()`, memory device management

- **`qmp_events.go`** - QMP event handling
  - Guest shutdown events, device events

### Configuration Files

- **`qemu_command.go`** - Command line builder
  - Constructs QEMU invocation with all flags

- **`kernel_cmdline.go`** - Kernel boot parameters
  - Console, root device, init parameters

- **`devices.go`** - Device configuration
  - Virtio-blk disks, virtio-net NICs

### Communication Files

- **`client.go`** - TTRPC client over vsock
  - Cached client management
  - Connection retry logic

- **`streaming.go`** - Vsock stream handling
  - Direct vsock connections for I/O

---

## Quick Search Commands

### Find in This Package

```bash
# Find state transitions
rg -n "transitionTo|vmState" internal/host/vm/qemu/

# Find QMP commands
rg -n "QMP|qmp_" internal/host/vm/qemu/

# Find vsock usage
rg -n "vsock|CID" internal/host/vm/qemu/

# Find device configuration
rg -n "AddDisk|AddNIC|AddTAP" internal/host/vm/qemu/
```

### Find QEMU Command Building

```bash
# Find command line construction
rg -n "buildCommand|args\s*=" internal/host/vm/qemu/

# Find kernel cmdline
rg -n "kernelCmdline|append" internal/host/vm/qemu/
```

---

## Common Gotchas

**State transition race**:
- Problem: Multiple goroutines trying to transition state
- Solution: Use `CompareAndSwap` for atomic transitions

**QMP socket not ready**:
- Problem: QEMU takes time to create QMP socket after process start
- Solution: Retry connection with backoff

**Vsock CID reuse**:
- Problem: Reusing CID too quickly causes connection issues
- Solution: Use CID allocator with cooldown period (see `vsock` package)

**Graceful shutdown timeout**:
- Problem: Guest doesn't respond to SIGTERM
- Solution: Always have SIGKILL fallback after grace period

**Platform-specific code**:
- Problem: Some features only work on Linux
- Solution: Check `*_darwin.go` and `*_linux.go` for platform differences

---

## Testing

### Test Organization

- Unit tests: Colocated (`*_test.go`)
- Integration tests require KVM: Run with `-tags=integration`

### Testing Patterns

**Testing command building**:
```go
func TestBuildCommand(t *testing.T) {
    cfg := InstanceConfig{...}
    args := buildCommand(cfg)

    assert.Contains(t, args, "-machine")
    assert.Contains(t, args, "q35")
}
```

**Testing shutdown sequence**:
```go
func TestShutdown_Graceful(t *testing.T) {
    // Setup mock process
    // Call Shutdown
    // Verify SIGTERM sent first
    // Verify clean exit
}
```

### Running Tests

```bash
# All tests
go test ./internal/host/vm/qemu/...

# With race detection
go test -race ./internal/host/vm/qemu/...

# Specific test
go test -run TestInstance_Start ./internal/host/vm/qemu/...
```

---

## Package-Specific Rules

### State Management Rules

- **MUST** use atomic operations for state transitions
- **MUST** check current state before transitioning
- **MUST NOT** skip states (New → Running is invalid)

### QMP Rules

- **MUST** handle QMP connection failures gracefully
- **SHOULD** use timeouts for all QMP commands
- **SHOULD** log QMP errors with command context

### Shutdown Rules

- **MUST** always have SIGKILL fallback
- **MUST** wait for process exit after SIGKILL
- **SHOULD** give reasonable grace period (config: `shutdown_grace`)

### Device Rules

- **MUST** add devices before `Start()`
- **MUST NOT** add devices after VM is running (use hotplug instead)
- **SHOULD** validate device configuration before adding
