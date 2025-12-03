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

# Install configuration files
echo "  → Installing configuration files to /usr/share/beacon/config..."
mkdir -p /usr/share/beacon/config
cp -r "${SCRIPT_DIR}/usr/share/beacon/config/"* /usr/share/beacon/config/
echo -e "    ${GREEN}✓${NC} Configuration files installed"

# Install state directories
echo "  → Creating state directories..."
mkdir -p /var/lib/beacon/bin
mkdir -p /var/lib/beacon/containerd
mkdir -p /run/beacon/containerd
mkdir -p /var/run/beacon
if [ -d "${SCRIPT_DIR}/var/lib/beacon/bin" ] && [ "$(ls -A "${SCRIPT_DIR}/var/lib/beacon/bin")" ]; then
    cp -r "${SCRIPT_DIR}/var/lib/beacon/bin/"* /var/lib/beacon/bin/
    chmod +x /var/lib/beacon/bin/* 2>/dev/null || true
fi
echo -e "    ${GREEN}✓${NC} State directories created"

# Install systemd services
echo "  → Installing systemd services..."
cp "${SCRIPT_DIR}/systemd/containerd.service" /etc/systemd/system/
cp "${SCRIPT_DIR}/systemd/buildkit.service" /etc/systemd/system/
systemctl daemon-reload
echo -e "    ${GREEN}✓${NC} Systemd services installed"

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

check_file "/usr/share/beacon/bin/containerd-shim-beaconbox-v1"
check_file "/usr/share/beacon/bin/beacon-kernel-x86_64"
check_file "/usr/share/beacon/bin/beacon-initrd"
check_file "/usr/share/beacon/bin/nerdctl"
check_file "/usr/share/beacon/config/containerd/config.toml"
check_file "/etc/systemd/system/containerd.service"
check_file "/etc/systemd/system/buildkit.service"

# Check CNI plugins
CNI_PLUGINS=(bridge host-local loopback)
for plugin in "${CNI_PLUGINS[@]}"; do
    check_file "/usr/share/beacon/bin/${plugin}"
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
    echo "     systemctl enable containerd"
    echo "     systemctl start containerd"
    echo ""
    echo "  2. (Optional) Enable and start buildkit:"
    echo "     systemctl enable buildkit"
    echo "     systemctl start buildkit"
    echo ""
    echo "  3. Check service status:"
    echo "     systemctl status containerd"
    echo "     systemctl status buildkit"
    echo ""
    echo "  4. Add /usr/share/beacon/bin to PATH:"
    echo "     export PATH=/usr/share/beacon/bin:\$PATH"
    echo ""
else
    echo -e "${RED}✗ Installation verification failed with $ERRORS error(s)${NC}"
    echo "Please check the errors above and ensure all required files are present."
    exit 1
fi
