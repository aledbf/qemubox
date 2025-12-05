package task

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"

	"github.com/aledbf/beacon/containerd/mountutil"
	"github.com/aledbf/beacon/containerd/vm"
)

// translateMountOptions translates standard mount options to virtiofs-compatible options.
// virtiofs supports a subset of mount options. This function filters and translates
// options from bind/overlay mounts to virtiofs equivalents.
func translateMountOptions(ctx context.Context, options []string) []string {
	var translated []string

	// Map of mount options that are compatible with virtiofs
	// or need translation
	compatibleOptions := map[string]string{
		"ro":       "ro",
		"rw":       "rw",
		"nodev":    "nodev",
		"nosuid":   "nosuid",
		"noexec":   "noexec",
		"relatime": "relatime",
		"noatime":  "noatime",
	}

	// Options that should be dropped (not supported by virtiofs)
	droppedOptions := map[string]bool{
		"rbind":      true,
		"bind":       true,
		"rprivate":   true,
		"private":    true,
		"rshared":    true,
		"shared":     true,
		"rslave":     true,
		"slave":      true,
		"remount":    true,
		"strictatime": true,
	}

	for _, opt := range options {
		// Check if it's a compatible option
		if mappedOpt, ok := compatibleOptions[opt]; ok {
			translated = append(translated, mappedOpt)
			continue
		}

		// Check if it should be dropped
		if droppedOptions[opt] {
			log.G(ctx).WithField("option", opt).Debug("dropping incompatible virtiofs mount option")
			continue
		}

		// For options with values (e.g., "uid=1000"), check the prefix
		if strings.Contains(opt, "=") {
			parts := strings.SplitN(opt, "=", 2)
			switch parts[0] {
			case "uid", "gid", "fmode", "dmode":
				// These options might be supported, include them
				translated = append(translated, opt)
			default:
				// Unknown option with value, log and skip
				log.G(ctx).WithField("option", opt).Debug("skipping unknown virtiofs mount option")
			}
			continue
		}

		// Unknown option without value, log and skip
		log.G(ctx).WithField("option", opt).Debug("skipping unknown virtiofs mount option")
	}

	return translated
}

func setupMounts(ctx context.Context, vmi vm.Instance, id string, m []*types.Mount, rootfs, lmounts string) ([]*types.Mount, error) {
	// Handle mounts

	// Cloud Hypervisor doesn't support AddFS (virtiofs requires shared memory + virtiofsd)
	// Skip filesystem mounts for Cloud Hypervisor - rely on EROFS snapshotter instead
	vmInfo := vmi.VMInfo()
	if vmInfo.Type == "cloudhypervisor" {
		// For Cloud Hypervisor, only process mounts through transformMounts
		// which handles EROFS and other disk-based mounts
		if len(m) == 0 {
			// No mounts specified, return empty - EROFS snapshotter provides rootfs
			return nil, nil
		}
		// Let transformMounts handle all mounts for Cloud Hypervisor
		return transformMounts(ctx, vmi, id, m)
	}

	if len(m) == 1 && (m[0].Type == "overlay" || m[0].Type == "bind") {
		tag := fmt.Sprintf("rootfs-%s", id)
		// virtiofs implementation has a limit of 36 characters for the tag
		if len(tag) > 36 {
			tag = tag[:36]
		}
		mnt := mount.Mount{
			Type:    m[0].Type,
			Source:  m[0].Source,
			Options: m[0].Options,
		}
		if err := mnt.Mount(rootfs); err != nil {
			return nil, err
		}
		if err := vmi.AddFS(ctx, tag, rootfs); err != nil {
			return nil, err
		}
		return []*types.Mount{{
			Type:    "virtiofs",
			Source:  tag,
			Options: translateMountOptions(ctx, m[0].Options),
		}}, nil
	} else if len(m) == 0 {
		tag := fmt.Sprintf("rootfs-%s", id)
		// virtiofs implementation has a limit of 36 characters for the tag
		if len(tag) > 36 {
			tag = tag[:36]
		}
		if err := vmi.AddFS(ctx, tag, rootfs); err != nil {
			return nil, err
		}
		return []*types.Mount{{
			Type:   "virtiofs",
			Source: tag,
		}}, nil
	}
	mounts, err := transformMounts(ctx, vmi, id, m)
	if err != nil && errdefs.IsNotImplemented(err) {
		if err := mountutil.All(ctx, rootfs, lmounts, m); err != nil {
			return nil, err
		}

		// Fallback to original rootfs mount
		tag := fmt.Sprintf("rootfs-%s", id)
		// virtiofs implementation has a limit of 36 characters for the tag
		if len(tag) > 36 {
			tag = tag[:36]
		}
		if err := vmi.AddFS(ctx, tag, rootfs); err != nil {
			return nil, err
		}
		return []*types.Mount{{
			Type:   "virtiofs",
			Source: tag,
		}}, nil
	}
	return mounts, err
}
