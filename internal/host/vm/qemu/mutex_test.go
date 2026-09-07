//go:build linux

package qemu

import (
	"strings"
	"testing"
)

// TestMutexCatchesReentry reproduces the shape of the two bugs vmMutex exists
// for: a method that takes Instance.mu, called from one that already holds it.
//
// Both real cases looked like a hang. The first left a restored VM paused
// forever waiting for a migrate-incoming nobody could send; the second left QEMU
// running with nothing to wake it. Neither produced an error, a log line, or a
// failing test - only a VM that never came up, and a process to find with ps.
func TestMutexCatchesReentry(t *testing.T) {
	if !reentryChecked {
		t.Skip("re-entry is only detected under the race detector; run with -race")
	}

	q := &Instance{}

	defer func() {
		p := recover()
		if p == nil {
			t.Fatal("locking Instance.mu twice from one goroutine did not panic; it would have deadlocked")
		}
		msg, ok := p.(string)
		if !ok {
			t.Fatalf("panicked with %T, want the diagnostic string", p)
		}
		// The message has to say what to do about it, because the person
		// reading it is looking at a stack trace in a test they did not write.
		for _, want := range []string{"already holding it", "Start and Shutdown"} {
			if !strings.Contains(msg, want) {
				t.Errorf("panic message does not mention %q:\n%s", want, msg)
			}
		}
		if !strings.Contains(msg, "TestMutexCatchesReentry") {
			t.Errorf("panic message does not carry the offending stack:\n%s", msg)
		}
	}()

	// Start and Shutdown look like this: take the lock for the whole body, then
	// call a helper.
	q.mu.Lock()
	defer q.mu.Unlock()

	// The helper takes it too. This is the bug: control never reaches the next
	// line, under the detector because it panics and without it because it
	// blocks on itself forever.
	q.mu.Lock() //nolint:staticcheck // the critical section is empty on purpose; reaching it is the failure
	defer q.mu.Unlock()
	t.Fatal("unreachable: the second Lock must not return")
}

// TestMutexAllowsSequentialLocks checks the detector does not fire on ordinary
// use: the same goroutine locking again after unlocking is correct, and is what
// every method here does.
func TestMutexAllowsSequentialLocks(t *testing.T) {
	q := &Instance{}
	for range 3 {
		q.mu.Lock()
		q.mu.Unlock() //nolint:staticcheck // exercising lock/unlock, not guarding anything
	}
}

// TestMutexAllowsAnotherGoroutineToWait checks that a second goroutine blocking
// on the lock is still just blocking, not a reported error. Only the *same*
// goroutine re-entering is a deadlock.
func TestMutexAllowsAnotherGoroutineToWait(t *testing.T) {
	q := &Instance{}
	q.mu.Lock()

	locked := make(chan struct{})
	go func() {
		q.mu.Lock()
		q.mu.Unlock() //nolint:staticcheck // exercising lock/unlock, not guarding anything
		close(locked)
	}()

	q.mu.Unlock()
	<-locked
}
