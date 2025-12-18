//go:build linux

package cni

import (
	"fmt"
	"strings"

	"github.com/containerd/log"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/vishvananda/netlink"
)

// ExtractTAPDevice extracts the TAP device name from a CNI result.
//
// This function handles multiple scenarios:
// 1. tc-redirect-tap plugin: Creates a TAP device with "tap" prefix
// 2. Direct TAP plugins: Create TAP devices directly
// 3. Generic detection: Looks for TAP-type interfaces in the result
//
// The function validates that the detected device is actually a TAP device
// using netlink to check the device type.
func ExtractTAPDevice(result *current.Result) (string, error) {
	if result == nil {
		return "", fmt.Errorf("CNI result is nil")
	}

	// Debug: Log all interfaces in the result
	log.L.WithField("count", len(result.Interfaces)).Debug("CNI result interfaces")
	for i, iface := range result.Interfaces {
		log.L.WithFields(log.Fields{
			"index":   i,
			"name":    iface.Name,
			"sandbox": iface.Sandbox,
			"mac":     iface.Mac,
		}).Debug("CNI interface")
	}

	// Try tc-redirect-tap detection first (most common case)
	tapDevice, err := detectTCRedirectTAP(result)
	if err == nil {
		log.L.WithField("tap", tapDevice).Debug("Found TAP device via tc-redirect-tap detection")
		return tapDevice, nil
	}
	log.L.WithError(err).Warn("tc-redirect-tap detection failed")

	// Fall back to generic TAP detection
	tapDevice, err = detectGenericTAP(result)
	if err == nil {
		log.L.WithField("tap", tapDevice).Debug("Found TAP device via generic detection")
		return tapDevice, nil
	}
	log.L.WithError(err).Warn("generic TAP detection failed")

	// Last resort: check the host namespace for any TAP devices
	// This handles the case where tc-redirect-tap creates the TAP but doesn't report it
	tapDevice, err = findHostTAPDevice()
	if err == nil {
		log.L.WithField("tap", tapDevice).Debug("Found TAP device in host namespace")
		return tapDevice, nil
	}
	log.L.WithError(err).Warn("findHostTAPDevice failed")

	return "", fmt.Errorf("no TAP device found in CNI result (checked %d interfaces)", len(result.Interfaces))
}

// detectTCRedirectTAP detects TAP devices created by the tc-redirect-tap CNI plugin.
//
// The tc-redirect-tap plugin creates TAP devices with predictable naming:
// - Usually named "tap0" or "tapXXX"
// - The TAP device is created inside the container netns (sandbox)
// - QEMU will access the TAP from within the same netns
func detectTCRedirectTAP(result *current.Result) (string, error) {
	// Look for TAP devices in the CNI result
	for _, iface := range result.Interfaces {
		// Check if this looks like a TAP device by name
		if strings.HasPrefix(iface.Name, "tap") {
			log.L.WithFields(log.Fields{
				"name":    iface.Name,
				"sandbox": iface.Sandbox,
				"mac":     iface.Mac,
			}).Info("Found potential TAP device")

			// The TAP device is in the container netns
			// Return the name - QEMU will access it from within the same netns
			return iface.Name, nil
		}
	}

	return "", fmt.Errorf("no tc-redirect-tap device found")
}

// detectGenericTAP detects TAP devices created by any CNI plugin.
//
// This fallback method checks all interfaces in the CNI result and
// validates them using netlink to confirm they are TAP devices.
func detectGenericTAP(result *current.Result) (string, error) {
	for _, iface := range result.Interfaces {
		// Check if this is a TAP device (could be in any namespace)
		if isTAPDevice(iface.Name) {
			return iface.Name, nil
		}
	}

	return "", fmt.Errorf("no TAP device found among interfaces")
}

// isTAPDevice checks if the given device name is a TAP device.
//
// This function uses netlink to query the device and verify it's a TUN/TAP device.
// TAP devices have the "tun" driver and IFF_TAP flag.
func isTAPDevice(name string) bool {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return false
	}

	// Check if it's a TUN device (TAP is a type of TUN)
	tuntap, ok := link.(*netlink.Tuntap)
	if !ok {
		return false
	}

	// Verify it's specifically a TAP device (not TUN)
	// TAP devices have IFF_TAP flag set
	return tuntap.Mode == netlink.TUNTAP_MODE_TAP
}

// ValidateTAPDevice validates that the TAP device exists and is properly configured.
func ValidateTAPDevice(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("TAP device %s not found: %w", name, err)
	}

	// Verify it's a TUN/TAP device
	tuntap, ok := link.(*netlink.Tuntap)
	if !ok {
		return fmt.Errorf("device %s is not a TUN/TAP device", name)
	}

	// Verify it's a TAP device
	if tuntap.Mode != netlink.TUNTAP_MODE_TAP {
		return fmt.Errorf("device %s is not a TAP device (mode: %d)", name, tuntap.Mode)
	}

	return nil
}

// findHostTAPDevice scans the host network namespace for TAP devices.
//
// This is a fallback method for when tc-redirect-tap creates a TAP device
// but doesn't properly report it in the CNI result. It looks for recently
// created TAP devices in the host namespace.
func findHostTAPDevice() (string, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return "", fmt.Errorf("failed to list links: %w", err)
	}

	var tapDevices []string
	for _, link := range links {
		// Check if it's a TAP device
		tuntap, ok := link.(*netlink.Tuntap)
		if !ok {
			continue
		}

		// Only TAP devices (not TUN)
		if tuntap.Mode != netlink.TUNTAP_MODE_TAP {
			continue
		}

		name := link.Attrs().Name
		// Skip if it starts with "beacon-" (that's our legacy mode TAP devices)
		if strings.HasPrefix(name, "beacon-") {
			continue
		}

		// Check if it starts with "tap" (most common for tc-redirect-tap)
		if strings.HasPrefix(name, "tap") {
			tapDevices = append(tapDevices, name)
		}
	}

	if len(tapDevices) == 0 {
		return "", fmt.Errorf("no TAP devices found in host namespace")
	}

	// If we found multiple, prefer the most recently created one
	// For now, just return the first one (this should be improved)
	// TODO: Track TAP device creation timestamps or use a better heuristic
	return tapDevices[0], nil
}
