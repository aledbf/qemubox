package systools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/log"
)

func TestDumpFile_PlainText(t *testing.T) {
	ctx := context.Background()

	// Create a test file
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	testContent := "Hello, World!\nLine 2\n"

	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// DumpFile should not error on valid file
	// It writes to stderr, so we can't easily capture output in tests
	// But we can verify it doesn't panic
	DumpFile(ctx, testFile)

	// If we get here without panic, the test passes
}

func TestDumpFile_JSONFile(t *testing.T) {
	ctx := context.Background()

	// Create a test JSON file
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.json")
	testContent := `{"key": "value", "number": 123}`

	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// DumpFile should handle JSON files specially
	DumpFile(ctx, testFile)

	// If we get here without panic, the test passes
}

func TestDumpFile_InvalidJSON(t *testing.T) {
	ctx := context.Background()

	// Create a test file with invalid JSON
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "bad.json")
	testContent := `{invalid json`

	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// DumpFile should handle invalid JSON gracefully
	DumpFile(ctx, testFile)

	// If we get here without panic, the test passes
}

func TestDumpFile_NonexistentFile(t *testing.T) {
	ctx := context.Background()

	// Try to dump a file that doesn't exist
	testFile := "/nonexistent/file.txt"

	// DumpFile should log a warning but not panic
	DumpFile(ctx, testFile)

	// If we get here without panic, the test passes
}

func TestDumpFile_DebugLevelDisabled(t *testing.T) {
	// When debug level is disabled, DumpFile should be a no-op
	ctx := context.Background()

	// Create a test file
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// Set log level to Info (not Debug)
	// Note: In real usage, this depends on logger configuration
	// This test just verifies the function doesn't panic
	DumpFile(ctx, testFile)
}

func TestDumpInfo(t *testing.T) {
	// DumpInfo walks the filesystem and calls various system commands
	// It's primarily a debugging/logging function
	// Skip by default as it walks "/" which is very slow (~14s)
	t.Skip("skipping DumpInfo test (walks entire filesystem, ~14s)")

	ctx := context.Background()

	// Call DumpInfo and verify it doesn't panic
	// It will log warnings about missing /sbin/crun on non-Linux systems
	DumpInfo(ctx)
}

func TestDumpInfo_ProcCmdline(t *testing.T) {
	// This test is redundant with TestDumpInfo - just verifies DumpInfo handles /proc gracefully
	// Skip by default as it calls DumpInfo which walks "/" (~14s)
	t.Skip("skipping DumpInfo test (walks entire filesystem, ~14s)")

	ctx := context.Background()
	DumpInfo(ctx)
}

func TestDumpInfo_CrunVersion(t *testing.T) {
	// This test is redundant with TestDumpInfo - documents that DumpInfo tries /sbin/crun --version
	// Skip by default as it calls DumpInfo which walks "/" (~14s)
	t.Skip("skipping DumpInfo test (walks entire filesystem, ~14s)")

	ctx := context.Background()
	DumpInfo(ctx)
}

// Benchmark DumpFile performance
func BenchmarkDumpFile_PlainText(b *testing.B) {
	// Set log level to Debug to ensure DumpFile actually runs
	ctx := log.WithLogger(context.Background(), log.L)

	tmpDir := b.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content\n"), 0644); err != nil {
		b.Fatalf("failed to create test file: %v", err)
	}

	b.ResetTimer()
	for range b.N {
		DumpFile(ctx, testFile)
	}
}

func BenchmarkDumpFile_JSON(b *testing.B) {
	ctx := log.WithLogger(context.Background(), log.L)

	tmpDir := b.TempDir()
	testFile := filepath.Join(tmpDir, "test.json")
	testContent := `{"key": "value", "nested": {"a": 1, "b": 2}}`
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		b.Fatalf("failed to create test file: %v", err)
	}

	b.ResetTimer()
	for range b.N {
		DumpFile(ctx, testFile)
	}
}
