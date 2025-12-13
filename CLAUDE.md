# Containerd Runtime - beaconbox Shim

**Technology**: Go 1.25+, containerd shim API, QEMU, KVM
**Entry Points**:
- `containerd/cmd/containerd-shim-beaconbox-v1/main.go` (shim)
- `containerd/cmd/vminitd/main.go` (VM init daemon)
**Parent Context**: This extends [../CLAUDE.md](../CLAUDE.md)

⚠️ **SECURITY-CRITICAL**: This module manages VM isolation boundaries. Changes have security implications.

---

## Architecture Overview

```
Host (containerd)
└─> containerd-shim-beaconbox-v1
    ├─> QEMU (VMM)
    │   └─> Linux VM
    │       └─> vminitd (PID 1)
    │           └─> crun (OCI runtime)
    │               └─> Container process
    ├─> Network Manager (TAP/bridge)
    └─> Storage (EROFS via virtio-fs)
```

## Critical Paths (NEVER modify without understanding)

### Fixed Installation Paths
```
/usr/share/beacon/bin/qemu-system-x86_64  # VMM binary
/usr/share/beacon/kernel/beacon-kernel-x86_64  # VM kernel
/usr/share/beacon/kernel/beacon-initrd    # Initial ramdisk
/var/lib/beacon/network.db                # IP allocation state
/var/log/beacon/                          # VM logs
```

Override with environment variables:
- `BEACON_SHARE_DIR` (default: `/usr/share/beacon`)
- `BEACON_STATE_DIR` (default: `/var/lib/beacon`)
- `BEACON_LOG_DIR` (default: `/var/log/beacon`)

---

## Key Modules

### `shim/` - Runtime Shim
- Implements containerd Shim API
- Manages VM lifecycle via QEMU
- Proxies I/O between containerd and VM (vsock)
- **Key file**: `shim/task/service.go:CreateTask()` - Container creation entry point

### `vminit/` - VM Init Daemon
- PID 1 inside VM
- Implements Task API via TTRPC over vsock
- Manages crun to execute containers
- **Key file**: `vminit/task/service.go:Create()` - OCI bundle creation

### `vm/` - VMM Integration
- **`vm/qemu/instance.go`** - QEMU VM management
- **VM RESOURCES**: Configurable CPU and memory via `vm.VMResourceConfig`

### `network/` - Network Management
- Creates `beacon0` bridge (10.88.0.0/16)
- Allocates IPs from pool (stored in BoltDB)
- Creates TAP devices per VM
- Configures nftables rules for NAT
- **Key file**: `network/network.go:Setup()` - Network initialization

### `services/` - VM Services
- `services/bundle.go` - OCI bundle creation
- `services/system.go` - System service management

---

## Security Model

### Isolation Layers
1. **Hypervisor** (primary): KVM hardware virtualization
2. **Network**: Isolated TAP devices + nftables
3. **Filesystem**: Read-only EROFS via virtio-fs
4. **OCI Runtime** (crun): cgroups v2 resource limits within VM
5. **vsock**: Isolated host-VM communication

### Network Namespace Removal
⚠️ **IMPORTANT**: Network namespace is explicitly removed from OCI spec:
- See `shim/task/service.go:CreateTask()` - removes `network` from namespaces
- Containers share VM's `eth0` interface
- Isolation provided by VM boundary, not network namespace

---

## Building

```bash
# Build shim
task build:shim

# Build vminitd
task build:vminitd

# Build initrd (packages vminitd)
task build:initrd

# Build VM kernel (requires Docker)
task build:kernel

# Build QEMU binaries and firmware
task build:qemu
```

---

## Testing

```bash
# Prerequisites check
/check-vm

# Integration tests (requires KVM)
cd containerd
go test -v -timeout 10m ./integration/...
```

---

## Common Operations

### Debugging VM Networking
```bash
# Check bridge
ip link show beacon0
ip addr show beacon0

# Check TAP devices
ip link show | grep beacon-

# Check IP allocations
ls -la /var/lib/beacon/network.db

# Check nftables rules
nft list ruleset | grep beacon_runner
```

### Debugging VM Startup
```bash
# Check logs
tail -f /var/log/beacon/vm-*.log

# Check vsock connections
ss -x | grep vsock

# Check QEMU process
ps aux | grep qemu-system-x86_64
```

---

## Anti-Patterns

❌ **NEVER**:
- Modify VM resource limits without understanding memory/CPU implications
- Remove security isolation layers (VM, network, filesystem)
- Hardcode paths (use `paths` package helpers)
- Skip KVM checks (will fail at runtime)
- Modify network namespace handling without security review

✅ **ALWAYS**:
- Test with real VMs (not just unit tests)
- Verify KVM access before running integration tests
- Check QEMU compatibility
- Validate network allocation/deallocation
- Use transactions for network state changes

---

## Key Files to Study

1. `shim/task/service.go` - Shim service implementation
2. `vminit/task/service.go` - VM init service
3. `vm/qemu/instance.go` - QEMU integration
4. `network/network.go` - Network management
5. `integration/vm_test.go` - Integration test examples

Read `containerd/README.md` for comprehensive architecture documentation.

---

## Development Workflow

### Making Changes

1. **Read architecture docs**: `containerd/README.md`
2. **Understand security model**: Review isolation layers
3. **Make changes**: Edit Go code
4. **Build**: `task build:shim` or `task build:vminitd`
5. **Test locally**: Run integration tests if you have KVM
6. **Document**: Update README.md if architecture changes

### Testing Without KVM

If you don't have KVM access (e.g., macOS development):
- Unit tests will run: `go test ./shim/... ./vminit/...`
- Integration tests will be skipped
- Use Linux VM or CI for full integration testing

