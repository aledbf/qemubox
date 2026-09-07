//go:build linux

package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/console"
	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/containerd/v2/pkg/sys/reaper"
	"github.com/containerd/errdefs"
	runc "github.com/containerd/go-runc"
	"github.com/containerd/log"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/spin-stack/spinbox/internal/guest/vminit/stream"
)

// Running a container without an OCI runtime.
//
// The runtime this replaces - crun, through go-runc - builds namespaces,
// cgroups, a capability set, a seccomp filter and a pivot_root around the
// process it starts. Every one of those is a boundary drawn inside a boundary:
// the container is already alone in a VM, which is where this project puts its
// isolation ("VM isolation is the primary security boundary", root CLAUDE.md).
// What the second boundary buys, in here, is very little, and what it costs is a
// runtime binary in the initrd, an OCI bundle, a spec to write and transform,
// and the time to fork it all.
//
// So this starts the process directly: chroot into the rootfs, set the
// environment the image asked for, exec. It is a few syscalls where crun was a
// program.
//
// # What is given up, and why it is affordable
//
// No new namespaces. The container shares the VM's PID, mount, IPC, UTS and
// network namespaces, so it can see the guest's other processes - vminitd, and
// anything else the VM runs. Inside a VM that holds exactly one workload, the
// list of things it could see is the list of things put there for it.
//
// No cgroups, so nothing limits the process below what the VM itself has. The
// VM's own -m and -smp are the limit, which is where a caller sets it anyway.
//
// No seccomp and the capability set of vminitd, which is root's. A workload that
// wants to break out has a kernel to attack either way, and it is the guest's
// kernel, not the host's.
//
// If any of those turns out to matter, the answer is the namespace or the cgroup
// on its own, not the runtime that comes with all of them.

// Direct is a container process started by exec rather than by an OCI runtime.
type Direct struct {
	mu        sync.Mutex
	waitBlock chan struct{}

	id     string
	bundle string
	rootfs string
	spec   *specs.Spec

	io    *processIO
	stdio stdio.Stdio

	cmd    *exec.Cmd
	exitCh chan runc.Exit
	wg     sync.WaitGroup

	stdinCloser io.Closer

	pid     int
	status  int
	exited  time.Time
	started bool
	deleted bool
}

var _ Process = (*Direct)(nil)

// NewDirect prepares a container process that will be started with exec.
func NewDirect(ctx context.Context, id, bundle, rootfs string, sio stdio.Stdio, streams stream.Manager) (*Direct, error) {
	spec, err := readSpec(bundle)
	if err != nil {
		return nil, err
	}
	if spec.Process == nil || len(spec.Process.Args) == 0 {
		return nil, fmt.Errorf("%w: the spec has no process to run", errdefs.ErrInvalidArgument)
	}

	pio, err := createIO(ctx, id, 0, 0, sio, streams)
	if err != nil {
		return nil, fmt.Errorf("preparing the process's stdio: %w", err)
	}

	return &Direct{
		waitBlock: make(chan struct{}),
		id:        id,
		bundle:    bundle,
		rootfs:    rootfs,
		spec:      spec,
		io:        pio,
		stdio:     sio,
	}, nil
}

