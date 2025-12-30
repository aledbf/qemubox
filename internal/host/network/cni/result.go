//go:build linux

package cni

import (
	"fmt"
	"net"
	"runtime"

	"github.com/containerd/log"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// CNIResult contains the parsed result from CNI plugin execution.
type CNIResult struct {
	// TAPDevice is the name of the TAP device created by CNI plugins.
	TAPDevice string
	// TAPMAC is the MAC address reported for the TAP device (if provided by CNI).
	TAPMAC string

	// IPAddress is the IP address allocated to the VM.
	IPAddress net.IP

	// Netmask is the network mask for the allocated IP address.
	Netmask string

	// Gateway is the gateway IP address for the network.
	Gateway net.IP
}

// ParseCNIResult parses a CNI result and extracts networking information.
//
// This function:
// 1. Extracts the TAP device from the CNI result interfaces
// 2. Parses IP address and gateway information
func ParseCNIResult(result *current.Result) (*CNIResult, error) {
	return ParseCNIResultWithNetNS(result, "")
}

// ParseCNIResultWithNetNS parses a CNI result and extracts networking information.
// If the TAP MAC is missing from the result, it attempts to read it from the
// TAP device inside the provided network namespace.
func ParseCNIResultWithNetNS(result *current.Result, netnsPath string) (*CNIResult, error) {
	if result == nil {
		return nil, fmt.Errorf("CNI result is nil")
	}

	// Extract TAP device
	tapDevice, tapMAC, err := ExtractTAPDeviceInfo(result)
	if err != nil {
		return nil, fmt.Errorf("failed to extract TAP device: %w", err)
	}
	if tapMAC == "" {
		if netnsPath == "" {
			return nil, fmt.Errorf("TAP device %q has empty MAC and no netns provided", tapDevice)
		}
		resolvedMAC, err := readInterfaceMAC(netnsPath, tapDevice)
		if err != nil {
			return nil, fmt.Errorf("failed to read TAP MAC from netns: %w", err)
		}
		tapMAC = resolvedMAC
	}

	// Parse IP address, netmask, and gateway
	if len(result.IPs) == 0 {
		return nil, fmt.Errorf("CNI result contains no IP addresses")
	}

	// Use the first IP configuration
	ipConfig := result.IPs[0]
	ipAddress := ipConfig.Address.IP
	gateway := ipConfig.Gateway

	// Extract netmask from the IPNet
	var netmask string
	if ipConfig.Address.Mask != nil {
		netmask = net.IP(ipConfig.Address.Mask).String()
	}

	return &CNIResult{
		TAPDevice: tapDevice,
		TAPMAC:    tapMAC,
		IPAddress: ipAddress,
		Netmask:   netmask,
		Gateway:   gateway,
	}, nil
}

func readInterfaceMAC(netnsPath, ifName string) (string, error) {
	// Get current namespace first so it closes last (LIFO order)
	origNS, err := netns.Get()
	if err != nil {
		return "", fmt.Errorf("get current netns: %w", err)
	}
	defer origNS.Close() // Closes last (LIFO)

	targetNS, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return "", fmt.Errorf("get target netns: %w", err)
	}
	defer targetNS.Close() // Closes first (LIFO)

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := netns.Set(targetNS); err != nil {
		return "", fmt.Errorf("set target netns: %w", err)
	}
	defer func() {
		if err := netns.Set(origNS); err != nil {
			log.L.WithError(err).Error("failed to restore original netns")
		}
	}()

	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return "", fmt.Errorf("lookup interface %s: %w", ifName, err)
	}
	mac := link.Attrs().HardwareAddr
	if len(mac) == 0 {
		return "", fmt.Errorf("interface %q has empty MAC", ifName)
	}
	return mac.String(), nil
}
