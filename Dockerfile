#   Copyright The containerd Authors.

#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at

#       http://www.apache.org/licenses/LICENSE-2.0

#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.

# -----------------------------------------------------------------------------
# syntax=docker/dockerfile:1

# Build the Linux kernel, initrd ,and containerd shim for running nerbox

ARG XX_VERSION=1.6.1
ARG GO_VERSION=1.25.4
ARG BASE_DEBIAN_DISTRO="bookworm"
ARG GOLANG_IMAGE="golang:${GO_VERSION}-${BASE_DEBIAN_DISTRO}"
ARG GOLANGCI_LINT_VERSION=2.5.0
ARG GOLANGCI_FROM_SOURCE=false
ARG DOCKER_VERSION=28.4.0
ARG DOCKER_IMAGE="docker:${DOCKER_VERSION}-cli"
ARG RUST_IMAGE="rust:1.89.0-slim-${BASE_DEBIAN_DISTRO}"

# xx is a helper for cross-compilation
FROM --platform=$BUILDPLATFORM tonistiigi/xx:${XX_VERSION} AS xx

FROM --platform=$BUILDPLATFORM ${GOLANG_IMAGE} AS base

RUN echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache
RUN apt-get update && apt-get install --no-install-recommends -y file apparmor curl

FROM base AS kernel-build-base

# Set environment variables for non-interactive installations
ENV DEBIAN_FRONTEND=noninteractive

RUN echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache

# Install build dependencies
RUN --mount=type=cache,sharing=locked,id=kernel-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=kernel-aptcache,target=/var/cache/apt \
        apt-get update && apt-get install -y build-essential libncurses-dev flex bison libssl-dev libelf-dev bc cpio git wget xz-utils curl

ARG KERNEL_VERSION="6.17.9"
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
COPY kernel/config-6.17.9-x86_64 /usr/src/linux/.config

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

    echo "Using kernel config from kernel/config-6.17.9-x86_64"
EOT

# Build the kernel
# Seperate from base to allow config construction from fragments in the future
FROM kernel-build-base AS kernel-build

# Compile the kernel and modules
RUN cd linux && make -j${KERNEL_NPROC} all

RUN <<EOT
    set -e
    cd linux
    mkdir /build
    cp .config /build/kernel-config

    case $(uname -m) in
        x86_64) cp vmlinux /build/kernel ;;
        aarch64) cp arch/arm64/boot/Image /build/kernel ;;
        *) echo "Unsupported architecture: $(uname -m)" ; exit 1 ;;
    esac
EOT

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
WORKDIR /usr/src/crun

RUN <<EOT
    mkdir /build
    case $(uname -m) in
        x86_64) ARCH=amd64 ;;
        aarch64) ARCH=arm64 ;;
        *) echo "Unsupported architecture: $(uname -m)" ; exit 1 ;;
    esac
    wget -O /build/crun https://github.com/containers/crun/releases/download/1.25.1/crun-1.25.1-linux-${ARCH}-disable-systemd
EOT

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

FROM scratch AS kernel
ARG KERNEL_ARCH="x86_64"
COPY --from=kernel-build /build/kernel /beacon-kernel-${KERNEL_ARCH}
COPY --from=kernel-build /build/kernel /beacon-kernel-${KERNEL_ARCH}
COPY --from=kernel-build /build/kernel-config /kernel-config

FROM scratch AS initrd
COPY --from=initrd-build /build/beacon-initrd /beacon-initrd

FROM scratch AS shim
COPY --from=shim-build /build/containerd-shim-beaconbox-v1 /containerd-shim-beaconbox-v1

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


FROM base AS golangci-build
WORKDIR /src
ARG GOLANGCI_LINT_VERSION
ADD https://github.com/golangci/golangci-lint.git#v${GOLANGCI_LINT_VERSION} .
COPY --link --from=xx / /
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/ \
  xx-go --wrap && \
  go mod download
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/ \
  xx-go --wrap && \
  mkdir -p out && \
  go build -o /out/golangci-lint ./cmd/golangci-lint

FROM scratch AS golangci-binary-false
FROM scratch AS golangci-binary-true
COPY --from=golangci-build /out/golangci-lint golangci-lint
FROM golangci-binary-${GOLANGCI_FROM_SOURCE} AS golangci-binary

FROM base AS lint-base
ENV GOFLAGS="-buildvcs=false"
RUN <<EOT
apt-get update
apt-get install -y --no-install-recommends gcc libc6-dev yamllint
rm -rf /var/lib/apt/lists/*
EOT
ARG GOLANGCI_LINT_VERSION
ARG GOLANGCI_FROM_SOURCE
COPY --link --from=golangci-binary / /usr/bin/
RUN [ "${GOLANGCI_FROM_SOURCE}" = "true" ] && exit 0; wget -O- -nv https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s v${GOLANGCI_LINT_VERSION}
COPY --link --from=xx / /
WORKDIR /go/src/github.com/containerd/beaconbox

FROM lint-base AS golangci-lint
ARG TARGETNAME
ARG TARGETPLATFORM
RUN --mount=target=/go/src/github.com/containerd/beaconbox \
    --mount=target=/root/.cache,type=cache,id=lint-cache-${TARGETNAME}-${TARGETPLATFORM} \
  xx-go --wrap && \
  golangci-lint run -c .golangci.yml && \
  touch /golangci-lint.done

FROM lint-base AS golangci-verify-false
RUN --mount=target=/go/src/github.com/containerd/beaconbox \
  golangci-lint config verify && \
  touch /golangci-verify.done

FROM scratch AS golangci-verify-true
COPY <<EOF /golangci-verify.done
EOF

FROM golangci-verify-${GOLANGCI_FROM_SOURCE} AS golangci-verify

FROM lint-base AS yamllint
RUN --mount=target=/go/src/github.com/containerd/beaconbox \
  yamllint -c .yamllint.yml --strict . && \
  touch /yamllint.done

FROM scratch AS lint
COPY --link --from=golangci-lint /golangci-lint.done /
COPY --link --from=golangci-verify /golangci-verify.done /
COPY --link --from=yamllint /yamllint.done /
