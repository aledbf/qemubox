//go:build !windows

package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	"github.com/containerd/fifo"
	runc "github.com/containerd/go-runc"
	"github.com/containerd/log"
	"golang.org/x/sys/unix"

	"github.com/spin-stack/spinbox/internal/guest/vminit/stream"
	"github.com/spin-stack/spinbox/internal/iobuf"
)

const binaryIOProcTermTimeout = 12 * time.Second // Give logger process solid 10 seconds for cleanup

type processIO struct {
	io runc.IO

	uri   *url.URL
	copy  bool
	stdio stdio.Stdio

	streams [3]io.ReadWriteCloser
}

func (p *processIO) Close() error {
	if p.io != nil {
		return p.io.Close()
	}
	for i, s := range p.streams {
		if s != nil && (i != 2 || s != p.streams[1]) {
			_ = s.Close()
		}
	}
	return nil
}

func (p *processIO) IO() runc.IO {
	return p.io
}

func (p *processIO) Copy(ctx context.Context, wg *sync.WaitGroup) (io.Closer, error) {
	if !p.copy {
		var c io.Closer
		if p.stdio.Stdin != "" {
			var err error
			// Open FIFO with background context - it needs to stay open for the lifetime of I/O forwarding,
			// not tied to any specific operation context.
			c, err = fifo.OpenFifo(context.Background(), p.stdio.Stdin, unix.O_WRONLY|unix.O_NONBLOCK, 0) //nolint:contextcheck // I/O FIFO needs independent lifetime
			if err != nil {
				return nil, fmt.Errorf("failed to open stdin fifo %s: %w", p.stdio.Stdin, err)
			}
		}
		return c, nil
	}
	var cwg sync.WaitGroup
	c, err := copyPipes(ctx, p.IO(), p.stdio.Stdin, p.stdio.Stdout, p.stdio.Stderr, p.streams, wg, &cwg)
	if err != nil {
		return nil, fmt.Errorf("unable to copy pipes: %w", err)
	}

	cwg.Wait()
	return c, nil
}

// ioConfig holds configuration for creating process I/O.
type ioConfig struct {
	id      string
	ioUID   int
	ioGID   int
	stdio   stdio.Stdio
	streams stream.Manager
}

// ioFactory creates I/O for a specific scheme.
// ctx is passed separately to avoid storing context in a struct.
type ioFactory func(ctx context.Context, cfg ioConfig, u *url.URL, pio *processIO) error

// ioFactories maps scheme names to their factory functions.
// Each factory is responsible for setting up pio.io, pio.copy, and pio.streams.
var ioFactories = map[string]ioFactory{
	"stream": createStreamIO,
	"fifo":   createFifoIO,
	"binary": createBinaryIO,
	"file":   createFileIO,
}

func createStreamIO(ctx context.Context, cfg ioConfig, u *url.URL, pio *processIO) error {
	streams, err := getStreams(cfg.stdio, cfg.streams)
	if err != nil {
		return err
	}
	log.G(ctx).WithField("id", cfg.id).WithField("streams", streams).Debug("using stream-based I/O")
	pio.streams = streams
	pio.copy = true
	pio.io, err = runc.NewPipeIO(cfg.ioUID, cfg.ioGID, withConditionalIO(cfg.stdio))
	return err
}

func createFifoIO(_ context.Context, cfg ioConfig, _ *url.URL, pio *processIO) error {
	pio.copy = true
	var err error
	pio.io, err = runc.NewPipeIO(cfg.ioUID, cfg.ioGID, withConditionalIO(cfg.stdio))
	return err
}

func createBinaryIO(ctx context.Context, cfg ioConfig, u *url.URL, pio *processIO) error {
	var err error
	pio.io, err = NewBinaryIO(ctx, cfg.id, u)
	return err
}

func createFileIO(_ context.Context, cfg ioConfig, u *url.URL, pio *processIO) error {
	filePath := u.Path
	if err := os.MkdirAll(filepath.Dir(filePath), 0750); err != nil {
		return err
	}
	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	pio.stdio.Stdout = filePath
	pio.stdio.Stderr = filePath
	pio.copy = true
	pio.io, err = runc.NewPipeIO(cfg.ioUID, cfg.ioGID, withConditionalIO(cfg.stdio))
	return err
}

// createIO creates I/O for a process based on the stdio configuration.
// Supported schemes: null, stream, fifo (default), binary, file.
func createIO(ctx context.Context, id string, ioUID, ioGID int, stdio stdio.Stdio, ss stream.Manager) (*processIO, error) {
	pio := &processIO{
		stdio: stdio,
	}

	// Handle null I/O case
	if stdio.IsNull() {
		i, err := runc.NewNullIO()
		if err != nil {
			return nil, err
		}
		pio.io = i
		return pio, nil
	}

	// Parse stdout URI to determine scheme
	u, err := url.Parse(stdio.Stdout)
	if err != nil {
		return nil, fmt.Errorf("unable to parse stdout uri: %w", err)
	}
	if u.Scheme == "" {
		u.Scheme = "fifo" // Default scheme
	}
	pio.uri = u

	// Look up factory for this scheme
	factory, ok := ioFactories[u.Scheme]
	if !ok {
		return nil, fmt.Errorf("unsupported STDIO scheme %s: %w", u.Scheme, errdefs.ErrNotImplemented)
	}

	// Create I/O using the factory
	cfg := ioConfig{
		id:      id,
		ioUID:   ioUID,
		ioGID:   ioGID,
		stdio:   stdio,
		streams: ss,
	}
	if err := factory(ctx, cfg, u, pio); err != nil {
		return nil, err
	}

	return pio, nil
}

