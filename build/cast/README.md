# QemuBox Demo Recording

Automated asciinema recordings using expect scripts.

## Quick Start

```bash
# Install dependencies
sudo apt-get install asciinema expect

# Record basic demo
./record.sh

# Record snapshot demo
./record-snapshot.sh

# Play back
asciinema play qemubox-demo.cast
```

## Files

| Script | Description | Output |
|--------|-------------|--------|
| `record.sh` | Basic demo (boot, Docker) | `qemubox-demo.cast` |
| `record-snapshot.sh` | Snapshot demo (persist state) | `qemubox-snapshot-demo.cast` |
| `qemubox.exp` | Expect script for basic demo | |
| `snapshot.exp` | Expect script for snapshot demo | |

## What Gets Recorded

**Basic Demo:**
- Pull qemubox sandbox image
- Boot VM with qemubox runtime
- Show systemd boot analysis
- Run Docker inside VM

**Snapshot Demo:**
- Run VM and make changes (files, packages)
- Commit running VM to new image with `nerdctl commit`
- Run new VM from committed image
- Verify changes persisted

## Customization

Adjust timing in `.exp` files:
```expect
set TYPING_DELAY 0.04   # Character delay (lower = faster)
set CMD_DELAY 1         # Pause after commands
set LONG_DELAY 2        # Pause for long operations
```

## Upload

```bash
asciinema upload qemubox-demo.cast
```

## Troubleshooting

**Login credentials:** `root` / `qemubox`

**Test without recording:**
```bash
expect qemubox.exp
```
