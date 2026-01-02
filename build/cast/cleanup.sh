#!/bin/bash
# Shared cleanup for qemubox demo scripts
# Usage: ./cleanup.sh [container-names...]
# If no container names provided, cleans up all orphaned CNI allocations

CTR="ctr --address /var/run/qemubox/containerd.sock"
NERDCTL="nerdctl --address /var/run/qemubox/containerd.sock"
CNI_NET_DIR="/var/lib/cni/networks/qemubox-net"

# Cleanup specific containers if provided
cleanup_container() {
    local name="$1"
    $CTR task kill "$name" 2>/dev/null || true
    $CTR task delete "$name" 2>/dev/null || true
    $CTR container rm "$name" 2>/dev/null || true
    $CTR snapshots --snapshotter erofs delete "$name" 2>/dev/null || true
}

# Clean orphaned CNI allocations (IPs allocated to non-running containers)
cleanup_orphaned_cni() {
    [ -d "$CNI_NET_DIR" ] || return 0

    # Get list of running container IDs
    local running
    running=$($CTR task ls -q 2>/dev/null | tr '\n' '|' | sed 's/|$//')

    # If nothing running, clean everything
    if [ -z "$running" ]; then
        sudo rm -f "$CNI_NET_DIR"/* 2>/dev/null || true
        return 0
    fi

    # Otherwise, remove allocations for non-running containers
    for ip_file in "$CNI_NET_DIR"/*; do
        [ -f "$ip_file" ] || continue
        local container_id
        container_id=$(cat "$ip_file" 2>/dev/null)
        if ! echo "$container_id" | grep -qE "^($running)$"; then
            sudo rm -f "$ip_file" 2>/dev/null || true
        fi
    done
}

# Main
if [ $# -gt 0 ]; then
    # Cleanup specific containers
    for name in "$@"; do
        cleanup_container "$name"
    done
fi

# Always clean orphaned CNI allocations
cleanup_orphaned_cni
