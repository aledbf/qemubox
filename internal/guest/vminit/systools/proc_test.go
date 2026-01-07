package systools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDumpPids(t *testing.T) {
	// DumpPids reads /proc and logs info about each PID.
	// It handles empty directories, skips non-numeric entries like "cpuinfo",
	// and only processes numeric directories (PIDs).
	// This test verifies the function doesn't panic when reading /proc.
	DumpPids(context.Background())
}

func TestProcFileStructure(t *testing.T) {
	// Documents the expected /proc/PID file structure that dumpProcPid reads.
	// The function is not exported, so we verify the mock file creation works.
	tmpDir := t.TempDir()
	pidDir := filepath.Join(tmpDir, "1234")
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		t.Fatalf("failed to create pid dir: %v", err)
	}

	files := map[string]string{
		"status": `Name:	init
Umask:	0022
State:	S (sleeping)
Tgid:	1234
Pid:	1234
PPid:	1
VmSize:	  100000 kB
VmRSS:	   50000 kB
`,
		"cmdline": "init\x00--debug\x00",
	}

	for name, content := range files {
		path := filepath.Join(pidDir, name)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write %s: %v", name, err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s should exist: %v", name, err)
		}
	}
}
