//go:build linux

package qemu

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"

	"github.com/aledbf/qemubox/containerd/internal/host/vm"
)

func (q *Instance) AddFS(ctx context.Context, tag, mountPath string, opts ...vm.MountOpt) error {
	log.G(ctx).WithFields(log.Fields{
		"tag":  tag,
		"path": mountPath,
	}).Warn("qemu: AddFS not supported, use disk-based approach instead")

	return fmt.Errorf("AddFS not implemented for QEMU: use EROFS or block devices")
}

// generateStableDiskID generates a stable device ID based on file metadata.
// This ensures consistent device naming across VM reboots and reduces issues
// with device enumeration order. Uses inode and device number as stable identifiers.
func generateStableDiskID(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("failed to stat disk path: %w", err)
	}

	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		// Fallback: use base64-encoded path (stable, no hash collisions)
		// Using RawURLEncoding makes it filesystem-safe (no padding '=' characters)
		encoded := base64.RawURLEncoding.EncodeToString([]byte(path))
		return "disk-" + encoded, nil
	}

	// Generate stable ID from device and inode numbers
	// Format: disk-<dev>-<inode>
	return fmt.Sprintf("disk-%x-%x", stat.Dev, stat.Ino), nil
}

// AddDisk schedules a disk to be attached to the VM
func (q *Instance) AddDisk(ctx context.Context, blockID, mountPath string, opts ...vm.MountOpt) error {
	if q.getState() != vmStateNew {
		return errors.New("cannot add disk after VM started")
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	var mc vm.MountConfig
	for _, o := range opts {
		o(&mc)
	}

	// Generate stable device ID if not provided
	if blockID == "" {
		stableID, err := generateStableDiskID(mountPath)
		if err != nil {
			return fmt.Errorf("failed to generate stable disk ID: %w", err)
		}
		blockID = stableID
	}

	q.disks = append(q.disks, &DiskConfig{
		Path:     mountPath,
		Readonly: mc.Readonly,
		ID:       blockID,
	})

	log.G(ctx).WithFields(log.Fields{
		"blockID":  blockID,
		"path":     mountPath,
		"readonly": mc.Readonly,
	}).Debug("qemu: scheduled disk device")

	return nil
}

// AddNIC adds a network interface (not supported for QEMU microvm, use TAP)
func (q *Instance) AddNIC(ctx context.Context, endpoint string, mac net.HardwareAddr, mode vm.NetworkMode, features, flags uint32) error {
	return fmt.Errorf("UNIX socket networking not supported by QEMU microvm; use AddTAPNIC instead: %w", errdefs.ErrNotImplemented)
}

// AddTAPNIC schedules a TAP network interface to be attached to the VM
func (q *Instance) AddTAPNIC(ctx context.Context, tapName string, mac net.HardwareAddr) error {
	if q.getState() != vmStateNew {
		return errors.New("cannot add NIC after VM started")
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	macStr := mac.String()
	q.nets = append(q.nets, &NetConfig{
		TapName: tapName,
		MAC:     macStr,
		ID:      fmt.Sprintf("net%d", len(q.nets)),
	})

	log.G(ctx).WithFields(log.Fields{
		"tap": tapName,
		"mac": macStr,
	}).Debug("qemu: scheduled TAP NIC device")

	return nil
}

// VMInfo returns metadata about the QEMU backend
func (q *Instance) VMInfo() vm.VMInfo {
	return vm.VMInfo{
		Type:          "qemu",
		SupportsTAP:   true,
		SupportsVSOCK: true,
	}
}
