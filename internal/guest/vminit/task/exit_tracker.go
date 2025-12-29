//go:build linux

package task

import (
	"sync"
	"sync/atomic"

	runcC "github.com/containerd/go-runc"

	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/process"
	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/runc"
)

// exitTracker manages the complex lifecycle coordination between process starts and exits.
//
// The Problem:
// Process exit events can arrive before Start() completes. We need to:
// 1. Ensure every exit is matched to the right process
// 2. Handle "early exits" (exit before Start() returns)
// 3. Ensure init process exits are published AFTER all exec process exits
//
// The Solution:
// - Use a subscription pattern: each Start registers interest in exits
// - When an exit arrives, notify ALL active subscriptions (they'll check if it's theirs)
// - Track running execs per container to delay init exit publication
type exitTracker struct {
	mu sync.Mutex

	// Monotonic counter for subscription IDs
	nextSubID uint64

	// Active subscriptions waiting for process start to complete
	// subscription ID -> exits collected while subscribed
	activeSubscriptions map[uint64]map[int][]runcC.Exit

	// Running processes by PID
	// pid -> list of container processes (usually 1, but PID reuse can cause >1)
	running map[int][]containerProcess

	// Running exec process count per container
	// Used to delay init exit until all execs have exited
	runningExecs map[*runc.Container]int

	// Channels waiting for exec count to reach 0
	// Only used during init exit handling
	execWaiters map[*runc.Container]chan struct{}

	// Stashed init exits waiting for exec count to reach 0
	initExits map[*runc.Container]runcC.Exit
}

func newExitTracker() *exitTracker {
	return &exitTracker{
		activeSubscriptions: make(map[uint64]map[int][]runcC.Exit),
		running:             make(map[int][]containerProcess),
		runningExecs:        make(map[*runc.Container]int),
		execWaiters:         make(map[*runc.Container]chan struct{}),
		initExits:           make(map[*runc.Container]runcC.Exit),
	}
}

// subscription represents an active wait for a process to start
type subscription struct {
	id      uint64
	tracker *exitTracker
	exits   map[int][]runcC.Exit
}

// Subscribe registers interest in process exits that occur before Start completes.
// Returns a subscription that must be handled via HandleStart or Cancel.
//
// If restarting an existing container (c != nil), removes the init process from
// the running map so early exits are properly detected.
func (t *exitTracker) Subscribe(c *runc.Container) *subscription {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Allocate unique subscription ID
	subID := atomic.AddUint64(&t.nextSubID, 1)

	exits := make(map[int][]runcC.Exit)
	t.activeSubscriptions[subID] = exits

	// If restarting a container, remove its init process from running map
	// so that if it exits before Start completes, we treat it as an early exit
	if c != nil {
		pid := c.Pid()
		var newRunning []containerProcess
		for _, cp := range t.running[pid] {
			if cp.Container != c {
				newRunning = append(newRunning, cp)
			}
		}
		if len(newRunning) > 0 {
			t.running[pid] = newRunning
		} else {
			delete(t.running, pid)
		}
	}

	return &subscription{
		id:      subID,
		tracker: t,
		exits:   exits,
	}
}

// HandleStart processes the start of a process, checking for early exits.
// Returns exits that occurred before Start completed (early exits).
//
// If pid == 0, the process failed to start - returns any collected exits.
// Otherwise, registers the process as running.
func (s *subscription) HandleStart(c *runc.Container, p process.Process, pid int) []runcC.Exit {
	t := s.tracker
	t.mu.Lock()
	defer t.mu.Unlock()

	// Unsubscribe - we're done collecting exits
	delete(t.activeSubscriptions, s.id)

	// Check if process exited before we could record it as started
	earlyExits := s.exits[pid]

	if pid == 0 || len(earlyExits) > 0 {
		// Process failed to start or already exited
		return earlyExits
	}

	// Process started successfully - record it as running
	t.running[pid] = append(t.running[pid], containerProcess{
		Container: c,
		Process:   p,
	})

	// Track exec processes for init exit ordering
	if _, isInit := p.(*process.Init); !isInit {
		t.runningExecs[c]++
	}

	return nil
}

// Cancel cancels the subscription without handling a start.
// Must be called if HandleStart is not called, to prevent memory leaks.
func (s *subscription) Cancel() {
	t := s.tracker
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.activeSubscriptions, s.id)
}

// NotifyExit handles a process exit event.
// Returns the container processes that exited (may be >1 due to PID reuse).
func (t *exitTracker) NotifyExit(e runcC.Exit) []containerProcess {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Notify all active subscriptions (for processes that haven't started yet)
	for _, exits := range t.activeSubscriptions {
		exits[e.Pid] = append(exits[e.Pid], e)
	}

	// Find running processes that exited
	cps := t.running[e.Pid]
	delete(t.running, e.Pid)

	// Track init exits separately (need to wait for execs)
	for _, cp := range cps {
		if _, isInit := cp.Process.(*process.Init); isInit {
			t.initExits[cp.Container] = e
		}
	}

	return cps
}

// ShouldDelayInitExit checks if an init process exit should be delayed
// until all exec processes exit.
//
// Returns:
// - true + nil channel: delay needed, no execs running yet (safe to publish immediately)
// - true + non-nil channel: delay needed, wait on channel for signal
// - false + nil channel: no delay needed
func (t *exitTracker) ShouldDelayInitExit(c *runc.Container) (bool, <-chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()

	numExecs := t.runningExecs[c]
	if numExecs == 0 {
		// No execs running, safe to publish immediately
		delete(t.runningExecs, c)
		return false, nil
	}

	// Execs still running - need to wait
	waitChan := make(chan struct{})
	t.execWaiters[c] = waitChan

	return true, waitChan
}

// NotifyExecExit decrements the exec counter for a container.
// If the counter reaches 0 and an init exit is waiting, signals the waiter.
func (t *exitTracker) NotifyExecExit(c *runc.Container) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.runningExecs[c]--

	if t.runningExecs[c] == 0 {
		delete(t.runningExecs, c)

		// Signal init exit waiter if one exists
		if waitChan, ok := t.execWaiters[c]; ok {
			close(waitChan)
			delete(t.execWaiters, c)
		}
	}
}

// GetInitExit returns the stashed init exit for a container, if any.
func (t *exitTracker) GetInitExit(c *runc.Container) (runcC.Exit, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	e, ok := t.initExits[c]
	if ok {
		delete(t.initExits, c)
	}
	return e, ok
}

// InitHasExited checks if the container's init process has exited.
func (t *exitTracker) InitHasExited(c *runc.Container) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	_, exited := t.initExits[c]
	return exited
}

// DecrementExecCount manually decrements the exec counter.
// Used when process start fails and HandleStart wasn't called.
func (t *exitTracker) DecrementExecCount(c *runc.Container) {
	t.NotifyExecExit(c)
}

// Cleanup removes all tracking state for a container.
// Should be called when a container is deleted.
func (t *exitTracker) Cleanup(c *runc.Container) {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.initExits, c)
	delete(t.runningExecs, c)
	delete(t.execWaiters, c)

	// Clean up any processes for this container from running map
	for pid, cps := range t.running {
		var remaining []containerProcess
		for _, cp := range cps {
			if cp.Container != c {
				remaining = append(remaining, cp)
			}
		}
		if len(remaining) > 0 {
			t.running[pid] = remaining
		} else {
			delete(t.running, pid)
		}
	}
}
