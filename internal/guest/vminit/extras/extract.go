//go:build linux

// Package extras provides file extraction from extras disk inside the VM guest.
// It parses the spin.extras_disk kernel parameter, reads the tar archive from
// the corresponding block device, and extracts files to their destination paths.
//
// Files are extracted to the paths specified in the tar entry names, which are
// set by the host when creating the extras disk. This allows the host to place
// files at any location in the VM filesystem.
//
// Extraction is idempotent: it only runs once per boot (tracked via marker file).
// Use spin.extras_force=1 kernel parameter to force re-extraction.
package extras

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containerd/log"
)

// markerFile tracks whether extraction has completed successfully.
// Located in tmpfs so it resets on each boot.
const markerFile = "/run/spin-stack/.extras-extracted"

// extrasConfig holds parsed kernel cmdline parameters for extras disk.
type extrasConfig struct {
	device string // Device path (e.g., "/dev/vdb"), empty if not configured
	force  bool   // Force extraction even if already done
}

// parseExtrasConfig parses spin.extras_* parameters from kernel cmdline.
func parseExtrasConfig(ctx context.Context, cmdline string) (*extrasConfig, error) {
	log.G(ctx).WithField("cmdline", cmdline).Debug("parsing kernel cmdline for extras config")

	cfg := &extrasConfig{}

	for param := range strings.FieldsSeq(cmdline) {
		if indexStr, ok := strings.CutPrefix(param, "spin.extras_disk="); ok {
			index, err := strconv.Atoi(indexStr)
			if err != nil {
				return nil, fmt.Errorf("parse extras_disk index: %w", err)
			}
			// Convert index to device: 0→vda, 1→vdb, etc.
			cfg.device = fmt.Sprintf("/dev/vd%c", 'a'+index)
			log.G(ctx).WithFields(log.Fields{
				"index":  index,
				"device": cfg.device,
			}).Info("found extras disk in kernel cmdline")
		}
		if param == "spin.extras_force=1" || param == "spin.extras_force" {
			cfg.force = true
			log.G(ctx).Debug("extras force extraction enabled")
		}
	}

	return cfg, nil
}

// DeviceFromCmdline parses spin.extras_disk=N from a kernel command line and
// returns the device path it names (e.g. "/dev/vdb"), or empty if the container
// has no extras disk.
// The second return is spin.extras_force, which re-extracts over the marker.
func DeviceFromCmdline(ctx context.Context, cmdline string) (string, bool, error) {
	cfg, err := parseExtrasConfig(ctx, cmdline)
	if err != nil {
		return "", false, err
	}
	return cfg.device, cfg.force, nil
}

// ExtractDevice unpacks the extras archive from device.
//
// It is idempotent: a marker file records that it has run, so calling it again
// - which the host does whenever it configures a VM, without knowing whether
// that VM booted or was restored - does nothing the second time, unless force
// says otherwise (spin.extras_force on the kernel command line).
func ExtractDevice(ctx context.Context, device string, force bool) error {
	if device == "" {
		log.G(ctx).Debug("no extras disk configured")
		return nil
	}

	if !force {
		if _, err := os.Stat(markerFile); err == nil {
			log.G(ctx).Debug("extras already extracted, skipping")
			return nil
		}
	}

	if err := ExtractFromDevice(ctx, device); err != nil {
		return err
	}

	// Mark extraction as complete
	if err := writeMarkerFile(); err != nil {
		log.G(ctx).WithError(err).Warn("failed to write extraction marker")
	}

	return nil
}

// writeMarkerFile creates the marker file to indicate successful extraction.
func writeMarkerFile() error {
	if err := os.MkdirAll(filepath.Dir(markerFile), 0755); err != nil {
		return err
	}
	return os.WriteFile(markerFile, []byte("done\n"), 0644) //nolint:gosec
}

// ExtractFromDevice reads tar from block device and extracts files to their
// destination paths as specified in the tar entry names.
func ExtractFromDevice(ctx context.Context, devicePath string) error {
	f, err := os.Open(devicePath)
	if err != nil {
		return fmt.Errorf("open device %s: %w", devicePath, err)
	}
	defer f.Close()

	tr := tar.NewReader(f)
	var extracted int
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}

		// The tar entry name is the destination path in the VM
		destPath := hdr.Name

		// Security: require absolute paths
		if !filepath.IsAbs(destPath) {
			log.G(ctx).WithField("path", destPath).Warn("skipping non-absolute path in extras disk")
			continue
		}

		// Clean the path to prevent traversal attacks
		destPath = filepath.Clean(destPath)

		// Create parent directories
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return fmt.Errorf("create parent dir for %s: %w", destPath, err)
		}

		// Extract file
		outFile, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
		if err != nil {
			return fmt.Errorf("create file %s: %w", destPath, err)
		}

		// Use CopyN with header size to prevent decompression bombs (gosec G110)
		n, err := io.CopyN(outFile, tr, hdr.Size)
		outFile.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("extract file %s: %w", destPath, err)
		}

		log.G(ctx).WithFields(log.Fields{
			"path": destPath,
			"size": n,
			"mode": fmt.Sprintf("%o", hdr.Mode),
		}).Debug("extracted file from extras disk")
		extracted++
	}

	log.G(ctx).WithField("count", extracted).Info("extracted files from extras disk")
	return nil
}
