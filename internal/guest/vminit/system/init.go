//go:build linux

// Package system provides system initialization for the VM guest environment.
package system

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/log"
	"golang.org/x/sys/unix"

	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/devices"
)

// Initialize performs all system initialization tasks for the VM guest.
// This includes mounting filesystems, configuring cgroups, and setting up DNS.
func Initialize(ctx context.Context) error {
	if err := mountFilesystems(); err != nil {
		return err
	}

	if err := setupDevNodes(ctx); err != nil {
		return err
	}

	// Configure CTRL+ALT+DELETE to send SIGINT to init instead of immediately rebooting
	// This allows vminitd to catch the signal and perform a clean shutdown
	// Default behavior (1) causes immediate kernel reboot without notifying init
	if err := os.WriteFile("/proc/sys/kernel/ctrl-alt-del", []byte("0"), 0644); err != nil {
		// In production, unexpected reboots could be a security concern
		// Log at error level but continue - the setting may not be available in all kernels
		log.G(ctx).WithError(err).Error("failed to configure ctrl-alt-del behavior - VM may reboot unexpectedly on CTRL+ALT+DEL")
	}

	// Wait for virtio block devices to appear
	// This is necessary because the kernel may not have probed all virtio devices yet
	// Not fatal if devices don't appear - they might appear later or not be needed
	devices.WaitForBlockDevices(ctx)

	if err := setupCgroupControl(); err != nil {
		return err
	}

	// #nosec G301 -- /etc must be world-readable inside the VM.
	if err := os.Mkdir("/etc", 0755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("failed to create /etc: %w", err)
	}

	// Configure DNS from kernel command line
	if err := configureDNS(ctx); err != nil {
		log.G(ctx).WithError(err).Warn("failed to configure DNS, continuing anyway")
	}

	return nil
}

// mountFilesystems mounts all required filesystems for the VM guest.
func mountFilesystems() error {
	// Create /lib if it doesn't exist (needed for modules)
	// #nosec G301 -- /lib must be world-readable inside the VM.
	if err := os.MkdirAll("/lib", 0755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("failed to create /lib: %w", err)
	}

	// Mount base filesystems first
	if err := mount.All([]mount.Mount{
		{
			Type:    "proc",
			Source:  "proc",
			Target:  "/proc",
			Options: []string{"nosuid", "noexec", "nodev"},
		},
		{
			Type:    "sysfs",
			Source:  "sysfs",
			Target:  "/sys",
			Options: []string{"nosuid", "noexec", "nodev"},
		},
		{
			Type:   "cgroup2",
			Source: "none",
			Target: "/sys/fs/cgroup",
		},
		{
			Type:    "tmpfs",
			Source:  "tmpfs",
			Target:  "/run",
			Options: []string{"nosuid", "noexec", "nodev"},
		},
		{
			Type:    "tmpfs",
			Source:  "tmpfs",
			Target:  "/tmp",
			Options: []string{"nosuid", "noexec", "nodev"},
		},
		{
			Type:    "devtmpfs",
			Source:  "devtmpfs",
			Target:  "/dev",
			Options: []string{"nosuid", "noexec"},
		},
	}, "/"); err != nil {
		return err
	}

	// Create /dev subdirectories after devtmpfs is mounted
	// #nosec G301 -- /dev/pts and /dev/shm must be accessible inside the VM.
	for _, dir := range []string{"/dev/pts", "/dev/shm"} {
		if err := os.MkdirAll(dir, 0755); err != nil && !os.IsExist(err) {
			return fmt.Errorf("failed to create %s: %w", dir, err)
		}
	}

	// Mount /dev subdirectories
	return mount.All([]mount.Mount{
		{
			Type:    "devpts",
			Source:  "devpts",
			Target:  "/dev/pts",
			Options: []string{"nosuid", "noexec", "gid=5", "mode=0620", "ptmxmode=0666"},
		},
		{
			Type:    "tmpfs",
			Source:  "shm",
			Target:  "/dev/shm",
			Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=64m"},
		},
	}, "/")
}

