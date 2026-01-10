//go:build linux

package network

import (
	"context"
	"fmt"
	"net"

	"github.com/containerd/log"
	"github.com/docker/docker/libnetwork/resolvconf"

	"github.com/spin-stack/spinbox/internal/host/network"
	"github.com/spin-stack/spinbox/internal/host/vm"
)

type linuxManager struct{}

func newManager() Manager {
	return &linuxManager{}
}

// InitNetworkManager creates and initializes a new NetworkManager instance.
// Spinbox uses CNI (Container Network Interface) for all network management.
// Network state is managed in-memory (cniResults map) and by CNI IPAM plugins
// (state stored in /var/lib/cni/networks/).
func (m *linuxManager) InitNetworkManager(ctx context.Context) (network.NetworkManager, error) {
	// Load CNI network configuration from environment
	netCfg := network.LoadNetworkConfig()

	// Create CNI-based NetworkManager
	nm, err := network.NewNetworkManager(ctx, netCfg)
	if err != nil {
		return nil, fmt.Errorf("create CNI network manager: %w", err)
	}

	log.G(ctx).Info("NetworkManager initialized")
	return nm, nil
}

// Setup sets up networking using NetworkManager for dynamic IP allocation
// and TAP device management. NetworkManager handles bridge creation, IP allocation,
// TAP device lifecycle, and NFTables rules.
// Returns the network configuration that should be passed to the VM kernel.
func (m *linuxManager) Setup(ctx context.Context, nm network.NetworkManager, vmi vm.Instance, containerID, netnsPath string) (*vm.NetworkConfig, error) {
	log.G(ctx).WithField("id", containerID).Info("setting up NetworkManager-based networking")

	// Create environment for this container
	env := &network.Environment{
		ID: containerID,
	}

	// Allocate network resources (IP + TAP device)
	if err := nm.EnsureNetworkResources(ctx, env); err != nil {
		return nil, fmt.Errorf("allocate network resources: %w", err)
	}

	log.G(ctx).WithFields(log.Fields{
		"tap":     env.NetworkInfo.TapName,
		"ip":      env.NetworkInfo.IP.String(),
		"gateway": env.NetworkInfo.Gateway.String(),
		"netmask": env.NetworkInfo.Netmask,
	}).Info("network resources allocated")

	if env.NetworkInfo.MAC == "" {
		if err := nm.ReleaseNetworkResources(ctx, env); err != nil {
			log.G(ctx).WithError(err).Warn("failed to release network resources")
		}
		return nil, fmt.Errorf("CNI did not report TAP MAC address")
	}

	guestMAC, err := net.ParseMAC(env.NetworkInfo.MAC)
	if err != nil {
		if err := nm.ReleaseNetworkResources(ctx, env); err != nil {
			log.G(ctx).WithError(err).Warn("failed to release network resources")
		}
		return nil, fmt.Errorf("invalid CNI TAP MAC address %q: %w", env.NetworkInfo.MAC, err)
	}

	log.G(ctx).WithFields(log.Fields{
		"tap":       env.NetworkInfo.TapName,
		"guest_mac": guestMAC.String(),
	}).Debug("generated unique guest MAC address")

	// Attach TAP to VM (QEMU opens by name)
	if err := vmi.AddTAPNIC(ctx, env.NetworkInfo.TapName, guestMAC); err != nil {
		if err := nm.ReleaseNetworkResources(ctx, env); err != nil {
			log.G(ctx).WithError(err).Warn("failed to release network resources")
		}
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
