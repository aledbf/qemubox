package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/opencontainers/runtime-spec/specs-go"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name          string
		setup         func(t *testing.T) string // returns bundle path
		transformers  []Transformer
		wantErr       bool
		wantErrSubstr string
		validate      func(t *testing.T, b *Bundle)
	}{
		{
			name: "valid bundle with relative rootfs",
			setup: func(t *testing.T) string {
				return createTestBundle(t, specs.Spec{
					Root: &specs.Root{Path: "rootfs"},
				})
			},
			wantErr: false,
			validate: func(t *testing.T, b *Bundle) {
				if b.Spec.Root.Path != "rootfs" {
					t.Errorf("expected root path 'rootfs', got %q", b.Spec.Root.Path)
				}
				if !filepath.IsAbs(b.Rootfs) {
					t.Errorf("expected absolute rootfs path, got %q", b.Rootfs)
				}
			},
		},
		{
			name: "valid bundle with absolute rootfs",
			setup: func(t *testing.T) string {
				absPath := filepath.Join(t.TempDir(), "container-rootfs")
				return createTestBundle(t, specs.Spec{
					Root: &specs.Root{Path: absPath},
				})
			},
			wantErr: false,
			validate: func(t *testing.T, b *Bundle) {
				if b.Spec.Root.Path != "rootfs" {
					t.Errorf("expected normalized root path 'rootfs', got %q", b.Spec.Root.Path)
				}
				if !filepath.IsAbs(b.Rootfs) {
					t.Errorf("expected absolute rootfs path, got %q", b.Rootfs)
				}
			},
		},
		{
			name: "empty path",
			setup: func(t *testing.T) string {
				return ""
			},
			wantErr:       true,
			wantErrSubstr: "bundle path cannot be empty",
		},
		{
			name: "nonexistent path",
			setup: func(t *testing.T) string {
				return "/nonexistent/path/to/bundle"
			},
			wantErr:       true,
			wantErrSubstr: "failed to read bundle config",
		},
		{
			name: "invalid json",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("not json"), 0644); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			wantErr:       true,
			wantErrSubstr: "failed to parse bundle spec",
		},
		{
			name: "missing root",
			setup: func(t *testing.T) string {
				return createTestBundle(t, specs.Spec{
					Root: nil,
				})
			},
			wantErr:       true,
			wantErrSubstr: "root path not specified",
		},
		{
			name: "transformer applied",
			setup: func(t *testing.T) string {
				return createTestBundle(t, specs.Spec{
					Root: &specs.Root{Path: "rootfs"},
				})
			},
			transformers: []Transformer{
				func(ctx context.Context, b *Bundle) error {
					b.Spec.Hostname = "test-host"
					return nil
				},
			},
			wantErr: false,
			validate: func(t *testing.T, b *Bundle) {
				if b.Spec.Hostname != "test-host" {
					t.Errorf("transformer not applied: got hostname %q", b.Spec.Hostname)
				}
			},
		},
		{
			name: "transformer error",
			setup: func(t *testing.T) string {
				return createTestBundle(t, specs.Spec{
					Root: &specs.Root{Path: "rootfs"},
				})
			},
			transformers: []Transformer{
				func(ctx context.Context, b *Bundle) error {
					return errors.New("transformer failed")
				},
			},
			wantErr:       true,
			wantErrSubstr: "transformer failed",
		},
		{
			name: "multiple transformers",
			setup: func(t *testing.T) string {
				return createTestBundle(t, specs.Spec{
					Root: &specs.Root{Path: "rootfs"},
				})
			},
			transformers: []Transformer{
				func(ctx context.Context, b *Bundle) error {
					b.Spec.Hostname = "host1"
					return nil
				},
				func(ctx context.Context, b *Bundle) error {
					b.Spec.Hostname = "host2"
					return nil
				},
			},
			wantErr: false,
			validate: func(t *testing.T, b *Bundle) {
				if b.Spec.Hostname != "host2" {
					t.Errorf("expected last transformer to win: got hostname %q", b.Spec.Hostname)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setup(t)
			b, err := Load(context.Background(), path, tt.transformers...)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.wantErrSubstr != "" && !contains(err.Error(), tt.wantErrSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErrSubstr)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.validate != nil {
				tt.validate(t, b)
			}
		})
	}
}

