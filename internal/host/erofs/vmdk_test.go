package erofs

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVmdkDescAddExtent(t *testing.T) {
	tests := []struct {
		name     string
		sectors  uint64
		filename string
		offset   uint64
		want     string
	}{
		{
			name:     "small extent",
			sectors:  1000,
			filename: "/dev/vda",
			offset:   0,
			want:     "RW 1000 FLAT \"/dev/vda\" 0\n",
		},
		{
			name:     "extent with offset",
			sectors:  500,
			filename: "/dev/vdb",
			offset:   1000,
			want:     "RW 500 FLAT \"/dev/vdb\" 1000\n",
		},
		{
			name:     "zero sectors",
			sectors:  0,
			filename: "/dev/vda",
			offset:   0,
			want:     "",
		},
		{
			name:     "exactly max extent size",
			sectors:  max2GbExtentSectors,
			filename: "/dev/vda",
			offset:   0,
			want:     "RW 4194304 FLAT \"/dev/vda\" 0\n",
		},
		{
			name:     "larger than max extent - splits into two",
			sectors:  max2GbExtentSectors + 100,
			filename: "/dev/vda",
			offset:   0,
			want:     "RW 4194304 FLAT \"/dev/vda\" 0\nRW 100 FLAT \"/dev/vda\" 4194304\n",
		},
		{
			name:     "multiple max extents",
			sectors:  max2GbExtentSectors * 2,
			filename: "/dev/vda",
			offset:   0,
			want:     "RW 4194304 FLAT \"/dev/vda\" 0\nRW 4194304 FLAT \"/dev/vda\" 4194304\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := vmdkDescAddExtent(&buf, tt.sectors, tt.filename, tt.offset)
			require.NoError(t, err)
			assert.Equal(t, tt.want, buf.String())
		})
	}
}

func TestDumpVMDKDescriptor(t *testing.T) {
	t.Run("single device", func(t *testing.T) {
		tmpDir := t.TempDir()
		devicePath := filepath.Join(tmpDir, "device1")

		// Create a 1MB file (2048 sectors)
		err := os.WriteFile(devicePath, make([]byte, 1024*1024), 0600)
		require.NoError(t, err)

		var buf bytes.Buffer
		err = DumpVMDKDescriptor(&buf, 0x12345678, []string{devicePath})
		require.NoError(t, err)

		output := buf.String()

		// Check header
		assert.Contains(t, output, "# Disk DescriptorFile")
		assert.Contains(t, output, "version=1")
		assert.Contains(t, output, "CID=12345678")
		assert.Contains(t, output, "parentCID=ffffffff")
		assert.Contains(t, output, "createType=\"twoGbMaxExtentFlat\"")

		// Check extent
		assert.Contains(t, output, "RW 2048 FLAT")
		assert.Contains(t, output, devicePath)

		// Check DDB section
		assert.Contains(t, output, "ddb.virtualHWVersion = \"4\"")
		assert.Contains(t, output, "ddb.geometry.heads = \"16\"")
		assert.Contains(t, output, "ddb.geometry.sectors = \"63\"")
		assert.Contains(t, output, "ddb.adapterType = \"ide\"")
	})

	t.Run("multiple devices", func(t *testing.T) {
		tmpDir := t.TempDir()
		device1 := filepath.Join(tmpDir, "device1")
		device2 := filepath.Join(tmpDir, "device2")

		// Create two 512KB files
		err := os.WriteFile(device1, make([]byte, 512*1024), 0600)
		require.NoError(t, err)
		err = os.WriteFile(device2, make([]byte, 512*1024), 0600)
		require.NoError(t, err)

		var buf bytes.Buffer
		err = DumpVMDKDescriptor(&buf, 0xABCDEF00, []string{device1, device2})
		require.NoError(t, err)

		output := buf.String()

		// Should have extents for both devices
		assert.Contains(t, output, device1)
		assert.Contains(t, output, device2)

		// Count extent lines
		extentCount := strings.Count(output, "RW ")
		assert.Equal(t, 2, extentCount, "should have 2 extent lines")
	})

	t.Run("device not found", func(t *testing.T) {
		var buf bytes.Buffer
		err := DumpVMDKDescriptor(&buf, 0x12345678, []string{"/nonexistent/device"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not exist")
	})

	t.Run("zero size device", func(t *testing.T) {
		tmpDir := t.TempDir()
		devicePath := filepath.Join(tmpDir, "empty")

		// Create empty file
		err := os.WriteFile(devicePath, []byte{}, 0600)
		require.NoError(t, err)

		var buf bytes.Buffer
		err = DumpVMDKDescriptor(&buf, 0x12345678, []string{devicePath})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "zero size")
	})

	t.Run("device too small", func(t *testing.T) {
		tmpDir := t.TempDir()
		devicePath := filepath.Join(tmpDir, "tiny")

		// Create file smaller than 512 bytes
		err := os.WriteFile(devicePath, make([]byte, 100), 0600)
		require.NoError(t, err)

		var buf bytes.Buffer
		err = DumpVMDKDescriptor(&buf, 0x12345678, []string{devicePath})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "too small")
	})

	t.Run("empty device list", func(t *testing.T) {
		var buf bytes.Buffer
		err := DumpVMDKDescriptor(&buf, 0x12345678, []string{})
		require.NoError(t, err)

		output := buf.String()
		// Should still have header and DDB, just no extents
		assert.Contains(t, output, "# Disk DescriptorFile")
		assert.Contains(t, output, "ddb.virtualHWVersion")
	})
}

