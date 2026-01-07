//go:build linux

// Package transform provides OCI bundle transformations for VM compatibility.
package transform

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/log"
	"github.com/opencontainers/runc/libcontainer/capabilities"
	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/aledbf/qemubox/containerd/internal/shim/bundle"
)

// TransformBindMounts converts bind mounts to extra files for the VM.
func TransformBindMounts(ctx context.Context, b *bundle.Bundle) error {
	for i, m := range b.Spec.Mounts {
		if m.Type == "bind" {
			filename := filepath.Base(m.Source)
			if filepath.Base(filepath.Dir(m.Source)) != filepath.Base(b.Path) {
				log.G(ctx).WithField("source", m.Source).Debug("ignoring bind mount")
				continue
			}

			buf, err := os.ReadFile(m.Source)
			if err != nil {
				return fmt.Errorf("failed to read mount file %q: %w", filename, err)
			}
			b.Spec.Mounts[i].Source = filename
			if err := b.AddExtraFile(filename, buf); err != nil {
				return fmt.Errorf("failed to add extra file %q: %w", filename, err)
			}
		}
	}
	return nil
}

// AdaptForVM adapts the OCI spec for running inside a VM.
// The VM provides isolation, so we:
// - Remove network/cgroup namespaces (container uses VM's)
// - Ensure cgroup2 mount exists
// - Grant full capabilities (VM is the security boundary)
func AdaptForVM(ctx context.Context, b *bundle.Bundle) error {
	// Remove network and cgroup namespaces
	if b.Spec.Linux != nil {
		var namespaces []specs.LinuxNamespace
		for _, ns := range b.Spec.Linux.Namespaces {
			if ns.Type != specs.NetworkNamespace && ns.Type != specs.CgroupNamespace {
				namespaces = append(namespaces, ns)
			}
		}
		b.Spec.Linux.Namespaces = namespaces
	}

	// Ensure cgroup2 mount exists
	hasCgroup := false
	for i, m := range b.Spec.Mounts {
		if m.Destination == "/sys/fs/cgroup" {
			b.Spec.Mounts[i].Type = "cgroup2"
			b.Spec.Mounts[i].Source = "cgroup2"
			b.Spec.Mounts[i].Options = ensureRW(m.Options)
			hasCgroup = true
			break
		}
	}
	if !hasCgroup {
		b.Spec.Mounts = append(b.Spec.Mounts, specs.Mount{
			Destination: "/sys/fs/cgroup",
			Type:        "cgroup2",
			Source:      "cgroup2",
			Options:     []string{"nosuid", "noexec", "nodev", "rw"},
		})
	}

	// Grant full capabilities
	if b.Spec.Process != nil {
		b.Spec.Process.Capabilities = &specs.LinuxCapabilities{
			Bounding:    capabilities.KnownCapabilities(),
			Effective:   capabilities.KnownCapabilities(),
			Permitted:   capabilities.KnownCapabilities(),
			Inheritable: capabilities.KnownCapabilities(),
			Ambient:     capabilities.KnownCapabilities(),
		}
	}

	// Clear readonly/masked paths - VM provides isolation, not OCI restrictions
	if b.Spec.Linux != nil {
		b.Spec.Linux.ReadonlyPaths = nil
		b.Spec.Linux.MaskedPaths = nil
	}

	return nil
}

func ensureRW(opts []string) []string {
	result := make([]string, 0, len(opts))
	hasRW := false
	for _, opt := range opts {
		if opt == "ro" {
			result = append(result, "rw")
			hasRW = true
		} else {
			if opt == "rw" {
				hasRW = true
			}
			result = append(result, opt)
		}
	}
	if !hasRW {
		result = append(result, "rw")
	}
	return result
}

// LoadForCreate loads and transforms an OCI bundle for container creation.
func LoadForCreate(ctx context.Context, bundlePath string) (*bundle.Bundle, error) {
	return bundle.Load(ctx, bundlePath,
		TransformBindMounts,
		AdaptForVM,
	)
}
