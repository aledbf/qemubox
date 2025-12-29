// Package transform provides OCI bundle transformations for VM compatibility.
// It handles modifications to OCI bundles required for running in VMs.
package transform

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/log"
	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/aledbf/qemubox/containerd/internal/shim/bundle"
)

// TransformFunc is a function that transforms an OCI bundle.
type TransformFunc func(ctx context.Context, b *bundle.Bundle) error

// TransformBindMounts transforms bind mounts in the OCI bundle.
// It converts bind mounts to extra files that can be passed to the VM.
func TransformBindMounts(ctx context.Context, b *bundle.Bundle) error {
	for i, m := range b.Spec.Mounts {
		if m.Type == "bind" {
			filename := filepath.Base(m.Source)
			// Check that the bind is from a path with the bundle id
			if filepath.Base(filepath.Dir(m.Source)) != filepath.Base(b.Path) {
				log.G(ctx).WithFields(log.Fields{
					"source": m.Source,
					"name":   filename,
				}).Debug("ignoring bind mount")
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

// DisableNetworkNamespace removes the network namespace from the OCI spec.
// This allows containers to share the VM's network namespace.
func DisableNetworkNamespace(ctx context.Context, b *bundle.Bundle) error {
	if b.Spec.Linux == nil {
		return nil
	}

	var namespaces []specs.LinuxNamespace
	for _, ns := range b.Spec.Linux.Namespaces {
		if ns.Type != specs.NetworkNamespace {
			namespaces = append(namespaces, ns)
		}
	}
	b.Spec.Linux.Namespaces = namespaces

	return nil
}

// LoadForCreate loads and transforms an OCI bundle for container creation.
// It applies all necessary transformations for VM compatibility.
func LoadForCreate(ctx context.Context, bundlePath string) (*bundle.Bundle, error) {
	return bundle.Load(ctx, bundlePath, TransformBindMounts, DisableNetworkNamespace)
}
