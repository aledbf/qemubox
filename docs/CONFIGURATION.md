# spinbox Configuration Reference

This document provides comprehensive documentation for spinbox's centralized configuration system.

## Overview

spinbox uses a single JSON configuration file to manage all runtime settings. This file must be present for the system to start.

**Default location**: `/etc/spinbox/config.json`

**Override with environment variable**: `SPINBOX_CONFIG=/path/to/config.json`

## Configuration File Structure

```json
{
  "paths": { ... },
  "runtime": { ... },
  "timeouts": { ... },
  "cpu_hotplug": { ... },
  "memory_hotplug": { ... }
}
```

## Paths Configuration

Controls filesystem paths for spinbox components.

```json
{
  "paths": {
    "share_dir": "/usr/share/spinbox",
    "state_dir": "/var/lib/spinbox",
    "log_dir": "/var/log/spinbox",
    "qemu_path": "",
    "qemu_share_path": ""
  }
}
```

### `paths.share_dir`
- **Type**: string
- **Default**: `/usr/share/spinbox`
- **Required**: Yes
- **Description**: Directory containing spinbox binaries, kernel, and initrd
- **Validation**: Must exist and contain:
  - `kernel/spinbox-kernel-x86_64` (VM kernel)
  - `kernel/spinbox-initrd` (initial ramdisk)

### `paths.state_dir`
- **Type**: string
- **Default**: `/var/lib/spinbox`
- **Required**: Yes
- **Description**: Directory for runtime state files
- **Validation**: Must be writable (created automatically if missing)
- **Contains**: CNI network metadata database (`cni-config.db`)

### `paths.log_dir`
- **Type**: string
- **Default**: `/var/log/spinbox`
- **Required**: Yes
- **Description**: Directory for VM logs
- **Validation**: Must be writable (created automatically if missing)
- **Contains**: VM logs (`vm-<container-id>.log`)

### `paths.qemu_path`
- **Type**: string
- **Default**: `""` (auto-discovered)
- **Required**: No
- **Description**: Explicit path to `qemu-system-x86_64` binary
- **Auto-discovery order** (if empty):
  1. `{share_dir}/bin/qemu-system-x86_64`
  2. `/usr/bin/qemu-system-x86_64`
  3. `/usr/local/bin/qemu-system-x86_64`
- **Validation**: If specified, must be executable

### `paths.qemu_share_path`
- **Type**: string
- **Default**: `""` (auto-discovered)
- **Required**: No
- **Description**: Path to QEMU's share directory containing BIOS/firmware files
- **Auto-discovery order** (if empty):
  1. `{share_dir}/qemu`
  2. `/usr/share/qemu`
  3. `/usr/local/share/qemu`
- **Validation**: If specified, must exist

## Runtime Configuration

Controls spinbox runtime behavior.

```json
{
  "runtime": {
    "vmm": "qemu"
  }
}
```

### `runtime.vmm`
- **Type**: string
- **Default**: `"qemu"`
- **Required**: Yes
- **Allowed values**: `"qemu"` (only supported VMM backend)
- **Description**: Virtual Machine Monitor backend to use
- **Validation**: Must be exactly `"qemu"`

**Note**: Log level is controlled by containerd's configuration, not the shim. Configure logging in containerd's config file (`/etc/containerd/config.toml`).

## Timeouts Configuration

Controls timeout durations for various lifecycle operations.

```json
{
  "timeouts": {
    "vm_start": "10s",
    "device_detection": "5s",
    "shutdown_grace": "2s",
    "event_reconnect": "2s",
    "task_client_retry": "1s",
    "io_wait": "30s",
    "qmp_command": "5s"
  }
}
```

### `timeouts.vm_start`
- **Type**: duration string
- **Default**: `"10s"`
- **Description**: Maximum time to wait for VM to boot and become ready
- **Examples**: `"5s"`, `"15s"`, `"30s"`

### `timeouts.device_detection`
- **Type**: duration string
- **Default**: `"5s"`
- **Description**: Maximum time for guest to detect hotplugged devices (disks, NICs)
- **Note**: Increase if using slow storage backends

