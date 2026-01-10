#!/bin/bash
# Unified asciinema recorder for spinbox demos
# Usage: ./record.sh [demo|snapshot] [output-name]

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODE="${1:-demo}"
CTR="ctr --address /var/run/spinbox/containerd.sock"
NERDCTL="nerdctl --address /var/run/spinbox/containerd.sock"

# Validate mode and set defaults
case "$MODE" in
    demo)
        OUTPUT="${2:-spinbox-demo}"
        EXPECT_SCRIPT="$SCRIPT_DIR/spinbox.exp"
        CONTAINERS="demo-vm"
        ;;
    snapshot)
        OUTPUT="${2:-spinbox-snapshot-demo}"
        EXPECT_SCRIPT="$SCRIPT_DIR/snapshot.exp"
        CONTAINERS="snapshot-demo snapshot-new"
        ;;
    *)
        echo "Usage: $0 [demo|snapshot] [output-name]"
        echo "  demo     - Basic demo (boot, Docker)"
        echo "  snapshot - Snapshot demo (persist state)"
        exit 1
        ;;
esac

# Cleanup function
cleanup() {
    for name in $CONTAINERS; do
        $CTR task kill "$name" 2>/dev/null || true
        $CTR task delete "$name" 2>/dev/null || true
        $CTR container rm "$name" 2>/dev/null || true
        $CTR snapshots --snapshotter spin-erofs rm "${name}-snapshot" 2>/dev/null || true
    done
    if [ "$MODE" = "snapshot" ]; then
        $NERDCTL rmi docker.io/spin-stack/sandbox:with-changes 2>/dev/null || true
    fi
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
[ -f "$EXPECT_SCRIPT" ] || { echo "Error: $(basename "$EXPECT_SCRIPT") not found"; exit 1; }

echo "SpinBox ${MODE^} Demo - Recording to ${OUTPUT}.cast"

# Pre-cleanup
cleanup

echo "Starting in 3 seconds..."
sleep 3

# Terminal size
COLS=240
ROWS=40
export COLUMNS=$COLS LINES=$ROWS
stty cols $COLS rows $ROWS 2>/dev/null || true

# Record
echo "Recording..."
asciinema rec "${OUTPUT}.cast" -c "expect $EXPECT_SCRIPT" \
    --cols $COLS --rows $ROWS --overwrite || {
    echo "Recording failed"
    exit 1
}

echo ""
echo "Recording saved to: ${OUTPUT}.cast"
echo "Play:   asciinema play ${OUTPUT}.cast"
echo "Upload: asciinema upload ${OUTPUT}.cast"
