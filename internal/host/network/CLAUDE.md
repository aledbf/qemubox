# CNI Network Management

**Technology**: Go, CNI, netlink, TAP devices
**Entry Point**: `network.go`, `manager_cni.go`
**Parent Context**: This extends [../../../../CLAUDE.md](../../../../CLAUDE.md)

---

## Quick Overview

**What this package does**:
Manages network resources for VM containers using the Container Network Interface (CNI). Allocates IP addresses, creates TAP devices for VM networking, and handles cleanup. This package runs on the host side and configures networking before the VM starts.

**Key responsibilities**:
- Allocate IP addresses and network configuration via CNI
- Create and manage TAP devices for VM network interfaces
- Pass TAP file descriptors to QEMU for virtio-net
- Clean up network resources when containers are deleted
- Track metrics for network operations

**Dependencies**:
- Internal: `config`
- External: `containernetworking/cni`, `vishvananda/netlink`, `vishvananda/netns`

---

## Development Commands

### From This Directory

```bash
# Run tests
go test ./...

# Run with race detection
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
network/
├── network.go         # NetworkManager interface
├── types.go           # Type definitions
├── manager_cni.go     # CNI-based implementation
├── manager_cni_test.go
├── metrics.go         # Operation metrics
├── network_darwin.go  # Darwin stub (no CNI)
├── cni/               # CNI integration
│   ├── cni.go         # CNI plugin wrapper
│   ├── cni_test.go
│   ├── errors.go      # Error classification
│   ├── errors_test.go
│   ├── result.go      # CNI result parsing
│   ├── result_test.go
│   ├── tap.go         # TAP device management
│   └── netns.go       # Network namespace helpers
└── CLAUDE.md          # This file
```

### Key Interfaces

```go
// NetworkManager is the main API for network operations
type NetworkManager interface {
    // Allocate network resources for a container
    EnsureNetworkResources(ctx context.Context, env *Environment) error

    // Release network resources for a container
    ReleaseNetworkResources(ctx context.Context, env *Environment) error

    // Get operation metrics
    Metrics() *Metrics

    // Close the manager
    Close() error
}

// Environment holds container network state
type Environment struct {
    ID          string       // Container ID
    NetworkInfo *NetworkInfo // Allocated network (TAP, IP, etc.)
}

// NetworkInfo contains allocated network resources
type NetworkInfo struct {
    TAPName string   // TAP device name
    TAPFile *os.File // TAP device FD (for QEMU)
    MAC     string   // MAC address
    IP      net.IP   // Allocated IP
    Gateway net.IP   // Gateway IP
    // ... more fields
}
```

---

## Code Patterns

### Allocating Network Resources

```go
// Create environment for container
env := &network.Environment{
    ID: containerID,
}

// Allocate network resources
if err := networkManager.EnsureNetworkResources(ctx, env); err != nil {
    return nil, fmt.Errorf("allocate network: %w", err)
}

// env.NetworkInfo is now populated
tapFD := env.NetworkInfo.TAPFile
ipAddr := env.NetworkInfo.IP
```

### Releasing Network Resources

```go
// Release on container deletion
env := &network.Environment{ID: containerID}
if err := networkManager.ReleaseNetworkResources(ctx, env); err != nil {
    log.G(ctx).WithError(err).Warn("failed to release network")
    // Continue cleanup - network release is best-effort
}
```

### Error Classification

CNI errors are classified for appropriate handling:

```go
if err := networkManager.EnsureNetworkResources(ctx, env); err != nil {
    if errors.Is(err, cni.ErrResourceConflict) {
        // Orphaned resources from previous run - try cleanup
        log.Warn("orphaned network resources detected")
    } else if errors.Is(err, cni.ErrIPAMExhausted) {
        // No IPs available - cannot proceed
        return nil, errdefs.ErrResourceExhausted
    } else if errors.Is(err, cni.ErrIPAMLeak) {
        // Cleanup verification failed - warn but continue
        log.Warn("IP address may be leaked")
    }
    return nil, err
}
```

### TAP Device Creation

```go
// Create TAP device in namespace
tap, err := cni.CreateTAP(tapName, netns)
if err != nil {
    return nil, fmt.Errorf("create TAP: %w", err)
}

// TAP file descriptor is passed to QEMU
// QEMU opens it with virtio-net-pci device
qemuArgs = append(qemuArgs,
    "-netdev", fmt.Sprintf("tap,id=net0,fd=%d", tap.Fd()),
    "-device", "virtio-net-pci,netdev=net0,mac="+macAddr,
)
```

