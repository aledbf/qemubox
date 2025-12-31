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

// RelaxOCISpec modifies the OCI spec to remove unnecessary container restrictions.
// Since the container runs inside a VM, the VM provides the security boundary.
// This removes restrictions that are redundant with VM isolation:
//   - Allows access to all devices
//   - Removes readonly and masked paths
//   - Removes seccomp restrictions
//   - Adds bind mount for /etc/resolv.conf (DNS from VM)
func RelaxOCISpec(ctx context.Context, bundlePath string) error {
	spec, err := readSpec(bundlePath)
	if err != nil {
		return err
	}

	if spec.Linux == nil {
		spec.Linux = &specs.Linux{}
	}

	// Allow access to all devices - VM provides isolation
	spec.Linux.Resources = &specs.LinuxResources{
		Devices: []specs.LinuxDeviceCgroup{
			{
				Allow:  true,
				Access: "rwm",
			},
		},
	}

	// Remove readonly and masked paths - not needed with VM isolation
	spec.Linux.ReadonlyPaths = nil
	spec.Linux.MaskedPaths = nil

	// Remove seccomp restrictions - VM provides syscall isolation
	spec.Linux.Seccomp = nil

	// Add /etc/resolv.conf bind mount if not already present
	hasResolv := false
	for _, m := range spec.Mounts {
		if m.Destination == "/etc/resolv.conf" {
			hasResolv = true
			break
		}
	}
	if !hasResolv {
		spec.Mounts = append(spec.Mounts, specs.Mount{
			Destination: "/etc/resolv.conf",
			Type:        "bind",
			Source:      "/etc/resolv.conf",
			Options:     []string{"rbind", "ro"},
		})
	}

	log.G(ctx).Debug("relaxed OCI spec for VM isolation")

	return writeSpec(bundlePath, spec)
}
