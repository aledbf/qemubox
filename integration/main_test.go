//go:build linux && integration

package integration

import (
	"log"
	"os"
	"os/exec"
	"testing"
)

func TestMain(m *testing.M) {
	// Check all prerequisites before running any tests
	if !haveKVM() {
		log.Print("skipping integration tests: /dev/kvm not available")
		os.Exit(0)
	}

	if !haveQEMU() {
		log.Print("skipping integration tests: qemu-system-x86_64 not found")
		os.Exit(0)
	}

	os.Exit(m.Run())
}

// haveKVM checks if KVM is available for VM tests.
func haveKVM() bool {
	_, err := os.Stat("/dev/kvm")
	return err == nil
}

// haveQEMU checks if QEMU binary is available.
func haveQEMU() bool {
	// Check in standard PATH and qemubox share directory
	if _, err := exec.LookPath("qemu-system-x86_64"); err == nil {
		return true
	}
	if _, err := os.Stat("/usr/share/qemubox/bin/qemu-system-x86_64"); err == nil {
		return true
	}
	return false
}
