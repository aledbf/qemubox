#!/bin/bash
set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Parse command line arguments
SHIM_ONLY=false
SKIP_CHECKS=false

print_usage() {
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Options:"
    echo "  --shim-only     Install only the qemubox shim, kernel, and QEMU binaries."
    echo "                  Assumes containerd and CNI plugins are already installed."
    echo "  --skip-checks   Skip prerequisite checks in shim-only mode"
    echo "  -h, --help      Show this help message"
    echo ""
    echo "Modes:"
    echo "  Full install (default):"
    echo "    Installs everything: containerd, runc, nerdctl, CNI plugins,"
    echo "    qemubox shim, kernel, QEMU, and systemd services."
    echo ""
    echo "  Shim-only install (--shim-only):"
    echo "    Installs only: qemubox shim, vminitd, kernel, initrd, QEMU binaries/firmware."
    echo "    Requires: containerd already installed, CNI plugins available, KVM access."
}

while [[ $# -gt 0 ]]; do
    case $1 in
        --shim-only)
            SHIM_ONLY=true
            shift
            ;;
        --skip-checks)
            SKIP_CHECKS=true
            shift
            ;;
        -h|--help)
            print_usage
            exit 0
            ;;
        *)
            echo -e "${RED}Error: Unknown option: $1${NC}"
            print_usage
            exit 1
            ;;
    esac
done

echo "================================================"
if [ "$SHIM_ONLY" = true ]; then
    echo "  Qemubox Installation Script (Shim-Only Mode)"
else
    echo "  Qemubox Installation Script"
fi
echo "================================================"
echo ""

# Check if running as root
if [[ $EUID -ne 0 ]]; then
   echo -e "${RED}Error: This script must be run as root${NC}"
   echo "Please run: sudo $0"
   exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# =============================================================================
# Prerequisite checks for shim-only mode
# =============================================================================

check_command_exists() {
    local cmd="$1"
    local name="${2:-$cmd}"
    if command -v "$cmd" &> /dev/null; then
        echo -e "  ${GREEN}✓${NC} $name found: $(command -v "$cmd")"
        return 0
    else
        echo -e "  ${RED}✗${NC} $name not found in PATH"
        return 1
    fi
}

check_containerd_installed() {
    echo "Checking containerd installation..."
    if ! check_command_exists "containerd" "containerd"; then
        return 1
    fi

    # Check version
    local version
    version=$(containerd --version 2>/dev/null | head -1) || true
    if [ -n "$version" ]; then
        echo -e "  ${GREEN}✓${NC} Version: $version"
    fi
    return 0
}

check_containerd_running() {
    echo "Checking containerd status..."

    # Check for common socket locations
    local sockets=(
        "/run/containerd/containerd.sock"
        "/var/run/containerd/containerd.sock"
    )

    local found=false
    for sock in "${sockets[@]}"; do
        if [ -S "$sock" ]; then
            echo -e "  ${GREEN}✓${NC} Socket found: $sock"
            found=true
            break
        fi
    done

    if [ "$found" = false ]; then
        echo -e "  ${YELLOW}⚠${NC}  No containerd socket found (service may not be running)"
        echo "      Expected locations: ${sockets[*]}"
        return 1
    fi

    # Check if service is active (if systemd is available)
    if command -v systemctl &> /dev/null; then
        if systemctl is-active --quiet containerd 2>/dev/null; then
            echo -e "  ${GREEN}✓${NC} containerd service is active"
        else
            echo -e "  ${YELLOW}⚠${NC}  containerd service is not active (may be managed differently)"
        fi
    fi

    return 0
}

