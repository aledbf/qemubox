#!/bin/bash
set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo "================================================"
echo "  Beacon Installation Script"
echo "================================================"
echo ""

# Check if running as root
if [[ $EUID -ne 0 ]]; then
   echo -e "${RED}Error: This script must be run as root${NC}"
   echo "Please run: sudo $0"
   exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "📂 Installing files..."

# Install binaries
echo "  → Installing binaries to /usr/share/beacon/bin..."
mkdir -p /usr/share/beacon/bin
cp -r "${SCRIPT_DIR}/usr/share/beacon/bin/"* /usr/share/beacon/bin/
chmod +x /usr/share/beacon/bin/*
echo -e "    ${GREEN}✓${NC} Binaries installed"

# Install kernel and initrd
echo "  → Installing kernel and initrd to /usr/share/beacon/kernel..."
mkdir -p /usr/share/beacon/kernel
cp -r "${SCRIPT_DIR}/usr/share/beacon/kernel/"* /usr/share/beacon/kernel/
echo -e "    ${GREEN}✓${NC} Kernel and initrd installed"

# Install configuration files
echo "  → Installing configuration files to /usr/share/beacon/config..."
mkdir -p /usr/share/beacon/config
cp -r "${SCRIPT_DIR}/usr/share/beacon/config/"* /usr/share/beacon/config/
echo -e "    ${GREEN}✓${NC} Configuration files installed"

# Install CNI plugins
if [ -d "${SCRIPT_DIR}/usr/share/beacon/libexec/cni" ]; then
    echo "  → Installing CNI plugins to /usr/share/beacon/libexec/cni..."
    mkdir -p /usr/share/beacon/libexec/cni
    cp -r "${SCRIPT_DIR}/usr/share/beacon/libexec/cni/"* /usr/share/beacon/libexec/cni/
    chmod +x /usr/share/beacon/libexec/cni/*
    echo -e "    ${GREEN}✓${NC} CNI plugins installed"
fi

# Install scripts
echo "  → Installing scripts to /usr/share/beacon..."
cp "${SCRIPT_DIR}/install.sh" /usr/share/beacon/
cp "${SCRIPT_DIR}/uninstall.sh" /usr/share/beacon/
chmod +x /usr/share/beacon/install.sh
chmod +x /usr/share/beacon/uninstall.sh
echo -e "    ${GREEN}✓${NC} Scripts installed"

# Install state directories
echo "  → Creating state directories..."
mkdir -p /var/lib/beacon/containerd
mkdir -p /run/beacon/containerd
mkdir -p /var/run/beacon
mkdir -p /var/log/beacon
echo -e "    ${GREEN}✓${NC} State directories created"

# Install systemd services
echo "  → Installing systemd services to /usr/share/beacon/systemd..."
mkdir -p /usr/share/beacon/systemd
cp "${SCRIPT_DIR}/usr/share/beacon/systemd/"*.service /usr/share/beacon/systemd/
echo -e "    ${GREEN}✓${NC} Systemd service files copied"

echo "  → Creating symlinks in /etc/systemd/system..."
ln -sf /usr/share/beacon/systemd/beacon-containerd.service /etc/systemd/system/beacon-containerd.service
ln -sf /usr/share/beacon/systemd/beacon-buildkit.service /etc/systemd/system/beacon-buildkit.service
systemctl daemon-reload
echo -e "    ${GREEN}✓${NC} Systemd services installed and linked"

echo ""
echo "🔍 Verifying installation..."

# Check required files
ERRORS=0

check_file() {
    if [ ! -f "$1" ]; then
        echo -e "  ${RED}✗${NC} Missing: $1"
        ERRORS=$((ERRORS + 1))
    else
        echo -e "  ${GREEN}✓${NC} Found: $1"
    fi
}

check_file "/usr/share/beacon/bin/containerd"
check_file "/usr/share/beacon/bin/containerd-shim-runc-v2"
check_file "/usr/share/beacon/bin/containerd-shim-beaconbox-v1"
check_file "/usr/share/beacon/bin/ctr"
check_file "/usr/share/beacon/bin/runc"
check_file "/usr/share/beacon/bin/nerdctl"
check_file "/usr/share/beacon/bin/buildkitd"
check_file "/usr/share/beacon/bin/buildctl"
check_file "/usr/share/beacon/bin/qemu-system-x86_64"
check_file "/usr/share/beacon/kernel/beacon-kernel-x86_64"
check_file "/usr/share/beacon/kernel/beacon-initrd"
check_file "/usr/share/beacon/config/containerd/config.toml"
check_file "/usr/share/beacon/config/buildkit/buildkitd.toml"
check_file "/usr/share/beacon/systemd/beacon-containerd.service"
check_file "/usr/share/beacon/systemd/beacon-buildkit.service"
check_file "/etc/systemd/system/beacon-containerd.service"
check_file "/etc/systemd/system/beacon-buildkit.service"

# Check CNI plugins
CNI_PLUGINS=(bridge host-local loopback)
for plugin in "${CNI_PLUGINS[@]}"; do
    check_file "/usr/share/beacon/libexec/cni/${plugin}"
done

echo ""
if [ $ERRORS -eq 0 ]; then
    echo -e "${GREEN}✓ Installation verification passed${NC}"
    echo ""
    echo "================================================"
    echo "  Installation Complete!"
    echo "================================================"
    echo ""
    echo "Next steps:"
    echo "  1. Enable and start containerd:"
    echo "     systemctl enable beacon-containerd"
    echo "     systemctl start beacon-containerd"
    echo ""
    echo "  2. (Optional) Enable and start buildkit:"
    echo "     systemctl enable beacon-buildkit"
    echo "     systemctl start beacon-buildkit"
    echo ""
    echo "  3. Check service status:"
    echo "     systemctl status beacon-containerd"
    echo "     systemctl status beacon-buildkit"
    echo ""
    echo "  4. Add /usr/share/beacon/bin to PATH:"
    echo "     export PATH=/usr/share/beacon/bin:\$PATH"
    echo ""
else
    echo -e "${RED}✗ Installation verification failed with $ERRORS error(s)${NC}"
    echo "Please check the errors above and ensure all required files are present."
    exit 1
fi
