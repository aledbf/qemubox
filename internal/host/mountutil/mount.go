//go:build linux

// Package mountutil performs local mounts on Linux. This package should likely
// be replaced with functions in the containerd mount code.
package mountutil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	types "github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/log"
)

// All mounts all the provided mounts to the provided rootfs, handling
// "format/" and "mkdir/" mount type prefixes for template substitution
// and directory creation.
// It returns an optional cleanup function that should be called on container
// delete to unmount any mounted filesystems.
//
//nolint:gocognit // The mount pipeline is kept inline to match containerd semantics.
func All(ctx context.Context, rootfs, mdir string, mounts []*types.Mount) (cleanup func(context.Context) error, retErr error) {
	if len(mounts) == 0 {
		return nil, nil
	}

	log.G(ctx).WithField("mounts", mounts).Info("mounting rootfs components")
	active := []mount.ActiveMount{}

	// Note: Use mount manager interface; mount temps to directory.
	for i, m := range mounts {
		var target string
		if i < len(mounts)-1 {
			target = filepath.Join(mdir, fmt.Sprintf("%d", i))
			if err := os.MkdirAll(target, 0750); err != nil {
				return nil, err
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
						return nil, fmt.Errorf("formatting mount option %q: %w", o, err)
					}
					m.Options[i] = s
				}
			}
			if format := formatString(m.Source); format != nil {
				s, err := format(active)
				if err != nil {
					return nil, fmt.Errorf("formatting mount source %q: %w", m.Source, err)
				}
				m.Source = s
			}
			if format := formatString(m.Target); format != nil {
				s, err := format(active)
				if err != nil {
					return nil, fmt.Errorf("formatting mount target %q: %w", m.Target, err)
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
						return nil, fmt.Errorf("unknown mkdir mount option %q", o)
					}
					part := strings.SplitN(o[len(prefix):], ":", 4)
					var dir string
					var mode os.FileMode = 0755
					uid, gid := -1, -1

					switch len(part) {
					case 4:
						// Format: path:mode:uid:gid
						var err error
						gid, err = strconv.Atoi(part[3])
						if err != nil {
							return nil, fmt.Errorf("invalid gid %q in mkdir option: %w", part[3], err)
						}
						fallthrough
					case 3:
						// Format: path:mode:uid
						var err error
						uid, err = strconv.Atoi(part[2])
						if err != nil {
							return nil, fmt.Errorf("invalid uid %q in mkdir option: %w", part[2], err)
						}
						fallthrough
					case 2:
						// Format: path:mode
						m, err := strconv.ParseUint(part[1], 8, 32)
						if err != nil {
							return nil, fmt.Errorf("invalid mode %q in mkdir option: %w", part[1], err)
						}
						mode = os.FileMode(m)
						fallthrough
					case 1:
						// Format: path
						dir = part[0]
						if !strings.HasPrefix(dir, mdir) {
							return nil, fmt.Errorf("mkdir mount source %q must be under %q", dir, mdir)
						}
						if err := os.MkdirAll(dir, mode); err != nil {
							return nil, err
						}
						// Set ownership if uid/gid were specified
						if uid != -1 || gid != -1 {
							if err := os.Chown(dir, uid, gid); err != nil {
								return nil, fmt.Errorf("failed to chown %q to %d:%d: %w", dir, uid, gid, err)
							}
						}
					default:
						return nil, fmt.Errorf("invalid mkdir mount option %q", o)
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
			// Cleanup already mounted filesystems on error
			for j := len(active) - 1; j >= 0; j-- {
				if active[j].Type == "mkdir" {
					continue
				}
				if unmountErr := mount.UnmountAll(active[j].MountPoint, 0); unmountErr != nil {
					log.G(ctx).WithError(unmountErr).WithField("mountpoint", active[j].MountPoint).Warn("failed to cleanup mount")
				}
			}
			log.G(ctx).WithFields(log.Fields{
				"type":    am.Type,
				"source":  am.Source,
				"target":  target,
				"options": am.Options,
			}).WithError(err).Error("mount failed")
			return nil, err
		}
		log.G(ctx).WithFields(log.Fields{
			"type":    am.Type,
			"source":  am.Source,
			"target":  target,
			"options": am.Options,
		}).Info("mounted rootfs component")
		active = append(active, am)
	}

	// Return cleanup function that unmounts in reverse order
	cleanup = func(cleanCtx context.Context) error {
		var lastErr error
		for i := len(active) - 1; i >= 0; i-- {
			if active[i].Type == "mkdir" {
				continue
			}
			if err := mount.UnmountAll(active[i].MountPoint, 0); err != nil {
				log.G(cleanCtx).WithError(err).WithField("mountpoint", active[i].MountPoint).Warn("failed to cleanup mount")
				lastErr = err
			}
		}
		return lastErr
	}

	return cleanup, nil
}

// formatCheck is the marker for format strings that need substitution.
// Using explicit pattern matching instead of Go templates to prevent injection attacks.
const formatCheck = "{{"

