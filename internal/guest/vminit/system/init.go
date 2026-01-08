//go:build linux

// Package system provides system initialization for the VM guest environment.
package system

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/log"
	"golang.org/x/sys/unix"

	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/devices"
)

// Initialize performs all system initialization tasks for the VM guest.
// This includes mounting filesystems, configuring cgroups, and setting up DNS.
// Mounts are parallelized where possible for faster boot times.
func Initialize(ctx context.Context) error {
	if err := mountFilesystems(ctx); err != nil {
		return err
	}

	// Run independent operations in parallel after base mounts are ready
	var wg sync.WaitGroup
	errCh := make(chan error, 4)

	// Setup device nodes (depends on /dev being mounted)
	wg.Go(func() {
		if err := setupDevNodes(ctx); err != nil {
			errCh <- err
		}
	})

	// Configure CTRL+ALT+DELETE (depends on /proc being mounted)
	wg.Go(func() {
		if err := os.WriteFile("/proc/sys/kernel/ctrl-alt-del", []byte("0"), 0644); err != nil {
			log.G(ctx).WithError(err).Error("failed to configure ctrl-alt-del behavior - VM may reboot unexpectedly on CTRL+ALT+DEL")
		}
	})

	// Setup cgroup controllers (depends on /sys/fs/cgroup being mounted)
	wg.Go(func() {
		if err := setupCgroupControl(); err != nil {
			errCh <- fmt.Errorf("cgroup setup failed: %w", err)
		}
	})

	// Create /etc directory
	wg.Go(func() {
		// #nosec G301 -- /etc must be world-readable inside the VM.
		if err := os.Mkdir("/etc", 0755); err != nil && !os.IsExist(err) {
			errCh <- fmt.Errorf("failed to create /etc: %w", err)
		}
	})

	wg.Wait()
	close(errCh)

	// Collect any errors from parallel operations
	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	// Wait for virtio block devices to appear (can run after base setup)
	// This is necessary because the kernel may not have probed all virtio devices yet
	// Not fatal if devices don't appear - they might appear later or not be needed
	devices.WaitForBlockDevices(ctx)

	// Configure DNS from kernel command line (depends on /etc existing)
	if err := configureDNS(ctx); err != nil {
		log.G(ctx).WithError(err).Warn("failed to configure DNS, continuing anyway")
	}

	return nil
}

// mountFilesystems mounts all required filesystems for the VM guest.
// Mounts are organized into phases based on dependencies:
//   - Phase 1: Independent base mounts (proc, sysfs, devtmpfs) - parallel
//   - Phase 2: Dependent mounts (cgroup2 needs /sys, tmpfs for /run, /tmp) - parallel
//   - Phase 3: /dev subdirectories (need /dev) - parallel
func mountFilesystems(ctx context.Context) error {
	// Create /lib if it doesn't exist (needed for modules)
	// #nosec G301 -- /lib must be world-readable inside the VM.
	if err := os.MkdirAll("/lib", 0755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("failed to create /lib: %w", err)
	}

	// Phase 1: Mount independent base filesystems in parallel
	phase1Mounts := []mount.Mount{
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
			Type:    "devtmpfs",
			Source:  "devtmpfs",
			Target:  "/dev",
			Options: []string{"nosuid", "noexec"},
		},
	}

	if err := mountParallel(ctx, phase1Mounts); err != nil {
		return fmt.Errorf("phase 1 mounts failed: %w", err)
	}

	// Phase 2: Mount filesystems that depend on phase 1 (parallel)
	phase2Mounts := []mount.Mount{
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
	}

	if err := mountParallel(ctx, phase2Mounts); err != nil {
		return fmt.Errorf("phase 2 mounts failed: %w", err)
	}

	// Create directories needed before phase 3
	// #nosec G301 -- /run/lock needs sticky bit like /tmp for lock files.
	if err := os.MkdirAll("/run/lock", 0o1777); err != nil && !os.IsExist(err) {
		return fmt.Errorf("failed to create /run/lock: %w", err)
	}

	// Create /dev subdirectories after devtmpfs is mounted
	// #nosec G301 -- /dev/pts and /dev/shm must be accessible inside the VM.
	for _, dir := range []string{"/dev/pts", "/dev/shm"} {
		if err := os.MkdirAll(dir, 0755); err != nil && !os.IsExist(err) {
			return fmt.Errorf("failed to create %s: %w", dir, err)
		}
	}

	// Phase 3: Mount /dev subdirectories (parallel)
	phase3Mounts := []mount.Mount{
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
	}

	if err := mountParallel(ctx, phase3Mounts); err != nil {
		return fmt.Errorf("phase 3 mounts failed: %w", err)
	}

	return nil
}

// mountParallel mounts multiple filesystems in parallel.
// Returns an error if any mount fails.
func mountParallel(ctx context.Context, mounts []mount.Mount) error {
	if len(mounts) == 0 {
		return nil
	}

	// For single mount, just do it directly
	if len(mounts) == 1 {
		return mount.All(mounts, "/")
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(mounts))

	for _, m := range mounts {
		wg.Go(func() {
			if err := mount.All([]mount.Mount{m}, "/"); err != nil {
				log.G(ctx).WithError(err).WithField("target", m.Target).Error("mount failed")
				errCh <- fmt.Errorf("mount %s failed: %w", m.Target, err)
			}
		})
	}

	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
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
