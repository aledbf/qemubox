//go:build linux

package runc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/containerd/log"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// resolvConfPath is the guest's resolver configuration, written by system.Apply from
// the address this VM was given. A VM given no address does not have one.
const resolvConfPath = "/etc/resolv.conf"

// ShouldKillAllOnExit reads the bundle's OCI spec and returns true if
// there is an error reading the spec or if the container has a private PID namespace
func ShouldKillAllOnExit(ctx context.Context, bundlePath string) bool {
	spec, err := readSpec(bundlePath)
	if err != nil {
		log.G(ctx).WithError(err).Error("shouldKillAllOnExit: failed to read config.json")
		return true
	}

	if spec.Linux != nil {
		for _, ns := range spec.Linux.Namespaces {
			if ns.Type == specs.PIDNamespace && ns.Path == "" {
				return false
			}
		}
	}
	return true
}

func readSpec(p string) (*specs.Spec, error) {
	const configFileName = "config.json"
	f, err := os.Open(filepath.Join(p, configFileName))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.L.WithError(err).Warn("failed to close config.json")
		}
	}()
	var s specs.Spec
	if err := json.NewDecoder(f).Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

func writeSpec(p string, spec *specs.Spec) error {
	const configFileName = "config.json"
	f, err := os.Create(filepath.Join(p, configFileName))
	if err != nil {
		return err
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.L.WithError(err).Warn("failed to close config.json")
		}
	}()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(spec)
}

// RelaxOCISpec modifies the OCI spec for VM-isolated containers.
// Since the container runs inside a VM, the VM provides the security boundary.
// This function:
//   - Bind-mounts /dev from the VM (gives access to all devices)
//   - Allows all device access in cgroups
//   - Removes readonly/masked paths and seccomp
//   - Adds /etc/resolv.conf for DNS
func RelaxOCISpec(ctx context.Context, bundlePath string) error {
	spec, err := readSpec(bundlePath)
	if err != nil {
		return err
	}

	if spec.Linux == nil {
		spec.Linux = &specs.Linux{}
	}

	// Allow access to all devices via cgroups
	spec.Linux.Resources = &specs.LinuxResources{
		Devices: []specs.LinuxDeviceCgroup{{Allow: true, Access: "rwm"}},
	}

	// Remove container isolation - VM provides it
	spec.Linux.ReadonlyPaths = nil
	spec.Linux.MaskedPaths = nil
	spec.Linux.Seccomp = nil

	// Replace /dev with bind mount from VM's /dev
	// This gives access to all devices (fuse, tun, etc.) automatically
	// Also skip /dev/* mounts since they're included in the bind mount
	var newMounts []specs.Mount
	for _, m := range spec.Mounts {
		switch m.Destination {
		case "/dev":
			// Replace tmpfs with bind mount
			newMounts = append(newMounts, specs.Mount{
				Destination: "/dev",
				Type:        "bind",
				Source:      "/dev",
				Options:     []string{"rbind", "rw"},
			})
		case "/dev/pts", "/dev/shm", "/dev/mqueue":
			// Skip - already in VM's /dev via bind mount
		default:
			newMounts = append(newMounts, m)
		}
	}

	// Add /etc/resolv.conf if not present.
	//
	// Only if this VM has one. It is written by system.Apply from the address the
	// VM was given, and a VM given no address has no DNS to pass on - which is a
	// real configuration now that the guest no longer requires a NIC to finish
	// starting. Bind-mounting a file that is not there fails the container
	// outright, with "cannot stat /etc/resolv.conf" from the runtime and nothing
	// saying why it was asked for.
	hasResolv := false
	for _, m := range newMounts {
		if m.Destination == resolvConfPath {
			hasResolv = true
			break
		}
	}
	if _, err := os.Stat(resolvConfPath); err != nil {
		hasResolv = true // nothing to bind; leave the container without DNS
	}
	if !hasResolv {
		newMounts = append(newMounts, specs.Mount{
			Destination: resolvConfPath,
			Type:        "bind",
			Source:      resolvConfPath,
			Options:     []string{"rbind", "ro"},
		})
	}

	spec.Mounts = newMounts

	return writeSpec(bundlePath, spec)
}
