#!/bin/bash
# Asciinema recorder for qemubox snapshot demo
# Usage: ./record-snapshot.sh [output-name]

set -e

OUTPUT="${1:-qemubox-snapshot-demo}"

# Check dependencies
for cmd in asciinema expect; do
    if ! command -v $cmd &> /dev/null; then
        echo "Error: $cmd is not installed"
        exit 1
    fi
done

# Check expect script exists
[ -f "snapshot.exp" ] || { echo "Error: snapshot.exp not found"; exit 1; }

echo "QemuBox Snapshot Demo - Recording to ${OUTPUT}.cast"

# Pre-cleanup to avoid conflicts from previous runs
CTR="ctr --address /var/run/qemubox/containerd.sock"
NERDCTL="nerdctl --address /var/run/qemubox/containerd.sock"
$CTR task kill snapshot-demo 2>/dev/null || true
$CTR task kill snapshot-new 2>/dev/null || true
$CTR task delete snapshot-demo 2>/dev/null || true
$CTR task delete snapshot-new 2>/dev/null || true
$CTR container rm snapshot-demo 2>/dev/null || true
$CTR container rm snapshot-new 2>/dev/null || true
$CTR snapshots --snapshotter erofs delete snapshot-demo 2>/dev/null || true
$CTR snapshots --snapshotter erofs delete snapshot-new 2>/dev/null || true
$NERDCTL rmi docker.io/aledbf/sandbox:with-changes 2>/dev/null || true

# Clean up orphaned CNI allocations
CNI_NET_DIR="/var/lib/cni/networks/qemubox-net"
if [ -d "$CNI_NET_DIR" ]; then
    grep -l "snapshot-demo\|snapshot-new" "$CNI_NET_DIR"/* 2>/dev/null | xargs -r sudo rm -f
fi

echo "Starting in 3 seconds..."
sleep 3

# Terminal size
COLS=120
ROWS=40
export COLUMNS=$COLS LINES=$ROWS
stty cols $COLS rows $ROWS 2>/dev/null || true

# Record
echo "Recording..."
asciinema rec "${OUTPUT}.cast" -c "expect snapshot.exp" \
    --cols $COLS --rows $ROWS --overwrite || {
    echo "Recording failed"
    exit 1
}

# Success
echo "Recording saved to: ${OUTPUT}.cast"
echo ""
echo "Play:   asciinema play ${OUTPUT}.cast"
echo "Upload: asciinema upload ${OUTPUT}.cast"

# Cleanup
$CTR task kill snapshot-demo 2>/dev/null || true
$CTR task kill snapshot-new 2>/dev/null || true
$CTR task delete snapshot-demo 2>/dev/null || true
$CTR task delete snapshot-new 2>/dev/null || true
$CTR container rm snapshot-demo 2>/dev/null || true
$CTR container rm snapshot-new 2>/dev/null || true
$CTR snapshots --snapshotter erofs delete snapshot-demo 2>/dev/null || true
$CTR snapshots --snapshotter erofs delete snapshot-new 2>/dev/null || true
$NERDCTL rmi docker.io/aledbf/sandbox:with-changes 2>/dev/null || true

# Clean up CNI allocations
CNI_NET_DIR="/var/lib/cni/networks/qemubox-net"
if [ -d "$CNI_NET_DIR" ]; then
    grep -l "snapshot-demo\|snapshot-new" "$CNI_NET_DIR"/* 2>/dev/null | xargs -r sudo rm -f
fi