func TestAddExtraFile(t *testing.T) {
	tests := []struct {
		name          string
		fileName      string
		data          []byte
		wantErr       bool
		wantErrSubstr string
	}{
		{
			name:     "valid file",
			fileName: "init.sh",
			data:     []byte("#!/bin/sh\necho hello"),
			wantErr:  false,
		},
		{
			name:          "empty name",
			fileName:      "",
			data:          []byte("data"),
			wantErr:       true,
			wantErrSubstr: "file name cannot be empty",
		},
		{
			name:          "override config.json",
			fileName:      "config.json",
			data:          []byte("{}"),
			wantErr:       true,
			wantErrSubstr: "cannot override config.json",
		},
		{
			name:          "path with separator",
			fileName:      "etc/passwd",
			data:          []byte("data"),
			wantErr:       true,
			wantErrSubstr: "must not contain path separators",
		},
		{
			name:          "parent directory reference",
			fileName:      "..",
			data:          []byte("data"),
			wantErr:       true,
			wantErrSubstr: "must not contain path separators or relative components",
		},
		{
			name:          "current directory reference",
			fileName:      ".",
			data:          []byte("data"),
			wantErr:       true,
			wantErrSubstr: "must not contain path separators or relative components",
		},
		{
			name:          "path traversal attempt",
			fileName:      "../etc/passwd",
			data:          []byte("data"),
			wantErr:       true,
			wantErrSubstr: "must not contain path separators",
		},
		{
			name:          "hidden path traversal",
			fileName:      "file/../../../etc/passwd",
			data:          []byte("data"),
			wantErr:       true,
			wantErrSubstr: "must not contain path separators",
		},
		{
			name:     "valid filename with extension",
			fileName: "script.sh",
			data:     []byte("#!/bin/sh"),
			wantErr:  false,
		},
		{
			name:     "valid filename with multiple dots",
			fileName: "file.tar.gz",
			data:     []byte("data"),
			wantErr:  false,
		},
		{
			name:     "empty data",
			fileName: "empty.txt",
			data:     nil,
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &Bundle{
				extraFiles: make(map[string][]byte),
			}

			err := b.AddExtraFile(tt.fileName, tt.data)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.wantErrSubstr != "" && !contains(err.Error(), tt.wantErrSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErrSubstr)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Verify file was added
			if _, ok := b.extraFiles[tt.fileName]; !ok {
				t.Errorf("file %q not added to extraFiles", tt.fileName)
			}
		})
	}
}

func TestFiles(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T) *Bundle
		wantFiles []string
		validate  func(t *testing.T, b *Bundle, files map[string][]byte)
	}{
		{
			name: "empty bundle",
			setup: func(t *testing.T) *Bundle {
				return &Bundle{
					Spec:       specs.Spec{Version: "1.0.0"},
					extraFiles: make(map[string][]byte),
				}
			},
			wantFiles: []string{"config.json"},
		},
		{
			name: "bundle with extra files",
			setup: func(t *testing.T) *Bundle {
				b := &Bundle{
					Spec:       specs.Spec{Version: "1.0.0"},
					extraFiles: make(map[string][]byte),
				}
				b.extraFiles["init.sh"] = []byte("#!/bin/sh")
				b.extraFiles["data.txt"] = []byte("hello")
				return b
			},
			wantFiles: []string{"config.json", "init.sh", "data.txt"},
		},
		{
			name: "deep copy - modifications don't affect bundle",
			setup: func(t *testing.T) *Bundle {
				b := &Bundle{
					Spec:       specs.Spec{Version: "1.0.0"},
					extraFiles: make(map[string][]byte),
				}
				b.extraFiles["test.txt"] = []byte("original")
				return b
			},
			wantFiles: []string{"config.json", "test.txt"},
			validate: func(t *testing.T, b *Bundle, files map[string][]byte) {
				// Modify the returned file
				files["test.txt"][0] = 'X'

				// Verify bundle's internal state unchanged
				if string(b.extraFiles["test.txt"]) != "original" {
					t.Errorf("bundle internal state was modified: got %q, want %q",
						string(b.extraFiles["test.txt"]), "original")
				}
			},
		},
		{
			name: "config.json contains marshaled spec",
			setup: func(t *testing.T) *Bundle {
				return &Bundle{
					Spec:       specs.Spec{Version: "1.0.2", Hostname: "test"},
					extraFiles: make(map[string][]byte),
				}
			},
			wantFiles: []string{"config.json"},
			validate: func(t *testing.T, b *Bundle, files map[string][]byte) {
				var spec specs.Spec
				if err := json.Unmarshal(files["config.json"], &spec); err != nil {
					t.Fatalf("failed to unmarshal config.json: %v", err)
				}
				if spec.Version != "1.0.2" {
					t.Errorf("spec.Version = %q, want %q", spec.Version, "1.0.2")
				}
				if spec.Hostname != "test" {
					t.Errorf("spec.Hostname = %q, want %q", spec.Hostname, "test")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := tt.setup(t)

			files, err := b.Files()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Check expected files are present
			for _, wantFile := range tt.wantFiles {
				if _, ok := files[wantFile]; !ok {
					t.Errorf("expected file %q not found", wantFile)
				}
			}

			// Check no unexpected files
			if len(files) != len(tt.wantFiles) {
				t.Errorf("got %d files, want %d", len(files), len(tt.wantFiles))
			}

			if tt.validate != nil {
				tt.validate(t, b, files)
			}
		})
	}
}

