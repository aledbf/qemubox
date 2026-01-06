//go:build linux

package task

import (
	"context"
	"path/filepath"

	eventstypes "github.com/containerd/containerd/api/events"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/pkg/protobuf"
	ptypes "github.com/containerd/containerd/v2/pkg/protobuf/types"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/typeurl/v2"

	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/runc"
	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/systools"
)

// Create a new initial process and container with the underlying OCI runtime
func (s *service) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("id", r.ID))

	log.G(ctx).WithField("bundle", r.Bundle).Info("create task request")

	handleStarted, cleanup := s.preStart(nil)
	defer cleanup()

	systools.DumpFile(ctx, filepath.Join(r.Bundle, "config.json"))

	container, err := runc.NewContainer(ctx, s.platform, r, s.streams)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	log.G(ctx).WithField("container_id", container.ID).Info("created container")

	s.containers[r.ID] = container

	s.send(&eventstypes.TaskCreate{
		ContainerID: r.ID,
		Bundle:      r.Bundle,
		Rootfs:      r.Rootfs,
		IO: &eventstypes.TaskIO{
			Stdin:    r.Stdin,
			Stdout:   r.Stdout,
			Stderr:   r.Stderr,
			Terminal: r.Terminal,
		},
		Checkpoint: r.Checkpoint,
		Pid:        uint32(container.Pid()),
	})

	// Get the init process. Should always succeed if Pid() returned non-zero.
	proc, err := container.Process("")
	if err != nil {
		log.G(ctx).WithError(err).Error("BUG: container has PID but no init process")
		return nil, errgrpc.ToGRPCf(errdefs.ErrInternal, "container in inconsistent state")
	}
	handleStarted(container, proc)

	return &taskAPI.CreateTaskResponse{
		Pid: uint32(container.Pid()),
	}, nil
}

// Start a process
func (s *service) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	log.G(ctx).WithFields(log.Fields{
		"id":   r.ID,
		"exec": r.ExecID,
	}).Info("Start: request received")

	container, err := s.getContainer(r.ID)
	if err != nil {
		log.G(ctx).WithError(err).WithField("id", r.ID).Error("Start: container not found")
		return nil, err
	}

	var cinit *runc.Container
	if r.ExecID == "" {
		cinit = container
	} else if s.exitTracker.InitHasExited(container) {
		return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "container %s init process is not running", container.ID)
	}
	handleStarted, cleanup := s.preStart(cinit)
	defer cleanup()

	log.G(ctx).WithFields(log.Fields{
		"container_id": r.ID,
		"exec_id":      r.ExecID,
	}).Info("starting container process")

	p, err := container.Start(ctx, r)
	if err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{
			"container_id": r.ID,
			"exec_id":      r.ExecID,
		}).Error("failed to start container process")
		// If we failed to even start the process, the exec counter
		// won't get decremented in handleProcessExit. Decrement it manually.
		if r.ExecID != "" {
			s.exitTracker.DecrementExecCount(container)
		}
		handleStarted(container, p)
		return nil, errgrpc.ToGRPC(err)
	}

	switch r.ExecID {
	case "":
		cg := container.Cgroup()
		if cg != nil {
			// Enable all available cgroup v2 controllers
			_ = cg.EnableControllers(ctx)
		}

		s.send(&eventstypes.TaskStart{
			ContainerID: container.ID,
			Pid:         uint32(p.Pid()),
		})
	default:
		s.send(&eventstypes.TaskExecStarted{
			ContainerID: container.ID,
			ExecID:      r.ExecID,
			Pid:         uint32(p.Pid()),
		})
	}
	log.G(ctx).WithFields(log.Fields{
		"container_id": container.ID,
		"exec_id":      r.ExecID,
		"pid":          p.Pid(),
	}).Info("started container process")
	handleStarted(container, p)
	return &taskAPI.StartResponse{
		Pid: uint32(p.Pid()),
	}, nil
}