func TestDumpVMDKDescriptorToFile(t *testing.T) {
	t.Run("creates valid file", func(t *testing.T) {
		tmpDir := t.TempDir()
		devicePath := filepath.Join(tmpDir, "device")
		vmdkPath := filepath.Join(tmpDir, "output.vmdk")

		// Create a 1MB device file
		err := os.WriteFile(devicePath, make([]byte, 1024*1024), 0600)
		require.NoError(t, err)

		err = DumpVMDKDescriptorToFile(vmdkPath, 0x12345678, []string{devicePath})
		require.NoError(t, err)

		// Verify file was created and contains expected content
		content, err := os.ReadFile(vmdkPath)
		require.NoError(t, err)

		assert.Contains(t, string(content), "# Disk DescriptorFile")
		assert.Contains(t, string(content), "CID=12345678")
		assert.Contains(t, string(content), devicePath)
	})

	t.Run("invalid output path", func(t *testing.T) {
		tmpDir := t.TempDir()
		devicePath := filepath.Join(tmpDir, "device")
		vmdkPath := filepath.Join("/nonexistent/dir", "output.vmdk")

		err := os.WriteFile(devicePath, make([]byte, 1024*1024), 0600)
		require.NoError(t, err)

		err = DumpVMDKDescriptorToFile(vmdkPath, 0x12345678, []string{devicePath})
		require.Error(t, err)
	})

	t.Run("device error propagates", func(t *testing.T) {
		tmpDir := t.TempDir()
		vmdkPath := filepath.Join(tmpDir, "output.vmdk")

		err := DumpVMDKDescriptorToFile(vmdkPath, 0x12345678, []string{"/nonexistent/device"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not exist")
	})
}

func TestConstants(t *testing.T) {
	// Verify the constants match expected VMDK format values
	assert.Equal(t, uint64(0x80000000>>9), uint64(max2GbExtentSectors), "max2GbExtentSectors should be 2GB in sectors")
	assert.Equal(t, uint64(4194304), uint64(max2GbExtentSectors), "max2GbExtentSectors should be 4194304 sectors (2GB)")
	assert.Equal(t, 63, sectorsPerTrack)
	assert.Equal(t, 16, numberHeads)
	assert.Equal(t, "twoGbMaxExtentFlat", subformat)
	assert.Equal(t, "ide", adapterType)
	assert.Equal(t, "4", hwVersion)
}

// Benchmarks

func BenchmarkVmdkDescAddExtent(b *testing.B) {
	var buf bytes.Buffer
	for b.Loop() {
		buf.Reset()
		_ = vmdkDescAddExtent(&buf, 1000000, "/dev/vda", 0)
	}
}

func BenchmarkDumpVMDKDescriptor(b *testing.B) {
	tmpDir := b.TempDir()
	devicePath := filepath.Join(tmpDir, "device")
	_ = os.WriteFile(devicePath, make([]byte, 1024*1024), 0600)

	var buf bytes.Buffer
	for b.Loop() {
		buf.Reset()
		_ = DumpVMDKDescriptor(&buf, 0x12345678, []string{devicePath})
	}
}
