//go:build linux

// Package task implements the vminit task service.
package task

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/containerd/cgroups/v3"
	"github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/containerd/v2/pkg/oom"
	oomv2 "github.com/containerd/containerd/v2/pkg/oom/v2"
	ptypes "github.com/containerd/containerd/v2/pkg/protobuf/types"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/containerd/v2/pkg/sys/reaper"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	runcC "github.com/containerd/go-runc"
	"github.com/containerd/ttrpc"

	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/process"
	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/runc"
	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/stream"
)

var (
	_     = shim.TTRPCService(&service{})
	empty = &ptypes.Empty{}
)

// NewTaskService creates a new instance of a task service
func NewTaskService(ctx context.Context, bundle string, publisher events.Publisher, sd shutdown.Service, sm stream.Manager) (task.TTRPCTaskService, error) {
	if cgroups.Mode() != cgroups.Unified {
		return nil, fmt.Errorf("only unified cgroups mode is supported: %w", errdefs.ErrNotImplemented)
	}
	ep, err := oomv2.New(publisher)
	if err != nil {
		return nil, err
	}
	go ep.Run(ctx)
	s := &service{
		context:     ctx,
		events:      make(chan interface{}, 128),
		ec:          reaper.Default.Subscribe(),
		ep:          ep,
		streams:     sm,
		shutdown:    sd,
		containers:  make(map[string]*runc.Container),
		exitTracker: newExitTracker(),
	}
	go s.processExits()
	runcC.Monitor = reaper.Default
	if err := s.initPlatform(); err != nil {
		return nil, fmt.Errorf("failed to initialized platform behavior: %w", err)
	}
	go s.forward(ctx, publisher)
	sd.RegisterCallback(func(context.Context) error {
		close(s.events)
		return nil
	})

	if address, err := shim.ReadAddress("address"); err == nil {
		sd.RegisterCallback(func(context.Context) error {
			return shim.RemoveSocket(address)
		})
	}
	return s, nil
}

// service is the shim implementation of a remote shim over GRPC
type service struct {
	mu sync.Mutex

	context  context.Context
	events   chan interface{}
	platform stdio.Platform
	ec       chan runcC.Exit
	ep       oom.Watcher

	streams stream.Manager

	containers map[string]*runc.Container

	// Exit tracking - manages the complex coordination between process starts and exits
	exitTracker *exitTracker

	shutdown shutdown.Service
}

type containerProcess struct {
	Container *runc.Container
	Process   process.Process
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	task.RegisterTTRPCTaskService(server, s)
	return nil
}

// Connect returns shim information such as the shim's pid
func (s *service) Connect(ctx context.Context, r *task.ConnectRequest) (*task.ConnectResponse, error) {
	var pid int
	if container, err := s.getContainer(r.ID); err == nil {
		pid = container.Pid()
	}
	return &task.ConnectResponse{
		ShimPid: uint32(os.Getpid()),
		TaskPid: uint32(pid),
	}, nil
}

func (s *service) Shutdown(ctx context.Context, r *task.ShutdownRequest) (*ptypes.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// return out if the shim is still servicing containers
	if len(s.containers) > 0 {
		return empty, nil
	}

	// please make sure that temporary resource has been cleanup or registered
	// for cleanup before calling shutdown
	s.shutdown.Shutdown()

	return empty, nil
}

func (s *service) getContainer(id string) (*runc.Container, error) {
	s.mu.Lock()
	container := s.containers[id]
	s.mu.Unlock()
	if container == nil {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "container not created")
	}
	return container, nil
}

// initialize a single epoll fd to manage our consoles. `initPlatform` should
// only be called once.
func (s *service) initPlatform() error {
	if s.platform != nil {
		return nil
	}
	p, err := runc.NewPlatform(s.streams)
	if err != nil {
		return err
	}
	s.platform = p
	s.shutdown.RegisterCallback(func(context.Context) error { return s.platform.Close() })
	return nil
}
