#!/bin/bash
set -euo pipefail

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# =============================================================================
# Logging helpers
# =============================================================================
log_ok()   { echo -e "  ${GREEN}✓${NC} $1"; }
log_err()  { echo -e "  ${RED}✗${NC} $1"; }
log_warn() { echo -e "  ${YELLOW}⚠${NC}  $1"; }
log_skip() { echo -e "  ${YELLOW}→${NC} $1"; }
section()  { echo -e "\n$1"; }

# =============================================================================
# Parse arguments
# =============================================================================
SHIM_ONLY=false
SKIP_CHECKS=false

print_usage() {
    cat <<EOF
Usage: $0 [OPTIONS]

Options:
  --shim-only     Install only the spinbox shim, kernel, and QEMU binaries.
                  Assumes containerd and CNI plugins are already installed.
  --skip-checks   Skip prerequisite checks in shim-only mode
  -h, --help      Show this help message

Modes:
  Full install (default):
    Installs everything: containerd, runc, nerdctl, CNI plugins,
    spinbox shim, kernel, QEMU, and systemd services.

  Shim-only install (--shim-only):
    Installs only: spinbox shim, kernel, initrd, QEMU binaries/firmware.
    Requires: containerd already installed, CNI plugins available, KVM access.
EOF
}

while [[ $# -gt 0 ]]; do
    case $1 in
        --shim-only)   SHIM_ONLY=true; shift ;;
        --skip-checks) SKIP_CHECKS=true; shift ;;
        -h|--help)     print_usage; exit 0 ;;
        *)
            echo -e "${RED}Error: Unknown option: $1${NC}"
            print_usage
            exit 1
            ;;
    esac
done

echo "================================================"
if [ "$SHIM_ONLY" = true ]; then
    echo "  Spinbox Installation Script (Shim-Only Mode)"
else
    echo "  Spinbox Installation Script"
fi
echo "================================================"
echo ""

# Check root
if [[ $EUID -ne 0 ]]; then
    echo -e "${RED}Error: This script must be run as root${NC}"
    echo "Please run: sudo $0"
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# =============================================================================
# Check helpers
# =============================================================================
check_command() {
    local cmd="$1" name="${2:-$1}"
    if command -v "$cmd" &>/dev/null; then
        log_ok "$name found: $(command -v "$cmd")"
        return 0
    fi
    log_err "$name not found in PATH"
    return 1
}

find_in_paths() {
    local -n result=$1
    shift
    for path in "$@"; do
        if [ -e "$path" ]; then
            # shellcheck disable=SC2034  # result is a nameref
            result="$path"
            return 0
        fi
    done
    return 1
}

check_package() {
    local pkg="$1"
    if dpkg -s "$pkg" &>/dev/null; then
        log_ok "$pkg"
        return 0
    fi
    # Try t64 variants (Ubuntu 24.04+ time_t transition)
    local alt=""
    if [[ "$pkg" == *t64 ]]; then
        alt="${pkg%t64}"
    else
        alt="${pkg}t64"
    fi
    if dpkg -s "$alt" &>/dev/null; then
        log_ok "$alt (provides $pkg)"
        return 0
    fi
    return 1
}

# =============================================================================
# Prerequisite checks
# =============================================================================
check_containerd_installed() {
    section "Checking containerd installation..."
    if ! check_command "containerd"; then return 1; fi
    local version
    version=$(containerd --version 2>/dev/null | head -1) || true
    [ -n "$version" ] && log_ok "Version: $version"
    return 0
}

check_containerd_running() {
    section "Checking containerd status..."
    local sock=""
    if find_in_paths sock "/run/containerd/containerd.sock" "/var/run/containerd/containerd.sock"; then
        log_ok "Socket found: $sock"
    else
        log_warn "No containerd socket found (service may not be running)"
        return 1
    fi
    if command -v systemctl &>/dev/null; then
        if systemctl is-active --quiet containerd 2>/dev/null; then
            log_ok "containerd service is active"
        else
            log_warn "containerd service is not active (may be managed differently)"
        fi
    fi
    return 0
}