// Delete the initial process and container
func (s *service) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	p, err := container.Delete(ctx, r)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	// Clean up container state for init task deletion.
	// Note: TaskDelete event is published by the shim directly to ensure it reaches
	// containerd before the shim shuts down (the event stream closes during shutdown).
	if r.ExecID == "" {
		s.mu.Lock()
		delete(s.containers, r.ID)
		s.mu.Unlock()
		s.exitTracker.Cleanup(container)
	}
	return &taskAPI.DeleteResponse{
		ExitStatus: uint32(p.ExitStatus()),
		ExitedAt:   protobuf.ToTimestamp(p.ExitedAt()),
		Pid:        uint32(p.Pid()),
	}, nil
}

// State returns runtime state information for a process
func (s *service) State(ctx context.Context, r *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	container, err := s.getContainer(r.ID)
	if err != nil {
		log.G(ctx).WithError(err).WithField("id", r.ID).Error("State: container not found")
		return nil, err
	}
	p, err := container.Process(r.ExecID)
	if err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{
			"id":   r.ID,
			"exec": r.ExecID,
		}).Error("State: process not found")
		return nil, errgrpc.ToGRPC(err)
	}
	st, err := p.Status(ctx)
	if err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{
			"id":   r.ID,
			"exec": r.ExecID,
		}).Error("State: failed to get process status")
		return nil, err
	}
	status := task.Status_UNKNOWN
	switch st {
	case "created":
		status = task.Status_CREATED
	case "running":
		status = task.Status_RUNNING
	case "stopped":
		status = task.Status_STOPPED
	case "paused":
		status = task.Status_PAUSED
	case "pausing":
		status = task.Status_PAUSING
	default:
		log.G(ctx).WithFields(log.Fields{
			"id":     r.ID,
			"exec":   r.ExecID,
			"status": st,
		}).Warn("State: unknown status string from process")
	}
	sio := p.Stdio()
	return &taskAPI.StateResponse{
		ID:         p.ID(),
		Bundle:     container.Bundle,
		Pid:        uint32(p.Pid()),
		Status:     status,
		Stdin:      sio.Stdin,
		Stdout:     sio.Stdout,
		Stderr:     sio.Stderr,
		Terminal:   sio.Terminal,
		ExitStatus: uint32(p.ExitStatus()),
		ExitedAt:   protobuf.ToTimestamp(p.ExitedAt()),
	}, nil
}

// Pause the container - not supported in VM-based runtime
func (s *service) Pause(ctx context.Context, r *taskAPI.PauseRequest) (*ptypes.Empty, error) {
	return nil, errgrpc.ToGRPCf(errdefs.ErrNotImplemented, "pause is not supported")
}

// Resume the container - not supported in VM-based runtime
func (s *service) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (*ptypes.Empty, error) {
	return nil, errgrpc.ToGRPCf(errdefs.ErrNotImplemented, "resume is not supported")
}

// Kill a process with the provided signal
func (s *service) Kill(ctx context.Context, r *taskAPI.KillRequest) (*ptypes.Empty, error) {
	container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	if err := container.Kill(ctx, r); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return empty, nil
}

// Checkpoint the container - not supported in VM-based runtime
func (s *service) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (*ptypes.Empty, error) {
	return nil, errgrpc.ToGRPCf(errdefs.ErrNotImplemented, "checkpoint is not supported")
}

// Update a running container
func (s *service) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*ptypes.Empty, error) {
	container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	if err := container.Update(ctx, r); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return empty, nil
}

func (s *service) Stats(ctx context.Context, r *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	cg := container.Cgroup()
	if cg == nil {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "cgroup does not exist")
	}

	stats, err := cg.Stats(ctx)
	if err != nil {
		return nil, err
	}

	data, err := typeurl.MarshalAny(stats)
	if err != nil {
		return nil, err
	}
	return &taskAPI.StatsResponse{
		Stats: typeurl.MarshalProto(data),
	}, nil
}
