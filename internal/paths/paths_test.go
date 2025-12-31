package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileExists_ResolvesSymlinks(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a real file
	realFile := filepath.Join(tmpDir, "realfile")
	if err := os.WriteFile(realFile, []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a symlink to the file
	symlinkPath := filepath.Join(tmpDir, "linkfile")
	if err := os.Symlink(realFile, symlinkPath); err != nil {
		t.Fatal(err)
	}

	// fileExists should return true for the symlink
	if !fileExists(symlinkPath) {
		t.Error("fileExists should return true for symlink to existing file")
	}

	// fileExists should return true for the real file
	if !fileExists(realFile) {
		t.Error("fileExists should return true for real file")
	}
}

func TestFileExists_FailsForBrokenSymlink(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a symlink to a non-existent target
	brokenLink := filepath.Join(tmpDir, "broken")
	if err := os.Symlink("/nonexistent/target", brokenLink); err != nil {
		t.Fatal(err)
	}

	// fileExists should return false for broken symlink
	if fileExists(brokenLink) {
		t.Error("fileExists should return false for broken symlink")
	}
}

func TestFileExists_FailsForDirectory(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a directory
	dirPath := filepath.Join(tmpDir, "testdir")
	if err := os.MkdirAll(dirPath, 0750); err != nil {
		t.Fatal(err)
	}

	// fileExists should return false for directory
	if fileExists(dirPath) {
		t.Error("fileExists should return false for directory")
	}
}

func TestDirExists_ResolvesSymlinks(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a real directory
	realDir := filepath.Join(tmpDir, "realdir")
	if err := os.MkdirAll(realDir, 0750); err != nil {
		t.Fatal(err)
	}

	// Create a symlink to the directory
	symlinkPath := filepath.Join(tmpDir, "linkdir")
	if err := os.Symlink(realDir, symlinkPath); err != nil {
		t.Fatal(err)
	}

	// dirExists should return true for the symlink
	if !dirExists(symlinkPath) {
		t.Error("dirExists should return true for symlink to existing directory")
	}

	// dirExists should return true for the real directory
	if !dirExists(realDir) {
		t.Error("dirExists should return true for real directory")
	}
}

func TestDirExists_FailsForBrokenSymlink(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a symlink to a non-existent target
	brokenLink := filepath.Join(tmpDir, "broken")
	if err := os.Symlink("/nonexistent/target", brokenLink); err != nil {
		t.Fatal(err)
	}

	// dirExists should return false for broken symlink
	if dirExists(brokenLink) {
		t.Error("dirExists should return false for broken symlink")
	}
}

func TestDirExists_FailsForFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a file
	filePath := filepath.Join(tmpDir, "testfile")
	if err := os.WriteFile(filePath, []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}

	// dirExists should return false for file
	if dirExists(filePath) {
		t.Error("dirExists should return false for file")
	}
}

func TestDirExists_SymlinkToFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a file
	realFile := filepath.Join(tmpDir, "realfile")
	if err := os.WriteFile(realFile, []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a symlink to the file (named like a directory)
	symlinkPath := filepath.Join(tmpDir, "fakedir")
	if err := os.Symlink(realFile, symlinkPath); err != nil {
		t.Fatal(err)
	}

	// dirExists should return false because target is a file, not a directory
	if dirExists(symlinkPath) {
		t.Error("dirExists should return false for symlink pointing to a file")
	}
}
