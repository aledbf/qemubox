//go:build linux

package task

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"

	"github.com/containerd/log"
	"github.com/docker/docker/libnetwork/resolvconf"

	"github.com/aledbf/beacon/containerd/network"
	"github.com/aledbf/beacon/containerd/vm"
)

// generateGuestMAC generates a unique MAC address for the VM guest interface
// based on the container ID. This ensures the guest MAC is different from the
// host TAP device MAC, preventing bridge loops.
//
// The MAC uses the locally administered unicast address format:
// - First byte: 0x02 (locally administered, unicast)
// - Remaining 5 bytes: derived from SHA256 hash of the ID
func generateGuestMAC(id string) net.HardwareAddr {
	hash := sha256.Sum256([]byte(id))
	mac := make(net.HardwareAddr, 6)
	mac[0] = 0x02 // Locally administered, unicast
	copy(mac[1:], hash[:5])
	return mac
}

// setupNetworking sets up networking using NetworkManager for dynamic IP allocation
// and TAP device management. NetworkManager handles bridge creation, IP allocation,
// TAP device lifecycle, and NFTables rules.
// Returns the network configuration that should be passed to the VM kernel
func setupNetworking(ctx context.Context, nm network.NetworkManagerInterface, vmi vm.Instance, containerID, netnsPath string) (*vm.NetworkConfig, error) {
	log.G(ctx).WithField("id", containerID).Info("setting up NetworkManager-based networking")

	// Create environment for this container
	env := &network.Environment{
		ID: containerID,
	}

	// Allocate network resources (IP + TAP device)
	if err := nm.EnsureNetworkResources(env); err != nil {
		return nil, fmt.Errorf("allocate network resources: %w", err)
	}

	log.G(ctx).WithFields(log.Fields{
		"tap":     env.NetworkInfo.TapName,
		"ip":      env.NetworkInfo.IP.String(),
		"gateway": env.NetworkInfo.Gateway.String(),
		"netmask": env.NetworkInfo.Netmask,
	}).Info("network resources allocated")

	// Generate unique MAC address for the guest VM
	// We must NOT use the TAP device's MAC as it would create a bridge loop
	guestMAC := generateGuestMAC(containerID)

	log.G(ctx).WithFields(log.Fields{
		"tap":       env.NetworkInfo.TapName,
		"guest_mac": guestMAC.String(),
	}).Debug("generated unique guest MAC address")

	// Attach TAP to VM (QEMU opens by name)
	if err := vmi.AddTAPNIC(ctx, env.NetworkInfo.TapName, guestMAC); err != nil {
		nm.ReleaseNetworkResources(env)
		return nil, fmt.Errorf("add TAP NIC to VM: %w", err)
	}

	log.G(ctx).WithField("tap", env.NetworkInfo.TapName).Info("TAP device attached to VM")

	dnsServers := resolveHostDNSServers(ctx)
	if len(dnsServers) == 0 {
		dnsServers = []string{"8.8.8.8", "8.8.4.4"}
	}

	log.G(ctx).WithField("dns", dnsServers).Debug("configured DNS servers")

	// Return network configuration for VM kernel
	return &vm.NetworkConfig{
		InterfaceName: "eth0",
		IP:            env.NetworkInfo.IP.String(),
		Gateway:       env.NetworkInfo.Gateway.String(),
		Netmask:       env.NetworkInfo.Netmask,
		DNS:           dnsServers,
	}, nil
}

func resolveHostDNSServers(ctx context.Context) []string {
	path := resolvconf.Path()
	file, err := resolvconf.GetSpecific(path)
	if err != nil {
		log.G(ctx).WithError(err).WithField("path", path).Warn("failed to read host resolv.conf for DNS")
		return nil
	}

	filtered, err := resolvconf.FilterResolvDNS(file.Content, false)
	if err != nil {
		log.G(ctx).WithError(err).WithField("path", path).Warn("failed to filter host resolv.conf for DNS")
		return nil
	}

	nameservers := resolvconf.GetNameservers(filtered.Content, resolvconf.IPv4)
	if len(nameservers) == 0 {
		log.G(ctx).WithField("path", path).Warn("no valid DNS servers found in host resolv.conf")
		return nil
	}

	log.G(ctx).WithFields(log.Fields{
		"path":        path,
		"nameservers": nameservers,
	}).Debug("resolved host DNS servers")

	return nameservers
}
