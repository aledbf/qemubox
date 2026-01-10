//go:build linux && integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Task state constants for event tracking.
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

// ctrEvent represents a containerd event from `ctr events`.
type ctrEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Namespace string    `json:"namespace"`
	Topic     string    `json:"topic"`
	Event     struct {
		ContainerID string `json:"container_id"`
		ID          string `json:"id"`
		ExitStatus  uint32 `json:"exit_status"`
		ExecID      string `json:"exec_id"`
	} `json:"event"`
}

// ctrEventTracker tracks containerd events from ctr events command.
type ctrEventTracker struct {
	containerID string
	execID      string
	checkExec   bool

	expectedInitExit *uint32
	expectedExecExit *uint32

	mu        sync.Mutex
	initState int
	execState int
	events    []ctrEvent
	err       error
	done      chan struct{}
	doneOnce  sync.Once
}

func newCtrEventTracker(containerID, execID string, checkExec bool, expectedInitExit, expectedExecExit *uint32) *ctrEventTracker {
	return &ctrEventTracker{
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

func (t *ctrEventTracker) closeDone() {
	t.doneOnce.Do(func() {
		close(t.done)
	})
}

func (t *ctrEventTracker) maybeDoneLocked() {
	if t.initState != initDeleted {
		return
	}
	if t.checkExec && t.execState != execExited {
		return
	}
	t.closeDone()
}

func (t *ctrEventTracker) setErrLocked(err error) {
	t.err = err
	t.closeDone()
}

func (t *ctrEventTracker) handleEvent(evt ctrEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Filter by container ID
	if evt.Event.ContainerID != t.containerID {
		return
	}

	t.events = append(t.events, evt)

	// Match topics by their suffix (containerd topics are like "/tasks/create")
	switch {
	case strings.HasSuffix(evt.Topic, "/create"):
		if t.initState != initPending {
			t.setErrLocked(fmt.Errorf("TaskCreate out of order: state=%d", t.initState))
			return
		}
		t.initState = initCreated

	case strings.HasSuffix(evt.Topic, "/start"):
		if t.initState != initCreated {
			t.setErrLocked(fmt.Errorf("TaskStart out of order: state=%d", t.initState))
			return
		}
		t.initState = initStarted

	case strings.HasSuffix(evt.Topic, "/exit"):
		// Check if this is an exec exit
		if t.checkExec && evt.Event.ID == t.execID {
			if t.execState != execStarted {
				t.setErrLocked(fmt.Errorf("TaskExit (exec) out of order: state=%d", t.execState))
				return
			}
			if t.expectedExecExit != nil && evt.Event.ExitStatus != *t.expectedExecExit {
				t.setErrLocked(fmt.Errorf("TaskExit (exec) exit status mismatch: got=%d want=%d", evt.Event.ExitStatus, *t.expectedExecExit))
				return
			}
			t.execState = execExited
			t.maybeDoneLocked()
			return
		}
		// Init exit
		if t.initState != initStarted {
			t.setErrLocked(fmt.Errorf("TaskExit out of order: state=%d", t.initState))
			return
		}
		if t.expectedInitExit != nil && evt.Event.ExitStatus != *t.expectedInitExit {
			t.setErrLocked(fmt.Errorf("TaskExit exit status mismatch: got=%d want=%d", evt.Event.ExitStatus, *t.expectedInitExit))
			return
		}
		t.initState = initExited

	case strings.HasSuffix(evt.Topic, "/delete"):
		if t.initState != initExited {
			t.setErrLocked(fmt.Errorf("TaskDelete out of order: state=%d", t.initState))
			return
		}
		t.initState = initDeleted
		t.maybeDoneLocked()

	case strings.HasSuffix(evt.Topic, "/exec-added"):
		if !t.checkExec || evt.Event.ExecID != t.execID {
			return
		}
		if t.execState != execPending {
			t.setErrLocked(fmt.Errorf("TaskExecAdded out of order: state=%d", t.execState))
			return
		}
		t.execState = execAdded

	case strings.HasSuffix(evt.Topic, "/exec-started"):
		if !t.checkExec || evt.Event.ExecID != t.execID {
			return
		}
		if t.execState != execAdded {
			t.setErrLocked(fmt.Errorf("TaskExecStarted out of order: state=%d", t.execState))
			return
		}
		t.execState = execStarted
	}
}

func (t *ctrEventTracker) wait(ctx context.Context) error {
	select {
	case <-t.done:
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.err
	case <-ctx.Done():
		t.mu.Lock()
		defer t.mu.Unlock()
		return fmt.Errorf("%w (initState=%d, execState=%d, events received=%d)",
			ctx.Err(), t.initState, t.execState, len(t.events))
	}
}

// ctrEventsRunner runs `ctr events` command for event tracking.
// We keep using ctr events because the Go client event subscription
// would require additional setup and this works reliably.
type ctrEventsRunner struct {
	t         *testing.T
	socket    string
	namespace string
}

func newCtrEventsRunner(t *testing.T, cfg testConfig) *ctrEventsRunner {
	return &ctrEventsRunner{
		t:         t,
		socket:    cfg.Socket,
		namespace: cfg.Namespace,
	}
}

// events starts `ctr events` and returns a channel of events.
func (c *ctrEventsRunner) events(ctx context.Context) (<-chan ctrEvent, func()) {
	c.t.Helper()

	eventsCh := make(chan ctrEvent, 100)
	cmd := exec.CommandContext(ctx, "ctr", "--address", c.socket, "--namespace", c.namespace, "events")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		c.t.Fatalf("create stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		c.t.Fatalf("start ctr events: %v", err)
	}

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			// ctr events output format:
			// 2026-01-03 19:15:51.035266231 +0000 UTC default /tasks/create {"container_id":"test"}
			// Fields: date time tz_offset tz_name namespace topic json_event

			var evt ctrEvent

			// Find the JSON object (starts with '{')
			jsonStart := strings.Index(line, "{")
			if jsonStart == -1 {
				continue
			}

			// Parse the prefix to extract topic
			prefix := strings.TrimSpace(line[:jsonStart])
			parts := strings.Fields(prefix)
			if len(parts) < 6 {
				continue
			}

			// Topic is the 6th field (0-indexed: 5)
			evt.Topic = parts[5]

			// Parse the JSON event
			jsonStr := line[jsonStart:]
			if err := json.Unmarshal([]byte(jsonStr), &evt.Event); err != nil {
				continue
			}

			select {
			case eventsCh <- evt:
			case <-ctx.Done():
				return
			}
		}
	}()

	cleanup := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		close(eventsCh)
	}

	return eventsCh, cleanup
}

