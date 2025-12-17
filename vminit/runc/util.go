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
	defer f.Close()
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
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(spec)
}

// InjectResolvConf adds a bind mount for /etc/resolv.conf from the VM root into the container
// This allows containers to inherit DNS configuration from the VM's /etc/resolv.conf
func InjectResolvConf(ctx context.Context, bundlePath string) error {
	spec, err := readSpec(bundlePath)
	if err != nil {
		return err
	}

	// Check if /etc/resolv.conf already exists as a mount
	for _, m := range spec.Mounts {
		if m.Destination == "/etc/resolv.conf" {
			log.G(ctx).Debug("resolv.conf mount already exists in spec, skipping injection")
			return nil
		}
	}

	// Add bind mount for /etc/resolv.conf
	resolvConfMount := specs.Mount{
		Destination: "/etc/resolv.conf",
		Type:        "bind",
		Source:      "/etc/resolv.conf",
		Options:     []string{"rbind", "ro"},
	}

	spec.Mounts = append(spec.Mounts, resolvConfMount)

	log.G(ctx).Info("injected /etc/resolv.conf bind mount into container spec")

	return writeSpec(bundlePath, spec)
}
