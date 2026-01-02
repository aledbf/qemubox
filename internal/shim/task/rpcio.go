package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	"github.com/containerd/fifo"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"

	stdiov1 "github.com/aledbf/qemubox/containerd/api/services/stdio/v1"
	"github.com/aledbf/qemubox/containerd/internal/iobuf"
)

const (
	// outputRetryInitialDelay is the initial delay for output forwarding retry.
	outputRetryInitialDelay = 100 * time.Millisecond
	// outputRetryMaxDelay is the maximum delay between retries.
	outputRetryMaxDelay = 2 * time.Second
	// outputRetryMaxAttempts is the maximum number of retry attempts before giving up.
	// 0 means retry indefinitely until context is cancelled or done is signaled.
	outputRetryMaxAttempts = 0
)

// ClientDialer is a function type for dialing the guest TTRPC client.
type ClientDialer func(ctx context.Context) (*ttrpc.Client, error)

// RPCIOForwarder forwards I/O between host FIFOs and guest process via TTRPC.
// It supports multiple attach operations by maintaining persistent RPC connections.
type RPCIOForwarder struct {
	dialClient  ClientDialer
	containerID string
	execID      string

	stdinPath  string
	stdoutPath string
	stderrPath string

	mu      sync.Mutex
	started bool
	done    chan struct{}
	wg      sync.WaitGroup

	cancel context.CancelFunc

	// stdinCloser is used to signal stdin closure
	stdinCloser chan struct{}

	// clients holds TTRPC clients to be closed on shutdown
	clients   []*ttrpc.Client
	clientsMu sync.Mutex
}

type retryState struct {
	delay    time.Duration
	attempts int
}

func newRetryState() *retryState {
	return &retryState{delay: outputRetryInitialDelay}
}

func (r *retryState) reset() {
	r.delay = outputRetryInitialDelay
	r.attempts = 0
}

// NewRPCIOForwarder creates a new RPC I/O forwarder.
func NewRPCIOForwarder(dialClient ClientDialer, containerID, execID string, sio stdio.Stdio) *RPCIOForwarder {
	return &RPCIOForwarder{
		dialClient:  dialClient,
		containerID: containerID,
		execID:      execID,
		stdinPath:   sio.Stdin,
		stdoutPath:  sio.Stdout,
		stderrPath:  sio.Stderr,
		done:        make(chan struct{}),
		stdinCloser: make(chan struct{}),
	}
}

// pollReadOnce reads from a non-blocking reader and returns true if no data is available.
func pollReadOnce(reader io.Reader, buf []byte) (int, error, bool) {
	n, err := reader.Read(buf)
	if n > 0 {
		return n, err, false
	}
	if err == nil || err == io.EOF || isNoDataError(err) {
		return 0, err, true
	}
	return 0, err, false
}

// Start begins forwarding I/O. This should be called after the guest process is created.
// The passed context is used to copy log fields, but the forwarder creates its own
// long-lived context that survives until Shutdown is called.
func (f *RPCIOForwarder) Start(ctx context.Context) error {
	f.mu.Lock()
	if f.started {
		f.mu.Unlock()
		return nil
	}
	f.started = true

	// Create a long-lived context for the forwarder goroutines.
	// This context is NOT tied to the RPC request context - it lives until Shutdown.
	// We use context.WithoutCancel to preserve log fields but detach from parent cancellation.
	fwdCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	f.cancel = cancel
	f.mu.Unlock()

	log.G(fwdCtx).WithFields(log.Fields{
		"container": f.containerID,
		"exec":      f.execID,
		"stdin":     f.stdinPath,
		"stdout":    f.stdoutPath,
		"stderr":    f.stderrPath,
	}).Debug("starting RPC I/O forwarder")

	// Start stdin forwarder if stdin is configured
	if f.stdinPath != "" {
		f.wg.Add(1)
		go f.forwardStdin(fwdCtx)
	}

	// Start stdout forwarder if stdout is configured
	if f.stdoutPath != "" {
		f.wg.Add(1)
		go f.forwardStdout(fwdCtx)
	}

	// Start stderr forwarder if stderr is configured and different from stdout
	if f.stderrPath != "" && f.stderrPath != f.stdoutPath {
		f.wg.Add(1)
		go f.forwardStderr(fwdCtx)
	}

	return nil
}

