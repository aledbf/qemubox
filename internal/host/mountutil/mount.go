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

// mkdirSpec holds the parsed mkdir specification from mount options.
type mkdirSpec struct {
	Path string
	Mode os.FileMode
	UID  int
	GID  int
}

// parseMkdirOption parses a single X-containerd.mkdir.path option.
// Format: X-containerd.mkdir.path=path[:mode[:uid[:gid]]]
func parseMkdirOption(opt, baseDir string) (*mkdirSpec, error) {
	const prefix = "X-containerd.mkdir.path="
	if !strings.HasPrefix(opt, prefix) {
		return nil, fmt.Errorf("unknown mkdir mount option %q", opt)
	}

	parts := strings.SplitN(opt[len(prefix):], ":", 4)
	spec := &mkdirSpec{
		Mode: 0755,
		UID:  -1,
		GID:  -1,
	}

	switch len(parts) {
	case 4:
		gid, err := strconv.Atoi(parts[3])
		if err != nil {
			return nil, fmt.Errorf("invalid gid %q in mkdir option: %w", parts[3], err)
		}
		spec.GID = gid
		fallthrough
	case 3:
		uid, err := strconv.Atoi(parts[2])
		if err != nil {
			return nil, fmt.Errorf("invalid uid %q in mkdir option: %w", parts[2], err)
		}
		spec.UID = uid
		fallthrough
	case 2:
		mode, err := strconv.ParseUint(parts[1], 8, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid mode %q in mkdir option: %w", parts[1], err)
		}
		spec.Mode = os.FileMode(mode)
		fallthrough
	case 1:
		spec.Path = parts[0]
		if !strings.HasPrefix(spec.Path, baseDir) {
			return nil, fmt.Errorf("mkdir path %q must be under %q", spec.Path, baseDir)
		}
	default:
		return nil, fmt.Errorf("invalid mkdir mount option %q", opt)
	}

	return spec, nil
}

// processMkdirOptions processes mkdir options and returns remaining options.
func processMkdirOptions(options []string, baseDir string) ([]string, []*mkdirSpec, error) {
	var remaining []string
	var specs []*mkdirSpec

	for _, opt := range options {
		if strings.HasPrefix(opt, "X-containerd.mkdir.") {
			spec, err := parseMkdirOption(opt, baseDir)
			if err != nil {
				return nil, nil, err
			}
			specs = append(specs, spec)
		} else {
			remaining = append(remaining, opt)
		}
	}

	return remaining, specs, nil
}

// applyMkdirSpecs creates directories from specs.
func applyMkdirSpecs(specs []*mkdirSpec) error {
	for _, spec := range specs {
		if err := os.MkdirAll(spec.Path, spec.Mode); err != nil {
			return err
		}
		if spec.UID != -1 || spec.GID != -1 {
			if err := os.Chown(spec.Path, spec.UID, spec.GID); err != nil {
				return fmt.Errorf("failed to chown %q to %d:%d: %w", spec.Path, spec.UID, spec.GID, err)
			}
		}
	}
	return nil
}

// applyFormatSubstitution applies format substitution to a mount.
func applyFormatSubstitution(m *types.Mount, active []mount.ActiveMount) error {
	for i, opt := range m.Options {
		fn := formatString(opt)
		if fn == nil {
			continue
		}
		s, err := fn(active)
		if err != nil {
			return fmt.Errorf("formatting mount option %q: %w", opt, err)
		}
		m.Options[i] = s
	}

	if fn := formatString(m.Source); fn != nil {
		s, err := fn(active)
		if err != nil {
			return fmt.Errorf("formatting mount source %q: %w", m.Source, err)
		}
		m.Source = s
	}

	if fn := formatString(m.Target); fn != nil {
		s, err := fn(active)
		if err != nil {
			return fmt.Errorf("formatting mount target %q: %w", m.Target, err)
		}
		m.Target = s
	}

	return nil
}

// cleanupMounts unmounts active mounts in reverse order.
func cleanupMounts(ctx context.Context, active []mount.ActiveMount) error {
	var lastErr error
	for i := len(active) - 1; i >= 0; i-- {
		if active[i].Type == "mkdir" {
			continue
		}
		if err := mount.UnmountAll(active[i].MountPoint, 0); err != nil {
			log.G(ctx).WithError(err).WithField("mountpoint", active[i].MountPoint).Warn("failed to cleanup mount")
			lastErr = err
		}
	}
	return lastErr
}

