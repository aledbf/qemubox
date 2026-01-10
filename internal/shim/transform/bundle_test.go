//go:build linux

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

	"github.com/spin-stack/spinbox/internal/shim/bundle"
)

const cgroupPath = "/sys/fs/cgroup"

func createTestBundle(t *testing.T, bundlePath string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(bundlePath, 0750))

	spec := specs.Spec{
		Version: "1.0.0",
		Root:    &specs.Root{Path: "rootfs"},
		Process: &specs.Process{Args: []string{"/bin/sh"}},
		Linux: &specs.Linux{
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace},
				{Type: specs.NetworkNamespace},
				{Type: specs.MountNamespace},
				{Type: specs.CgroupNamespace},
			},
		},
	}

	specBytes, err := json.Marshal(spec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bundlePath, "config.json"), specBytes, 0600))
	require.NoError(t, os.MkdirAll(filepath.Join(bundlePath, "rootfs"), 0750))
}

func TestTransformBindMounts(t *testing.T) {
	ctx := context.Background()

	t.Run("transforms bind mount from bundle path", func(t *testing.T) {
		tmpDir := t.TempDir()
		bundlePath := filepath.Join(tmpDir, "test-container")
		createTestBundle(t, bundlePath)

		testFile := filepath.Join(bundlePath, "config.yaml")
		testContent := []byte("key: value\n")
		require.NoError(t, os.WriteFile(testFile, testContent, 0600))

		b, err := bundle.Load(ctx, bundlePath)
		require.NoError(t, err)

		b.Spec.Mounts = append(b.Spec.Mounts, specs.Mount{
			Destination: "/etc/config.yaml",
			Type:        "bind",
			Source:      testFile,
		})

		err = TransformBindMounts(ctx, b)
		require.NoError(t, err)

		assert.Equal(t, "config.yaml", b.Spec.Mounts[len(b.Spec.Mounts)-1].Source)
		files, err := b.Files()
		require.NoError(t, err)
		assert.Equal(t, testContent, files["config.yaml"])
	})

	t.Run("ignores bind mount from different path", func(t *testing.T) {
		tmpDir := t.TempDir()
		bundlePath := filepath.Join(tmpDir, "test-container")
		createTestBundle(t, bundlePath)

		otherDir := filepath.Join(tmpDir, "other")
		require.NoError(t, os.MkdirAll(otherDir, 0750))
		testFile := filepath.Join(otherDir, "secret.txt")
		require.NoError(t, os.WriteFile(testFile, []byte("secret"), 0600))

		b, err := bundle.Load(ctx, bundlePath)
		require.NoError(t, err)

		b.Spec.Mounts = append(b.Spec.Mounts, specs.Mount{
			Destination: "/etc/secret.txt",
			Type:        "bind",
			Source:      testFile,
		})

		err = TransformBindMounts(ctx, b)
		require.NoError(t, err)

		assert.Equal(t, testFile, b.Spec.Mounts[len(b.Spec.Mounts)-1].Source)
	})
}