// forwardStdin reads from host FIFO and writes to guest via WriteStdin RPC.
// It handles reconnection when the writer (ctr attach) disconnects and reconnects.
func (f *RPCIOForwarder) forwardStdin(ctx context.Context) {
	defer f.wg.Done()

	logger := log.G(ctx).WithField("container", f.containerID).WithField("direction", "stdin")
	logger.Debug("starting stdin forwarder")

	buf := iobuf.Get()
	defer iobuf.Put(buf)

	// Get RPC client once
	client, conn, err := f.getStdIOClient(ctx)
	if err != nil {
		logger.WithError(err).Error("failed to get stdio client")
		return
	}
	defer f.closeClient(ctx, conn)

	logger.Debug("opening stdin fifo (non-blocking)")
	fifoReader, err := fifo.OpenFifo(ctx, f.stdinPath, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			logger.Debug("stdin fifo open cancelled")
			return
		}
		logger.WithError(err).Error("failed to open stdin fifo")
		return
	}
	defer func() { _ = fifoReader.Close() }()

	logger.Debug("stdin fifo opened, forwarding data")

	pollInterval := 50 * time.Millisecond
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Debug("stdin forwarder context done")
			return
		case <-f.done:
			logger.Debug("stdin forwarder done")
			return
		case <-f.stdinCloser:
			logger.Debug("stdin closed by user")
			_, _ = client.CloseStdin(ctx, &stdiov1.CloseStdinRequest{
				ContainerId: f.containerID,
				ExecId:      f.execID,
			})
			return
		default:
			n, err, noData := pollReadOnce(fifoReader, *buf)
			if n > 0 {
				logger.WithField("bytes", n).Debug("forwarding stdin data to guest")
				_, err = client.WriteStdin(ctx, &stdiov1.WriteStdinRequest{
					ContainerId: f.containerID,
					ExecId:      f.execID,
					Data:        (*buf)[:n],
				})
				if err != nil {
					logger.WithError(err).Warn("error writing to stdin RPC")
					return
				}
				continue
			}

			if !noData {
				if err != nil && !isClosedConnError(err) {
					logger.WithError(err).Warn("error reading from stdin fifo")
				}
				return
			}

			<-ticker.C
		}
	}
}

// forwardStdout reads from guest via ReadStdout RPC and writes to host FIFO.
func (f *RPCIOForwarder) forwardStdout(ctx context.Context) {
	defer f.wg.Done()
	f.forwardOutput(ctx, "stdout", f.stdoutPath)
}

// forwardStderr reads from guest via ReadStderr RPC and writes to host FIFO.
func (f *RPCIOForwarder) forwardStderr(ctx context.Context) {
	defer f.wg.Done()
	f.forwardOutput(ctx, "stderr", f.stderrPath)
}

func (f *RPCIOForwarder) forwardOutput(ctx context.Context, streamName, path string) {
	logger := log.G(ctx).WithField("container", f.containerID).WithField("direction", streamName)
	logger.Debug("starting output forwarder")

	// Open FIFO for writing. This blocks until containerd opens the FIFO for reading.
	// This synchronization ensures containerd is ready to receive data before we write,
	// preventing a race where data is written and the FIFO closed before containerd reads.
	//
	// We use fifo.OpenFifo which handles the blocking gracefully with context cancellation.
	logger.WithField("path", path).Debug("opening output fifo for writing (will block until reader ready)")
	fifoWriter, err := fifo.OpenFifo(ctx, path, syscall.O_WRONLY, 0)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			logger.Debug("output fifo open cancelled")
			return
		}
		logger.WithError(err).Error("failed to open output fifo")
		return
	}
	defer func() { _ = fifoWriter.Close() }()
	logger.Debug("opened output fifo for writing (reader is ready)")

	// Retry loop for connection and streaming
	retry := newRetryState()

	for {
		// Check for shutdown before each attempt
		select {
		case <-ctx.Done():
			logger.Debug("output forwarder context done")
			return
		case <-f.done:
			logger.Debug("output forwarder done")
			return
		default:
		}

		// Get RPC client
		logger.Debug("getting stdio RPC client")
		client, conn, err := f.getStdIOClient(ctx)
		if err != nil {
			if !f.retryWait(ctx, logger, retry, err, "failed to get stdio client, will retry") {
				return
			}
			continue
		}
		logger.Debug("got stdio RPC client")

		// Start streaming from guest
		var stream stdiov1.StdIO_ReadStdoutClient
		req := &stdiov1.ReadOutputRequest{
			ContainerId: f.containerID,
			ExecId:      f.execID,
		}

		logger.Debug("starting RPC output stream")
		if streamName == "stdout" {
			stream, err = client.ReadStdout(ctx, req)
		} else {
			stream, err = client.ReadStderr(ctx, req)
		}
		if err != nil {
			f.closeClient(ctx, conn)
			if errdefs.IsNotFound(err) {
				logger.WithError(err).Debug("output stream not found, stopping forwarder")
				return
			}
			if !f.retryWait(ctx, logger, retry, err, "failed to start output stream, will retry") {
				return
			}
			continue
		}
		logger.Debug("RPC output stream started, waiting for data")

		// Reset retry state on successful connection
		retry.reset()

		// Stream data until error or EOF
		streamErr := f.streamOutput(ctx, logger, stream, fifoWriter)
		f.closeClient(ctx, conn)
		if streamErr == nil {
			// Clean EOF, we're done
			return
		}

		if errdefs.IsNotFound(streamErr) {
			logger.WithError(streamErr).Debug("output stream not found, stopping forwarder")
			return
		}

		// Check if this is a terminal error or if we should retry
		if errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded) {
			return
		}

		// For other errors, retry with backoff
		if !f.retryWait(ctx, logger, retry, streamErr, "output stream error, will retry") {
			return
		}
	}
}

