//go:build linux

package mounts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/errdefs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/spin-stack/spinbox/internal/host/vm"
)

func TestTransformMounts_ExceedsDiskCap(t *testing.T) {
	m := &linuxManager{}

	// More individual EROFS layer devices than maxVirtioDisks: the cap check
	// runs before any disk is attached, so a nil vm.Instance is never used.
	var ms []*types.Mount
	for i := 0; i <= maxVirtioDisks; i++ {
		ms = append(ms, &types.Mount{
			Type:   "erofs",
			Source: fmt.Sprintf("/path/to/layer%d.erofs", i),
		})
	}

	_, err := m.transformMounts(context.Background(), nil, "container-id", ms)
	require.Error(t, err)
	// Transient precondition (VMDK/fsmeta not ready), not Unimplemented.
	assert.ErrorIs(t, err, errdefs.ErrFailedPrecondition)
}

func TestTruncateID(t *testing.T) {
	tests := []struct {
		name     string
		prefix   string
		id       string
		expected string
	}{
		{
			name:     "short id",
			prefix:   "disk",
			id:       "abc123",
			expected: "disk-abc123",
		},
		{
			name:     "exactly 36 chars",
			prefix:   "rootfs",
			id:       "12345678901234567890123456789",
			expected: "rootfs-12345678901234567890123456789",
		},
		{
			name:     "truncated to 36 chars",
			prefix:   "rootfs",
			id:       "this-is-a-very-long-container-id-that-exceeds-limit",
			expected: "rootfs-this-is-a-very-long-container",
		},
		{
			name:     "empty id",
			prefix:   "disk",
			id:       "",
			expected: "disk-",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := truncateID(tt.prefix, tt.id)
			assert.Equal(t, tt.expected, result)
			assert.LessOrEqual(t, len(result), 36)
		})
	}
}

