#!/bin/bash
# Shared cleanup for spinbox demo scripts
# Usage: ./cleanup.sh [container-names...]
# If no container names provided, cleans up all orphaned CNI allocations and firewall rules

set -euo pipefail

CTR="ctr --address /var/run/spinbox/containerd.sock"
NERDCTL="nerdctl --address /var/run/spinbox/containerd.sock"
CNI_NET_DIR="/var/lib/cni/networks/spinbox-net"
CNI_SUBNET="10.88."

# Cleanup specific containers if provided
cleanup_container() {
    local name="$1"
    $CTR task kill "$name" 2>/dev/null || true
    $CTR task delete "$name" 2>/dev/null || true
    $CTR container rm "$name" 2>/dev/null || true
    $CTR snapshots --snapshotter spin-erofs delete "$name" 2>/dev/null || true
}

# Get list of IPs allocated to running containers
get_active_ips() {
    local running
    running=$($CTR task ls -q 2>/dev/null | tr '\n' '|' | sed 's/|$//')

    if [ -z "$running" ] || [ ! -d "$CNI_NET_DIR" ]; then
        return
    fi

    for ip_file in "$CNI_NET_DIR"/*; do
        [ -f "$ip_file" ] || continue
        local container_id ip
        container_id=$(cat "$ip_file" 2>/dev/null) || continue
        ip=$(basename "$ip_file")
        if echo "$container_id" | grep -qE "^($running)$"; then
            echo "$ip"
        fi
    done
}

# Clean orphaned CNI IPAM allocations (IPs allocated to non-running containers)
cleanup_orphaned_ipam() {
    [ -d "$CNI_NET_DIR" ] || return 0

    local running
    running=$($CTR task ls -q 2>/dev/null | tr '\n' '|' | sed 's/|$//')

    # If nothing running, clean everything
    if [ -z "$running" ]; then
        echo "No running containers, cleaning all IPAM allocations..."
        sudo rm -f "$CNI_NET_DIR"/* 2>/dev/null || true
        return 0
    fi

    # Otherwise, remove allocations for non-running containers
    local cleaned=0
    for ip_file in "$CNI_NET_DIR"/*; do
        [ -f "$ip_file" ] || continue
        local container_id
        container_id=$(cat "$ip_file" 2>/dev/null)
        if ! echo "$container_id" | grep -qE "^($running)$"; then
            echo "Removing orphaned IPAM allocation: $(basename "$ip_file")"
            sudo rm -f "$ip_file" 2>/dev/null || true
            ((cleaned++)) || true
        fi
    done

    if [ "$cleaned" -gt 0 ]; then
        echo "Cleaned $cleaned orphaned IPAM allocations"
    fi
}

# Clean orphaned CNI firewall rules (iptables/nftables)
cleanup_orphaned_firewall() {
    # Check if iptables is available
    if ! command -v iptables &>/dev/null; then
        echo "Warning: iptables not found, skipping firewall cleanup"
        return 0
    fi

    # Get active IPs (IPs belonging to running containers)
    local active_ips
    active_ips=$(get_active_ips | tr '\n' '|' | sed 's/|$//')

    echo "Cleaning orphaned CNI firewall rules..."

    # Clean NAT POSTROUTING rules for orphaned IPs
    cleanup_nat_rules "$active_ips"

    # Clean filter FORWARD rules for orphaned IPs
    cleanup_filter_rules "$active_ips"

    # Clean orphaned CNI chains (chains with no references)
    cleanup_orphaned_chains
}

# Clean orphaned NAT POSTROUTING rules
cleanup_nat_rules() {
    local active_ips="$1"
    local cleaned=0

    # List all CNI rules in POSTROUTING
    # Format: -A POSTROUTING -s 10.88.x.x/32 -j CNI-xxxx
    while IFS= read -r line; do
        # Extract IP from the rule
        local ip
        ip=$(echo "$line" | grep -oE "${CNI_SUBNET}[0-9]+\.[0-9]+" | head -1) || continue
        [ -n "$ip" ] || continue

        # Check if IP is active
        if [ -n "$active_ips" ] && echo "$ip" | grep -qE "^($active_ips)$"; then
            continue
        fi

        # Delete the rule - need to get the full rule spec
        local rule_spec
        rule_spec=$(echo "$line" | sed 's/^-A POSTROUTING //')
        if [ -n "$rule_spec" ]; then
            sudo iptables -t nat -D POSTROUTING $rule_spec 2>/dev/null && ((cleaned++)) || true
        fi
    done < <(sudo iptables -t nat -S POSTROUTING 2>/dev/null | grep -E "CNI-" | grep -E "${CNI_SUBNET}")

    if [ "$cleaned" -gt 0 ]; then
        echo "Cleaned $cleaned orphaned NAT POSTROUTING rules"
    fi
}

# Clean orphaned filter CNI-FORWARD rules
cleanup_filter_rules() {
    local active_ips="$1"
    local cleaned=0

    # Check if CNI-FORWARD chain exists
    if ! sudo iptables -S CNI-FORWARD &>/dev/null; then
        return 0
    fi

    # List all rules in CNI-FORWARD
    while IFS= read -r line; do
        # Extract IP from the rule
        local ip
        ip=$(echo "$line" | grep -oE "${CNI_SUBNET}[0-9]+\.[0-9]+" | head -1) || continue
        [ -n "$ip" ] || continue

        # Check if IP is active
        if [ -n "$active_ips" ] && echo "$ip" | grep -qE "^($active_ips)$"; then
            continue
        fi

        # Delete the rule
        local rule_spec
        rule_spec=$(echo "$line" | sed 's/^-A CNI-FORWARD //')
        if [ -n "$rule_spec" ]; then
            sudo iptables -D CNI-FORWARD $rule_spec 2>/dev/null && ((cleaned++)) || true
        fi
    done < <(sudo iptables -S CNI-FORWARD 2>/dev/null | grep -E "${CNI_SUBNET}")

    if [ "$cleaned" -gt 0 ]; then
        echo "Cleaned $cleaned orphaned CNI-FORWARD rules"
    fi
}

# Clean orphaned CNI-xxxx chains (chains that are empty or unreferenced)
cleanup_orphaned_chains() {
    local cleaned=0

    # Get all CNI-xxxx chains in nat table (excluding CNI-FORWARD, CNI-ADMIN)
    while IFS= read -r chain; do
        # Skip if chain is still referenced in POSTROUTING
        if sudo iptables -t nat -S POSTROUTING 2>/dev/null | grep -q "\-j ${chain}"; then
            continue
        fi

        # Flush and delete the chain
        sudo iptables -t nat -F "$chain" 2>/dev/null || true
        if sudo iptables -t nat -X "$chain" 2>/dev/null; then
            ((cleaned++)) || true
        fi
    done < <(sudo iptables -t nat -S 2>/dev/null | grep -oE "^-N CNI-[a-f0-9]+" | sed 's/-N //')

    if [ "$cleaned" -gt 0 ]; then
        echo "Cleaned $cleaned orphaned CNI NAT chains"
    fi
}

# Show current state
show_status() {
    echo "=== CNI Cleanup Status ==="

    # Running containers
    echo -e "\nRunning containers:"
    $CTR task ls 2>/dev/null || echo "  (none or containerd not running)"

    # IPAM allocations
    echo -e "\nIPAM allocations in $CNI_NET_DIR:"
    if [ -d "$CNI_NET_DIR" ]; then
        ls -la "$CNI_NET_DIR" 2>/dev/null | tail -n +2 || echo "  (empty)"
    else
        echo "  (directory does not exist)"
    fi

    # CNI firewall rules count
    if command -v iptables &>/dev/null; then
        local nat_rules filter_rules chains
        nat_rules=$(sudo iptables -t nat -S POSTROUTING 2>/dev/null | grep -cE "CNI-.*${CNI_SUBNET}" || echo 0)
        filter_rules=$(sudo iptables -S CNI-FORWARD 2>/dev/null | grep -cE "${CNI_SUBNET}" || echo 0)
        chains=$(sudo iptables -t nat -S 2>/dev/null | grep -cE "^-N CNI-[a-f0-9]+" || echo 0)
        echo -e "\nFirewall rules:"
        echo "  NAT POSTROUTING CNI rules: $nat_rules"
        echo "  Filter CNI-FORWARD rules: $filter_rules"
        echo "  CNI NAT chains: $chains"
    fi

    echo ""
}

# Main
main() {
    # Handle --status flag
    if [ "${1:-}" = "--status" ]; then
        show_status
        exit 0
    fi

    # Cleanup specific containers if provided
    if [ $# -gt 0 ]; then
        for name in "$@"; do
            echo "Cleaning up container: $name"
            cleanup_container "$name"
        done
    fi

    # Clean orphaned CNI IPAM allocations
    cleanup_orphaned_ipam

    # Clean orphaned CNI firewall rules
    cleanup_orphaned_firewall

    echo "Cleanup complete."
}

main "$@"
