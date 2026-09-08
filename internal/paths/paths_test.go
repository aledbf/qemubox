package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spin-stack/spinbox/internal/config"
)

// release writes a share directory holding a whole spin-machine release.
func release(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{
		"bin/qemu-system-x86_64",
		"bin/qemu-img",
		"kernel/vmlinux",
		"qemu/pvh.bin",
	} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestMachineNamesWhatIsMissing(t *testing.T) {
	// A share directory that is not a release is the case worth having a message
	// for: it is what a host looks like before `task machine` has ever run, and
	// what it looks like after a half-finished upgrade.
	_, err := Machine(config.PathsConfig{ShareDir: t.TempDir()})
	if err == nil {
		t.Fatal("Machine accepted a directory with no release in it")
	}
	if !strings.Contains(err.Error(), "task machine") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}

func TestMachinePaths(t *testing.T) {
	dir := release(t)
	cfg := config.PathsConfig{ShareDir: dir}

	rel, err := Machine(cfg)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ name, got, want string }{
		{"QemuPath", QemuPath(cfg, rel), filepath.Join(dir, "bin/qemu-system-x86_64")},
		{"QemuSharePath", QemuSharePath(cfg, rel), filepath.Join(dir, "qemu")},
		{"Kernel", rel.Kernel(), filepath.Join(dir, "kernel/vmlinux")},
		{"InitrdPath", InitrdPath(cfg), filepath.Join(dir, "kernel/spinbox-initrd")},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestExplicitPathsWin(t *testing.T) {
	// Somebody who names a QEMU means it. The release still has to be whole —
	// an override is not a way to run half a machine — which is why Machine is
	// called first and these are applied to its answer.
	dir := release(t)
	cfg := config.PathsConfig{
		ShareDir:      dir,
		QEMUPath:      "/opt/qemu/bin/qemu-system-x86_64",
		QEMUSharePath: "/opt/qemu/share",
	}

	rel, err := Machine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := QemuPath(cfg, rel); got != cfg.QEMUPath {
		t.Errorf("QemuPath = %q, want the override %q", got, cfg.QEMUPath)
	}
	if got := QemuSharePath(cfg, rel); got != cfg.QEMUSharePath {
		t.Errorf("QemuSharePath = %q, want the override %q", got, cfg.QEMUSharePath)
	}
}