check_containerd_config() {
    echo "Checking containerd configuration for qemubox runtime..."

    local config_paths=(
        "/etc/containerd/config.toml"
    )

    local config_found=false
    local runtime_configured=false

    for cfg in "${config_paths[@]}"; do
        if [ -f "$cfg" ]; then
            config_found=true
            echo -e "  ${GREEN}✓${NC} Config found: $cfg"

            # Check if qemubox runtime is configured
            if grep -q "qemubox" "$cfg" 2>/dev/null; then
                runtime_configured=true
                echo -e "  ${GREEN}✓${NC} qemubox runtime found in config"
            fi
            break
        fi
    done

    if [ "$config_found" = false ]; then
        echo -e "  ${YELLOW}⚠${NC}  No containerd config.toml found"
        echo "      You may need to create one at /etc/containerd/config.toml"
    fi

    if [ "$runtime_configured" = false ]; then
        echo -e "  ${YELLOW}⚠${NC}  qemubox runtime not found in containerd config"
        echo ""
        echo "      Add the following to your containerd config.toml:"
        echo ""
        echo -e "      ${BLUE}[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.qemubox]${NC}"
        echo -e "      ${BLUE}  runtime_type = \"io.containerd.qemubox.v1\"${NC}"
        echo -e "      ${BLUE}  snapshotter = \"nexus-erofs\"${NC}"
        echo ""
        echo "      Note: You also need the nexus-erofs proxy plugin configured."
        echo "      See /usr/share/qemubox/config/containerd/config.toml for an example."
        echo ""
        return 1
    fi

    return 0
}

check_kvm_access() {
    echo "Checking KVM access..."

    if [ ! -e /dev/kvm ]; then
        echo -e "  ${RED}✗${NC} /dev/kvm does not exist"
        echo "      KVM is required for qemubox. Ensure your system supports virtualization."
        return 1
    fi

    if [ ! -r /dev/kvm ] || [ ! -w /dev/kvm ]; then
        echo -e "  ${YELLOW}⚠${NC}  /dev/kvm exists but may not be accessible"
        echo "      Ensure the user running containers has access to /dev/kvm"
        echo "      (e.g., add to 'kvm' group: usermod -aG kvm <user>)"
    else
        echo -e "  ${GREEN}✓${NC} /dev/kvm is accessible"
    fi

    return 0
}

check_cni_plugins() {
    echo "Checking CNI plugins..."

    local cni_paths=(
        "/opt/cni/bin"
        "/usr/lib/cni"
        "/usr/libexec/cni"
    )

    local cni_dir=""
    for path in "${cni_paths[@]}"; do
        if [ -d "$path" ]; then
            cni_dir="$path"
            echo -e "  ${GREEN}✓${NC} CNI plugin directory found: $path"
            break
        fi
    done

    if [ -z "$cni_dir" ]; then
        echo -e "  ${RED}✗${NC} No CNI plugin directory found"
        echo "      Expected locations: ${cni_paths[*]}"
        return 1
    fi

    # Check for required plugins
    local required_plugins=(bridge host-local loopback)
    local missing=0

    for plugin in "${required_plugins[@]}"; do
        if [ -x "${cni_dir}/${plugin}" ]; then
            echo -e "  ${GREEN}✓${NC} Plugin: $plugin"
        else
            echo -e "  ${RED}✗${NC} Missing plugin: $plugin"
            missing=$((missing + 1))
        fi
    done

    # Check for optional but recommended plugins
    local optional_plugins=(firewall portmap)
    for plugin in "${optional_plugins[@]}"; do
        if [ -x "${cni_dir}/${plugin}" ]; then
            echo -e "  ${GREEN}✓${NC} Plugin (optional): $plugin"
        else
            echo -e "  ${YELLOW}⚠${NC}  Optional plugin not found: $plugin"
        fi
    done

    if [ $missing -gt 0 ]; then
        echo ""
        echo "      Install CNI plugins from:"
        echo "      https://github.com/containernetworking/plugins/releases"
        return 1
    fi

    return 0
}

