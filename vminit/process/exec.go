//go:build !windows

package process

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/containerd/console"
	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	runc "github.com/containerd/go-runc"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

type execProcess struct {
	wg sync.WaitGroup

	execState execState

	mu      sync.Mutex
	id      string
	console console.Console
	io      *processIO
	status  int
	exited  time.Time
	pid     safePid
	closers []io.Closer
	stdin   io.Closer
	stdio   stdio.Stdio
	path    string
	spec    specs.Process

	parent    *Init
	waitBlock chan struct{}
}

func (e *execProcess) Wait() {
	<-e.waitBlock
}

func (e *execProcess) ID() string {
	return e.id
}

func (e *execProcess) Pid() int {
	return e.pid.get()
}

func (e *execProcess) ExitStatus() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status
}

func (e *execProcess) ExitedAt() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.exited
}

func (e *execProcess) SetExited(status int) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.execState.SetExited(status)
}

func (e *execProcess) setExited(status int) {
	e.status = status
	e.exited = time.Now()
	_ = e.parent.Platform.ShutdownConsole(context.Background(), e.console)
	close(e.waitBlock)
}

func (e *execProcess) Delete(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.execState.Delete(ctx)
}

func (e *execProcess) delete(ctx context.Context) error {
	_ = waitTimeout(ctx, &e.wg, 2*time.Second)
	if e.io != nil {
		for _, c := range e.closers {
			_ = c.Close()
		}
		_ = e.io.Close()
	}
	pidfile := filepath.Join(e.path, fmt.Sprintf("%s.pid", e.id))
	// silently ignore error
	_ = os.Remove(pidfile)
	return nil
}

func (e *execProcess) Resize(ws console.WinSize) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.execState.Resize(ws)
}

func (e *execProcess) resize(ws console.WinSize) error {
	if e.console == nil {
		return nil
	}
	return e.console.Resize(ws)
}

func (e *execProcess) Kill(ctx context.Context, sig uint32, _ bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.execState.Kill(ctx, sig, false)
}

func (e *execProcess) kill(_ context.Context, sig uint32, _ bool) error {
	pid := e.pid.get()
	switch {
	case pid == 0:
		return fmt.Errorf("process not created: %w", errdefs.ErrFailedPrecondition)
	case !e.exited.IsZero():
		return fmt.Errorf("process already finished: %w", errdefs.ErrNotFound)
	default:
		if err := unix.Kill(pid, syscall.Signal(sig)); err != nil {
			return fmt.Errorf("exec kill error: %w", checkKillError(err))
		}
	}
	return nil
}

func (e *execProcess) Stdin() io.Closer {
	return e.stdin
}

func (e *execProcess) Stdio() stdio.Stdio {
	return e.stdio
}

func (e *execProcess) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.execState.Start(ctx)
}

func (e *execProcess) start(ctx context.Context) error {
	// The reaper may receive exit signal right after
	// the container is started, before the e.pid is updated.
	// In that case, we want to block the signal handler to
	// access e.pid until it is updated.
	e.pid.Lock()
	defer e.pid.Unlock()

	var (
		socket  *runc.Socket
		pio     *processIO
		pidFile = newExecPidFile(e.path, e.id)
		err     error
	)

	if e.stdio.Terminal {
		if socket, err = runc.NewTempConsoleSocket(); err != nil {
			err = fmt.Errorf("failed to create runc console socket: %w", err)
			return err
		}
		defer func() { _ = socket.Close() }()
	} else {
		if pio, err = createIO(ctx, e.id, e.parent.IoUID, e.parent.IoGID, e.stdio, e.parent.streams); err != nil {
			err = fmt.Errorf("failed to create init process I/O: %w", err)
			return err
		}
		e.io = pio
		defer func() {
			if err != nil && e.io != nil {
				_ = e.io.Close()
			}
		}()
	}

	opts := &runc.ExecOpts{
		PidFile: pidFile.Path(),
		Detach:  true,
	}
	if pio != nil {
		opts.IO = pio.IO()
	}
	if socket != nil {
		opts.ConsoleSocket = socket
	}
	if execErr := e.parent.runtime.Exec(ctx, e.parent.id, e.spec, opts); execErr != nil {
		close(e.waitBlock)
		err = e.parent.runtimeError(execErr, "OCI runtime exec failed")
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if socket != nil {
		console, err := socket.ReceiveMaster()
		if err != nil {
			err = fmt.Errorf("failed to retrieve console master: %w", err)
			return err
		}
		if e.console, err = e.parent.Platform.CopyConsole(ctx, console, e.id, e.stdio.Stdin, e.stdio.Stdout, e.stdio.Stderr, &e.wg); err != nil {
			err = fmt.Errorf("failed to start console copy: %w", err)
			return err
		}
		if sc, ok := console.(interface{ StdinCloser() io.Closer }); ok {
			c := sc.StdinCloser()
			e.stdin = c
			e.closers = append(e.closers, c)
		}
	} else {
		c, copyErr := pio.Copy(ctx, &e.wg)
		if copyErr != nil {
			err = fmt.Errorf("failed to start io pipe copy: %w", copyErr)
			return err
		}
		if c != nil {
			e.stdin = c
			e.closers = append(e.closers, c)
		}
	}
	pid, err := pidFile.Read()
	if err != nil {
		err = fmt.Errorf("failed to retrieve OCI runtime exec pi: %w", err)
		return err
	}
	e.pid.pid = pid
	return nil
}

func (e *execProcess) Status(ctx context.Context) (string, error) {
	s, err := e.parent.Status(ctx)
	if err != nil {
		return "", err
	}
	// if the container as a whole is in the pausing/paused state, so are all
	// other processes inside the container, use container state here
	switch s {
	case statePaused, "pausing":
		return s, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.execState.Status(ctx)
}