// streamOutput reads from the RPC stream and writes to the FIFO.
// Returns nil on clean EOF, or an error if the stream should be retried.
//
// IMPORTANT: This function prioritizes draining all data from the stream
// before returning. We do NOT check ctx.Done() or f.done before receiving,
// ensuring that any data sent by the guest is received and written to the FIFO.
// The function only exits when:
// - EOF is received from the stream (clean exit)
// - An error occurs (connection closed, etc.)
func (f *RPCIOForwarder) streamOutput(ctx context.Context, logger *log.Entry, stream stdiov1.StdIO_ReadStdoutClient, fifoWriter io.Writer) error {
	for {
		// Receive from stream - this blocks until data arrives or stream closes.
		// We intentionally do NOT check ctx.Done() or f.done before calling Recv()
		// to ensure we drain all pending data before exiting.
		chunk, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				logger.Debug("output stream EOF")
				return nil
			}
			if isClosedConnError(err) {
				// Connection closed - could be transient, return error for retry
				return err
			}
			logger.WithError(err).Warn("error receiving from output stream")
			return err
		}

		if chunk.Eof {
			logger.Debug("output chunk EOF")
			return nil
		}

		if len(chunk.Data) > 0 {
			logger.WithField("bytes", len(chunk.Data)).Debug("received data from guest, writing to fifo")
			n, err := fifoWriter.Write(chunk.Data)
			if err != nil {
				logger.WithError(err).Warn("error writing to output fifo")
				// FIFO write errors are terminal - don't retry
				return nil
			}
			if n != len(chunk.Data) {
				logger.WithField("expected", len(chunk.Data)).WithField("actual", n).Warn("short write to output fifo")
			}
			logger.WithField("bytesWritten", n).Debug("successfully wrote data to fifo")
		}
	}
}

// sleepWithCancel sleeps for the given duration, returning false if cancelled.
func (f *RPCIOForwarder) sleepWithCancel(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-f.done:
		return false
	case <-timer.C:
		return true
	}
}

func (f *RPCIOForwarder) retryWait(ctx context.Context, logger *log.Entry, state *retryState, err error, msg string) bool {
	state.attempts++
	if outputRetryMaxAttempts > 0 && state.attempts >= outputRetryMaxAttempts {
		logger.WithError(err).WithField("attempts", state.attempts).Error("retry limit reached")
		return false
	}
	logger.WithError(err).WithField("retryDelay", state.delay).Debug(msg)
	if !f.sleepWithCancel(ctx, state.delay) {
		return false
	}
	state.delay = f.nextRetryDelay(state.delay)
	return true
}

// nextRetryDelay calculates the next retry delay with exponential backoff.
func (f *RPCIOForwarder) nextRetryDelay(current time.Duration) time.Duration {
	next := current * 2
	if next > outputRetryMaxDelay {
		next = outputRetryMaxDelay
	}
	return next
}

