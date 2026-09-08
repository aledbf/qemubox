//go:build linux

package mounts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	digest "github.com/opencontainers/go-digest"

	"github.com/spin-stack/spinbox/internal/host/mountutil"
	"github.com/spin-stack/spinbox/internal/host/vm"
)

const mountTypeOverlay = "overlay"

// maxVirtioDisks bounds how many individual layer block devices we attach.
// In the normal path the snapshotter collapses all EROFS layers into a single
// merged VMDK, so a container needs only a couple of devices. Hitting this
// limit almost always means the merged VMDK (fsmeta) is not ready yet.
const maxVirtioDisks = 10

const (
	// layerBlobExt is the extension of EROFS layer blob files.
	layerBlobExt = ".erofs"
	// manifestFilename is the snapshotter's per-snapshot layer manifest. It has
	// one line per layer in VMDK order (oldest first), positionally 1:1 with the
	// device= options: "sha256:<hex>" for content-addressed blobs and
	// "blob:<basename>" for fallback blobs. Written atomically by the snapshotter.
	manifestFilename = "layers.manifest"
	// fallbackTokenPrefix labels manifest entries for blobs without a content
	// digest (e.g. snapshot-<id>.erofs), keeping the file 1:1 with device=.
	fallbackTokenPrefix = "blob:"
)

// layerDigestFromDevicePath mirrors the snapshotter's DigestFromLayerBlobPath:
// a blob named "sha256-<hex>.erofs" maps to the digest "sha256:<hex>". Returns
// "" for fallback-named blobs (e.g. snapshot-xxx.erofs), which are intentionally
// omitted from layers.manifest.
func layerDigestFromDevicePath(p string) string {
	name := filepath.Base(p)
	if !strings.HasSuffix(name, layerBlobExt) {
		return ""
	}
	name = strings.TrimSuffix(name, layerBlobExt)
	ds := strings.Replace(name, "-", ":", 1)
	if _, err := digest.Parse(ds); err != nil {
		return ""
	}
	return ds
}

// verifyVMDKManifest checks that the merged VMDK's layers.manifest (sibling of
// vmdkPath) matches the layer devices exactly, line-for-line in order. The
// manifest is 1:1 with the device= options: each line is the content digest of
// the layer blob, or "blob:<basename>" for fallback blobs without a digest.
//
// The snapshotter writes the manifest atomically, so a present manifest is
// always complete; a missing manifest (older snapshot or a logged write
// failure) is tolerated. A present manifest that disagrees with the device set
// indicates a stale or incomplete VMDK and is rejected.
func verifyVMDKManifest(vmdkPath string, options []string) error {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(vmdkPath), manifestFilename))
	if os.IsNotExist(err) {
		return nil // nothing to verify (best-effort manifest)
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", manifestFilename, err)
	}

	var manifest []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			manifest = append(manifest, line)
		}
	}
	if len(manifest) == 0 {
		return nil // empty manifest: do not gate on it
	}

	// Expected token per device= option, in order: the content digest, or a
	// "blob:<basename>" placeholder for fallback blobs (mirrors the snapshotter).
	var expected []string
	for _, opt := range options {
		blob, ok := strings.CutPrefix(opt, "device=")
		if !ok {
			continue
		}
		if d := layerDigestFromDevicePath(blob); d != "" {
			expected = append(expected, d)
		} else {
			expected = append(expected, fallbackTokenPrefix+filepath.Base(blob))
		}
	}

	if !slices.Equal(manifest, expected) {
		return fmt.Errorf(
			"merged VMDK %s manifest does not match layer devices (manifest=%d, devices=%d): "+
				"stale or incomplete VMDK generation: %w",
			filepath.Base(vmdkPath), len(manifest), len(expected), errdefs.ErrFailedPrecondition)
	}
	return nil
}

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

func (m *linuxManager) Setup(ctx context.Context, vmi vm.Instance, id string, rootfsMounts []*types.Mount) (SetupResult, error) {
	// Transform mounts to use virtio-blk devices inside the VM
	mounts, err := m.transformMounts(ctx, vmi, id, rootfsMounts)
	if err != nil {
		return SetupResult{}, err
	}
	return SetupResult{Mounts: mounts}, nil
}

type diskOptions struct {
	name     string
	source   string
	serial   string
	readOnly bool
	format   string
}

