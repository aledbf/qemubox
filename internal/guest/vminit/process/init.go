//go:build !windows

package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/containerd/console"
	"github.com/containerd/containerd/v2/core/mount"
	google_protobuf "github.com/containerd/containerd/v2/pkg/protobuf/types"
	"github.com/containerd/containerd/v2/pkg/stdio"
	runc "github.com/containerd/go-runc"
	"github.com/containerd/log"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/stream"
	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/systools"
)

// Init represents an initial process for a container
type Init struct {
	wg        sync.WaitGroup
	initState initState

	// mu is used to ensure that `Start()` and `Exited()` calls return in
	// the right order when invoked in separate goroutines.
	// This is the case within the shim implementation as it makes use of
	// the reaper interface.
	mu sync.Mutex

	waitBlock chan struct{}

	WorkDir string

	id           string
	Bundle       string
	console      console.Console
	Platform     stdio.Platform
	io           *processIO
	runtime      *runc.Runc
	status       int
	exited       time.Time
	pid          int
	closers      []io.Closer
	stdin        io.Closer
	stdio        stdio.Stdio
	Rootfs       string
	IoUID        int
	IoGID        int
	NoPivotRoot  bool
	NoNewKeyring bool
	CriuWorkPath string
	streams      stream.Manager
}

// NewRunc returns a new runc instance for a process
func NewRunc(root, path, runtime string, systemd bool) *runc.Runc {
	if root == "" {
		root = RuncRoot
	}
	return &runc.Runc{
		Command:       runtime,
		Debug:         true,
		Log:           filepath.Join(path, "log.json"),
		LogFormat:     runc.JSON,
		PdeathSignal:  unix.SIGKILL,
		Root:          root,
		SystemdCgroup: systemd,
	}
}

// New returns a new process
func New(id string, runtime *runc.Runc, stdio stdio.Stdio, sm stream.Manager) *Init {
	p := &Init{
		id:        id,
		runtime:   runtime,
		stdio:     stdio,
		status:    0,
		waitBlock: make(chan struct{}),
		streams:   sm,
	}
	p.initState = newInitStateMachine(p)
	return p
}

// Create the process with the provided config
func (p *Init) Create(ctx context.Context, r *CreateConfig) error {
	var (
		err              error
		socket           *runc.Socket
		pio              *processIO
		pidFile          = newPidFile(p.Bundle)
		retErr           error
		containerCreated bool
	)

	// Clean up container if it was created but later steps fail
	defer func() {
		if retErr != nil && containerCreated {
			// Container was created via runtime.Create() but something else failed.
			// We need to delete the container to avoid leaking resources.
			// Use context.WithoutCancel to ensure cleanup proceeds even if ctx is cancelled
			cleanupCtx := context.WithoutCancel(ctx)
			if err := p.runtime.Delete(cleanupCtx, r.ID, &runc.DeleteOpts{
				Force: true,
			}); err != nil {
				log.G(ctx).WithError(err).WithField("container_id", r.ID).
					Warn("failed to delete container during cleanup after partial create")
			}
		}
	}()

	if r.Terminal {
		if socket, err = runc.NewTempConsoleSocket(); err != nil {
			retErr = fmt.Errorf("failed to create OCI runtime console socket: %w", err)
			return retErr
		}
		defer func() { _ = socket.Close() }()
	} else {
		if pio, err = createIO(ctx, p.id, p.IoUID, p.IoGID, p.stdio, p.streams); err != nil {
			retErr = fmt.Errorf("failed to create init process I/O: %w", err)
			return retErr
		}
		p.io = pio
		defer func() {
			if retErr != nil && p.io != nil {
				_ = p.io.Close()
			}
		}()
	}

	// TODO(security): NoPivot is disabled due to EINVAL from pivot_root syscall.
	// Investigation needed:
	// 1. Is rootfs mount propagation set correctly? (should be MS_PRIVATE)
	// 2. Is rootfs on the same filesystem as old_root?
	// 3. Are we calling pivot_root with correct arguments?
	// See: https://man7.org/linux/man-pages/man2/pivot_root.2.html
	//
	// SECURITY RISK: Without pivot_root, container processes can access
	// VM filesystem outside the container rootfs. This reduces isolation.
	// The VM boundary provides isolation, but proper pivot_root is still
	// defense-in-depth and should be re-enabled.
	opts := &runc.CreateOpts{
		PidFile:      pidFile.Path(),
		NoPivot:      true, // FIXME: Re-enable after investigating EINVAL (see comment above)
		NoNewKeyring: p.NoNewKeyring,
	}
	if p.io != nil {
		opts.IO = p.io.IO()
	}
	if socket != nil {
		opts.ConsoleSocket = socket
	}

	if err := p.runtime.Create(context.WithoutCancel(ctx), r.ID, r.Bundle, opts); err != nil {
		systools.DumpFile(ctx, p.runtime.Log)
		retErr = p.runtimeError(err, "OCI runtime create failed")
		return retErr
	}
	// Mark container as created so defer cleanup can delete it if later steps fail
	containerCreated = true

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if socket != nil {
		console, err := socket.ReceiveMaster()
		if err != nil {
			retErr = fmt.Errorf("failed to retrieve console master: %w", err)
			return retErr
		}
		console, err = p.Platform.CopyConsole(ctx, console, p.id, r.Stdin, r.Stdout, r.Stderr, &p.wg)
		if err != nil {
			retErr = fmt.Errorf("failed to start console copy: %w", err)
			return retErr
		}
		p.console = console
		if sc, ok := console.(interface{ StdinCloser() io.Closer }); ok {
			c := sc.StdinCloser()
			p.stdin = c
			p.closers = append(p.closers, c)
		}
	} else {
		c, err := pio.Copy(ctx, &p.wg)
		if err != nil {
			retErr = fmt.Errorf("failed to start io pipe copy: %w", err)
			return retErr
		}
		if c != nil {
			p.stdin = c
			p.closers = append(p.closers, c)
		}
	}
	pid, err := pidFile.Read()
	if err != nil {
		retErr = fmt.Errorf("failed to retrieve OCI runtime container pid: %w", err)
		return retErr
	}
	p.pid = pid
	return nil
}

