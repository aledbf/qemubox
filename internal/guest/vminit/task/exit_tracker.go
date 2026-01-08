//go:build linux

package task

import (
	"sync"
	"sync/atomic"

	runcC "github.com/containerd/go-runc"

	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/process"
	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/runc"
)

// exitTracker manages process lifecycle coordination, specifically handling the race
// between process start and exit events.
//
// # Problem Statement
//
// Process exit events can arrive before Start() completes. We must:
//  1. Handle "early exits" - exits that occur before Start() returns
//  2. Match each exit to the correct process (PID reuse makes this tricky)
//  3. Ensure init process exits are published AFTER all exec process exits
//
// # Architecture
//
// The tracker is composed of two focused sub-components:
//
//  1. earlyExitDetector: Handles the subscription pattern for detecting exits
//     that occur before Start() returns to the caller.
//
//  2. exitCoordinator: Handles init/exec exit ordering to ensure init exits
//     are always published last.
//
// # Flow Diagram
//
//	preStart() ──► Subscribe() ──► [process starts] ──► HandleStart()
//	                   │                                     │
//	                   └─── collects exits during window ────┘
//
//	exec starts ──► runningExecs++ ──► ... ──► exec exits ──► runningExecs--
//	                                                                │
//	init exits ──► wait if runningExecs > 0 ◄──────────────────────┘
//
// # Concurrency Design
//
// Each sub-component has its own mutex. This is safe because:
//   - No operation requires atomicity across multiple components
//   - detector: only cares about broadcasting exits to subscriptions
//   - coordinator: only cares about exec counting and init exit stashing
//   - processes: only cares about PID-to-process mapping
//
// Operations naturally sequence: detect exit → remove process → stash if init.
// Each step is independent and doesn't need to see a consistent view of the others.
type exitTracker struct {
	detector    *earlyExitDetector
	coordinator *exitCoordinator
	processes   *processRegistry
}

func newExitTracker() *exitTracker {
	return &exitTracker{
		detector:    newEarlyExitDetector(),
		coordinator: newExitCoordinator(),
		processes:   newProcessRegistry(),
	}
}

// Subscribe registers interest in process exits that occur before Start completes.
// Returns a subscription that must be completed via HandleStart or cancelled via Cancel.
//
// If restarting an existing container (c != nil), removes its init process from
// the running map so early exits during restart are properly detected.
func (t *exitTracker) Subscribe(c *runc.Container) *subscription {
	sub := t.detector.subscribe()

	// When restarting, remove the old init process from running map
	if c != nil {
		t.processes.removeByContainer(c.Pid(), c)
	}

	return &subscription{
		sub:     sub,
		tracker: t,
	}
}

// NotifyExit handles a process exit event.
// Returns the container processes that exited (may be >1 due to PID reuse).
func (t *exitTracker) NotifyExit(e runcC.Exit) []containerProcess {
	// Notify early exit detector (broadcasts to all active subscriptions)
	t.detector.notifyExit(e)

	// Find and remove running processes with this PID
	cps := t.processes.removeByPID(e.Pid)

	// Stash init exits for later (need to wait for execs to complete)
	for _, cp := range cps {
		if cp.Process.IsInit() {
			t.coordinator.stashInitExit(cp.Container, e)
		}
	}

	return cps
}

// ShouldDelayInitExit checks if an init process exit should be delayed
// until all exec processes exit.
//
// Returns:
//   - (false, nil): No delay needed, safe to publish init exit immediately
//   - (true, chan): Delay needed, wait on channel for signal when execs complete
func (t *exitTracker) ShouldDelayInitExit(c *runc.Container) (bool, <-chan struct{}) {
	return t.coordinator.shouldDelayInitExit(c)
}

// NotifyExecExit decrements the exec counter for a container.
// If the counter reaches 0 and an init exit is waiting, signals the waiter.
func (t *exitTracker) NotifyExecExit(c *runc.Container) {
	t.coordinator.notifyExecExit(c)
}

// GetInitExit returns and clears the stashed init exit for a container.
// Returns (exit, true) if init has exited, (zero, false) otherwise.
func (t *exitTracker) GetInitExit(c *runc.Container) (runcC.Exit, bool) {
	return t.coordinator.getInitExit(c)
}