type pipeOutput struct {
	name  string
	index int
	label string
}

func copyPipes(ctx context.Context, rio runc.IO, stdin, stdout, stderr string, streams [3]io.ReadWriteCloser, wg, cwg *sync.WaitGroup) (io.Closer, error) {
	var sameFile *countingWriteCloser
	outputs := []pipeOutput{
		{name: stdout, index: 1, label: "stdout"},
		{name: stderr, index: 2, label: "stderr"},
	}

	for _, out := range outputs {
		if out.name == "" {
			continue
		}
		fw, fr, err := openPipeOutput(ctx, out, stdout, stderr, streams, &sameFile)
		if err != nil {
			return nil, err
		}
		startPipeCopy(ctx, rio, out, fw, fr, wg, cwg)
	}

	return startPipeStdin(ctx, rio, stdin, streams, cwg)
}

func openPipeOutput(ctx context.Context, out pipeOutput, stdout, stderr string, streams [3]io.ReadWriteCloser, sameFile **countingWriteCloser) (io.WriteCloser, io.Closer, error) {
	if streams[out.index] != nil {
		return streams[out.index], nil, nil
	}

	ok, err := fifo.IsFifo(out.name)
	if err != nil {
		return nil, nil, err
	}
	if ok {
		fw, err := fifo.OpenFifo(ctx, out.name, syscall.O_WRONLY, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("containerd-shim: opening w/o fifo %q failed: %w", out.name, err)
		}
		fr, err := fifo.OpenFifo(ctx, out.name, syscall.O_RDONLY, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("containerd-shim: opening r/o fifo %q failed: %w", out.name, err)
		}
		return fw, fr, nil
	}

	if *sameFile != nil {
		(*sameFile).bumpCount(1)
		return *sameFile, nil, nil
	}

	fw, err := os.OpenFile(out.name, syscall.O_WRONLY|syscall.O_APPEND, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("containerd-shim: opening file %q failed: %w", out.name, err)
	}
	if stdout == stderr {
		*sameFile = newCountingWriteCloser(fw, 1)
		return *sameFile, nil, nil
	}
	return fw, nil, nil
}

func startPipeCopy(ctx context.Context, rio runc.IO, out pipeOutput, wc io.WriteCloser, rc io.Closer, wg, cwg *sync.WaitGroup) {
	wg.Add(1)
	cwg.Add(1)
	go func() {
		cwg.Done()
		p := iobuf.Get()
		defer iobuf.Put(p)

		var err error
		if out.index == 1 {
			_, err = io.CopyBuffer(wc, rio.Stdout(), *p)
		} else {
			_, err = io.CopyBuffer(wc, rio.Stderr(), *p)
		}
		if err != nil {
			log.G(ctx).WithError(err).WithField("stream", out.label).Warn("error copying output")
		}
		wg.Done()
		if err := wc.Close(); err != nil {
			log.G(ctx).WithError(err).WithField("stream", out.label).Warn("error closing output writer")
		}
		if rc != nil {
			if err := rc.Close(); err != nil {
				log.G(ctx).WithError(err).WithField("stream", out.label).Warn("error closing output reader")
			}
		}
	}()
}

func startPipeStdin(ctx context.Context, rio runc.IO, stdin string, streams [3]io.ReadWriteCloser, cwg *sync.WaitGroup) (io.Closer, error) {
	if stdin == "" {
		return nopCloser{}, nil
	}
	var (
		c   io.Closer
		f   io.ReadCloser
		err error
	)
	if streams[0] != nil {
		f = streams[0]
		c = streams[0]
	} else {
		f, err = fifo.OpenFifo(ctx, stdin, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, fmt.Errorf("containerd-shim: opening %s failed: %w", stdin, err)
		}
		c = f
	}
	cwg.Add(1)
	go func() {
		cwg.Done()
		p := iobuf.Get()
		defer iobuf.Put(p)

		if _, err := io.CopyBuffer(rio.Stdin(), f, *p); err != nil {
			// Ignore "use of closed network connection" - expected during shutdown
			if !isClosedConnError(err) {
				log.G(ctx).WithError(err).Warn("error copying stdin")
			}
		}
		if err := rio.Stdin().Close(); err != nil {
			// Ignore "file already closed" - benign race during shutdown
			if !isAlreadyClosedError(err) {
				log.G(ctx).WithError(err).Warn("error closing stdin")
			}
		}
		if err := f.Close(); err != nil {
			if !isAlreadyClosedError(err) {
				log.G(ctx).WithError(err).Warn("error closing stdin fifo")
			}
		}
	}()
	return c, nil
}

