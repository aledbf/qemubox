#!/bin/bash
set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo "================================================"
echo "  Qemubox Installation Script"
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
echo "  → Installing binaries to /usr/share/qemubox/bin..."
mkdir -p /usr/share/qemubox/bin
cp -r "${SCRIPT_DIR}/usr/share/qemubox/bin/"* /usr/share/qemubox/bin/
chmod +x /usr/share/qemubox/bin/*
echo -e "    ${GREEN}✓${NC} Binaries installed"

# Install kernel and initrd
echo "  → Installing kernel and initrd to /usr/share/qemubox/kernel..."
mkdir -p /usr/share/qemubox/kernel
cp -r "${SCRIPT_DIR}/usr/share/qemubox/kernel/"* /usr/share/qemubox/kernel/
echo -e "    ${GREEN}✓${NC} Kernel and initrd installed"

# Install configuration files
echo "  → Installing configuration files to /usr/share/qemubox/config..."
mkdir -p /usr/share/qemubox/config
cp -r "${SCRIPT_DIR}/usr/share/qemubox/config/"* /usr/share/qemubox/config/
echo -e "    ${GREEN}✓${NC} Configuration files installed"

# Install CNI plugins
if [ -d "${SCRIPT_DIR}/usr/share/qemubox/libexec/cni" ]; then
    echo "  → Installing CNI plugins to /usr/share/qemubox/libexec/cni..."
    mkdir -p /usr/share/qemubox/libexec/cni
    cp -r "${SCRIPT_DIR}/usr/share/qemubox/libexec/cni/"* /usr/share/qemubox/libexec/cni/
    chmod +x /usr/share/qemubox/libexec/cni/*
    echo -e "    ${GREEN}✓${NC} CNI plugins installed"
fi

# Install scripts
echo "  → Installing scripts to /usr/share/qemubox..."
cp "${SCRIPT_DIR}/install.sh" /usr/share/qemubox/
cp "${SCRIPT_DIR}/uninstall.sh" /usr/share/qemubox/
chmod +x /usr/share/qemubox/install.sh
chmod +x /usr/share/qemubox/uninstall.sh
echo -e "    ${GREEN}✓${NC} Scripts installed"

# Install state directories
echo "  → Creating state directories..."
mkdir -p /var/lib/qemubox/containerd
mkdir -p /run/qemubox/containerd
mkdir -p /run/qemubox/vm
mkdir -p /var/run/qemubox
mkdir -p /var/log/qemubox
echo -e "    ${GREEN}✓${NC} State directories created"

# Install systemd services
echo "  → Installing systemd services to /usr/share/qemubox/systemd..."
mkdir -p /usr/share/qemubox/systemd
cp "${SCRIPT_DIR}/usr/share/qemubox/systemd/"*.service /usr/share/qemubox/systemd/
echo -e "    ${GREEN}✓${NC} Systemd service files copied"

echo "  → Creating symlinks in /etc/systemd/system..."
ln -sf /usr/share/qemubox/systemd/qemubox-containerd.service /etc/systemd/system/qemubox-containerd.service
ln -sf /usr/share/qemubox/systemd/qemubox-buildkit.service /etc/systemd/system/qemubox-buildkit.service
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

check_file "/usr/share/qemubox/bin/containerd"
check_file "/usr/share/qemubox/bin/containerd-shim-runc-v2"
check_file "/usr/share/qemubox/bin/containerd-shim-qemubox-v1"
check_file "/usr/share/qemubox/bin/ctr"
check_file "/usr/share/qemubox/bin/runc"
check_file "/usr/share/qemubox/bin/nerdctl"
check_file "/usr/share/qemubox/bin/buildkitd"
check_file "/usr/share/qemubox/bin/buildctl"
check_file "/usr/share/qemubox/bin/qemu-system-x86_64"
check_file "/usr/share/qemubox/qemu/bios-256k.bin"
check_file "/usr/share/qemubox/kernel/qemubox-kernel-x86_64"
check_file "/usr/share/qemubox/kernel/qemubox-initrd"
check_file "/usr/share/qemubox/config/containerd/config.toml"
check_file "/usr/share/qemubox/config/buildkit/config.toml"
check_file "/usr/share/qemubox/config/cni/net.d/10-qemubox.conflist"
check_file "/usr/share/qemubox/systemd/qemubox-containerd.service"
check_file "/usr/share/qemubox/systemd/qemubox-buildkit.service"
check_file "/etc/systemd/system/qemubox-containerd.service"
check_file "/etc/systemd/system/qemubox-buildkit.service"

# Check CNI plugins
CNI_PLUGINS=(bridge host-local loopback)
for plugin in "${CNI_PLUGINS[@]}"; do
    check_file "/usr/share/qemubox/libexec/cni/${plugin}"
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
    echo "     systemctl enable qemubox-containerd"
    echo "     systemctl start qemubox-containerd"
    echo ""
    echo "  2. (Optional) Enable and start buildkit:"
    echo "     systemctl enable qemubox-buildkit"
    echo "     systemctl start qemubox-buildkit"
    echo ""
    echo "  3. Check service status:"
    echo "     systemctl status qemubox-containerd"
    echo "     systemctl status qemubox-buildkit"
    echo ""
    echo "  4. Add /usr/share/qemubox/bin to PATH:"
    echo "     export PATH=/usr/share/qemubox/bin:\$PATH"
    echo ""
else
    echo -e "${RED}✗ Installation verification failed with $ERRORS error(s)${NC}"
    echo "Please check the errors above and ensure all required files are present."
    exit 1
fi
