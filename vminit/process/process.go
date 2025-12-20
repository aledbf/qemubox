// Package process manages vminit process lifecycle and IO.
package process

import (
	"context"
	"io"
	"time"

	"github.com/containerd/console"
	"github.com/containerd/containerd/v2/pkg/stdio"
)

// Process on a system
//
//nolint:interfacebloat // Process aggregates lifecycle hooks for vminit execution.
type Process interface {
	// ID returns the id for the process
	ID() string
	// Pid returns the pid for the process
	Pid() int
	// ExitStatus returns the exit status
	ExitStatus() int
	// ExitedAt is the time the process exited
	ExitedAt() time.Time
	// Stdin returns the process STDIN
	Stdin() io.Closer
	// Stdio returns io information for the container
	Stdio() stdio.Stdio
	// Status returns the process status
	Status(ctx context.Context) (string, error)
	// Wait blocks until the process has exited
	Wait()
	// Resize resizes the process console
	Resize(ws console.WinSize) error
	// Start execution of the process
	Start(ctx context.Context) error
	// Delete deletes the process and its resourcess
	Delete(ctx context.Context) error
	// Kill kills the process
	Kill(ctx context.Context, sig uint32, all bool) error
	// SetExited sets the exit status for the process
	SetExited(status int)
}
