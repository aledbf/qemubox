//go:build linux

package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/containerd/log"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// metadataServiceIP is the link-local address of the host metadata service the
// supervisor agent talks to. A host route to it is installed via the gateway
// when the supervisor is enabled (spin.metadata_addr present).
var metadataServiceIP = net.IPv4(169, 254, 169, 254)

// NetworkIdentity holds the network settings parsed from the kernel ip= parameter.
//
// Format (kernel nfsroot style):
//
//	ip=<client-ip>:<server-ip>:<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>:<dns0>:<dns1>
//	   0           1           2       3          4          5        6          7      8
//
// Historically the kernel consumed ip= (CONFIG_IP_PNP) and brought the
// interface up itself. We now configure the interface in userspace
// (configureNetwork) and treat ip= purely as a config channel: the same string
// still describes the address, gateway, netmask, device and DNS - it is just
// parsed here instead of by ip_auto_config.
type NetworkIdentity struct {
	IP      string
	Gateway string
	Netmask string
	Device  string
	DNS     []string

	// MAC is the hardware address to take, empty to keep the one the NIC has.
	//
	// A restored VM needs this where it does not need a CID: virtio-net carries
	// its MAC in the migration stream (VMSTATE_UINT8_ARRAY(mac, VirtIONet,
	// ETH_ALEN)), so the restored guest comes up believing it is the template's
	// NIC, while vhost-vsock does not carry guest-cid and the CID sorts itself out.
	MAC string

	// MetadataRoute asks for the host route to the metadata service via Gateway.
	// The boot path sets it from spin.metadata_addr on the kernel command line.
	MetadataRoute bool
}

// device returns the interface name, defaulting to eth0 (the kernel names the
// single virtio-net device eth0 because of net.ifnames=0).
func (c NetworkIdentity) device() string {
	if c.Device == "" {
		return "eth0"
	}
	return c.Device
}

// parseIPConfig extracts the ip= parameter from a kernel command line. The
// second return is false when no ip= parameter is present.
func parseIPConfig(cmdline string) (NetworkIdentity, bool) {
	for param := range strings.FieldsSeq(cmdline) {
		v, ok := strings.CutPrefix(param, "ip=")
		if !ok {
			continue
		}
		parts := strings.Split(v, ":")
		field := func(i int) string {
			if i < len(parts) {
				return parts[i]
			}
			return ""
		}
		cfg := NetworkIdentity{
			IP:      field(0),
			Gateway: field(2),
			Netmask: field(3),
			Device:  field(5),
		}
		if d := field(7); d != "" {
			cfg.DNS = append(cfg.DNS, d)
		}
		if d := field(8); d != "" {
			cfg.DNS = append(cfg.DNS, d)
		}
		return cfg, true
	}
	return NetworkIdentity{}, false
}

// configureNetwork brings up the guest network interface and assigns its
// address and default route from the parsed ip= configuration.
//
// This replaces the kernel's ip_auto_config (CONFIG_IP_PNP), which we dropped.
// Doing it in userspace via netlink skips the kernel's conservative device-open
// and carrier-wait delays (~12 ms in the boot profile) and overlaps with the
// rest of vminitd startup; the virtio-net carrier is up immediately with a TAP
// backend, so there is nothing to wait for.
func configureNetwork(ctx context.Context, cfg NetworkIdentity) error {
	if cfg.IP == "" {
		log.G(ctx).Debug("no IP in kernel ip= parameter, skipping interface configuration")
		return nil
	}

	ip := net.ParseIP(cfg.IP)
	if ip == nil {
		return fmt.Errorf("invalid IP %q in ip= parameter", cfg.IP)
	}
	mask, err := parseNetmask(cfg.Netmask)
	if err != nil {
		return err
	}

	dev := cfg.device()
	link, err := linkByNameWait(ctx, dev)
	if err != nil {
		return fmt.Errorf("find link %q: %w", dev, err)
	}

	// Take the MAC before bringing the link up. A restored VM inherits the
	// template's, because virtio-net carries it in the migration stream, and the
	// address it is about to claim belongs to a different machine on the wire.
	if cfg.MAC != "" {
		mac, err := net.ParseMAC(cfg.MAC)
		if err != nil {
			return fmt.Errorf("invalid MAC %q: %w", cfg.MAC, err)
		}
		if !bytes.Equal(link.Attrs().HardwareAddr, mac) {
			if err := netlink.LinkSetHardwareAddr(link, mac); err != nil {
				return fmt.Errorf("set MAC %s on %q: %w", mac, dev, err)
			}
		}
	}

	// Bring the interface up first, then assign the address. Adding the address
	// while the link is up installs the connected route, which the default route
	// below needs in order to resolve the gateway.
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("set link %q up: %w", dev, err)
	}

	addr := &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: mask}}
	if err := netlink.AddrAdd(link, addr); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("add address %s to %q: %w", addr.IPNet, dev, err)
	}

	if cfg.Gateway != "" {
		gw := net.ParseIP(cfg.Gateway)
		if gw == nil {
			return fmt.Errorf("invalid gateway %q in ip= parameter", cfg.Gateway)
		}
		route := &netlink.Route{LinkIndex: link.Attrs().Index, Gw: gw}
		if err := netlink.RouteAdd(route); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("add default route via %s: %w", cfg.Gateway, err)
		}
	}

	log.G(ctx).WithFields(log.Fields{
		"device":  dev,
		"ip":      addr.IPNet.String(),
		"gateway": cfg.Gateway,
	}).Info("configured guest network interface")
	return nil
}

