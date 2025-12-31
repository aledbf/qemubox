//go:build linux

// Package devices provides device detection and management for the VM guest.
package devices

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/containerd/log"
)

const (
	// BlockDeviceTimeout is how long to wait for virtio block devices.
	// 5 seconds is sufficient for QEMU virtio device initialization.
	BlockDeviceTimeout = 5 * time.Second

	// BlockDevicePollInterval is the polling frequency for device detection.
	BlockDevicePollInterval = 10 * time.Millisecond

	// maxDeviceNodeRetries is how many times to check for /dev nodes.
	maxDeviceNodeRetries = 10
)

// findVirtioBlockDevices finds virtio block devices in /sys/block.
func findVirtioBlockDevices() ([]string, error) {
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil, err
	}

	var devices []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "vd") {
			devices = append(devices, entry.Name())
		}
	}
	return devices, nil
}

// waitForDevNodes polls for device nodes to appear in /dev.
// Returns true if all device nodes are ready, false otherwise.
func waitForDevNodes(ctx context.Context, devices []string) bool {
	for range maxDeviceNodeRetries {
		var devNodes []string
		for _, dev := range devices {
			devPath := "/dev/" + dev
			if _, err := os.Stat(devPath); err == nil {
				devNodes = append(devNodes, devPath)
			}
		}

		if len(devNodes) == len(devices) {
			log.G(ctx).WithField("dev_nodes", devNodes).Info("virtio block device nodes ready")
			return true
		}

		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// WaitForBlockDevices waits for virtio block devices to appear in /dev.
// The kernel needs time to probe PCI devices and create device nodes.
// This is a best-effort operation - if devices don't appear, we continue anyway.
func WaitForBlockDevices(ctx context.Context) {
	timeout := BlockDeviceTimeout
	pollInterval := BlockDevicePollInterval

	log.G(ctx).Debug("waiting for virtio block devices to appear")

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		// Check /sys/block for all block devices
		vdDevices, err := findVirtioBlockDevices()
		if err == nil && len(vdDevices) > 0 {
			log.G(ctx).WithField("devices", vdDevices).Info("found virtio block devices in /sys/block")

			// Poll for /dev nodes to appear
			// udev may need time to create device nodes after kernel detection
			if waitForDevNodes(ctx, vdDevices) {
				return
			}
		}

		select {
		case <-ctx.Done():
			log.G(ctx).Warn("timeout waiting for virtio block devices, continuing anyway")
			return
		case <-ticker.C:
			// continue polling
		}
	}
}