func TestRuntimeV2ShimEventsAndExecOrdering(t *testing.T) {
	cfg := loadTestConfig()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Use CI test ID if available, otherwise generate one
	containerID := os.Getenv("SPINBOX_TEST_ID")
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
	tracker := newCtrEventTracker(containerID, execID, true, &expectedInitExit, &expectedExecExit)

	client := setupContainerdClient(t, cfg)
	defer client.Close()

	// Start event listener using ctr events (reliable event streaming)
	eventsRunner := newCtrEventsRunner(t, cfg)
	eventsCtx, eventsCancel := context.WithCancel(ctx)
	defer eventsCancel()
	eventsCh, cleanupEvents := eventsRunner.events(eventsCtx)
	defer cleanupEvents()

	go func() {
		for evt := range eventsCh {
			tracker.handleEvent(evt)
		}
	}()

	// Give ctr events a moment to connect and start receiving
	time.Sleep(100 * time.Millisecond)

	// Ensure image is pulled and unpacked for the configured snapshotter.
	t.Log("ensuring image is unpacked...")
	ctxNS := namespaces.WithNamespace(ctx, cfg.Namespace)
	img, err := client.GetImage(ctxNS, cfg.Image)
	if err != nil {
		img, err = client.Pull(ctxNS, cfg.Image, containerd.WithPullSnapshotter(cfg.Snapshotter))
		if err != nil {
			t.Fatalf("pull image: %v", err)
		}
	}
	if err := img.Unpack(ctxNS, cfg.Snapshotter); err != nil && !errdefs.IsAlreadyExists(err) {
		t.Fatalf("unpack image: %v", err)
	}

	// Create container using Go client API.
	// This is the key difference from ctr CLI: NewContainer only creates metadata,
	// it does NOT mount the snapshot on the host. The shim handles mounts internally.
	t.Log("creating container...")
	container, err := client.NewContainer(ctxNS, containerID,
		containerd.WithSnapshotter(cfg.Snapshotter),
		containerd.WithImage(img),
		containerd.WithNewSnapshot(containerID+"-snapshot", img),
		containerd.WithRuntime(cfg.Runtime, nil),
		containerd.WithNewSpec(
			oci.WithImageConfig(img),
			oci.WithProcessArgs("/bin/sh", "-c", "sleep 5"),
		),
	)
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	defer func() {
		if err := container.Delete(ctxNS, containerd.WithSnapshotCleanup); err != nil {
			t.Logf("cleanup container %s: %v", containerID, err)
		}
	}()

	// Create task - this starts the shim which handles mounts internally
	t.Log("creating task...")
	task, err := container.NewTask(ctxNS, cio.NullIO)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	defer func() {
		if _, err := task.Delete(ctxNS, containerd.WithProcessKill); err != nil {
			// Ignore "ttrpc: closed" errors - expected when task completes
			if !strings.Contains(err.Error(), "ttrpc: closed") &&
				!strings.Contains(err.Error(), "not found") {
				t.Logf("cleanup task: %v", err)
			}
		}
	}()

	// Set up wait channel before starting
	exitCh, err := task.Wait(ctxNS)
	if err != nil {
		t.Fatalf("wait for task: %v", err)
	}

	// Start the task
	if err := task.Start(ctxNS); err != nil {
		t.Fatalf("start task: %v", err)
	}
	t.Log("task started successfully")

	// Small delay to let task reach running state
	time.Sleep(500 * time.Millisecond)

	// Check task status
	t.Log("checking task status...")
	status, err := task.Status(ctxNS)
	if err != nil {
		t.Fatalf("get task status: %v", err)
	}
	t.Logf("task status: %s", status.Status)

	// Run exec using Go client API
	t.Log("running exec...")
	// Use /bin/echo (external binary) via shell - builtin echo fails with NullIO
	execProcess, err := task.Exec(ctxNS, execID, &specs.Process{
		Args: []string{"/bin/sh", "-c", "/bin/echo shim-validate"},
		Cwd:  "/",
	}, cio.NullIO)
	if err != nil {
		t.Fatalf("create exec: %v", err)
	}

	execExitCh, err := execProcess.Wait(ctxNS)
	if err != nil {
		t.Fatalf("wait for exec: %v", err)
	}

	if err := execProcess.Start(ctxNS); err != nil {
		t.Fatalf("start exec: %v", err)
	}

	// Wait for exec to complete
	execStatus := <-execExitCh
	execExitCode, _, err := execStatus.Result()
	if err != nil {
		t.Fatalf("get exec result: %v", err)
	}
	t.Logf("exec completed with exit code %d", execExitCode)

	// Clean up exec process
	if _, err := execProcess.Delete(ctxNS); err != nil {
		t.Logf("delete exec process: %v", err)
	}

	// Wait for task to exit (sleep 5 should complete)
	t.Log("waiting for task to exit...")
	waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
	defer waitCancel()

	select {
	case taskStatus := <-exitCh:
		exitCode, _, err := taskStatus.Result()
		if err != nil {
			t.Fatalf("get task result: %v", err)
		}
		t.Logf("task exited with code %d", exitCode)
	case <-waitCtx.Done():
		t.Fatalf("timeout waiting for task to exit")
	}

	// Delete task
	t.Log("deleting task...")
	if _, err := task.Delete(ctxNS); err != nil {
		// Ignore expected errors
		if !strings.Contains(err.Error(), "ttrpc: closed") &&
			!strings.Contains(err.Error(), "not found") {
			t.Logf("task delete: %v", err)
		}
	}
	t.Log("task deleted")

	// Wait for event tracker to complete
	eventWaitCtx, eventWaitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer eventWaitCancel()

	if err := tracker.wait(eventWaitCtx); err != nil {
		t.Fatalf("event order validation failed: %v", err)
	}

	t.Logf("shim validation ok (runtime=%s, container=%s)", cfg.Runtime, containerID)
	t.Logf("validated event topics: /tasks/create, /tasks/start, /tasks/exit, /tasks/delete")
	t.Logf("validated exec topics: /tasks/exec-added, /tasks/exec-started, /tasks/exit")
}