// Wait for the process to exit
func (p *Init) Wait() {
	<-p.waitBlock
}

// ID of the process
func (p *Init) ID() string {
	return p.id
}

// Pid of the process
func (p *Init) Pid() int {
	return p.pid
}

// ExitStatus of the process
func (p *Init) ExitStatus() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.status
}

// ExitedAt at time when the process exited
func (p *Init) ExitedAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.exited
}

// Status of the process
func (p *Init) Status(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.initState.Status(ctx)
}

// Start the init process
func (p *Init) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.initState.Start(ctx)
}

func (p *Init) start(ctx context.Context) error {
	if p.runtime == nil {
		return errors.New("runtime not initialized")
	}
	log.G(ctx).WithFields(log.Fields{
		"container_id": p.id,
		"bundle":       p.Bundle,
		"runtime":      p.runtime.Command,
	}).Info("executing OCI runtime start command")
	err := p.runtime.Start(ctx, p.id)
	if err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{
			"container_id": p.id,
		}).Error("OCI runtime start command failed")
	} else {
		log.G(ctx).WithFields(log.Fields{
			"container_id": p.id,
			"pid":          p.pid,
		}).Info("OCI runtime start command succeeded")
	}
	return p.runtimeError(err, "OCI runtime start failed")
}

// SetExited of the init process with the next status
func (p *Init) SetExited(status int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.initState.SetExited(status)
}

func (p *Init) setExited(status int) {
	p.exited = time.Now()
	p.status = status
	if p.Platform != nil {
		_ = p.Platform.ShutdownConsole(context.Background(), p.console)
	}
	if p.waitBlock != nil {
		close(p.waitBlock)
	}
}

// Delete the init process
func (p *Init) Delete(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.initState.Delete(ctx)
}

