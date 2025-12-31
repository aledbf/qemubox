//go:build linux

package task

import (
	"testing"

	runcC "github.com/containerd/go-runc"

	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/process"
	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/testutil"
)

func TestExitTracker_SubscribeAndHandleStart(t *testing.T) {
	tracker := newExitTracker()
	container := testutil.MockContainer("test-container")
	proc := &testutil.MockProcess{IDValue: "init", PIDValue: 1234}

	// Subscribe before starting
	sub := tracker.Subscribe(nil)

	// HandleStart with successful start
	earlyExits := sub.HandleStart(container, proc, 1234)

	if len(earlyExits) != 0 {
		t.Errorf("Expected no early exits, got %d", len(earlyExits))
	}

	// Verify process is tracked as running
	tracker.mu.Lock()
	running := tracker.running[1234]
	tracker.mu.Unlock()

	if len(running) != 1 {
		t.Fatalf("Expected 1 running process, got %d", len(running))
	}

	if running[0].Process.ID() != proc.ID() {
		t.Error("Process not tracked correctly")
	}
}

func TestExitTracker_EarlyExit(t *testing.T) {
	tracker := newExitTracker()
	container := testutil.MockContainer("test-container")
	proc := &testutil.MockProcess{IDValue: "init", PIDValue: 1234}

	// Subscribe before starting
	sub := tracker.Subscribe(nil)

	// Simulate exit arriving before HandleStart
	exit := runcC.Exit{Pid: 1234, Status: 0}
	tracker.NotifyExit(exit)

	// Now handle start - should get the early exit
	earlyExits := sub.HandleStart(container, proc, 1234)

	if len(earlyExits) != 1 {
		t.Fatalf("Expected 1 early exit, got %d", len(earlyExits))
	}

	if earlyExits[0].Pid != 1234 {
		t.Errorf("Expected exit for PID 1234, got %d", earlyExits[0].Pid)
	}
}

func TestExitTracker_InitExitDelayed(t *testing.T) {
	tracker := newExitTracker()
	container := testutil.MockContainer("test-container")
	initProc := &process.Init{}
	execProc := &testutil.MockProcess{IDValue: "exec1", PIDValue: 1235}

	// Start init process
	sub1 := tracker.Subscribe(nil)
	sub1.HandleStart(container, initProc, 1234)

	// Start exec process
	sub2 := tracker.Subscribe(nil)
	sub2.HandleStart(container, execProc, 1235)

	// Verify exec counter incremented
	tracker.mu.Lock()
	execCount := tracker.runningExecs[container]
	tracker.mu.Unlock()

	if execCount != 1 {
		t.Fatalf("Expected 1 running exec, got %d", execCount)
	}

	// Try to handle init exit - should be delayed
	shouldDelay, waitChan := tracker.ShouldDelayInitExit(container)

	if !shouldDelay {
		t.Error("Expected init exit to be delayed")
	}

	if waitChan == nil {
		t.Fatal("Expected non-nil wait channel")
	}

	// Verify channel is not closed yet
	select {
	case <-waitChan:
		t.Error("Wait channel should not be closed yet")
	default:
		// Expected
	}

	// Notify exec exit
	tracker.NotifyExecExit(container)

	// Now channel should be closed
	select {
	case <-waitChan:
		// Expected
	default:
		t.Error("Wait channel should be closed after exec exits")
	}
}

func TestExitTracker_InitExitNotDelayed(t *testing.T) {
	tracker := newExitTracker()
	container := testutil.MockContainer("test-container")

	// No execs running - init exit should not be delayed
	shouldDelay, waitChan := tracker.ShouldDelayInitExit(container)

	if shouldDelay {
		t.Error("Expected init exit not to be delayed when no execs running")
	}

	if waitChan != nil {
		t.Error("Expected nil wait channel when not delayed")
	}
}

func TestExitTracker_ConcurrentSubscribers(t *testing.T) {
	tracker := newExitTracker()
	container := testutil.MockContainer("test-container")

	// Create multiple concurrent subscriptions
	sub1 := tracker.Subscribe(nil)
	sub2 := tracker.Subscribe(nil)
	sub3 := tracker.Subscribe(nil)

	// Notify exit - all subscribers should receive it
	exit := runcC.Exit{Pid: 1234, Status: 0}
	tracker.NotifyExit(exit)

	// Each subscriber should see the exit
	proc := &testutil.MockProcess{IDValue: "proc", PIDValue: 1234}

	exits1 := sub1.HandleStart(container, proc, 1234)
	exits2 := sub2.HandleStart(container, proc, 1234)
	exits3 := sub3.HandleStart(container, proc, 1234)

	if len(exits1) != 1 || len(exits2) != 1 || len(exits3) != 1 {
		t.Errorf("All subscribers should receive the exit: got %d, %d, %d",
			len(exits1), len(exits2), len(exits3))
	}
}

