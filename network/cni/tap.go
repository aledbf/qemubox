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

	log.L.WithField("count", len(result.Interfaces)).Debug("CNI result interfaces")

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
// - QEMU will access the TAP from within the same netns
func detectTCRedirectTAP(result *current.Result) (string, string, error) {
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
			return iface.Name, iface.Mac, nil
		}
	}

	return "", "", fmt.Errorf("no tc-redirect-tap device found")
}
