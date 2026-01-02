package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	"github.com/containerd/fifo"
	"github.com/containerd/log"

	"github.com/aledbf/qemubox/containerd/internal/host/vm"
	"github.com/aledbf/qemubox/containerd/internal/iobuf"
)

const (
	defaultScheme = "fifo"
)

// isFifoScheme returns true if the path is a bare path (no scheme) or uses the fifo:// scheme.
// These are the paths used by containerd for attach functionality.
func isFifoScheme(path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	u, err := url.Parse(path)
	if err != nil {
		return false, err
	}
	// Bare paths have no scheme, and we default to fifo
	// Explicit fifo:// scheme also counts
	return u.Scheme == "" || u.Scheme == "fifo", nil
}

type forwardIOSetup struct {
	pio         stdio.Stdio
	streams     [3]io.ReadWriteCloser
	passthrough bool
	usePIOPaths bool
	// filePaths stores the actual file paths when using file:// scheme
	// These are used on the host for copyStreams, while pio contains stream:// URIs for the VM
	stdoutFilePath string
	stderrFilePath string
}

// IOForwarder owns a complete I/O forwarding lifecycle.
//
// Implementations:
//   - directForwarder: Uses synchronous io.Copy goroutines. Start/CloseStdin/WaitForComplete are no-ops.
//   - RPCIOForwarder: Uses TTRPC streaming. All methods are active.
type IOForwarder interface {
	// GuestStdio returns the stdio config to pass to the guest.
	GuestStdio() stdio.Stdio
	// Start begins forwarding. Must be called after guest process is created.
	Start(ctx context.Context) error
	// Shutdown stops forwarding and cleans up resources.
	Shutdown(ctx context.Context) error
	// CloseStdin signals that stdin should be closed.
	CloseStdin()
	// WaitForComplete blocks until I/O is complete (without shutting down).
	WaitForComplete()
}

type directForwarder struct {
	guest    stdio.Stdio
	shutdown func(context.Context) error

	// FIFO keepalive: read-side FDs opened at setup, kept alive until Shutdown.
	// This prevents FIFO buffer loss if containerd hasn't opened its read side yet.
	// See Kata Containers pattern: process.rs stdout_r/stderr_r
	stdoutKeepalive io.Closer
	stderrKeepalive io.Closer
}

func (d *directForwarder) GuestStdio() stdio.Stdio {
	return d.guest
}

func (d *directForwarder) Start(ctx context.Context) error {
	return nil
}

func (d *directForwarder) Shutdown(ctx context.Context) error {
	var err error
	if d.shutdown != nil {
		err = d.shutdown(ctx)
	}

	// Close FIFO keepalive FDs LAST, after all I/O is complete.
	// This ensures the FIFO buffer persists until containerd has read all data.
	if d.stdoutKeepalive != nil {
		if closeErr := d.stdoutKeepalive.Close(); closeErr != nil {
			log.G(ctx).WithError(closeErr).Debug("error closing stdout keepalive FIFO")
		}
		d.stdoutKeepalive = nil
	}
	if d.stderrKeepalive != nil {
		if closeErr := d.stderrKeepalive.Close(); closeErr != nil {
			log.G(ctx).WithError(closeErr).Debug("error closing stderr keepalive FIFO")
		}
		d.stderrKeepalive = nil
	}

	return err
}

func (d *directForwarder) CloseStdin() {}

func (d *directForwarder) WaitForComplete() {
	// Direct forwarder uses synchronous io.Copy goroutines.
	// WaitForComplete is a no-op since the shutdown function handles waiting.
}

func setupForwardIO(ctx context.Context, vmi vm.Instance, pio stdio.Stdio) (forwardIOSetup, error) {
	log.G(ctx).WithFields(log.Fields{
		"stdin":    pio.Stdin,
		"stdout":   pio.Stdout,
		"stderr":   pio.Stderr,
		"terminal": pio.Terminal,
	}).Debug("setupForwardIO: entry")

	u, err := url.Parse(pio.Stdout)
	if err != nil {
		return forwardIOSetup{}, fmt.Errorf("unable to parse stdout uri: %w", err)
	}
	if u.Scheme == "" {
		u.Scheme = defaultScheme
	}
	log.G(ctx).WithField("scheme", u.Scheme).Debug("setupForwardIO: parsed scheme")

	switch u.Scheme {
	case "stream":
		// Pass through - no stream setup needed
		return forwardIOSetup{pio: pio, passthrough: true}, nil
	case "file":
		return setupFileScheme(ctx, vmi, pio, u.Path)
	case "fifo", "binary", "pipe":
		return setupStreamScheme(ctx, vmi, pio)
	default:
		return forwardIOSetup{}, fmt.Errorf("unsupported STDIO scheme %s: %w", u.Scheme, errdefs.ErrNotImplemented)
	}
}

