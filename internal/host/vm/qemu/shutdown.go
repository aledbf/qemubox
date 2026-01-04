//go:build linux

package qemu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/containerd/log"
)

// Shutdown timing constants.
// These control the timeout durations during the VM shutdown sequence.
const (
	// shutdownQMPTimeout is the timeout for QMP commands during shutdown.
	shutdownQMPTimeout = 2 * time.Second

	// shutdownACPIWait is how long to wait for guest to receive ACPI signal
	// before sending the quit command.
	shutdownACPIWait = 500 * time.Millisecond

	// shutdownQuitTimeout is the timeout for the QMP quit command.
	shutdownQuitTimeout = 1 * time.Second

	// shutdownQuitWait is how long to wait for QEMU to exit after quit command.
	shutdownQuitWait = 2 * time.Second

	// shutdownKillWait is how long to wait for process to exit after SIGKILL.
	shutdownKillWait = 2 * time.Second
)

func (q *Instance) shutdownGuest(ctx context.Context, logger *log.Entry) {
	// Send graceful shutdown to guest OS
	// Try CTRL+ALT+DELETE first (more reliable for some distributions), then ACPI powerdown
	// We use a fresh context here because the caller's context might be cancelled/expired,
	// but we still need time to properly shut down the VM.
	if q.qmpClient != nil {
		logger.Info("qemu: sending CTRL+ALT+DELETE via QMP")
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownQMPTimeout)
		if err := q.qmpClient.SendCtrlAltDelete(shutdownCtx); err != nil {
			logger.WithError(err).Debug("qemu: failed to send CTRL+ALT+DELETE, trying ACPI powerdown")
			// Fall back to ACPI powerdown
			if err := q.qmpClient.Shutdown(shutdownCtx); err != nil {
				logger.WithError(err).Warning("qemu: failed to send ACPI powerdown")
			}
		}
		cancel()
	}
}

func (q *Instance) cleanupAfterFailedKill() {
	// Clean up QMP and TAPs before returning error
	if q.qmpClient != nil {
		_ = q.qmpClient.Close()
		q.qmpClient = nil
	}
	q.closeTAPFiles()
}

func (q *Instance) stopQemuProcess(ctx context.Context, logger *log.Entry) error {
	// Brief wait to let guest start shutdown, then send quit
	// QEMU won't exit on its own - it always needs an explicit quit command
	if q.cmd == nil || q.cmd.Process == nil {
		return nil
	}

	// Wait for guest to receive ACPI signal
	select {
	case exitErr := <-q.waitCh:
		// Unexpected early exit - shouldn't happen but handle it
		logger.WithError(exitErr).Debug("qemu: process exited during ACPI wait")
		q.cmd = nil
		return nil
	case <-time.After(shutdownACPIWait):
		// Expected - continue to quit command
	}

	// Send quit command to tell QEMU to exit
	if q.qmpClient != nil {
		logger.Debug("qemu: sending quit command to QEMU")
		quitCtx, quitCancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownQuitTimeout)
		if err := q.qmpClient.Quit(quitCtx); err != nil {
			logger.WithError(err).Debug("qemu: failed to send quit command")
			quitCancel()
			// Fall through to SIGKILL
		} else {
			quitCancel()
			// Wait for quit to complete (should be fast - ~50ms)
			select {
			case exitErr := <-q.waitCh:
				if exitErr != nil && exitErr.Error() != "signal: killed" {
					logger.WithError(exitErr).Debug("qemu: process exited with error after quit")
				} else {
					logger.Info("qemu: process exited after quit command")
				}
				q.cmd = nil
				return nil
			case <-time.After(shutdownQuitWait):
				// Quit didn't work - fall through to SIGKILL
				logger.Warning("qemu: quit command timeout, sending SIGKILL")
			}
		}
	}

	// Still not dead - SIGKILL as last resort
	logger.Warning("qemu: sending SIGKILL to process")
	if err := q.cmd.Process.Kill(); err != nil {
		logger.WithError(err).Error("qemu: failed to send SIGKILL")
		q.cmd = nil
		q.cleanupAfterFailedKill()
		return fmt.Errorf("failed to kill QEMU process: %w", err)
	}
	logger.Info("qemu: sent SIGKILL to process")

	// Wait for SIGKILL to complete (with timeout)
	select {
	case exitErr := <-q.waitCh:
		if exitErr != nil {
			logger.WithError(exitErr).Debug("qemu: process exited after SIGKILL")
		}
	case <-time.After(shutdownKillWait):
		logger.Error("qemu: process did not exit after SIGKILL")
		q.cmd = nil
		q.cleanupAfterFailedKill()
		return fmt.Errorf("process did not exit after SIGKILL")
	}
	q.cmd = nil
	return nil
}

