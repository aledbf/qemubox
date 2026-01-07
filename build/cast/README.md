# QemuBox Demo Recording

Automated asciinema recordings using expect scripts.

## Quick Start

```bash
# Install dependencies
sudo apt-get install asciinema expect

# Record basic demo
./record.sh demo

# Record snapshot demo
./record.sh snapshot

# Play back
asciinema play qemubox-demo.cast
```

## Usage

```bash
./record.sh [demo|snapshot] [output-name]
```

| Mode | Description | Default Output |
|------|-------------|----------------|
| `demo` | Basic demo (boot, Docker) | `qemubox-demo.cast` |
| `snapshot` | Snapshot demo (persist state) | `qemubox-snapshot-demo.cast` |

## What Gets Recorded

**Basic Demo (`demo`):**
- Pull qemubox sandbox image
- Boot VM with qemubox runtime
- Show systemd boot analysis
- Run Docker inside VM

**Snapshot Demo (`snapshot`):**
- Run VM and make changes (files, packages)
- Commit running VM to new image
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

**Manual cleanup:**
```bash
./cleanup.sh demo-vm
./cleanup.sh snapshot-demo snapshot-new
```