// getStdIOClient returns a StdIO client connected to the guest.
// The underlying TTRPC connection is tracked and will be closed on Shutdown.
func (f *RPCIOForwarder) getStdIOClient(ctx context.Context) (stdiov1.StdIOClient, *ttrpc.Client, error) {
	conn, err := f.dialClient(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to dial guest: %w", err)
	}

	f.trackClient(conn)

	return stdiov1.NewStdIOClient(conn), conn, nil
}

func (f *RPCIOForwarder) trackClient(conn *ttrpc.Client) {
	f.clientsMu.Lock()
	f.clients = append(f.clients, conn)
	f.clientsMu.Unlock()
}

func (f *RPCIOForwarder) closeClient(ctx context.Context, conn *ttrpc.Client) {
	if conn == nil {
		return
	}
	if err := conn.Close(); err != nil {
		log.G(ctx).WithError(err).Debug("error closing stdio TTRPC client")
	}
	f.clientsMu.Lock()
	for i, client := range f.clients {
		if client == conn {
			f.clients = append(f.clients[:i], f.clients[i+1:]...)
			break
		}
	}
	f.clientsMu.Unlock()
}

// CloseStdin signals that stdin should be closed.
func (f *RPCIOForwarder) CloseStdin() {
	select {
	case f.stdinCloser <- struct{}{}:
	default:
	}
}

// WaitForComplete blocks until all I/O forwarder goroutines complete naturally.
// This should be called when waiting for I/O to finish (e.g., before forwarding
// exit events) without forcing shutdown. Returns immediately if not started.
func (f *RPCIOForwarder) WaitForComplete() {
	f.mu.Lock()
	started := f.started
	f.mu.Unlock()
	if !started {
		return
	}
	f.wg.Wait()
}

// Shutdown stops all I/O forwarding and cleans up resources.
func (f *RPCIOForwarder) Shutdown(ctx context.Context) error {
	f.mu.Lock()
	if !f.started {
		f.mu.Unlock()
		return nil
	}
	f.mu.Unlock()

	log.G(ctx).WithField("container", f.containerID).Debug("shutting down RPC I/O forwarder")

	// Cancel the forwarder context to stop all goroutines
	if f.cancel != nil {
		f.cancel()
	}

	// Signal done and wait for goroutines to finish
	close(f.done)
	f.wg.Wait()

	// Close all tracked TTRPC clients
	f.clientsMu.Lock()
	for _, client := range f.clients {
		if err := client.Close(); err != nil {
			log.G(ctx).WithError(err).Debug("error closing stdio TTRPC client")
		}
	}
	f.clients = nil
	f.clientsMu.Unlock()

	return nil
}

// GuestStdio returns the stdio configuration to pass to the guest.
// This uses the rpcio:// scheme to indicate RPC-based I/O.
func (f *RPCIOForwarder) GuestStdio() stdio.Stdio {
	var stdin, stdout, stderr string
	if f.stdinPath != "" {
		stdin = fmt.Sprintf("rpcio://%s/%s", f.containerID, f.execID)
	}
	if f.stdoutPath != "" {
		stdout = fmt.Sprintf("rpcio://%s/%s", f.containerID, f.execID)
	}
	if f.stderrPath != "" {
		stderr = fmt.Sprintf("rpcio://%s/%s", f.containerID, f.execID)
	}
	return stdio.Stdio{
		Stdin:    stdin,
		Stdout:   stdout,
		Stderr:   stderr,
		Terminal: false,
	}
}

// forwardIORPC sets up RPC-based I/O forwarding for non-TTY mode.
// Returns the guest stdio config and the forwarder (caller starts after guest Create).
func (s *service) forwardIORPC(ctx context.Context, containerID, execID string, sio stdio.Stdio) (stdio.Stdio, IOForwarder, error) {
	forwarder := NewRPCIOForwarder(s.vmLifecycle.DialClient, containerID, execID, sio)

	guestStdio := forwarder.GuestStdio()

	log.G(ctx).WithFields(log.Fields{
		"container":   containerID,
		"exec":        execID,
		"guestStdin":  guestStdio.Stdin,
		"guestStdout": guestStdio.Stdout,
		"guestStderr": guestStdio.Stderr,
	}).Debug("RPC I/O forwarder created (not yet started)")

	return guestStdio, forwarder, nil
}
