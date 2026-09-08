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

	systemAPI "github.com/spin-stack/spinbox/api/spinbox/services/system/v1"
)

// Shutdown timing constants.
// These control the timeout durations during the VM shutdown sequence.
const (
	// shutdownQMPTimeout is the timeout for QMP commands during shutdown.
	shutdownQMPTimeout = 2 * time.Second

	// shutdownPrepareGuestTimeout bounds the time we wait for the guest to flush
	// filesystem state before starting poweroff.
	shutdownPrepareGuestTimeout = 5 * time.Second

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
	//
	// The client is copied out under the lock and used without it: these QMP
	// commands take up to shutdownQMPTimeout, and the lock is not held across
	// waits here. See Shutdown.
	q.mu.Lock()
	qmp := q.qmpClient
	q.mu.Unlock()

	if qmp != nil {
		logger.Info("qemu: sending CTRL+ALT+DELETE via QMP")
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownQMPTimeout)
		if err := qmp.SendCtrlAltDelete(shutdownCtx); err != nil {
			logger.WithError(err).Debug("qemu: failed to send CTRL+ALT+DELETE, trying ACPI powerdown")
			// Fall back to ACPI powerdown
			if err := qmp.Shutdown(shutdownCtx); err != nil {
				logger.WithError(err).Warning("qemu: failed to send ACPI powerdown")
			}
		}
		cancel()
	}
}

func (q *Instance) cleanupAfterFailedKill() {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Clean up QMP and TAPs before returning error
	if q.qmpClient != nil {
		_ = q.qmpClient.Close()
		q.qmpClient = nil
	}
	q.closeTAPFilesLocked()
}

func (q *Instance) prepareGuestShutdown(ctx context.Context, logger *log.Entry) {
	q.mu.Lock()
	client, paused := q.client, q.paused
	q.mu.Unlock()

	if client == nil {
		return
	}

	// A paused VM cannot answer. Its vCPUs are stopped, so the RPC never reaches
	// a guest that could reply, and the only outcome is waiting out
	// shutdownPrepareGuestTimeout - five seconds spent on a question that could
	// not be answered. A template VM is paused from the moment its state is
	// saved, and that wait was the whole of what building one cost beyond
	// booting the guest, which takes 74 ms.
	//
	// There is nothing to prepare either way: the pause is what quiesced it.
	if paused {
		logger.Debug("qemu: VM is paused, nothing to prepare for shutdown")
		return
	}

	logger.Info("qemu: requesting guest shutdown preparation")
	prepareCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownPrepareGuestTimeout)
	defer cancel()

	sysClient := systemAPI.NewTTRPCSystemServiceClient(client)
	if _, err := sysClient.PrepareShutdown(prepareCtx, &systemAPI.PrepareShutdownRequest{}); err != nil {
		logger.WithError(err).Warn("qemu: guest shutdown preparation failed")
		return
	}

	logger.Info("qemu: guest shutdown preparation completed")
}

