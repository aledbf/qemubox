//go:build linux

// Package supervisor provides supervisor agent lifecycle management in the VM guest.
// It parses supervisor configuration from kernel cmdline and starts the supervisor binary.
package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containerd/log"
)

// Kernel cmdline parameter names (must match shim/supervisor package)
const (
	ParamWorkspaceID  = "spin.workspace_id"
	ParamSecret       = "spin.secret"
	ParamMetadataAddr = "spin.metadata_addr"
	ParamControlPlane = "spin.control_plane"
)

// Default values
const (
	// DefaultMetadataAddr is the default metadata service address.
	DefaultMetadataAddr = "169.254.169.254:80"

	// BundleBasePath is where bundle files are placed by the bundle service.
	BundleBasePath = "/run/spinbox"

	// BinaryName is the supervisor binary filename in the bundle.
	BinaryName = "spin-supervisor"

	// PidFile is where the supervisor PID is written.
	PidFile = "/run/spin-supervisor.pid"

	// LogFile is where supervisor logs are written.
	LogFile = "/var/log/spin-supervisor.log"
)

// Config holds supervisor configuration parsed from kernel cmdline.
type Config struct {
	WorkspaceID  string
	Secret       string
	MetadataAddr string
	ControlPlane string
	BinaryPath   string
	ContainerID  string // Extracted from bundle path
	Namespace    string // Extracted from bundle path
}

// ParseFromCmdline parses supervisor configuration from /proc/cmdline.
// Returns nil if supervisor is not enabled (no spin.* parameters).
func ParseFromCmdline(ctx context.Context) (*Config, error) {
	cmdlineBytes, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return nil, fmt.Errorf("failed to read /proc/cmdline: %w", err)
	}

	cmdline := string(cmdlineBytes)
	log.G(ctx).WithField("cmdline", cmdline).Debug("parsing kernel cmdline for supervisor config")

	cfg := &Config{}
	foundParams := false

	for param := range strings.FieldsSeq(cmdline) {
		switch {
		case strings.HasPrefix(param, ParamWorkspaceID+"="):
			cfg.WorkspaceID = strings.TrimPrefix(param, ParamWorkspaceID+"=")
			foundParams = true
		case strings.HasPrefix(param, ParamSecret+"="):
			cfg.Secret = strings.TrimPrefix(param, ParamSecret+"=")
			foundParams = true
		case strings.HasPrefix(param, ParamMetadataAddr+"="):
			cfg.MetadataAddr = strings.TrimPrefix(param, ParamMetadataAddr+"=")
			foundParams = true
		case strings.HasPrefix(param, ParamControlPlane+"="):
			cfg.ControlPlane = strings.TrimPrefix(param, ParamControlPlane+"=")
			foundParams = true
		}
	}

	if !foundParams {
		return nil, nil // Supervisor not enabled
	}

	// Apply defaults
	if cfg.MetadataAddr == "" {
		cfg.MetadataAddr = DefaultMetadataAddr
	}

	return cfg, nil
}

// Validate checks that required fields are present.
func (c *Config) Validate() error {
	if c.WorkspaceID == "" {
		return fmt.Errorf("missing %s in kernel cmdline", ParamWorkspaceID)
	}
	if c.Secret == "" {
		return fmt.Errorf("missing %s in kernel cmdline", ParamSecret)
	}
	if c.ControlPlane == "" {
		return fmt.Errorf("missing %s in kernel cmdline", ParamControlPlane)
	}
	return nil
}

// FindBinary searches for the supervisor binary in bundle directories.
// Bundle files are placed at /run/spinbox/{namespace}/{id}/
func (c *Config) FindBinary(ctx context.Context) error {
	// Search for supervisor binary in bundle directories
	entries, err := os.ReadDir(BundleBasePath)
	if err != nil {
		return fmt.Errorf("failed to read bundle base path: %w", err)
	}

	for _, nsEntry := range entries {
		if !nsEntry.IsDir() {
			continue
		}

		nsPath := filepath.Join(BundleBasePath, nsEntry.Name())
		idEntries, err := os.ReadDir(nsPath)
		if err != nil {
			continue
		}

		for _, idEntry := range idEntries {
			if !idEntry.IsDir() {
				continue
			}

			binaryPath := filepath.Join(nsPath, idEntry.Name(), BinaryName)
			if _, err := os.Stat(binaryPath); err == nil {
				c.BinaryPath = binaryPath
				c.Namespace = nsEntry.Name()
				c.ContainerID = idEntry.Name()
				log.G(ctx).WithFields(log.Fields{
					"path":      binaryPath,
					"namespace": c.Namespace,
					"container": c.ContainerID,
				}).Debug("found supervisor binary")
				return nil
			}
		}
	}

	return fmt.Errorf("supervisor binary not found in %s", BundleBasePath)
}

