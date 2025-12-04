# Build the Linux kernel, initrd, and containerd shim for running beaconbox
# This multi-stage build produces:
# - Custom Linux kernel with container/virtualization support
# - initrd with vminitd and crun
# - containerd shim for beaconbox runtime

# Base image versions
ARG GO_VERSION=1.25.4
ARG BASE_DEBIAN_DISTRO="bookworm"
ARG GOLANG_IMAGE="golang:${GO_VERSION}-${BASE_DEBIAN_DISTRO}"
ARG DOCKER_VERSION=28.4.0
ARG DOCKER_IMAGE="docker:${DOCKER_VERSION}-cli"


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

RUN wget https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz && \
    tar -xf linux-${KERNEL_VERSION}.tar.xz && \
    rm linux-${KERNEL_VERSION}.tar.xz && \
    mv linux-${KERNEL_VERSION} linux

# Copy kernel config from repository
COPY kernel/config-6.18-x86_64 /usr/src/linux/.config

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

    echo "Using kernel config from kernel/config-6.18-x86_64"
EOT

# Compile the kernel (separate from base to allow config construction from fragments in the future)
FROM kernel-build-base AS kernel-build

ARG KERNEL_ARCH
ARG KERNEL_NPROC

# Compile the kernel and modules
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

WORKDIR /go/src/github.com/containerd/beaconbox

ARG GO_DEBUG_GCFLAGS
ARG GO_GCFLAGS
ARG GO_BUILD_FLAGS
ARG GO_LDFLAGS
ARG TARGETOS
ARG TARGETARCH
ARG TARGETPLATFORM

RUN --mount=type=bind,target=.,rw \
    --mount=type=cache,target=/root/.cache/go-build,id=shim-build-$TARGETPLATFORM \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build ${GO_DEBUG_GCFLAGS} ${GO_GCFLAGS} ${GO_BUILD_FLAGS} -o /build/containerd-shim-beaconbox-v1 ${GO_LDFLAGS} -tags 'no_grpc' ./cmd/containerd-shim-beaconbox-v1

FROM base AS vminit-build

WORKDIR /go/src/github.com/containerd/beaconbox

ARG GO_DEBUG_GCFLAGS
ARG GO_GCFLAGS
ARG GO_BUILD_FLAGS
ARG TARGETPLATFORM

RUN --mount=type=bind,target=.,rw \
    --mount=type=cache,target=/root/.cache/go-build,id=vminit-build-$TARGETPLATFORM \
    go build ${GO_DEBUG_GCFLAGS} ${GO_GCFLAGS} ${GO_BUILD_FLAGS} -o /build/vminitd -ldflags '-extldflags \"-static\" -s -w' -tags 'osusergo netgo static_build no_grpc'  ./cmd/vminitd

FROM base AS crun-build
ARG TARGETARCH
WORKDIR /usr/src/crun

RUN mkdir /build && \
    wget -O /build/crun https://github.com/containers/crun/releases/download/1.25.1/crun-1.25.1-linux-${TARGETARCH}-disable-systemd

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
    (find . -print0 | cpio --null -H newc -o ) | gzip -9 > /build/beacon-initrd
EOT

# ============================================================================
# Output Stages (minimal scratch images with artifacts)
# ============================================================================

FROM scratch AS kernel
ARG KERNEL_ARCH="x86_64"
COPY --from=kernel-build /build/kernel /beacon-kernel-${KERNEL_ARCH}
COPY --from=kernel-build /build/kernel-config /kernel-config

FROM scratch AS initrd
COPY --from=initrd-build /build/beacon-initrd /beacon-initrd

FROM scratch AS shim
COPY --from=shim-build /build/containerd-shim-beaconbox-v1 /containerd-shim-beaconbox-v1

# ============================================================================
# Development Environment
# ============================================================================

FROM "${DOCKER_IMAGE}" AS docker-cli

FROM "${GOLANG_IMAGE}" AS dlv
RUN go install github.com/go-delve/delve/cmd/dlv@latest

FROM ${GOLANG_IMAGE} AS dev
ARG CONTAINERD_VERSION=2.1.4
ARG TARGETARCH

ENV PATH=/go/src/github.com/aledbf/beacon/containerd/_output:$PATH
WORKDIR /go/src/github.com/containerd/beaconbox

RUN --mount=type=cache,sharing=locked,id=dev-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=dev-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y erofs-utils git make wget

RUN wget https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${TARGETARCH}.tar.gz && \
    tar -C /usr/local/bin --strip-components=1 -xf containerd-${CONTAINERD_VERSION}-linux-${TARGETARCH}.tar.gz && \
    rm containerd-${CONTAINERD_VERSION}-linux-${TARGETARCH}.tar.gz

COPY --from=docker-cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker-cli /usr/local/libexec/docker/cli-plugins/docker-buildx /usr/local/libexec/docker/cli-plugins/docker-buildx

COPY --from=dlv /go/bin/dlv /usr/local/bin/dlv

VOLUME /var/lib/containerd