### `timeouts.shutdown_grace`
- **Type**: duration string
- **Default**: `"2s"`
- **Description**: Grace period after sending shutdown signal before forceful termination (SIGKILL)
- **Note**: Allows processes to clean up before forced shutdown

### `timeouts.event_reconnect`
- **Type**: duration string
- **Default**: `"2s"`
- **Description**: Timeout for reconnecting to the guest event stream after disconnection

### `timeouts.task_client_retry`
- **Type**: duration string
- **Default**: `"1s"`
- **Description**: Timeout for retrying vsock dial operations to guest
- **Note**: Controls connection retry behavior during VM startup

### `timeouts.io_wait`
- **Type**: duration string
- **Default**: `"30s"`
- **Description**: Maximum time to wait for I/O forwarders to complete during shutdown
- **Note**: Ensures stdout/stderr data is flushed before container exits

### `timeouts.qmp_command`
- **Type**: duration string
- **Default**: `"5s"`
- **Description**: Timeout for QMP (QEMU Machine Protocol) commands
- **Note**: Affects hotplug operations and VM queries

## CPU Hotplug Configuration

Controls dynamic CPU allocation for VMs.

```json
{
  "cpu_hotplug": {
    "monitor_interval": "5s",
    "scale_up_cooldown": "10s",
    "scale_down_cooldown": "30s",
    "scale_up_threshold": 80.0,
    "scale_down_threshold": 50.0,
    "scale_up_throttle_limit": 5.0,
    "scale_up_stability": 2,
    "scale_down_stability": 6,
    "enable_scale_down": true
  }
}
```

### `cpu_hotplug.monitor_interval`
- **Type**: duration string
- **Default**: `"5s"`
- **Description**: How often to check CPU usage
- **Examples**: `"1s"`, `"10s"`, `"1m"`, `"30s"`
- **Validation**: Must be a valid Go duration

### `cpu_hotplug.scale_up_cooldown`
- **Type**: duration string
- **Default**: `"10s"`
- **Description**: Minimum time between scale-up operations
- **Purpose**: Prevents rapid oscillation

### `cpu_hotplug.scale_down_cooldown`
- **Type**: duration string
- **Default**: `"30s"`
- **Description**: Minimum time between scale-down operations
- **Purpose**: Prevents rapid oscillation (longer than scale-up for stability)

### `cpu_hotplug.scale_up_threshold`
- **Type**: float64 (percentage)
- **Default**: `80.0`
- **Range**: 0-100
- **Description**: CPU usage percentage to trigger adding a vCPU
- **Example**: `80.0` = add vCPU when usage exceeds 80%

### `cpu_hotplug.scale_down_threshold`
- **Type**: float64 (percentage)
- **Default**: `50.0`
- **Range**: 0-100
- **Description**: Target CPU utilization after removing one vCPU
- **Example**: `50.0` = remove vCPU only if projected usage stays under 50%
- **Note**: Uses projected utilization calculation: `current_usage * current_cpus / (current_cpus - 1)`

### `cpu_hotplug.scale_up_throttle_limit`
- **Type**: float64 (percentage)
- **Default**: `5.0`
- **Range**: 0-100
- **Description**: Don't scale up if CPU throttling exceeds this percentage
- **Purpose**: Prevents scaling when container is already CPU-limited by cgroups

### `cpu_hotplug.scale_up_stability`
- **Type**: integer
- **Default**: `2`
- **Range**: >0
- **Description**: Number of consecutive high readings required before scaling up
- **Example**: `2` = need 2 consecutive readings above threshold (10s total at default interval)

### `cpu_hotplug.scale_down_stability`
- **Type**: integer
- **Default**: `6`
- **Range**: >0
- **Description**: Number of consecutive low readings required before scaling down
- **Example**: `6` = need 6 consecutive readings below threshold (30s total at default interval)

### `cpu_hotplug.enable_scale_down`
- **Type**: boolean
- **Default**: `true`
- **Description**: Allow removing vCPUs
- **Note**: Disabled by default in cpuhotplug defaults (kernel support varies)

