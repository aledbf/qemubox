package systools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/log"
)

func TestDumpFile(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, tmpDir string) string
	}{
		{
			name: "plain text file",
			setup: func(t *testing.T, tmpDir string) string {
				path := filepath.Join(tmpDir, "test.txt")
				if err := os.WriteFile(path, []byte("Hello, World!\nLine 2\n"), 0644); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "JSON file",
			setup: func(t *testing.T, tmpDir string) string {
				path := filepath.Join(tmpDir, "test.json")
				if err := os.WriteFile(path, []byte(`{"key": "value", "number": 123}`), 0644); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "invalid JSON file",
			setup: func(t *testing.T, tmpDir string) string {
				path := filepath.Join(tmpDir, "bad.json")
				if err := os.WriteFile(path, []byte(`{invalid json`), 0644); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "non-existent file",
			setup: func(t *testing.T, _ string) string {
				return "/nonexistent/file.txt"
			},
		},
		{
			name: "debug level disabled",
			setup: func(t *testing.T, tmpDir string) string {
				path := filepath.Join(tmpDir, "debug.txt")
				if err := os.WriteFile(path, []byte("content"), 0644); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			path := tt.setup(t, tmpDir)

			// DumpFile should not panic for any input
			DumpFile(context.Background(), path)
		})
	}
}

func TestDumpInfo(t *testing.T) {
	// DumpInfo walks the filesystem and calls various system commands.
	// Skip by default as it walks "/" which is very slow (~14s).
	// Covers: /proc/cmdline access, /sbin/crun --version, filesystem traversal.
	t.Skip("skipping DumpInfo test (walks entire filesystem, ~14s)")

	DumpInfo(context.Background())
}

// Benchmark DumpFile performance
func BenchmarkDumpFile_PlainText(b *testing.B) {
	ctx := log.WithLogger(context.Background(), log.L)

	tmpDir := b.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content\n"), 0644); err != nil {
		b.Fatalf("failed to create test file: %v", err)
	}

	for b.Loop() {
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

	for b.Loop() {
		DumpFile(ctx, testFile)
	}
}
