//go:build linux

package qemu

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aledbf/qemubox/containerd/internal/host/vm"
)

func TestGenerateStableDiskID(t *testing.T) {
	t.Run("generates stable ID for existing file", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFile := filepath.Join(tmpDir, "test.img")
		require.NoError(t, os.WriteFile(testFile, []byte("test"), 0600))

		id1, err := generateStableDiskID(testFile)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(id1, "disk-"))

		// Same file should produce same ID
		id2, err := generateStableDiskID(testFile)
		require.NoError(t, err)
		assert.Equal(t, id1, id2)
	})

	t.Run("different files produce different IDs", func(t *testing.T) {
		tmpDir := t.TempDir()
		file1 := filepath.Join(tmpDir, "file1.img")
		file2 := filepath.Join(tmpDir, "file2.img")

		require.NoError(t, os.WriteFile(file1, []byte("test1"), 0600))
		require.NoError(t, os.WriteFile(file2, []byte("test2"), 0600))

		id1, err := generateStableDiskID(file1)
		require.NoError(t, err)

		id2, err := generateStableDiskID(file2)
		require.NoError(t, err)

		assert.NotEqual(t, id1, id2)
	})

	t.Run("returns error for non-existent file", func(t *testing.T) {
		_, err := generateStableDiskID("/nonexistent/path")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to stat disk path")
	})
}

func TestInstance_AddDisk(t *testing.T) {
	ctx := context.Background()

	t.Run("adds disk when VM is new", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFile := filepath.Join(tmpDir, "test.img")
		require.NoError(t, os.WriteFile(testFile, []byte("test"), 0600))

		q := &Instance{}
		q.setState(vmStateNew)

		err := q.AddDisk(ctx, "test-disk", testFile)
		require.NoError(t, err)

		require.Len(t, q.disks, 1)
		assert.Equal(t, "test-disk", q.disks[0].ID)
		assert.Equal(t, testFile, q.disks[0].Path)
		assert.False(t, q.disks[0].Readonly)
	})

	t.Run("adds readonly disk", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFile := filepath.Join(tmpDir, "test.img")
		require.NoError(t, os.WriteFile(testFile, []byte("test"), 0600))

		q := &Instance{}
		q.setState(vmStateNew)

		err := q.AddDisk(ctx, "test-disk", testFile, vm.WithReadOnly())
		require.NoError(t, err)

		require.Len(t, q.disks, 1)
		assert.True(t, q.disks[0].Readonly)
	})

	t.Run("generates stable ID when blockID is empty", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFile := filepath.Join(tmpDir, "test.img")
		require.NoError(t, os.WriteFile(testFile, []byte("test"), 0600))

		q := &Instance{}
		q.setState(vmStateNew)

		err := q.AddDisk(ctx, "", testFile)
		require.NoError(t, err)

		require.Len(t, q.disks, 1)
		assert.True(t, strings.HasPrefix(q.disks[0].ID, "disk-"))
	})

	t.Run("fails after VM started", func(t *testing.T) {
		q := &Instance{}
		q.setState(vmStateRunning)

		err := q.AddDisk(ctx, "test-disk", "/some/path")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot add disk after VM started")
	})

	t.Run("adds multiple disks", func(t *testing.T) {
		tmpDir := t.TempDir()
		file1 := filepath.Join(tmpDir, "disk1.img")
		file2 := filepath.Join(tmpDir, "disk2.img")
		require.NoError(t, os.WriteFile(file1, []byte("1"), 0600))
		require.NoError(t, os.WriteFile(file2, []byte("2"), 0600))

		q := &Instance{}
		q.setState(vmStateNew)

		require.NoError(t, q.AddDisk(ctx, "disk1", file1))
		require.NoError(t, q.AddDisk(ctx, "disk2", file2))

		assert.Len(t, q.disks, 2)
	})
}

func TestInstance_AddNIC(t *testing.T) {
	ctx := context.Background()
	q := &Instance{}
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")

	err := q.AddNIC(ctx, "endpoint", mac, vm.NetworkModeUnixgram, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")
}

func TestInstance_AddTAPNIC(t *testing.T) {
	ctx := context.Background()

	t.Run("adds TAP NIC when VM is new", func(t *testing.T) {
		q := &Instance{}
		q.setState(vmStateNew)
		mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")

		err := q.AddTAPNIC(ctx, "tap0", mac)
		require.NoError(t, err)

		require.Len(t, q.nets, 1)
		assert.Equal(t, "tap0", q.nets[0].TapName)
		assert.Equal(t, "aa:bb:cc:dd:ee:ff", q.nets[0].MAC)
		assert.Equal(t, "net0", q.nets[0].ID)
	})

	t.Run("fails after VM started", func(t *testing.T) {
		q := &Instance{}
		q.setState(vmStateRunning)
		mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")

		err := q.AddTAPNIC(ctx, "tap0", mac)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot add NIC after VM started")
	})

	t.Run("adds multiple NICs with sequential IDs", func(t *testing.T) {
		q := &Instance{}
		q.setState(vmStateNew)
		mac1, _ := net.ParseMAC("aa:bb:cc:dd:ee:f1")
		mac2, _ := net.ParseMAC("aa:bb:cc:dd:ee:f2")

		require.NoError(t, q.AddTAPNIC(ctx, "tap0", mac1))
		require.NoError(t, q.AddTAPNIC(ctx, "tap1", mac2))

		require.Len(t, q.nets, 2)
		assert.Equal(t, "net0", q.nets[0].ID)
		assert.Equal(t, "net1", q.nets[1].ID)
	})
}

func TestInstance_VMInfo(t *testing.T) {
	q := &Instance{}

	info := q.VMInfo()

	assert.Equal(t, "qemu", info.Type)
	assert.True(t, info.SupportsTAP)
	assert.True(t, info.SupportsVSOCK)
}

// Benchmarks

func BenchmarkGenerateStableDiskID(b *testing.B) {
	tmpDir := b.TempDir()
	testFile := filepath.Join(tmpDir, "test.img")
	_ = os.WriteFile(testFile, []byte("test"), 0600)

	for b.Loop() {
		_, _ = generateStableDiskID(testFile)
	}
}

func BenchmarkAddDisk(b *testing.B) {
	ctx := context.Background()
	tmpDir := b.TempDir()
	testFile := filepath.Join(tmpDir, "test.img")
	_ = os.WriteFile(testFile, []byte("test"), 0600)

	for b.Loop() {
		q := &Instance{}
		q.setState(vmStateNew)
		_ = q.AddDisk(ctx, "disk", testFile)
	}
}