func TestExitTracker_SubscriptionCancellation(t *testing.T) {
	tracker := newExitTracker()

	// Subscribe and cancel without HandleStart
	sub := tracker.Subscribe(nil)
	sub.Cancel()

	// Verify subscription was cleaned up
	tracker.mu.Lock()
	numSubs := len(tracker.activeSubscriptions)
	tracker.mu.Unlock()

	if numSubs != 0 {
		t.Errorf("Expected 0 active subscriptions after cancel, got %d", numSubs)
	}
}

func TestExitTracker_Cleanup(t *testing.T) {
	tracker := newExitTracker()
	container := testutil.MockContainer("test-container")
	proc := &testutil.MockProcess{IDValue: "init", PIDValue: 1234}

	// Start process
	sub := tracker.Subscribe(nil)
	sub.HandleStart(container, proc, 1234)

	// Store init exit
	exit := runcC.Exit{Pid: 1234, Status: 0}
	tracker.NotifyExit(exit)

	// Verify state exists
	tracker.mu.Lock()
	_, hasRunning := tracker.running[1234]
	_, hasInitExit := tracker.initExits[container]
	tracker.mu.Unlock()

	if hasRunning {
		t.Error("Should not have running processes after exit")
	}
	if !hasInitExit {
		t.Error("Should have init exit stored")
	}

	// Cleanup container
	tracker.Cleanup(container)

	// Verify all state cleaned up
	tracker.mu.Lock()
	_, hasRunningAfter := tracker.running[1234]
	_, hasInitExitAfter := tracker.initExits[container]
	_, hasExecCount := tracker.runningExecs[container]
	_, hasWaiter := tracker.execWaiters[container]
	tracker.mu.Unlock()

	if hasRunningAfter || hasInitExitAfter || hasExecCount || hasWaiter {
		t.Error("Cleanup did not remove all container state")
	}
}

func TestExitTracker_PIDReuse(t *testing.T) {
	tracker := newExitTracker()
	container1 := testutil.MockContainer("container-1")
	container2 := testutil.MockContainer("container-2")
	proc1 := &testutil.MockProcess{IDValue: "proc1", PIDValue: 1234}
	proc2 := &testutil.MockProcess{IDValue: "proc2", PIDValue: 1234}

	// Start both processes with same PID (simulating PID reuse)
	sub1 := tracker.Subscribe(nil)
	sub1.HandleStart(container1, proc1, 1234)

	sub2 := tracker.Subscribe(nil)
	sub2.HandleStart(container2, proc2, 1234)

	// Verify both are tracked
	tracker.mu.Lock()
	running := tracker.running[1234]
	tracker.mu.Unlock()

	if len(running) != 2 {
		t.Fatalf("Expected 2 processes with same PID, got %d", len(running))
	}

	// Exit notification should return both
	exit := runcC.Exit{Pid: 1234, Status: 0}
	exited := tracker.NotifyExit(exit)

	if len(exited) != 2 {
		t.Errorf("Expected 2 exited processes, got %d", len(exited))
	}
}

func TestExitTracker_InitHasExited(t *testing.T) {
	tracker := newExitTracker()
	container := testutil.MockContainer("test-container")

	// Initially, init has not exited
	if tracker.InitHasExited(container) {
		t.Error("Init should not have exited initially")
	}

	// Notify init exit
	initProc := &process.Init{}
	sub := tracker.Subscribe(nil)
	sub.HandleStart(container, initProc, 1234)

	exit := runcC.Exit{Pid: 1234, Status: 0}
	tracker.NotifyExit(exit)

	// Now init should be marked as exited
	if !tracker.InitHasExited(container) {
		t.Error("Init should be marked as exited")
	}
}

func TestExitTracker_DecrementExecCount(t *testing.T) {
	tracker := newExitTracker()
	container := testutil.MockContainer("test-container")
	execProc := &testutil.MockProcess{IDValue: "exec1", PIDValue: 1235}

	// Start exec process (increments counter)
	sub := tracker.Subscribe(nil)
	sub.HandleStart(container, execProc, 1235)

	tracker.mu.Lock()
	count := tracker.runningExecs[container]
	tracker.mu.Unlock()

	if count != 1 {
		t.Fatalf("Expected exec count of 1, got %d", count)
	}

	// Manually decrement (simulating error path)
	tracker.DecrementExecCount(container)

	tracker.mu.Lock()
	countAfter := tracker.runningExecs[container]
	tracker.mu.Unlock()

	if countAfter != 0 {
		t.Errorf("Expected exec count of 0 after decrement, got %d", countAfter)
	}
}