func (q *Instance) stopQemuProcess(ctx context.Context, logger *log.Entry) error {
	// Everything this waits on is copied out first. The waits below run to several
	// seconds and the lock is not held across them; see Shutdown.
	q.mu.Lock()
	cmd, waitCh, qmp := q.cmd, q.waitCh, q.qmpClient
	q.mu.Unlock()

	// Brief wait to let guest start shutdown, then send quit
	// QEMU won't exit on its own - it always needs an explicit quit command
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	// forget clears the process handle once it is gone, so a later phase does not
	// act on a process that no longer exists.
	forget := func() {
		q.mu.Lock()
		q.cmd = nil
		q.mu.Unlock()
	}

	// Wait for guest to receive ACPI signal
	select {
	case exitErr := <-waitCh:
		// Unexpected early exit - shouldn't happen but handle it
		logger.WithError(exitErr).Debug("qemu: process exited during ACPI wait")
		forget()
		return nil
	case <-time.After(shutdownACPIWait):
		// Expected - continue to quit command
	}

	// Send quit command to tell QEMU to exit
	if qmp != nil {
		logger.Debug("qemu: sending quit command to QEMU")
		quitCtx, quitCancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownQuitTimeout)
		if err := qmp.Quit(quitCtx); err != nil {
			logger.WithError(err).Debug("qemu: failed to send quit command")
			quitCancel()
			// Fall through to SIGKILL
		} else {
			quitCancel()
			// Wait for quit to complete (should be fast - ~50ms)
			select {
			case exitErr := <-waitCh:
				if exitErr != nil && exitErr.Error() != "signal: killed" {
					logger.WithError(exitErr).Debug("qemu: process exited with error after quit")
				} else {
					logger.Info("qemu: process exited after quit command")
				}
				forget()
				return nil
			case <-time.After(shutdownQuitWait):
				// Quit didn't work - fall through to SIGKILL
				logger.Warning("qemu: quit command timeout, sending SIGKILL")
			}
		}
	}

	// Still not dead - SIGKILL as last resort
	logger.Warning("qemu: sending SIGKILL to process")
	if err := cmd.Process.Kill(); err != nil {
		logger.WithError(err).Error("qemu: failed to send SIGKILL")
		forget()
		q.cleanupAfterFailedKill()
		return fmt.Errorf("failed to kill QEMU process: %w", err)
	}
	logger.Info("qemu: sent SIGKILL to process")

	// Wait for SIGKILL to complete (with timeout)
	select {
	case exitErr := <-waitCh:
		if exitErr != nil {
			logger.WithError(exitErr).Debug("qemu: process exited after SIGKILL")
		}
	case <-time.After(shutdownKillWait):
		logger.Error("qemu: process did not exit after SIGKILL")
		forget()
		q.cleanupAfterFailedKill()
		return fmt.Errorf("process did not exit after SIGKILL")
	}
	forget()
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
//
// It takes q.mu, which is safe because nothing here waits: closing a ttrpc
// client, a socket and a FIFO handle are all immediate.
func (q *Instance) closeClientConnections(logger *log.Entry) {
	q.mu.Lock()
	defer q.mu.Unlock()

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
	q.mu.Lock()
	cancel := q.runCancel
	q.mu.Unlock()

	if cancel != nil {
		logger.Debug("qemu: cancelling background monitors")
		cancel()
	}
}

func (q *Instance) cleanupResources(logger *log.Entry) {
	q.mu.Lock()
	defer q.mu.Unlock()

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
	q.closeTAPFilesLocked()

	// Release CID lease (allows CID reuse by other VMs)
	if q.cidLease != nil {
		if err := q.cidLease.Release(); err != nil {
			logger.WithError(err).Debug("qemu: failed to release CID lease")
		}
		q.cidLease = nil
	}

	// Remove VM state directory (contains QMP socket, vsock socket, console FIFO)
	if q.stateDir != "" {
		if err := os.RemoveAll(q.stateDir); err != nil {
			logger.WithError(err).Debug("qemu: failed to remove state directory")
		}
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

	// Phase 1: Cancel background monitors
	q.cancelBackgroundMonitors(logger)

	// No lock is held across phases 2-5, and that is deliberate. They spend
	// seconds waiting: an RPC to the guest with a five-second timeout, then up to
	// four and a half more for the process to die. Holding q.mu across that is
	// what the root CLAUDE.md says not to do, and it is what made
	// prepareGuestShutdown unable to look at a field without deadlocking.
	//
	// Nothing needs it held. Shutdown is entered through a compare-and-swap out
	// of vmStateRunning, so only one goroutine is ever inside it, and every
	// reader that could race - Client, DialClient, CPUHotplugger, StartStream -
	// requires vmStateRunning, which this function has already left. Each phase
	// takes the lock where it touches fields.
	q.prepareGuestShutdown(ctx, logger)
	q.closeClientConnections(logger)
	q.shutdownGuest(ctx, logger)

	if err := q.stopQemuProcess(ctx, logger); err != nil {
		return err
	}

	q.cleanupResources(logger)
	return nil
}

// StartStream creates a new stream connection to the VM for I/O operations.
