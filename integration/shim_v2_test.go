//go:build linux && integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	eventsapi "github.com/containerd/containerd/api/events"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/runtime"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/typeurl/v2"
)

const (
	initPending = iota
	initCreated
	initStarted
	initExited
	initDeleted
)

const (
	execPending = iota
	execAdded
	execStarted
	execExited
)

type eventTracker struct {
	containerID string
	execID      string
	checkExec   bool

	expectedInitExit *uint32
	expectedExecExit *uint32

	mu        sync.Mutex
	initState int
	execState int
	err       error
	done      chan struct{}
	doneOnce  sync.Once
}

func newEventTracker(containerID, execID string, checkExec bool, expectedInitExit, expectedExecExit *uint32) *eventTracker {
	return &eventTracker{
		containerID:      containerID,
		execID:           execID,
		checkExec:        checkExec,
		expectedInitExit: expectedInitExit,
		expectedExecExit: expectedExecExit,
		initState:        initPending,
		execState:        execPending,
		done:             make(chan struct{}),
	}
}

func (t *eventTracker) closeDone() {
	t.doneOnce.Do(func() {
		close(t.done)
	})
}

func (t *eventTracker) setErr(err error) {
	if err == nil {
		return
	}
	t.mu.Lock()
	if t.err != nil {
		t.mu.Unlock()
		return
	}
	t.err = err
	t.mu.Unlock()
	t.closeDone()
}

func (t *eventTracker) maybeDoneLocked() {
	if t.initState != initDeleted {
		return
	}
	if t.checkExec && t.execState != execExited {
		return
	}
	t.closeDone()
}