type nopCloser struct{}

func (nopCloser) Close() error {
	return nil
}

// isClosedConnError checks if the error is a closed network connection error.
// This is expected during normal shutdown when the vsock connection closes.
func isClosedConnError(err error) bool {
	if err == nil {
		return false
	}
	// Check for "use of closed network connection"
	return err.Error() == "use of closed network connection" ||
		err.Error() == "read: connection reset by peer"
}

// isAlreadyClosedError checks if the error is an "already closed" error.
// This is expected during normal shutdown when multiple goroutines try to close the same resource.
func isAlreadyClosedError(err error) bool {
	if err == nil {
		return false
	}
	// Check for "file already closed" or "close of closed"
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

// NewBinaryIO runs a custom binary process for pluggable shim logging
func NewBinaryIO(ctx context.Context, id string, uri *url.URL) (_ runc.IO, err error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	var closers []func() error
	defer func() {
		if err == nil {
			return
		}
		result := []error{err}
		for _, fn := range closers {
			result = append(result, fn())
		}
		err = errors.Join(result...)
	}()

	out, err := newPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipes: %w", err)
	}
	closers = append(closers, out.Close)

	serr, err := newPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr pipes: %w", err)
	}
	closers = append(closers, serr.Close)

	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	closers = append(closers, r.Close, w.Close)

	cmd := NewBinaryCmd(ctx, uri, id, ns)
	cmd.ExtraFiles = append(cmd.ExtraFiles, out.r, serr.r, w)
	// don't need to register this with the reaper or wait when
	// running inside a shim
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start binary process: %w", err)
	}
	closers = append(closers, func() error { return cmd.Process.Kill() })

	// close our side of the pipe after start
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("failed to close write pipe after start: %w", err)
	}

	// wait for the logging binary to be ready
	b := make([]byte, 1)
	if _, err := r.Read(b); err != nil && err != io.EOF {
		return nil, fmt.Errorf("failed to read from logging binary: %w", err)
	}

	return &binaryIO{
		cmd: cmd,
		out: out,
		err: serr,
	}, nil
}

type binaryIO struct {
	cmd      *exec.Cmd
	out, err *pipe
}

func (b *binaryIO) CloseAfterStart() error {
	var result []error

	for _, v := range []*pipe{b.out, b.err} {
		if v != nil {
			if err := v.r.Close(); err != nil {
				result = append(result, err)
			}
		}
	}

	return errors.Join(result...)
}

func (b *binaryIO) Close() error {
	var result []error

	for _, v := range []*pipe{b.out, b.err} {
		if v != nil {
			if err := v.Close(); err != nil {
				result = append(result, err)
			}
		}
	}

	if err := b.cancel(); err != nil {
		result = append(result, err)
	}

	return errors.Join(result...)
}

func (b *binaryIO) cancel() error {
	if b.cmd == nil || b.cmd.Process == nil {
		return nil
	}

	// Send SIGTERM first, so logger process has a chance to flush and exit properly
	if err := b.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		result := []error{fmt.Errorf("failed to send SIGTERM: %w", err)}

		log.L.WithError(err).Warn("failed to send SIGTERM signal, killing logging shim")

		if err := b.cmd.Process.Kill(); err != nil {
			result = append(result, fmt.Errorf("failed to kill process after faulty SIGTERM: %w", err))
		}

		return errors.Join(result...)
	}

	done := make(chan error, 1)
	go func() {
		done <- b.cmd.Wait()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(binaryIOProcTermTimeout):
		log.L.Warn("failed to wait for shim logger process to exit, killing")

		err := b.cmd.Process.Kill()
		if err != nil {
			return fmt.Errorf("failed to kill shim logger process: %w", err)
		}

		return nil
	}
}

func (b *binaryIO) Stdin() io.WriteCloser {
	return nil
}

func (b *binaryIO) Stdout() io.ReadCloser {
	return nil
}

func (b *binaryIO) Stderr() io.ReadCloser {
	return nil
}

func (b *binaryIO) Set(cmd *exec.Cmd) {
	if b.out != nil {
		cmd.Stdout = b.out.w
	}
	if b.err != nil {
		cmd.Stderr = b.err.w
	}
}

func newPipe() (*pipe, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	return &pipe{
		r: r,
		w: w,
	}, nil
}

type pipe struct {
	r *os.File
	w *os.File
}

func (p *pipe) Close() error {
	var result []error

	if err := p.w.Close(); err != nil {
		result = append(result, fmt.Errorf("pipe: failed to close write pipe: %w", err))
	}

	if err := p.r.Close(); err != nil {
		result = append(result, fmt.Errorf("pipe: failed to close read pipe: %w", err))
	}

	return errors.Join(result...)
}
