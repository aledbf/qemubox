//go:build linux

package mounts

import (
	"context"
	"strings"
	"testing"

	"github.com/containerd/containerd/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestTranslateMountOptions(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		options  []string
		expected []string
	}{
		{
			name:     "passes through options unchanged",
			options:  []string{"ro", "noatime"},
			expected: []string{"ro", "noatime"},
		},
		{
			name:     "empty options",
			options:  []string{},
			expected: []string{},
		},
		{
			name:     "nil options",
			options:  nil,
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := translateMountOptions(ctx, tt.options)
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
		assert.Equal(t, "/dev/vda", mounts[0].Source)
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
		assert.Equal(t, "/dev/vda", mounts[0].Source)
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
		assert.Equal(t, "/dev/vda", mounts[0].Source)
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

// Benchmarks

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
