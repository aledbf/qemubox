//go:build !windows

package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	runc "github.com/containerd/go-runc"
	"golang.org/x/sys/unix"

	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/stream"
)

const (
	// RuncRoot is the path to the root runc state directory
	RuncRoot = "/run/runc"
	// InitPidFile name of the file that contains the init pid
	InitPidFile = "init.pid"
)

// safePid is a thread safe wrapper for pid.
type safePid struct {
	sync.Mutex
	pid int
}

func (s *safePid) get() int {
	s.Lock()
	defer s.Unlock()
	return s.pid
}

// Note: consider moving this to the runc package.
func getLastRuntimeError(r *runc.Runc) (string, error) {
	if r.Log == "" {
		return "", nil
	}

	f, err := os.OpenFile(r.Log, os.O_RDONLY, 0400)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	var (
		errMsg string
		log    struct {
			Level string    `json:"level"`
			Msg   string    `json:"msg"`
			Time  time.Time `json:"time"`
		}
	)

	dec := json.NewDecoder(f)
	for err = nil; err == nil; {
		if err = dec.Decode(&log); err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		if log.Level == "error" {
			errMsg = strings.TrimSpace(log.Msg)
		}
	}

	return errMsg, nil
}

func checkKillError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "os: process already finished") ||
		strings.Contains(err.Error(), "container not running") ||
		strings.Contains(strings.ToLower(err.Error()), "no such process") ||
		errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("process already finished: %w", errdefs.ErrNotFound)
	} else if strings.Contains(err.Error(), "does not exist") {
		return fmt.Errorf("no such container: %w", errdefs.ErrNotFound)
	}
	return fmt.Errorf("unknown error after kill: %w", err)
}

func newPidFile(bundle string) *pidFile {
	return &pidFile{
		path: filepath.Join(bundle, InitPidFile),
	}
}

func newExecPidFile(bundle, id string) *pidFile {
	return &pidFile{
		path: filepath.Join(bundle, fmt.Sprintf("%s.pid", id)),
	}
}

type pidFile struct {
	path string
}

func (p *pidFile) Path() string {
	return p.path
}

func (p *pidFile) Read() (int, error) {
	return runc.ReadPidFile(p.path)
}

// waitTimeout handles waiting on a waitgroup with a specified timeout.
// this is commonly used for waiting on IO to finish after a process has exited
func waitTimeout(ctx context.Context, wg *sync.WaitGroup, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// State represents a process state using a typed enum for compile-time safety
type State uint8

const (
	// StateCreated indicates the process has been created but not started
	StateCreated State = iota
	// StateRunning indicates the process is currently running
	StateRunning
	// StateStopped indicates the process has exited
	StateStopped
	// StateDeleted indicates the process has been deleted
	StateDeleted
)

// String implements fmt.Stringer for State
func (s State) String() string {
	switch s {
	case StateCreated:
		return "created"
	case StateRunning:
		return "running"
	case StateStopped:
		return "stopped"
	case StateDeleted:
		return "deleted"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

func getStreams(stdio stdio.Stdio, sm stream.Manager) ([3]io.ReadWriteCloser, error) {
	var streams [3]io.ReadWriteCloser
	var err error
	if stdio.Stdin != "" {
		streams[0], err = getStream(stdio.Stdin, sm)
		if err != nil {
			return streams, fmt.Errorf("failed to get stdin stream: %w", err)
		}
	}
	if stdio.Stdout != "" {
		streams[1], err = getStream(stdio.Stdout, sm)
		if err != nil {
			if streams[0] != nil {
				_ = streams[0].Close()
			}
			return streams, fmt.Errorf("failed to get stdout stream: %w", err)
		}
	}
	if stdio.Stderr != "" {
		streams[2], err = getStream(stdio.Stderr, sm)
		if err != nil {
			if streams[0] != nil {
				_ = streams[0].Close()
			}
			if streams[1] != nil {
				_ = streams[1].Close()
			}
			return streams, fmt.Errorf("failed to get stderr stream: %w", err)
		}
	}
	return streams, nil
}

func getStream(uri string, sm stream.Manager) (io.ReadWriteCloser, error) {
	if !strings.HasPrefix(uri, "stream://") {
		return nil, fmt.Errorf("not a stream: %w", errdefs.ErrInvalidArgument)
	}
	sid, err := strconv.ParseUint(uri[9:], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid stream id %q: %w", uri, err)
	}
	c, err := sm.Get(uint32(sid))
	if err != nil {
		return nil, fmt.Errorf("unable to get stream %d: %w", sid, errdefs.ErrNotFound)
	}
	return c, err
}
