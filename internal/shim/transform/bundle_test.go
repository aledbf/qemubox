package transform

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aledbf/qemubox/containerd/internal/shim/bundle"
)

// createTestBundle creates a minimal OCI bundle for testing
func createTestBundle(t *testing.T, bundlePath string) {
	t.Helper()

	// Create bundle directory
	require.NoError(t, os.MkdirAll(bundlePath, 0750))

	// Create minimal OCI spec
	spec := specs.Spec{
		Version: "1.0.0",
		Root: &specs.Root{
			Path: "rootfs",
		},
		Process: &specs.Process{
			Args: []string{"/bin/sh"},
		},
		Linux: &specs.Linux{
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace},
				{Type: specs.NetworkNamespace},
				{Type: specs.MountNamespace},
			},
		},
	}

	specBytes, err := json.Marshal(spec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bundlePath, "config.json"), specBytes, 0600))

	// Create rootfs directory
	require.NoError(t, os.MkdirAll(filepath.Join(bundlePath, "rootfs"), 0750))
}

func TestTransformBindMounts(t *testing.T) {
	ctx := context.Background()

	t.Run("transforms bind mount from bundle path", func(t *testing.T) {
		tmpDir := t.TempDir()
		bundlePath := filepath.Join(tmpDir, "test-container")
		createTestBundle(t, bundlePath)

		// Create a file to bind mount
		testFile := filepath.Join(bundlePath, "config.yaml")
		testContent := []byte("key: value\n")
		require.NoError(t, os.WriteFile(testFile, testContent, 0600))

		// Load bundle and add bind mount
		b, err := bundle.Load(ctx, bundlePath)
		require.NoError(t, err)

		b.Spec.Mounts = append(b.Spec.Mounts, specs.Mount{
			Destination: "/etc/config.yaml",
			Type:        "bind",
			Source:      testFile,
			Options:     []string{"rbind", "ro"},
		})

		// Apply transform
		err = TransformBindMounts(ctx, b)
		require.NoError(t, err)

		// Verify mount source was changed to filename only
		assert.Equal(t, "config.yaml", b.Spec.Mounts[len(b.Spec.Mounts)-1].Source)

		// Verify extra file was added
		files, err := b.Files()
		require.NoError(t, err)
		assert.Contains(t, files, "config.yaml")
		assert.Equal(t, testContent, files["config.yaml"])
	})

	t.Run("ignores bind mount from different path", func(t *testing.T) {
		tmpDir := t.TempDir()
		bundlePath := filepath.Join(tmpDir, "test-container")
		createTestBundle(t, bundlePath)

		// Create a file outside the bundle
		otherDir := filepath.Join(tmpDir, "other")
		require.NoError(t, os.MkdirAll(otherDir, 0750))
		testFile := filepath.Join(otherDir, "secret.txt")
		require.NoError(t, os.WriteFile(testFile, []byte("secret"), 0600))

		// Load bundle and add bind mount from different path
		b, err := bundle.Load(ctx, bundlePath)
		require.NoError(t, err)

		originalMount := specs.Mount{
			Destination: "/etc/secret.txt",
			Type:        "bind",
			Source:      testFile,
		}
		b.Spec.Mounts = append(b.Spec.Mounts, originalMount)

		// Apply transform
		err = TransformBindMounts(ctx, b)
		require.NoError(t, err)

		// Mount should remain unchanged
		assert.Equal(t, testFile, b.Spec.Mounts[len(b.Spec.Mounts)-1].Source)

		// No extra file should be added
		files, err := b.Files()
		require.NoError(t, err)
		assert.NotContains(t, files, "secret.txt")
	})

	t.Run("ignores non-bind mounts", func(t *testing.T) {
		tmpDir := t.TempDir()
		bundlePath := filepath.Join(tmpDir, "test-container")
		createTestBundle(t, bundlePath)

		b, err := bundle.Load(ctx, bundlePath)
		require.NoError(t, err)

		b.Spec.Mounts = append(b.Spec.Mounts, specs.Mount{
			Destination: "/proc",
			Type:        "proc",
			Source:      "proc",
		})

		initialMountCount := len(b.Spec.Mounts)

		err = TransformBindMounts(ctx, b)
		require.NoError(t, err)

		// Mount count should be unchanged
		assert.Len(t, b.Spec.Mounts, initialMountCount)
	})

	t.Run("handles file read error", func(t *testing.T) {
		tmpDir := t.TempDir()
		bundlePath := filepath.Join(tmpDir, "test-container")
		createTestBundle(t, bundlePath)

		b, err := bundle.Load(ctx, bundlePath)
		require.NoError(t, err)

		// Add bind mount to non-existent file within bundle path
		b.Spec.Mounts = append(b.Spec.Mounts, specs.Mount{
			Destination: "/etc/missing",
			Type:        "bind",
			Source:      filepath.Join(bundlePath, "nonexistent"),
		})

		err = TransformBindMounts(ctx, b)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to read mount file")
	})
}