func (p *Init) delete(ctx context.Context) error {
	// Use the minimum of default timeout and context deadline
	timeout := 2 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}
	_ = waitTimeout(ctx, &p.wg, timeout)
	if p.runtime == nil {
		return errors.New("runtime not initialized")
	}
	err := p.runtime.Delete(ctx, p.id, nil)
	// ignore errors if a runtime has already deleted the process
	// but we still hold metadata and pipes
	//
	// this is common during a checkpoint, runc will delete the container state
	// after a checkpoint and the container will no longer exist within runc
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			err = nil
		} else {
			err = p.runtimeError(err, "failed to delete task")
		}
	}
	if p.io != nil {
		for _, c := range p.closers {
			_ = c.Close()
		}
		_ = p.io.Close()
	}
	if err2 := mount.UnmountRecursive(p.Rootfs, 0); err2 != nil {
		log.G(ctx).WithError(err2).Warn("failed to cleanup rootfs mount")
		if err == nil {
			err = fmt.Errorf("failed rootfs umount: %w", err2)
		}
	}
	return err
}

// Resize the init processes console
func (p *Init) Resize(ws console.WinSize) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.console == nil {
		return nil
	}
	return p.console.Resize(ws)
}

// Kill the init process
func (p *Init) Kill(ctx context.Context, signal uint32, all bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.initState.Kill(ctx, signal, all)
}

func (p *Init) kill(ctx context.Context, signal uint32, all bool) error {
	if p.runtime == nil {
		return errors.New("runtime not initialized")
	}
	err := p.runtime.Kill(ctx, p.id, int(signal), &runc.KillOpts{
		All: all,
	})
	return checkKillError(err)
}

// KillAll processes belonging to the init process
func (p *Init) KillAll(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.runtime == nil {
		return errors.New("runtime not initialized")
	}
	err := p.runtime.Kill(ctx, p.id, int(unix.SIGKILL), &runc.KillOpts{
		All: true,
	})
	return p.runtimeError(err, "OCI runtime killall failed")
}

// Stdin of the process
func (p *Init) Stdin() io.Closer {
	return p.stdin
}

// Runtime returns the OCI runtime configured for the init process
func (p *Init) Runtime() *runc.Runc {
	return p.runtime
}

// Exec returns a new child process
func (p *Init) Exec(ctx context.Context, path string, r *ExecConfig) (Process, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.initState.Exec(ctx, path, r)
}

// exec returns a new exec'd process
func (p *Init) exec(_ context.Context, path string, r *ExecConfig) (Process, error) {
	// process exec request
	var spec specs.Process
	if err := json.Unmarshal(r.Spec.Value, &spec); err != nil {
		return nil, err
	}
	spec.Terminal = r.Terminal

	e := &execProcess{
		id:     r.ID,
		path:   path,
		parent: p,
		spec:   spec,
		stdio: stdio.Stdio{
			Stdin:    r.Stdin,
			Stdout:   r.Stdout,
			Stderr:   r.Stderr,
			Terminal: r.Terminal,
		},
		waitBlock: make(chan struct{}),
	}
	e.execState = newExecStateMachine(e)
	return e, nil
}

// Update the processes resource configuration
func (p *Init) Update(ctx context.Context, r *google_protobuf.Any) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.initState.Update(ctx, r)
}

func (p *Init) update(ctx context.Context, r *google_protobuf.Any) error {
	var resources specs.LinuxResources
	if err := json.Unmarshal(r.Value, &resources); err != nil {
		return err
	}
	return p.runtime.Update(ctx, p.id, &resources)
}

// Stdio of the process
func (p *Init) Stdio() stdio.Stdio {
	return p.stdio
}

// IsInit returns true since this is the init process
func (p *Init) IsInit() bool {
	return true
}

func (p *Init) runtimeError(rErr error, msg string) error {
	if rErr == nil {
		return nil
	}

	rMsg, err := getLastRuntimeError(p.runtime)
	switch {
	case err != nil:
		return fmt.Errorf("%s: %s (%s): %w", msg, "unable to retrieve OCI runtime error", err.Error(), rErr)
	case rMsg == "":
		return fmt.Errorf("%s: %w", msg, rErr)
	default:
		return fmt.Errorf("%s: %s", msg, rMsg)
	}
}

func withConditionalIO(c stdio.Stdio) runc.IOOpt {
	return func(o *runc.IOOption) {
		o.OpenStdin = c.Stdin != ""
		o.OpenStdout = c.Stdout != ""
		o.OpenStderr = c.Stderr != ""
	}
}
