# qemubox

**Experimental** containerd runtime that runs each container in its own lightweight VM using QEMU/KVM.

This project started as a fork of [nerdbox](https://github.com/containerd/nerdbox).

- Linux only (requires KVM)
- One VM per container for strong isolation
- Standard CNI networking
- EROFS snapshots for efficient storage

## Why Fork?

[nerdbox](https://github.com/containerd/nerdbox) is an excellent project that pioneered the "shim-level VM isolation" approach for containerd. It uses [libkrun](https://github.com/containers/libkrun) as its VMM, enabling cross-platform support (Linux, macOS, Windows) and rootless operation.

**qemubox** takes a different path:

| | nerdbox | qemubox |
|---|---------|---------|
| **VMM** | libkrun (Rust) | QEMU/KVM |
| **Platforms** | Linux, macOS, Windows | Linux only |
| **Focus** | Cross-platform, rootless | Server workloads, KVM features |
| **Networking** | libkrun networking | Standard CNI plugins |

We forked because we wanted:

1. **QEMU's maturity** - Battle-tested VMM with extensive device support, debugging tools (QMP, gdbstub), and broad kernel compatibility
2. **Standard CNI networking** - Reuse existing CNI plugins (Calico, Cilium, etc.) instead of custom networking
3. **KVM-specific features** - CPU/memory hotplug, vhost-net, virtio-fs, and other Linux-specific optimizations
4. **Simpler deployment** - Single static binary VMM without Rust runtime dependencies

If you need cross-platform support or rootless containers, use nerdbox.

## Why VMs?

VM isolation provides a stronger security boundary than namespace-based containers, while maintaining compatibility with standard containerd tooling.

## Quick Start

### Prerequisites

- Linux with KVM (`/dev/kvm` accessible)

### Install

Download the latest release and run the installer:

```bash
tar xzf qemubox-VERSION-linux-x86_64.tar.gz
cd qemubox-VERSION-linux-x86_64
sudo ./install.sh
```

The release includes everything: containerd, QEMU, kernel, CNI plugins, and configuration.

For systems with existing containerd, use shim-only mode:

```bash
sudo ./install.sh --shim-only
```

See `./install.sh --help` for all options.

### Start

```bash
sudo systemctl enable --now qemubox-containerd
```

### Run a Container

```bash
# Add qemubox binaries to PATH
export PATH=/usr/share/qemubox/bin:$PATH

# Pull an image
ctr image pull --snapshotter erofs docker.io/library/alpine:latest

# Run with qemubox runtime
ctr run -t --rm \
  --snapshotter erofs \
  --runtime io.containerd.qemubox.v1 \
  docker.io/library/alpine:latest \
  test /bin/sh
```

## Architecture

```mermaid
graph TB
    subgraph "Host (Linux)"
        containerd[containerd]
        shim[containerd-shim-qemubox-v1]
        cni[CNI Plugins<br/>bridge, firewall, IPAM]
        tap[TAP device]
        store[EROFS snapshots]
        kernel[VM kernel]
        initrd[VM initrd]
        qemu[QEMU/KVM]

        containerd -->|shim v2 ttrpc| shim
        shim -->|"CNI setup (netns/tap/IPAM)"| cni
        cni -->|create/attach| tap
        shim -->|"spawn/config (kernel args, vsock CID)"| qemu
        store -->|rootfs via virtio-blk| qemu
        kernel -->|bzImage| qemu
        initrd -->|initrd| qemu
        qemu -.->|virtio-net to TAP| tap
    end

    subgraph "Linux VM"
        vminitd[vminitd<br/>PID 1 / TTRPC]
        systemsvc[system service]
        bundlesvc[bundle service]
        crun[crun<br/>OCI Runtime]
        container[Container Process]

        vminitd --> systemsvc
        vminitd --> bundlesvc
        vminitd --> crun
        crun --> container
    end

    shim -.->|vsock ttrpc + stdio| vminitd

    style containerd fill:#e1f5ff
    style shim fill:#fff4e1
    style cni fill:#e8f5e9
    style qemu fill:#fce4ec
    style vminitd fill:#f3e5f5
    style crun fill:#fff9c4
    style container fill:#ffebee
```

**Key components:**
- **Shim**: Manages VM lifecycle and proxies I/O via vsock
- **CNI**: Standard CNI plugin chains for networking
- **QEMU**: Boots lightweight VMs with virtio devices
- **vminitd**: Init daemon inside VM that runs crun

## How It Works

1. **containerd** calls the qemubox shim to create a container
2. **CNI** allocates an IP and creates a TAP device
3. **QEMU** boots a microVM with kernel, network, and storage
4. **vminitd** (PID 1 in VM) connects to shim via vsock
5. **crun** starts the container process with resource limits
6. Container I/O flows through vsock to containerd

## Security

Multiple isolation layers:

- **VM boundary**: Hardware virtualization (KVM) isolates each container
- **Network**: Isolated TAP devices, firewall rules via CNI
- **Storage**: Read-only EROFS via virtio-blk
- **Resource**: cgroups v2 prevents resource exhaustion
- **Communication**: vsock (no network-based IPC)

## Limitations

- Linux only (KVM required)
- x86_64 only (arm64 untested)
- One VM per container (no sharing)
- Cold start for each container (no VM pooling)

## Development

### Build from Source

```bash
# Install Task runner
go install github.com/go-task/task/v3/cmd/task@latest

# Build everything (requires Docker for kernel/initrd)
task build

# Create release tarball
task release
```

### Repository Layout

```
cmd/           - Entrypoints (shim, vminitd)
internal/      - Implementation
  host/        - VM, network, storage management
  shim/        - Containerd shim implementation
  guest/       - vminitd and guest services
  config/      - Configuration management
api/           - Protobuf/TTRPC definitions
build/         - Build inputs (kernel config)
deploy/        - Installation scripts and configs
examples/      - Example configurations
hack/          - Development scripts
images/        - Container/VM image builds
```

## Documentation

- [`CLAUDE.md`](CLAUDE.md) - Architecture and development guide
- [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md) - Configuration reference
- [`deploy/config/`](deploy/config/) - Production configurations
- [`examples/`](examples/) - Example configurations (config.json, CNI)

## Comparison

| Project | Approach | Trade-offs |
|---------|----------|------------|
| **qemubox** | VM per container (QEMU/KVM) | Strong isolation, Linux-only, cold start overhead |
| **[nerdbox](https://github.com/containerd/nerdbox)** | VM per container (libkrun) | Cross-platform, rootless, newer VMM |
| **[Kata Containers](https://katacontainers.io/)** | VM per container (multiple VMMs) | Production-ready, more complex |
| **[gVisor](https://gvisor.dev/)** | User-space kernel | No VM overhead, different syscall compatibility |
| **runc** | Namespaces only | Fast, weaker isolation |

## License

Apache 2.0

## Contributing

Experimental project. Issues and PRs welcome.
