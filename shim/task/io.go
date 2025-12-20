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

func (s *service) forwardIO(ctx context.Context, ss streamCreator, sio stdio.Stdio) (stdio.Stdio, func(ctx context.Context) error, error) {
	// When using a terminal, stderr is not used (it's merged into stdout/pty)
	if sio.Terminal {
		sio.Stderr = ""
	}
	pio := sio
	if pio.IsNull() {
		return pio, nil, nil
	}

	u, err := url.Parse(pio.Stdout)
	if err != nil {
		return stdio.Stdio{}, nil, fmt.Errorf("unable to parse stdout uri: %w", err)
	}
	if u.Scheme == "" {
		u.Scheme = defaultScheme
	}
	var streams [3]io.ReadWriteCloser
	switch u.Scheme {
	case "stream":
		// Pass through
		return pio, nil, nil
	case "fifo":
		pio, streams, err = createStreams(ctx, ss, pio)
		if err != nil {
			return stdio.Stdio{}, nil, err
		}
	case "file":
		filePath := u.Path
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			return stdio.Stdio{}, nil, err
		}
		var f *os.File
		f, err = os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return stdio.Stdio{}, nil, err
		}
		if err := f.Close(); err != nil {
			return stdio.Stdio{}, nil, err
		}
		pio.Stdout = filePath
		pio.Stderr = filePath
		pio, streams, err = createStreams(ctx, ss, pio)
		if err != nil {
			return stdio.Stdio{}, nil, err
		}
	case "binary":
		// Binary scheme spawns a custom logging binary process
		// The binary receives stdout/stderr via file descriptors
		// URI format: binary:///path/to/binary?arg=value
		pio, streams, err = createStreams(ctx, ss, pio)
		if err != nil {
			return stdio.Stdio{}, nil, err
		}
	case "pipe":
		// Pipe scheme uses OS pipes directly for I/O
		// This is similar to fifo but uses anonymous pipes instead of named pipes
		pio, streams, err = createStreams(ctx, ss, pio)
		if err != nil {
			return stdio.Stdio{}, nil, err
		}
	default:
		return stdio.Stdio{}, nil, fmt.Errorf("unsupported STDIO scheme %s: %w", u.Scheme, errdefs.ErrNotImplemented)
	}
	if err != nil {
		return stdio.Stdio{}, nil, err
	}

	defer func() {
		if err != nil {
			for i, c := range streams {
				if c != nil && (i != 2 || c != streams[1]) {
					c.Close()
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
				c.Close()
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

func createStreams(ctx context.Context, ss streamCreator, io stdio.Stdio) (_ stdio.Stdio, conns [3]io.ReadWriteCloser, err error) {
	defer func() {
		if err != nil {
			for i, c := range conns {
				if c != nil && (i != 2 || c != conns[1]) {
					c.Close()
				}
			}
		}
	}()
	if io.Stdin != "" {
		sid, conn, err := ss.StartStream(ctx)
		if err != nil {
			return io, conns, fmt.Errorf("failed to start fifo stream: %w", err)
		}
		io.Stdin = fmt.Sprintf("stream://%d", sid)
		conns[0] = conn
	}

	stdout := io.Stdout
	if stdout != "" {
		sid, conn, err := ss.StartStream(ctx)
		if err != nil {
			return io, conns, fmt.Errorf("failed to start fifo stream: %w", err)
		}
		io.Stdout = fmt.Sprintf("stream://%d", sid)
		conns[1] = conn
	}

	if io.Stderr != "" {
		if io.Stderr == stdout {
			io.Stderr = io.Stdout
			conns[2] = conns[1]
		} else {
			sid, conn, err := ss.StartStream(ctx)
			if err != nil {
				return io, conns, fmt.Errorf("failed to start fifo stream: %w", err)
			}
			io.Stderr = fmt.Sprintf("stream://%d", sid)
			conns[2] = conn
		}
	}
	return io, conns, nil
}

func copyStreams(ctx context.Context, streams [3]io.ReadWriteCloser, stdin, stdout, stderr string, done chan struct{}) error {
	var cwg sync.WaitGroup
	var copying atomic.Int32
	copying.Store(2)
	var sameFile *countingWriteCloser
	for _, i := range []struct {
		name string
		dest func(wc io.WriteCloser, rc io.Closer)
	}{
		{
			name: stdout,
			dest: func(wc io.WriteCloser, rc io.Closer) {
				cwg.Add(1)
				go func() {
					cwg.Done()
					p := iobuf.Pool.Get().(*[]byte)
					defer iobuf.Pool.Put(p)
					if _, err := io.CopyBuffer(wc, streams[1], *p); err != nil {
						log.G(ctx).WithError(err).WithField("stream", streams[1]).Warn("error copying stdout")
					}
					if copying.Add(-1) == 0 {
						close(done)
					}
					wc.Close()
					if rc != nil {
						rc.Close()
					}
				}()
			},
		}, {
			name: stderr,
			dest: func(wc io.WriteCloser, rc io.Closer) {
				cwg.Add(1)
				go func() {
					cwg.Done()
					p := iobuf.Pool.Get().(*[]byte)
					defer iobuf.Pool.Put(p)
					if _, err := io.CopyBuffer(wc, streams[2], *p); err != nil {
						log.G(ctx).WithError(err).Warn("error copying stderr")
					}
					if copying.Add(-1) == 0 {
						close(done)
					}
					wc.Close()
					if rc != nil {
						rc.Close()
					}
				}()
			},
		},
	} {
		if i.name == "" {
			if copying.Add(-1) == 0 {
				close(done)
			}
			continue
		}
		ok, err := fifo.IsFifo(i.name)
		if err != nil {
			return err
		}
		var (
			fw io.WriteCloser
			fr io.Closer
		)
		if ok {
			if fw, err = fifo.OpenFifo(ctx, i.name, syscall.O_WRONLY, 0); err != nil {
				return fmt.Errorf("containerd-shim: opening w/o fifo %q failed: %w", i.name, err)
			}
			if fr, err = fifo.OpenFifo(ctx, i.name, syscall.O_RDONLY, 0); err != nil {
				return fmt.Errorf("containerd-shim: opening r/o fifo %q failed: %w", i.name, err)
			}
		} else {
			if sameFile != nil {
				sameFile.bumpCount(1)
				i.dest(sameFile, nil)
				continue
			}
			if fw, err = os.OpenFile(i.name, syscall.O_WRONLY|syscall.O_APPEND, 0); err != nil {
				return fmt.Errorf("containerd-shim: opening file %q failed: %w", i.name, err)
			}
			if stdout == stderr {
				sameFile = newCountingWriteCloser(fw, 1)
			}
		}
		i.dest(fw, fr)
	}
	if stdin != "" {
		// Open FIFO with background context - it needs to stay open for the lifetime of I/O forwarding,
		// not tied to any specific operation context.
		f, err := fifo.OpenFifo(context.Background(), stdin, syscall.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:contextcheck // I/O FIFO needs independent lifetime
		if err != nil {
			return fmt.Errorf("containerd-shim: opening %s failed: %s", stdin, err)
		}
		cwg.Add(1)
		go func() {
			cwg.Done()
			p := iobuf.Pool.Get().(*[]byte)
			defer iobuf.Pool.Put(p)

			io.CopyBuffer(streams[0], f, *p)
			streams[0].Close()
			f.Close()
		}()
	}
	cwg.Wait()
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
