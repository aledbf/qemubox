//go:build linux

package cni

import (
	"fmt"
	"net"

	current "github.com/containernetworking/cni/pkg/types/100"
)

// CNIResult contains the parsed result from CNI plugin execution.
type CNIResult struct {
	// TAPDevice is the name of the TAP device created by CNI plugins.
	TAPDevice string

	// IPAddress is the IP address allocated to the VM.
	IPAddress net.IP

	// Gateway is the gateway IP address for the network.
	Gateway net.IP
}

// ParseCNIResult parses a CNI result and extracts networking information.
//
// This function:
// 1. Extracts the TAP device from the CNI result interfaces
// 2. Parses IP address and gateway information
func ParseCNIResult(result *current.Result) (*CNIResult, error) {
	if result == nil {
		return nil, fmt.Errorf("CNI result is nil")
	}

	// Extract TAP device
	tapDevice, err := ExtractTAPDevice(result)
	if err != nil {
		return nil, fmt.Errorf("failed to extract TAP device: %w", err)
	}

	// Parse IP address and gateway
	var ipAddress net.IP
	var gateway net.IP

	if len(result.IPs) > 0 {
		// Use the first IP configuration
		ipConfig := result.IPs[0]
		ipAddress = ipConfig.Address.IP
		gateway = ipConfig.Gateway
	}

	return &CNIResult{
		TAPDevice: tapDevice,
		IPAddress: ipAddress,
		Gateway:   gateway,
	}, nil
}