check_cni_config() {
    echo "Checking CNI network configuration..."

    local cni_conf_dirs=(
        "/etc/cni/net.d"
    )

    for dir in "${cni_conf_dirs[@]}"; do
        if [ -d "$dir" ]; then
            local configs
            configs=$(find "$dir" -name "*.conflist" -o -name "*.conf" 2>/dev/null | head -5)
            if [ -n "$configs" ]; then
                echo -e "  ${GREEN}✓${NC} CNI config directory: $dir"
                echo "$configs" | while read -r f; do
                    echo -e "  ${GREEN}✓${NC} Config: $(basename "$f")"
                done
                return 0
            fi
        fi
    done

    echo -e "  ${YELLOW}⚠${NC}  No CNI configuration found in /etc/cni/net.d/"
    echo "      You may need to create a CNI network configuration."
    echo "      Example available at: /usr/share/qemubox/config/cni/net.d/10-qemubox.conflist"
    return 1
}

check_qemu_dependencies() {
    echo "Checking QEMU binary dependencies..."

    local qemu_bin="${SCRIPT_DIR}/usr/share/qemubox/bin/qemu-system-x86_64"

    if [ ! -f "$qemu_bin" ]; then
        echo -e "  ${YELLOW}⚠${NC}  QEMU binary not found in release package (will skip dependency check)"
        return 0
    fi

    # Check if ldd is available
    if ! command -v ldd &> /dev/null; then
        echo -e "  ${YELLOW}⚠${NC}  ldd not available, skipping dependency check"
        return 0
    fi

    # Run ldd and check for missing libraries
    local ldd_output
    ldd_output=$(ldd "$qemu_bin" 2>&1) || true

    if echo "$ldd_output" | grep -q "not found"; then
        echo -e "  ${RED}✗${NC} Missing QEMU dependencies:"
        echo "$ldd_output" | grep "not found" | while read -r line; do
            echo -e "      ${RED}•${NC} $line"
        done
        echo ""
        echo "      Install the missing libraries before proceeding."
        return 1
    elif echo "$ldd_output" | grep -q "not a dynamic executable"; then
        echo -e "  ${GREEN}✓${NC} QEMU binary is statically linked (no external dependencies)"
        return 0
    else
        echo -e "  ${GREEN}✓${NC} All QEMU dependencies are satisfied"
        return 0
    fi
}

run_prerequisite_checks() {
    echo ""
    echo "🔍 Running prerequisite checks for shim-only installation..."
    echo ""

    local errors=0
    local warnings=0

    # Critical checks (will fail installation)
    if ! check_containerd_installed; then
        errors=$((errors + 1))
    fi
    echo ""

    if ! check_kvm_access; then
        errors=$((errors + 1))
    fi
    echo ""

    if ! check_cni_plugins; then
        errors=$((errors + 1))
    fi
    echo ""

    if ! check_qemu_dependencies; then
        errors=$((errors + 1))
    fi
    echo ""

    # Warning checks (installation continues)
    if ! check_containerd_running; then
        warnings=$((warnings + 1))
    fi
    echo ""

    if ! check_containerd_config; then
        warnings=$((warnings + 1))
    fi
    echo ""

    if ! check_cni_config; then
        warnings=$((warnings + 1))
    fi
    echo ""

    # Summary
    echo "================================================"
    if [ $errors -gt 0 ]; then
        echo -e "${RED}Prerequisite checks failed with $errors error(s)${NC}"
        echo ""
        echo "Please resolve the errors above before installing."
        echo "Use --skip-checks to bypass these checks (not recommended)."
        return 1
    elif [ $warnings -gt 0 ]; then
        echo -e "${YELLOW}Prerequisite checks passed with $warnings warning(s)${NC}"
        echo ""
        echo "Installation can proceed, but please review the warnings above."
    else
        echo -e "${GREEN}All prerequisite checks passed${NC}"
    fi
    echo "================================================"
    echo ""

    return 0
}

# Run prerequisite checks for shim-only mode
if [ "$SHIM_ONLY" = true ] && [ "$SKIP_CHECKS" = false ]; then
    if ! run_prerequisite_checks; then
        exit 1
    fi
fi

echo "📂 Installing files..."