// readSpec loads the OCI spec the caller wrote into the bundle.
//
// The spec is still the input, because it is how a caller says what to run - the
// arguments, the environment, the working directory, the user. What has gone is
// the program that used to consume it.
func readSpec(bundle string) (*specs.Spec, error) {
	b, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("reading the container spec: %w", err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(b, &spec); err != nil {
		return nil, fmt.Errorf("parsing the container spec: %w", err)
	}
	return &spec, nil
}

// Start makes the container's filesystem ready and execs its process.
func (d *Direct) Start(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.started {
		return fmt.Errorf("%w: container %s already started", errdefs.ErrFailedPrecondition, d.id)
	}

	if err := d.prepareRootfs(ctx); err != nil {
		return err
	}

	p := d.spec.Process
	// #nosec G204 -- the command is what the caller asked to run; that is the
	// entire purpose of this process.
	cmd := exec.Command(p.Args[0], p.Args[1:]...)
	cmd.Env = p.Env
	cmd.Dir = "/"
	if p.Cwd != "" {
		cmd.Dir = p.Cwd
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// Chroot rather than pivot_root: the rootfs is a directory this process
		// mounted and the container is the only thing that will look at it.
		Chroot: d.rootfs,
		// Its own session, so a signal sent to vminitd's process group does not
		// reach the workload, and so killing the container can name a group.
		Setsid: true,
		Credential: &syscall.Credential{
			Uid: p.User.UID,
			Gid: p.User.GID,
		},
	}
	// The child writes into the pipes createIO made, and Copy pumps those to
	// wherever the caller asked for the output to go - a vsock stream, a FIFO.
	// Same machinery the OCI runtime path uses; only the thing on the far end of
	// the pipes has changed.
	if rio := d.io.IO(); rio != nil {
		rio.Set(cmd)
	}
	stdinCloser, err := d.io.Copy(ctx, &d.wg)
	if err != nil {
		return fmt.Errorf("starting the stdio copy: %w", err)
	}
	d.stdinCloser = stdinCloser

	// Started through the reaper because vminitd is PID 1: it reaps every child
	// on SIGCHLD, and a plain cmd.Wait would race it for the exit status and
	// usually lose. The reaper hands the status to whoever registered the pid.
	exitCh, startErr := reaper.Default.Start(cmd)
	if startErr != nil {
		return fmt.Errorf("starting %s: %w", p.Args[0], startErr)
	}

	d.cmd = cmd
	d.exitCh = exitCh
	d.pid = cmd.Process.Pid
	d.started = true

	log.G(ctx).WithFields(log.Fields{
		"id": d.id, "pid": d.pid, "args": p.Args,
	}).Info("started container process without an OCI runtime")

	go d.reap()
	return nil
}

// reap waits for the process and records what it exited with.
func (d *Direct) reap() {
	status, err := reaper.Default.Wait(d.cmd, d.exitCh)
	if err != nil {
		log.L.WithError(err).WithField("id", d.id).Warn("waiting for the container process")
		status = 255
	}

	// The output is closed after the process is gone, so a reader on the other
	// side sees end-of-file only once there is nothing more to come.
	if d.io != nil {
		d.io.Close()
	}
	d.markExited(status)
}

// markExited records the exit once, whichever of the two paths gets there
// first: this process reaping its own child, or the task service telling it
// about an exit it saw.
//
// Closing waitBlock twice panics, and a panic here is not a failed container -
// vminitd is PID 1, so the kernel takes it as init dying and reboots the VM. It
// showed up as "guest reset/reboot detected" from the host and nothing at all
// from the guest, whose console went with it.
func (d *Direct) markExited(status int) {
	d.mu.Lock()
	if !d.exited.IsZero() {
		d.mu.Unlock()
		return
	}
	d.status = status
	d.exited = time.Now()
	d.mu.Unlock()

	close(d.waitBlock)
}

// prepareRootfs mounts what an OCI container expects to find.
//
// The mounts are made from here rather than from inside a new mount namespace,
// because this process does not create one - see the note at the top of the
// file. They are made under the rootfs directory, which the chroot then makes
// the container's root.
func (d *Direct) prepareRootfs(ctx context.Context) error {
	type mount struct {
		target, fstype, source string
		flags                  uintptr
		data                   string
	}
	const (
		noSuidNoExecNoDev = syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV
		// procfs is the one mount a container cannot do without: anything that
		// reads its own state needs it.
		procfs = "proc"
	)

	for _, m := range []mount{
		{target: procfs, fstype: procfs, source: procfs, flags: noSuidNoExecNoDev},
		{target: "sys", fstype: "sysfs", source: "sysfs", flags: noSuidNoExecNoDev | syscall.MS_RDONLY},
		{target: "dev", fstype: "", source: "/dev", flags: syscall.MS_BIND | syscall.MS_REC},
	} {
		target := filepath.Join(d.rootfs, m.target)
		// #nosec G301 -- these are the standard modes for these directories.
		if err := os.MkdirAll(target, 0755); err != nil {
			return fmt.Errorf("creating %s in the container: %w", m.target, err)
		}
		if mounted, err := isMounted(target); err != nil {
			return err
		} else if mounted {
			continue
		}
		if err := syscall.Mount(m.source, target, m.fstype, m.flags, m.data); err != nil {
			// /sys and /dev are conveniences; /proc is not optional, since
			// anything that reads its own state needs it.
			if m.target == procfs {
				return fmt.Errorf("mounting /proc in the container: %w", err)
			}
			log.G(ctx).WithError(err).WithField("mount", m.target).
				Warn("could not mount this into the container; continuing")
		}
	}
	return nil
}

// isMounted reports whether something is already mounted at path, so that
// starting a second process in a prepared rootfs does not stack mounts.
func isMounted(path string) (bool, error) {
	var st, parent syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return false, fmt.Errorf("checking %s: %w", path, err)
	}
	if err := syscall.Stat(filepath.Dir(path), &parent); err != nil {
		return false, fmt.Errorf("checking %s: %w", filepath.Dir(path), err)
	}
	return st.Dev != parent.Dev, nil
}