check_containerd_config() {
    section "Checking containerd configuration for spinbox runtime..."
    local cfg="/etc/containerd/config.toml"
    if [ ! -f "$cfg" ]; then
        log_warn "No containerd config.toml found"
        echo "      You may need to create one at /etc/containerd/config.toml"
        return 1
    fi
    log_ok "Config found: $cfg"
    if grep -q "spinbox" "$cfg" 2>/dev/null; then
        log_ok "spinbox runtime found in config"
        return 0
    fi
    log_warn "spinbox runtime not found in containerd config"
    cat <<EOF

      Add the following to your containerd config.toml:

      ${BLUE}[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.spinbox]${NC}
      ${BLUE}  runtime_type = "io.containerd.spinbox.v1"${NC}
      ${BLUE}  snapshotter = "spin-erofs"${NC}

      Note: You also need the spin-erofs proxy plugin configured.
      See /usr/share/spinbox/config/containerd/config.toml for an example.

EOF
    return 1
}

check_kvm_access() {
    section "Checking KVM access..."
    if [ ! -e /dev/kvm ]; then
        log_err "/dev/kvm does not exist"
        echo "      KVM is required for spinbox. Ensure your system supports virtualization."
        return 1
    fi
    if [ ! -r /dev/kvm ] || [ ! -w /dev/kvm ]; then
        log_warn "/dev/kvm exists but may not be accessible"
        echo "      Ensure the user running containers has access to /dev/kvm"
        echo "      (e.g., add to 'kvm' group: usermod -aG kvm <user>)"
    else
        log_ok "/dev/kvm is accessible"
    fi
    return 0
}

check_cni_plugins() {
    section "Checking CNI plugins..."
    local cni_dir=""
    if ! find_in_paths cni_dir "/opt/cni/bin" "/usr/lib/cni" "/usr/libexec/cni"; then
        log_err "No CNI plugin directory found"
        echo "      Expected: /opt/cni/bin, /usr/lib/cni, or /usr/libexec/cni"
        return 1
    fi
    log_ok "CNI plugin directory found: $cni_dir"

    local required=(bridge host-local loopback)
    local optional=(firewall portmap)
    local missing=0

    for plugin in "${required[@]}"; do
        if [ -x "${cni_dir}/${plugin}" ]; then
            log_ok "Plugin: $plugin"
        else
            log_err "Missing plugin: $plugin"
            ((missing++))
        fi
    done
    for plugin in "${optional[@]}"; do
        if [ -x "${cni_dir}/${plugin}" ]; then
            log_ok "Plugin (optional): $plugin"
        else
            log_warn "Optional plugin not found: $plugin"
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
    section "Checking CNI network configuration..."
    local dir="/etc/cni/net.d"
    if [ -d "$dir" ]; then
        local configs
        configs=$(find "$dir" \( -name "*.conflist" -o -name "*.conf" \) 2>/dev/null | head -5)
        if [ -n "$configs" ]; then
            log_ok "CNI config directory: $dir"
            echo "$configs" | while read -r f; do
                log_ok "Config: $(basename "$f")"
            done
            return 0
        fi
    fi
    log_warn "No CNI configuration found in /etc/cni/net.d/"
    echo "      You may need to create a CNI network configuration."
    echo "      Example: /usr/share/spinbox/config/cni/net.d/10-spinbox.conflist"
    return 1
}

