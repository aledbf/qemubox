//go:build linux

// mountutil performs local mounts on Linux. This package should likely
// be replaced with functions in the containerd mount code.
package mountutil

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	types "github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/log"
)

func All(ctx context.Context, rootfs, mdir string, mounts []*types.Mount) (retErr error) {
	log.G(ctx).WithField("mounts", mounts).Info("mounting rootfs components")
	active := []mount.ActiveMount{}

	// TODO: Use mount manager interface, mount temps to directory
	for i, m := range mounts {
		var target string
		if i < len(mounts)-1 {
			target = filepath.Join(mdir, fmt.Sprintf("%d", i))
			if err := os.MkdirAll(target, 0711); err != nil {
				return err
			}
		} else {
			target = rootfs
		}
		if t, ok := strings.CutPrefix(m.Type, "format/"); ok {
			m.Type = t
			for i, o := range m.Options {
				format := formatString(o)
				if format != nil {
					s, err := format(active)
					if err != nil {
						return fmt.Errorf("formatting mount option %q: %w", o, err)
					}
					m.Options[i] = s
				}
			}
			if format := formatString(m.Source); format != nil {
				s, err := format(active)
				if err != nil {
					return fmt.Errorf("formatting mount source %q: %w", m.Source, err)
				}
				m.Source = s
			}
			if format := formatString(m.Target); format != nil {
				s, err := format(active)
				if err != nil {
					return fmt.Errorf("formatting mount target %q: %w", m.Target, err)
				}
				m.Target = s
			}
		}
		if t, ok := strings.CutPrefix(m.Type, "mkdir/"); ok {
			m.Type = t
			var options []string
			for _, o := range m.Options {
				if strings.HasPrefix(o, "X-containerd.mkdir.") {
					prefix := "X-containerd.mkdir.path="
					if !strings.HasPrefix(o, prefix) {
						return fmt.Errorf("unknown mkdir mount option %q", o)
					}
					part := strings.SplitN(o[len(prefix):], ":", 4)
					var dir string
					var mode os.FileMode = 0755
					var uid, gid int = -1, -1

					switch len(part) {
					case 4:
						// Format: path:mode:uid:gid
						var err error
						gid, err = strconv.Atoi(part[3])
						if err != nil {
							return fmt.Errorf("invalid gid %q in mkdir option: %w", part[3], err)
						}
						fallthrough
					case 3:
						// Format: path:mode:uid
						var err error
						uid, err = strconv.Atoi(part[2])
						if err != nil {
							return fmt.Errorf("invalid uid %q in mkdir option: %w", part[2], err)
						}
						fallthrough
					case 2:
						// Format: path:mode
						m, err := strconv.ParseUint(part[1], 8, 32)
						if err != nil {
							return fmt.Errorf("invalid mode %q in mkdir option: %w", part[1], err)
						}
						mode = os.FileMode(m)
						fallthrough
					case 1:
						// Format: path
						dir = part[0]
						if !strings.HasPrefix(dir, mdir) {
							return fmt.Errorf("mkdir mount source %q must be under %q", dir, mdir)
						}
						if err := os.MkdirAll(dir, mode); err != nil {
							return err
						}
						// Set ownership if uid/gid were specified
						if uid != -1 || gid != -1 {
							if err := os.Chown(dir, uid, gid); err != nil {
								return fmt.Errorf("failed to chown %q to %d:%d: %w", dir, uid, gid, err)
							}
						}
					default:
						return fmt.Errorf("invalid mkdir mount option %q", o)
					}
				} else {
					options = append(options, o)
				}
			}
			m.Options = options

		}
		t := time.Now()
		am := mount.ActiveMount{
			Mount: mount.Mount{
				Type:    m.Type,
				Source:  m.Source,
				Target:  m.Target,
				Options: m.Options,
			},
			MountedAt:  &t,
			MountPoint: target,
		}
		if err := am.Mount.Mount(target); err != nil {
			log.G(ctx).WithFields(log.Fields{
				"type":    am.Type,
				"source":  am.Source,
				"target":  target,
				"options": am.Options,
			}).WithError(err).Error("mount failed")
			return err
		}
		log.G(ctx).WithFields(log.Fields{
			"type":    am.Type,
			"source":  am.Source,
			"target":  target,
			"options": am.Options,
		}).Info("mounted")
		active = append(active, am)

	}
	defer func() {
		if retErr != nil {
			for i := len(active) - 1; i >= 0; i-- {
				// TODO: delegate custom types to handlers
				if active[i].Type == "mkdir" {
					continue
				}
				if err := mount.UnmountAll(active[i].MountPoint, 0); err != nil {
					log.G(ctx).WithError(err).WithField("mountpoint", active[i].MountPoint).Warn("failed to cleanup mount")
				}
			}
		}
	}()

	return nil
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