// blockSerial returns the virtio-blk serial for the disk at the given slot
// letter ('a', 'b', ...). The guest resolves the device by this serial via
// /sys/block/<dev>/serial, so the layer→device mapping does not depend on PCI
// enumeration order. Kept short to fit virtio-blk's 20-char serial limit.
func blockSerial(disk byte) string {
	return fmt.Sprintf("sbxblk%d", int(disk-'a'))
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

	if len(addDisks) > maxVirtioDisks {
		// Getting this many individual layer devices means the snapshotter handed
		// us the per-layer EROFS mounts instead of a single merged VMDK. That
		// normally happens because the merged VMDK (fsmeta) is generated
		// asynchronously and is not ready yet (the first View/Prepare can race
		// it), or because the layer set is incompatible and never produced an
		// fsmeta. Surface that explicitly so the caller waits/retries instead of
		// seeing an opaque device-count error.
		return nil, fmt.Errorf(
			"got %d individual EROFS layer devices (max %d): the merged VMDK/fsmeta is not ready yet "+
				"(generated asynchronously) or the layers are incompatible; wait for fsmeta generation or "+
				"check layer compatibility: %w",
			len(addDisks), maxVirtioDisks, errdefs.ErrFailedPrecondition)
	}

	if err := m.addDisksToVM(ctx, vmi, addDisks); err != nil {
		return nil, err
	}

	// Check if we need to generate an overlay mount.
	// When spin-erofs provides multiple EROFS layers + ext4 (no overlay mount provided),
	// we need to generate one to combine them into a usable rootfs.
	am = m.generateOverlayIfNeeded(ctx, am)

	return am, nil
}