## Memory Hotplug Configuration

Controls dynamic memory allocation for VMs.

```json
{
  "memory_hotplug": {
    "monitor_interval": "10s",
    "scale_up_cooldown": "30s",
    "scale_down_cooldown": "60s",
    "scale_up_threshold": 85.0,
    "scale_down_threshold": 60.0,
    "oom_safety_margin_mb": 128,
    "increment_size_mb": 128,
    "scale_up_stability": 3,
    "scale_down_stability": 6,
    "enable_scale_down": false
  }
}
```

### `memory_hotplug.monitor_interval`
- **Type**: duration string
- **Default**: `"10s"`
- **Description**: How often to check memory usage
- **Note**: Slower than CPU monitoring (memory changes less frequently)

### `memory_hotplug.scale_up_cooldown`
- **Type**: duration string
- **Default**: `"30s"`
- **Description**: Minimum time between scale-up operations
- **Note**: Longer than CPU (memory operations are more expensive)

### `memory_hotplug.scale_down_cooldown`
- **Type**: duration string
- **Default**: `"60s"`
- **Description**: Minimum time between scale-down operations
- **Note**: Very conservative (memory unplug is risky)

### `memory_hotplug.scale_up_threshold`
- **Type**: float64 (percentage)
- **Default**: `85.0`
- **Range**: 0-100
- **Description**: Memory usage percentage to trigger adding memory
- **Example**: `85.0` = add memory when usage exceeds 85%

### `memory_hotplug.scale_down_threshold`
- **Type**: float64 (percentage)
- **Default**: `60.0`
- **Range**: 0-100
- **Description**: Memory usage percentage to trigger removing memory
- **Example**: `60.0` = remove memory when usage falls below 60%

### `memory_hotplug.oom_safety_margin_mb`
- **Type**: int64 (megabytes)
- **Default**: `128`
- **Range**: >0
- **Description**: Always keep this much free memory (OOM protection)
- **Purpose**: Prevents out-of-memory conditions

### `memory_hotplug.increment_size_mb`
- **Type**: int64 (megabytes)
- **Default**: `128`
- **Range**: >0, must be 128MB-aligned
- **Description**: Size of each memory hotplug/unplug operation
- **Constraint**: Must be multiple of 128MB (QEMU DIMM slot size)
- **Valid examples**: `128`, `256`, `512`, `1024`
- **Invalid examples**: `100`, `150`, `200`

### `memory_hotplug.scale_up_stability`
- **Type**: integer
- **Default**: `3`
- **Range**: >0
- **Description**: Number of consecutive high readings before scaling up
- **Example**: `3` = need 3 consecutive readings (30s total at default interval)

### `memory_hotplug.scale_down_stability`
- **Type**: integer
- **Default**: `6`
- **Range**: >0
- **Description**: Number of consecutive low readings before scaling down
- **Example**: `6` = need 6 consecutive readings (60s total at default interval)

### `memory_hotplug.enable_scale_down`
- **Type**: boolean
- **Default**: `false`
- **Description**: Allow removing memory
- **Warning**: EXPERIMENTAL - memory unplug is risky and may fail

## Configuration Loading

### Load Order
1. Check `SPINBOX_CONFIG` environment variable
2. If set, load from that path (fail if missing)
3. Otherwise, load from `/etc/spinbox/config.json` (fail if missing)

### Validation
Configuration is validated on load:
- All paths must exist/be writable
- All durations must parse correctly (e.g., "5s", "2m", "500ms")
- All timeout durations must be positive
- All thresholds must be in range (0-100)
- VMM must be "qemu"
- Memory increment must be 128MB-aligned

### Fail-Fast Behavior
If configuration is missing or invalid, spinbox will:
1. Print detailed error to stderr
2. Exit with status code 1
3. Never start with invalid/missing config

## Examples

