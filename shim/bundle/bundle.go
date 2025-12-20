// Package bundle loads and transforms OCI bundles for the shim.
package bundle

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"github.com/containerd/errdefs"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Bundle represents an OCI bundle with extra files for the VM.
type Bundle struct {
	Path   string // Path is the bundle path.
	Spec   specs.Spec
	Rootfs string // Rootfs is the absolute path to the root filesystem.

	// extraFiles are files that are not part of the OCI bundle but are needed
	// to setup containers in the VM. Keep it unexported to force consumers to
	// call Files to get all the files, including the updated OCI spec.
	extraFiles map[string][]byte
}

// Transformer mutates a bundle before it is sent to the VM.
type Transformer func(ctx context.Context, b *Bundle) error

// Load loads an OCI bundle from the given path and apply a series of transformers
// to turn the host-side bundle into a VM-side bundle.
func Load(ctx context.Context, path string, transformers ...Transformer) (*Bundle, error) {
	specBytes, err := os.ReadFile(filepath.Join(path, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("failed to read bundle config: %w", err)
	}

	var spec specs.Spec
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		return nil, fmt.Errorf("failed to parse bundle spec: %w", err)
	}

	b := &Bundle{
		Path:       path,
		Spec:       spec,
		extraFiles: make(map[string][]byte),
	}

	if err := resolveRootfsPath(ctx, b); err != nil {
		return nil, fmt.Errorf("failed to resolve rootfs path: %w", err)
	}

	for _, t := range transformers {
		if err := t(ctx, b); err != nil {
			return nil, fmt.Errorf("transformer failed: %w", err)
		}
	}

	return b, nil
}

// AddExtraFile adds an extra file to the bundle that is not part of the OCI spec.
func (b *Bundle) AddExtraFile(name string, data []byte) error {
	if name == "" {
		return fmt.Errorf("file name cannot be empty")
	}
	if name == "config.json" {
		return fmt.Errorf("cannot override config.json")
	}
	if filepath.Base(name) != name {
		return fmt.Errorf("file name %q must not contain path separators", name)
	}
	b.extraFiles[name] = data
	return nil
}

// Files returns all the bundle files that must be setup inside the VM.
// Note: The returned map is a shallow copy; byte slices are shared with the bundle.
func (b *Bundle) Files() (map[string][]byte, error) {
	files := maps.Clone(b.extraFiles)

	specBytes, err := json.Marshal(b.Spec)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal spec: %w", err)
	}
	files["config.json"] = specBytes

	return files, nil
}

func resolveRootfsPath(_ context.Context, b *Bundle) error {
	if b.Spec.Root == nil {
		return fmt.Errorf("%w: root path not specified", errdefs.ErrInvalidArgument)
	}

	if filepath.IsAbs(b.Spec.Root.Path) {
		b.Rootfs = b.Spec.Root.Path
	} else {
		b.Rootfs = filepath.Join(b.Path, b.Spec.Root.Path)
	}
	b.Spec.Root.Path = "rootfs"

	return nil
}