// setupFileScheme handles the "file://" URI scheme.
// It creates VM-side streams and saves the original file path for host-side copying.
func setupFileScheme(ctx context.Context, vmi vm.Instance, pio stdio.Stdio, stdoutFilePath string) (forwardIOSetup, error) {
	log.G(ctx).WithField("stdoutFilePath", stdoutFilePath).Debug("file scheme: using file path for logging")

	// Validate parent directory can be created for stdout
	if err := os.MkdirAll(filepath.Dir(stdoutFilePath), 0750); err != nil {
		return forwardIOSetup{}, fmt.Errorf("failed to create parent directory for stdout: %w", err)
	}

	// Parse stderr path - it may be different from stdout
	stderrFilePath := stdoutFilePath // default to same as stdout
	if pio.Stderr != "" && pio.Stderr != pio.Stdout {
		stderrURL, err := url.Parse(pio.Stderr)
		if err != nil {
			return forwardIOSetup{}, fmt.Errorf("unable to parse stderr uri: %w", err)
		}
		if stderrURL.Scheme == "file" || stderrURL.Scheme == "" {
			stderrFilePath = stderrURL.Path
			// Validate parent directory for stderr if different
			if stderrFilePath != stdoutFilePath {
				if err := os.MkdirAll(filepath.Dir(stderrFilePath), 0750); err != nil {
					return forwardIOSetup{}, fmt.Errorf("failed to create parent directory for stderr: %w", err)
				}
			}
		}
	}

	// createStreams replaces pio.Stdout/Stderr with stream:// URIs for the VM
	streamPio, streams, err := createStreams(ctx, vmi, pio)
	if err != nil {
		return forwardIOSetup{}, err
	}

	log.G(ctx).WithFields(log.Fields{
		"stdout":         streamPio.Stdout,
		"stderr":         streamPio.Stderr,
		"stdoutFilePath": stdoutFilePath,
		"stderrFilePath": stderrFilePath,
	}).Debug("file scheme: created streams, will copy to file on host")

	// Return setup with:
	// - streamPio: Contains stream:// URIs for VM
	// - stdoutFilePath/stderrFilePath: Original file paths for host-side copyStreams
	return forwardIOSetup{
		pio:            streamPio,
		streams:        streams,
		usePIOPaths:    true,
		stdoutFilePath: stdoutFilePath,
		stderrFilePath: stderrFilePath,
	}, nil
}

// setupStreamScheme handles "fifo://", "binary://", and "pipe://" URI schemes.
// It creates VM-side streams and uses the original pio paths for host-side I/O.
func setupStreamScheme(ctx context.Context, vmi vm.Instance, pio stdio.Stdio) (forwardIOSetup, error) {
	streamPio, streams, err := createStreams(ctx, vmi, pio)
	if err != nil {
		return forwardIOSetup{}, err
	}
	return forwardIOSetup{pio: streamPio, streams: streams, usePIOPaths: false}, nil
}

func (s *service) forwardIO(ctx context.Context, vmi vm.Instance, sio stdio.Stdio) (stdio.Stdio, IOForwarder, error) {
	return s.forwardIOWithIDs(ctx, vmi, "", "", sio)
}