func TestFilterOptions(t *testing.T) {
	tests := []struct {
		name     string
		input    []string
		expected []string
	}{
		{
			name:     "removes loop option",
			input:    []string{"ro", "loop", "noatime"},
			expected: []string{"ro", "noatime"},
		},
		{
			name:     "no loop option",
			input:    []string{"ro", "noatime"},
			expected: []string{"ro", "noatime"},
		},
		{
			name:     "only loop option",
			input:    []string{"loop"},
			expected: nil,
		},
		{
			name:     "empty options",
			input:    []string{},
			expected: nil,
		},
		{
			name:     "nil options",
			input:    nil,
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterOptions(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestHandleOverlay(t *testing.T) {
	ctx := context.Background()
	m := &linuxManager{}

	tests := []struct {
		name            string
		mount           *types.Mount
		expectMounts    int  // Expected number of mounts returned
		expectTransform bool // Whether overlay was transformed (tmpfs added)
		expectPassThru  bool // Whether mount is passed through unchanged
	}{
		{
			name: "valid overlay with template variables in all - no transform",
			mount: &types.Mount{
				Type: "overlay",
				Options: []string{
					"lowerdir={{ overlay 0 4 }}",
					"upperdir={{ mount 5 }}/upper",
					"workdir={{ mount 5 }}/work",
				},
			},
			expectMounts:    1, // Just the overlay
			expectTransform: false,
			expectPassThru:  true,
		},
		{
			name: "overlay with lowerdir template but host upperdir/workdir - transforms",
			mount: &types.Mount{
				Type: "format/mkdir/overlay",
				Options: []string{
					"lowerdir={{ overlay 0 4 }}",
					"upperdir=/tmp/upper",
					"workdir=/tmp/work",
				},
			},
			expectMounts:    2, // tmpfs + transformed overlay
			expectTransform: true,
			expectPassThru:  false,
		},
		{
			name: "overlay without lowerdir template - no transform",
			mount: &types.Mount{
				Type: "overlay",
				Options: []string{
					"lowerdir=/lower",
					"upperdir=/tmp/upper",
					"workdir=/tmp/work",
				},
			},
			expectMounts:    1, // Just the original overlay
			expectTransform: false,
			expectPassThru:  true,
		},
		{
			name: "overlay without upperdir or workdir - pass through",
			mount: &types.Mount{
				Type: "overlay",
				Options: []string{
					"lowerdir=/lower",
				},
			},
			expectMounts:   1, // Just the original overlay
			expectPassThru: true,
		},
		{
			name: "overlay with only upperdir - pass through",
			mount: &types.Mount{
				Type: "overlay",
				Options: []string{
					"lowerdir=/lower",
					"upperdir=/tmp/upper",
				},
			},
			expectMounts:   1, // Just the original overlay
			expectPassThru: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mounts, disks, err := m.handleOverlay(ctx, "test-id", tt.mount)
			require.NoError(t, err)
			require.Len(t, mounts, tt.expectMounts)
			require.Empty(t, disks) // handleOverlay doesn't add disks

			if tt.expectTransform {
				// First mount should be tmpfs
				assert.Equal(t, "tmpfs", mounts[0].Type)
				assert.Equal(t, "tmpfs", mounts[0].Source)

				// Second mount should be the transformed overlay
				assert.Equal(t, "format/mkdir/overlay", mounts[1].Type)
				// Check that upperdir/workdir now use templates
				hasUpperTemplate := false
				hasWorkTemplate := false
				hasMkdirPaths := false
				for _, opt := range mounts[1].Options {
					if strings.HasPrefix(opt, "upperdir=") && strings.Contains(opt, "{{ mount") {
						hasUpperTemplate = true
					}
					if strings.HasPrefix(opt, "workdir=") && strings.Contains(opt, "{{ mount") {
						hasWorkTemplate = true
					}
					if strings.HasPrefix(opt, "X-containerd.mkdir.path=") {
						hasMkdirPaths = true
					}
				}
				assert.True(t, hasUpperTemplate, "transformed overlay should have upperdir template")
				assert.True(t, hasWorkTemplate, "transformed overlay should have workdir template")
				assert.True(t, hasMkdirPaths, "transformed overlay should have mkdir paths")
			}

			if tt.expectPassThru {
				// Mount should be unchanged
				assert.Equal(t, tt.mount.Type, mounts[0].Type)
			}
		})
	}
}

func TestNewManager(t *testing.T) {
	mgr := newManager()
	require.NotNil(t, mgr)

	_, ok := mgr.(*linuxManager)
	assert.True(t, ok, "should return linuxManager")
}

func TestHandleExt4(t *testing.T) {
	m := &linuxManager{}

	t.Run("read-write mount", func(t *testing.T) {
		disks := byte('a')
		mnt := &types.Mount{
			Type:    "ext4",
			Source:  "/dev/sda1",
			Target:  "/data",
			Options: []string{"noatime"},
		}

		mounts, diskOpts, err := m.handleExt4("container-id", &disks, mnt)

		require.NoError(t, err)
		require.Len(t, mounts, 1)
		require.Len(t, diskOpts, 1)

		assert.Equal(t, "ext4", mounts[0].Type)
		assert.Equal(t, "serial:sbxblk0", mounts[0].Source)
		assert.Equal(t, "/data", mounts[0].Target)

		assert.False(t, diskOpts[0].readOnly)
		assert.Equal(t, byte('b'), disks) // Should increment
	})

	t.Run("read-only mount", func(t *testing.T) {
		disks := byte('a')
		mnt := &types.Mount{
			Type:    "ext4",
			Source:  "/dev/sda1",
			Target:  "/data",
			Options: []string{"ro", "noatime"},
		}

		_, diskOpts, err := m.handleExt4("container-id", &disks, mnt)

		require.NoError(t, err)
		require.Len(t, diskOpts, 1)
		assert.True(t, diskOpts[0].readOnly)
	})

	t.Run("read-only with readonly option", func(t *testing.T) {
		disks := byte('a')
		mnt := &types.Mount{
			Type:    "ext4",
			Source:  "/dev/sda1",
			Target:  "/data",
			Options: []string{"readonly"},
		}

		_, diskOpts, err := m.handleExt4("container-id", &disks, mnt)

		require.NoError(t, err)
		assert.True(t, diskOpts[0].readOnly)
	})

	t.Run("filters loop option", func(t *testing.T) {
		disks := byte('a')
		mnt := &types.Mount{
			Type:    "ext4",
			Source:  "/dev/sda1",
			Options: []string{"loop", "noatime"},
		}

		mounts, _, err := m.handleExt4("container-id", &disks, mnt)

		require.NoError(t, err)
		assert.NotContains(t, mounts[0].Options, "loop")
		assert.Contains(t, mounts[0].Options, "noatime")
	})
}

func TestTransformMount(t *testing.T) {
	ctx := context.Background()
	m := &linuxManager{}

	t.Run("handles format/erofs like erofs", func(t *testing.T) {
		disks := byte('a')
		mnt := &types.Mount{
			Type:   "format/erofs",
			Source: "/path/to/fsmeta.erofs",
			Options: []string{
				"ro",
				"loop",
				"device=/path/to/layer1.erofs",
				"device=/path/to/layer2.erofs",
			},
		}

		mounts, diskOpts, err := m.transformMount(ctx, "id", &disks, mnt)

		require.NoError(t, err)
		require.Len(t, mounts, 1)
		require.Len(t, diskOpts, 1)
		// Output should be erofs (not format/erofs) for the guest
		assert.Equal(t, "erofs", mounts[0].Type)
		assert.Equal(t, "serial:sbxblk0", mounts[0].Source)
		// device= options should be filtered out (not needed when using VMDK)
		for _, opt := range mounts[0].Options {
			assert.False(t, strings.HasPrefix(opt, "device="), "device= options should be filtered")
		}
	})

	t.Run("passes through unknown types", func(t *testing.T) {
		disks := byte('a')
		mnt := &types.Mount{
			Type:    "tmpfs",
			Source:  "tmpfs",
			Target:  "/tmp",
			Options: []string{"size=100m"},
		}

		mounts, diskOpts, err := m.transformMount(ctx, "id", &disks, mnt)

		require.NoError(t, err)
		require.Len(t, mounts, 1)
		assert.Empty(t, diskOpts)
		assert.Equal(t, mnt, mounts[0])
	})

	t.Run("handles ext4 mount", func(t *testing.T) {
		disks := byte('a')
		mnt := &types.Mount{
			Type:   "ext4",
			Source: "/dev/sda1",
			Target: "/data",
		}

		mounts, diskOpts, err := m.transformMount(ctx, "id", &disks, mnt)

		require.NoError(t, err)
		require.Len(t, mounts, 1)
		require.Len(t, diskOpts, 1)
		assert.Equal(t, "ext4", mounts[0].Type)
		assert.Equal(t, "serial:sbxblk0", mounts[0].Source)
	})

	t.Run("handles overlay with templates", func(t *testing.T) {
		disks := byte('a')
		mnt := &types.Mount{
			Type: "overlay",
			Options: []string{
				"lowerdir=/lower",
				"upperdir={{ mount 0 }}/upper",
				"workdir={{ mount 0 }}/work",
			},
		}

		mounts, diskOpts, err := m.transformMount(ctx, "id", &disks, mnt)

		require.NoError(t, err)
		require.Len(t, mounts, 1)
		assert.Empty(t, diskOpts)
		assert.Equal(t, mnt, mounts[0])
	})
}

func TestHandleEROFS(t *testing.T) {
	ctx := context.Background()
	m := &linuxManager{}

	t.Run("basic erofs mount", func(t *testing.T) {
		disks := byte('a')
		mnt := &types.Mount{
			Type:    "erofs",
			Source:  "/path/to/image.erofs",
			Target:  "/rootfs",
			Options: []string{"ro"},
		}

		mounts, diskOpts, err := m.handleEROFS(ctx, "container-id", &disks, mnt)

		require.NoError(t, err)
		require.Len(t, mounts, 1)
		require.Len(t, diskOpts, 1)

		assert.Equal(t, "erofs", mounts[0].Type)
		assert.Equal(t, "serial:sbxblk0", mounts[0].Source)
		assert.Equal(t, "/rootfs", mounts[0].Target)

		assert.Equal(t, "/path/to/image.erofs", diskOpts[0].source)
		assert.True(t, diskOpts[0].readOnly)
		assert.Equal(t, vm.DefaultDiskFormat, diskOpts[0].format)
		assert.Equal(t, byte('b'), disks)
		// The disk serial must match the serial encoded in the guest mount Source
		// so the guest can resolve the device via /sys/block/<dev>/serial.
		assert.Equal(t, "sbxblk0", diskOpts[0].serial)
		assert.Equal(t, "serial:sbxblk0", mounts[0].Source)
	})

	t.Run("vmdk extension detected", func(t *testing.T) {
		disks := byte('a')
		mnt := &types.Mount{
			Type:   "erofs",
			Source: "/path/to/merged.vmdk",
			Target: "/rootfs",
		}

		_, diskOpts, err := m.handleEROFS(ctx, "container-id", &disks, mnt)

		require.NoError(t, err)
		require.Len(t, diskOpts, 1)
		assert.Equal(t, "vmdk", diskOpts[0].format)
	})

	t.Run("filters device options", func(t *testing.T) {
		disks := byte('a')
		mnt := &types.Mount{
			Type:   "format/erofs",
			Source: "/path/to/fsmeta.erofs",
			Options: []string{
				"ro",
				"device=/path/to/layer1.erofs",
				"device=/path/to/layer2.erofs",
			},
		}

		mounts, _, err := m.handleEROFS(ctx, "container-id", &disks, mnt)

		require.NoError(t, err)
		require.Len(t, mounts, 1)

		// device= options should be filtered out
		for _, opt := range mounts[0].Options {
			assert.False(t, strings.HasPrefix(opt, "device="), "device= options should be filtered")
		}
		// ro should be preserved
		assert.Contains(t, mounts[0].Options, "ro")
	})

	t.Run("increments disk letter correctly", func(t *testing.T) {
		disks := byte('c')
		mnt := &types.Mount{
			Type:   "erofs",
			Source: "/path/to/image.erofs",
		}

		mounts, _, err := m.handleEROFS(ctx, "container-id", &disks, mnt)

		require.NoError(t, err)
		assert.Equal(t, "serial:sbxblk2", mounts[0].Source)
		assert.Equal(t, byte('d'), disks)
	})

	t.Run("truncates long container id in disk name", func(t *testing.T) {
		disks := byte('a')
		mnt := &types.Mount{
			Type:   "erofs",
			Source: "/path/to/image.erofs",
		}

		_, diskOpts, err := m.handleEROFS(ctx, "very-long-container-id-that-exceeds-the-maximum-allowed-length", &disks, mnt)

		require.NoError(t, err)
		require.Len(t, diskOpts, 1)
		assert.LessOrEqual(t, len(diskOpts[0].name), 36)
	})
}

func TestGenerateOverlayIfNeeded(t *testing.T) {
	ctx := context.Background()
	m := &linuxManager{}

	t.Run("returns unchanged for single mount", func(t *testing.T) {
		mounts := []*types.Mount{
			{Type: "erofs", Source: "/dev/vda"},
		}

		result := m.generateOverlayIfNeeded(ctx, mounts)

		assert.Len(t, result, 1)
		assert.Equal(t, mounts, result)
	})

	t.Run("returns unchanged for empty mounts", func(t *testing.T) {
		result := m.generateOverlayIfNeeded(ctx, []*types.Mount{})

		assert.Empty(t, result)
	})

	t.Run("returns unchanged when overlay already exists", func(t *testing.T) {
		mounts := []*types.Mount{
			{Type: "erofs", Source: "/dev/vda"},
			{Type: "ext4", Source: "/dev/vdb"},
			{Type: "overlay", Source: "overlay"},
		}

		result := m.generateOverlayIfNeeded(ctx, mounts)

		assert.Equal(t, mounts, result)
	})

	t.Run("returns unchanged when format/overlay exists", func(t *testing.T) {
		mounts := []*types.Mount{
			{Type: "erofs", Source: "/dev/vda"},
			{Type: "ext4", Source: "/dev/vdb"},
			{Type: "format/overlay", Source: "overlay"},
		}

		result := m.generateOverlayIfNeeded(ctx, mounts)

		assert.Equal(t, mounts, result)
	})

	t.Run("returns unchanged for only erofs mounts", func(t *testing.T) {
		mounts := []*types.Mount{
			{Type: "erofs", Source: "/dev/vda"},
			{Type: "erofs", Source: "/dev/vdb"},
		}

		result := m.generateOverlayIfNeeded(ctx, mounts)

		assert.Equal(t, mounts, result)
	})

	t.Run("returns unchanged when ext4 is not last", func(t *testing.T) {
		mounts := []*types.Mount{
			{Type: "ext4", Source: "/dev/vda"},
			{Type: "erofs", Source: "/dev/vdb"},
		}

		result := m.generateOverlayIfNeeded(ctx, mounts)

		assert.Equal(t, mounts, result)
	})

	t.Run("generates overlay for erofs + ext4 pattern", func(t *testing.T) {
		mounts := []*types.Mount{
			{Type: "erofs", Source: "/dev/vda"},
			{Type: "ext4", Source: "/dev/vdb"},
		}

		result := m.generateOverlayIfNeeded(ctx, mounts)

		require.Len(t, result, 3) // original 2 + generated overlay
		assert.Equal(t, "erofs", result[0].Type)
		assert.Equal(t, "ext4", result[1].Type)
		assert.Equal(t, "format/mkdir/overlay", result[2].Type)

		// Check overlay options
		overlayOpts := result[2].Options
		hasLowerdir := false
		hasUpperdir := false
		hasWorkdir := false
		hasMkdirUpper := false
		hasMkdirWork := false

		for _, opt := range overlayOpts {
			switch {
			case strings.HasPrefix(opt, "lowerdir="):
				hasLowerdir = true
				assert.Contains(t, opt, "{{ overlay 0 0 }}")
			case strings.HasPrefix(opt, "upperdir="):
				hasUpperdir = true
				assert.Contains(t, opt, "{{ mount 1 }}/upper")
			case strings.HasPrefix(opt, "workdir="):
				hasWorkdir = true
				assert.Contains(t, opt, "{{ mount 1 }}/work")
			case opt == "X-containerd.mkdir.path={{ mount 1 }}/upper":
				hasMkdirUpper = true
			case opt == "X-containerd.mkdir.path={{ mount 1 }}/work":
				hasMkdirWork = true
			}
		}

		assert.True(t, hasLowerdir, "should have lowerdir")
		assert.True(t, hasUpperdir, "should have upperdir")
		assert.True(t, hasWorkdir, "should have workdir")
		assert.True(t, hasMkdirUpper, "should have mkdir for upper")
		assert.True(t, hasMkdirWork, "should have mkdir for work")
	})

	t.Run("generates overlay for multiple erofs + ext4", func(t *testing.T) {
		mounts := []*types.Mount{
			{Type: "erofs", Source: "/dev/vda"},
			{Type: "erofs", Source: "/dev/vdb"},
			{Type: "erofs", Source: "/dev/vdc"},
			{Type: "ext4", Source: "/dev/vdd"},
		}

		result := m.generateOverlayIfNeeded(ctx, mounts)

		require.Len(t, result, 5) // original 4 + generated overlay

		// Check overlay uses correct indices
		overlayOpts := result[4].Options
		for _, opt := range overlayOpts {
			if strings.HasPrefix(opt, "lowerdir=") {
				assert.Contains(t, opt, "{{ overlay 0 2 }}", "should reference all 3 erofs layers")
			}
			if strings.HasPrefix(opt, "upperdir=") {
				assert.Contains(t, opt, "{{ mount 3 }}", "ext4 is at index 3")
			}
		}
	})
}

func TestAnalyzeOverlayOptions(t *testing.T) {
	tests := []struct {
		name     string
		options  []string
		expected overlayAnalysis
	}{
		{
			name:    "empty options",
			options: []string{},
			expected: overlayAnalysis{
				workdirIdx:   -1,
				upperdirIdx:  -1,
				overlayEnd:   -1,
				hasLowerTmpl: false,
			},
		},
		{
			name: "all templates - no transform needed",
			options: []string{
				"lowerdir={{ overlay 0 4 }}",
				"upperdir={{ mount 5 }}/upper",
				"workdir={{ mount 5 }}/work",
			},
			expected: overlayAnalysis{
				workdirIdx:    2,
				upperdirIdx:   1,
				overlayEnd:    4,
				hasLowerTmpl:  true,
				needTransform: false,
			},
		},
		{
			name: "host paths need transform",
			options: []string{
				"lowerdir={{ overlay 0 2 }}",
				"upperdir=/tmp/upper",
				"workdir=/tmp/work",
			},
			expected: overlayAnalysis{
				workdirIdx:    2,
				upperdirIdx:   1,
				overlayEnd:    2,
				hasLowerTmpl:  true,
				needTransform: true,
			},
		},
		{
			name: "no lowerdir template",
			options: []string{
				"lowerdir=/lower1:/lower2",
				"upperdir=/tmp/upper",
				"workdir=/tmp/work",
			},
			expected: overlayAnalysis{
				workdirIdx:    2,
				upperdirIdx:   1,
				overlayEnd:    -1,
				hasLowerTmpl:  false,
				needTransform: true,
			},
		},
		{
			name: "only lowerdir present",
			options: []string{
				"lowerdir={{ overlay 0 1 }}",
			},
			expected: overlayAnalysis{
				workdirIdx:   -1,
				upperdirIdx:  -1,
				overlayEnd:   1,
				hasLowerTmpl: true,
			},
		},
		{
			name: "reverse order indices in template",
			options: []string{
				"lowerdir={{ overlay 5 2 }}",
			},
			expected: overlayAnalysis{
				workdirIdx:   -1,
				upperdirIdx:  -1,
				overlayEnd:   5, // max(5, 2)
				hasLowerTmpl: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := analyzeOverlayOptions(tt.options)

			assert.Equal(t, tt.expected.workdirIdx, result.workdirIdx, "workdirIdx mismatch")
			assert.Equal(t, tt.expected.upperdirIdx, result.upperdirIdx, "upperdirIdx mismatch")
			assert.Equal(t, tt.expected.overlayEnd, result.overlayEnd, "overlayEnd mismatch")
			assert.Equal(t, tt.expected.hasLowerTmpl, result.hasLowerTmpl, "hasLowerTmpl mismatch")
			assert.Equal(t, tt.expected.needTransform, result.needTransform, "needTransform mismatch")
		})
	}
}

func BenchmarkTruncateID(b *testing.B) {
	for range b.N {
		truncateID("rootfs", "this-is-a-very-long-container-id-that-exceeds-limit")
	}
}

func BenchmarkFilterOptions(b *testing.B) {
	options := []string{"ro", "loop", "noatime", "nodiratime"}
	for range b.N {
		filterOptions(options)
	}
}

func TestLayerDigestFromDevicePath(t *testing.T) {
	hex := strings.Repeat("a", 64)
	assert.Equal(t, "sha256:"+hex, layerDigestFromDevicePath("/var/lib/snap/sha256-"+hex+".erofs"))
	// Fallback-named blobs have no extractable digest.
	assert.Empty(t, layerDigestFromDevicePath("/var/lib/snap/snapshot-42.erofs"))
	// Non-erofs paths are ignored.
	assert.Empty(t, layerDigestFromDevicePath("/var/lib/snap/fsmeta.bin"))
}

func TestVerifyVMDKManifest(t *testing.T) {
	d1 := "sha256:" + strings.Repeat("1", 64)
	d2 := "sha256:" + strings.Repeat("2", 64)
	dev := func(d string) string {
		return "device=/var/lib/snap/" + strings.Replace(d, ":", "-", 1) + ".erofs"
	}

	writeManifest := func(t *testing.T, lines ...string) string {
		t.Helper()
		dir := t.TempDir()
		if lines != nil {
			require.NoError(t, os.WriteFile(filepath.Join(dir, manifestFilename), []byte(strings.Join(lines, "\n")+"\n"), 0o600))
		}
		return filepath.Join(dir, "merged.vmdk")
	}

	t.Run("matches device order", func(t *testing.T) {
		vmdk := writeManifest(t, d1, d2)
		opts := []string{"ro", "loop", dev(d1), dev(d2)}
		require.NoError(t, verifyVMDKManifest(vmdk, opts))
	})

	t.Run("missing manifest is tolerated", func(t *testing.T) {
		vmdk := writeManifest(t) // no manifest file written
		require.NoError(t, verifyVMDKManifest(vmdk, []string{dev(d1)}))
	})

	t.Run("fallback layer recorded as blob: token (1:1)", func(t *testing.T) {
		// A fallback (non-digest) blob sits between digest layers; the manifest
		// records it as "blob:<basename>" so it stays 1:1 with device=.
		vmdk := writeManifest(t, d1, "blob:snapshot-7.erofs", d2)
		opts := []string{dev(d1), "device=/var/lib/snap/snapshot-7.erofs", dev(d2)}
		require.NoError(t, verifyVMDKManifest(vmdk, opts))
	})

	t.Run("missing fallback line desyncs and is rejected", func(t *testing.T) {
		// Manifest dropped the fallback placeholder: now 2 lines vs 3 devices.
		vmdk := writeManifest(t, d1, d2)
		opts := []string{dev(d1), "device=/var/lib/snap/snapshot-7.erofs", dev(d2)}
		err := verifyVMDKManifest(vmdk, opts)
		require.Error(t, err)
		assert.ErrorIs(t, err, errdefs.ErrFailedPrecondition)
	})

	t.Run("wrong order is rejected", func(t *testing.T) {
		vmdk := writeManifest(t, d2, d1)
		opts := []string{dev(d1), dev(d2)}
		err := verifyVMDKManifest(vmdk, opts)
		require.Error(t, err)
		assert.ErrorIs(t, err, errdefs.ErrFailedPrecondition)
	})

	t.Run("count mismatch is rejected", func(t *testing.T) {
		vmdk := writeManifest(t, d1, d2)
		opts := []string{dev(d1)}
		err := verifyVMDKManifest(vmdk, opts)
		require.Error(t, err)
		assert.ErrorIs(t, err, errdefs.ErrFailedPrecondition)
	})
}