func TestAdaptForVM(t *testing.T) {
	ctx := context.Background()

	t.Run("removes network and cgroup namespaces", func(t *testing.T) {
		tmpDir := t.TempDir()
		bundlePath := filepath.Join(tmpDir, "test-container")
		createTestBundle(t, bundlePath)

		b, err := bundle.Load(ctx, bundlePath)
		require.NoError(t, err)

		err = AdaptForVM(ctx, b)
		require.NoError(t, err)

		for _, ns := range b.Spec.Linux.Namespaces {
			assert.NotEqual(t, specs.NetworkNamespace, ns.Type)
			assert.NotEqual(t, specs.CgroupNamespace, ns.Type)
		}

		// PID and Mount should remain
		hasPID, hasMount := false, false
		for _, ns := range b.Spec.Linux.Namespaces {
			if ns.Type == specs.PIDNamespace {
				hasPID = true
			}
			if ns.Type == specs.MountNamespace {
				hasMount = true
			}
		}
		assert.True(t, hasPID)
		assert.True(t, hasMount)
	})

	t.Run("adds cgroup2 mount if missing", func(t *testing.T) {
		tmpDir := t.TempDir()
		bundlePath := filepath.Join(tmpDir, "test-container")
		createTestBundle(t, bundlePath)

		b, err := bundle.Load(ctx, bundlePath)
		require.NoError(t, err)

		err = AdaptForVM(ctx, b)
		require.NoError(t, err)

		var cgMount *specs.Mount
		for i, m := range b.Spec.Mounts {
			if m.Destination == cgroupPath {
				cgMount = &b.Spec.Mounts[i]
				break
			}
		}
		require.NotNil(t, cgMount)
		assert.Equal(t, "cgroup2", cgMount.Type)
		assert.Contains(t, cgMount.Options, "rw")
	})

	t.Run("transforms existing cgroup mount", func(t *testing.T) {
		tmpDir := t.TempDir()
		bundlePath := filepath.Join(tmpDir, "test-container")

		require.NoError(t, os.MkdirAll(bundlePath, 0750))
		spec := specs.Spec{
			Version: "1.0.0",
			Root:    &specs.Root{Path: "rootfs"},
			Process: &specs.Process{Args: []string{"/bin/sh"}},
			Mounts: []specs.Mount{
				{Destination: cgroupPath, Type: "cgroup", Source: "cgroup", Options: []string{"ro"}},
			},
		}
		specBytes, _ := json.Marshal(spec)
		require.NoError(t, os.WriteFile(filepath.Join(bundlePath, "config.json"), specBytes, 0600))
		require.NoError(t, os.MkdirAll(filepath.Join(bundlePath, "rootfs"), 0750))

		b, err := bundle.Load(ctx, bundlePath)
		require.NoError(t, err)

		err = AdaptForVM(ctx, b)
		require.NoError(t, err)

		assert.Equal(t, "cgroup2", b.Spec.Mounts[0].Type)
		assert.Contains(t, b.Spec.Mounts[0].Options, "rw")
		assert.NotContains(t, b.Spec.Mounts[0].Options, "ro")
	})

	t.Run("grants full capabilities", func(t *testing.T) {
		tmpDir := t.TempDir()
		bundlePath := filepath.Join(tmpDir, "test-container")
		createTestBundle(t, bundlePath)

		b, err := bundle.Load(ctx, bundlePath)
		require.NoError(t, err)

		err = AdaptForVM(ctx, b)
		require.NoError(t, err)

		require.NotNil(t, b.Spec.Process.Capabilities)
		assert.Contains(t, b.Spec.Process.Capabilities.Bounding, "CAP_SYS_ADMIN")
		assert.Contains(t, b.Spec.Process.Capabilities.Effective, "CAP_SYS_ADMIN")
	})

	t.Run("handles nil Linux config", func(t *testing.T) {
		tmpDir := t.TempDir()
		bundlePath := filepath.Join(tmpDir, "test-container")

		require.NoError(t, os.MkdirAll(bundlePath, 0750))
		spec := specs.Spec{
			Version: "1.0.0",
			Root:    &specs.Root{Path: "rootfs"},
			Process: &specs.Process{Args: []string{"/bin/sh"}},
		}
		specBytes, _ := json.Marshal(spec)
		require.NoError(t, os.WriteFile(filepath.Join(bundlePath, "config.json"), specBytes, 0600))
		require.NoError(t, os.MkdirAll(filepath.Join(bundlePath, "rootfs"), 0750))

		b, err := bundle.Load(ctx, bundlePath)
		require.NoError(t, err)

		err = AdaptForVM(ctx, b)
		require.NoError(t, err)
	})
}

func TestLoadForCreate(t *testing.T) {
	ctx := context.Background()

	t.Run("applies all transforms", func(t *testing.T) {
		tmpDir := t.TempDir()
		bundlePath := filepath.Join(tmpDir, "test-container")
		createTestBundle(t, bundlePath)

		testFile := filepath.Join(bundlePath, "app.conf")
		require.NoError(t, os.WriteFile(testFile, []byte("config"), 0600))

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

		b, err := LoadForCreate(ctx, bundlePath)
		require.NoError(t, err)

		// Check namespaces removed
		for _, ns := range b.Spec.Linux.Namespaces {
			assert.NotEqual(t, specs.NetworkNamespace, ns.Type)
			assert.NotEqual(t, specs.CgroupNamespace, ns.Type)
		}

		// Check cgroup2 mount
		var cgMount *specs.Mount
		for i, m := range b.Spec.Mounts {
			if m.Destination == cgroupPath {
				cgMount = &b.Spec.Mounts[i]
				break
			}
		}
		require.NotNil(t, cgMount)
		assert.Equal(t, "cgroup2", cgMount.Type)

		// Check capabilities
		assert.Contains(t, b.Spec.Process.Capabilities.Bounding, "CAP_SYS_ADMIN")

		// Check bind mount transformed
		files, err := b.Files()
		require.NoError(t, err)
		assert.Contains(t, files, "app.conf")
	})

	t.Run("returns error for invalid path", func(t *testing.T) {
		_, err := LoadForCreate(ctx, "/nonexistent")
		require.Error(t, err)
	})
}