func TestResolveRootfsPath(t *testing.T) {
	tests := []struct {
		name           string
		bundlePath     string
		rootPath       string
		isAbs          bool
		nilRoot        bool
		wantErr        bool
		wantErrIs      error
		validateRootfs func(t *testing.T, bundlePath, rootfs string)
	}{
		{
			name:       "relative path",
			bundlePath: "/var/lib/containerd/bundles/123",
			rootPath:   "rootfs",
			isAbs:      false,
			wantErr:    false,
			validateRootfs: func(t *testing.T, bundlePath, rootfs string) {
				expected := filepath.Join(bundlePath, "rootfs")
				if rootfs != expected {
					t.Errorf("rootfs = %q, want %q", rootfs, expected)
				}
			},
		},
		{
			name:       "absolute path",
			bundlePath: "/var/lib/containerd/bundles/123",
			rootPath:   "/var/lib/containerd/snapshots/overlay/456",
			isAbs:      true,
			wantErr:    false,
			validateRootfs: func(t *testing.T, bundlePath, rootfs string) {
				if rootfs != "/var/lib/containerd/snapshots/overlay/456" {
					t.Errorf("rootfs = %q, want %q", rootfs, "/var/lib/containerd/snapshots/overlay/456")
				}
			},
		},
		{
			name:       "nil root",
			bundlePath: "/var/lib/containerd/bundles/123",
			nilRoot:    true,
			wantErr:    true,
			wantErrIs:  errdefs.ErrInvalidArgument,
		},
		{
			name:       "root path normalized to rootfs",
			bundlePath: "/bundle",
			rootPath:   "custom/path/to/rootfs",
			isAbs:      false,
			wantErr:    false,
			validateRootfs: func(t *testing.T, bundlePath, rootfs string) {
				// After transformation, spec.Root.Path should be "rootfs"
				// but b.Rootfs should be the absolute path
				if !filepath.IsAbs(rootfs) {
					t.Errorf("rootfs should be absolute, got %q", rootfs)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &Bundle{
				Path: tt.bundlePath,
				Spec: specs.Spec{},
			}

			if !tt.nilRoot {
				b.Spec.Root = &specs.Root{Path: tt.rootPath}
			}

			err := resolveRootfsPath(context.Background(), b)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
					t.Errorf("expected error to wrap %v, got %v", tt.wantErrIs, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Verify spec.Root.Path was normalized
			if b.Spec.Root.Path != "rootfs" {
				t.Errorf("spec.Root.Path = %q, want %q", b.Spec.Root.Path, "rootfs")
			}

			if tt.validateRootfs != nil {
				tt.validateRootfs(t, tt.bundlePath, b.Rootfs)
			}
		})
	}
}

// Helper functions

func createTestBundle(t *testing.T, spec specs.Spec) string {
	t.Helper()

	dir := t.TempDir()
	specBytes, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "config.json"), specBytes, 0644); err != nil {
		t.Fatal(err)
	}

	return dir
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && stringContains(s, substr)))
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
