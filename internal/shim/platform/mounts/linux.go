//go:build linux

package mounts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"

	"github.com/aledbf/qemubox/containerd/internal/host/erofs"
	"github.com/aledbf/qemubox/containerd/internal/host/mountutil"
	"github.com/aledbf/qemubox/containerd/internal/host/vm"
)

type linuxManager struct{}

func newManager() Manager {
	return &linuxManager{}
}

// truncateID ensures disk/tag IDs don't exceed QEMU's limits.
// QEMU restricts device IDs to 36 characters for internal tracking.
func truncateID(prefix, id string) string {
	const maxIDLen = 36
	tag := fmt.Sprintf("%s-%s", prefix, id)
	if len(tag) > maxIDLen {
		return tag[:maxIDLen]
	}
	return tag
}

func (m *linuxManager) Setup(ctx context.Context, vmi vm.Instance, id string, rootfsMounts []*types.Mount, bundleRootfs string, mountDir string) (SetupResult, error) {
	// Try virtiofs first (not currently implemented for QEMU), fall back to block devices

	if len(rootfsMounts) == 1 && (rootfsMounts[0].Type == "overlay" || rootfsMounts[0].Type == "bind") {
		tag := truncateID("rootfs", id)
		mnt := mount.Mount{
			Type:    rootfsMounts[0].Type,
			Source:  rootfsMounts[0].Source,
			Options: rootfsMounts[0].Options,
		}
		if err := mnt.Mount(bundleRootfs); err != nil {
			return SetupResult{}, err
		}
		if err := vmi.AddFS(ctx, tag, bundleRootfs); err != nil {
			return SetupResult{}, err
		}
		return SetupResult{Mounts: []*types.Mount{{
			Type:    "virtiofs",
			Source:  tag,
			Options: translateMountOptions(ctx, rootfsMounts[0].Options),
		}}}, nil
	} else if len(rootfsMounts) == 0 {
		tag := truncateID("rootfs", id)
		if err := vmi.AddFS(ctx, tag, bundleRootfs); err != nil {
			return SetupResult{}, err
		}
		return SetupResult{Mounts: []*types.Mount{{
			Type:   "virtiofs",
			Source: tag,
		}}}, nil
	}
	mounts, err := m.transformMounts(ctx, vmi, id, rootfsMounts)
	if err != nil && errdefs.IsNotImplemented(err) {
		cleanup, err := mountutil.All(ctx, bundleRootfs, mountDir, rootfsMounts)
		if err != nil {
			return SetupResult{}, err
		}

		// Fallback to original rootfs mount
		tag := truncateID("rootfs", id)
		if err := vmi.AddFS(ctx, tag, bundleRootfs); err != nil {
			if cleanup != nil {
				_ = cleanup(context.WithoutCancel(ctx))
			}
			return SetupResult{}, err
		}
		return SetupResult{
			Mounts: []*types.Mount{{
				Type:   "virtiofs",
				Source: tag,
			}},
			Cleanup: cleanup,
		}, nil
	}
	if err != nil {
		return SetupResult{}, err
	}
	return SetupResult{Mounts: mounts}, nil
}

type diskOptions struct {
	name     string
	source   string
	readOnly bool
	vmdk     bool
}

// transformMounts does not perform any local mounts but transforms
// the mounts to be used inside the VM via virtio.
func (m *linuxManager) transformMounts(ctx context.Context, vmi vm.Instance, id string, ms []*types.Mount) ([]*types.Mount, error) {
	disks := byte('a')
	var (
		addDisks []diskOptions
		am       []*types.Mount
	)

	for _, mnt := range ms {
		mounts, disksToAdd, err := m.transformMount(ctx, id, &disks, mnt)
		if err != nil {
			return nil, err
		}
		am = append(am, mounts...)
		addDisks = append(addDisks, disksToAdd...)
	}

	if len(addDisks) > 10 {
		return nil, fmt.Errorf("exceeded maximum virtio disk count: %d > 10: %w", len(addDisks), errdefs.ErrNotImplemented)
	}

	if err := m.addDisksToVM(ctx, vmi, addDisks); err != nil {
		return nil, err
	}

	return am, nil
}

func (m *linuxManager) transformMount(ctx context.Context, id string, disks *byte, mnt *types.Mount) ([]*types.Mount, []diskOptions, error) {
	switch mnt.Type {
	case "erofs":
		return m.handleEROFS(ctx, id, disks, mnt)
	case "ext4":
		return m.handleExt4(id, disks, mnt)
	case "overlay", "format/overlay", "format/mkdir/overlay":
		if err := m.validateOverlay(ctx, mnt); err != nil {
			return nil, nil, err
		}
		return []*types.Mount{mnt}, nil, nil
	default:
		// Mount types with mkfs/ or format/ prefixes require processing by the
		// mount manager to create backing files. Return ErrNotImplemented to
		// trigger the fallback path that uses mountutil.All().
		if strings.HasPrefix(mnt.Type, "mkfs/") || strings.HasPrefix(mnt.Type, "format/") {
			return nil, nil, fmt.Errorf("mount type %q requires mount manager: %w", mnt.Type, errdefs.ErrNotImplemented)
		}
		return []*types.Mount{mnt}, nil, nil
	}
}