### Minimal Configuration
```json
{
  "paths": {
    "share_dir": "/usr/share/spinbox",
    "state_dir": "/var/lib/spinbox",
    "log_dir": "/var/log/spinbox"
  },
  "runtime": {
    "vmm": "qemu"
  },
  "timeouts": {},
  "cpu_hotplug": {},
  "memory_hotplug": {}
}
```
Empty sections use defaults.

### Debug Configuration

To enable debug logging, configure it in containerd's config file (`/etc/containerd/config.toml`):

```toml
[debug]
  level = "debug"
```

Example spinbox config for aggressive hotplug testing:

```json
{
  "paths": {
    "share_dir": "/usr/share/spinbox",
    "state_dir": "/var/lib/spinbox",
    "log_dir": "/var/log/spinbox"
  },
  "runtime": {
    "vmm": "qemu"
  },
  "timeouts": {
    "vm_start": "30s",
    "io_wait": "60s"
  },
  "cpu_hotplug": {
    "monitor_interval": "1s",
    "enable_scale_down": true
  },
  "memory_hotplug": {
    "monitor_interval": "5s",
    "enable_scale_down": true
  }
}
```
Longer timeouts for debugging, fast hotplug monitoring.

### Conservative Configuration
```json
{
  "paths": {
    "share_dir": "/usr/share/spinbox",
    "state_dir": "/var/lib/spinbox",
    "log_dir": "/var/log/spinbox"
  },
  "runtime": {
    "vmm": "qemu"
  },
  "timeouts": {},
  "cpu_hotplug": {
    "scale_up_threshold": 90.0,
    "scale_down_threshold": 30.0,
    "enable_scale_down": false
  },
  "memory_hotplug": {
    "scale_up_threshold": 90.0,
    "scale_down_threshold": 50.0,
    "oom_safety_margin_mb": 256,
    "enable_scale_down": false
  }
}
```
High thresholds, large safety margin, no scale-down.

### Aggressive Scaling
```json
{
  "paths": {
    "share_dir": "/usr/share/spinbox",
    "state_dir": "/var/lib/spinbox",
    "log_dir": "/var/log/spinbox"
  },
  "runtime": {
    "vmm": "qemu"
  },
  "timeouts": {
    "qmp_command": "2s"
  },
  "cpu_hotplug": {
    "monitor_interval": "2s",
    "scale_up_threshold": 70.0,
    "scale_down_threshold": 60.0,
    "scale_up_stability": 1,
    "scale_down_stability": 3,
    "enable_scale_down": true
  },
  "memory_hotplug": {
    "monitor_interval": "5s",
    "scale_up_threshold": 75.0,
    "scale_down_threshold": 65.0,
    "increment_size_mb": 256,
    "scale_up_stability": 2,
    "scale_down_stability": 4,
    "enable_scale_down": true
  }
}
```
Low thresholds, fast response, scale-down enabled, shorter QMP timeout.

## Troubleshooting

### Error: "Config file not found"
```
FATAL: Failed to load spinbox configuration: config file not found at /etc/spinbox/config.json
```
**Solution**: Create config file from example:
```bash
sudo cp examples/config.json /etc/spinbox/config.json
```

### Error: "Invalid JSON"
```
FATAL: Failed to load spinbox configuration: failed to parse config file
```
**Solution**: Validate JSON syntax:
```bash
cat /etc/spinbox/config.json | jq .
```

### Error: "Kernel not found"
```
paths validation failed: kernel not found at /usr/share/spinbox/kernel/spinbox-kernel-x86_64
```
**Solution**: Build or install kernel:
```bash
task build:kernel
```

### Error: "Invalid threshold"
```
cpu_hotplug validation failed: scale_up_threshold must be between 0 and 100, got 150.0
```
**Solution**: Fix threshold value (must be 0-100).

### Error: "Increment size not aligned"
```
memory_hotplug validation failed: increment_size_mb must be 128MB-aligned, got 100
```
**Solution**: Use 128MB-aligned value (128, 256, 512, 1024, etc.).

## See Also

- [README.md](../README.md) - Main documentation
- [CLAUDE.md](../CLAUDE.md) - Architecture and development guide
- [examples/config.json](../examples/config.json) - Example configuration with defaults
