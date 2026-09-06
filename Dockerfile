# Build the Linux kernel, initrd, and containerd shim for running spinbox
# This multi-stage build produces:
# - Custom Linux kernel with container/virtualization support
# - initrd with vminitd and crun
# - containerd shim for spinbox runtime

# Base image versions
ARG GO_VERSION=1.26.2
ARG BASE_DEBIAN_DISTRO="bookworm"
ARG GOLANG_IMAGE="golang:${GO_VERSION}-${BASE_DEBIAN_DISTRO}"


# ============================================================================
# Base Images
# ============================================================================

# We only support x86_64/amd64 - platform is set via docker-bake.hcl
FROM ${GOLANG_IMAGE} AS base

RUN echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache
RUN apt-get update && apt-get install --no-install-recommends -y file apparmor curl

# ============================================================================
# Kernel Build Stages
# ============================================================================

FROM base AS kernel-build-base

# Set environment variables for non-interactive installations
ENV DEBIAN_FRONTEND=noninteractive

RUN echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache

# Install build dependencies
RUN --mount=type=cache,sharing=locked,id=kernel-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=kernel-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y build-essential libncurses-dev flex bison libssl-dev libelf-dev bc cpio git wget xz-utils curl lz4

ARG KERNEL_VERSION="7.2.1"
ARG KERNEL_ARCH="x86_64"
ARG KERNEL_NPROC="16"

# Install and configure Docker configuration checker
# Modified to remove SELinux and AppArmor checks which aren't needed for kernel building
RUN curl -o /usr/local/bin/check-docker-config.sh -fsSL https://raw.githubusercontent.com/moby/moby/master/contrib/check-config.sh \
  && chmod +x /usr/local/bin/check-docker-config.sh \
  && sed -i '/IP_VS_RR/s/\\$//; /SECURITY_SELINUX/d; /SECURITY_APPARMOR/d' /usr/local/bin/check-docker-config.sh

# Set the working directory
WORKDIR /usr/src

# Download kernel source (cached across builds)
RUN --mount=type=cache,sharing=locked,id=kernel-src-${KERNEL_VERSION},target=/var/cache/kernel \
    KERNEL_MAJOR="$(echo "${KERNEL_VERSION}" | cut -d. -f1)" && \
    if [ ! -f "/var/cache/kernel/linux-${KERNEL_VERSION}.tar.xz" ]; then \
        wget -O "/var/cache/kernel/linux-${KERNEL_VERSION}.tar.xz" "https://cdn.kernel.org/pub/linux/kernel/v${KERNEL_MAJOR}.x/linux-${KERNEL_VERSION}.tar.xz"; \
    fi && \
    tar -xf "/var/cache/kernel/linux-${KERNEL_VERSION}.tar.xz" -C /usr/src && \
    mv linux-${KERNEL_VERSION} linux

# Copy kernel config from repository (per kernel version + arch)
COPY build/kernel/config-${KERNEL_VERSION}-${KERNEL_ARCH} /usr/src/linux/.config