# Install binaries
if [ "$SHIM_ONLY" = true ]; then
    echo "  → Installing shim binaries to /usr/share/qemubox/bin..."
    mkdir -p /usr/share/qemubox/bin
    # Only install qemubox-specific binaries
    for bin in containerd-shim-qemubox-v1 vminitd qemu-system-x86_64; do
        if [ -f "${SCRIPT_DIR}/usr/share/qemubox/bin/${bin}" ]; then
            cp "${SCRIPT_DIR}/usr/share/qemubox/bin/${bin}" /usr/share/qemubox/bin/
            chmod +x "/usr/share/qemubox/bin/${bin}"
        fi
    done
    echo -e "    ${GREEN}✓${NC} Shim binaries installed"
else
    echo "  → Installing binaries to /usr/share/qemubox/bin..."
    mkdir -p /usr/share/qemubox/bin
    cp -r "${SCRIPT_DIR}/usr/share/qemubox/bin/"* /usr/share/qemubox/bin/
    chmod +x /usr/share/qemubox/bin/*
    echo -e "    ${GREEN}✓${NC} Binaries installed"
fi

# Install kernel and initrd
echo "  → Installing kernel and initrd to /usr/share/qemubox/kernel..."
mkdir -p /usr/share/qemubox/kernel
cp -r "${SCRIPT_DIR}/usr/share/qemubox/kernel/"* /usr/share/qemubox/kernel/
echo -e "    ${GREEN}✓${NC} Kernel and initrd installed"

# Install configuration files (skip in shim-only mode, except qemubox config)
if [ "$SHIM_ONLY" = false ]; then
    echo "  → Installing configuration files to /usr/share/qemubox/config..."
    mkdir -p /usr/share/qemubox/config
    cp -r "${SCRIPT_DIR}/usr/share/qemubox/config/"* /usr/share/qemubox/config/
    echo -e "    ${GREEN}✓${NC} Configuration files installed"
else
    # In shim-only mode, only copy qemubox config as reference
    echo "  → Installing qemubox config reference to /usr/share/qemubox/config..."
    mkdir -p /usr/share/qemubox/config/qemubox
    if [ -f "${SCRIPT_DIR}/usr/share/qemubox/config/qemubox/config.json" ]; then
        cp "${SCRIPT_DIR}/usr/share/qemubox/config/qemubox/config.json" /usr/share/qemubox/config/qemubox/
    fi
    # Also copy CNI config as reference (user may want to use it)
    if [ -d "${SCRIPT_DIR}/usr/share/qemubox/config/cni" ]; then
        mkdir -p /usr/share/qemubox/config/cni
        cp -r "${SCRIPT_DIR}/usr/share/qemubox/config/cni/"* /usr/share/qemubox/config/cni/
    fi
    echo -e "    ${GREEN}✓${NC} Reference configuration files installed"
fi

# Install QEMU firmware files
if [ -d "${SCRIPT_DIR}/usr/share/qemubox/qemu" ]; then
    echo "  → Installing QEMU firmware to /usr/share/qemubox/qemu..."
    mkdir -p /usr/share/qemubox/qemu
    cp -r "${SCRIPT_DIR}/usr/share/qemubox/qemu/"* /usr/share/qemubox/qemu/
    echo -e "    ${GREEN}✓${NC} QEMU firmware installed"
fi

# Install CNI plugins (skip in shim-only mode - assumes system CNI plugins)
if [ "$SHIM_ONLY" = false ]; then
    if [ -d "${SCRIPT_DIR}/usr/share/qemubox/libexec/cni" ]; then
        echo "  → Installing CNI plugins to /usr/share/qemubox/libexec/cni..."
        mkdir -p /usr/share/qemubox/libexec/cni
        cp -r "${SCRIPT_DIR}/usr/share/qemubox/libexec/cni/"* /usr/share/qemubox/libexec/cni/
        chmod +x /usr/share/qemubox/libexec/cni/*
        echo -e "    ${GREEN}✓${NC} CNI plugins installed"
    fi
else
    echo -e "  ${YELLOW}→${NC} Skipping CNI plugins (shim-only mode assumes system CNI plugins)"
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
mkdir -p /var/lib/qemubox
mkdir -p /var/run/qemubox
mkdir -p /var/log/qemubox
mkdir -p /run/qemubox/vm
if [ "$SHIM_ONLY" = false ]; then
    # Full install: create containerd-specific directories
    mkdir -p /var/lib/qemubox/containerd
    mkdir -p /run/qemubox/containerd
    mkdir -p /run/qemubox/containerd/fifo
    # nexus-erofs snapshotter directories
    mkdir -p /var/lib/qemubox/nexus-erofs-snapshotter
    mkdir -p /run/qemubox/nexus-erofs-snapshotter
fi
echo -e "    ${GREEN}✓${NC} State directories created"

# Install qemubox configuration file
echo "  → Installing qemubox configuration..."
mkdir -p /etc/qemubox
if [ ! -f /etc/qemubox/config.json ]; then
    echo "    → Creating /etc/qemubox/config.json from example..."
    cp "${SCRIPT_DIR}/usr/share/qemubox/config/qemubox/config.json" /etc/qemubox/config.json
    echo -e "    ${GREEN}✓${NC} Configuration file created at /etc/qemubox/config.json"
else
    echo -e "    ${YELLOW}⚠${NC}  /etc/qemubox/config.json already exists, skipping (preserving existing configuration)"
    echo "      → Example config available at /usr/share/qemubox/config/qemubox/config.json"
fi

# Install systemd services (skip in shim-only mode)
if [ "$SHIM_ONLY" = false ]; then
    echo "  → Installing systemd services to /usr/share/qemubox/systemd..."
    mkdir -p /usr/share/qemubox/systemd
    cp "${SCRIPT_DIR}/usr/share/qemubox/systemd/"*.service /usr/share/qemubox/systemd/
    echo -e "    ${GREEN}✓${NC} Systemd service files copied"

    echo "  → Creating symlinks in /etc/systemd/system..."
    ln -sf /usr/share/qemubox/systemd/qemubox-nexus-erofs-snapshotter.service /etc/systemd/system/qemubox-nexus-erofs-snapshotter.service
    ln -sf /usr/share/qemubox/systemd/qemubox-containerd.service /etc/systemd/system/qemubox-containerd.service
    systemctl daemon-reload
    echo -e "    ${GREEN}✓${NC} Systemd services installed and linked"
else
    echo -e "  ${YELLOW}→${NC} Skipping systemd services (shim-only mode uses existing containerd)"
fi

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

if [ "$SHIM_ONLY" = true ]; then
    # Shim-only mode: verify only qemubox-specific components
    echo "Verifying shim-only installation..."
    check_file "/usr/share/qemubox/bin/containerd-shim-qemubox-v1"
    check_file "/usr/share/qemubox/bin/vminitd"
    check_file "/usr/share/qemubox/bin/qemu-system-x86_64"
    check_file "/usr/share/qemubox/qemu/bios-256k.bin"
    check_file "/usr/share/qemubox/qemu/kvmvapic.bin"
    check_file "/usr/share/qemubox/qemu/vgabios-stdvga.bin"
    check_file "/usr/share/qemubox/kernel/qemubox-kernel-x86_64"
    check_file "/usr/share/qemubox/kernel/qemubox-initrd"
    check_file "/usr/share/qemubox/config/qemubox/config.json"
    check_file "/etc/qemubox/config.json"
else
    # Full installation: verify all components
    echo "Verifying full installation..."
    check_file "/usr/share/qemubox/bin/containerd"
    check_file "/usr/share/qemubox/bin/containerd-shim-runc-v2"
    check_file "/usr/share/qemubox/bin/containerd-shim-qemubox-v1"
    check_file "/usr/share/qemubox/bin/nexus-erofs-snapshotter"
    check_file "/usr/share/qemubox/bin/ctr"
    check_file "/usr/share/qemubox/bin/runc"
    check_file "/usr/share/qemubox/bin/nerdctl"
    check_file "/usr/share/qemubox/bin/qemu-system-x86_64"
    check_file "/usr/share/qemubox/qemu/bios-256k.bin"
    check_file "/usr/share/qemubox/qemu/kvmvapic.bin"
    check_file "/usr/share/qemubox/qemu/vgabios-stdvga.bin"
    check_file "/usr/share/qemubox/kernel/qemubox-kernel-x86_64"
    check_file "/usr/share/qemubox/kernel/qemubox-initrd"
    check_file "/usr/share/qemubox/config/containerd/config.toml"
    check_file "/usr/share/qemubox/config/cni/net.d/10-qemubox.conflist"
    check_file "/usr/share/qemubox/config/qemubox/config.json"
    check_file "/etc/qemubox/config.json"
    check_file "/usr/share/qemubox/systemd/qemubox-nexus-erofs-snapshotter.service"
    check_file "/usr/share/qemubox/systemd/qemubox-containerd.service"
    check_file "/etc/systemd/system/qemubox-nexus-erofs-snapshotter.service"
    check_file "/etc/systemd/system/qemubox-containerd.service"

    # Check CNI plugins (full install only)
    CNI_PLUGINS=(bridge host-local loopback)
    for plugin in "${CNI_PLUGINS[@]}"; do
        check_file "/usr/share/qemubox/libexec/cni/${plugin}"
    done
fi

echo ""
if [ $ERRORS -eq 0 ]; then
    echo -e "${GREEN}✓ Installation verification passed${NC}"
    echo ""
    echo "================================================"
    if [ "$SHIM_ONLY" = true ]; then
        echo "  Shim-Only Installation Complete!"
    else
        echo "  Installation Complete!"
    fi
    echo "================================================"
    echo ""
    if [ "$SHIM_ONLY" = true ]; then
        echo "Next steps:"
        echo "  1. Review and customize qemubox configuration (if needed):"
        echo "     vi /etc/qemubox/config.json"
        echo ""
        echo "  2. Configure the qemubox runtime in your containerd config:"
        echo "     Add to /etc/containerd/config.toml:"
        echo ""
        echo -e "     ${BLUE}[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.qemubox]${NC}"
        echo -e "     ${BLUE}  runtime_type = \"io.containerd.qemubox.v1\"${NC}"
        echo -e "     ${BLUE}  snapshotter = \"nexus-erofs\"${NC}"
        echo ""
        echo "     Note: You also need to configure the nexus-erofs proxy plugin."
        echo "     See /usr/share/qemubox/config/containerd/config.toml for an example."
        echo ""
        echo "  3. Ensure the shim is in containerd's PATH or use absolute path:"
        echo "     Shim location: /usr/share/qemubox/bin/containerd-shim-qemubox-v1"
        echo ""
        echo "  4. Restart containerd to pick up the new runtime:"
        echo "     systemctl restart containerd"
        echo ""
        echo "  5. Test the runtime:"
        echo "     ctr run --rm --runtime io.containerd.qemubox.v1 docker.io/library/alpine:latest test echo hello"
        echo ""
    else
        echo "Next steps:"
        echo "  1. Review and customize configuration (if needed):"
        echo "     vi /etc/qemubox/config.json"
        echo "     (See /usr/share/qemubox/config/qemubox/config.json for defaults)"
        echo ""
        echo "  2. Enable and start services (snapshotter starts automatically with containerd):"
        echo "     systemctl enable qemubox-containerd"
        echo "     systemctl start qemubox-containerd"
        echo ""
        echo "  3. Check service status:"
        echo "     systemctl status qemubox-nexus-erofs-snapshotter"
        echo "     systemctl status qemubox-containerd"
        echo ""
        echo "  4. Add /usr/share/qemubox/bin to PATH:"
        echo "     export PATH=/usr/share/qemubox/bin:\$PATH"
        echo ""
    fi
else
    echo -e "${RED}✗ Installation verification failed with $ERRORS error(s)${NC}"
    echo "Please check the errors above and ensure all required files are present."
    exit 1
fi
