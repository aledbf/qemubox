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

	// Verify process is tracked by triggering exit and checking result
	exit := runcC.Exit{Pid: 1234, Status: 0}
	exited := tracker.NotifyExit(exit)

	if len(exited) != 1 {
		t.Fatalf("Expected 1 running process to exit, got %d", len(exited))
	}

	if exited[0].Process.ID() != proc.ID() {
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

	// Try to handle init exit - should be delayed because exec is still running
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

	// Create a new subscription to verify internal state is clean
	sub2 := tracker.Subscribe(nil)

	// Notify exit - should not affect cancelled subscription
	exit := runcC.Exit{Pid: 1234, Status: 0}
	tracker.NotifyExit(exit)

	// Only sub2 should receive the exit, not the cancelled sub1
	proc := &testutil.MockProcess{IDValue: "proc", PIDValue: 1234}
	exits := sub2.HandleStart(testutil.MockContainer("test"), proc, 1234)

	if len(exits) != 1 {
		t.Errorf("Expected 1 exit in new subscription, got %d", len(exits))
	}
}

func TestExitTracker_Cleanup(t *testing.T) {
	tracker := newExitTracker()
	container := testutil.MockContainer("test-container")
	// Use *process.Init so NotifyExit correctly identifies it as an init process
	proc := &process.Init{}

	// Start process
	sub := tracker.Subscribe(nil)
	sub.HandleStart(container, proc, 1234)

	// Store init exit
	exit := runcC.Exit{Pid: 1234, Status: 0}
	exited := tracker.NotifyExit(exit)

	if len(exited) != 1 {
		t.Fatalf("Expected 1 exited process, got %d", len(exited))
	}

	// Verify init exit is stored
	if !tracker.InitHasExited(container) {
		t.Error("Should have init exit stored")
	}

	// Cleanup container
	tracker.Cleanup(container)

	// Verify init exit state is cleaned up
	if tracker.InitHasExited(container) {
		t.Error("Cleanup did not remove init exit state")
	}

	// Verify container state is cleaned up by checking ShouldDelayInitExit returns false
	shouldDelay, _ := tracker.ShouldDelayInitExit(container)
	if shouldDelay {
		t.Error("Cleanup did not remove container state")
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

	// Check that init exit would be delayed (meaning exec count > 0)
	shouldDelay, waitChan := tracker.ShouldDelayInitExit(container)
	if !shouldDelay {
		t.Fatal("Expected init exit to be delayed with exec running")
	}

	// Manually decrement (simulating error path)
	tracker.DecrementExecCount(container)

	// Wait channel should now be closed
	select {
	case <-waitChan:
		// Expected - channel closed when exec count reached 0
	default:
		t.Error("Wait channel should be closed after DecrementExecCount")
	}

	// Now init exit should not be delayed
	shouldDelayAfter, _ := tracker.ShouldDelayInitExit(container)
	if shouldDelayAfter {
		t.Error("Expected init exit not to be delayed after decrement")
	}
}

func TestExitTracker_GetInitExit(t *testing.T) {
	tracker := newExitTracker()
	container := testutil.MockContainer("test-container")

	// Initially, no init exit
	_, ok := tracker.GetInitExit(container)
	if ok {
		t.Error("Should not have init exit initially")
	}

	// Start and exit init process
	initProc := &process.Init{}
	sub := tracker.Subscribe(nil)
	sub.HandleStart(container, initProc, 1234)

	exit := runcC.Exit{Pid: 1234, Status: 42}
	tracker.NotifyExit(exit)

	// Now should have init exit
	gotExit, ok := tracker.GetInitExit(container)
	if !ok {
		t.Fatal("Should have init exit after NotifyExit")
	}

	if gotExit.Status != 42 {
		t.Errorf("Expected exit status 42, got %d", gotExit.Status)
	}

	// GetInitExit clears the exit - second call should return false
	_, ok = tracker.GetInitExit(container)
	if ok {
		t.Error("GetInitExit should clear the exit after first retrieval")
	}
}
