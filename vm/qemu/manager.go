package qemu

import (
	"fmt"
	"os"

	"github.com/aledbf/beacon/containerd/paths"
)

// findQemu returns the path to the qemu-system-x86_64 binary
func findQemu() (string, error) {
	path := paths.QemuPath()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("qemu-system-x86_64 binary not found at %s", path)
}

// findKernel returns the path to the kernel binary for QEMU
func findKernel() (string, error) {
	path := paths.KernelPath()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("kernel not found at %s (use BEACON_SHARE_DIR to override)", path)
}

// findInitrd returns the path to the initrd for QEMU
func findInitrd() (string, error) {
	path := paths.InitrdPath()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("initrd not found at %s (use BEACON_SHARE_DIR to override)", path)
}
