# Kernel config trim: 2026-09-07

Base: `07b92cb`, Linux 7.2.1, x86_64 PVH, i9-13900HK host.

The reduced config removes 3,467,772 bytes from ELF PT_LOAD file data
(11.30%) and 49 executed initcalls (7.40%). Production PID1 entry is
4.380 ms earlier by the difference of medians in an interleaved A/B run.
This is an aggregate result; it does not assign milliseconds to individual
drivers or prove how much of the gain comes from copying versus initialization.

## Scope and mechanism

The QEMU command builder uses modern PCI virtio block, network, RNG and vsock
devices, an ISA serial console, and ACPI CPU/memory hotplug. The config removes
unused SCSI/virtio-scsi, 9p, virtio-fs transport, virtio-console, virtio-input,
virtio-MMIO, virtio-pmem/RTC, guest-side vhost-vsock and RPMSG, ISO9660,
HID/USB, virtual terminals, physical Ethernet/PHY, MEI, PCI serial drivers,
and physical uncore/C-state PMUs. Generic FUSE remains available.

`olddefconfig` reduces enabled symbols from 1,369 to 1,264. The checked-in base
has 1,367; the comparison above uses the resolved configs of both built kernels.
The full config is normalized with `olddefconfig`, accounting for the large
deletion of now-inapplicable child options.

`nm` independently shows 49 removed initcall entries, including `init_scsi`,
`ip_vs_init`, `init_v9fs`, `virtio_fs_init`, `con_init`, `hvc_console_init`,
`hid_init`, `mei_init`, and the unused serial drivers. Runtime tracepoint counts
confirm the same reduction. The static tables contain 663/614 entries, while
the runtime profiler observes 662/613.

ACPI, CPU/memory hotplug, ACPI_BUTTON, required virtio drivers, erofs, overlay,
ext4, cgroups, seccomp, RNG and CPU mitigations remain enabled. Build checks
protect the PVH entry, serial console and required container/device features
after dependency resolution. `readelf -n` confirms Xen notes including type
0x12 (PVH entry); both artifacts remain ELF executables.

## Production measurements

Eight rounds of A then B. Each arm installs its kernel, restarts spinbox,
checks aggregate CPU use over 200 ms against a 25% ceiling, runs one normal
`ctr run --rm ... /bin/echo BOOTED`, then runs `TestBootLatency` (five boots).
Same main shim, initrd, QEMU and image in both arms. No debug annotations in
these samples, no concurrent builds, and no host governor/affinity/turbo changes.

| Metric | A: main | B: trimmed | Difference |
| --- | ---: | ---: | ---: |
| PT_LOAD filesz, bytes | 30,680,232 | 27,212,460 | -3,467,772 |
| Executed initcalls, one separate debug boot/arm | 662 | 613 | -49 |
| PID1 entry, ms, median [min–max], n=8 | 51.653 [49.029–83.537] | 47.274 [45.571–48.426] | -4.380 |
| First accept, ms, median [min–max], n=8 | 56.718 [53.546–87.624] | 51.948 [50.383–53.536] | -4.770 |
| BOOT_METRIC, ms, median of 8 batch medians [all-batch min–max], 40 boots/arm | 106 [106–108] | 106 [105–107] | 0 |

Raw production samples, in execution order within each arm:

| Round | A PID1 ms | B PID1 ms | A accept ms | B accept ms | A BOOT_METRIC min/median/max ms | B BOOT_METRIC min/median/max ms |
| --- | ---: | ---: | ---: | ---: | --- | --- |
| 1 | 83.537 | 47.153 | 87.624 | 52.193 | 106/106/107 | 105/106/107 |
| 2 | 52.686 | 48.426 | 56.869 | 53.536 | 106/107/107 | 106/106/107 |
| 3 | 49.029 | 47.034 | 54.012 | 50.833 | 106/106/108 | 106/106/107 |
| 4 | 49.768 | 45.963 | 53.793 | 51.282 | 106/106/106 | 106/106/107 |
| 5 | 58.920 | 47.683 | 63.296 | 52.922 | 106/106/107 | 106/106/107 |
| 6 | 49.725 | 47.394 | 53.546 | 53.016 | 106/106/106 | 106/106/107 |
| 7 | 51.825 | 45.571 | 57.013 | 50.383 | 106/106/107 | 106/106/107 |
| 8 | 51.481 | 47.775 | 56.567 | 51.703 | 106/106/107 | 106/106/107 |

All samples are retained, including A's 83.537 ms initial boot and 58.920 ms
sample. Their cause was not established. PID1 distributions do not overlap,
but these are eight samples on one host; the magnitude is not a universal
guarantee. Improvements below roughly 1.5 ms would not be conclusive with
this sample size and the established host noise floor.

`TestBootLatency` uses `waitForOutput`, which polls every 100 ms. Its unchanged
106 ms median cannot resolve a few milliseconds of kernel improvement. No
end-to-end `ctr run` speedup is claimed from this metric.

The debug boots are used only for counts: A reports
`initcalls=662 source=tracepoints refined=662/662`, B reports
`initcalls=613 source=tracepoints refined=613/613`. Their elapsed times are
excluded from production comparisons.

## Negatives and limits

- The first build failed before compilation because the inherited Docker
  checker requires `NETFILTER_XT_MATCH_IPVS`. Its specific Swarm requirement
  is excluded; the remaining networking checks still run and pass.
- Disabling FAT_FS alone did not survive `olddefconfig`; FAT/MSDOS/VFAT remain
  enabled and contribute no saving in this change.
- USB has no active host controller configured in the baseline. No individual
  boot-time benefit is claimed for disabling its menu.
- ACPI remains the largest debug initcall. This does not resolve its existing
  cost, the pre-kernel gap, host pipeline contention, or the rejected RNG and
  host-tuning proposals.
- The existing integration resource-constraints test checks the OCI spec;
  it does not exercise live CPU/memory hotplug. The hotplug config and ACPI
  code remain unchanged, with build assertions protecting the required flags.

## Validation and restoration

`task build:kernel` passes with the normalized config and feature assertions;
the resulting config matches the checked-in file exactly. `task lint` reports
zero issues (its cache writes were denied by the sandbox, without affecting
the result). `go test -race ./...` passes.

The complete integration binary, built with `go test -c -tags=integration`,
passes all 26 top-level tests with B, with no failures or skips. This includes
cold/hot commit, erofs+overlay writes, pause/resume, exec/event ordering,
production readiness and kernel profiling.

After testing, the installed kernel and `_output` kernel/config are restored
to A; installed shim/initrd are the main builds used for both arms. Spinbox
and the erofs snapshotter are stopped. Raw logs and both kernel artifacts
are retained locally under `/tmp/spinbox-kernel-trim.8w92wb`.

## Artifact identity

SHA-256 of the measured artifacts:

```text
A kernel  74fd456d3a6ed28257a5c5f7b598731976b5f9089d5da057317c4a8bb6a379f3
B kernel  a71b00cb84ba2d7dc58f2235903784d045e9f70046b8fefc5a777f3428b120d3
initrd    39cfec5eb09c50bcbb874018c578bebe6974f909ed82755d18c7dfe4384c06e5
shim      b6c8557bc8764ed34d91cffa581c83b531d15e45e588f0c686dd1a6cf5bc172c
```