func (d *Direct) ID() string { return d.id }

func (d *Direct) Pid() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pid
}

func (d *Direct) ExitStatus() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}

func (d *Direct) ExitedAt() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.exited
}

func (d *Direct) Stdin() io.Closer {
	return d.stdinCloser
}

func (d *Direct) Stdio() stdio.Stdio { return d.stdio }

func (d *Direct) Status(context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case d.deleted:
		return "deleted", nil
	case !d.started:
		return "created", nil
	case d.exited.IsZero():
		return "running", nil
	default:
		return "stopped", nil
	}
}

// Wait blocks until the process has exited.
func (d *Direct) Wait() { <-d.waitBlock }

// Resize is a no-op: this process has no console of its own.
//
// A terminal would need a pty allocated here and its master handed to the
// stdio streams, which nothing that drives this path asks for yet. It returns
// nil rather than an error so that a caller resizing a window does not fail a
// container that simply has no window.
func (d *Direct) Resize(console.WinSize) error { return nil }

// Kill signals the process, or its whole session when all is set.
func (d *Direct) Kill(_ context.Context, sig uint32, all bool) error {
	d.mu.Lock()
	pid, started, exited := d.pid, d.started, !d.exited.IsZero()
	d.mu.Unlock()

	if !started || exited {
		return fmt.Errorf("%w: container %s is not running", errdefs.ErrNotFound, d.id)
	}

	target := pid
	if all {
		// Setsid made the process a group leader, so the negative pid reaches
		// everything it started.
		target = -pid
	}
	if err := syscall.Kill(target, syscall.Signal(sig)); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("%w: container %s is gone", errdefs.ErrNotFound, d.id)
		}
		return fmt.Errorf("signalling container %s: %w", d.id, err)
	}
	return nil
}

// Delete releases what Start took.
func (d *Direct) Delete(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.started && d.exited.IsZero() {
		return fmt.Errorf("%w: container %s is still running", errdefs.ErrFailedPrecondition, d.id)
	}
	if d.io != nil {
		d.io.Close()
	}
	d.deleted = true
	return nil
}

// SetExited records an exit status observed elsewhere.
func (d *Direct) SetExited(status int) { d.markExited(status) }

// IsInit reports that this is a container's main process. It is the only kind
// this type runs; additional processes still go through the runtime.
func (d *Direct) IsInit() bool { return true }

// KillAll signals everything the container started.
//
// Setsid made the process a session and group leader, so its descendants are in
// its process group and a single negative pid reaches all of them - which is
// what the OCI runtime used a cgroup for.
func (d *Direct) KillAll(ctx context.Context) error {
	return d.Kill(ctx, uint32(syscall.SIGKILL), true)
}