func (m *linuxManager) transformMount(ctx context.Context, id string, disks *byte, mnt *types.Mount) ([]*types.Mount, []diskOptions, error) {
	switch mnt.Type {
	case "erofs", "format/erofs":
		// format/erofs is used by spin-erofs for multi-device EROFS mounts (fsmeta with device= options).
		// The format/ prefix signals that containerd's standard mount manager cannot handle this type,
		// but spinbox can - it extracts file paths and converts them to virtio-blk devices.
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

	// Check if this is an fsmeta.erofs with device= options (from spin-erofs).
	// If so, look for merged.vmdk which allows using a single QEMU device.
	if strings.HasSuffix(source, "fsmeta.erofs") {
		// Check for merged.vmdk in same directory
		vmdkPath := strings.TrimSuffix(source, "fsmeta.erofs") + "merged.vmdk"
		if fi, err := os.Stat(vmdkPath); err == nil && fi.Size() > 0 {
			// Before trusting the VMDK, verify its layers.manifest matches the
			// layer devices we were handed. A size>0 VMDK from a stale or
			// half-finished generation would otherwise be used blindly.
			if err := verifyVMDKManifest(vmdkPath, mnt.Options); err != nil {
				return nil, nil, err
			}
			source = vmdkPath
			isVMDK = true
		}
	} else if strings.HasSuffix(source, ".vmdk") {
		isVMDK = true
	}

	serial := blockSerial(*disks)
	format := vm.DefaultDiskFormat
	if isVMDK {
		format = "vmdk"
	}
	addDisks := []diskOptions{{
		name:     disk,
		source:   source,
		serial:   serial,
		readOnly: true,
		format:   format,
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
		Source:  mountutil.BlockSerialScheme + serial,
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
	serial := blockSerial(*disks)
	out := &types.Mount{
		Type:    "ext4",
		Source:  mountutil.BlockSerialScheme + serial,
		Target:  mnt.Target,
		Options: mnt.Options,
	}
	*disks++

	addDisks := []diskOptions{{
		name:     disk,
		source:   mnt.Source,
		serial:   serial,
		readOnly: readOnly,
		format:   vm.DefaultDiskFormat,
	}}
	return []*types.Mount{out}, addDisks, nil
}

// overlayAnalysis holds the results of analyzing overlay mount options.
type overlayAnalysis struct {
	workdirIdx    int  // index of workdir= option, -1 if not found
	upperdirIdx   int  // index of upperdir= option, -1 if not found
	hasLowerTmpl  bool // true if lowerdir uses a template
	needTransform bool // true if upperdir/workdir need transformation
	overlayEnd    int  // end index from overlay template, -1 if not found
}

// analyzeOverlayOptions examines overlay mount options to determine if transformation is needed.
func analyzeOverlayOptions(options []string) overlayAnalysis {
	result := overlayAnalysis{workdirIdx: -1, upperdirIdx: -1, overlayEnd: -1}
	overlayRe := regexp.MustCompile(`\{\{\s*overlay\s+(\d+)\s+(\d+)\s*\}\}`)

	for i, opt := range options {
		switch {
		case strings.HasPrefix(opt, "upperdir="):
			result.upperdirIdx = i
			if !strings.Contains(opt, "{{") {
				result.needTransform = true
			}
		case strings.HasPrefix(opt, "workdir="):
			result.workdirIdx = i
			if !strings.Contains(opt, "{{") {
				result.needTransform = true
			}
		case strings.HasPrefix(opt, "lowerdir="):
			if strings.Contains(opt, "{{") {
				result.hasLowerTmpl = true
				if matches := overlayRe.FindStringSubmatch(opt); len(matches) == 3 {
					start, _ := strconv.Atoi(matches[1])
					end, _ := strconv.Atoi(matches[2])
					result.overlayEnd = max(start, end)
				}
			}
		}
	}
	return result
}

// handleOverlay transforms an overlay mount for VM use.
//
// The overlay's upperdir/workdir paths (which are host paths) need to be transformed
// to use VM-accessible storage. This function transforms them to use tmpfs-based
// paths inside the guest.
//
// The lowerdir template (e.g., {{ overlay 0 4 }}) is preserved and will be
// expanded by the guest's mountutil to reference the EROFS mount points.
//
// The transformation works by:
// 1. Parsing the lowerdir template to find the mount indices (e.g., "0 4" from "{{ overlay 0 4 }}")
// 2. Adding a tmpfs mount as the next mount after the EROFS layers
// 3. Using templates to reference the tmpfs mount for upperdir/workdir
func (m *linuxManager) handleOverlay(ctx context.Context, id string, mnt *types.Mount) ([]*types.Mount, []diskOptions, error) {
	analysis := analyzeOverlayOptions(mnt.Options)

	// If lowerdir has a template but upperdir/workdir are host paths,
	// transform them to use tmpfs inside the guest.
	if analysis.hasLowerTmpl && analysis.needTransform && analysis.upperdirIdx >= 0 && analysis.workdirIdx >= 0 && analysis.overlayEnd >= 0 {
		// The tmpfs mount will be at index overlayEnd+1
		// We use templates to reference it for upperdir/workdir
		tmpfsIndex := analysis.overlayEnd + 1

		log.G(ctx).WithFields(log.Fields{
			"id":         id,
			"overlayEnd": analysis.overlayEnd,
			"tmpfsIndex": tmpfsIndex,
			"needsUpper": analysis.needTransform,
		}).Debug("transforming overlay for VM use with tmpfs upper/work dirs")

		// Create a copy of options with transformed paths using templates
		// Pre-allocate capacity for original options + 2 mkdir paths
		newOpts := make([]string, 0, len(mnt.Options)+2)
		for i, opt := range mnt.Options {
			switch i {
			case analysis.upperdirIdx:
				// Transform upperdir to use tmpfs mount point via template
				newOpts = append(newOpts, fmt.Sprintf("upperdir={{ mount %d }}/upper", tmpfsIndex))
			case analysis.workdirIdx:
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
	if analysis.workdirIdx < 0 || analysis.upperdirIdx < 0 {
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
		if do.format != "" {
			opts = append(opts, vm.WithFormat(do.format))
		}
		if do.serial != "" {
			opts = append(opts, vm.WithSerial(do.serial))
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

// generateOverlayIfNeeded detects when mounts contain multiple EROFS layers followed by
// an ext4 layer (from spin-erofs snapshotter) and generates an overlay mount to combine them.
//
// Pattern detected:
//   - 1+ erofs mounts (read-only lower layers)
//   - 1 ext4 mount at the end (writable upper layer)
//   - No existing overlay mount
//
// Generated overlay uses:
//   - lowerdir: all EROFS mount points (via {{ overlay 0 N }} template)
//   - upperdir/workdir: directories inside the ext4 mount (for persistence)
func (m *linuxManager) generateOverlayIfNeeded(ctx context.Context, mounts []*types.Mount) []*types.Mount {
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
	}).Debug("generating overlay mount for spin-erofs layers")

	// The EROFS layers are mounted at indices 0 to erofsCount-1
	// The ext4 layer is mounted at index ext4Index (= erofsCount)
	// Upper/work directories go inside the ext4 mount for persistence
	erofsEndIndex := erofsCount - 1

	// Build overlay options using templates:
	// - lowerdir references EROFS mount points in order (newest first, as provided by spin-erofs)
	// - upperdir/workdir reference directories inside the ext4 mount (persistent)
	// Note: {{ overlay 0 N }} expands ascending: mounts/0:mounts/1:...:mounts/N
	// spin-erofs provides layers newest-first, which matches overlay's expected lowerdir order
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
