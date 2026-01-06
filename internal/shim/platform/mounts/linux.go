//go:build linux

package mounts

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"

	"github.com/aledbf/qemubox/containerd/internal/host/mountutil"
	"github.com/aledbf/qemubox/containerd/internal/host/vm"
)

const mountTypeOverlay = "overlay"

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

	if len(rootfsMounts) == 1 && (rootfsMounts[0].Type == mountTypeOverlay || rootfsMounts[0].Type == "bind") {
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

	// Check if we need to generate an overlay mount.
	// When nexus-erofs provides multiple EROFS layers + ext4 (no overlay mount provided),
	// we need to generate one to combine them into a usable rootfs.
	am = m.maybeGenerateOverlay(ctx, am)

	return am, nil
}

func (m *linuxManager) transformMount(ctx context.Context, id string, disks *byte, mnt *types.Mount) ([]*types.Mount, []diskOptions, error) {
	switch mnt.Type {
	case "erofs", "format/erofs":
		// format/erofs is used by nexus-erofs for multi-device EROFS mounts (fsmeta with device= options).
		// The format/ prefix signals that containerd's standard mount manager cannot handle this type,
		// but qemubox can - it extracts file paths and converts them to virtio-blk devices.
		return m.handleEROFS(ctx, id, disks, mnt)
	case "ext4":
		return m.handleExt4(id, disks, mnt)
	case mountTypeOverlay, "format/overlay", "format/mkdir/overlay":
		return m.handleOverlay(ctx, id, mnt)
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

func (m *linuxManager) handleEROFS(_ context.Context, id string, disks *byte, mnt *types.Mount) ([]*types.Mount, []diskOptions, error) {
	disk := truncateID(fmt.Sprintf("disk-%d", *disks), id)

	source := mnt.Source
	isVMDK := false

	// Check if this is an fsmeta.erofs with device= options (from nexus-erofs).
	// If so, look for merged.vmdk which allows using a single QEMU device.
	if strings.HasSuffix(source, "fsmeta.erofs") {
		// Check for merged.vmdk in same directory
		vmdkPath := strings.TrimSuffix(source, "fsmeta.erofs") + "merged.vmdk"
		if fi, err := os.Stat(vmdkPath); err == nil && fi.Size() > 0 {
			source = vmdkPath
			isVMDK = true
		}
	} else if strings.HasSuffix(source, ".vmdk") {
		isVMDK = true
	}

	addDisks := []diskOptions{{
		name:     disk,
		source:   source,
		readOnly: true,
		vmdk:     isVMDK,
	}}

	// When using VMDK, the guest doesn't need device= options - it's a single device.
	// Filter out device= options from guest mount.
	var guestOptions []string
	for _, opt := range mnt.Options {
		if !strings.HasPrefix(opt, "device=") {
			guestOptions = append(guestOptions, opt)
		}
	}

	out := &types.Mount{
		Type:    "erofs",
		Source:  fmt.Sprintf("/dev/vd%c", *disks),
		Target:  mnt.Target,
		Options: filterOptions(guestOptions),
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

// handleOverlay transforms an overlay mount for VM use.
//
// For VMs without virtiofs support (e.g., QEMU), the overlay's upperdir/workdir
// paths (which are host paths) need to be transformed to use VM-accessible storage.
// This function transforms them to use tmpfs-based paths inside the guest.
//
// The lowerdir template (e.g., {{ overlay 0 4 }}) is preserved and will be
// expanded by the guest's mountutil to reference the EROFS mount points.
//
// The transformation works by:
// 1. Parsing the lowerdir template to find the mount indices (e.g., "0 4" from "{{ overlay 0 4 }}")
// 2. Adding a tmpfs mount as the next mount after the EROFS layers
// 3. Using templates to reference the tmpfs mount for upperdir/workdir
func (m *linuxManager) handleOverlay(ctx context.Context, id string, mnt *types.Mount) ([]*types.Mount, []diskOptions, error) {
	var (
		wdi           = -1
		udi           = -1
		hasLowerTmpl  = false
		needTransform = false
		overlayEnd    = -1
	)

	// Regex to extract overlay range from lowerdir template
	overlayRe := regexp.MustCompile(`\{\{\s*overlay\s+(\d+)\s+(\d+)\s*\}\}`)

	// Find option indices and check for templates
	for i, opt := range mnt.Options {
		switch {
		case strings.HasPrefix(opt, "upperdir="):
			udi = i
			if !strings.Contains(opt, "{{") {
				needTransform = true
			}
		case strings.HasPrefix(opt, "workdir="):
			wdi = i
			if !strings.Contains(opt, "{{") {
				needTransform = true
			}
		case strings.HasPrefix(opt, "lowerdir="):
			if strings.Contains(opt, "{{") {
				hasLowerTmpl = true
				// Extract the end index from overlay template
				if matches := overlayRe.FindStringSubmatch(opt); len(matches) == 3 {
					start, _ := strconv.Atoi(matches[1])
					end, _ := strconv.Atoi(matches[2])
					if end > start {
						overlayEnd = end
					} else {
						overlayEnd = start
					}
				}
			}
		}
	}

	// If lowerdir has a template but upperdir/workdir are host paths,
	// transform them to use tmpfs inside the guest.
	// This enables running containers with EROFS layers in VMs without virtiofs.
	if hasLowerTmpl && needTransform && udi >= 0 && wdi >= 0 && overlayEnd >= 0 {
		// The tmpfs mount will be at index overlayEnd+1
		// We use templates to reference it for upperdir/workdir
		tmpfsIndex := overlayEnd + 1

		log.G(ctx).WithFields(log.Fields{
			"id":         id,
			"overlayEnd": overlayEnd,
			"tmpfsIndex": tmpfsIndex,
			"needsUpper": needTransform,
		}).Debug("transforming overlay for VM use with tmpfs upper/work dirs")

		// Create a copy of options with transformed paths using templates
		// Pre-allocate capacity for original options + 2 mkdir paths
		newOpts := make([]string, 0, len(mnt.Options)+2)
		for i, opt := range mnt.Options {
			switch i {
			case udi:
				// Transform upperdir to use tmpfs mount point via template
				newOpts = append(newOpts, fmt.Sprintf("upperdir={{ mount %d }}/upper", tmpfsIndex))
			case wdi:
				// Transform workdir to use tmpfs mount point via template
				newOpts = append(newOpts, fmt.Sprintf("workdir={{ mount %d }}/work", tmpfsIndex))
			default:
				newOpts = append(newOpts, opt)
			}
		}

		// Add X-containerd.mkdir.path options to create the directories
		// These are processed by the mkdir/ prefix handler before mounting
		// The templates are expanded by format/ prefix first
		newOpts = append(newOpts,
			fmt.Sprintf("X-containerd.mkdir.path={{ mount %d }}/upper", tmpfsIndex),
			fmt.Sprintf("X-containerd.mkdir.path={{ mount %d }}/work", tmpfsIndex),
		)

		// Create a tmpfs mount that will be mounted at index tmpfsIndex
		// This provides ephemeral storage for overlay's upper/work directories
		tmpfsMount := &types.Mount{
			Type:   "tmpfs",
			Source: "tmpfs",
			Options: []string{
				"mode=0755",
			},
		}

		// The overlay mount with transformed options
		// Use format/mkdir/ prefix to:
		// 1. format/ - expand templates (lowerdir, upperdir, workdir, mkdir paths)
		// 2. mkdir/ - create upper/work directories inside tmpfs
		overlayMount := &types.Mount{
			Type:    "format/mkdir/overlay",
			Source:  mnt.Source,
			Target:  mnt.Target,
			Options: newOpts,
		}

		return []*types.Mount{tmpfsMount, overlayMount}, nil, nil
	}

	// No transformation needed - pass through as-is
	if wdi < 0 || udi < 0 {
		log.G(ctx).WithField("options", mnt.Options).Warn("overlayfs missing workdir or upperdir")
	}

	return []*types.Mount{mnt}, nil, nil
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

// maybeGenerateOverlay detects when mounts contain multiple EROFS layers followed by
// an ext4 layer (from nexus-erofs snapshotter) and generates an overlay mount to combine them.
//
// Pattern detected:
//   - 1+ erofs mounts (read-only lower layers)
//   - 1 ext4 mount at the end (writable upper layer)
//   - No existing overlay mount
//
// Generated overlay uses:
//   - lowerdir: all EROFS mount points (via {{ overlay 0 N }} template)
//   - upperdir/workdir: directories inside the ext4 mount (for persistence)
func (m *linuxManager) maybeGenerateOverlay(ctx context.Context, mounts []*types.Mount) []*types.Mount {
	if len(mounts) < 2 {
		return mounts
	}

	// Check if an overlay mount already exists
	for _, mnt := range mounts {
		if mnt.Type == mountTypeOverlay || strings.Contains(mnt.Type, mountTypeOverlay) {
			return mounts
		}
	}

	// Count EROFS mounts and find ext4 mount
	var erofsCount int
	var ext4Index int = -1
	for i, mnt := range mounts {
		switch mnt.Type {
		case "erofs":
			erofsCount++
		case "ext4":
			ext4Index = i
		}
	}

	// Pattern: 1+ EROFS layers + ext4 as the last mount
	if erofsCount == 0 || ext4Index != len(mounts)-1 {
		return mounts
	}

	log.G(ctx).WithFields(log.Fields{
		"erofs_count": erofsCount,
		"ext4_index":  ext4Index,
	}).Debug("generating overlay mount for nexus-erofs layers")

	// The EROFS layers are mounted at indices 0 to erofsCount-1
	// The ext4 layer is mounted at index ext4Index (= erofsCount)
	// Upper/work directories go inside the ext4 mount for persistence
	erofsEndIndex := erofsCount - 1

	// Build overlay options using templates:
	// - lowerdir references EROFS mount points in order (newest first, as provided by nexus-erofs)
	// - upperdir/workdir reference directories inside the ext4 mount (persistent)
	// Note: {{ overlay 0 N }} expands ascending: mounts/0:mounts/1:...:mounts/N
	// nexus-erofs provides layers newest-first, which matches overlay's expected lowerdir order
	overlayOpts := []string{
		fmt.Sprintf("lowerdir={{ overlay 0 %d }}", erofsEndIndex), // ascending: newest first
		fmt.Sprintf("upperdir={{ mount %d }}/upper", ext4Index),
		fmt.Sprintf("workdir={{ mount %d }}/work", ext4Index),
		// mkdir directives to create upper/work dirs inside ext4
		fmt.Sprintf("X-containerd.mkdir.path={{ mount %d }}/upper", ext4Index),
		fmt.Sprintf("X-containerd.mkdir.path={{ mount %d }}/work", ext4Index),
	}

	// Create new mount list:
	// 1. All EROFS mounts (indices 0 to erofsCount-1)
	// 2. ext4 mount (index erofsCount) - used for upperdir/workdir
	// 3. overlay mount (becomes the rootfs)
	newMounts := make([]*types.Mount, 0, len(mounts)+1)
	newMounts = append(newMounts, mounts...) // All original mounts

	// Add overlay mount with format/mkdir prefixes for template expansion
	newMounts = append(newMounts, &types.Mount{
		Type:    "format/mkdir/overlay",
		Source:  mountTypeOverlay,
		Options: overlayOpts,
	})

	return newMounts
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
