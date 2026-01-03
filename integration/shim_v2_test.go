//go:build linux && integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
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

	switch {
	case strings.Contains(evt.Topic, "task-create"):
		if t.initState != initPending {
			t.setErrLocked(fmt.Errorf("TaskCreate out of order: state=%d", t.initState))
			return
		}
		t.initState = initCreated

	case strings.Contains(evt.Topic, "task-start"):
		if t.initState != initCreated {
			t.setErrLocked(fmt.Errorf("TaskStart out of order: state=%d", t.initState))
			return
		}
		t.initState = initStarted

	case strings.Contains(evt.Topic, "task-exit"):
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

	case strings.Contains(evt.Topic, "task-delete"):
		if t.initState != initExited {
			t.setErrLocked(fmt.Errorf("TaskDelete out of order: state=%d", t.initState))
			return
		}
		t.initState = initDeleted
		t.maybeDoneLocked()

	case strings.Contains(evt.Topic, "exec-added"):
		if !t.checkExec || evt.Event.ExecID != t.execID {
			return
		}
		if t.execState != execPending {
			t.setErrLocked(fmt.Errorf("TaskExecAdded out of order: state=%d", t.execState))
			return
		}
		t.execState = execAdded

	case strings.Contains(evt.Topic, "exec-started"):
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
		return ctx.Err()
	}
}

// ctrCmd runs a ctr command and returns its output.
func ctrCmd(t *testing.T, socket, namespace string, args ...string) (string, error) {
	t.Helper()
	fullArgs := append([]string{"--address", socket, "-n", namespace}, args...)
	cmd := exec.Command("ctr", fullArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String(), fmt.Errorf("ctr %s: %v\nstderr: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), nil
}

// ctrCmdContext runs a ctr command with context and returns its output.
func ctrCmdContext(ctx context.Context, t *testing.T, socket, namespace string, args ...string) (string, error) {
	t.Helper()
	fullArgs := append([]string{"--address", socket, "-n", namespace}, args...)
	cmd := exec.CommandContext(ctx, "ctr", fullArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String(), fmt.Errorf("ctr %s: %v\nstderr: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), nil
}

// startCtrEvents starts `ctr events` and returns a channel of events.
func startCtrEvents(ctx context.Context, t *testing.T, socket, namespace string) (<-chan ctrEvent, func()) {
	t.Helper()

	eventsCh := make(chan ctrEvent, 100)
	cmd := exec.CommandContext(ctx, "ctr", "--address", socket, "-n", namespace, "events")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start ctr events: %v", err)
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
				t.Logf("failed to parse event prefix: %s", line)
				continue
			}

			// Topic is the 6th field (0-indexed: 5)
			evt.Topic = parts[5]

			// Parse the JSON event
			jsonStr := line[jsonStart:]
			if err := json.Unmarshal([]byte(jsonStr), &evt.Event); err != nil {
				t.Logf("failed to parse event JSON: %v (json: %s)", err, jsonStr)
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
		cmd.Process.Kill()
		cmd.Wait()
		close(eventsCh)
	}

	return eventsCh, cleanup
}

func TestRuntimeV2ShimEventsAndExecOrdering(t *testing.T) {
	cfg := loadTestConfig()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

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
	tracker := newCtrEventTracker(containerID, execID, true, &expectedInitExit, &expectedExecExit)

	// Start event listener
	eventsCtx, eventsCancel := context.WithCancel(ctx)
	defer eventsCancel()
	eventsCh, cleanupEvents := startCtrEvents(eventsCtx, t, cfg.Socket, cfg.Namespace)
	defer cleanupEvents()

	go func() {
		for evt := range eventsCh {
			tracker.handleEvent(evt)
		}
	}()

	// Ensure image is pulled and unpacked for the configured snapshotter.
	t.Log("pulling image...")
	if _, err := ctrCmd(t, cfg.Socket, cfg.Namespace, "image", "pull",
		"--snapshotter", cfg.Snapshotter,
		cfg.Image,
	); err != nil {
		t.Fatalf("pull image: %v", err)
	}

	// Cleanup function for container
	cleanup := func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()

		// Kill and delete task (ignore errors)
		ctrCmdContext(cleanupCtx, t, cfg.Socket, cfg.Namespace, "task", "kill", "-s", "SIGKILL", containerID)
		time.Sleep(500 * time.Millisecond)
		ctrCmdContext(cleanupCtx, t, cfg.Socket, cfg.Namespace, "task", "delete", "-f", containerID)

		// Delete container (with snapshot cleanup)
		ctrCmdContext(cleanupCtx, t, cfg.Socket, cfg.Namespace, "container", "delete", containerID)
	}
	defer cleanup()

	// Create container - containerd will automatically create a snapshot from the image
	t.Log("creating container...")
	if _, err := ctrCmd(t, cfg.Socket, cfg.Namespace, "container", "create",
		"--snapshotter", cfg.Snapshotter,
		"--runtime", cfg.Runtime,
		cfg.Image, containerID,
		"/bin/sh", "-c", "sleep 5",
	); err != nil {
		t.Fatalf("create container: %v", err)
	}

	// Create and start task
	t.Log("creating task...")
	if _, err := ctrCmd(t, cfg.Socket, cfg.Namespace, "task", "start", "--null-io", "-d", containerID); err != nil {
		t.Fatalf("start task: %v", err)
	}
	t.Log("task started successfully")

	// Small delay to let task reach running state
	time.Sleep(500 * time.Millisecond)

	// Check task status
	t.Log("checking task status...")
	status, err := ctrCmd(t, cfg.Socket, cfg.Namespace, "task", "ls")
	if err != nil {
		t.Fatalf("task ls: %v", err)
	}
	if !strings.Contains(status, containerID) {
		t.Fatalf("task not found in task list: %s", status)
	}
	t.Logf("task status:\n%s", status)

	// Run exec
	t.Log("running exec...")
	if _, err := ctrCmd(t, cfg.Socket, cfg.Namespace, "task", "exec",
		"--exec-id", execID,
		containerID,
		"/bin/sh", "-c", "echo shim-validate",
	); err != nil {
		t.Fatalf("exec: %v", err)
	}
	t.Log("exec completed")

	// Wait for task to exit (sleep 5 should complete)
	t.Log("waiting for task to exit...")
	waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
	defer waitCancel()

	for {
		select {
		case <-waitCtx.Done():
			t.Fatalf("timeout waiting for task to exit")
		default:
		}

		status, err := ctrCmd(t, cfg.Socket, cfg.Namespace, "task", "ls")
		if err != nil {
			// Task may have been deleted
			break
		}
		if !strings.Contains(status, containerID) || strings.Contains(status, "STOPPED") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Log("task exited")

	// Delete task
	t.Log("deleting task...")
	if _, err := ctrCmd(t, cfg.Socket, cfg.Namespace, "task", "delete", containerID); err != nil {
		// Ignore errors if task already deleted
		if !strings.Contains(err.Error(), "not found") {
			t.Logf("task delete warning: %v", err)
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
	t.Logf("validated event topics: task-create, task-start, task-exit, task-delete")
	t.Logf("validated exec topics: exec-added, exec-started, task-exit")
}
