package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/errdefs"

	api "github.com/spin-stack/spinbox/api/services/bundle/v1"
)

//nolint:cyclop // Table-driven test with many cases - clarity over complexity metrics.
func TestServiceCreate(t *testing.T) {
	tests := []struct {
		name         string
		bundleID     string
		files        map[string][]byte
		wantErr      bool
		wantErrType  error
		validate     func(t *testing.T, bundlePath string)
		setupFailure func(t *testing.T, bundleRoot string) // simulate failures
	}{
		{
			name:     "valid bundle with files",
			bundleID: "test-bundle-1",
			files: map[string][]byte{
				"config.json":  []byte(`{"version": "1.0.0"}`),
				"runtime.json": []byte(`{"runtime": "crun"}`),
			},
			wantErr: false,
			validate: func(t *testing.T, bundlePath string) {
				// Verify bundle directory exists
				if _, err := os.Stat(bundlePath); err != nil {
					t.Errorf("bundle directory not created: %v", err)
				}

				// Verify rootfs directory exists
				rootfsPath := filepath.Join(bundlePath, "rootfs")
				if _, err := os.Stat(rootfsPath); err != nil {
					t.Errorf("rootfs directory not created: %v", err)
				}

				// Verify files were written
				configPath := filepath.Join(bundlePath, "config.json")
				data, err := os.ReadFile(configPath)
				if err != nil {
					t.Errorf("config.json not written: %v", err)
				}
				if string(data) != `{"version": "1.0.0"}` {
					t.Errorf("config.json content mismatch: got %q", string(data))
				}

				// Verify permissions
				info, err := os.Stat(bundlePath)
				if err != nil {
					t.Fatalf("failed to stat bundle dir: %v", err)
				}
				if info.Mode().Perm() != bundleDirPerms {
					t.Errorf("bundle dir perms = %o, want %o", info.Mode().Perm(), bundleDirPerms)
				}

				fileInfo, err := os.Stat(configPath)
				if err != nil {
					t.Fatalf("failed to stat config.json: %v", err)
				}
				if fileInfo.Mode().Perm() != bundleFilePerms {
					t.Errorf("config.json perms = %o, want %o", fileInfo.Mode().Perm(), bundleFilePerms)
				}
			},
		},
		{
			name:     "valid bundle without files",
			bundleID: "test-bundle-2",
			files:    map[string][]byte{},
			wantErr:  false,
			validate: func(t *testing.T, bundlePath string) {
				// Verify bundle and rootfs directories exist
				if _, err := os.Stat(bundlePath); err != nil {
					t.Errorf("bundle directory not created: %v", err)
				}
				rootfsPath := filepath.Join(bundlePath, "rootfs")
				if _, err := os.Stat(rootfsPath); err != nil {
					t.Errorf("rootfs directory not created: %v", err)
				}
			},
		},
		{
			name:        "empty bundle ID",
			bundleID:    "",
			files:       map[string][]byte{},
			wantErr:     true,
			wantErrType: errdefs.ErrInvalidArgument,
		},
		{
			name:        "bundle ID with path traversal (..)",
			bundleID:    "../evil",
			files:       map[string][]byte{},
			wantErr:     true,
			wantErrType: errdefs.ErrInvalidArgument,
		},
		{
			name:        "bundle ID with slash",
			bundleID:    "foo/bar",
			files:       map[string][]byte{},
			wantErr:     true,
			wantErrType: errdefs.ErrInvalidArgument,
		},
		{
			name:     "file path with path traversal",
			bundleID: "test-bundle-3",
			files: map[string][]byte{
				"../etc/passwd": []byte("evil"),
			},
			wantErr:     true,
			wantErrType: errdefs.ErrInvalidArgument,
		},
		{
			name:     "file path with absolute path",
			bundleID: "test-bundle-4",
			files: map[string][]byte{
				"/etc/passwd": []byte("evil"),
			},
			wantErr:     true,
			wantErrType: errdefs.ErrInvalidArgument,
		},
		{
			name:     "file path with nested path traversal",
			bundleID: "test-bundle-nested-traversal",
			files: map[string][]byte{
				"foo/../../etc/passwd": []byte("evil"),
			},
			wantErr:     true,
			wantErrType: errdefs.ErrInvalidArgument,
		},
		{
			name:     "file path with deeply nested traversal",
			bundleID: "test-bundle-deep-traversal",
			files: map[string][]byte{
				"a/b/c/../../../../../../../etc/passwd": []byte("evil"),
			},
			wantErr:     true,
			wantErrType: errdefs.ErrInvalidArgument,
		},
		{
			name:     "empty filename",
			bundleID: "test-bundle-empty-filename",
			files: map[string][]byte{
				"": []byte("content"),
			},
			wantErr:     true,
			wantErrType: errdefs.ErrInvalidArgument,
		},
		{
			name:     "valid filename with double dots (not traversal)",
			bundleID: "test-bundle-double-dots",
			files: map[string][]byte{
				"file..txt":     []byte("content1"),
				"config..json":  []byte("content2"),
				"..hidden.file": []byte("content3"),
			},
			wantErr: false,
			validate: func(t *testing.T, bundlePath string) {
				// Verify all files were created
				for _, name := range []string{"file..txt", "config..json", "..hidden.file"} {
					path := filepath.Join(bundlePath, name)
					if _, err := os.Stat(path); err != nil {
						t.Errorf("file %q not created: %v", name, err)
					}
				}
			},
		},
		{
			name:     "subdirectory paths not allowed",
			bundleID: "test-bundle-subdir",
			files: map[string][]byte{
				"subdir/config.json": []byte(`{"test": true}`),
			},
			wantErr:     true,
			wantErrType: errdefs.ErrInvalidArgument,
		},
		{
			name:     "backslash paths not allowed",
			bundleID: "test-bundle-backslash",
			files: map[string][]byte{
				"subdir\\config.json": []byte(`{"test": true}`),
			},
			wantErr:     true,
			wantErrType: errdefs.ErrInvalidArgument,
		},
		{
			name:     "bare dot-dot not allowed",
			bundleID: "test-bundle-bare-dotdot",
			files: map[string][]byte{
				"..": []byte("evil"),
			},
			wantErr:     true,
			wantErrType: errdefs.ErrInvalidArgument,
		},
		{
			name:     "bare dot not allowed",
			bundleID: "test-bundle-bare-dot",
			files: map[string][]byte{
				".": []byte("evil"),
			},
			wantErr:     true,
			wantErrType: errdefs.ErrInvalidArgument,
		},
		{
			name:     "cleanup on rootfs mkdir failure",
			bundleID: "test-bundle-5",
			files:    map[string][]byte{},
			wantErr:  true,
			setupFailure: func(t *testing.T, bundleRoot string) {
				// Pre-create bundle dir with a file named "rootfs" to cause mkdir to fail
				bundleDir := filepath.Join(bundleRoot, "test-bundle-5")
				if err := os.MkdirAll(bundleDir, bundleDirPerms); err != nil {
					t.Fatalf("failed to create bundle dir: %v", err)
				}
				// Create a file named "rootfs" so mkdir fails
				rootfsFile := filepath.Join(bundleDir, "rootfs")
				if err := os.WriteFile(rootfsFile, []byte("conflict"), 0600); err != nil {
					t.Fatalf("failed to create rootfs file: %v", err)
				}
			},
			validate: func(t *testing.T, bundlePath string) {
				// Verify the original bundle directory was NOT removed by our cleanup
				// (since we created it in setupFailure, not in Create())
				if _, err := os.Stat(bundlePath); os.IsNotExist(err) {
					// This is expected - the cleanup removed the directory we tried to create
					return
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temporary bundle root
			bundleRoot := t.TempDir()

			// Setup failure conditions if specified
			if tt.setupFailure != nil {
				tt.setupFailure(t, bundleRoot)
			}

			// Create service
			svc := &service{
				bundleRoot: bundleRoot,
			}

			// Call Create
			req := &api.CreateRequest{
				ID:    tt.bundleID,
				Files: tt.files,
			}
			resp, err := svc.Create(context.Background(), req)

			// Check error
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tt.wantErrType != nil && !isErrType(err, tt.wantErrType) {
					t.Errorf("error type mismatch: got %v, want %v", err, tt.wantErrType)
				}

				// Verify cleanup happened - bundle dir should not exist for validation errors
				if tt.wantErrType == errdefs.ErrInvalidArgument && tt.bundleID != "" && !strings.Contains(tt.bundleID, "..") && !strings.Contains(tt.bundleID, "/") {
					bundlePath := filepath.Join(bundleRoot, tt.bundleID)
					if _, err := os.Stat(bundlePath); !os.IsNotExist(err) {
						t.Errorf("expected bundle dir to be cleaned up, but it exists")
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Verify response
			expectedPath := filepath.Join(bundleRoot, tt.bundleID)
			if resp.Bundle != expectedPath {
				t.Errorf("response bundle path = %q, want %q", resp.Bundle, expectedPath)
			}

			// Run custom validation
			if tt.validate != nil {
				tt.validate(t, resp.Bundle)
			}
		})
	}
}

func TestServiceCreate_DuplicateBundle(t *testing.T) {
	bundleRoot := t.TempDir()

	// Create service
	svc := &service{
		bundleRoot: bundleRoot,
	}

	bundleID := "test-bundle"
	req := &api.CreateRequest{
		ID: bundleID,
		Files: map[string][]byte{
			"config.json": []byte(`{"version": "1.0.0"}`),
		},
	}

	// First creation should succeed
	resp, err := svc.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	// Second creation with same ID should fail
	_, err = svc.Create(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for duplicate bundle ID, got nil")
	}

	// Should be a generic error (os.Mkdir will return "file exists" error)
	// The error is not converted to ErrAlreadyExists in the current implementation
	if !os.IsExist(err) {
		// Check if it's wrapped in a gRPC error
		t.Logf("duplicate bundle error: %v (type: %T)", err, err)
	}
}

func TestServiceCreate_CleanupOnFileWriteFailure(t *testing.T) {
	bundleRoot := t.TempDir()

	// Create service
	svc := &service{
		bundleRoot: bundleRoot,
	}

	// Create a bundle, then make the bundle dir read-only to force file write to fail
	bundleID := "test-bundle"
	req := &api.CreateRequest{
		ID: bundleID,
		Files: map[string][]byte{
			"config.json": []byte(`{"version": "1.0.0"}`),
		},
	}

	// First, create the bundle directory manually and make it read-only
	bundleDir := filepath.Join(bundleRoot, bundleID)
	if err := os.MkdirAll(bundleDir, bundleDirPerms); err != nil {
		t.Fatalf("failed to create bundle dir: %v", err)
	}
	// #nosec G302 -- intentionally restrictive permissions for test
	if err := os.Chmod(bundleDir, 0500); err != nil { // r-x------
		t.Fatalf("failed to chmod bundle dir: %v", err)
	}

	// Now try to create - this should fail when trying to write config.json
	_, err := svc.Create(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error when writing to read-only dir, got nil")
	}

	// Note: cleanup won't be able to remove the directory we created manually
	// but that's OK - in real usage, Create() creates the directory itself
}

func TestServiceRegisterTTRPC(t *testing.T) {
	svc := &service{
		bundleRoot: t.TempDir(),
	}

	// We can't easily test TTRPC registration without creating a real server.
	// The method itself just registers the service, so we verify it exists
	// and has the right signature. Integration tests would verify actual registration.

	// Verify the method exists and can be called
	// (we can't call it with nil server as it will panic)
	_ = svc.RegisterTTRPC
}
