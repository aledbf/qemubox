# Containerd Runtime - qemubox Shim

**Technology**: Go 1.25+, containerd shim API, QEMU, KVM
**Entry Points**:
- `containerd/cmd/containerd-shim-qemubox-v1/main.go` (shim)
- `containerd/cmd/vminitd/main.go` (VM init daemon)
**Parent Context**: This extends [../CLAUDE.md](../CLAUDE.md)

⚠️ **SECURITY-CRITICAL**: This module manages VM isolation boundaries. Changes have security implications.

---

## Architecture Overview

```
Host (containerd)
└─> containerd-shim-qemubox-v1
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
/usr/share/qemubox/bin/qemu-system-x86_64  # VMM binary
/usr/share/qemubox/kernel/qemubox-kernel-x86_64  # VM kernel
/usr/share/qemubox/kernel/qemubox-initrd    # Initial ramdisk
/var/lib/qemubox/cni-config.db             # CNI network configuration metadata
/var/lib/cni/networks/                    # CNI IPAM state (IP allocations)
/var/log/qemubox/                          # VM logs
```

Override with environment variables:
- `QEMUBOX_SHARE_DIR` (default: `/usr/share/qemubox`)
- `QEMUBOX_STATE_DIR` (default: `/var/lib/qemubox`)
- `QEMUBOX_LOG_DIR` (default: `/var/log/qemubox`)

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
- **CNI-based networking**: Uses standard CNI plugin chains exclusively
- Creates TAP devices per VM using CNI
- IP allocation managed by CNI IPAM plugins (host-local, static, dhcp)
- Bridge name and subnet configured in CNI config file (default example: `qemubox0` with 10.88.0.0/16)
- Firewall rules managed by CNI firewall plugin
- **Key files**:
  - `network/network.go` - Network manager interface
  - `network/manager_cni.go` - CNI implementation
  - `network/cni/` - CNI plugin execution package

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
# Check bridge (name from CNI config, default example: qemubox0)
ip link show qemubox0
ip addr show qemubox0

# Check TAP devices
ip link show | grep tap

# Verify CNI configuration
ls /etc/cni/net.d/
cat /etc/cni/net.d/10-qemubox.conflist | jq .

# Check CNI plugins
ls -la /opt/cni/bin/

# Check CNI network metadata
ls -la /var/lib/qemubox/cni-config.db

# Check CNI IPAM state (host-local)
ls -la /var/lib/cni/networks/qemubox-net/

# Check firewall rules (managed by CNI)
nft list ruleset | grep qemubox

# Test CNI plugin manually
CNI_COMMAND=ADD CNI_CONTAINERID=test CNI_NETNS=/var/run/netns/test \
CNI_IFNAME=eth0 CNI_PATH=/opt/cni/bin \
/opt/cni/bin/bridge < /etc/cni/net.d/10-qemubox.conflist
```

**Common CNI Issues**:
```bash
# Plugin not found
ls /opt/cni/bin/bridge || echo "Install CNI plugins"

