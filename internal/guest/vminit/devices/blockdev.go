//go:build linux

// Package devices provides device detection and management for the VM guest.
package devices

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"
	"unsafe"

	"github.com/containerd/log"
	"golang.org/x/sys/unix"
)

const (
	// BlockDeviceTimeout is how long to wait for virtio block devices.
	// 5 seconds is sufficient for QEMU virtio device initialization.
	BlockDeviceTimeout = 5 * time.Second

	// inotifyPollInterval is how often to check context while waiting for inotify events.
	inotifyPollInterval = 100 // milliseconds
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

// inotifyEventHandler is a callback for processing inotify events.
// Returns true to stop watching (goal achieved), false to continue.
type inotifyEventHandler func(name string) (done bool)

// parseInotifyEvents parses inotify events from a buffer and calls handler for each.
// Returns true if handler signaled done for any event.
func parseInotifyEvents(buf []byte, nread int, handler inotifyEventHandler) bool {
	for offset := 0; offset < nread; {
		// Bounds check before unsafe pointer cast
		if offset+unix.SizeofInotifyEvent > nread {
			break
		}
		// #nosec G103 -- unsafe.Pointer is required to parse inotify events from the kernel
		event := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))
		nameLen := int(event.Len)
		if nameLen > 0 {
			nameEnd := offset + unix.SizeofInotifyEvent + nameLen
			if nameEnd > nread {
				break
			}
			name := string(buf[offset+unix.SizeofInotifyEvent : nameEnd])
			name = strings.TrimRight(name, "\x00")
			if handler(name) {
				return true
			}
		}
		offset += unix.SizeofInotifyEvent + nameLen
	}
	return false
}

// pollInotifyFd polls an inotify fd for events with context support.
// Returns: (hasData, shouldContinue, error)
func pollInotifyFd(ctx context.Context, fd int, timeoutMs int) (bool, bool, error) {
	select {
	case <-ctx.Done():
		return false, false, nil
	default:
	}

	pollFds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(pollFds, timeoutMs)
	if err != nil {
		if errors.Is(err, unix.EINTR) {
			return false, true, nil
		}
		return false, false, err
	}

	return n > 0, true, nil
}

// readInotifyEvents reads events from an inotify fd into buf.
// Returns: (bytesRead, shouldContinue, error)
func readInotifyEvents(fd int, buf []byte) (int, bool, error) {
	nread, err := unix.Read(fd, buf)
	if err != nil {
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			return 0, true, nil
		}
		return 0, false, err
	}
	return nread, true, nil
}

// waitForDevNodesInotify waits for device nodes to appear in /dev using inotify.
// This is more efficient than polling - the kernel notifies us when devices are created.
func waitForDevNodesInotify(ctx context.Context, devices []string, timeout time.Duration) bool {
	if len(devices) == 0 {
		return true
	}

	// Build set of devices we're waiting for
	waiting := make(map[string]bool)
	for _, dev := range devices {
		if _, err := os.Stat("/dev/" + dev); err != nil {
			waiting[dev] = true
		}
	}

	if len(waiting) == 0 {
		return true
	}

	// Create inotify instance
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		log.G(ctx).WithError(err).Debug("inotify init failed, falling back to polling")
		return waitForDevNodesPoll(ctx, devices, timeout)
	}
	defer unix.Close(fd)

	// Watch /dev for CREATE events
	if _, err = unix.InotifyAddWatch(fd, "/dev", unix.IN_CREATE); err != nil {
		log.G(ctx).WithError(err).Debug("inotify watch failed, falling back to polling")
		return waitForDevNodesPoll(ctx, devices, timeout)
	}

	// Check again after setting up watch (race condition window)
	for dev := range waiting {
		if _, err := os.Stat("/dev/" + dev); err == nil {
			delete(waiting, dev)
		}
	}
	if len(waiting) == 0 {
		return true
	}

	deadline := time.Now().Add(timeout)
	buf := make([]byte, 4096)

	for time.Now().Before(deadline) && len(waiting) > 0 {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		pollTimeout := min(int(remaining.Milliseconds()), inotifyPollInterval)
		hasData, shouldContinue, err := pollInotifyFd(ctx, fd, pollTimeout)
		if !shouldContinue {
			if err != nil {
				log.G(ctx).WithError(err).Debug("poll failed")
			}
			return false
		}
		if !hasData {
			continue
		}

		nread, shouldContinue, err := readInotifyEvents(fd, buf)
		if !shouldContinue {
			if err != nil {
				log.G(ctx).WithError(err).Debug("read inotify failed")
			}
			return false
		}
		if nread == 0 {
			continue
		}

		parseInotifyEvents(buf, nread, func(name string) bool {
			if waiting[name] {
				delete(waiting, name)
				log.G(ctx).WithField("device", name).Debug("device node created")
			}
			return false // keep watching
		})
	}

	return len(waiting) == 0
}

