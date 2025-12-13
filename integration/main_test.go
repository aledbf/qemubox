//go:build linux

package integration

import (
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/aledbf/beacon/containerd/vm"
	"github.com/aledbf/beacon/containerd/vm/qemu"
)

func TestMain(m *testing.M) {
	var err error

	absPath, err := filepath.Abs("../build")
	if err != nil {
		log.Fatalf("Failed to resolve build path: %v", err)
	}
	if err := os.Setenv("PATH", absPath+":"+os.Getenv("PATH")); err != nil {
		log.Fatalf("Failed to set PATH environment variable: %v", err)
	}

	r := m.Run()

	os.Exit(r)
}

func runWithVM(t *testing.T, runTest func(*testing.T, vm.Instance)) {
	t.Helper()

	instance, err := qemu.NewInstance(t.Context(), t.Name(), t.TempDir(), nil)
	if err != nil {
		t.Fatal("Failed to create VM instance:", err)
	}

	if err := instance.Start(t.Context()); err != nil {
		t.Fatal("Failed to start VM instance:", err)
	}

	t.Cleanup(func() {
		instance.Shutdown(t.Context())
	})

	runTest(t, instance)
}