// InitHasExited checks if the container's init process has exited.
func (t *exitTracker) InitHasExited(c *runc.Container) bool {
	return t.coordinator.initHasExited(c)
}

// DecrementExecCount manually decrements the exec counter.
// Used when process start fails and HandleStart wasn't called.
func (t *exitTracker) DecrementExecCount(c *runc.Container) {
	t.coordinator.notifyExecExit(c)
}

// Cleanup removes all tracking state for a container.
// Should be called when a container is deleted.
func (t *exitTracker) Cleanup(c *runc.Container) {
	t.coordinator.cleanup(c)
	t.processes.cleanupContainer(c)
}

// subscription represents an active wait for a process to start.
// It collects any exit events that occur during the start window.
type subscription struct {
	sub     *earlyExitSubscription
	tracker *exitTracker
}

// HandleStart completes the subscription, checking for early exits.
//
// Returns exits that occurred before Start completed. If non-empty, the process
// exited before we could register it as running.
//
// If pid == 0, the process failed to start - returns any collected exits.
// Otherwise, registers the process as running and tracks exec count.
func (s *subscription) HandleStart(c *runc.Container, p process.Process, pid int) []runcC.Exit {
	// Complete subscription and check for early exits
	earlyExits := s.sub.complete(pid)

	if pid == 0 || len(earlyExits) > 0 {
		return earlyExits
	}

	// Register process as running
	s.tracker.processes.add(pid, containerProcess{
		Container: c,
		Process:   p,
	})

	// Track exec processes for init exit ordering
	if !p.IsInit() {
		s.tracker.coordinator.incrementExecCount(c)
	}

	return nil
}

// Cancel cancels the subscription without completing a start.
// Must be called if HandleStart is not called, to prevent memory leaks.
func (s *subscription) Cancel() {
	s.sub.cancel()
}

// earlyExitDetector tracks subscriptions that need to be notified of process exits.
// This handles the race condition where a process exits before Start() returns.
type earlyExitDetector struct {
	mu            sync.Mutex
	nextSubID     uint64
	subscriptions map[uint64]*earlyExitSubscription
}

func newEarlyExitDetector() *earlyExitDetector {
	return &earlyExitDetector{
		subscriptions: make(map[uint64]*earlyExitSubscription),
	}
}

// subscribe creates a new subscription that will collect exit events.
func (d *earlyExitDetector) subscribe() *earlyExitSubscription {
	d.mu.Lock()
	defer d.mu.Unlock()

	subID := atomic.AddUint64(&d.nextSubID, 1)
	sub := &earlyExitSubscription{
		id:       subID,
		detector: d,
		exits:    make(map[int][]runcC.Exit),
	}
	d.subscriptions[subID] = sub

	return sub
}

// notifyExit broadcasts an exit event to all active subscriptions.
func (d *earlyExitDetector) notifyExit(e runcC.Exit) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, sub := range d.subscriptions {
		sub.exits[e.Pid] = append(sub.exits[e.Pid], e)
	}
}

// remove removes a subscription from the detector.
func (d *earlyExitDetector) remove(id uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.subscriptions, id)
}

// earlyExitSubscription collects exit events during the start window.
type earlyExitSubscription struct {
	id       uint64
	detector *earlyExitDetector
	exits    map[int][]runcC.Exit // PID -> exits collected during start window
}

// complete finishes the subscription and returns any early exits for the given PID.
func (s *earlyExitSubscription) complete(pid int) []runcC.Exit {
	s.detector.remove(s.id)
	return s.exits[pid]
}

// cancel removes the subscription without returning exits.
func (s *earlyExitSubscription) cancel() {
	s.detector.remove(s.id)
}

// exitCoordinator ensures init exits are published after all exec exits.
// This maintains the containerd contract that init exit is the last event.
type exitCoordinator struct {
	mu         sync.Mutex
	containers map[*runc.Container]*containerExitState
}

func newExitCoordinator() *exitCoordinator {
	return &exitCoordinator{
		containers: make(map[*runc.Container]*containerExitState),
	}
}

// containerExitState holds per-container exit coordination state.
type containerExitState struct {
	// runningExecs is the number of exec processes currently running.
	// Init exit is delayed until this reaches zero.
	runningExecs int

	// execWaiter is signaled (closed) when runningExecs reaches zero.
	// Created on-demand when init exits while execs are running.
	execWaiter chan struct{}

	// initExit holds the stashed init exit event, if the init has exited.
	// Nil means init has not exited yet.
	initExit *runcC.Exit
}