// All mounts all the provided mounts to the provided rootfs, handling
// "format/" and "mkdir/" mount type prefixes for template substitution
// and directory creation.
// It returns an optional cleanup function that should be called on container
// delete to unmount any mounted filesystems.
func All(ctx context.Context, rootfs, mdir string, mounts []*types.Mount) (cleanup func(context.Context) error, retErr error) {
	if len(mounts) == 0 {
		return nil, nil
	}

	// The full mount spec is ~480 bytes and every byte of it is a PIO exit on
	// the guest's 8250 (measured: one kvm_pio per byte written to 0x3f8, plus
	// the driver's LSR poll before each). The count is what a normal run needs;
	// the spec itself is a debugging detail and moves behind Debug.
	log.G(ctx).WithField("count", len(mounts)).Info("mounting rootfs components")
	log.G(ctx).WithField("mounts", mounts).Debug("rootfs mount spec")
	var active []mount.ActiveMount

	for i, m := range mounts {
		// Determine target directory
		target := rootfs
		if i < len(mounts)-1 {
			target = filepath.Join(mdir, fmt.Sprintf("%d", i))
			if err := os.MkdirAll(target, 0750); err != nil {
				if cleanupErr := cleanupMounts(ctx, active); cleanupErr != nil {
					log.G(ctx).WithError(cleanupErr).Warn("cleanup failed after MkdirAll error")
				}
				return nil, err
			}
		}

		// Handle format/ prefix
		if t, ok := strings.CutPrefix(m.Type, "format/"); ok {
			m.Type = t
			if err := applyFormatSubstitution(m, active); err != nil {
				if cleanupErr := cleanupMounts(ctx, active); cleanupErr != nil {
					log.G(ctx).WithError(cleanupErr).Warn("cleanup failed after format substitution error")
				}
				return nil, err
			}
		}

		// Handle mkdir/ prefix
		if t, ok := strings.CutPrefix(m.Type, "mkdir/"); ok {
			m.Type = t
			remaining, specs, err := processMkdirOptions(m.Options, mdir)
			if err != nil {
				if cleanupErr := cleanupMounts(ctx, active); cleanupErr != nil {
					log.G(ctx).WithError(cleanupErr).Warn("cleanup failed after mkdir options error")
				}
				return nil, err
			}
			if err := applyMkdirSpecs(specs); err != nil {
				if cleanupErr := cleanupMounts(ctx, active); cleanupErr != nil {
					log.G(ctx).WithError(cleanupErr).Warn("cleanup failed after mkdir specs error")
				}
				return nil, err
			}
			m.Options = remaining
		}

		// Resolve serial-identified block sources ("serial:<id>") to their real
		// /dev/<dev> path. No-op for ordinary sources (host fallback path).
		if resolved, ok, err := resolveBlockSerialSource(ctx, m.Source); err != nil {
			if cleanupErr := cleanupMounts(ctx, active); cleanupErr != nil {
				log.G(ctx).WithError(cleanupErr).Warn("cleanup failed after serial resolution error")
			}
			return nil, err
		} else if ok {
			m.Source = resolved
		}

		// Perform the mount
		now := time.Now()
		am := mount.ActiveMount{
			Mount: mount.Mount{
				Type:    m.Type,
				Source:  m.Source,
				Target:  m.Target,
				Options: m.Options,
			},
			MountedAt:  &now,
			MountPoint: target,
		}

		if err := am.Mount.Mount(target); err != nil {
			if cleanupErr := cleanupMounts(ctx, active); cleanupErr != nil {
				log.G(ctx).WithError(cleanupErr).Warn("cleanup failed after mount error")
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
		}).Debug("mounted rootfs component")
		active = append(active, am)
	}

	cleanup = func(cleanCtx context.Context) error {
		return cleanupMounts(cleanCtx, active)
	}

	return cleanup, nil
}

// formatCheck is the marker for format strings that need substitution.
const formatCheck = "{{"

