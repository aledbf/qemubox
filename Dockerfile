# The guest software spinbox builds: the initrd, the shim and vminitd.
#
# Not the kernel, and not QEMU. Those are the machine, they are one versioned artefact
# built elsewhere, and `task machine` puts one under _output/. A kernel built here would
# be a second machine nobody pinned — and the artefacts go into the template fingerprint
# by content, so two of them is two fleets of templates that cannot be told apart.

# Base image versions
ARG GO_VERSION=1.27.1
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

ARG CRUN_VERSION="1.29.1"
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

FROM scratch AS initrd
COPY --from=initrd-build /build/spinbox-initrd /spinbox-initrd

FROM scratch AS shim
COPY --from=shim-build /build/containerd-shim-spinbox-v1 /containerd-shim-spinbox-v1
