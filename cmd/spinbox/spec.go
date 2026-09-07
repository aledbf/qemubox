//go:build linux

package main

import (
	"encoding/json"
	"fmt"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// The OCI spec, written here rather than generated.
//
// containerd's oci package would produce one in three lines, and this command
// used it at first. It is the wrong dependency for a command whose purpose is to
// run a container without containerd: the generator refuses to work without a
// containerd namespace ("namespace is required"), because in containerd every
// spec belongs to one - and there is no namespace here, nothing else on this
// host knows this container exists.
//
// What crun consumes is an OCI runtime spec, and that is an OCI standard rather
// than a containerd one. So the spec is built against runtime-spec directly. It
// is longer, and every line of it is a decision this command is now making on
// purpose instead of inheriting.

const (
	// optNoDev, optNoSuid and optNoExec are the mount options every OCI
	// container gets on its pseudo-filesystems.
	optNoDev  = "nodev"
	optNoSuid = "nosuid"
	optNoExec = "noexec"
)

// defaultPath is what a process gets when the image does not say otherwise.
const defaultPath = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// containerSpec builds the OCI spec for the one process this command runs.
func containerSpec(cmd []string, hostname string) ([]byte, error) {
	if len(cmd) == 0 {
		return nil, fmt.Errorf("a container needs a command")
	}

	spec := &specs.Spec{
		Version:  specs.Version,
		Hostname: hostname,
		// Run without an OCI runtime: chroot and exec, rather than crun building
		// namespaces and cgroups inside a VM that is already the boundary. See
		// process.Direct in the guest for what that gives up.
		Annotations: map[string]string{"io.spin.exec.direct": "true"},
		Root: &specs.Root{
			// Relative to the bundle, which is where the guest mounts the
			// container's filesystem.
			Path: "rootfs",
		},
		Process: &specs.Process{
			Args: cmd,
			Cwd:  "/",
			Env:  []string{defaultPath, "TERM=xterm"},
			User: specs.User{UID: 0, GID: 0},
			// The bounding set a container gets by default. It is the docker
			// default set minus nothing: this command runs a workload the caller
			// chose on a machine of their own, inside a VM, and the VM is the
			// isolation boundary here rather than the capability set.
			Capabilities: &specs.LinuxCapabilities{
				Bounding:  defaultCapabilities,
				Effective: defaultCapabilities,
				Permitted: defaultCapabilities,
			},
			NoNewPrivileges: true,
		},
		Linux: &specs.Linux{
			// No network namespace. The container shares the VM's, which has
			// whatever network the VM was given - none, at the moment. A
			// container in its own netns inside a VM would need something to
			// wire the two together, which is the job CNI used to do on the
			// host and which nothing does here yet.
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace},
				{Type: specs.IPCNamespace},
				{Type: specs.UTSNamespace},
				{Type: specs.MountNamespace},
			},
			MaskedPaths: []string{
				"/proc/acpi", "/proc/kcore", "/proc/keys", "/proc/latency_stats",
				"/proc/timer_list", "/proc/timer_stats", "/proc/sched_debug",
				"/proc/scsi", "/sys/firmware", "/sys/devices/virtual/powercap",
			},
			ReadonlyPaths: []string{
				"/proc/asound", "/proc/bus", "/proc/fs", "/proc/irq",
				"/proc/sys", "/proc/sysrq-trigger",
			},
		},
		Mounts: defaultMounts(),
	}

	return json.Marshal(spec)
}

// defaultMounts is the filesystem every OCI container expects to find.
//
// /etc/resolv.conf is deliberately absent: this VM has no network, so there is
// nothing to resolve with and nothing to bind. The guest adds one when the VM
// has one.
func defaultMounts() []specs.Mount {
	return []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc",
			Options: []string{optNoSuid, optNoExec, optNoDev}},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs",
			Options: []string{optNoSuid, "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts",
			Options: []string{optNoSuid, optNoExec, "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm",
			Options: []string{optNoSuid, optNoExec, optNoDev, "mode=1777", "size=65536k"}},
		{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue",
			Options: []string{optNoSuid, optNoExec, optNoDev}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs",
			Options: []string{optNoSuid, optNoExec, optNoDev, "ro"}},
	}
}

// defaultCapabilities is the set a container starts with.
var defaultCapabilities = []string{
	"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FSETID", "CAP_FOWNER",
	"CAP_MKNOD", "CAP_NET_RAW", "CAP_SETGID", "CAP_SETUID",
	"CAP_SETFCAP", "CAP_SETPCAP", "CAP_NET_BIND_SERVICE",
	"CAP_SYS_CHROOT", "CAP_KILL", "CAP_AUDIT_WRITE",
}
