# Udev Loop Device Optimization

## Problem

When qemubox boots VMs, the containerd snapshotter (nexuserofs) creates multiple loop devices to mount EROFS filesystem layers. Each container image layer becomes a separate loop device (e.g., 5 layers = 5 loop devices).

This triggers a CPU spike because udev workers attempt SCSI ID detection on each loop device, spawning multiple processes per device.

### Symptoms

```
Jan 05 00:40:05 mini-pc kernel: loop0: detected capacity change from 0 to 48
Jan 05 00:40:05 mini-pc kernel: erofs (device loop0): mounted with root inode @ nid 36.
Jan 05 00:40:05 mini-pc 55-scsi-sg3_id.rules[1866808]: WARNING: SCSI device loop2 has no device ID...
Jan 05 00:40:05 mini-pc 55-scsi-sg3_id.rules[1866806]: WARNING: SCSI device loop0 has no device ID...
Jan 05 00:40:05 mini-pc 55-scsi-sg3_id.rules[1866807]: WARNING: SCSI device loop1 has no device ID...
```

The warnings repeat multiple times per device as udev retries the detection.

## Root Cause

The `55-scsi-sg3_id.rules` udev rule (from the `sg3-utils` package) runs SCSI ID detection on block devices to populate device identification attributes. This is useful for real SCSI/SAS/SATA devices but pointless for loop devices which:

1. Don't have SCSI device IDs
2. Are virtual devices backed by files
3. Are created/destroyed frequently during container operations

The detection process:
1. Loop device creation triggers udev `add` event
2. `55-scsi-sg3_id.rules` matches the block device
3. Multiple `sg_inq` processes spawn to query SCSI attributes
4. All queries fail (loop devices aren't SCSI)
5. CPU cycles wasted, warnings logged

## Solution

Create a udev rule that runs before `55-scsi-sg3_id.rules` (lower number = higher priority) to mark loop devices as non-SCSI, preventing the expensive detection.

### Installation

```bash
sudo tee /etc/udev/rules.d/50-qemubox-skip-loop-scsi.rules << 'EOF'
# Skip SCSI ID detection for loop devices - they don't have SCSI IDs
# Prevent CPU spikes from udev workers processing containerd loop devices
SUBSYSTEM=="block", KERNEL=="loop*", ENV{ID_SCSI}="0", ENV{ID_SCSI_INQUIRY}="0"
EOF

sudo udevadm control --reload-rules
```

### How It Works

The rule sets two environment variables for all loop devices:

| Variable | Purpose |
|----------|---------|
| `ID_SCSI=0` | Marks the device as non-SCSI |
| `ID_SCSI_INQUIRY=0` | Disables SCSI inquiry commands |

When `55-scsi-sg3_id.rules` runs, it checks these variables and skips devices already marked as non-SCSI, avoiding the expensive `sg_inq` process spawning.

### Rule Syntax Explanation

```
SUBSYSTEM=="block", KERNEL=="loop*", ENV{ID_SCSI}="0", ENV{ID_SCSI_INQUIRY}="0"
         ^                ^                  ^                    ^
         |                |                  |                    |
    Match block      Match loop         Set ID_SCSI          Set ID_SCSI_INQUIRY
    subsystem        devices            to "0"               to "0"
```

- `==` is a match operator (condition)
- `=` is an assignment operator (action)

### Verification

After applying the rule, verify it's loaded:

```bash
# Check rule is loaded
udevadm info --query=property --name=/dev/loop0 | grep ID_SCSI

# Monitor udev events during container start
udevadm monitor --property --subsystem-match=block
```

The warnings from `55-scsi-sg3_id.rules` should no longer appear when loop devices are created.

## Alternative Approaches Considered

### GOTO Label (Not Portable)

```
KERNEL=="loop*", GOTO="sg3_utils_id_end"
```

**Issue**: The label name varies between distributions. Ubuntu uses different labels than Fedora/RHEL.

### OPTIONS+="last_rule"

```
KERNEL=="loop*", OPTIONS+="last_rule"
```

**Issue**: Stops ALL further rule processing for loop devices, which may break other legitimate rules.

### Setting DEVTYPE

```
KERNEL=="loop*", ENV{DEVTYPE}="disk"
```

**Issue**: `DEVTYPE` is a read-only attribute and cannot be set.

## Performance Impact

Without the fix:
- 5-10 `sg_inq` processes per loop device
- Multiple retry attempts
- Noticeable CPU spike during container startup
- Log spam with warnings

With the fix:
- Zero `sg_inq` processes for loop devices
- No CPU overhead from SCSI detection
- Clean logs

## Affected Systems

This optimization is relevant for systems running:

- qemubox with nexuserofs snapshotter
- Any containerd setup using EROFS layers via loop devices
- High-frequency container creation workloads
- Systems with `sg3-utils` package installed

## References

- [udev(7) man page](https://man7.org/linux/man-pages/man7/udev.7.html)
- [sg3_utils documentation](https://sg.danny.cz/sg/sg3_utils.html)
- [Linux loop device documentation](https://man7.org/linux/man-pages/man4/loop.4.html)