RUN <<EOT
    set -e
    cd /usr/src/linux

    # Use the provided kernel config as-is and resolve any missing dependencies
    make ARCH=${KERNEL_ARCH} olddefconfig

    # Verify the critical configs are STILL enabled after olddefconfig
    echo "Verifying critical kernel configs after olddefconfig..."
    grep -q "CONFIG_VIRTIO_NET=y" .config || (echo "ERROR: CONFIG_VIRTIO_NET not enabled after olddefconfig!" ; echo "Current VIRTIO_NET setting:" ; grep VIRTIO_NET .config ; exit 1)
    grep -q "CONFIG_VIRTIO_PCI=y" .config || (echo "ERROR: CONFIG_VIRTIO_PCI not enabled!" ; exit 1)
    grep -q "CONFIG_NET_CLS_ACT=y" .config || (echo "ERROR: CONFIG_NET_CLS_ACT not enabled!" ; exit 1)

    # Memory hotplug, checked here for the same reason the others are: olddefconfig
    # resolves what it cannot satisfy, and a dropped symbol is silent. Without these
    # three the host can add a DIMM, QEMU will accept it, query-memory-devices will
    # show it, and /sys/devices/system/memory/memory<N>/online — the file
    # internal/guest/services/system.go writes to — will not exist, so the guest never
    # sees the memory. That was the state until this config enabled them.
    grep -q "CONFIG_MEMORY_HOTPLUG=y" .config || (echo "ERROR: CONFIG_MEMORY_HOTPLUG not enabled (memory hotplug cannot reach the guest)!" ; exit 1)
    grep -q "CONFIG_MEMORY_HOTREMOVE=y" .config || (echo "ERROR: CONFIG_MEMORY_HOTREMOVE not enabled (unplug cannot work)!" ; exit 1)
    grep -q "CONFIG_ACPI_HOTPLUG_MEMORY=y" .config || (echo "ERROR: CONFIG_ACPI_HOTPLUG_MEMORY not enabled (a pc-dimm is never noticed)!" ; exit 1)

    # The initrd is lz4 (see the initrd stage). Without this the kernel does not
    # recognise the archive at all and the VM does not boot.
    grep -q "CONFIG_RD_LZ4=y" .config || (echo "ERROR: CONFIG_RD_LZ4 not enabled (the lz4 initramfs cannot be unpacked)!" ; exit 1)

    # Boot performance: the RAID6 PQ benchmark probes all SIMD implementations at
    # boot to pick the fastest, adding noticeable latency. It must stay disabled.
    # (RAID6_PQ itself is not selected today, so the symbol is normally absent.)
    ! grep -q "CONFIG_RAID6_PQ_BENCHMARK=y" .config || (echo "ERROR: CONFIG_RAID6_PQ_BENCHMARK must not be enabled (boot perf)!" ; exit 1)


    # Show what virtio and network options are actually set
    echo "Virtio configuration:"
    grep "CONFIG_VIRTIO" .config | grep -v "^#" || echo "No VIRTIO options enabled!"
    echo ""
    echo "Network device configuration:"
    grep -E "CONFIG_NETDEVICES|CONFIG_NET_CORE|CONFIG_ETHERNET|CONFIG_VIRTIO_NET" .config | grep -v "^#"

    # Verify config against Docker requirements
    echo "Verifying kernel config for Docker support..."
    /usr/local/bin/check-docker-config.sh /usr/src/linux/.config || (echo "Kernel config verification failed!" ; exit 1)

    echo "Using kernel config from build/kernel/config-${KERNEL_VERSION}-${KERNEL_ARCH}"
EOT

# Compile the kernel (separate from base to allow config construction from fragments in the future)
FROM kernel-build-base AS kernel-build

ARG KERNEL_ARCH
ARG KERNEL_NPROC

# Compile the kernel and modules
# Note: No cache mount here - Docker's layer cache is more effective
# since kernel compilation is not incremental between builds
RUN cd linux && make ARCH=${KERNEL_ARCH} -j${KERNEL_NPROC} all

RUN <<EOT
    set -e
    cd linux
    mkdir /build
    cp .config /build/kernel-config

    # Only x86_64 is supported
    if [ "${KERNEL_ARCH}" != "x86_64" ]; then
        echo "ERROR: Only x86_64 architecture is supported, got: ${KERNEL_ARCH}"
        exit 1
    fi

    cp vmlinux /build/kernel
EOT

# ============================================================================
# Go Binary Build Stages
# ============================================================================

FROM base AS shim-build

WORKDIR /go/src/github.com/containerd/spinbox

ARG GO_DEBUG_GCFLAGS
ARG GO_GCFLAGS
ARG GO_BUILD_FLAGS
ARG GO_LDFLAGS
ARG TARGETOS
ARG TARGETARCH
ARG TARGETPLATFORM

RUN --mount=type=bind,target=.,rw \
    --mount=type=cache,target=/root/.cache/go-build,id=shim-build-$TARGETPLATFORM \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build ${GO_DEBUG_GCFLAGS} ${GO_GCFLAGS} ${GO_BUILD_FLAGS} -o /build/containerd-shim-spinbox-v1 ${GO_LDFLAGS} -tags 'no_grpc' ./cmd/containerd-shim-spinbox-v1

FROM base AS vminit-build

WORKDIR /go/src/github.com/containerd/spinbox

ARG GO_DEBUG_GCFLAGS
ARG GO_GCFLAGS
ARG GO_BUILD_FLAGS
ARG TARGETPLATFORM
# VMINITD_CACHE_BUST forces rebuild when set to a new value (e.g., git commit SHA)
# This prevents stale vminitd binaries from being served from BuildKit cache
ARG VMINITD_CACHE_BUST

