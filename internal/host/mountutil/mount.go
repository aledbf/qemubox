//go:build linux

// Package mountutil performs local mounts on Linux using containerd's mount manager.
package mountutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"text/template"
	"time"

	types "github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/mount/manager"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	bolt "go.etcd.io/bbolt"
)

const defaultNamespace = "default"

var activationCounter atomic.Uint64

// All mounts all the provided mounts to the provided rootfs, using containerd's
// mount manager to handle "format/" and "mkdir/" mount types.
// It returns an optional cleanup function that should be called on container
// delete to unmount and deactivate any managed mounts.
func All(ctx context.Context, rootfs, mdir string, mounts []*types.Mount) (cleanup func(context.Context) error, retErr error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(mdir, 0750); err != nil {
		return nil, err
	}

	ctx = ensureNamespace(ctx)

	// Preprocess mounts: handle format/ and mkdir/ prefixes
	processed, err := preprocessMounts(ctx, rootfs, mdir, mounts)
	if err != nil {
		return nil, err
	}

	mnts := mount.FromProto(processed)
	mgr, db, err := newManager(mdir)
	if err != nil {
		return nil, err
	}

	activationName := fmt.Sprintf("qemubox-%d-%d", time.Now().UnixNano(), activationCounter.Add(1))
	info, err := mgr.Activate(ctx, activationName, mnts)
	if err != nil {
		if errdefs.IsNotImplemented(err) {
			if err := db.Close(); err != nil {
				log.G(ctx).WithError(err).Warn("failed to close mount manager db")
			}
			if err := mount.All(mnts, rootfs); err != nil {
				_ = mount.UnmountMounts(mnts, rootfs, 0)
				return nil, err
			}
			return func(cleanCtx context.Context) error {
				return mount.UnmountMounts(mnts, rootfs, 0)
			}, nil
		}
		_ = db.Close()
		return nil, err
	}

	cleanup = func(cleanCtx context.Context) error {
		var errs []error
		if err := mount.UnmountMounts(info.System, rootfs, 0); err != nil {
			errs = append(errs, err)
		}
		if err := mgr.Deactivate(cleanCtx, activationName); err != nil {
			errs = append(errs, err)
		}
		if err := db.Close(); err != nil {
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	}

	if err := mount.All(info.System, rootfs); err != nil {
		_ = mount.UnmountMounts(info.System, rootfs, 0)
		_ = cleanup(context.WithoutCancel(ctx))
		return nil, err
	}

	return cleanup, nil
}

// preprocessMounts handles format/ and mkdir/ mount type prefixes,
// performing template substitution and directory creation as needed.
func preprocessMounts(ctx context.Context, rootfs, mdir string, mounts []*types.Mount) ([]*types.Mount, error) {
	log.G(ctx).WithField("mounts", mounts).Debugf("preprocessing mounts")

	active := []mount.ActiveMount{}
	result := make([]*types.Mount, len(mounts))

	for i, m := range mounts {
		// Clone the mount to avoid modifying the original
		processed := &types.Mount{
			Type:    m.Type,
			Source:  m.Source,
			Target:  m.Target,
			Options: append([]string{}, m.Options...),
		}

		// Determine the mount point for this mount
		var mountPoint string
		if i < len(mounts)-1 {
			mountPoint = filepath.Join(mdir, fmt.Sprintf("%d", i))
			if err := os.MkdirAll(mountPoint, 0711); err != nil {
				return nil, err
			}
		} else {
			mountPoint = rootfs
		}

		// Handle format/ prefix - template substitution
		if t, ok := strings.CutPrefix(processed.Type, "format/"); ok {
			processed.Type = t
			for j, o := range processed.Options {
				format := formatString(o)
				if format != nil {
					s, err := format(active)
					if err != nil {
						return nil, fmt.Errorf("formatting mount option %q: %w", o, err)
					}
					processed.Options[j] = s
				}
			}
			if format := formatString(processed.Source); format != nil {
				s, err := format(active)
				if err != nil {
					return nil, fmt.Errorf("formatting mount source %q: %w", processed.Source, err)
				}
				processed.Source = s
			}
			if format := formatString(processed.Target); format != nil {
				s, err := format(active)
				if err != nil {
					return nil, fmt.Errorf("formatting mount target %q: %w", processed.Target, err)
				}
				processed.Target = s
			}
		}

		// Handle mkdir/ prefix - create directories
		if t, ok := strings.CutPrefix(processed.Type, "mkdir/"); ok {
			processed.Type = t
			var options []string
			for _, o := range processed.Options {
				if strings.HasPrefix(o, "X-containerd.mkdir.") {
					prefix := "X-containerd.mkdir.path="
					if !strings.HasPrefix(o, prefix) {
						return nil, fmt.Errorf("unknown mkdir mount option %q", o)
					}
					part := strings.SplitN(o[len(prefix):], ":", 4)
					switch len(part) {
					case 4:
						// TODO: Support setting uid/gid
						fallthrough
					case 3:
						fallthrough
					case 2:
						fallthrough
					case 1:
						dir := part[0]
						if !strings.HasPrefix(dir, mdir) {
							return nil, fmt.Errorf("mkdir mount source %q must be under %q", dir, mdir)
						}
						if err := os.MkdirAll(dir, 0755); err != nil {
							return nil, err
						}
					default:
						return nil, fmt.Errorf("invalid mkdir mount option %q", o)
					}
				} else {
					options = append(options, o)
				}
			}
			processed.Options = options
		}

		// Track as active mount for subsequent template references
		t := time.Now()
		active = append(active, mount.ActiveMount{
			Mount: mount.Mount{
				Type:    processed.Type,
				Source:  processed.Source,
				Target:  processed.Target,
				Options: processed.Options,
			},
			MountedAt:  &t,
			MountPoint: mountPoint,
		})

		result[i] = processed
	}

	return result, nil
}

const formatCheck = "{{"

func formatString(s string) func([]mount.ActiveMount) (string, error) {
	if !strings.Contains(s, formatCheck) {
		return nil
	}

	return func(a []mount.ActiveMount) (string, error) {
		fm := template.FuncMap{
			"source": func(i int) (string, error) {
				if i < 0 || i >= len(a) {
					return "", fmt.Errorf("index out of bounds: %d, has %d active mounts", i, len(a))
				}
				return a[i].Source, nil
			},
			"target": func(i int) (string, error) {
				if i < 0 || i >= len(a) {
					return "", fmt.Errorf("index out of bounds: %d, has %d active mounts", i, len(a))
				}
				return a[i].Target, nil
			},
			"mount": func(i int) (string, error) {
				if i < 0 || i >= len(a) {
					return "", fmt.Errorf("index out of bounds: %d, has %d active mounts", i, len(a))
				}
				return a[i].MountPoint, nil
			},
			"overlay": func(start, end int) (string, error) {
				var dirs []string
				if start > end {
					if start >= len(a) || end < 0 {
						return "", fmt.Errorf("invalid range: %d-%d, has %d active mounts", start, end, len(a))
					}
					for i := start; i >= end; i-- {
						dirs = append(dirs, a[i].MountPoint)
					}
				} else {
					if start < 0 || end >= len(a) {
						return "", fmt.Errorf("invalid range: %d-%d, has %d active mounts", start, end, len(a))
					}
					for i := start; i <= end; i++ {
						dirs = append(dirs, a[i].MountPoint)
					}
				}
				return strings.Join(dirs, ":"), nil
			},
		}
		t, err := template.New("").Funcs(fm).Parse(s)
		if err != nil {
			return "", err
		}

		buf := bytes.NewBuffer(nil)
		if err := t.Execute(buf, nil); err != nil {
			return "", err
		}
		return buf.String(), nil
	}
}

func newManager(mdir string) (mount.Manager, *bolt.DB, error) {
	dbPath := filepath.Join(mdir, "mounts.db")
	db, err := bolt.Open(dbPath, 0600, nil)
	if err != nil {
		return nil, nil, err
	}
	mgr, err := manager.NewManager(db, mdir)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return mgr, db, nil
}

func ensureNamespace(ctx context.Context) context.Context {
	if ns, ok := namespaces.Namespace(ctx); ok && ns != "" {
		return ctx
	}
	return namespaces.WithNamespace(ctx, defaultNamespace)
}
