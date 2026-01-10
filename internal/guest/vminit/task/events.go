//go:build linux

package task

import (
	"context"

	eventstypes "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/containerd/v2/core/runtime"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/protobuf"
	runcC "github.com/containerd/go-runc"
	"github.com/containerd/log"

	"github.com/spin-stack/spinbox/internal/guest/vminit/process"
	"github.com/spin-stack/spinbox/internal/guest/vminit/runc"
)

// preStart prepares for starting a container process and handling its exit.
// The container being started should be passed in as c when starting the container
// init process for an already-created container. c should be nil when creating a
// container or when starting an exec.
//
// The returned handleStarted closure records that the process has started so
// that its exit can be handled efficiently. If the process has already exited,
// it handles the exit immediately.
// handleStarted should be called after the event announcing the start of the
// process has been published. Note that s.lifecycleMu must not be held when
// calling handleStarted.
//
// The returned cleanup closure releases resources used to handle early exits.
// It must be called before the caller of preStart returns, otherwise severe
// memory leaks will occur.
func (s *service) preStart(c *runc.Container) (func(*runc.Container, process.Process), func()) {
	sub := s.exitTracker.Subscribe(c)

	handleStarted := func(c *runc.Container, p process.Process) {
		var pid int
		if p != nil {
			pid = p.Pid()
		}

		// Check if process exited before we could register it
		earlyExits := sub.HandleStart(c, p, pid)

		// Handle any early exits
		for _, ee := range earlyExits {
			s.handleProcessExit(ee, c, p)
		}
	}

	cleanup := func() {
		sub.Cancel()
	}

	return handleStarted, cleanup
}

func (s *service) processExits() {
	for e := range s.ec {
		// While unlikely, it is not impossible for a container process to exit
		// and have its PID be recycled for a new container process before we
		// have a chance to process the first exit. As we have no way to tell
		// for sure which of the processes the exit event corresponds to (until
		// pidfd support is implemented) there is no way for us to handle the
		// exit correctly in that case.

		// Notify exit tracker and get container processes that exited
		cps := s.exitTracker.NotifyExit(e)

		for _, cp := range cps {
			if cp.Process.IsInit() {
				s.handleInitExit(e, cp.Container, cp.Process.(*process.Init))
			} else {
				s.handleProcessExit(e, cp.Container, cp.Process)
			}
		}
	}
}

func (s *service) send(evt interface{}) {
	s.events <- evt
}

// handleInitExit processes container init process exits.
// This is handled separately from non-init exits, because there
// are some extra invariants we want to ensure in this case, namely:
// - for a given container, the init process exit MUST be the last exit published
// This is achieved by:
// - killing all running container processes (if the container has a shared pid
// namespace, otherwise all other processes have been reaped already).
// - waiting for the container's running exec counter to reach 0.
// - finally, publishing the init exit.
func (s *service) handleInitExit(e runcC.Exit, c *runc.Container, p *process.Init) {
	// kill all running container processes
	if runc.ShouldKillAllOnExit(s.context, c.Bundle) {
		if err := p.KillAll(s.context); err != nil {
			log.G(s.context).WithError(err).WithField("id", p.ID()).
				Error("failed to kill init's children")
		}
	}

	// Check if we need to delay init exit until all execs complete
	shouldDelay, waitChan := s.exitTracker.ShouldDelayInitExit(c)
	if !shouldDelay {
		// No execs running, publish immediately
		s.handleProcessExit(e, c, p)
		return
	}

	// Execs still running - wait for them to complete
	go func() {
		<-waitChan
		// All running execs have exited now, publish the init exit
		s.handleProcessExit(e, c, p)
	}()
}

func (s *service) handleProcessExit(e runcC.Exit, c *runc.Container, p process.Process) {
	p.SetExited(e.Status)

	// With direct stream I/O, synchronization happens at the host side.
	// The host waits for stream EOF before forwarding TaskExit to containerd.
	// The guest just sends the exit event - the stream close (from process exit)
	// naturally signals completion to the host.

	s.send(&eventstypes.TaskExit{
		ContainerID: c.ID,
		ID:          p.ID(),
		Pid:         uint32(e.Pid),
		ExitStatus:  uint32(e.Status),
		ExitedAt:    protobuf.ToTimestamp(p.ExitedAt()),
	})

	// Decrement exec counter for non-init processes
	if !p.IsInit() {
		s.exitTracker.NotifyExecExit(c)
	}
}

func (s *service) forward(ctx context.Context, publisher events.Publisher) {
	ns, ok := namespaces.Namespace(ctx)
	if !ok || ns == "" {
		ns = "default"
	}
	ctx = namespaces.WithNamespace(context.WithoutCancel(ctx), ns)
	for e := range s.events {
		err := publisher.Publish(ctx, runtime.GetTopic(e), e)
		if err != nil {
			log.G(ctx).WithError(err).Error("post event")
		}
	}
}
