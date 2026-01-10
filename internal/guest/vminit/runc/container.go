//go:build linux

package runc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/containerd/console"
	"github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types/runc/options"
	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/typeurl/v2"

	"github.com/spin-stack/spinbox/internal/guest/vminit/process"
	"github.com/spin-stack/spinbox/internal/guest/vminit/stream"
	"github.com/spin-stack/spinbox/internal/host/mountutil"
)

// defaultRuntimePath is the fallback OCI runtime path.
const defaultRuntimePath = "/sbin/crun"

// runtimePathOnce ensures getRuntimePath() only runs filesystem checks once.
var (
	runtimePathOnce     sync.Once
	resolvedRuntimePath string
)

// getRuntimePath returns the OCI runtime path to use.
// It checks the SPINBOX_OCI_RUNTIME environment variable first,
// then tries common locations, falling back to /sbin/crun.
func getRuntimePath() string {
	runtimePathOnce.Do(func() {
		// Check environment variable first
		if path := os.Getenv("SPINBOX_OCI_RUNTIME"); path != "" {
			resolvedRuntimePath = path
			return
		}
		// Try common locations
		for _, p := range []string{"/sbin/crun", "/usr/bin/crun", "/usr/local/bin/crun", "/sbin/runc", "/usr/bin/runc"} {
			if _, err := os.Stat(p); err == nil {
				resolvedRuntimePath = p
				return
			}
		}
		// Fallback to default
		resolvedRuntimePath = defaultRuntimePath
	})
	return resolvedRuntimePath
}

// NewContainer returns a new runc container
func NewContainer(ctx context.Context, platform stdio.Platform, r *task.CreateTaskRequest, streams stream.Manager) (*Container, error) {
	opts := &options.Options{}
	if r.Options.GetValue() != nil {
		v, err := typeurl.UnmarshalAny(r.Options)
		if err != nil {
			return nil, err
		}
		if v != nil {
			parsed, ok := v.(*options.Options)
			if !ok {
				return nil, fmt.Errorf("unexpected options type %T", v)
			}
			opts = parsed
		}
	}

	var pmounts []process.Mount
	for _, m := range r.Rootfs {
		pmounts = append(pmounts, process.Mount{
			Type:    m.Type,
			Source:  m.Source,
			Target:  m.Target,
			Options: m.Options,
		})
	}

	rootfs := ""
	if len(pmounts) > 0 {
		rootfs = filepath.Join(r.Bundle, "rootfs")
		if err := os.Mkdir(rootfs, 0750); err != nil && !os.IsExist(err) {
			return nil, err
		}
	}

	config := &process.CreateConfig{
		ID:               r.ID,
		Bundle:           r.Bundle,
		Runtime:          getRuntimePath(),
		Rootfs:           pmounts,
		Terminal:         r.Terminal,
		Stdin:            r.Stdin,
		Stdout:           r.Stdout,
		Stderr:           r.Stderr,
		Checkpoint:       r.Checkpoint,
		ParentCheckpoint: r.ParentCheckpoint,
		Options:          r.Options,
	}

	if err := WriteOptions(r.Bundle, opts); err != nil {
		return nil, err
	}

	var mountCleanup func(context.Context) error
	if len(r.Rootfs) != 0 && (len(r.Rootfs) != 1 || r.Rootfs[0].Type != "bind" || r.Rootfs[0].Source != rootfs) {
		log.G(ctx).WithField("mounts", r.Rootfs).Info("mounting rootfs components")
		mdir := filepath.Join(r.Bundle, "mounts")
		var err error
		mountCleanup, err = mountutil.All(ctx, rootfs, mdir, r.Rootfs)
		if err != nil {
			return nil, err
		}
		log.G(ctx).WithField("rootfs", rootfs).Info("rootfs components mounted")
	}

	// Relax OCI spec restrictions - VM provides the security boundary
	if err := RelaxOCISpec(ctx, r.Bundle); err != nil {
		log.G(ctx).WithError(err).Warn("failed to relax OCI spec")
	}

	p := newInit(
		r.Bundle,
		filepath.Join(r.Bundle, "work"),
		platform,
		config,
		opts,
		rootfs,
		streams,
	)
	if err := p.Create(ctx, config); err != nil {
		if mountCleanup != nil {
			_ = mountCleanup(context.WithoutCancel(ctx))
		}
		return nil, err
	}
	container := &Container{
		ID:              r.ID,
		Bundle:          r.Bundle,
		process:         p,
		processes:       make(map[string]process.Process),
		reservedProcess: make(map[string]struct{}),
		mountCleanup:    mountCleanup,
	}
	pid := p.Pid()
	if pid > 0 {
		if cg, err := loadProcessCgroup(ctx, pid); err == nil {
			container.cgroup = cg
		}
	}
	return container, nil
}