---

## Key Files

### Core Files (understand these first)

- **`network.go`** - `NetworkManager` interface definition
  - Public API for network operations

- **`types.go`** - Type definitions
  - `Environment`, `NetworkInfo`, `Metrics`

- **`manager_cni.go`** - CNI-based implementation
  - Main implementation of `NetworkManager`
  - Thread-safe with internal locking

- **`metrics.go`** - Operation metrics
  - Tracks setup/teardown counts and durations

### CNI Integration

- **`cni/cni.go`** - CNI plugin wrapper
  - Wraps libcni for CNI operations
  - Handles CNI config discovery

- **`cni/errors.go`** - Error classification
  - `ErrResourceConflict`, `ErrIPAMExhausted`, `ErrIPAMLeak`
  - Classifies CNI errors for handling

- **`cni/result.go`** - CNI result parsing
  - Extracts IP, gateway, routes from CNI result

- **`cni/tap.go`** - TAP device management
  - Creates TAP devices for VM networking
  - Manages TAP in network namespaces

- **`cni/netns.go`** - Network namespace helpers
  - Enter/exit network namespaces
  - Namespace file descriptor management

---

## Quick Search Commands

### Find in This Package

```bash
# Find NetworkManager methods
rg -n "func \(.*NetworkManager\)" internal/host/network/

# Find error classification
rg -n "ErrResource|ErrIPAM" internal/host/network/

# Find TAP operations
rg -n "TAP|tap" internal/host/network/

# Find CNI operations
rg -n "CNI|cni\." internal/host/network/
```

### Find Cleanup Logic

```bash
# Find release/cleanup
rg -n "Release|Cleanup|Delete" internal/host/network/

# Find error handling in cleanup
rg -n "defer|cleanup" internal/host/network/
```

---

## Common Gotchas

**TAP device persistence**:
- Problem: TAP device must stay open for QEMU
- Solution: Pass FD to QEMU, don't close until VM exits

**Network namespace handling**:
- Problem: Operations must run in correct namespace
- Solution: Use `netns.Do()` to execute in namespace context

**CNI plugin discovery**:
- Problem: CNI config not found
- Solution: Check `/etc/cni/net.d/*.conflist` exists

**IP address leaks**:
- Problem: IPs not released on unclean shutdown
- Solution: Track allocations, implement cleanup on startup

**Concurrent access**:
- Problem: Multiple containers allocating simultaneously
- Solution: Internal locking in manager implementation

**Platform-specific code**:
- Problem: CNI only works on Linux
- Solution: `network_darwin.go` provides stub for macOS

---

## Testing

### Test Organization

- Unit tests: Colocated (`*_test.go`)
- Integration tests require root and CNI plugins

### Testing Patterns

**Testing CNI result parsing**:
```go
func TestParseResult(t *testing.T) {
    result := &cni.Result{...}
    info, err := ParseResult(result)
    require.NoError(t, err)
    assert.Equal(t, expectedIP, info.IP)
}
```

**Testing error classification**:
```go
func TestClassifyError(t *testing.T) {
    err := &cni.Error{Code: 11, Msg: "IP exhausted"}
    assert.True(t, errors.Is(ClassifyError(err), ErrIPAMExhausted))
}
```

### Running Tests

```bash
# All tests
go test ./internal/host/network/...

# With race detection
go test -race ./internal/host/network/...

# Run CNI tests (requires root)
sudo go test ./internal/host/network/cni/...
```

---

## Package-Specific Rules

### Resource Management Rules

- **MUST** release network resources on container deletion
- **MUST** handle cleanup failures gracefully (log, don't fail)
- **MUST NOT** close TAP FD until QEMU exits
- **SHOULD** track metrics for debugging

### CNI Rules

- **MUST** call CNI DEL even if container didn't start properly
- **MUST** use correct network namespace for operations
- **SHOULD** classify CNI errors for appropriate handling

### Concurrency Rules

- **MUST** use internal locking for thread safety
- **MUST NOT** hold locks during slow CNI operations
- **SHOULD** use context for cancellation

### Error Handling Rules

- **MUST** classify errors using `errors.Is()`
- **MUST** wrap errors with context
- **SHOULD** log cleanup failures but continue cleanup