func (t *eventTracker) handleEvent(e *eventsapi.Envelope) {
	if e == nil || e.Event == nil {
		return
	}
	decoded, err := typeurl.UnmarshalAny(e.Event)
	if err != nil {
		t.setErr(fmt.Errorf("unmarshal event: %w", err))
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	switch evt := decoded.(type) {
	case *eventsapi.TaskCreate:
		if evt.ContainerID != t.containerID {
			return
		}
		if t.initState != initPending {
			t.err = fmt.Errorf("TaskCreate out of order: state=%d", t.initState)
			t.closeDone()
			return
		}
		t.initState = initCreated
	case *eventsapi.TaskStart:
		if evt.ContainerID != t.containerID {
			return
		}
		if t.initState != initCreated {
			t.err = fmt.Errorf("TaskStart out of order: state=%d", t.initState)
			t.closeDone()
			return
		}
		t.initState = initStarted
	case *eventsapi.TaskExit:
		if evt.ContainerID != t.containerID {
			return
		}
		if t.checkExec && evt.ID == t.execID {
			if t.execState != execStarted {
				t.err = fmt.Errorf("TaskExit (exec) out of order: state=%d", t.execState)
				t.closeDone()
				return
			}
			if t.expectedExecExit != nil && evt.ExitStatus != *t.expectedExecExit {
				t.err = fmt.Errorf("TaskExit (exec) exit status mismatch: got=%d want=%d", evt.ExitStatus, *t.expectedExecExit)
				t.closeDone()
				return
			}
			if evt.ExitedAt == nil || evt.ExitedAt.AsTime().IsZero() {
				t.err = fmt.Errorf("TaskExit (exec) missing exit timestamp")
				t.closeDone()
				return
			}
			t.execState = execExited
			t.maybeDoneLocked()
			return
		}
		if t.initState != initStarted {
			t.err = fmt.Errorf("TaskExit out of order: state=%d", t.initState)
			t.closeDone()
			return
		}
		if t.expectedInitExit != nil && evt.ExitStatus != *t.expectedInitExit {
			t.err = fmt.Errorf("TaskExit exit status mismatch: got=%d want=%d", evt.ExitStatus, *t.expectedInitExit)
			t.closeDone()
			return
		}
		if evt.ExitedAt == nil || evt.ExitedAt.AsTime().IsZero() {
			t.err = fmt.Errorf("TaskExit missing exit timestamp")
			t.closeDone()
			return
		}
		t.initState = initExited
	case *eventsapi.TaskDelete:
		if evt.ContainerID != t.containerID {
			return
		}
		if t.initState != initExited {
			t.err = fmt.Errorf("TaskDelete out of order: state=%d", t.initState)
			t.closeDone()
			return
		}
		t.initState = initDeleted
		t.maybeDoneLocked()
	case *eventsapi.TaskExecAdded:
		if !t.checkExec || evt.ContainerID != t.containerID || evt.ExecID != t.execID {
			return
		}
		if t.execState != execPending {
			t.err = fmt.Errorf("TaskExecAdded out of order: state=%d", t.execState)
			t.closeDone()
			return
		}
		t.execState = execAdded
	case *eventsapi.TaskExecStarted:
		if !t.checkExec || evt.ContainerID != t.containerID || evt.ExecID != t.execID {
			return
		}
		if t.execState != execAdded {
			t.err = fmt.Errorf("TaskExecStarted out of order: state=%d", t.execState)
			t.closeDone()
			return
		}
		t.execState = execStarted
	}
}

func (t *eventTracker) wait(ctx context.Context) error {
	select {
	case <-t.done:
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestRuntimeV2ShimEventsAndExecOrdering(t *testing.T) {
	cfg := loadTestConfig()

	client := setupContainerdClient(t, cfg)
	defer client.Close()

	ensureImagePulled(t, client, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ctx = namespaces.WithNamespace(ctx, cfg.Namespace)

	containerID := fmt.Sprintf("shim-validate-%d", time.Now().UnixNano())
	execID := "exec1"
	expectedInitExit := uint32(0)
	expectedExecExit := uint32(0)
	tracker := newEventTracker(containerID, execID, true, &expectedInitExit, &expectedExecExit)

	eventsCtx, eventsCancel := context.WithCancel(ctx)
	defer eventsCancel()
	eventsCh, eventsErrCh := client.Subscribe(eventsCtx)
	go func() {
		for {
			select {
			case e := <-eventsCh:
				tracker.handleEvent(e)
			case err := <-eventsErrCh:
				if err != nil && !errors.Is(err, context.Canceled) {
					tracker.setErr(fmt.Errorf("event stream error: %w", err))
				}
				return
			case <-eventsCtx.Done():
				return
			}
		}
	}()

	image, err := client.GetImage(ctx, cfg.Image)
	if err != nil {
		t.Fatalf("get image %s: %v", cfg.Image, err)
	}

	container, err := client.NewContainer(
		ctx,
		containerID,
		containerd.WithImage(image),
		containerd.WithSnapshotter(cfg.Snapshotter),
		containerd.WithNewSnapshot(containerID, image),
		containerd.WithNewSpec(
			oci.WithImageConfig(image),
			oci.WithProcessArgs("/bin/sh", "-c", "sleep 5"),
		),
		containerd.WithRuntime(cfg.Runtime, nil),
	)
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	defer func() {
		_ = container.Delete(ctx, containerd.WithSnapshotCleanup)
	}()

	task, err := container.NewTask(ctx, cio.NullIO)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	defer func() {
		_, _ = task.Delete(ctx)
	}()

	exitCh, err := task.Wait(ctx)
	if err != nil {
		t.Fatalf("wait task: %v", err)
	}
	if err := task.Start(ctx); err != nil {
		t.Fatalf("start task: %v", err)
	}

	spec, err := container.Spec(ctx)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	execSpec := *spec.Process
	execSpec.Args = []string{"/bin/sh", "-c", "echo shim-validate"}

	proc, err := task.Exec(ctx, execID, &execSpec, cio.NullIO)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	execExitCh, err := proc.Wait(ctx)
	if err != nil {
		t.Fatalf("wait exec: %v", err)
	}
	if err := proc.Start(ctx); err != nil {
		t.Fatalf("start exec: %v", err)
	}
	<-execExitCh

	<-exitCh
	if _, err := task.Delete(ctx); err != nil {
		t.Fatalf("delete task: %v", err)
	}

	if err := tracker.wait(ctx); err != nil {
		t.Fatalf("event order validation failed: %v", err)
	}

	t.Logf("shim validation ok (runtime=%s, container=%s)", cfg.Runtime, containerID)
	t.Logf("validated topics: %s %s %s %s",
		runtime.TaskCreateEventTopic,
		runtime.TaskStartEventTopic,
		runtime.TaskExitEventTopic,
		runtime.TaskDeleteEventTopic,
	)
	t.Logf("validated exec topics: %s %s %s",
		runtime.TaskExecAddedEventTopic,
		runtime.TaskExecStartedEventTopic,
		runtime.TaskExitEventTopic,
	)
}
