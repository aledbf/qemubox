//go:build !race

package qemu

import "sync"

// vmMutex is Instance.mu.
//
// Go's sync.Mutex is not reentrant: a goroutine that locks it twice blocks on
// itself forever. That is a real hazard here rather than a theoretical one,
// because Start and Shutdown hold this lock across the whole of a VM's lifecycle
// operation - launching QEMU, loading a migration stream, connecting vsock with
// retries, RPCs with multi-second timeouts - and call a dozen helpers while
// holding it. Every one of those helpers must therefore not take the lock, and
// nothing says so at the call site.
//
// It has caught two people out already: QMPClient() called from loadTemplate,
// which left a restored VM paused forever, and a paused-state accessor called
// from prepareGuestShutdown, which left QEMU running with nothing to wake it.
// Neither failed loudly. Both looked like a hang.
//
// So under the race detector - which is how this project's tests run - vmMutex
// notices the second lock and panics with the stack that caused it. See
// mutex_race.go. In an ordinary build it is exactly sync.Mutex and costs
// nothing; this is a type alias, not a wrapper.
type vmMutex = sync.Mutex

// reentryChecked says whether vmMutex reports re-entry, which is only true under
// the race detector. Tests that exercise the detection skip without it.
const reentryChecked = false
