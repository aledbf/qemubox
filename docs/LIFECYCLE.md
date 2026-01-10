# VM and Container Lifecycle

This document describes the lifecycle management of QEMU VMs and containers in spinbox.
Understanding these states and transitions is essential for debugging and development.

## State Machine

The shim maintains a single state machine that tracks the overall lifecycle:

```
                    ┌─────────────────────────────────────────┐
                    │                                         │
                    ▼                                         │
┌──────┐       ┌──────────┐       ┌─────────┐       ┌─────────┴───┐
│ Idle │──────▶│ Creating │──────▶│ Running │──────▶│ ShuttingDown│
└──────┘       └──────────┘       └─────────┘       └─────────────┘
    ▲               │                   │                   ▲
    │               │                   │                   │
    │   (failure)   │                   ▼                   │
    └───────────────┘             ┌──────────┐              │
                                  │ Deleting │──────────────┘
                                  └──────────┘
```

### State Descriptions

| State | Description |
|-------|-------------|
| **Idle** | Initial state. No container exists. Ready to accept Create(). |
| **Creating** | Create() is in progress. VM is starting, network is being configured. |
| **Running** | Container is running inside the VM. Normal operational state. |
| **Deleting** | Delete() is in progress. Container is being removed, cleanup starting. |
| **ShuttingDown** | Terminal state. All resources are being released. Shim will exit. |

### Valid Transitions

| From | To | Trigger |
|------|-----|---------|
| Idle | Creating | Create() called |
| Creating | Running | Container created successfully |
| Creating | Idle | Container creation failed |
| Running | Deleting | Delete() called |
| Running | ShuttingDown | VM crash, shutdown RPC, or signal |
| Deleting | ShuttingDown | Deletion completed |
| Any | ShuttingDown | Forced shutdown (crash, timeout) |

## Component Lifecycles

### VM Instance Lifecycle

```
┌───────────────────────────────────────────────────────────────────┐
│                        VM Instance                                │
├───────────────────────────────────────────────────────────────────┤
│                                                                   │
│   vmStateNew ──▶ vmStateStarting ──▶ vmStateRunning ──▶ vmStateShutdown
│       │                                    │                      │
│       │                                    │                      │
│       ▼                                    ▼                      │
│   (AddDisk,                           (QMP commands,              │
│    AddNet)                             vsock I/O)                 │
│                                                                   │
└───────────────────────────────────────────────────────────────────┘
```

**Key Points:**
- Disks and networks must be added before Start()
- QMP commands only work in Running state
- Shutdown is idempotent - safe to call multiple times

### Network Lifecycle

```
┌─────────────────────────────────────────────────────────────────────┐
│                        Network Resources                            │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  Setup() ─────────────────────────────────────────▶ ReleaseNetworkResources()
│     │                                                        │      │
│     ▼                                                        ▼      │
│  ┌─────────────┐  ┌──────────────┐  ┌─────────────┐  ┌──────────┐  │
│  │ CNI Add     │─▶│ IP Allocated │─▶│ TAP Created │─▶│ Firewall │  │
│  │ (bridge)    │  │ (host-local) │  │ (tc-tap)    │  │ (nft)    │  │
│  └─────────────┘  └──────────────┘  └─────────────┘  └──────────┘  │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

**Key Points:**
- Network setup creates: bridge (if needed), IP allocation, TAP device, firewall rules
- Network cleanup removes: firewall rules, TAP device, releases IP
- Must be cleaned up AFTER VM shutdown (QEMU releases TAP FDs)

### Mount/Storage Lifecycle

```
┌─────────────────────────────────────────────────────────────────────┐
│                        Mount Resources                              │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  OCI Mounts ──▶ Transform ──▶ virtio-blk devices ──▶ Guest mounts  │
│                                                                     │
│  EROFS snapshots are exposed as read-only block devices             │
│  ext4 layers provide writable overlay                               │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

**Key Points:**
- Mounts are transformed to virtio-blk devices before VM start
- Guest detects devices via `/sys/block` and mounts them
- Cleanup happens after VM shutdown

## Cleanup Sequence

