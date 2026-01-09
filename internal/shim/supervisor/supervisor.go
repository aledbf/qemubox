//go:build linux

// Package supervisor provides integration support for the spin supervisor agent.
// It handles extracting supervisor configuration from OCI annotations and
// injecting the supervisor binary into the VM bundle.
package supervisor

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/opencontainers/runtime-spec/specs-go"
)

// Annotation keys for supervisor configuration.
// These are set by the spin runner when creating containers.
const (
	// AnnotationEnabled indicates whether supervisor should be enabled for this container.
	AnnotationEnabled = "io.spin.supervisor.enabled"

	// AnnotationWorkspaceID is the workspace UUID.
	AnnotationWorkspaceID = "io.spin.supervisor.workspace_id"

	// AnnotationSecret is the hex-encoded 32-byte kernel secret for HMAC validation.
	AnnotationSecret = "io.spin.supervisor.secret"

	// AnnotationMetadataAddr is the metadata service address (default: 169.254.169.254:80).
	AnnotationMetadataAddr = "io.spin.supervisor.metadata_addr"

	// AnnotationControlPlane is the control plane address for supervisor connection.
	AnnotationControlPlane = "io.spin.supervisor.control_plane"

	// AnnotationBinaryPath is the host path to the supervisor binary.
	// If not set, defaults to DefaultBinaryPath.
	AnnotationBinaryPath = "io.spin.supervisor.binary_path"
)

// Default values
const (
	// DefaultMetadataAddr is the default metadata service address (link-local).
	DefaultMetadataAddr = "169.254.169.254:80"

	// DefaultBinaryPath is the default location of the supervisor binary on the host.
	DefaultBinaryPath = "/usr/local/bin/spin-supervisor"

	// BundleFileName is the name of the supervisor binary in the bundle.
	BundleFileName = "spin-supervisor"

	// GuestBinaryPath is where the supervisor binary will be available in the VM.
	// The bundle service places files at /run/qemubox/{namespace}/{id}/
	GuestBinaryPath = "/run/qemubox/spin-supervisor"
)

// Config holds the supervisor configuration extracted from annotations.
type Config struct {
	Enabled       bool
	WorkspaceID   string
	Secret        string
	MetadataAddr  string
	ControlPlane  string
	BinaryPath    string
	BinaryContent []byte
}

// FromAnnotations extracts supervisor configuration from OCI spec annotations.
// Returns nil if supervisor is not enabled.
func FromAnnotations(spec *specs.Spec) *Config {
	if spec == nil || spec.Annotations == nil {
		return nil
	}

	annotations := spec.Annotations

	// Check if supervisor is enabled
	if annotations[AnnotationEnabled] != "true" {
		return nil
	}

	cfg := &Config{
		Enabled:      true,
		WorkspaceID:  annotations[AnnotationWorkspaceID],
		Secret:       annotations[AnnotationSecret],
		MetadataAddr: annotations[AnnotationMetadataAddr],
		ControlPlane: annotations[AnnotationControlPlane],
		BinaryPath:   annotations[AnnotationBinaryPath],
	}

	// Apply defaults
	if cfg.MetadataAddr == "" {
		cfg.MetadataAddr = DefaultMetadataAddr
	}
	if cfg.BinaryPath == "" {
		cfg.BinaryPath = DefaultBinaryPath
	}

	return cfg
}

// Validate checks that all required fields are present.
func (c *Config) Validate() error {
	if !c.Enabled {
		return nil
	}

	if c.WorkspaceID == "" {
		return fmt.Errorf("supervisor: missing required annotation %s", AnnotationWorkspaceID)
	}
	if c.Secret == "" {
		return fmt.Errorf("supervisor: missing required annotation %s", AnnotationSecret)
	}
	if c.ControlPlane == "" {
		return fmt.Errorf("supervisor: missing required annotation %s", AnnotationControlPlane)
	}

	return nil
}

// LoadBinary reads the supervisor binary from the configured path.
func (c *Config) LoadBinary() error {
	if !c.Enabled {
		return nil
	}

	// Clean and validate the path
	cleanPath := filepath.Clean(c.BinaryPath)
	if !filepath.IsAbs(cleanPath) {
		return fmt.Errorf("supervisor: binary path must be absolute: %s", c.BinaryPath)
	}

	data, err := os.ReadFile(cleanPath) //nolint:gosec // Path is from trusted annotation
	if err != nil {
		return fmt.Errorf("supervisor: failed to read binary from %s: %w", cleanPath, err)
	}

	c.BinaryContent = data
	return nil
}

// InitArgs returns the kernel cmdline arguments for the supervisor.
// These are passed after the -- separator in the init= parameter.
func (c *Config) InitArgs() []string {
	if !c.Enabled {
		return nil
	}

	return []string{
		fmt.Sprintf("spin.workspace_id=%s", c.WorkspaceID),
		fmt.Sprintf("spin.secret=%s", c.Secret),
		fmt.Sprintf("spin.metadata_addr=%s", c.MetadataAddr),
		fmt.Sprintf("spin.control_plane=%s", c.ControlPlane),
	}
}