func TestDisableNetworkNamespace(t *testing.T) {
	ctx := context.Background()

	t.Run("removes network namespace", func(t *testing.T) {
		tmpDir := t.TempDir()
		bundlePath := filepath.Join(tmpDir, "test-container")
		createTestBundle(t, bundlePath)

		b, err := bundle.Load(ctx, bundlePath)
		require.NoError(t, err)

		// Verify initial state has network namespace
		hasNetwork := false
		for _, ns := range b.Spec.Linux.Namespaces {
			if ns.Type == specs.NetworkNamespace {
				hasNetwork = true
				break
			}
		}
		require.True(t, hasNetwork, "initial spec should have network namespace")

		// Apply transform
		err = DisableNetworkNamespace(ctx, b)
		require.NoError(t, err)

		// Verify network namespace is removed
		for _, ns := range b.Spec.Linux.Namespaces {
			assert.NotEqual(t, specs.NetworkNamespace, ns.Type, "network namespace should be removed")
		}

		// Other namespaces should remain
		hasPID := false
		hasMount := false
		for _, ns := range b.Spec.Linux.Namespaces {
			if ns.Type == specs.PIDNamespace {
				hasPID = true
			}
			if ns.Type == specs.MountNamespace {
				hasMount = true
			}
		}
		assert.True(t, hasPID, "PID namespace should remain")
		assert.True(t, hasMount, "Mount namespace should remain")
	})

	t.Run("handles nil Linux config", func(t *testing.T) {
		tmpDir := t.TempDir()
		bundlePath := filepath.Join(tmpDir, "test-container")

		// Create bundle with no Linux config
		require.NoError(t, os.MkdirAll(bundlePath, 0750))
		spec := specs.Spec{
			Version: "1.0.0",
			Root:    &specs.Root{Path: "rootfs"},
		}
		specBytes, _ := json.Marshal(spec)
		require.NoError(t, os.WriteFile(filepath.Join(bundlePath, "config.json"), specBytes, 0600))
		require.NoError(t, os.MkdirAll(filepath.Join(bundlePath, "rootfs"), 0750))

		b, err := bundle.Load(ctx, bundlePath)
		require.NoError(t, err)

		// Should not panic
		err = DisableNetworkNamespace(ctx, b)
		require.NoError(t, err)
	})

	t.Run("handles empty namespaces", func(t *testing.T) {
		tmpDir := t.TempDir()
		bundlePath := filepath.Join(tmpDir, "test-container")

		// Create bundle with empty namespaces
		require.NoError(t, os.MkdirAll(bundlePath, 0750))
		spec := specs.Spec{
			Version: "1.0.0",
			Root:    &specs.Root{Path: "rootfs"},
			Linux:   &specs.Linux{Namespaces: []specs.LinuxNamespace{}},
		}
		specBytes, _ := json.Marshal(spec)
		require.NoError(t, os.WriteFile(filepath.Join(bundlePath, "config.json"), specBytes, 0600))
		require.NoError(t, os.MkdirAll(filepath.Join(bundlePath, "rootfs"), 0750))

		b, err := bundle.Load(ctx, bundlePath)
		require.NoError(t, err)

		err = DisableNetworkNamespace(ctx, b)
		require.NoError(t, err)
		assert.Empty(t, b.Spec.Linux.Namespaces)
	})

	t.Run("no network namespace present", func(t *testing.T) {
		tmpDir := t.TempDir()
		bundlePath := filepath.Join(tmpDir, "test-container")

		// Create bundle with only non-network namespaces
		require.NoError(t, os.MkdirAll(bundlePath, 0750))
		spec := specs.Spec{
			Version: "1.0.0",
			Root:    &specs.Root{Path: "rootfs"},
			Linux: &specs.Linux{
				Namespaces: []specs.LinuxNamespace{
					{Type: specs.PIDNamespace},
				},
			},
		}
		specBytes, _ := json.Marshal(spec)
		require.NoError(t, os.WriteFile(filepath.Join(bundlePath, "config.json"), specBytes, 0600))
		require.NoError(t, os.MkdirAll(filepath.Join(bundlePath, "rootfs"), 0750))

		b, err := bundle.Load(ctx, bundlePath)
		require.NoError(t, err)

		err = DisableNetworkNamespace(ctx, b)
		require.NoError(t, err)

		// PID namespace should still be present
		assert.Len(t, b.Spec.Linux.Namespaces, 1)
		assert.Equal(t, specs.PIDNamespace, b.Spec.Linux.Namespaces[0].Type)
	})
}

func TestLoadForCreate(t *testing.T) {
	ctx := context.Background()

	t.Run("applies all transforms", func(t *testing.T) {
		tmpDir := t.TempDir()
		bundlePath := filepath.Join(tmpDir, "test-container")
		createTestBundle(t, bundlePath)

		// Create a file to bind mount
		testFile := filepath.Join(bundlePath, "app.conf")
		require.NoError(t, os.WriteFile(testFile, []byte("config"), 0600))

		// Modify the config.json to include the bind mount
		specBytes, _ := os.ReadFile(filepath.Join(bundlePath, "config.json"))
		var spec specs.Spec
		require.NoError(t, json.Unmarshal(specBytes, &spec))

		spec.Mounts = append(spec.Mounts, specs.Mount{
			Destination: "/etc/app.conf",
			Type:        "bind",
			Source:      testFile,
		})

		specBytes, _ = json.Marshal(spec)
		require.NoError(t, os.WriteFile(filepath.Join(bundlePath, "config.json"), specBytes, 0600))

		// Load with transforms
		b, err := LoadForCreate(ctx, bundlePath)
		require.NoError(t, err)

		// Verify network namespace was removed
		for _, ns := range b.Spec.Linux.Namespaces {
			assert.NotEqual(t, specs.NetworkNamespace, ns.Type)
		}

		// Verify bind mount was transformed
		files, err := b.Files()
		require.NoError(t, err)
		assert.Contains(t, files, "app.conf")
	})

	t.Run("returns error for invalid bundle path", func(t *testing.T) {
		ctx := context.Background()

		_, err := LoadForCreate(ctx, "/nonexistent/bundle")
		require.Error(t, err)
	})

	t.Run("returns error for empty path", func(t *testing.T) {
		ctx := context.Background()

		_, err := LoadForCreate(ctx, "")
		require.Error(t, err)
	})
}