// forwardIOWithIDs sets up I/O forwarding between host and guest.
// For non-TTY mode with fifo:// scheme, it uses RPC-based I/O (supports attach).
// For other schemes (file://, binary://, pipe://, stream://), it uses direct streaming.
// Returns:
//   - guestStdio: the stdio config to pass to the guest
//   - startFunc: function to call AFTER guest creates the process (nil for non-RPC modes)
//   - shutdownFunc: function to call to shut down I/O forwarding
//   - forwarder: the RPC forwarder (nil for non-RPC modes) - used for CloseIO
//   - error: any error during setup
func (s *service) forwardIOWithIDs(ctx context.Context, vmi vm.Instance, containerID, execID string, sio stdio.Stdio) (stdio.Stdio, IOForwarder, error) {
	// When using a terminal, stderr is not used (it's merged into stdout/pty)
	if sio.Terminal {
		sio.Stderr = ""
	}
	pio := sio
	if pio.IsNull() {
		return pio, nil, nil
	}

	// For non-TTY mode with fifo:// scheme (or bare paths) on all configured streams,
	// use RPC-based I/O to support task attach. Other schemes use direct streaming.
	// TTY mode always uses direct streams (unchanged behavior).
	if !sio.Terminal && containerID != "" {
		stdinIsFifo, stdinErr := isFifoScheme(sio.Stdin)
		stdoutIsFifo, stdoutErr := isFifoScheme(sio.Stdout)
		stderrIsFifo, stderrErr := isFifoScheme(sio.Stderr)
		if stdinErr != nil || stdoutErr != nil || stderrErr != nil {
			log.G(ctx).WithFields(log.Fields{
				"stdinErr":  stdinErr,
				"stdoutErr": stdoutErr,
				"stderrErr": stderrErr,
			}).Debug("skipping RPC I/O due to stdio URI parse error")
		} else if (sio.Stdin == "" || stdinIsFifo) && (sio.Stdout == "" || stdoutIsFifo) && (sio.Stderr == "" || stderrIsFifo) {
			log.G(ctx).WithFields(log.Fields{
				"container": containerID,
				"exec":      execID,
				"terminal":  sio.Terminal,
			}).Debug("using RPC-based I/O for non-TTY fifo mode")
			guestStdio, forwarder, err := s.forwardIORPC(ctx, containerID, execID, sio)
			return guestStdio, forwarder, err
		}
	}
	if !sio.Terminal && containerID != "" {
		log.G(ctx).WithFields(log.Fields{
			"container": containerID,
			"exec":      execID,
			"terminal":  sio.Terminal,
		}).Debug("using direct I/O for non-TTY non-fifo mode")
	}

	setup, err := setupForwardIO(ctx, vmi, pio)
	if err != nil {
		return stdio.Stdio{}, nil, err
	}
	if setup.passthrough {
		return setup.pio, &directForwarder{guest: setup.pio}, nil
	}
	pio = setup.pio
	streams := setup.streams

	defer func() {
		if err != nil {
			for i, c := range streams {
				if c != nil && (i != 2 || c != streams[1]) {
					_ = c.Close()
				}
			}
		}
	}()
	ioDone := make(chan struct{})
	stdinPath := sio.Stdin
	stdoutPath := sio.Stdout
	stderrPath := sio.Stderr
	if setup.usePIOPaths {
		// Use the saved file paths for copyStreams on the host
		// pio.Stdout/Stderr contain stream:// URIs which will be sent to the VM
		stdoutPath = setup.stdoutFilePath
		stderrPath = setup.stderrFilePath
		log.G(ctx).WithFields(log.Fields{
			"stdoutPath":  stdoutPath,
			"stderrPath":  stderrPath,
			"vmStdout":    pio.Stdout,
			"vmStderr":    pio.Stderr,
			"usePIOPaths": setup.usePIOPaths,
		}).Debug("forwardIO: using file paths for copyStreams, stream URIs for VM")
	}
	keepalives, err := copyStreams(ctx, streams, stdinPath, stdoutPath, stderrPath, ioDone)
	if err != nil {
		return stdio.Stdio{}, nil, err
	}
	shutdown := func(ctx context.Context) error {
		for i, c := range streams {
			if c != nil && (i != 2 || c != streams[1]) {
				_ = c.Close()
			}
		}
		select {
		case <-ioDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return pio, &directForwarder{
		guest:           pio,
		shutdown:        shutdown,
		stdoutKeepalive: keepalives.stdout,
		stderrKeepalive: keepalives.stderr,
	}, nil
}

func createStreams(ctx context.Context, vmi vm.Instance, sio stdio.Stdio) (stdio.Stdio, [3]io.ReadWriteCloser, error) {
	var conns [3]io.ReadWriteCloser
	var retErr error
	defer func() {
		if retErr != nil {
			for i, c := range conns {
				if c != nil && (i != 2 || c != conns[1]) {
					_ = c.Close()
				}
			}
		}
	}()
	if sio.Stdin != "" {
		sid, conn, err := vmi.StartStream(ctx)
		if err != nil {
			retErr = fmt.Errorf("failed to start fifo stream: %w", err)
			return sio, conns, retErr
		}
		sio.Stdin = fmt.Sprintf("stream://%d", sid)
		conns[0] = conn
	}

	stdout := sio.Stdout
	if stdout != "" {
		sid, conn, err := vmi.StartStream(ctx)
		if err != nil {
			retErr = fmt.Errorf("failed to start fifo stream: %w", err)
			return sio, conns, retErr
		}
		sio.Stdout = fmt.Sprintf("stream://%d", sid)
		conns[1] = conn
	}

	if sio.Stderr != "" {
		if sio.Stderr == stdout {
			sio.Stderr = sio.Stdout
			conns[2] = conns[1]
		} else {
			sid, conn, err := vmi.StartStream(ctx)
			if err != nil {
				retErr = fmt.Errorf("failed to start fifo stream: %w", err)
				return sio, conns, retErr
			}
			sio.Stderr = fmt.Sprintf("stream://%d", sid)
			conns[2] = conn
		}
	}
	return sio, conns, nil
}

type outputTarget struct {
	name   string
	stream io.ReadWriteCloser
	label  string
}

// fifoKeepalives holds the read-side FIFO FDs for keepalive purposes.
type fifoKeepalives struct {
	stdout io.Closer
	stderr io.Closer
}

func copyStreams(ctx context.Context, streams [3]io.ReadWriteCloser, stdin, stdout, stderr string, done chan struct{}) (fifoKeepalives, error) {
	var cwg sync.WaitGroup
	var copying atomic.Int32
	copying.Store(2)
	var sameFile *countingWriteCloser
	var keepalives fifoKeepalives

	outputs := []outputTarget{
		{name: stdout, stream: streams[1], label: "stdout"},
		{name: stderr, stream: streams[2], label: "stderr"},
	}

	for i, target := range outputs {
		if target.name == "" {
			if copying.Add(-1) == 0 {
				close(done)
			}
			continue
		}
		fw, fr, err := openOutputDestination(ctx, target.name, stdout, stderr, &sameFile)
		if err != nil {
			return keepalives, err
		}
		// Store keepalive closers - these will be closed by the directForwarder.Shutdown()
		if i == 0 {
			keepalives.stdout = fr
		} else {
			keepalives.stderr = fr
		}
		startOutputCopy(ctx, &cwg, &copying, done, target, fw)
	}

	if err := startStdinCopy(ctx, &cwg, streams[0], stdin); err != nil {
		return keepalives, err
	}

	cwg.Wait()
	return keepalives, nil
}

func openOutputDestination(ctx context.Context, name, stdout, stderr string, sameFile **countingWriteCloser) (io.WriteCloser, io.Closer, error) {
	ok, err := fifo.IsFifo(name)
	if err != nil {
		return nil, nil, err
	}
	if ok {
		fw, err := fifo.OpenFifo(ctx, name, syscall.O_WRONLY, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("containerd-shim: opening w/o fifo %q failed: %w", name, err)
		}
		fr, err := fifo.OpenFifo(ctx, name, syscall.O_RDONLY, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("containerd-shim: opening r/o fifo %q failed: %w", name, err)
		}
		return fw, fr, nil
	}

	if *sameFile != nil {
		(*sameFile).bumpCount(1)
		return *sameFile, nil, nil
	}

	// Ensure parent directory exists before opening file
	if err := os.MkdirAll(filepath.Dir(name), 0750); err != nil {
		return nil, nil, fmt.Errorf("containerd-shim: creating parent directory for %q failed: %w", name, err)
	}

	log.G(ctx).WithField("file", name).Debug("openOutputDestination: opening file for writing")
	fw, err := os.OpenFile(name, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return nil, nil, fmt.Errorf("containerd-shim: opening file %q failed: %w", name, err)
	}
	log.G(ctx).WithField("file", name).Debug("openOutputDestination: successfully opened file")
	if stdout == stderr {
		*sameFile = newCountingWriteCloser(fw, 1)
		return *sameFile, nil, nil
	}
	return fw, nil, nil
}

func startOutputCopy(ctx context.Context, cwg *sync.WaitGroup, copying *atomic.Int32, done chan struct{}, target outputTarget, wc io.WriteCloser) {
	cwg.Add(1)
	go func() {
		cwg.Done()
		log.G(ctx).WithFields(log.Fields{
			"target": target.name,
			"label":  target.label,
		}).Debug("startOutputCopy: starting to copy stream data")
		p := iobuf.Get()
		defer iobuf.Put(p)
		n, err := io.CopyBuffer(wc, target.stream, *p)
		if err != nil {
			log.G(ctx).WithError(err).WithFields(log.Fields{
				"stream": target.stream,
				"bytes":  n,
			}).Warn("error copying " + target.label)
		} else {
			log.G(ctx).WithFields(log.Fields{
				"target": target.name,
				"label":  target.label,
				"bytes":  n,
			}).Debug("startOutputCopy: finished copying stream data")
		}
		if copying.Add(-1) == 0 {
			close(done)
		}
		if err := wc.Close(); err != nil {
			log.G(ctx).WithError(err).WithField("stream", target.stream).Warn("error closing output writer")
		}
		// Note: keepalive closer (rc) is now stored in directForwarder and closed in Shutdown()
	}()
}

func startStdinCopy(ctx context.Context, cwg *sync.WaitGroup, stream io.ReadWriteCloser, stdin string) error {
	if stdin == "" {
		log.G(ctx).Debug("startStdinCopy: stdin is empty, skipping")
		return nil
	}
	log.G(ctx).WithField("stdin", stdin).Debug("startStdinCopy: opening stdin FIFO")
	// Open FIFO with background context - it needs to stay open for the lifetime of I/O forwarding,
	// not tied to any specific operation context. Using the RPC context would cause the FIFO to
	// close when the Create RPC completes, breaking stdin for later attach operations.
	f, err := fifo.OpenFifo(context.WithoutCancel(ctx), stdin, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("containerd-shim: opening %s failed: %w", stdin, err)
	}
	log.G(ctx).WithField("stdin", stdin).Debug("startStdinCopy: stdin FIFO opened, starting copy goroutine")
	cwg.Add(1)
	go func() {
		cwg.Done()
		log.G(ctx).Debug("startStdinCopy: copy goroutine started")
		defer func() {
			if err := stream.Close(); err != nil {
				if !isAlreadyClosedError(err) {
					log.G(ctx).WithError(err).Warn("error closing stdin stream")
				}
			}
			if err := f.Close(); err != nil {
				if !isAlreadyClosedError(err) {
					log.G(ctx).WithError(err).Warn("error closing stdin fifo")
				}
			}
		}()

		p := iobuf.Get()
		defer iobuf.Put(p)
		buf := *p

		var totalBytes int64
		pollInterval := 50 * time.Millisecond
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()

		for {
			// Read from FIFO - with O_NONBLOCK this returns immediately
			// If no writer is connected, we get 0 bytes (EOF-like behavior)
			n, err := f.Read(buf)
			if n > 0 {
				totalBytes += int64(n)
				// Write to stream (vsock to guest)
				if _, werr := stream.Write(buf[:n]); werr != nil {
					if !isClosedConnError(werr) {
						log.G(ctx).WithError(werr).Warn("error writing to stdin stream")
					}
					log.G(ctx).WithField("bytes", totalBytes).Debug("startStdinCopy: copy finished (stream write error)")
					return
				}
				// Got data, continue reading immediately
				continue
			}

			if err != nil && !errors.Is(err, io.EOF) && !isNoDataError(err) {
				// Real error (not just "no data available")
				if !isClosedConnError(err) {
					log.G(ctx).WithError(err).Warn("error reading stdin fifo")
				}
				log.G(ctx).WithField("bytes", totalBytes).Debug("startStdinCopy: copy finished (read error)")
				return
			}

			// No data available (n == 0 or EOF from non-blocking read)
			// Wait before polling again
			<-ticker.C
		}
	}()
	return nil
}

// isNoDataError checks for non-blocking read errors that indicate no data is available.
func isNoDataError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.EAGAIN) ||
		errors.Is(err, syscall.EWOULDBLOCK) ||
		errors.Is(err, syscall.EINTR)
}

