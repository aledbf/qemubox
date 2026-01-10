//go:build linux && integration

package integration

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
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

	// Clean up any orphaned containers from previous test runs
	cleanupOrphanedContainers()

	os.Exit(m.Run())
}

// cleanupOrphanedContainers removes any leftover qbx-* containers from previous failed runs.
func cleanupOrphanedContainers() {
	cfg := loadTestConfig()
	ctrBin := "/usr/share/spinbox/bin/ctr"

	// List all tasks
	cmd := exec.Command(ctrBin, "--address", cfg.Socket, "--namespace", cfg.Namespace, "tasks", "list")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return // ctr not available or containerd not running
	}

	// Parse output and find qbx-* containers
	lines := strings.Split(out.String(), "\n")
	for _, line := range lines[1:] { // Skip header
		fields := strings.Fields(line)
		if len(fields) < 1 {
			continue
		}
		containerID := fields[0]
		if !strings.HasPrefix(containerID, "qbx-") {
			continue
		}

		log.Printf("cleaning up orphaned container: %s", containerID)

		// Kill task
		_ = exec.Command(ctrBin, "--address", cfg.Socket, "--namespace", cfg.Namespace,
			"tasks", "kill", "--signal", "SIGKILL", containerID).Run()

		// Wait briefly for task to stop
		for range 5 {
			checkCmd := exec.Command(ctrBin, "--address", cfg.Socket, "--namespace", cfg.Namespace, "tasks", "list")
			var checkOut bytes.Buffer
			checkCmd.Stdout = &checkOut
			if err := checkCmd.Run(); err != nil {
				break
			}
			if !strings.Contains(checkOut.String(), containerID) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		// Delete task
		_ = exec.Command(ctrBin, "--address", cfg.Socket, "--namespace", cfg.Namespace,
			"tasks", "delete", containerID).Run()

		// Delete container
		_ = exec.Command(ctrBin, "--address", cfg.Socket, "--namespace", cfg.Namespace,
			"containers", "delete", containerID).Run()
	}
}

// haveKVM checks if KVM is available for VM tests.
func haveKVM() bool {
	_, err := os.Stat("/dev/kvm")
	return err == nil
}

// haveQEMU checks if QEMU binary is available.
func haveQEMU() bool {
	// Check in standard PATH and spinbox share directory
	if _, err := exec.LookPath("qemu-system-x86_64"); err == nil {
		return true
	}
	if _, err := os.Stat("/usr/share/spinbox/bin/qemu-system-x86_64"); err == nil {
		return true
	}
	return false
}
