//go:build linux

package cni

import (
	"fmt"
	"strings"

	"github.com/containerd/log"
	current "github.com/containernetworking/cni/pkg/types/100"
)

// ExtractTAPDevice extracts the TAP device name from a CNI result.
//
// This function handles the tc-redirect-tap plugin case, which creates
// a TAP device with a "tap" prefix and reports it in the CNI result.
func ExtractTAPDevice(result *current.Result) (string, error) {
	tapDevice, _, err := ExtractTAPDeviceInfo(result)
	return tapDevice, err
}

// ExtractTAPDeviceInfo extracts the TAP device name and MAC from a CNI result.
func ExtractTAPDeviceInfo(result *current.Result) (string, string, error) {
	if result == nil {
		return "", "", fmt.Errorf("CNI result is nil")
	}

	log.L.WithField("count", len(result.Interfaces)).Debug("CNI result interface count")

	// Detect TAP device reported by tc-redirect-tap.
	tapDevice, tapMAC, err := detectTCRedirectTAP(result)
	if err == nil {
		log.L.WithFields(log.Fields{
			"tap": tapDevice,
			"mac": tapMAC,
		}).Debug("Found TAP device via tc-redirect-tap detection")
		return tapDevice, tapMAC, nil
	}

	return "", "", fmt.Errorf("no TAP device found in CNI result (checked %d interfaces)", len(result.Interfaces))
}

// detectTCRedirectTAP detects TAP devices created by the tc-redirect-tap CNI plugin.
//
// The tc-redirect-tap plugin creates TAP devices with predictable naming:
// - Usually named "tap0" or "tapXXX"
// - The TAP device is created inside the container netns (sandbox)
// - spinbox opens the TAP inside that netns and passes the FD to QEMU
//
// To avoid false positives (e.g., interfaces like "tape0", "taproot"), we
// validate that the interface is in a sandbox (container netns).
func detectTCRedirectTAP(result *current.Result) (string, string, error) {
	// Look for TAP devices in the CNI result
	for _, iface := range result.Interfaces {
		// Check if this looks like a TAP device by name
		if strings.HasPrefix(iface.Name, "tap") {
			log.L.WithFields(log.Fields{
				"name":    iface.Name,
				"sandbox": iface.Sandbox,
				"mac":     iface.Mac,
			}).Debug("tap-prefixed interface candidate")

			// Validate that the interface is in a sandbox (container netns).
			// TAP devices created by tc-redirect-tap for VM use will have
			// a non-empty Sandbox field pointing to the netns path.
			// This prevents false positives from interfaces like "tape0" or "taproot".
			if iface.Sandbox == "" {
				log.L.WithField("name", iface.Name).Debug("skipping tap-prefixed interface not in sandbox")
				continue
			}

			log.L.WithFields(log.Fields{
				"name":    iface.Name,
				"sandbox": iface.Sandbox,
				"mac":     iface.Mac,
			}).Info("Found TAP device in sandbox")

			// The TAP device is in the container netns.
			// Return the name so the host can open it in that netns.
			return iface.Name, iface.Mac, nil
		}
	}

	return "", "", fmt.Errorf("no tc-redirect-tap device found in sandbox")
}