// configureDNS writes /etc/resolv.conf from the DNS servers in the ip= config.
func configureDNS(ctx context.Context, cfg NetworkIdentity) error {
	if len(cfg.DNS) == 0 {
		log.G(ctx).Debug("no DNS servers in kernel ip= parameter")
		return nil
	}

	var resolvConf strings.Builder
	for _, ns := range cfg.DNS {
		fmt.Fprintf(&resolvConf, "nameserver %s\n", ns)
	}

	// #nosec G306 -- /etc/resolv.conf must be world-readable for non-root processes.
	if err := os.WriteFile("/etc/resolv.conf", []byte(resolvConf.String()), 0644); err != nil {
		return fmt.Errorf("failed to write /etc/resolv.conf: %w", err)
	}

	log.G(ctx).WithField("nameservers", cfg.DNS).Info("configured DNS resolvers from kernel ip= parameter")
	return nil
}

// configureMetadataRoute adds a host route to the metadata service via the
// gateway so the supervisor agent can reach it. It is a no-op unless the
// supervisor is enabled (spin.metadata_addr present in the cmdline). The
// interface is already up (configureNetwork ran first), so the route installs
// directly via netlink.
func configureMetadataRoute(ctx context.Context, cfg NetworkIdentity) error {
	if cfg.Gateway == "" {
		log.G(ctx).Debug("no gateway, skipping metadata route")
		return nil
	}

	if !cfg.MetadataRoute {
		log.G(ctx).Debug("no metadata route requested, skipping")
		return nil
	}

	gw := net.ParseIP(cfg.Gateway)
	if gw == nil {
		return fmt.Errorf("invalid gateway %q for metadata route", cfg.Gateway)
	}

	dev := cfg.device()
	link, err := netlink.LinkByName(dev)
	if err != nil {
		return fmt.Errorf("find link %q for metadata route: %w", dev, err)
	}

	route := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       &net.IPNet{IP: metadataServiceIP, Mask: net.CIDRMask(32, 32)},
		Gw:        gw,
	}
	if err := netlink.RouteAdd(route); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("add metadata route to %s via %s: %w", metadataServiceIP, cfg.Gateway, err)
	}

	log.G(ctx).WithFields(log.Fields{
		"metadata_ip": metadataServiceIP.String(),
		"gateway":     cfg.Gateway,
	}).Info("added route to metadata service")
	return nil
}

// hasParam reports whether the cmdline contains a parameter with the given
// prefix (e.g. "spin.metadata_addr=").
func hasParam(cmdline, prefix string) bool {
	for param := range strings.FieldsSeq(cmdline) {
		if strings.HasPrefix(param, prefix) {
			return true
		}
	}
	return false
}

// parseNetmask converts a dotted-decimal netmask (e.g. "255.255.255.0") into an
// IPv4 mask. An empty netmask defaults to /24, matching the kernel ip= behavior
// of deriving a sane mask when the field is omitted.
func parseNetmask(netmask string) (net.IPMask, error) {
	if netmask == "" {
		return net.CIDRMask(24, 32), nil
	}
	v4 := net.ParseIP(netmask).To4()
	if v4 == nil {
		return nil, fmt.Errorf("invalid netmask %q in ip= parameter", netmask)
	}
	mask := net.IPMask(v4)
	if ones, bits := mask.Size(); ones == 0 && bits == 0 {
		return nil, fmt.Errorf("non-contiguous netmask %q in ip= parameter", netmask)
	}
	return mask, nil
}

// linkByNameWait resolves a network link by name, briefly retrying in case the
// virtio-net device has not been registered yet. By the time vminitd (PID1)
// runs, the device_initcall that probes virtio-net has completed, so the happy
// path succeeds on the first try with no sleep.
func linkByNameWait(ctx context.Context, name string) (netlink.Link, error) {
	const attempts = 20
	const interval = 50 * time.Millisecond

	var lastErr error
	for range attempts {
		link, err := netlink.LinkByName(name)
		if err == nil {
			return link, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
	return nil, lastErr
}
