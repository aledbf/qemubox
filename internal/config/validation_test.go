//go:build linux

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalizePath_CleansDotDot(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a subdirectory
	subDir := filepath.Join(tmpDir, "subdir")
	if err := os.MkdirAll(subDir, 0750); err != nil {
		t.Fatal(err)
	}

	// Test path with .. that should be cleaned
	pathWithDotDot := filepath.Join(subDir, "..", "subdir")
	canonical, err := canonicalizePath(pathWithDotDot)
	if err != nil {
		t.Fatalf("canonicalizePath failed: %v", err)
	}

	// Should resolve to the real subdir path
	if canonical != subDir {
		t.Errorf("expected %s, got %s", subDir, canonical)
	}
}

func TestCanonicalizePath_ResolvesSymlinks(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a real directory
	realDir := filepath.Join(tmpDir, "realdir")
	if err := os.MkdirAll(realDir, 0750); err != nil {
		t.Fatal(err)
	}

	// Create a symlink to the real directory
	symlinkPath := filepath.Join(tmpDir, "linkdir")
	if err := os.Symlink(realDir, symlinkPath); err != nil {
		t.Fatal(err)
	}

	// canonicalizePath should resolve the symlink
	canonical, err := canonicalizePath(symlinkPath)
	if err != nil {
		t.Fatalf("canonicalizePath failed: %v", err)
	}

	if canonical != realDir {
		t.Errorf("expected symlink to resolve to %s, got %s", realDir, canonical)
	}
}

func TestCanonicalizePath_HandlesNonExistentPath(t *testing.T) {
	tmpDir := t.TempDir()

	// Path that doesn't exist yet
	nonExistent := filepath.Join(tmpDir, "does", "not", "exist")
	canonical, err := canonicalizePath(nonExistent)
	if err != nil {
		t.Fatalf("canonicalizePath failed for non-existent path: %v", err)
	}

	// Should return a cleaned path based on existing parent
	if !strings.HasPrefix(canonical, tmpDir) {
		t.Errorf("expected path to start with %s, got %s", tmpDir, canonical)
	}
}

func TestCanonicalizePath_SymlinkEscapeAttempt(t *testing.T) {
	tmpDir := t.TempDir()

	// Create two directories - one "safe" and one "unsafe"
	safeDir := filepath.Join(tmpDir, "safe")
	unsafeDir := filepath.Join(tmpDir, "unsafe")
	if err := os.MkdirAll(safeDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(unsafeDir, 0750); err != nil {
		t.Fatal(err)
	}

	// Create a symlink inside safe that points to unsafe
	escapeLink := filepath.Join(safeDir, "escape")
	if err := os.Symlink(unsafeDir, escapeLink); err != nil {
		t.Fatal(err)
	}

	// canonicalizePath should resolve to the unsafe directory
	canonical, err := canonicalizePath(escapeLink)
	if err != nil {
		t.Fatalf("canonicalizePath failed: %v", err)
	}

	// The canonical path should be the unsafeDir, exposing the escape attempt
	if canonical != unsafeDir {
		t.Errorf("expected symlink to resolve to %s, got %s", unsafeDir, canonical)
	}

	// This test demonstrates that canonicalization reveals the true target
	// Callers should then validate the resolved path is within allowed boundaries
}

func TestValidateDirectoryExists_AllowsSymlinkTarget(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a target directory outside "allowed" area
	targetDir := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(targetDir, 0750); err != nil {
		t.Fatal(err)
	}

	// Create a symlink that appears to be in safe area
	safeArea := filepath.Join(tmpDir, "safe")
	if err := os.MkdirAll(safeArea, 0750); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(safeArea, "sneaky")
	if err := os.Symlink(targetDir, symlinkPath); err != nil {
		t.Fatal(err)
	}

	// validateDirectoryExists should succeed (the target exists)
	// but the error message should show the resolved path
	err := validateDirectoryExists(symlinkPath, "test_field")
	if err != nil {
		t.Logf("Error (expected none): %v", err)
		t.Fatal("validateDirectoryExists should succeed for valid symlink target")
	}

	// The key security benefit: error messages and logs show the real path
	// Administrators can detect symlink-based configuration manipulation
}

func TestEnsureDirectoryWritable_CreatesAtCanonicalPath(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a symlink to a directory
	realDir := filepath.Join(tmpDir, "realdir")
	if err := os.MkdirAll(realDir, 0750); err != nil {
		t.Fatal(err)
	}

	symlinkPath := filepath.Join(tmpDir, "linkdir")
	if err := os.Symlink(realDir, symlinkPath); err != nil {
		t.Fatal(err)
	}

	// Request a new subdirectory via the symlink
	newSubDir := filepath.Join(symlinkPath, "newsubdir")

	err := ensureDirectoryWritable(newSubDir, "test_field")
	if err != nil {
		t.Fatalf("ensureDirectoryWritable failed: %v", err)
	}

	// The directory should be created at the canonical location
	expectedRealPath := filepath.Join(realDir, "newsubdir")
	info, err := os.Stat(expectedRealPath)
	if err != nil {
		t.Fatalf("directory not created at canonical path %s: %v", expectedRealPath, err)
	}
	if !info.IsDir() {
		t.Errorf("expected directory at %s", expectedRealPath)
	}
}

func TestValidateExecutable_ResolvesSymlinks(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a real executable
	realExe := filepath.Join(tmpDir, "realexe")
	if err := os.WriteFile(realExe, []byte("#!/bin/sh\n"), 0750); err != nil {
		t.Fatal(err)
	}

	// Create a symlink to the executable
	symlinkPath := filepath.Join(tmpDir, "linkexe")
	if err := os.Symlink(realExe, symlinkPath); err != nil {
		t.Fatal(err)
	}

	// validateExecutable should succeed for the symlink
	err := validateExecutable(symlinkPath, "test_exe")
	if err != nil {
		t.Errorf("validateExecutable failed for symlink: %v", err)
	}
}

func TestValidateExecutable_FailsForBrokenSymlink(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a symlink to a non-existent target
	brokenLink := filepath.Join(tmpDir, "broken")
	if err := os.Symlink("/nonexistent/target", brokenLink); err != nil {
		t.Fatal(err)
	}

	err := validateExecutable(brokenLink, "test_exe")
	if err == nil {
		t.Error("validateExecutable should fail for broken symlink")
	}
}
