# qemubox

[![CI](https://github.com/aledbf/qemubox/actions/workflows/ci.yml/badge.svg)](https://github.com/aledbf/qemubox/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/aledbf/qemubox/branch/main/graph/badge.svg)](https://codecov.io/gh/aledbf/qemubox)
[![Go Report Card](https://goreportcard.com/badge/github.com/aledbf/qemubox/containerd)](https://goreportcard.com/report/github.com/aledbf/qemubox/containerd)
[![Go Reference](https://pkg.go.dev/badge/github.com/aledbf/qemubox/containerd.svg)](https://pkg.go.dev/github.com/aledbf/qemubox/containerd)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

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

## Demos

### Boot & Docker
[![asciicast](https://asciinema.org/a/INszYqaxkMzQtzIKIDGX8QJqi.svg)](https://asciinema.org/a/INszYqaxkMzQtzIKIDGX8QJqi)

### Snapshot & Commit
[![asciicast](https://asciinema.org/a/765288.svg)](https://asciinema.org/a/765288)

Persist disk state between VM runs: make changes (files, packages), commit to a new image with `nerdctl commit`, and run a new VM with all modifications preserved.

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
sudo systemctl start --now qemubox-containerd
```

### Run a Container

```bash
# Add qemubox binaries to PATH
export PATH=/usr/share/qemubox/bin:$PATH

# Pull an image
ctr --address /var/run/qemubox/containerd.sock image pull \
  --snapshotter erofs ghcr.io/aledbf/qemubox/sandbox:v0.0.9

# Run with qemubox runtime
ctr --address /var/run/qemubox/containerd.sock run -t --rm \
  --snapshotter erofs \
  --runtime io.containerd.qemubox.v1 \
  ghcr.io/aledbf/qemubox/sandbox:v0.0.9 test-qemu-shim
```
(use root:qemubox to log in)

## Architecture

```mermaid
graph LR
    %% Host (Linux)
    subgraph host ["**Host (Linux)**"]
        direction TB
        containerd:::hostproc
        shim[containerd-shim-qemubox-v1]:::hostproc
        cni[CNI Plugins<br/>bridge, firewall, IPAM]:::netdev
        tap[TAP device]:::netdev
        store[EROFS snapshots]:::block
        kernel[VM kernel]:::hostvm
        initrd[VM initrd]:::hostvm
        qemu[QEMU/KVM]:::hypervisor

        containerd -- "shim v2 ttrpc" --> shim
        shim -- "CNI setup
        (netns/tap/IPAM)" --> cni
        cni -- "create/attach" --> tap
        shim -- "spawn/config
        (kernel args, vsock CID)" --> qemu
        store -- "rootfs via
        virtio-blk" --> qemu
        kernel -- "bzImage" --> qemu
        initrd -- "initrd" --> qemu
        qemu -.->|virtio-net to TAP| tap
    end

    %% Linux VM
    subgraph vm ["**Linux VM**"]
        direction TB
        vminitd[vminitd<br/>PID 1 / TTRPC]:::vmcore
        systemsvc[system service]:::srv
        bundlesvc[bundle service]:::srv
        crun[crun<br/>OCI Runtime]:::runtime
        container[Container Process]:::ctr

        vminitd --> systemsvc
        vminitd --> bundlesvc
        vminitd --> crun
        crun --> container
    end

    %% Connections between host and VM
    shim -.->|vsock ttrpc + stdio| vminitd

    %% Styling with classDefs for consistency and clarity
    classDef hostproc fill:#2962ff,stroke:#152f6b,stroke-width:2px,color:#fff
    classDef netdev fill:#26a69a,stroke:#00695c,stroke-width:2px,color:#fff
    classDef block fill:#ffee58,stroke:#666600,stroke-width:2px,color:#111
    classDef hostvm fill:#ec407a,stroke:#880e4f,stroke-width:2px,color:#fff
    classDef hypervisor fill:#6d4c41,stroke:#442b2d,stroke-width:2px,color:#fff
    classDef vmcore fill:#8e24aa,stroke:#4a148c,stroke-width:2px,color:#fff
    classDef srv fill:#80cbc4,stroke:#004d40,stroke-width:2px,color:#111
    classDef runtime fill:#ffb300,stroke:#6d4c00,stroke-width:2px,color:#111
    classDef ctr fill:#ef5350,stroke:#b71c1c,stroke-width:2px,color:#fff

    class containerd,shim hostproc
    class cni,tap netdev
    class store block
    class kernel,initrd hostvm
    class qemu hypervisor
    class vminitd vmcore
    class systemsvc,bundlesvc srv
    class crun runtime
    class container ctr

    linkStyle 9 stroke-dasharray:4 4,stroke-width:2px,stroke:#6d4c41
    linkStyle 8 stroke-width:2px,stroke:#00acc1,stroke-dasharray:3 6
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

### Container Lifecycle

```mermaid
sequenceDiagram
    autonumber

    %% Group host-side actors in a blue box
    box Host (Linux)
        participant C as containerd
        participant S as containerd-shim
        participant N as CNI Plugins
        participant Q as QEMU/KVM
    end

    %% Group VM-side actors in a green box
    box VM (Guest Linux)
        participant V as vminitd (PID 1)
        participant R as crun
    end

    %% --- Create Phase ---
    rect rgb(220,240,255)
    note over C,R: Create Phase
    activate C
    C->>+S: CreateTask(bundle, rootfs)
    activate S
    S->>S: Check KVM<br/>Load OCI spec<br/>Prepare VM
    S->>S: Setup EROFS mounts
    S->>N: Setup network (CNI)
    N-->>S: Return TAP device + IP config
    S->>Q: Start VM<br/>(kernel, initrd, virtio-devs)
    deactivate S
    activate Q
    Q-->>V: Boot kernel<br/>start vminitd (PID 1)
    deactivate Q
    activate V
    V-->>S: Connect via vsock
    S->>V: Send bundle (OCI, rootfs)
    S->>V: CreateTask
    V->>R: OCI create container
    activate R
    R-->>V: Container created
    deactivate R
    V-->>S: Return PID
    S-->>C: Return PID
    deactivate C
    deactivate S
    deactivate V
    end
    
    %% --- Start Phase ---
    rect rgb(224,255,224)
    note over C,R: Start Phase
    activate C
    C->>S: Start
    activate S
    S->>V: Start
    activate V
    V->>R: Start container
    activate R
    R-->>V: Running
    deactivate R
    V-->>S: PID
    S-->>C: PID
    deactivate C
    deactivate S
    deactivate V
    end

    %% --- Delete Phase ---
    rect rgb(255,228,216)
    note over C,R: Delete Phase
    activate C
    C->>S: Delete
    activate S
    S->>V: Delete
    activate V
    V->>R: Delete container
    activate R
    R-->>V: Deleted
    deactivate R
    V-->>S: Exit status
    deactivate V
    S->>S: Stop hotplug controllers
    S->>Q: Shutdown VM
    activate Q
    Q-->>S: VM exited
    deactivate Q
    S->>N: Release network
    N-->>S: Network cleaned
    S-->>C: Exit status
    deactivate C
    deactivate S
    end
```

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

## Roadmap

- **Remove KVM requirement**: Support [PVM (Protected Virtual Machine)](https://github.com/virt-pvm/linux) kernel for environments without `/dev/kvm` ([LWN article](https://lwn.net/Articles/963718/))
- **Filesystem merge snapshots**: Leverage containerd's [fsmerge feature](https://github.com/containerd/containerd/pull/12374) for more efficient storage
- **Metrics and tracing**: Add TTRPC tracing to provide detailed observability into VM and container behavior
- **Annotations for features**: Allow enabling/disabling features (CPU/memory hotplug, etc.) via OCI annotations
- ~~**Snapshot demo**: Demonstrate VM snapshots - restart a VM with previous state/changes preserved~~ ✓ [Done](https://asciinema.org/a/765288)

## License

Apache 2.0

## Contributing

Experimental project. Issues and PRs welcome.