// Pattern matchers for safe substitution (compiled once)
var (
	sourcePattern  = regexp.MustCompile(`\{\{\s*source\s+(\d+)\s*\}\}`)
	targetPattern  = regexp.MustCompile(`\{\{\s*target\s+(\d+)\s*\}\}`)
	mountPattern   = regexp.MustCompile(`\{\{\s*mount\s+(\d+)\s*\}\}`)
	overlayPattern = regexp.MustCompile(`\{\{\s*overlay\s+(\d+)\s+(\d+)\s*\}\}`)
)

// parseIndex validates and returns an index from a string.
func parseIndex(indexStr string, maxLen int) (int, error) {
	i, err := strconv.Atoi(indexStr)
	if err != nil {
		return 0, fmt.Errorf("invalid index %q: %w", indexStr, err)
	}
	if i < 0 || i >= maxLen {
		return 0, fmt.Errorf("index out of bounds: %d, has %d active mounts", i, maxLen)
	}
	return i, nil
}

// replaceSimplePattern replaces a single-index pattern using the provided getter.
func replaceSimplePattern(s string, pattern *regexp.Regexp, mounts []mount.ActiveMount, getter func(int) string) (string, error) {
	var capturedErr error
	result := pattern.ReplaceAllStringFunc(s, func(match string) string {
		if capturedErr != nil {
			return match
		}
		matches := pattern.FindStringSubmatch(match)
		if len(matches) != 2 {
			capturedErr = fmt.Errorf("invalid pattern: %s", match)
			return match
		}
		idx, err := parseIndex(matches[1], len(mounts))
		if err != nil {
			capturedErr = err
			return match
		}
		return getter(idx)
	})
	return result, capturedErr
}

// buildOverlayDirs builds the colon-separated directory list for overlay.
func buildOverlayDirs(start, end int, mounts []mount.ActiveMount) (string, error) {
	var dirs []string
	if start > end {
		if start >= len(mounts) || end < 0 {
			return "", fmt.Errorf("invalid range: %d-%d, has %d active mounts", start, end, len(mounts))
		}
		for i := start; i >= end; i-- {
			dirs = append(dirs, mounts[i].MountPoint)
		}
	} else {
		if start < 0 || end >= len(mounts) {
			return "", fmt.Errorf("invalid range: %d-%d, has %d active mounts", start, end, len(mounts))
		}
		for i := start; i <= end; i++ {
			dirs = append(dirs, mounts[i].MountPoint)
		}
	}
	return strings.Join(dirs, ":"), nil
}

// replaceOverlayPattern handles the overlay N M pattern replacement.
func replaceOverlayPattern(s string, mounts []mount.ActiveMount) (string, error) {
	var capturedErr error
	result := overlayPattern.ReplaceAllStringFunc(s, func(match string) string {
		if capturedErr != nil {
			return match
		}
		matches := overlayPattern.FindStringSubmatch(match)
		if len(matches) != 3 {
			capturedErr = fmt.Errorf("invalid overlay pattern: %s", match)
			return match
		}
		start, err := strconv.Atoi(matches[1])
		if err != nil {
			capturedErr = fmt.Errorf("invalid start index in overlay: %w", err)
			return match
		}
		end, err := strconv.Atoi(matches[2])
		if err != nil {
			capturedErr = fmt.Errorf("invalid end index in overlay: %w", err)
			return match
		}
		dirs, err := buildOverlayDirs(start, end, mounts)
		if err != nil {
			capturedErr = err
			return match
		}
		return dirs
	})
	return result, capturedErr
}

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
		var err error

		result, err = replaceSimplePattern(result, sourcePattern, a, func(i int) string { return a[i].Source })
		if err != nil {
			return "", fmt.Errorf("source pattern: %w", err)
		}

		result, err = replaceSimplePattern(result, targetPattern, a, func(i int) string { return a[i].Target })
		if err != nil {
			return "", fmt.Errorf("target pattern: %w", err)
		}

		result, err = replaceSimplePattern(result, mountPattern, a, func(i int) string { return a[i].MountPoint })
		if err != nil {
			return "", fmt.Errorf("mount pattern: %w", err)
		}

		result, err = replaceOverlayPattern(result, a)
		if err != nil {
			return "", fmt.Errorf("overlay pattern: %w", err)
		}

		if strings.Contains(result, "{{") {
			return "", fmt.Errorf("unsupported format pattern in %q", s)
		}

		return result, nil
	}
}
