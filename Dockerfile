# Build the Linux kernel, initrd, and containerd shim for running spinbox
# This multi-stage build produces:
# - Custom Linux kernel with container/virtualization support
# - initrd with vminitd and crun
# - containerd shim for spinbox runtime

# Base image versions
ARG GO_VERSION=1.25.5
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

ARG KERNEL_VERSION="6.18"
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
    if [ ! -f "/var/cache/kernel/linux-${KERNEL_VERSION}.tar.xz" ]; then \
        wget -O "/var/cache/kernel/linux-${KERNEL_VERSION}.tar.xz" https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz; \
    fi && \
    tar -xf "/var/cache/kernel/linux-${KERNEL_VERSION}.tar.xz" -C /usr/src && \
    mv linux-${KERNEL_VERSION} linux

# Copy kernel config from repository
COPY build/kernel/config-6.18-x86_64 /usr/src/linux/.config

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
    
    # Show what virtio and network options are actually set
    echo "Virtio configuration:"
    grep "CONFIG_VIRTIO" .config | grep -v "^#" || echo "No VIRTIO options enabled!"
    echo ""
    echo "Network device configuration:"
    grep -E "CONFIG_NETDEVICES|CONFIG_NET_CORE|CONFIG_ETHERNET|CONFIG_VIRTIO_NET" .config | grep -v "^#"

    # Verify config against Docker requirements
    echo "Verifying kernel config for Docker support..."
    /usr/local/bin/check-docker-config.sh /usr/src/linux/.config || (echo "Kernel config verification failed!" ; exit 1)

    echo "Using kernel config from build/kernel/config-6.18-x86_64"
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

RUN --mount=type=bind,target=.,rw \
    --mount=type=cache,target=/root/.cache/go-build,id=vminit-build-$TARGETPLATFORM \
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
        apt-get update && apt-get install -y --no-install-recommends cpio kmod

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
    (find . -print0 | cpio --null -H newc -o ) | gzip -9 > /build/spinbox-initrd
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
