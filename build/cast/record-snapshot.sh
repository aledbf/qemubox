#!/bin/bash
# Asciinema recorder for qemubox snapshot demo
# Usage: ./record-snapshot.sh [output-name]

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUTPUT="${1:-qemubox-snapshot-demo}"
NERDCTL="nerdctl --address /var/run/qemubox/containerd.sock"

# Ensure cleanup on exit/interrupt
cleanup() {
    "$SCRIPT_DIR/cleanup.sh" snapshot-demo snapshot-new
    $NERDCTL rmi docker.io/aledbf/sandbox:with-changes 2>/dev/null || true
}
trap cleanup EXIT

# Check dependencies
for cmd in asciinema expect; do
    if ! command -v $cmd &> /dev/null; then
        echo "Error: $cmd is not installed"
        exit 1
    fi
done

# Check expect script exists
[ -f "$SCRIPT_DIR/snapshot.exp" ] || { echo "Error: snapshot.exp not found"; exit 1; }

echo "QemuBox Snapshot Demo - Recording to ${OUTPUT}.cast"

# Pre-cleanup to avoid conflicts from previous runs
"$SCRIPT_DIR/cleanup.sh" snapshot-demo snapshot-new
$NERDCTL rmi docker.io/aledbf/sandbox:with-changes 2>/dev/null || true

echo "Starting in 3 seconds..."
sleep 3

# Terminal size
COLS=120
ROWS=40
export COLUMNS=$COLS LINES=$ROWS
stty cols $COLS rows $ROWS 2>/dev/null || true

# Record
echo "Recording..."
asciinema rec "${OUTPUT}.cast" -c "expect $SCRIPT_DIR/snapshot.exp" \
    --cols $COLS --rows $ROWS --overwrite || {
    echo "Recording failed"
    exit 1
}

# Success
echo "Recording saved to: ${OUTPUT}.cast"
echo ""
echo "Play:   asciinema play ${OUTPUT}.cast"
echo "Upload: asciinema upload ${OUTPUT}.cast"

# Cleanup handled by trap