// isClosedConnError checks if the error is a closed network connection error.
// This is expected during normal shutdown when the vsock connection closes.
func isClosedConnError(err error) bool {
	if err == nil {
		return false
	}
	// Check for typed errors first (Go 1.16+)
	var netErr *net.OpError
	if errors.As(err, &netErr) && errors.Is(netErr.Err, net.ErrClosed) {
		return true
	}
	// Fallback to string matching for vsock-specific errors or older Go versions
	msg := err.Error()
	return msg == "use of closed network connection" ||
		msg == "read: connection reset by peer"
}

// isAlreadyClosedError checks if the error is an "already closed" error.
// This is expected during normal shutdown when multiple goroutines try to close the same resource.
func isAlreadyClosedError(err error) bool {
	if err == nil {
		return false
	}
	// Check for fs.ErrClosed (Go 1.16+)
	if errors.Is(err, fs.ErrClosed) {
		return true
	}
	// Fallback to string matching for compatibility
	errStr := err.Error()
	return errStr == "file already closed" ||
		errStr == "close of closed file" ||
		errStr == "close of closed network connection"
}

// countingWriteCloser masks io.Closer() until close has been invoked a certain number of times.
type countingWriteCloser struct {
	io.WriteCloser
	count atomic.Int64
}

func newCountingWriteCloser(c io.WriteCloser, count int64) *countingWriteCloser {
	cwc := &countingWriteCloser{
		c,
		atomic.Int64{},
	}
	cwc.bumpCount(count)
	return cwc
}

func (c *countingWriteCloser) bumpCount(delta int64) int64 {
	return c.count.Add(delta)
}

func (c *countingWriteCloser) Close() error {
	if c.bumpCount(-1) > 0 {
		return nil
	}
	return c.WriteCloser.Close()
}