Cleanup follows a strict dependency order. Each phase must complete before the next begins:

```
┌─────────────┐     ┌────────────┐     ┌────────────┐     ┌─────────────┐
│   Hotplug   │────▶│     I/O    │────▶│ Connection │────▶│     VM      │
│    Stop     │     │  Shutdown  │     │   Close    │     │  Shutdown   │
└─────────────┘     └────────────┘     └────────────┘     └─────────────┘
                                                                 │
                                                                 ▼
                    ┌────────────┐     ┌────────────┐     ┌─────────────┐
                    │   Event    │◀────│   Mount    │◀────│   Network   │
                    │   Close    │     │  Cleanup   │     │   Cleanup   │
                    └────────────┘     └────────────┘     └─────────────┘
```

### Phase Details

| Phase | Purpose | Dependencies |
|-------|---------|--------------|
| **Hotplug Stop** | Stop CPU/memory scaling controllers | None |
| **I/O Shutdown** | Close stdin, flush stdout/stderr | Hotplug stopped |
| **Connection Close** | Close TTRPC and vsock connections | I/O complete |
| **VM Shutdown** | Send ACPI shutdown, then SIGKILL | Connections closed |
| **Network Cleanup** | Release CNI resources (IP, TAP, firewall) | VM exited |
| **Mount Cleanup** | Release mount manager state | VM exited |
| **Event Close** | Close event channel, signal forwarder exit | All cleanup done |

### Cleanup Error Handling

- All phases are attempted even if earlier phases fail
- Errors are collected in `ShutdownResult`
- Each phase is idempotent (safe to call multiple times)
- Failed phases are logged with specific phase identification

## Timeout Configuration

All lifecycle timeouts are configurable in `/etc/spinbox/config.json`:

```json
{
  "timeouts": {
    "vm_start": "10s",
    "device_detection": "5s",
    "shutdown_grace": "2s",
    "event_reconnect": "2s",
    "task_client_retry": "1s",
    "io_wait": "30s",
    "qmp_command": "5s"
  }
}
```

### Timeout Reference

| Timeout | Default | Description |
|---------|---------|-------------|
| `vm_start` | 10s | VM boot: QEMU exec to vsock connection |
| `device_detection` | 5s | Guest block device detection |
| `shutdown_grace` | 2s | Wait for guest OS shutdown before SIGKILL |
| `event_reconnect` | 2s | Event stream reconnection attempts |
| `task_client_retry` | 1s | Task RPC vsock dial retries |
| `io_wait` | 30s | Wait for I/O completion on exit |
| `qmp_command` | 5s | QEMU QMP command timeout |

### Tuning Guidelines

- **Slow storage**: Increase `vm_start` and `device_detection`
- **Large outputs**: Increase `io_wait`
- **Slow shutdown handlers**: Increase `shutdown_grace`
- **Unstable vsock**: Increase `task_client_retry` and `event_reconnect`

## Error Types

The lifecycle package defines sentinel errors for common failure modes:

```go
// Check for specific error types
if errors.Is(err, lifecycle.ErrVMNotRunning) {
    // Handle VM not running
}

// Inspect shutdown errors
if shutdownErr, ok := err.(*lifecycle.ShutdownError); ok {
    fmt.Printf("Failed phase: %s\n", shutdownErr.Phase)
}
```

### Sentinel Errors

| Error | Description |
|-------|-------------|
| `ErrVMNotRunning` | Operation requires running VM |
| `ErrVMAlreadyExists` | VM creation attempted when one exists |
| `ErrVMNotCreated` | Operation requires VM but none exists |
| `ErrDeviceTimeout` | Device detection timed out |
| `ErrCIDExhausted` | No vsock CIDs available |
| `ErrNetworkSetupFailed` | CNI setup failed |
| `ErrMountSetupFailed` | Mount transformation failed |
| `ErrInvalidStateTransition` | Invalid state machine transition |
| `ErrShutdownInProgress` | Shutdown already in progress |
| `ErrCreationInProgress` | Container creation already in progress |
| `ErrDeletionInProgress` | Container deletion already in progress |
| `ErrCleanupIncomplete` | Cleanup did not fully complete |

