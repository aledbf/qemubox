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

	usePIOPaths := false
	switch u.Scheme {
	case "stream":
		// Pass through
		return forwardIOSetup{pio: pio, passthrough: true}, nil
	case "fifo", "binary", "pipe":
		// Handled below via createStreams
	case "file":
		filePath := u.Path
		log.G(ctx).WithField("filePath", filePath).Debug("file scheme: using file path for logging")

		// Validate parent directory can be created
		if err := os.MkdirAll(filepath.Dir(filePath), 0750); err != nil {
			return forwardIOSetup{}, fmt.Errorf("failed to create parent directory: %w", err)
		}

		// createStreams will replace pio.Stdout/Stderr with stream:// URIs
		// These stream URIs will be sent to the VM
		pio, streams, err := createStreams(ctx, vmi, pio)
		if err != nil {
			return forwardIOSetup{}, err
		}

		log.G(ctx).WithFields(log.Fields{
			"stdout":         pio.Stdout,
			"stderr":         pio.Stderr,
			"stdoutFilePath": filePath,
		}).Debug("file scheme: created streams, will copy to file on host")

		// Save file paths separately - these will be used on the host side in copyStreams
		// The pio.Stdout/Stderr contain stream:// URIs which will be sent to the VM
		return forwardIOSetup{
			pio:            pio,
			streams:        streams,
			usePIOPaths:    true,
			stdoutFilePath: filePath,
			stderrFilePath: filePath,
		}, nil
	default:
		return forwardIOSetup{}, fmt.Errorf("unsupported STDIO scheme %s: %w", u.Scheme, errdefs.ErrNotImplemented)
	}

	pio, streams, err := createStreams(ctx, vmi, pio)
	if err != nil {
		return forwardIOSetup{}, err
	}
	return forwardIOSetup{pio: pio, streams: streams, usePIOPaths: usePIOPaths}, nil
}

func (s *service) forwardIO(ctx context.Context, vmi vm.Instance, sio stdio.Stdio) (stdio.Stdio, func(ctx context.Context) error, error) {
	// When using a terminal, stderr is not used (it's merged into stdout/pty)
	if sio.Terminal {
		sio.Stderr = ""
	}
	pio := sio
	if pio.IsNull() {
		return pio, nil, nil
	}

	setup, err := setupForwardIO(ctx, vmi, pio)
	if err != nil {
		return stdio.Stdio{}, nil, err
	}
	if setup.passthrough {
		return setup.pio, nil, nil
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
	if err = copyStreams(ctx, streams, stdinPath, stdoutPath, stderrPath, ioDone); err != nil {
		return stdio.Stdio{}, nil, err
	}
	return pio, func(ctx context.Context) error {
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

func copyStreams(ctx context.Context, streams [3]io.ReadWriteCloser, stdin, stdout, stderr string, done chan struct{}) error {
	var cwg sync.WaitGroup
	var copying atomic.Int32
	copying.Store(2)
	var sameFile *countingWriteCloser

	outputs := []outputTarget{
		{name: stdout, stream: streams[1], label: "stdout"},
		{name: stderr, stream: streams[2], label: "stderr"},
	}

	for _, target := range outputs {
		if target.name == "" {
			if copying.Add(-1) == 0 {
				close(done)
			}
			continue
		}
		fw, fr, err := openOutputDestination(ctx, target.name, stdout, stderr, &sameFile)
		if err != nil {
			return err
		}
		startOutputCopy(ctx, &cwg, &copying, done, target, fw, fr)
	}

	if err := startStdinCopy(ctx, &cwg, streams[0], stdin); err != nil {
		return err
	}

	cwg.Wait()
	return nil
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

func startOutputCopy(ctx context.Context, cwg *sync.WaitGroup, copying *atomic.Int32, done chan struct{}, target outputTarget, wc io.WriteCloser, rc io.Closer) {
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
		if rc != nil {
			if err := rc.Close(); err != nil {
				log.G(ctx).WithError(err).WithField("stream", target.stream).Warn("error closing output reader")
			}
		}
	}()
}

func startStdinCopy(ctx context.Context, cwg *sync.WaitGroup, stream io.ReadWriteCloser, stdin string) error {
	if stdin == "" {
		return nil
	}
	// Open FIFO with background context - it needs to stay open for the lifetime of I/O forwarding,
	// not tied to any specific operation context.
	f, err := fifo.OpenFifo(ctx, stdin, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("containerd-shim: opening %s failed: %w", stdin, err)
	}
	cwg.Add(1)
	go func() {
		cwg.Done()
		p := iobuf.Get()
		defer iobuf.Put(p)

		if _, err := io.CopyBuffer(stream, f, *p); err != nil {
			// Ignore "use of closed network connection" - expected during shutdown
			if !isClosedConnError(err) {
				log.G(ctx).WithError(err).Warn("error copying stdin")
			}
		}
		if err := stream.Close(); err != nil {
			// Ignore "file already closed" - benign race during shutdown
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
	return nil
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
