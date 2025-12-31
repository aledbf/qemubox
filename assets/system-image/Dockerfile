# Qemubox Workspace Image
# Uses systemd as PID 1
# Designed to run inside qemubox VM-isolated containers
#
# Build: task build
FROM ubuntu:25.10

ENV DEBIAN_FRONTEND=noninteractive

# Install system packages
RUN apt-get update && apt-get install -y --no-install-recommends \
    systemd systemd-sysv \
    openssh-server \
    git \
    curl wget ca-certificates \
    make \
    sudo \
    jq \
    procps \
    btop \
    iputils-ping dnsutils netcat-openbsd \
    iproute2 isal fuse-overlayfs net-tools \
    iptables arptables ebtables nftables \
    bash \
    dbus-broker \
    diffutils \
    gawk \
    grep \
    gzip xz-utils \
    kmod \
    less \
    nano \
    sed \
    strace \
    udev \
    util-linux \
    pciutils ethtool \
    qemu-guest-agent \
    && rm -rf /var/lib/apt/lists/*

# Version configuration for development tools
ARG TASK_VERSION=3.45.5
ARG GIT_LFS_VERSION=3.7.0

# Copy build scripts
COPY scripts/* /tmp/

# Install development tools
RUN chmod +x /tmp/install-dev-tools.sh && /tmp/install-dev-tools.sh && \
    chmod +x /tmp/configure-system.sh && /tmp/configure-system.sh && \
    chmod +x /tmp/optimize-systemd.sh && /tmp/optimize-systemd.sh

# Copy configuration files
COPY rootfs/ /

EXPOSE 22
STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/sbin/init"]