// getOrCreateState returns the exit state for a container, creating it if needed.
// Caller must hold c.mu.
func (c *exitCoordinator) getOrCreateState(container *runc.Container) *containerExitState {
	state := c.containers[container]
	if state == nil {
		state = &containerExitState{}
		c.containers[container] = state
	}
	return state
}

// incrementExecCount increments the exec counter for a container.
func (c *exitCoordinator) incrementExecCount(container *runc.Container) {
	c.mu.Lock()
	defer c.mu.Unlock()

	state := c.getOrCreateState(container)
	state.runningExecs++
}

// stashInitExit stores an init exit event for later publication.
func (c *exitCoordinator) stashInitExit(container *runc.Container, e runcC.Exit) {
	c.mu.Lock()
	defer c.mu.Unlock()

	state := c.getOrCreateState(container)
	state.initExit = &e
}

// shouldDelayInitExit checks if an init exit should be delayed.
func (c *exitCoordinator) shouldDelayInitExit(container *runc.Container) (bool, <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	state := c.containers[container]
	if state == nil || state.runningExecs == 0 {
		return false, nil
	}

	// Execs still running - create waiter channel
	waitChan := make(chan struct{})
	state.execWaiter = waitChan

	return true, waitChan
}

// notifyExecExit decrements the exec counter and signals waiters if needed.
func (c *exitCoordinator) notifyExecExit(container *runc.Container) {
	c.mu.Lock()
	defer c.mu.Unlock()

	state := c.containers[container]
	if state == nil {
		return
	}

	state.runningExecs--
	if state.runningExecs > 0 {
		return
	}

	// All execs done - signal waiter if init is waiting
	if state.execWaiter != nil {
		close(state.execWaiter)
		state.execWaiter = nil
	}
}

// getInitExit returns and clears the stashed init exit.
func (c *exitCoordinator) getInitExit(container *runc.Container) (runcC.Exit, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	state := c.containers[container]
	if state == nil || state.initExit == nil {
		return runcC.Exit{}, false
	}

	exit := *state.initExit
	state.initExit = nil
	return exit, true
}

// initHasExited checks if the container's init has exited.
func (c *exitCoordinator) initHasExited(container *runc.Container) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	state := c.containers[container]
	return state != nil && state.initExit != nil
}

// cleanup removes all state for a container.
func (c *exitCoordinator) cleanup(container *runc.Container) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.containers, container)
}

// processRegistry tracks running processes by PID.
// Multiple processes may share a PID due to PID reuse.
type processRegistry struct {
	mu      sync.Mutex
	running map[int][]containerProcess
}

func newProcessRegistry() *processRegistry {
	return &processRegistry{
		running: make(map[int][]containerProcess),
	}
}

// add registers a process as running.
func (r *processRegistry) add(pid int, cp containerProcess) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.running[pid] = append(r.running[pid], cp)
}

// removeByPID removes and returns all processes with the given PID.
func (r *processRegistry) removeByPID(pid int) []containerProcess {
	r.mu.Lock()
	defer r.mu.Unlock()

	cps := r.running[pid]
	delete(r.running, pid)
	return cps
}

// removeByContainer removes processes for a specific container from a PID.
func (r *processRegistry) removeByContainer(pid int, c *runc.Container) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cps := r.running[pid]
	if len(cps) == 0 {
		return
	}

	// Filter out processes belonging to this container
	remaining := cps[:0]
	for _, cp := range cps {
		if cp.Container != c {
			remaining = append(remaining, cp)
		}
	}

	if len(remaining) > 0 {
		r.running[pid] = remaining
	} else {
		delete(r.running, pid)
	}
}

// cleanupContainer removes all processes for a container.
func (r *processRegistry) cleanupContainer(c *runc.Container) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for pid := range r.running {
		cps := r.running[pid]
		remaining := cps[:0]
		for _, cp := range cps {
			if cp.Container != c {
				remaining = append(remaining, cp)
			}
		}

		if len(remaining) > 0 {
			r.running[pid] = remaining
		} else {
			delete(r.running, pid)
		}
	}
}
