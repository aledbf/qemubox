package task

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"

	"github.com/aledbf/beacon/containerd/erofs"
	"github.com/aledbf/beacon/containerd/vm"
	"github.com/aledbf/beacon/containerd/vm/cloudhypervisor"
)

type diskOptions struct {
	name     string
	source   string
	readOnly bool
	vmdk     bool
}

// transformMounts does not perform any local mounts but transforms
// the mounts to be used inside the VM via virtio
func transformMounts(ctx context.Context, vmi *cloudhypervisor.Instance, id string, ms []*types.Mount) ([]*types.Mount, error) {
	var (
		disks    byte = 'a'
		addDisks []diskOptions
		am       []*types.Mount
		err      error
	)

	for _, m := range ms {
		switch m.Type {
		case "erofs":
			disk := fmt.Sprintf("disk-%d-%s", disks, id)
			// virtiofs implementation has a limit of 36 characters for the tag
			if len(disk) > 36 {
				disk = disk[:36]
			}

			var options []string
			devices := []string{m.Source}

			// Extract device= options which specify additional EROFS layers
			for _, o := range m.Options {
				if d, found := strings.CutPrefix(o, "device="); found {
					devices = append(devices, d)
					continue
				}
				options = append(options, o)
			}

			// If multiple layers, generate VMDK descriptor to merge them
			if len(devices) > 1 {
				mergedfsPath := filepath.Dir(m.Source) + "/merged_fs.vmdk"
				if _, err := os.Stat(mergedfsPath); err != nil {
					if !os.IsNotExist(err) {
						log.G(ctx).WithError(err).Warnf("failed to stat %v", mergedfsPath)
						return nil, errdefs.ErrNotImplemented
					}
					if err := erofs.DumpVMDKDescriptorToFile(mergedfsPath, 0xfffffffe, devices); err != nil {
						log.G(ctx).WithError(err).Warnf("failed to generate %v", mergedfsPath)
						return nil, errdefs.ErrNotImplemented
					}
				}
				addDisks = append(addDisks, diskOptions{
					name:     disk,
					source:   mergedfsPath,
					readOnly: true,
					vmdk:     true,
				})
			} else {
				addDisks = append(addDisks, diskOptions{
					name:     disk,
					source:   m.Source,
					readOnly: true,
					vmdk:     false,
				})
			}

			am = append(am, &types.Mount{
				Type:    "erofs",
				Source:  fmt.Sprintf("/dev/vd%c", disks),
				Target:  m.Target,
				Options: filterOptions(options),
			})
			disks++
		case "ext4":
			disk := fmt.Sprintf("disk-%d-%s", disks, id)
			// virtiofs implementation has a limit of 36 characters for the tag
			if len(disk) > 36 {
				disk = disk[:36]
			}
			// TODO: Check read only option
			addDisks = append(addDisks, diskOptions{
				name:     disk,
				source:   m.Source,
				readOnly: false,
				vmdk:     false,
			})
			am = append(am, &types.Mount{
				Type:    "ext4",
				Source:  fmt.Sprintf("/dev/vd%c", disks),
				Target:  m.Target,
				Options: filterOptions(m.Options),
			})
			disks++
		case "overlay", "format/overlay", "format/mkdir/overlay":
			var (
				wdi = -1
				udi = -1
			)
			for i, opt := range m.Options {
				if strings.HasPrefix(opt, "upperdir=") {
					udi = i
				} else if strings.HasPrefix(opt, "workdir=") {
					wdi = i
				}
				// TODO: Handle virtio for lowers?
			}
			if wdi > -1 && udi > -1 {
				//
				// If any upperdir or workdir isn't transformed, they both
				// should fall back to virtiofs passthroughfs.  But...
				//
				if !strings.Contains(m.Options[wdi], "{{") ||
					!strings.Contains(m.Options[udi], "{{") {
					// Having the upper as virtiofs may return invalid argument, avoid
					// transforming and attempt to perform the mounts on the host if
					// supported.
					return nil, fmt.Errorf("cannot use virtiofs for upper dir in overlay: %w", errdefs.ErrNotImplemented)
				}
			} else {
				log.G(ctx).WithField("options", m.Options).Warnf("overlayfs missing workdir or upperdir")
			}

			am = append(am, m)
		default:
			am = append(am, m)
		}
	}

	if len(addDisks) > 10 {
		return nil, fmt.Errorf("exceeded maximum virtio disk count: %d > 10: %w", len(addDisks), errdefs.ErrNotImplemented)
	}

	for _, do := range addDisks {
		var opts []vm.MountOpt
		if do.readOnly {
			opts = append(opts, vm.WithReadOnly())
		}
		if do.vmdk {
			opts = append(opts, vm.WithVmdk())
		}
		if err := vmi.AddDisk(ctx, do.name, do.source, opts...); err != nil {
			return nil, err
		}
	}

	return am, err
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