## Debugging Lifecycle Issues

### Identifying Current State

Check logs for state transitions:

```bash
journalctl -u containerd | grep "state transition"
```

### Common Issues

#### "container creation already in progress"
- Cause: Concurrent Create() calls
- Fix: Wait for first Create() to complete or fail

#### "VM not running"
- Cause: Operation attempted after VM exit
- Check: VM logs at `/var/log/spinbox/vm-*.log`

#### "device detection timeout"
- Cause: Slow disk I/O or many devices
- Fix: Increase `device_detection` timeout in config

#### "shutdown completed with errors"
- Check: Which phase failed (`failed_phases` in log)
- Network cleanup failures often indicate orphaned CNI state
- Run `./deploy/cleanup-cni.sh` to clean orphaned resources

### Cleanup After Crash

If the shim crashes during operation:

```bash
# Check for orphaned resources
./deploy/cleanup-cni.sh --status

# Clean orphaned CNI allocations
sudo ./deploy/cleanup-cni.sh

# Restart containerd
systemctl restart spinbox-containerd
```

## Implementation Files

| File | Description |
|------|-------------|
| `internal/shim/lifecycle/state.go` | State machine implementation |
| `internal/shim/lifecycle/cleanup.go` | Cleanup orchestrator |
| `internal/shim/lifecycle/errors.go` | Error types and sentinel errors |
| `internal/shim/lifecycle/vm.go` | VM lifecycle manager |
| `internal/shim/task/service.go` | Service implementation using lifecycle |
| `internal/host/vm/qemu/shutdown.go` | QEMU shutdown sequence |

## Concurrency Model

The shim uses a combination of mutexes and atomics for thread safety:

### Lock Hierarchy

Locks must be acquired in this order to prevent deadlocks:

1. `containerMu` - Protects container and containerID
2. `controllerMu` - Protects hotplug controller maps

### Rules

- Never hold both locks simultaneously unless absolutely necessary
- Never hold locks during slow operations (VM start, network setup, RPCs)
- State machine uses atomics for lock-free state checks

### Goroutine Ownership

| Goroutine | Responsibility | Lifetime |
|-----------|----------------|----------|
| Main | TTRPC service calls | Shim process |
| Event forwarder | Forward events to containerd | Until events channel closes |
| CPU hotplug | Monitor and scale CPUs | Create() to shutdown() |
| Memory hotplug | Monitor and scale memory | Create() to shutdown() |
| Console streamer | Stream QEMU console to log | VM start to shutdown |

## VM Startup Sequence

Detailed sequence of operations during Create():

```
1. Validate inputs (container ID, bundle path)
2. Check KVM availability
3. Load and transform OCI bundle
4. Compute VM resource configuration
5. Create VM instance (allocate CID, create state dir)
6. Setup mounts (transform to virtio-blk)
7. Setup network (CNI Add)
8. Start VM (QEMU exec)
   a. Create console FIFO
   b. Open TAP file descriptors
   c. Build kernel command line
   d. Build QEMU command line
   e. Start QEMU process
   f. Connect to QMP socket
   g. Connect to vsock
   h. Start background monitors
9. Start event forwarder
10. Create bundle in guest (via TTRPC)
11. Setup I/O forwarding
12. Create task in guest (via TTRPC)
13. Start hotplug controllers
14. Return success
```

## VM Shutdown Sequence

Detailed sequence during shutdown:

```
1. State transition to ShuttingDown
2. Cancel background monitors
3. Stop hotplug controllers
4. Shutdown I/O forwarders (wait for completion)
5. Close connection manager
6. Close client connections (TTRPC, vsock, console FIFO)
7. Send CTRL+ALT+DELETE via QMP
8. If that fails, send ACPI powerdown
9. Wait for ACPI (500ms)
10. Send QMP quit command
11. Wait for QEMU exit (2s)
12. If still running, SIGKILL
13. Wait for SIGKILL (2s)
14. Cleanup resources (QMP, console, TAP FDs, CID lease, state dir)
15. Release network resources (CNI Del)
16. Cleanup mounts
17. Close event channel
18. Exit shim
```