// closeAndLog is a helper to close a resource and log any errors.
// It checks for nil before closing to avoid panics.
// It silently ignores "file already closed" errors since these are expected during cleanup.
func closeAndLog(logger *log.Entry, name string, closer io.Closer) {
	if closer == nil {
		return
	}
	if err := closer.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		logger.WithError(err).WithField("resource", name).Debug("failed to close resource")
	}
}

// closeClientConnections closes all client connections to the VM.
// This includes TTRPC client, vsock connection, and console FIFO.
// Must be called with q.mu held.
func (q *Instance) closeClientConnections(logger *log.Entry) {
	// Close TTRPC client to stop guest communication
	if q.client != nil {
		logger.Debug("qemu: closing TTRPC client")
		closeAndLog(logger, "ttrpc", q.client)
		q.client = nil
	}

	// Close vsock listener
	if q.vsockConn != nil {
		logger.Debug("qemu: closing vsock connection")
		closeAndLog(logger, "vsock", q.vsockConn)
		q.vsockConn = nil
	}

	// Close console FIFO to cancel the streaming goroutine.
	// This interrupts the blocked Read() and allows graceful goroutine exit.
	if q.consoleFifo != nil {
		logger.Debug("qemu: closing console FIFO to cancel streaming goroutine")
		closeAndLog(logger, "console-fifo", q.consoleFifo)
		q.consoleFifo = nil
	}
}

// cancelBackgroundMonitors cancels all background monitoring goroutines.
// This includes VM status monitors and guest RPC handlers.
func (q *Instance) cancelBackgroundMonitors(logger *log.Entry) {
	if q.runCancel != nil {
		logger.Debug("qemu: cancelling background monitors")
		q.runCancel()
	}
}

func (q *Instance) cleanupResources(logger *log.Entry) {
	// Close QMP client
	closeAndLog(logger, "qmp", q.qmpClient)
	q.qmpClient = nil

	// Close console file (this will also stop the FIFO streaming goroutine)
	closeAndLog(logger, "console", q.consoleFile)
	q.consoleFile = nil

	// Remove FIFO pipe
	if q.consoleFifoPath != "" {
		if err := os.Remove(q.consoleFifoPath); err != nil && !os.IsNotExist(err) {
			logger.WithError(err).Debug("qemu: error removing console FIFO")
		}
	}

	// Close TAP file descriptors
	q.closeTAPFiles()

	// Release CID lease (allows CID reuse by other VMs)
	if q.cidLease != nil {
		if err := q.cidLease.Release(); err != nil {
			logger.WithError(err).Debug("qemu: failed to release CID lease")
		}
		q.cidLease = nil
	}
}

// Shutdown gracefully shuts down the VM following a multi-phase process:
// 1. State transition and background monitor cancellation
// 2. Client connection closure (TTRPC, vsock, console)
// 3. Guest OS shutdown via QMP (CTRL+ALT+DELETE or ACPI)
// 4. QEMU process termination
// 5. Resource cleanup (QMP, console file, TAP FDs, FIFO)
func (q *Instance) Shutdown(ctx context.Context) error {
	logger := log.G(ctx)
	logger.Info("qemu: Shutdown() called, initiating VM shutdown")

	// Phase 1: State transition check (idempotent - prevents re-entry)
	if !q.compareAndSwapState(vmStateRunning, vmStateShutdown) {
		currentState := q.getState()
		logger.WithField("state", currentState).Debug("qemu: VM not in running state, shutdown may already be in progress")
		return nil // Not an error - idempotent shutdown
	}

	// Phase 1: Cancel background monitors before acquiring lock
	q.cancelBackgroundMonitors(logger)

	// Phase 2-5: Acquire lock for remainder of shutdown sequence
	q.mu.Lock()
	defer q.mu.Unlock()

	q.closeClientConnections(logger)
	q.shutdownGuest(ctx, logger)

	if err := q.stopQemuProcess(ctx, logger); err != nil {
		return err
	}

	q.cleanupResources(logger)
	return nil
}

// StartStream creates a new stream connection to the VM for I/O operations.