// Pattern matchers for safe substitution (compiled once)
var (
	// Matches {{ source N }} where N is a number (whitespace flexible)
	sourcePattern = regexp.MustCompile(`\{\{\s*source\s+(\d+)\s*\}\}`)
	// Matches {{ target N }} where N is a number (whitespace flexible)
	targetPattern = regexp.MustCompile(`\{\{\s*target\s+(\d+)\s*\}\}`)
	// Matches {{ mount N }} where N is a number (whitespace flexible)
	mountPattern = regexp.MustCompile(`\{\{\s*mount\s+(\d+)\s*\}\}`)
	// Matches {{ overlay N M }} where N and M are numbers (whitespace flexible)
	overlayPattern = regexp.MustCompile(`\{\{\s*overlay\s+(\d+)\s+(\d+)\s*\}\}`)
)

// formatString returns a function that performs safe string substitution.
// Uses explicit pattern matching instead of Go templates to prevent injection attacks.
//
// Supported patterns:
//   - {{source N}} - replaced with active[N].Source
//   - {{target N}} - replaced with active[N].Target
//   - {{mount N}} - replaced with active[N].MountPoint
//   - {{overlay N M}} - replaced with colon-separated mount points from N to M
func formatString(s string) func([]mount.ActiveMount) (string, error) {
	if !strings.Contains(s, formatCheck) {
		return nil
	}

	return func(a []mount.ActiveMount) (string, error) {
		result := s
		var parseErr error

		// Helper to get index and validate bounds
		getIndex := func(indexStr string) (int, error) {
			i, err := strconv.Atoi(indexStr)
			if err != nil {
				return 0, fmt.Errorf("invalid index %q: %w", indexStr, err)
			}
			if i < 0 || i >= len(a) {
				return 0, fmt.Errorf("index out of bounds: %d, has %d active mounts", i, len(a))
			}
			return i, nil
		}

		// Replace {{source N}}
		result = sourcePattern.ReplaceAllStringFunc(result, func(match string) string {
			if parseErr != nil {
				return match
			}
			matches := sourcePattern.FindStringSubmatch(match)
			if len(matches) != 2 {
				parseErr = fmt.Errorf("invalid source pattern: %s", match)
				return match
			}
			idx, err := getIndex(matches[1])
			if err != nil {
				parseErr = err
				return match
			}
			return a[idx].Source
		})

		// Replace {{target N}}
		result = targetPattern.ReplaceAllStringFunc(result, func(match string) string {
			if parseErr != nil {
				return match
			}
			matches := targetPattern.FindStringSubmatch(match)
			if len(matches) != 2 {
				parseErr = fmt.Errorf("invalid target pattern: %s", match)
				return match
			}
			idx, err := getIndex(matches[1])
			if err != nil {
				parseErr = err
				return match
			}
			return a[idx].Target
		})

		// Replace {{mount N}}
		result = mountPattern.ReplaceAllStringFunc(result, func(match string) string {
			if parseErr != nil {
				return match
			}
			matches := mountPattern.FindStringSubmatch(match)
			if len(matches) != 2 {
				parseErr = fmt.Errorf("invalid mount pattern: %s", match)
				return match
			}
			idx, err := getIndex(matches[1])
			if err != nil {
				parseErr = err
				return match
			}
			return a[idx].MountPoint
		})

		// Replace {{overlay N M}}
		result = overlayPattern.ReplaceAllStringFunc(result, func(match string) string {
			if parseErr != nil {
				return match
			}
			matches := overlayPattern.FindStringSubmatch(match)
			if len(matches) != 3 {
				parseErr = fmt.Errorf("invalid overlay pattern: %s", match)
				return match
			}
			start, err := strconv.Atoi(matches[1])
			if err != nil {
				parseErr = fmt.Errorf("invalid start index in overlay: %w", err)
				return match
			}
			end, err := strconv.Atoi(matches[2])
			if err != nil {
				parseErr = fmt.Errorf("invalid end index in overlay: %w", err)
				return match
			}

			var dirs []string
			if start > end {
				if start >= len(a) || end < 0 {
					parseErr = fmt.Errorf("invalid range: %d-%d, has %d active mounts", start, end, len(a))
					return match
				}
				for i := start; i >= end; i-- {
					dirs = append(dirs, a[i].MountPoint)
				}
			} else {
				if start < 0 || end >= len(a) {
					parseErr = fmt.Errorf("invalid range: %d-%d, has %d active mounts", start, end, len(a))
					return match
				}
				for i := start; i <= end; i++ {
					dirs = append(dirs, a[i].MountPoint)
				}
			}
			return strings.Join(dirs, ":")
		})

		if parseErr != nil {
			return "", parseErr
		}

		// Check for any remaining unprocessed patterns (indicates unsupported syntax)
		if strings.Contains(result, "{{") {
			return "", fmt.Errorf("unsupported format pattern in %q", s)
		}

		return result, nil
	}
}
