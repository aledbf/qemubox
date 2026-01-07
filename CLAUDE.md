# qemubox

Containerd shim that runs containers inside QEMU/KVM virtual machines.

## Build

```bash
task build:shim      # Host shim binary
task build:vminitd   # Guest init daemon
task build:initrd    # Guest initrd (includes vminitd)
task build:kernel    # VM kernel (requires Docker)
```

## Test

```bash
go test ./...                           # Unit tests
go test -v -timeout 10m ./integration/...  # Integration (requires KVM)
golangci-lint run                       # Lint
```

## Structure

```
cmd/
  containerd-shim-qemubox-v1/  # Host: containerd shim
  vminitd/                     # Guest: PID 1 inside VM

internal/
  shim/task/       # Shim service (CreateTask, Start, Delete)
  guest/vminit/    # VM init daemon (TTRPC server over vsock)
  host/vm/qemu/    # QEMU VM lifecycle
  host/network/    # CNI networking (TAP devices, IP allocation)
  config/          # Configuration loading
```

## Configuration

Required: `/etc/qemubox/config.json` (or `QEMUBOX_CONFIG` env var)

See `examples/config.json` and `docs/CONFIGURATION.md`.

## Networking

Uses standard CNI. Config auto-discovered from `/etc/cni/net.d/*.conflist`.

Key types in `internal/host/network/`:
- `NetworkManager` interface - main API
- `CleanupResult` - teardown status (use `.Err()` to get error)
- `Metrics` - per-instance operation metrics (access via `manager.Metrics()`)

CNI errors are classified via `errors.Is()`:
- `cni.ErrResourceConflict` - orphaned veth/IP
- `cni.ErrIPAMExhausted` - no IPs available
- `cni.ErrIPAMLeak` - cleanup verification failed

## Security

VM isolation is the primary security boundary. Network namespaces are NOT used for containers - they share the VM's network interface.

Changes affecting isolation boundaries require security review.