const optionsFilename = "options.json"

// ReadOptions reads the option information from the path.
// When the file does not exist, ReadOptions returns nil without an error.
func ReadOptions(path string) (*options.Options, error) {
	filePath := filepath.Join(path, optionsFilename)
	if _, err := os.Stat(filePath); err != nil {
		if os.IsNotExist(err) {
			return &options.Options{}, nil
		}
		return nil, err
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	var opts options.Options
	if err := json.Unmarshal(data, &opts); err != nil {
		return nil, err
	}
	return &opts, nil
}

// WriteOptions writes the options information into the path
func WriteOptions(path string, opts *options.Options) error {
	data, err := json.Marshal(opts)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(path, optionsFilename), data, 0600)
}

func newInit(path, workDir string, platform stdio.Platform,
	r *process.CreateConfig, options *options.Options, rootfs string, streams stream.Manager) *process.Init {
	runtime := process.NewRunc(options.Root, path, getRuntimePath(), options.SystemdCgroup)

	p := process.New(r.ID, runtime, stdio.Stdio{
		Stdin:    r.Stdin,
		Stdout:   r.Stdout,
		Stderr:   r.Stderr,
		Terminal: r.Terminal,
	}, streams)
	p.Bundle = r.Bundle
	p.Platform = platform
	p.Rootfs = rootfs
	p.WorkDir = workDir
	p.IoUID = int(options.IoUid)
	p.IoGID = int(options.IoGid)
	p.NoPivotRoot = options.NoPivotRoot
	p.NoNewKeyring = options.NoNewKeyring
	p.CriuWorkPath = options.CriuWorkPath
	if p.CriuWorkPath == "" {
		// if criu work path not set, use container WorkDir
		p.CriuWorkPath = p.WorkDir
	}
	return p
}

// Container for operating on a runc container and its processes
type Container struct {
	mu sync.Mutex

	// ID of the container
	ID string
	// Bundle path
	Bundle string

	// cgroup manager abstracts cgroup v1 and v2 operations
	cgroup          CgroupManager
	process         process.Process
	processes       map[string]process.Process
	reservedProcess map[string]struct{}
	mountCleanup    func(context.Context) error
}

// All processes in the container
func (c *Container) All() []process.Process {
	c.mu.Lock()
	defer c.mu.Unlock()

	var o []process.Process
	for _, p := range c.processes {
		o = append(o, p)
	}
	if c.process != nil {
		o = append(o, c.process)
	}
	return o
}

// ExecdProcesses added to the container
func (c *Container) ExecdProcesses() []process.Process {
	c.mu.Lock()
	defer c.mu.Unlock()
	var o []process.Process
	for _, p := range c.processes {
		o = append(o, p)
	}
	return o
}

// Pid of the main process of a container
func (c *Container) Pid() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.process.Pid()
}

// Cgroup of the container
func (c *Container) Cgroup() CgroupManager {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cgroup
}

// CgroupSet sets the cgroup to the container
func (c *Container) CgroupSet(cg CgroupManager) {
	c.mu.Lock()
	c.cgroup = cg
	c.mu.Unlock()
}

// Process returns the process by id
func (c *Container) Process(id string) (process.Process, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == "" {
		if c.process == nil {
			return nil, fmt.Errorf("container must be created: %w", errdefs.ErrFailedPrecondition)
		}
		return c.process, nil
	}
	p, ok := c.processes[id]
	if !ok {
		return nil, fmt.Errorf("process does not exist %s: %w", id, errdefs.ErrNotFound)
	}
	return p, nil
}

