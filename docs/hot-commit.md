# Hot Commit Runbook

This document describes how to commit a container's writable layer to a new
image layer **while keeping the container around**, using only standard
containerd operations. It also documents the invariants spinbox provides so the
read is consistent and the snapshotter's safety gate is not violated.

## Overview

A container's writes land in a writable ext4 layer (`rwlayer.img`) that QEMU
exposes to the guest as a virtio-blk device. Committing means turning the
current contents of that layer into an immutable image layer.

There are two ways to do it:

| Mode | When | Disk consistency from | QEMU lock on rwlayer.img |
|------|------|-----------------------|--------------------------|
| **Cold** | container stopped | `PrepareShutdown` sync on poweroff | released (process gone) |
| **Hot** | container kept (paused) | `Pause` → `FIFREEZE` | still held (process alive) |

Both are driven by standard containerd verbs. spinbox adds no custom commit RPC.

## Why naive "commit while running" is unsafe

A running container has dirty pages in the guest page cache that have not been
written through virtio-blk to `rwlayer.img`. Reading the host file at that moment
yields a **torn filesystem** (missing writes, an open journal).

Two mechanisms protect against this:

1. **OFD lock gate (safety).** spinbox pins `file.locking=on` on the writable
   `-drive` (see `internal/host/vm/qemu/spec.go`). While the QEMU
   process is alive, it holds an image lock on `rwlayer.img`. The snapshotter's
   commit path takes an OFD `F_WRLCK` on the same inode to claim exclusive
   ownership; because QEMU holds the lock, that **commit fails loudly** instead
   of silently producing a corrupt layer. This gate is always on — it is the
   backstop, not the hot-commit path.

2. **Filesystem quiesce (consistency).** To read a consistent image *without*
   stopping the container, the guest filesystem must be frozen first. That is
   what `Pause` now does.

## What Pause / Resume guarantee

spinbox folds the quiesce into the standard containerd `Pause`/`Resume` verbs
(there is no containerd "quiesce" verb, so this is the compatible surface):

- **`Pause`** (`internal/shim/task/service.go`):
  1. `FreezeFilesystems` (vsock RPC to vminitd) issues `FIFREEZE` on the
     writable ext4 layer. This flushes the guest page cache to the block device
     (QEMU writes it through to `rwlayer.img`) and quiesces the on-disk
     filesystem (journal closed, no in-flight writes). FIFREEZE runs in the live
     guest, so it happens **before** the vCPUs are stopped.
  2. QMP `stop` suspends the vCPUs.
  - After `Pause` returns successfully, **`rwlayer.img` on the host is a
    consistent filesystem image.** If the freeze fails, `Pause` fails (and rolls
    the freeze back) — a successful `Pause` always implies a consistent disk.
  - While paused, the whole VM (including vminitd) is frozen, so `State`
    short-circuits to `PAUSED` from cached process state.

- **`Resume`**: QMP `cont` restarts the vCPUs, then `ThawFilesystems`
  (`FITHAW`) unfreezes the filesystem (FITHAW also needs the live guest, so it
  runs **after** `cont`).

> Note: QMP `stop` alone does **not** make the disk consistent — it only halts
> vCPUs and does not flush the guest page cache. The `FIFREEZE` is the part that
> makes the rwlayer safe to read.

## Hot-commit sequence

```bash
# 1. Quiesce: freeze the writable fs and suspend the container.
ctr task pause <container-id>
#    -> shim: FreezeFilesystems (FIFREEZE) then QMP stop
#    -> rwlayer.img is now consistent on disk

# 2. Commit: the snapshotter reads the (frozen, consistent) rwlayer.
#    See "Snapshotter contract" below.
#    <snapshotter hot-commit of the active snapshot>

# 3. Unquiesce: resume the container.
ctr task resume <container-id>
#    -> shim: QMP cont then ThawFilesystems (FITHAW)
```

If step 2 fails, still run `ctr task resume` so the container is not left frozen.

## Snapshotter contract for hot commit

The QEMU process is still alive while the container is paused, so it **still
holds the `file.locking` lock** on `rwlayer.img`. Therefore a hot commit must:

- **Read `rwlayer.img` read-only.** The data is consistent because the guest
  filesystem is frozen; a plain read does not require (and must not wait for)
  the exclusive `F_WRLCK`. QEMU's advisory image lock does not prevent another
  process from `read()`-ing the bytes.
- **Not take the exclusive `F_WRLCK`.** That lock is the ownership gate; it will
  conflict with the live QEMU and is reserved for cold commit (after the VM is
  gone). Attempting it against a paused-but-alive VM is the "fails loudly" path
  and is expected to fail.
- **Only read while the task is `PAUSED`.** Reading a `RUNNING` task's
  `rwlayer.img` is unsafe (not frozen). Verify status via `ctr task ls` /
  `State` before reading.

In short: the freeze provides consistency for a read-only copy; the OFD
`F_WRLCK` gate remains the guard for destructive/cold commits.

## Cold commit (no pause needed)

When the container is being stopped anyway, no freeze is required:

```bash
ctr task kill -s SIGTERM <container-id>   # or task delete
# -> on poweroff the shim calls PrepareShutdown (guest sync) and QEMU exits,
#    releasing the rwlayer.img lock.
# <snapshotter commit of the active snapshot>   # F_WRLCK now succeeds
```

## Failure handling

- `Pause` rolls back the freeze if QMP `stop` fails, so a failed `Pause` never
  leaves the filesystem frozen.
- `Resume` thaws after `cont`; always call `Resume` after a hot-commit attempt,
  even on commit failure, to guarantee the filesystem is thawed.
- If a shim/VM crashes while paused, the QEMU process dies, which releases the
  `rwlayer.img` lock and (kernel-side) the freeze, so no manual cleanup of the
  frozen state is required.

## Related code

- `internal/shim/task/service.go` — `Pause`/`Resume` (freeze→stop / cont→thaw),
  `State` PAUSED short-circuit.
- `internal/guest/vminit/system/freeze.go` — `FIFREEZE`/`FITHAW` on the writable
  ext4 layer.
- `api/services/system/v1/info.proto` — `FreezeFilesystems`/`ThawFilesystems`
  (internal vsock RPC).
- `internal/host/vm/qemu/spec.go` — `file.locking=on` on the rwlayer
  drive (the OFD lock gate).
