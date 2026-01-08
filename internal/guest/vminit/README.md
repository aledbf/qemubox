# vminit - Guest Init Daemon

The vminit package implements the guest-side init daemon (PID 1) that runs inside QEMU/KVM virtual machines. It manages container lifecycle via the containerd task API over TTRPC/vsock.

## Architecture

```
┌────────────────────────────────────────────────────────────────────┐
│                         vminitd (PID 1)                            │
├────────────────────────────────────────────────────────────────────┤
│                                                                    │
│  ┌─────────────────┐     ┌─────────────────────────────────────┐   │
│  │  System Init    │     │  TTRPC Service (vsock)              │   │
│  │  (system/)      │────▶│  ├─ Task Service (Create/Start/etc) │   │
│  │  • Mounts       │     │  ├─ Streaming Service (I/O)         │   │
│  │  • Cgroups v2   │     │  └─ Events Service                  │   │
│  │  • DNS          │     └─────────────────────────────────────┘   │
│  └─────────────────┘                    │                          │
│                                         ▼                          │
│  ┌──────────────────────────────────────────────────────────────┐  │
│  │  Container Management (runc/)                                │  │
│  │  ├─ Container: rootfs, cgroups, OCI runtime wrapper          │  │
│  │  └─ Process: Init + Exec processes with state machines       │  │
│  └──────────────────────────────────────────────────────────────┘  │
│                                                                    │
└────────────────────────────────────────────────────────────────────┘
```

## Package Structure

```
vminit/
├── service/     # TTRPC server and plugin management
├── task/        # Task service (containerd API implementation)
├── runc/        # Container abstraction over OCI runtime
├── process/     # Init and Exec process management
├── system/      # VM system initialization (mounts, cgroups)
├── streaming/   # vsock-based I/O streaming
├── events/      # Event publishing to containerd
├── devices/     # Block device handling
├── bundle/      # OCI bundle management
└── testutil/    # Test utilities
```

## Key Components

### Process State Machine (process/state.go)

```
CREATED ──Start()──► RUNNING ──SetExited()──► STOPPED ──Delete()──► DELETED
    │                    │
    └──SetExited()───────┘  (early exit)
```

Transitions are validated via `validTransitions` map.

### Exit Tracker (task/exit_tracker.go)

Handles race conditions where processes exit before `Start()` returns:

- **earlyExitDetector**: Captures exits during the start window via subscriptions
- **exitCoordinator**: Ensures init exit is published after all exec exits
- **processRegistry**: Tracks running processes by PID (handles PID reuse)

See code comments for concurrency design details.