// waitForDevNodesPoll is a fallback polling implementation when inotify is unavailable.
func waitForDevNodesPoll(ctx context.Context, devices []string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	backoff := 5 * time.Millisecond
	maxBackoff := 50 * time.Millisecond

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		default:
		}

		allExist := true
		for _, dev := range devices {
			if _, err := os.Stat("/dev/" + dev); err != nil {
				allExist = false
				break
			}
		}

		if allExist {
			return true
		}

		time.Sleep(backoff)
		backoff = min(backoff*2, maxBackoff)
	}

	return false
}

// WaitForBlockDevices waits for virtio block devices to appear in /dev.
// Uses inotify for efficient event-driven waiting instead of polling.
// The kernel needs time to probe PCI devices and create device nodes.
// This is a best-effort operation - if devices don't appear, we continue anyway.
func WaitForBlockDevices(ctx context.Context) {
	log.G(ctx).Debug("waiting for virtio block devices to appear")

	ctx, cancel := context.WithTimeout(ctx, BlockDeviceTimeout)
	defer cancel()

	// First, wait for devices to appear in /sys/block using inotify
	vdDevices := waitForSysBlockDevices(ctx)
	if len(vdDevices) == 0 {
		log.G(ctx).Warn("no virtio block devices found in /sys/block")
		return
	}

	log.G(ctx).WithField("devices", vdDevices).Debug("found virtio block devices in /sys/block")

	// Now wait for /dev nodes to appear
	if waitForDevNodesInotify(ctx, vdDevices, BlockDeviceTimeout) {
		var devNodes []string
		for _, dev := range vdDevices {
			devNodes = append(devNodes, "/dev/"+dev)
		}
		log.G(ctx).WithField("dev_nodes", devNodes).Debug("virtio block device nodes ready")
	} else {
		log.G(ctx).Warn("timeout waiting for some device nodes, continuing anyway")
	}
}

// waitForSysBlockDevices waits for virtio block devices to appear in /sys/block using inotify.
func waitForSysBlockDevices(ctx context.Context) []string {
	// Fast path: check if devices already exist
	if devices, err := findVirtioBlockDevices(); err == nil && len(devices) > 0 {
		return devices
	}

	// Create inotify instance to watch /sys/block
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return waitForSysBlockDevicesPoll(ctx)
	}
	defer unix.Close(fd)

	if _, err = unix.InotifyAddWatch(fd, "/sys/block", unix.IN_CREATE); err != nil {
		return waitForSysBlockDevicesPoll(ctx)
	}

	// Check again after setting up watch
	if devices, err := findVirtioBlockDevices(); err == nil && len(devices) > 0 {
		return devices
	}

	buf := make([]byte, 4096)

	for {
		hasData, shouldContinue, _ := pollInotifyFd(ctx, fd, inotifyPollInterval)
		if !shouldContinue {
			devices, _ := findVirtioBlockDevices()
			return devices
		}

		if !hasData {
			// Timeout, check if any devices appeared
			if devices, err := findVirtioBlockDevices(); err == nil && len(devices) > 0 {
				return devices
			}
			continue
		}

		nread, shouldContinue, _ := readInotifyEvents(fd, buf)
		if !shouldContinue {
			break
		}
		if nread == 0 {
			continue
		}

		// Check if any event is for a virtio device
		foundVirtio := parseInotifyEvents(buf, nread, func(name string) bool {
			return strings.HasPrefix(name, "vd")
		})
		if foundVirtio {
			devices, _ := findVirtioBlockDevices()
			return devices
		}
	}

	devices, _ := findVirtioBlockDevices()
	return devices
}

// waitForSysBlockDevicesPoll is a fallback polling implementation.
func waitForSysBlockDevicesPoll(ctx context.Context) []string {
	backoff := 5 * time.Millisecond
	maxBackoff := 50 * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			devices, _ := findVirtioBlockDevices()
			return devices
		default:
		}

		devices, err := findVirtioBlockDevices()
		if err == nil && len(devices) > 0 {
			return devices
		}

		time.Sleep(backoff)
		backoff = min(backoff*2, maxBackoff)
	}
}