// Start starts the supervisor binary as a background daemon.
// Returns the PID of the started process.
func (c *Config) Start(ctx context.Context) (int, error) {
	if c.BinaryPath == "" {
		return 0, fmt.Errorf("supervisor binary path not set")
	}

	// Ensure binary is executable
	// #nosec G302 -- Supervisor binary must be executable (0755) to run.
	if err := os.Chmod(c.BinaryPath, 0755); err != nil {
		return 0, fmt.Errorf("failed to make supervisor binary executable: %w", err)
	}

	// Create log directory
	logDir := filepath.Dir(LogFile)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.G(ctx).WithError(err).Warn("failed to create log directory")
	}

	// Open log file for supervisor output
	logFile, err := os.OpenFile(LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644) //nolint:gosec
	if err != nil {
		log.G(ctx).WithError(err).Warn("failed to open supervisor log file, using /dev/null")
		logFile, _ = os.Open("/dev/null")
	}

	// Build environment variables for supervisor
	// The supervisor reads config from /proc/cmdline, but we can also pass via env
	env := os.Environ()
	env = append(env,
		fmt.Sprintf("SPIN_WORKSPACE_ID=%s", c.WorkspaceID),
		fmt.Sprintf("SPIN_SECRET=%s", c.Secret),
		fmt.Sprintf("SPIN_METADATA_ADDR=%s", c.MetadataAddr),
		fmt.Sprintf("SPIN_CONTROL_PLANE=%s", c.ControlPlane),
	)

	// Start supervisor as background process
	cmd := exec.Command(c.BinaryPath) //nolint:gosec // Path is from trusted bundle
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // Create new session (detach from terminal)
	}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return 0, fmt.Errorf("failed to start supervisor: %w", err)
	}

	pid := cmd.Process.Pid

	// Write PID file
	if err := os.WriteFile(PidFile, []byte(fmt.Sprintf("%d\n", pid)), 0644); err != nil { //nolint:gosec
		log.G(ctx).WithError(err).Warn("failed to write supervisor PID file")
	}

	// Don't wait for the process - let it run in background
	// Release the process so it becomes a daemon
	if err := cmd.Process.Release(); err != nil {
		log.G(ctx).WithError(err).Warn("failed to release supervisor process")
	}

	log.G(ctx).WithFields(log.Fields{
		"pid":          pid,
		"binary":       c.BinaryPath,
		"workspace_id": c.WorkspaceID,
		"log_file":     LogFile,
	}).Info("started supervisor agent")

	// Close our handle to the log file (supervisor keeps its own)
	logFile.Close()

	return pid, nil
}

// StartSupervisor is the main entry point for starting the supervisor agent.
// It parses config from cmdline, finds the binary, and starts it.
// Returns nil if supervisor is not enabled or binary is not found.
func StartSupervisor(ctx context.Context) error {
	cfg, err := ParseFromCmdline(ctx)
	if err != nil {
		return fmt.Errorf("failed to parse supervisor config: %w", err)
	}

	if cfg == nil {
		log.G(ctx).Debug("supervisor not enabled (no spin.* parameters in cmdline)")
		return nil
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid supervisor config: %w", err)
	}

	if err := cfg.FindBinary(ctx); err != nil {
		// Binary not found - this is not fatal, supervisor may not be in this bundle
		log.G(ctx).WithError(err).Debug("supervisor binary not found, skipping supervisor start")
		return nil
	}

	if _, err := cfg.Start(ctx); err != nil {
		return fmt.Errorf("failed to start supervisor: %w", err)
	}

	return nil
}