RUN --mount=type=bind,target=.,rw \
    --mount=type=cache,target=/root/.cache/go-build,id=vminit-build-$TARGETPLATFORM \
    echo "Building vminitd (cache bust: ${VMINITD_CACHE_BUST:-none})" && \
    go build ${GO_DEBUG_GCFLAGS} ${GO_GCFLAGS} ${GO_BUILD_FLAGS} -o /build/vminitd -ldflags '-extldflags \"-static\" -s -w' -tags 'osusergo netgo static_build'  ./cmd/vminitd

FROM base AS crun-build
ARG TARGETARCH
WORKDIR /usr/src/crun

ARG CRUN_VERSION="1.26"
# Download crun binary (cached across builds using cache mount)
RUN --mount=type=cache,sharing=locked,id=crun-download,target=/var/cache/crun \
    mkdir -p /build && \
    if [ ! -f "/var/cache/crun/crun-${CRUN_VERSION}-linux-${TARGETARCH}-disable-systemd" ]; then \
        echo "Downloading crun ${CRUN_VERSION} for ${TARGETARCH}..."; \
        wget -O "/var/cache/crun/crun-${CRUN_VERSION}-linux-${TARGETARCH}-disable-systemd" \
            https://github.com/containers/crun/releases/download/${CRUN_VERSION}/crun-${CRUN_VERSION}-linux-${TARGETARCH}-disable-systemd; \
    else \
        echo "Using cached crun binary for ${TARGETARCH}"; \
    fi && \
    cp "/var/cache/crun/crun-${CRUN_VERSION}-linux-${TARGETARCH}-disable-systemd" /build/crun && \
    chmod +x /build/crun

# ============================================================================
# initrd Build Stage
# ============================================================================

FROM base AS initrd-build
WORKDIR /usr/src/init
ARG TARGETPLATFORM
RUN --mount=type=cache,sharing=locked,id=initrd-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=initrd-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y --no-install-recommends cpio kmod lz4

RUN mkdir -p sbin bin proc sys tmp run lib/modules

COPY --from=vminit-build /build/vminitd ./init
COPY --from=crun-build /build/crun ./sbin/crun

RUN <<EOT
    set -e
    chmod +x sbin/crun

    # Run depmod to generate module dependencies
    # Find the kernel version directory
    KERNEL_VERSION=$(ls lib/modules/)
    if [ -n "${KERNEL_VERSION}" ]; then
        echo "Running depmod for kernel version: ${KERNEL_VERSION}"
        depmod -b . ${KERNEL_VERSION}
    fi

    mkdir /build
    # lz4 and not gzip, and the reason is in the boot profile: unpacking this
    # archive is the single largest item in kernel boot. KMSG_GAP measured 41.6 ms
    # of silence ending at "Freeing initrd memory: 6584K" - 34% of the kernel's
    # 122 ms, more than acpi_init and every other initcall put together, and it
    # was invisible until the gap report because no initcall owns it.
    #
    # gzip -9 optimises the artefact and charges every boot for it: gzip is among
    # the slowest formats to *decompress*, and -9 changes only how hard the build
    # tries. Measured on one machine, same kernel and same QEMU, from "Unpacking
    # initramfs..." to "Freeing initrd memory":
    #
    #     gzip -9    58.7 ms   6,738,472 bytes
    #     lz4 -l -9   9.5 ms   7,834,657 bytes
    #
    # 49 ms of every VM's boot for 1.05 MB (+16%) of a file that is read from local
    # disk once per VM. Uncompressed would be the floor, and is one word from here,
    # but it triples what QEMU copies into guest RAM at launch - which moves the
    # cost to qemu_launch_us rather than removing it, and has to be measured on
    # both sides before it is worth taking.
    #
    # -l is not optional: the kernel's initramfs decompressor expects lz4's legacy
    # frame format, and a default-framed archive is not recognised at all - which
    # presents as a VM that does not boot, not as a warning.
    (find . -print0 | cpio --null -H newc -o ) | lz4 -l -9 > /build/spinbox-initrd
EOT

# ============================================================================
# Output Stages (minimal scratch images with artifacts)
# ============================================================================

FROM scratch AS kernel
ARG KERNEL_ARCH="x86_64"
COPY --from=kernel-build /build/kernel /spinbox-kernel-${KERNEL_ARCH}
COPY --from=kernel-build /build/kernel-config /kernel-config

FROM scratch AS initrd
COPY --from=initrd-build /build/spinbox-initrd /spinbox-initrd

FROM scratch AS shim
COPY --from=shim-build /build/containerd-shim-spinbox-v1 /containerd-shim-spinbox-v1
