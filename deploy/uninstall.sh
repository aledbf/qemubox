#!/bin/bash
set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo "================================================"
echo "  Spinbox Uninstallation Script"
echo "================================================"
echo ""

# Check if running as root
if [[ $EUID -ne 0 ]]; then
   echo -e "${RED}Error: This script must be run as root${NC}"
   echo "Please run: sudo $0"
   exit 1
fi

# Confirmation prompt
echo -e "${YELLOW}WARNING: This will remove all Spinbox components and data.${NC}"
echo "This includes:"
echo "  - All binaries in /usr/share/spinbox"
echo "  - All configuration files in /usr/share/spinbox/config"
echo "  - All state data in /var/lib/spinbox (containers, images, etc.)"
echo "  - Systemd service files"
echo ""
read -p "Are you sure you want to continue? (yes/no): " -r
echo ""
if [[ ! $REPLY =~ ^[Yy][Ee][Ss]$ ]]; then
    echo "Uninstallation cancelled."
    exit 0
fi

echo "🛑 Stopping services..."

# Stop and disable services
if systemctl is-active --quiet spinbox-containerd; then
    echo "  → Stopping spinbox-containerd service..."
    systemctl stop spinbox-containerd
    echo -e "    ${GREEN}✓${NC} spinbox-containerd stopped"
fi

if systemctl is-enabled --quiet spinbox-containerd 2>/dev/null; then
    echo "  → Disabling spinbox-containerd service..."
    systemctl disable spinbox-containerd
    echo -e "    ${GREEN}✓${NC} spinbox-containerd disabled"
fi

echo ""
echo "🗑️  Removing files..."

# Remove systemd service symlinks
if [ -L /etc/systemd/system/spinbox-containerd.service ]; then
    echo "  → Removing spinbox-containerd.service symlink..."
    rm -f /etc/systemd/system/spinbox-containerd.service
    echo -e "    ${GREEN}✓${NC} spinbox-containerd.service symlink removed"
fi

systemctl daemon-reload
echo -e "  ${GREEN}✓${NC} Systemd reloaded"

# Remove binaries and configuration
if [ -d /usr/share/spinbox ]; then
    echo "  → Removing /usr/share/spinbox..."
    rm -rf /usr/share/spinbox
    echo -e "    ${GREEN}✓${NC} /usr/share/spinbox removed"
fi

# Remove state data
if [ -d /var/lib/spinbox ]; then
    echo "  → Removing /var/lib/spinbox..."
    rm -rf /var/lib/spinbox
    echo -e "    ${GREEN}✓${NC} /var/lib/spinbox removed"
fi

# Remove runtime directories
if [ -d /run/spinbox ]; then
    echo "  → Removing /run/spinbox..."
    rm -rf /run/spinbox
    echo -e "    ${GREEN}✓${NC} /run/spinbox removed"
fi

if [ -d /var/run/spinbox ]; then
    echo "  → Removing /var/run/spinbox..."
    rm -rf /var/run/spinbox
    echo -e "    ${GREEN}✓${NC} /var/run/spinbox removed"
fi

# Remove socket if it exists
if [ -S /var/run/spinbox/containerd.sock ]; then
    echo "  → Removing containerd socket..."
    rm -f /var/run/spinbox/containerd.sock
    echo -e "    ${GREEN}✓${NC} Socket removed"
fi

echo ""
echo "================================================"
echo -e "${GREEN}✓ Uninstallation Complete!${NC}"
echo "================================================"
echo ""
echo "All Spinbox components have been removed from your system."
echo ""