// ReserveProcess checks for the existence of an id and atomically
// reserves the process id if it does not already exist
//
// Returns true if the process id was successfully reserved and a
// cancel func to release the reservation
func (c *Container) ReserveProcess(id string) (bool, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.processes[id]; ok {
		return false, nil
	}
	if _, ok := c.reservedProcess[id]; ok {
		return false, nil
	}
	c.reservedProcess[id] = struct{}{}
	return true, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.reservedProcess, id)
	}
}

// ProcessAdd adds a new process to the container
func (c *Container) ProcessAdd(process process.Process) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.reservedProcess, process.ID())
	c.processes[process.ID()] = process
}

// ProcessRemove removes the process by id from the container
func (c *Container) ProcessRemove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.processes, id)
}

// Start a container process
func (c *Container) Start(ctx context.Context, r *task.StartRequest) (process.Process, error) {
	p, err := c.Process(r.ExecID)
	if err != nil {
		return nil, err
	}
	if err := p.Start(ctx); err != nil {
		return p, err
	}
	if c.Cgroup() == nil && p.Pid() > 0 {
		if cg, err := loadProcessCgroup(ctx, p.Pid()); err == nil {
			c.cgroup = cg
		}
	}
	return p, nil
}

// Delete the container or a process by id
func (c *Container) Delete(ctx context.Context, r *task.DeleteRequest) (process.Process, error) {
	p, err := c.Process(r.ExecID)
	if err != nil {
		return nil, err
	}
	if err := p.Delete(ctx); err != nil {
		return nil, err
	}
	if r.ExecID != "" {
		c.ProcessRemove(r.ExecID)
		return p, nil
	}
	c.mu.Lock()
	cleanup := c.mountCleanup
	c.mountCleanup = nil
	c.mu.Unlock()
	if cleanup != nil {
		if err := cleanup(ctx); err != nil {
			log.G(ctx).WithError(err).Warn("failed to cleanup mounts after delete")
		}
	}
	return p, nil
}

// Exec an additional process
func (c *Container) Exec(ctx context.Context, r *task.ExecProcessRequest) (process.Process, error) {
	initProc, ok := c.process.(*process.Init)
	if !ok {
		return nil, fmt.Errorf("expected init process, got %T", c.process)
	}
	proc, err := initProc.Exec(ctx, c.Bundle, &process.ExecConfig{
		ID:       r.ExecID,
		Terminal: r.Terminal,
		Stdin:    r.Stdin,
		Stdout:   r.Stdout,
		Stderr:   r.Stderr,
		Spec:     r.Spec,
	})
	if err != nil {
		return nil, err
	}
	c.ProcessAdd(proc)
	return proc, nil
}

// ResizePty of a process
func (c *Container) ResizePty(ctx context.Context, r *task.ResizePtyRequest) error {
	p, err := c.Process(r.ExecID)
	if err != nil {
		return err
	}
	ws := console.WinSize{
		Width:  uint16(r.Width),
		Height: uint16(r.Height),
	}
	return p.Resize(ws)
}

// Kill a process
func (c *Container) Kill(ctx context.Context, r *task.KillRequest) error {
	p, err := c.Process(r.ExecID)
	if err != nil {
		return err
	}
	return p.Kill(ctx, r.Signal, r.All)
}

// CloseIO of a process
func (c *Container) CloseIO(ctx context.Context, r *task.CloseIORequest) error {
	p, err := c.Process(r.ExecID)
	if err != nil {
		return err
	}
	if stdin := p.Stdin(); stdin != nil {
		if err := stdin.Close(); err != nil {
			return fmt.Errorf("close stdin: %w", err)
		}
	}
	return nil
}

// Update the resource information of a running container
func (c *Container) Update(ctx context.Context, r *task.UpdateTaskRequest) error {
	p, err := c.Process("")
	if err != nil {
		return err
	}
	initProc, ok := p.(*process.Init)
	if !ok {
		return fmt.Errorf("expected init process, got %T", p)
	}
	return initProc.Update(ctx, r.Resources)
}

// HasPid returns true if the container owns a specific pid
func (c *Container) HasPid(pid int) bool {
	if c.Pid() == pid {
		return true
	}
	for _, p := range c.All() {
		if p.Pid() == pid {
			return true
		}
	}
	return false
}

func loadProcessCgroup(ctx context.Context, pid int) (CgroupManager, error) {
	return LoadProcessCgroup(ctx, pid)
}
