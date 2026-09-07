//go:build race

package qemu

import (
	"bytes"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
)

// vmMutex is a sync.Mutex that refuses to deadlock against itself.
//
// See mutex.go for why Instance.mu needs this. In short: Start and Shutdown hold
// it across multi-second operations and call many helpers underneath, so a
// helper that takes the lock hangs the VM - silently, and looking exactly like
// the VM being slow. Panicking with the stack that did it turns a hang into a
// failing test.
//
// This build is selected by the race detector rather than by a tag of its own,
// so that it is on wherever `go test -race` runs - which is the project's
// pre-commit checklist and its CI - and off in every binary that ships. It is
// not itself a race detector; it detects a mistake the race detector cannot see,
// because a goroutine deadlocking against itself races with nothing.
type vmMutex struct {
	mu sync.Mutex

	// holder is the goroutine currently holding the lock, or 0. Written under
	// the lock and read before taking it, which is why it is atomic: the read
	// happens from a goroutine that does not hold the lock yet, and may never.
	holder atomic.Uint64
}

const reentryChecked = true

// Lock takes the lock, or panics if this goroutine already holds it.
func (m *vmMutex) Lock() {
	self := goroutineID()
	if m.holder.Load() == self {
		panic(fmt.Sprintf(
			"qemu: goroutine %d is locking Instance.mu while already holding it, "+
				"which would block forever.\n"+
				"A method that takes the lock was called from one that already holds it - "+
				"Start and Shutdown hold it for their whole bodies. Read the field directly, "+
				"or split the method into a locking and a non-locking half.\n%s",
			self, stack()))
	}
	m.mu.Lock()
	m.holder.Store(self)
}

// Unlock releases the lock.
func (m *vmMutex) Unlock() {
	m.holder.Store(0)
	m.mu.Unlock()
}

// goroutineID returns the current goroutine's id.
//
// There is no supported way to ask for it, so this reads it out of the first
// line of a stack dump, which is "goroutine 123 [running]:". That is expensive
// and unlovely, and it is why this file is only built under the race detector -
// where a stack dump per lock is lost among everything else the detector does.
func goroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	field := bytes.Fields(bytes.TrimPrefix(buf[:n], []byte("goroutine ")))
	if len(field) == 0 {
		return 0
	}
	id, err := strconv.ParseUint(string(field[0]), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// stack returns the current goroutine's stack, for the panic message: the point
// of this whole file is to name the call that took the lock twice.
func stack() []byte {
	buf := make([]byte, 8192)
	return buf[:runtime.Stack(buf, false)]
}
