package task

import (
	"context"
	"fmt"
	"io"
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

	"github.com/aledbf/beacon/containerd/iobuf"
)

const (
	defaultScheme = "fifo"
)

type streamCreator interface {
	StartStream(ctx context.Context) (uint32, net.Conn, error)
}

type forwardIOSetup struct {
	pio         stdio.Stdio
	streams     [3]io.ReadWriteCloser
	passthrough bool
}

func setupForwardIO(ctx context.Context, ss streamCreator, pio stdio.Stdio) (forwardIOSetup, error) {
	u, err := url.Parse(pio.Stdout)
	if err != nil {
		return forwardIOSetup{}, fmt.Errorf("unable to parse stdout uri: %w", err)
	}
	if u.Scheme == "" {
		u.Scheme = defaultScheme
	}

	switch u.Scheme {
	case "stream":
		// Pass through
		return forwardIOSetup{pio: pio, passthrough: true}, nil
	case "fifo", "binary", "pipe":
		// Handled below via createStreams
	case "file":
		filePath := u.Path
		if err := os.MkdirAll(filepath.Dir(filePath), 0750); err != nil {
			return forwardIOSetup{}, err
		}
		f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return forwardIOSetup{}, err
		}
		if err := f.Close(); err != nil {
			return forwardIOSetup{}, err
		}
		pio.Stdout = filePath
		pio.Stderr = filePath
	default:
		return forwardIOSetup{}, fmt.Errorf("unsupported STDIO scheme %s: %w", u.Scheme, errdefs.ErrNotImplemented)
	}

	pio, streams, err := createStreams(ctx, ss, pio)
	if err != nil {
		return forwardIOSetup{}, err
	}
	return forwardIOSetup{pio: pio, streams: streams}, nil
}

func (s *service) forwardIO(ctx context.Context, ss streamCreator, sio stdio.Stdio) (stdio.Stdio, func(ctx context.Context) error, error) {
	// When using a terminal, stderr is not used (it's merged into stdout/pty)
	if sio.Terminal {
		sio.Stderr = ""
	}
	pio := sio
	if pio.IsNull() {
		return pio, nil, nil
	}

	setup, err := setupForwardIO(ctx, ss, pio)
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
	if err = copyStreams(ctx, streams, sio.Stdin, sio.Stdout, sio.Stderr, ioDone); err != nil {
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

func createStreams(ctx context.Context, ss streamCreator, sio stdio.Stdio) (stdio.Stdio, [3]io.ReadWriteCloser, error) {
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
		sid, conn, err := ss.StartStream(ctx)
		if err != nil {
			retErr = fmt.Errorf("failed to start fifo stream: %w", err)
			return sio, conns, retErr
		}
		sio.Stdin = fmt.Sprintf("stream://%d", sid)
		conns[0] = conn
	}

	stdout := sio.Stdout
	if stdout != "" {
		sid, conn, err := ss.StartStream(ctx)
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
			sid, conn, err := ss.StartStream(ctx)
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

	fw, err := os.OpenFile(name, syscall.O_WRONLY|syscall.O_APPEND, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("containerd-shim: opening file %q failed: %w", name, err)
	}
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
		p := iobuf.Get()
		defer iobuf.Put(p)
		if _, err := io.CopyBuffer(wc, target.stream, *p); err != nil {
			log.G(ctx).WithError(err).WithField("stream", target.stream).Warn("error copying " + target.label)
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
			log.G(ctx).WithError(err).Warn("error copying stdin")
		}
		if err := stream.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("error closing stdin stream")
		}
		if err := f.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("error closing stdin fifo")
		}
	}()
	return nil
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