# Config file not found
ls /etc/cni/net.d/*.conflist || echo "Create CNI config"

# Permission denied
sudo chmod +x /opt/cni/bin/*

# IP allocation conflicts
sudo rm -rf /var/lib/cni/networks/qemubox-net/
```

### Debugging VM Startup
```bash
# Check logs
tail -f /var/log/qemubox/vm-*.log

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
4. `network/network.go` - CNI-based network manager interface
5. `network/manager_cni.go` - CNI mode implementation
6. `network/cni/cni.go` - CNI plugin executor
7. `integration/vm_test.go` - Integration test examples

**Documentation**:
- `containerd/README.md` - Comprehensive architecture documentation
- `containerd/docs/CNI_SETUP.md` - CNI setup and configuration guide
- `containerd/examples/cni/` - Example CNI configurations

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
sudo mkdir -p /usr/share/qemubox/bin
sudo cp /usr/bin/qemu-system-x86_64 /usr/share/qemubox/bin/
```

### "Permission denied on /dev/kvm"
```bash
# Add user to kvm group
sudo usermod -aG kvm $USER
# Log out and log back in
```

### "Network device not found"
- Bridge is created automatically by CNI bridge plugin based on configuration
- Bridge name is defined in CNI config file (default example: qemubox0)
- Check logs for initialization errors
- Verify nftables is installed

### "IP allocation failed"

```bash
# Check CNI network metadata
ls -la /var/lib/qemubox/cni-config.db

# Check CNI IPAM state
ls -la /var/lib/cni/networks/qemubox-net/

# Clear stale allocations
sudo rm -rf /var/lib/cni/networks/qemubox-net/*

# Restart containerd
systemctl restart qemubox-containerd
```

### "CNI plugin not found"
```bash
# Error: failed to execute CNI plugin chain: exec: "bridge": executable file not found

# Solution 1: Install CNI plugins
mkdir -p /opt/cni/bin
wget https://github.com/containernetworking/plugins/releases/download/v1.4.0/cni-plugins-linux-amd64-v1.4.0.tgz
tar -xzf cni-plugins-linux-amd64-v1.4.0.tgz -C /opt/cni/bin

# Solution 2: Verify standard CNI plugin directory
ls -la /opt/cni/bin/

# Solution 3: Check permissions
sudo chmod +x /opt/cni/bin/*
```

### "No TAP device found in CNI result"
```bash
# Error: CNI plugins created veth pair but no TAP device

# Solution 1: Install tc-redirect-tap plugin
git clone https://github.com/firecracker-microvm/firecracker-go-sdk
cd firecracker-go-sdk/cni/tc-redirect-tap
go build -o /opt/cni/bin/tc-redirect-tap
chmod +x /opt/cni/bin/tc-redirect-tap

# Solution 2: Add tc-redirect-tap to CNI configuration
# Edit /etc/cni/net.d/10-qemubox.conflist
# Add: {"type": "tc-redirect-tap"} to plugins array

# Solution 3: Use a CNI plugin that directly creates TAP devices
# (Alternative to tc-redirect-tap)
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
2. **Execute CNI plugin chain**: CNI plugins create bridge (if needed), allocate IP, create TAP device
3. **CNI bridge plugin**: Creates bridge based on CNI config (e.g., qemubox0)
4. **CNI IPAM plugin**: Allocates IP from configured subnet (e.g., 10.88.0.2 - 10.88.255.254)
5. **CNI tc-redirect-tap plugin**: Creates TAP device and attaches to bridge
6. **CNI firewall plugin**: Configures nftables rules
7. **Pass to QEMU**: TAP device name and IP configuration as kernel params

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
- CPU scale-down uses projected utilization after removing one vCPU (default target 50%)
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

### CNI-Based Networking

Beacon uses **CNI (Container Network Interface)** exclusively for all network management:

- Standard CNI plugin chains for network configuration
- Integration with CNI ecosystem (Calico, Cilium, etc.)
- Support for multiple networks, custom routing, network policies
- Firecracker-compatible via tc-redirect-tap plugin
- IP allocation delegated to CNI IPAM plugins (host-local, static, dhcp)

### CNI Configuration

Qemubox uses standard CNI paths for all network configuration:
- **CNI config directory**: `/etc/cni/net.d`
- **CNI plugin binaries**: `/opt/cni/bin`
- **Network config auto-discovery**: Loads the first `.conflist` file alphabetically (e.g., `10-qemubox.conflist`)

This follows standard containerd and CNI conventions. The network name and configuration are determined entirely by your CNI config files.

**CNI Plugin Installation**:
```bash
# Install standard CNI plugins
mkdir -p /opt/cni/bin /etc/cni/net.d
wget https://github.com/containernetworking/plugins/releases/download/v1.4.0/cni-plugins-linux-amd64-v1.4.0.tgz
tar -xzf cni-plugins-linux-amd64-v1.4.0.tgz -C /opt/cni/bin

# (Optional) Install tc-redirect-tap for Firecracker pattern
git clone https://github.com/firecracker-microvm/firecracker-go-sdk
cd firecracker-go-sdk/cni/tc-redirect-tap
go build -o /opt/cni/bin/tc-redirect-tap
```

**Example CNI Configuration** (`/etc/cni/net.d/10-qemubox.conflist`):
```json
{
  "cniVersion": "1.0.0",
  "name": "qemubox-net",
  "plugins": [
    {
      "type": "bridge",
      "bridge": "qemubox0",
      "isGateway": true,
      "ipMasq": true,
      "ipam": {
        "type": "host-local",
        "ranges": [[{"subnet": "10.88.0.0/16", "gateway": "10.88.0.1"}]],
        "routes": [{"dst": "0.0.0.0/0"}]
      }
    },
    {"type": "firewall"},
    {"type": "tc-redirect-tap"}
  ]
}
```

See `examples/cni/` for more configuration examples.

### IP Allocation and State

- **IP allocation**: Managed by CNI IPAM plugins (host-local, static, dhcp)
- **Network metadata**: `/var/lib/qemubox/cni-config.db` (BoltDB - stores CNI network config)
- **IP allocation state**: `/var/lib/cni/networks/<network-name>/` (managed by CNI IPAM)
- **Default subnet**: 10.88.0.0/16 (configured in CNI conflist)
  - Gateway: 10.88.0.1
  - Container IPs: 10.88.0.2 - 10.88.255.254 (65,533 addresses)

### Firewall Rules

```bash
# View beacon firewall rules
nft list ruleset | grep qemubox_runner

# Tables created:
# - beacon_runner_filter (forwarding)
# - beacon_runner_nat (postrouting NAT)
```

**CNI Mode**: Firewall rules managed by CNI firewall plugin (iptables/nftables)

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