---

## Common Issues

### "qemu-system-x86_64 binary not found"
```bash
# Build QEMU from source or use package manager
# Option 1: Build via task
task build:qemu

# Option 2: Use system package manager
sudo apt-get install qemu-system-x86

# Option 3: Install to beacon directory
sudo mkdir -p /usr/share/beacon/bin
sudo cp /usr/bin/qemu-system-x86_64 /usr/share/beacon/bin/
```

### "Permission denied on /dev/kvm"
```bash
# Add user to kvm group
sudo usermod -aG kvm $USER
# Log out and log back in
```

### "Network device beacon0 not found"
- Bridge is created automatically on first container
- Check logs for initialization errors
- Verify nftables is installed

### "IP allocation failed"
```bash
# Check network database
ls -la /var/lib/beacon/network.db

# Reconciliation runs every minute to clean stale leases
# Wait or manually trigger by restarting shim
```

---

## Critical Code Paths

### Container Creation Flow

1. **containerd calls shim**: `shim/task/service.go:CreateTask()`
2. **Shim allocates network**: `network/network.go:AllocateIP()`
3. **Shim creates VM**: `vm/qemu/instance.go:Start()`
4. **VM boots Linux kernel**: Kernel loads with network config
5. **vminitd starts**: `vminit/task/service.go:main()` (PID 1)
6. **vminitd connects to shim**: TTRPC over vsock
7. **Shim calls vminitd.Create()**: `vminit/task/service.go:Create()`
8. **vminitd calls crun**: OCI runtime creates container
9. **Container process starts**: Inside VM with resource limits

### Network Setup Flow

1. **Network manager init**: `network/network.go:New()`
2. **Create beacon0 bridge**: If not exists
3. **Allocate IP from pool**: 10.88.0.2 - 10.88.255.254
4. **Create TAP device**: `beacon-<hash>`
5. **Attach TAP to bridge**: Link TAP to beacon0
6. **Configure nftables**: NAT and forwarding rules
7. **Pass to QEMU**: TAP device and IP as kernel params

### VM Lifecycle

```
Create → Start → (Running) → Stop → Delete
   ↓       ↓                    ↓      ↓
   VM    Boot                 Shutdown Cleanup
  Init   Kernel              VM Dies   Release IP
```

---

## Resource Management

### VM Resources (Configurable)

VM resources are now configurable via `vm.VMResourceConfig`:
```go
type VMResourceConfig struct {
    BootCPUs          int   // Initial vCPUs (default: 1)
    MaxCPUs           int   // Max vCPUs for hotplug (default: 2)
    MemorySize        int64 // Initial memory in bytes (default: 512 MiB)
    MemoryHotplugSize int64 // Max memory for hotplug in bytes (default: 2 GiB)
}
```

**Features**:
- Dynamic CPU hotplug support via QEMU QMP
- Configurable memory limits
- Container-specific resource allocation

### Container Resource Limits (cgroups v2)

From `vminit/task/service.go`:
```go
// crun applies OCI spec resource limits via cgroups v2
// Example: Container requests 512MB memory
//   → crun creates cgroup with 512MB limit within VM
```

---

## Network Architecture

### Subnet and IP Allocation

```
beacon0 bridge: 10.88.0.1/16
Container IPs:  10.88.0.2 - 10.88.255.254 (65,533 addresses)
```

**Persistent storage**: `/var/lib/beacon/network.db` (BoltDB)

### Firewall Rules

```bash
# View beacon firewall rules
nft list ruleset | grep beacon_runner

# Tables created:
# - beacon_runner_filter (forwarding)
# - beacon_runner_nat (postrouting NAT)
```

---

## vsock Communication

### Connection Setup

1. **Shim listens on vsock**: CID=2 (host), port assigned by kernel
2. **VM boots with vsock device**: QEMU configures virtio-vsock
3. **vminitd connects**: CID=3 (guest) → CID=2 (host)
4. **TTRPC handshake**: Task service registration
5. **Bi-directional communication**: RPC calls + stdio streaming

### Debugging vsock

```bash
# Check vsock connections (requires root)
ss -x | grep vsock

# Check QEMU vsock config
ps aux | grep qemu-system-x86_64 | grep vsock
```

---

## EROFS and virtio-fs

### Storage Layer

```
Host: EROFS snapshot mounted at /var/lib/containerd/...
  ↓ (virtio-fs with DAX)
VM: Mounted at /run/containerd/...
  ↓
Container rootfs: Overlay or direct mount
```

**Performance**: virtio-fs with DAX provides near-native filesystem performance

---

## Pre-PR Checklist

```bash
# Build all components
task build:shim
task build:vminitd
task build:initrd

# Run unit tests
go test ./shim/... ./vminit/... ./vm/... ./network/...

# Run integration tests (if KVM available)
cd containerd && go test -v ./integration/...

# Lint
golangci-lint run
```

---

## Security Considerations

Before making changes, ask:
1. Does this affect VM isolation boundaries?
2. Could this allow container escape?
3. Does this change network namespace handling?
4. Are paths properly validated (no path traversal)?
5. Is sensitive data logged or exposed?
6. Does this require additional KVM permissions?

If YES to any: **Get security review before merging**

---

## Additional Resources

- **Architecture**: `containerd/README.md` (comprehensive)
- **QEMU docs**: https://www.qemu.org/documentation/
- **containerd shim API**: https://github.com/containerd/containerd/tree/main/runtime/v2
- **cgroups v2**: https://www.kernel.org/doc/html/latest/admin-guide/cgroup-v2.html
- **vsock**: https://wiki.qemu.org/Features/VirtioVsock