check_ubuntu_packages() {
    section "Checking required Ubuntu packages for QEMU..."
    if [ ! -f /etc/os-release ]; then
        log_warn "Cannot detect OS, skipping package check"
        return 0
    fi
    # shellcheck disable=SC1091
    source /etc/os-release
    if [[ "${ID:-}" != "ubuntu" && "${ID:-}" != "debian" ]]; then
        log_warn "Not Ubuntu/Debian (detected: ${ID:-unknown}), skipping package check"
        return 0
    fi
    log_ok "Detected: ${PRETTY_NAME:-$ID}"

    if ! command -v dpkg &>/dev/null; then
        log_warn "dpkg not available, skipping package check"
        return 0
    fi

    local packages=(
        libpixman-1-0 libseccomp2 zlib1g libzstd1 libaio1t64
        liburing2 libpmem1 librdmacm1 libibverbs1 libdw1 libbpf1
    )
    local missing=()
    for pkg in "${packages[@]}"; do
        if ! check_package "$pkg"; then
            missing+=("$pkg")
        fi
    done
    if [ ${#missing[@]} -gt 0 ]; then
        echo ""
        log_err "Missing required packages:"
        for pkg in "${missing[@]}"; do
            echo -e "      ${RED}•${NC} $pkg"
        done
        echo ""
        echo "      Install with:"
        echo -e "      ${BLUE}sudo apt-get install ${missing[*]}${NC}"
        return 1
    fi
    return 0
}

check_qemu_dependencies() {
    section "Checking QEMU binary dependencies..."
    local qemu_bin="${SCRIPT_DIR}/usr/share/spinbox/bin/qemu-system-x86_64"
    if [ ! -f "$qemu_bin" ]; then
        log_warn "QEMU binary not found in release package (will skip dependency check)"
        return 0
    fi
    if ! command -v ldd &>/dev/null; then
        log_warn "ldd not available, skipping dependency check"
        return 0
    fi
    local output
    output=$(ldd "$qemu_bin" 2>&1) || true
    if echo "$output" | grep -q "not found"; then
        log_err "Missing QEMU dependencies:"
        echo "$output" | grep "not found" | while read -r line; do
            echo -e "      ${RED}•${NC} $line"
        done
        return 1
    elif echo "$output" | grep -q "not a dynamic executable"; then
        log_ok "QEMU binary is statically linked (no external dependencies)"
    else
        log_ok "All QEMU library dependencies are satisfied"
    fi
    return 0
}

run_prerequisite_checks() {
    echo ""
    echo "Running prerequisite checks for shim-only installation..."
    local errors=0 warnings=0

    check_containerd_installed || ((errors++))
    check_kvm_access           || ((errors++))
    check_cni_plugins          || ((errors++))
    check_ubuntu_packages      || ((errors++))
    check_qemu_dependencies    || ((errors++))
    check_containerd_running   || ((warnings++))
    check_containerd_config    || ((warnings++))
    check_cni_config           || ((warnings++))

    echo ""
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
    return 0
}

run_basic_checks() {
    echo ""
    echo "Running prerequisite checks..."
    local errors=0
    check_kvm_access        || ((errors++))
    check_ubuntu_packages   || ((errors++))
    check_qemu_dependencies || ((errors++))
    if [ $errors -gt 0 ]; then
        echo ""
        echo "================================================"
        echo -e "${RED}Prerequisite checks failed with $errors error(s)${NC}"
        echo ""
        echo "Please resolve the errors above before installing."
        echo "Use --skip-checks to bypass these checks (not recommended)."
        echo "================================================"
        return 1
    fi
    return 0
}

# =============================================================================
# Installation helpers
# =============================================================================
install_dir() {
    local src="$1" dst="$2" desc="$3" executable="${4:-false}"
    echo "  → Installing ${desc}..."
    mkdir -p "$dst"
    cp -r "${src}"/* "$dst"
    [ "$executable" = true ] && chmod +x "$dst"/*
    log_ok "${desc} installed"
}

# =============================================================================
# Run checks
# =============================================================================
if [ "$SHIM_ONLY" = true ] && [ "$SKIP_CHECKS" = false ]; then
    run_prerequisite_checks || exit 1
fi
if [ "$SHIM_ONLY" = false ] && [ "$SKIP_CHECKS" = false ]; then
    run_basic_checks || exit 1
fi

# =============================================================================
# Install files
# =============================================================================
echo ""
echo "Installing files..."

if [ "$SHIM_ONLY" = true ]; then
    echo "  → Installing shim binaries to /usr/share/spinbox/bin..."
    mkdir -p /usr/share/spinbox/bin
    for bin in containerd-shim-spinbox-v1 qemu-system-x86_64; do
        src="${SCRIPT_DIR}/usr/share/spinbox/bin/${bin}"
        [ -f "$src" ] && cp "$src" /usr/share/spinbox/bin/ && chmod +x "/usr/share/spinbox/bin/${bin}"
    done
    log_ok "Shim binaries installed"
else
    install_dir "${SCRIPT_DIR}/usr/share/spinbox/bin" "/usr/share/spinbox/bin" "binaries" true
fi

install_dir "${SCRIPT_DIR}/usr/share/spinbox/kernel" "/usr/share/spinbox/kernel" "kernel and initrd"

if [ "$SHIM_ONLY" = false ]; then
    install_dir "${SCRIPT_DIR}/usr/share/spinbox/config" "/usr/share/spinbox/config" "configuration files"
else
    echo "  → Installing spinbox config reference..."
    mkdir -p /usr/share/spinbox/config/spinbox
    [ -f "${SCRIPT_DIR}/usr/share/spinbox/config/spinbox/config.json" ] && \
        cp "${SCRIPT_DIR}/usr/share/spinbox/config/spinbox/config.json" /usr/share/spinbox/config/spinbox/
    [ -d "${SCRIPT_DIR}/usr/share/spinbox/config/cni" ] && \
        mkdir -p /usr/share/spinbox/config/cni && \
        cp -r "${SCRIPT_DIR}/usr/share/spinbox/config/cni/"* /usr/share/spinbox/config/cni/
    log_ok "Reference configuration files installed"
fi

if [ -d "${SCRIPT_DIR}/usr/share/spinbox/qemu" ]; then
    install_dir "${SCRIPT_DIR}/usr/share/spinbox/qemu" "/usr/share/spinbox/qemu" "QEMU firmware"
fi

if [ "$SHIM_ONLY" = false ]; then
    if [ -d "${SCRIPT_DIR}/usr/share/spinbox/libexec/cni" ]; then
        install_dir "${SCRIPT_DIR}/usr/share/spinbox/libexec/cni" "/usr/share/spinbox/libexec/cni" "CNI plugins" true
    fi
else
    log_skip "Skipping CNI plugins (shim-only mode assumes system CNI plugins)"
fi

echo "  → Installing scripts..."
cp "${SCRIPT_DIR}/install.sh" /usr/share/spinbox/
cp "${SCRIPT_DIR}/uninstall.sh" /usr/share/spinbox/
chmod +x /usr/share/spinbox/install.sh /usr/share/spinbox/uninstall.sh
log_ok "Scripts installed"

echo "  → Creating state directories..."
mkdir -p /var/lib/spinbox /var/run/spinbox /var/log/spinbox /run/spinbox/vm
if [ "$SHIM_ONLY" = false ]; then
    mkdir -p /var/lib/spinbox/containerd /run/spinbox/containerd /run/spinbox/containerd/fifo
    mkdir -p /var/lib/spin-stack/erofs-snapshotter /run/spin-stack/erofs-snapshotter
fi
log_ok "State directories created"

echo "  → Installing spinbox configuration..."
mkdir -p /etc/spinbox
if [ ! -f /etc/spinbox/config.json ]; then
    cp "${SCRIPT_DIR}/usr/share/spinbox/config/spinbox/config.json" /etc/spinbox/config.json
    log_ok "Configuration file created at /etc/spinbox/config.json"
else
    log_warn "/etc/spinbox/config.json already exists, skipping"
    echo "      Example: /usr/share/spinbox/config/spinbox/config.json"
fi

if [ "$SHIM_ONLY" = false ]; then
    echo "  → Installing systemd services..."
    mkdir -p /usr/share/spinbox/systemd
    cp "${SCRIPT_DIR}/usr/share/spinbox/systemd/"*.service /usr/share/spinbox/systemd/
    ln -sf /usr/share/spinbox/systemd/spinbox-spin-erofs-snapshotter.service /etc/systemd/system/
    ln -sf /usr/share/spinbox/systemd/spinbox-containerd.service /etc/systemd/system/
    systemctl daemon-reload
    log_ok "Systemd services installed"
else
    log_skip "Skipping systemd services (shim-only mode uses existing containerd)"
fi

# =============================================================================
# Verification
# =============================================================================
echo ""
echo "Verifying installation..."

ERRORS=0
check_file() {
    if [ -f "$1" ]; then
        log_ok "Found: $1"
    else
        log_err "Missing: $1"
        ((ERRORS++))
    fi
}

# Core files (both modes)
CORE_FILES=(
    "/usr/share/spinbox/bin/containerd-shim-spinbox-v1"
    "/usr/share/spinbox/bin/qemu-system-x86_64"
    "/usr/share/spinbox/qemu/bios-256k.bin"
    "/usr/share/spinbox/qemu/kvmvapic.bin"
    "/usr/share/spinbox/qemu/vgabios-stdvga.bin"
    "/usr/share/spinbox/kernel/spinbox-kernel-x86_64"
    "/usr/share/spinbox/kernel/spinbox-initrd"
    "/usr/share/spinbox/config/spinbox/config.json"
    "/etc/spinbox/config.json"
)

# Full install additional files
FULL_FILES=(
    "/usr/share/spinbox/bin/containerd"
    "/usr/share/spinbox/bin/containerd-shim-runc-v2"
    "/usr/share/spinbox/bin/spin-erofs-snapshotter"
    "/usr/share/spinbox/bin/ctr"
    "/usr/share/spinbox/bin/runc"
    "/usr/share/spinbox/bin/nerdctl"
    "/usr/share/spinbox/config/containerd/config.toml"
    "/usr/share/spinbox/config/cni/net.d/10-spinbox.conflist"
    "/usr/share/spinbox/systemd/spinbox-spin-erofs-snapshotter.service"
    "/usr/share/spinbox/systemd/spinbox-containerd.service"
    "/etc/systemd/system/spinbox-spin-erofs-snapshotter.service"
    "/etc/systemd/system/spinbox-containerd.service"
)

CNI_PLUGINS=(bridge host-local loopback)

if [ "$SHIM_ONLY" = true ]; then
    echo "Verifying shim-only installation..."
    for f in "${CORE_FILES[@]}"; do check_file "$f"; done
else
    echo "Verifying full installation..."
    for f in "${CORE_FILES[@]}" "${FULL_FILES[@]}"; do check_file "$f"; done
    for plugin in "${CNI_PLUGINS[@]}"; do
        check_file "/usr/share/spinbox/libexec/cni/${plugin}"
    done
fi

# =============================================================================
# Summary
# =============================================================================
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
        cat <<EOF
Next steps:
  1. Review and customize spinbox configuration (if needed):
     vi /etc/spinbox/config.json

  2. Configure the spinbox runtime in your containerd config:
     Add to /etc/containerd/config.toml:

     ${BLUE}[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.spinbox]${NC}
     ${BLUE}  runtime_type = "io.containerd.spinbox.v1"${NC}
     ${BLUE}  snapshotter = "spin-erofs"${NC}

     Note: You also need to configure the spin-erofs proxy plugin.
     See /usr/share/spinbox/config/containerd/config.toml for an example.

  3. Ensure the shim is in containerd's PATH or use absolute path:
     Shim location: /usr/share/spinbox/bin/containerd-shim-spinbox-v1

  4. Restart containerd to pick up the new runtime:
     systemctl restart containerd

  5. Test the runtime:
     ctr run --rm --runtime io.containerd.spinbox.v1 docker.io/library/alpine:latest test echo hello

EOF
    else
        cat <<EOF
Next steps:
  1. Review and customize configuration (if needed):
     vi /etc/spinbox/config.json
     (See /usr/share/spinbox/config/spinbox/config.json for defaults)

  2. Enable and start services (snapshotter starts automatically with containerd):
     systemctl enable spinbox-containerd
     systemctl start spinbox-containerd

  3. Check service status:
     systemctl status spinbox-spin-erofs-snapshotter
     systemctl status spinbox-containerd

  4. Add /usr/share/spinbox/bin to PATH:
     export PATH=/usr/share/spinbox/bin:\$PATH

EOF
    fi
else
    echo -e "${RED}✗ Installation verification failed with $ERRORS error(s)${NC}"
    echo "Please check the errors above and ensure all required files are present."
    exit 1
fi