func (m *linuxManager) handleEROFS(ctx context.Context, id string, disks *byte, mnt *types.Mount) ([]*types.Mount, []diskOptions, error) {
	disk := truncateID(fmt.Sprintf("disk-%d", *disks), id)

	var options []string
	devices := []string{mnt.Source}

	// Extract device= options which specify additional EROFS layers
	for _, o := range mnt.Options {
		if d, found := strings.CutPrefix(o, "device="); found {
			devices = append(devices, d)
			continue
		}
		options = append(options, o)
	}

	addDisks := []diskOptions{{
		name:     disk,
		source:   mnt.Source,
		readOnly: true,
		vmdk:     false,
	}}

	// If multiple layers, generate VMDK descriptor to merge them
	if len(devices) > 1 {
		mergedfsPath := filepath.Dir(mnt.Source) + "/merged_fs.vmdk"
		if _, err := os.Stat(mergedfsPath); err != nil {
			if !os.IsNotExist(err) {
				return nil, nil, fmt.Errorf("failed to stat merged EROFS descriptor %s: %w", mergedfsPath, err)
			}
			if err := erofs.DumpVMDKDescriptorToFile(mergedfsPath, 0xfffffffe, devices); err != nil {
				return nil, nil, fmt.Errorf("failed to generate merged EROFS descriptor %s: %w", mergedfsPath, err)
			}
		}
		addDisks[0].source = mergedfsPath
		addDisks[0].vmdk = true
	}

	out := &types.Mount{
		Type:    "erofs",
		Source:  fmt.Sprintf("/dev/vd%c", *disks),
		Target:  mnt.Target,
		Options: filterOptions(options),
	}
	*disks++
	return []*types.Mount{out}, addDisks, nil
}

func (m *linuxManager) handleExt4(id string, disks *byte, mnt *types.Mount) ([]*types.Mount, []diskOptions, error) {
	disk := truncateID(fmt.Sprintf("disk-%d", *disks), id)
	// Check if mount should be read-only
	readOnly := false
	for _, opt := range mnt.Options {
		if opt == "ro" || opt == "readonly" {
			readOnly = true
			break
		}
	}
	mnt.Options = filterOptions(mnt.Options)
	out := &types.Mount{
		Type:    "ext4",
		Source:  fmt.Sprintf("/dev/vd%c", *disks),
		Target:  mnt.Target,
		Options: mnt.Options,
	}
	*disks++

	addDisks := []diskOptions{{
		name:     disk,
		source:   mnt.Source,
		readOnly: readOnly,
		vmdk:     false,
	}}
	return []*types.Mount{out}, addDisks, nil
}

func (m *linuxManager) validateOverlay(ctx context.Context, mnt *types.Mount) error {
	var (
		wdi = -1
		udi = -1
	)
	for i, opt := range mnt.Options {
		if strings.HasPrefix(opt, "upperdir=") {
			udi = i
		} else if strings.HasPrefix(opt, "workdir=") {
			wdi = i
		}
		// Note: virtiofs for lowerdir is not handled here.
	}
	if wdi > -1 && udi > -1 {
		//
		// If any upperdir or workdir isn't transformed, they both
		// should fall back to virtiofs passthroughfs.  But...
		//
		if !strings.Contains(mnt.Options[wdi], "{{") ||
			!strings.Contains(mnt.Options[udi], "{{") {
			// Having the upper as virtiofs may return invalid argument, avoid
			// transforming and attempt to perform the mounts on the host if
			// supported.
			return fmt.Errorf("cannot use virtiofs for upper dir in overlay: %w", errdefs.ErrNotImplemented)
		}
	} else {
		log.G(ctx).WithField("options", mnt.Options).Warnf("overlayfs missing workdir or upperdir")
	}
	return nil
}

func (m *linuxManager) addDisksToVM(ctx context.Context, vmi vm.Instance, disks []diskOptions) error {
	for _, do := range disks {
		var opts []vm.MountOpt
		if do.readOnly {
			opts = append(opts, vm.WithReadOnly())
		}
		if do.vmdk {
			opts = append(opts, vm.WithVmdk())
		}
		if err := vmi.AddDisk(ctx, do.name, do.source, opts...); err != nil {
			return err
		}
	}
	return nil
}

func filterOptions(options []string) []string {
	var filtered []string
	for _, o := range options {
		switch o {
		case "loop":
		default:
			filtered = append(filtered, o)
		}
	}
	return filtered
}

// translateMountOptions will translate mount options when virtiofs is implemented.
// TODO(virtiofs): Implement option translation when adding virtiofs support.
// Current implementation uses virtio-blk, not virtiofs, so this function is not
// exercised. When virtiofs is added, this should translate mount options appropriately.
func translateMountOptions(ctx context.Context, options []string) []string {
	// Pass through options unchanged for now
	// AddFS() currently returns ErrNotImplemented, so this code path is not reached
	return options
}
