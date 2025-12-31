#!/usr/bin/env bash
set -euo pipefail

echo "Installing development tools..."

# -------------------------------------------------
# Version configuration
# -------------------------------------------------
TASK_VERSION="${TASK_VERSION:-3.45.5}"
GIT_LFS_VERSION="${GIT_LFS_VERSION:-3.7.0}"
BUILDKIT_VERSION="${BUILDKIT_VERSION:-0.26.2}"

# -------------------------------------------------
# Helper functions
# -------------------------------------------------
download_and_verify() {
    local url="$1"
    local output="$2"
    local expected_sha256="$3"

    echo "Downloading ${url}..."
    curl -fsSL "${url}" -o "${output}"

    if [ -n "${expected_sha256}" ]; then
        echo "Verifying checksum..."
        echo "${expected_sha256}  ${output}" | sha256sum -c -
    fi
}

# -------------------------------------------------
# Install Task (Taskfile runner)
# -------------------------------------------------
echo "Installing Task v${TASK_VERSION}..."
download_and_verify \
    "https://github.com/go-task/task/releases/download/v${TASK_VERSION}/task_linux_amd64.tar.gz" \
    "/tmp/task.tar.gz" \
    ""

tar -xzf /tmp/task.tar.gz -C /tmp
install -m 755 /tmp/task /usr/local/bin/task
task --version

# -------------------------------------------------
# Install Git LFS
# -------------------------------------------------
echo "Installing Git LFS v${GIT_LFS_VERSION}..."
download_and_verify \
    "https://github.com/git-lfs/git-lfs/releases/download/v${GIT_LFS_VERSION}/git-lfs-linux-amd64-v${GIT_LFS_VERSION}.tar.gz" \
    "/tmp/git-lfs.tar.gz" \
    ""

tar -xzf /tmp/git-lfs.tar.gz -C /tmp
install -m 755 "/tmp/git-lfs-${GIT_LFS_VERSION}/git-lfs" /usr/local/bin/git-lfs
git-lfs version

# -------------------------------------------------
# Install BuildKit
# -------------------------------------------------
echo "Installing BuildKit v${BUILDKIT_VERSION}..."
download_and_verify \
    "https://github.com/moby/buildkit/releases/download/v${BUILDKIT_VERSION}/buildkit-v${BUILDKIT_VERSION}.linux-amd64.tar.gz" \
    "/tmp/buildkit.tar.gz" \
    ""

tar -xzf /tmp/buildkit.tar.gz -C /usr/local
# Remove QEMU binaries we don't need
rm -rf /usr/local/bin/buildkit-qemu-* 2>/dev/null || true
buildctl --version

install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc

# Add the repository to Apt sources:
tee /etc/apt/sources.list.d/docker.sources <<EOF
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: $(. /etc/os-release && echo "${UBUNTU_CODENAME:-$VERSION_CODENAME}")
Components: stable
Signed-By: /etc/apt/keyrings/docker.asc
EOF

apt update

apt install -y \
    docker-ce \
    docker-ce-cli \
    containerd.io \
    docker-buildx-plugin \
    docker-compose-plugin \
    isal pigz

# -------------------------------------------------
# Cleanup
# -------------------------------------------------
echo "Cleaning up temporary files..."
rm -rf /var/tmp/* /usr/local/share/doc /usr/local/share/man

apt-get clean

echo "✅ Development tools installation complete"
