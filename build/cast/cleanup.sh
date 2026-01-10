#!/bin/bash
# Cleanup containers for spinbox demos
# Usage: ./cleanup.sh [container-names...]

set -euo pipefail

CTR="ctr --address /var/run/spinbox/containerd.sock"

cleanup_container() {
    local name="$1"
    $CTR task kill "$name" 2>/dev/null || true
    $CTR task delete "$name" 2>/dev/null || true
    $CTR container rm "$name" 2>/dev/null || true
    $CTR snapshots --snapshotter spin-erofs rm "${name}-snapshot" 2>/dev/null || true
}

if [ $# -eq 0 ]; then
    echo "Usage: $0 <container-name> [container-name...]"
    echo "For full CNI cleanup, use: deploy/cleanup-cni.sh"
    exit 1
fi

for name in "$@"; do
    echo "Cleaning up: $name"
    cleanup_container "$name"
done

echo "Done."