// setupDevNodes creates device nodes and symlinks that may not be created by devtmpfs.
// This includes /dev/fuse for FUSE filesystems and standard symlinks like /dev/fd.
func setupDevNodes(ctx context.Context) error {
	// Create /dev/fuse if it doesn't exist (major 10, minor 229)
	// FUSE is built into the kernel but devtmpfs may not create the device node
	// until something tries to use it. Docker's fuse-overlayfs needs this.
	fusePath := "/dev/fuse"
	if _, err := os.Stat(fusePath); os.IsNotExist(err) {
		// #nosec G302 -- /dev/fuse must be world-readable for FUSE operations.
		if err := unix.Mknod(fusePath, unix.S_IFCHR|0666, int(unix.Mkdev(10, 229))); err != nil {
			log.G(ctx).WithError(err).Warn("failed to create /dev/fuse, FUSE filesystems may not work")
		} else {
			log.G(ctx).Info("created /dev/fuse device node")
		}
	}

	// Create standard /dev symlinks if they don't exist
	// These are typically created by udev but we don't run udev in the VM
	symlinks := map[string]string{
		"/dev/fd":     "/proc/self/fd",
		"/dev/stdin":  "/proc/self/fd/0",
		"/dev/stdout": "/proc/self/fd/1",
		"/dev/stderr": "/proc/self/fd/2",
	}

	for link, target := range symlinks {
		if _, err := os.Lstat(link); os.IsNotExist(err) {
			if err := os.Symlink(target, link); err != nil {
				log.G(ctx).WithError(err).WithField("link", link).Warn("failed to create symlink")
			}
		}
	}

	// Create /dev/ptmx symlink to /dev/pts/ptmx if it doesn't exist
	// This is needed for pseudo-terminal allocation with devpts
	ptmxPath := "/dev/ptmx"
	if _, err := os.Lstat(ptmxPath); os.IsNotExist(err) {
		if err := os.Symlink("/dev/pts/ptmx", ptmxPath); err != nil {
			log.G(ctx).WithError(err).Warn("failed to create /dev/ptmx symlink")
		}
	}

	return nil
}

// setupCgroupControl enables cgroup controllers for container resource management.
func setupCgroupControl() error {
	// #nosec G306 -- kernel-managed cgroup control file expects 0644.
	return os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+cpu +cpuset +io +memory +pids"), 0644)
}

// configureDNS parses DNS servers from kernel ip= parameter and writes /etc/resolv.conf
// The kernel ip= parameter format is:
// ip=<client-ip>:<server-ip>:<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>:<dns0-ip>:<dns1-ip>
func configureDNS(ctx context.Context) error {
	// Read kernel command line
	cmdlineBytes, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return fmt.Errorf("failed to read /proc/cmdline: %w", err)
	}

	cmdline := string(cmdlineBytes)
	log.G(ctx).WithField("cmdline", cmdline).Debug("parsing kernel command line for DNS config")

	// Parse ip= parameter
	var nameservers []string
	for param := range strings.FieldsSeq(cmdline) {
		if ipParam, ok := strings.CutPrefix(param, "ip="); ok {
			// Split by colons: client-ip:server-ip:gw-ip:netmask:hostname:device:autoconf:dns0-ip:dns1-ip
			parts := strings.Split(ipParam, ":")

			// DNS servers are at index 7 and 8 (0-indexed)
			// Format: ip=<client-ip>:<server-ip>:<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>:<dns0-ip>:<dns1-ip>
			//         0           1           2      3         4          5        6           7         8
			if len(parts) > 7 && parts[7] != "" {
				nameservers = append(nameservers, parts[7])
			}
			if len(parts) > 8 && parts[8] != "" {
				nameservers = append(nameservers, parts[8])
			}
			break
		}
	}

	if len(nameservers) == 0 {
		log.G(ctx).Debug("no DNS servers found in kernel ip= parameter")
		return nil
	}

	// Build resolv.conf content
	var resolvConf strings.Builder
	for _, ns := range nameservers {
		fmt.Fprintf(&resolvConf, "nameserver %s\n", ns)
	}

	// Write /etc/resolv.conf
	// #nosec G306 -- /etc/resolv.conf must be world-readable for non-root processes.
	if err := os.WriteFile("/etc/resolv.conf", []byte(resolvConf.String()), 0644); err != nil {
		return fmt.Errorf("failed to write /etc/resolv.conf: %w", err)
	}

	log.G(ctx).WithField("nameservers", nameservers).Info("configured DNS resolvers from kernel ip= parameter")
	return nil
}
