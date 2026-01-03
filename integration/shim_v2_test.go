//go:build linux && integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	eventsapi "github.com/containerd/containerd/api/events"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/events"
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

func (t *eventTracker) setErrLocked(err error) {
	t.err = err
	t.closeDone()
}

func (t *eventTracker) handleExecExit(evt *eventsapi.TaskExit) {
	if t.execState != execStarted {
		t.setErrLocked(fmt.Errorf("TaskExit (exec) out of order: state=%d", t.execState))
		return
	}
	if t.expectedExecExit != nil && evt.ExitStatus != *t.expectedExecExit {
		t.setErrLocked(fmt.Errorf("TaskExit (exec) exit status mismatch: got=%d want=%d", evt.ExitStatus, *t.expectedExecExit))
		return
	}
	if evt.ExitedAt == nil || evt.ExitedAt.AsTime().IsZero() {
		t.setErrLocked(fmt.Errorf("TaskExit (exec) missing exit timestamp"))
		return
	}
	t.execState = execExited
	t.maybeDoneLocked()
}

func (t *eventTracker) handleInitExit(evt *eventsapi.TaskExit) {
	if t.initState != initStarted {
		t.setErrLocked(fmt.Errorf("TaskExit out of order: state=%d", t.initState))
		return
	}
	if t.expectedInitExit != nil && evt.ExitStatus != *t.expectedInitExit {
		t.setErrLocked(fmt.Errorf("TaskExit exit status mismatch: got=%d want=%d", evt.ExitStatus, *t.expectedInitExit))
		return
	}
	if evt.ExitedAt == nil || evt.ExitedAt.AsTime().IsZero() {
		t.setErrLocked(fmt.Errorf("TaskExit missing exit timestamp"))
		return
	}
	t.initState = initExited
}

func (t *eventTracker) handleEvent(e *events.Envelope) {
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
			t.setErrLocked(fmt.Errorf("TaskCreate out of order: state=%d", t.initState))
			return
		}
		t.initState = initCreated
	case *eventsapi.TaskStart:
		if evt.ContainerID != t.containerID {
			return
		}
		if t.initState != initCreated {
			t.setErrLocked(fmt.Errorf("TaskStart out of order: state=%d", t.initState))
			return
		}
		t.initState = initStarted
	case *eventsapi.TaskExit:
		if evt.ContainerID != t.containerID {
			return
		}
		if t.checkExec && evt.ID == t.execID {
			t.handleExecExit(evt)
			return
		}
		t.handleInitExit(evt)
	case *eventsapi.TaskDelete:
		if evt.ContainerID != t.containerID {
			return
		}
		if t.initState != initExited {
			t.setErrLocked(fmt.Errorf("TaskDelete out of order: state=%d", t.initState))
			return
		}
		t.initState = initDeleted
		t.maybeDoneLocked()
	case *eventsapi.TaskExecAdded:
		if !t.checkExec || evt.ContainerID != t.containerID || evt.ExecID != t.execID {
			return
		}
		if t.execState != execPending {
			t.setErrLocked(fmt.Errorf("TaskExecAdded out of order: state=%d", t.execState))
			return
		}
		t.execState = execAdded
	case *eventsapi.TaskExecStarted:
		if !t.checkExec || evt.ContainerID != t.containerID || evt.ExecID != t.execID {
			return
		}
		if t.execState != execAdded {
			t.setErrLocked(fmt.Errorf("TaskExecStarted out of order: state=%d", t.execState))
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

	// Use CI test ID if available, otherwise generate one
	containerID := os.Getenv("QEMUBOX_TEST_ID")
	if containerID == "" {
		containerID = fmt.Sprintf("shim-validate-%d", time.Now().UnixNano())
	} else {
		containerID = fmt.Sprintf("%s-shim-%d", containerID, time.Now().UnixNano()%10000)
	}

	// Set up log collector to capture logs specific to this container
	logCollector := newTestLogCollector(t, containerID)
	defer func() {
		if t.Failed() {
			logCollector.dumpLogs()
		}
	}()

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
			oci.WithProcessArgs("/bin/sh", "-c", "sleep 3"),
		),
		containerd.WithRuntime(cfg.Runtime, nil),
	)
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	defer func() {
		if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
			t.Logf("cleanup container: %v", err)
		}
	}()

	t.Log("creating task...")
	task, err := container.NewTask(ctx, cio.NullIO)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Logf("task created with pid %d", task.Pid())
	defer func() {
		// Use WithProcessKill to force cleanup even if task is in CREATED state
		// (e.g., when Start fails)
		if _, err := task.Delete(ctx, containerd.WithProcessKill); err != nil {
			t.Logf("cleanup task: %v", err)
		}
	}()

	exitCh, err := task.Wait(ctx)
	if err != nil {
		t.Fatalf("wait task: %v", err)
	}

	// Verify task is in created state before starting.
	// This confirms the shim is responding to RPC calls.
	status, err := task.Status(ctx)
	if err != nil {
		t.Fatalf("get task status: %v", err)
	}
	t.Logf("task status before start: %s", status.Status)

	t.Log("starting task...")
	if err := task.Start(ctx); err != nil {
		t.Fatalf("start task: %v", err)
	}
	t.Log("task started successfully")

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
