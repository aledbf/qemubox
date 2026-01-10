package paths

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spin-stack/spinbox/internal/config"
)

func TestFileExists(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, tmpDir string) string
		want  bool
	}{
		{
			name: "returns true for existing file",
			setup: func(t *testing.T, tmpDir string) string {
				path := filepath.Join(tmpDir, "file")
				if err := os.WriteFile(path, []byte("test"), 0644); err != nil {
					t.Fatal(err)
				}
				return path
			},
			want: true,
		},
		{
			name: "returns true for symlink to existing file",
			setup: func(t *testing.T, tmpDir string) string {
				realFile := filepath.Join(tmpDir, "realfile")
				if err := os.WriteFile(realFile, []byte("test"), 0644); err != nil {
					t.Fatal(err)
				}
				symlinkPath := filepath.Join(tmpDir, "linkfile")
				if err := os.Symlink(realFile, symlinkPath); err != nil {
					t.Fatal(err)
				}
				return symlinkPath
			},
			want: true,
		},
		{
			name: "returns false for broken symlink",
			setup: func(t *testing.T, tmpDir string) string {
				brokenLink := filepath.Join(tmpDir, "broken")
				if err := os.Symlink("/nonexistent/target", brokenLink); err != nil {
					t.Fatal(err)
				}
				return brokenLink
			},
			want: false,
		},
		{
			name: "returns false for directory",
			setup: func(t *testing.T, tmpDir string) string {
				dirPath := filepath.Join(tmpDir, "testdir")
				if err := os.MkdirAll(dirPath, 0750); err != nil {
					t.Fatal(err)
				}
				return dirPath
			},
			want: false,
		},
		{
			name: "returns false for non-existent path",
			setup: func(t *testing.T, tmpDir string) string {
				return filepath.Join(tmpDir, "nonexistent")
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			path := tt.setup(t, tmpDir)

			if got := fileExists(path); got != tt.want {
				t.Errorf("fileExists() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDirExists(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, tmpDir string) string
		want  bool
	}{
		{
			name: "returns true for existing directory",
			setup: func(t *testing.T, tmpDir string) string {
				dirPath := filepath.Join(tmpDir, "testdir")
				if err := os.MkdirAll(dirPath, 0750); err != nil {
					t.Fatal(err)
				}
				return dirPath
			},
			want: true,
		},
		{
			name: "returns true for symlink to existing directory",
			setup: func(t *testing.T, tmpDir string) string {
				realDir := filepath.Join(tmpDir, "realdir")
				if err := os.MkdirAll(realDir, 0750); err != nil {
					t.Fatal(err)
				}
				symlinkPath := filepath.Join(tmpDir, "linkdir")
				if err := os.Symlink(realDir, symlinkPath); err != nil {
					t.Fatal(err)
				}
				return symlinkPath
			},
			want: true,
		},
		{
			name: "returns false for broken symlink",
			setup: func(t *testing.T, tmpDir string) string {
				brokenLink := filepath.Join(tmpDir, "broken")
				if err := os.Symlink("/nonexistent/target", brokenLink); err != nil {
					t.Fatal(err)
				}
				return brokenLink
			},
			want: false,
		},
		{
			name: "returns false for file",
			setup: func(t *testing.T, tmpDir string) string {
				filePath := filepath.Join(tmpDir, "testfile")
				if err := os.WriteFile(filePath, []byte("test"), 0644); err != nil {
					t.Fatal(err)
				}
				return filePath
			},
			want: false,
		},
		{
			name: "returns false for symlink to file",
			setup: func(t *testing.T, tmpDir string) string {
				realFile := filepath.Join(tmpDir, "realfile")
				if err := os.WriteFile(realFile, []byte("test"), 0644); err != nil {
					t.Fatal(err)
				}
				symlinkPath := filepath.Join(tmpDir, "fakedir")
				if err := os.Symlink(realFile, symlinkPath); err != nil {
					t.Fatal(err)
				}
				return symlinkPath
			},
			want: false,
		},
		{
			name: "returns false for non-existent path",
			setup: func(t *testing.T, tmpDir string) string {
				return filepath.Join(tmpDir, "nonexistent")
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			path := tt.setup(t, tmpDir)

			if got := dirExists(path); got != tt.want {
				t.Errorf("dirExists() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPathFunctions(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.PathsConfig
		fn   func(config.PathsConfig) string
		want string
	}{
		{
			name: "KernelPath",
			cfg:  config.PathsConfig{ShareDir: "/usr/share/spinbox"},
			fn:   KernelPath,
			want: "/usr/share/spinbox/kernel/spinbox-kernel-x86_64",
		},
		{
			name: "InitrdPath",
			cfg:  config.PathsConfig{ShareDir: "/usr/share/spinbox"},
			fn:   InitrdPath,
			want: "/usr/share/spinbox/kernel/spinbox-initrd",
		},
		{
			name: "QemuPath with explicit config",
			cfg: config.PathsConfig{
				ShareDir: "/usr/share/spinbox",
				QEMUPath: "/custom/path/qemu-system-x86_64",
			},
			fn:   QemuPath,
			want: "/custom/path/qemu-system-x86_64",
		},
		{
			name: "QemuSharePath with explicit config",
			cfg: config.PathsConfig{
				ShareDir:      "/usr/share/spinbox",
				QEMUSharePath: "/custom/share/qemu",
			},
			fn:   QemuSharePath,
			want: "/custom/share/qemu",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fn(tt.cfg); got != tt.want {
				t.Errorf("%s() = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestDiscoverQemuPath(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, shareDir string) string // returns expected path
	}{
		{
			name: "finds in share dir bin",
			setup: func(t *testing.T, shareDir string) string {
				binDir := filepath.Join(shareDir, "bin")
				if err := os.MkdirAll(binDir, 0755); err != nil {
					t.Fatal(err)
				}
				qemuPath := filepath.Join(binDir, "qemu-system-x86_64")
				if err := os.WriteFile(qemuPath, []byte("#!/bin/sh\n"), 0755); err != nil {
					t.Fatal(err)
				}
				return qemuPath
			},
		},
		{
			name: "finds symlink to qemu",
			setup: func(t *testing.T, shareDir string) string {
				// Create real binary elsewhere
				realBinDir := filepath.Join(shareDir, "real-bin")
				if err := os.MkdirAll(realBinDir, 0755); err != nil {
					t.Fatal(err)
				}
				realQemuPath := filepath.Join(realBinDir, "qemu-system-x86_64")
				if err := os.WriteFile(realQemuPath, []byte("#!/bin/sh\n"), 0755); err != nil {
					t.Fatal(err)
				}
				// Create symlink in expected location
				binDir := filepath.Join(shareDir, "bin")
				if err := os.MkdirAll(binDir, 0755); err != nil {
					t.Fatal(err)
				}
				symlinkPath := filepath.Join(binDir, "qemu-system-x86_64")
				if err := os.Symlink(realQemuPath, symlinkPath); err != nil {
					t.Fatal(err)
				}
				return symlinkPath
			},
		},
		{
			name: "falls back to default when not found",
			setup: func(t *testing.T, shareDir string) string {
				return "/usr/bin/qemu-system-x86_64"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shareDir := t.TempDir()
			want := tt.setup(t, shareDir)

			if got := discoverQemuPath(shareDir); got != want {
				t.Errorf("discoverQemuPath() = %q, want %q", got, want)
			}
		})
	}
}

func TestDiscoverQemuSharePath(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, shareDir string) string // returns expected path
	}{
		{
			name: "finds in share dir",
			setup: func(t *testing.T, shareDir string) string {
				qemuShareDir := filepath.Join(shareDir, "qemu")
				if err := os.MkdirAll(qemuShareDir, 0755); err != nil {
					t.Fatal(err)
				}
				return qemuShareDir
			},
		},
		{
			name: "falls back to default when not found",
			setup: func(t *testing.T, shareDir string) string {
				return "/usr/share/qemu"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shareDir := t.TempDir()
			want := tt.setup(t, shareDir)

			if got := discoverQemuSharePath(shareDir); got != want {
				t.Errorf("discoverQemuSharePath() = %q, want %q", got, want)
			}
		})
	}
}

func TestQemuPathDiscovery(t *testing.T) {
	shareDir := t.TempDir()

	// Create qemu binary in share dir
	binDir := filepath.Join(shareDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	qemuPath := filepath.Join(binDir, "qemu-system-x86_64")
	if err := os.WriteFile(qemuPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}

	cfg := config.PathsConfig{ShareDir: shareDir}

	if got := QemuPath(cfg); got != qemuPath {
		t.Errorf("QemuPath() with discovery = %q, want %q", got, qemuPath)
	}
}

func TestQemuSharePathDiscovery(t *testing.T) {
	shareDir := t.TempDir()

	// Create qemu share directory
	qemuShareDir := filepath.Join(shareDir, "qemu")
	if err := os.MkdirAll(qemuShareDir, 0755); err != nil {
		t.Fatal(err)
	}

	cfg := config.PathsConfig{ShareDir: shareDir}

	if got := QemuSharePath(cfg); got != qemuShareDir {
		t.Errorf("QemuSharePath() with discovery = %q, want %q", got, qemuShareDir)
	}
}
