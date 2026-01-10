//go:build linux

// Package runc provides container lifecycle helpers for vminit.
package runc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/containerd/console"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/fifo"
	"github.com/containerd/log"

	"github.com/spin-stack/spinbox/internal/guest/vminit/process"
	"github.com/spin-stack/spinbox/internal/guest/vminit/stream"
	"github.com/spin-stack/spinbox/internal/iobuf"
)

// NewPlatform returns a linux platform for use with I/O operations
func NewPlatform(m stream.Manager) (stdio.Platform, error) {
	epoller, err := console.NewEpoller()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize epoller: %w", err)
	}
	go func() {
		if err := epoller.Wait(); err != nil {
			log.L.WithError(err).Error("epoller wait failed")
		}
	}()
	return &linuxPlatform{
		epoller: epoller,
		streams: m,
	}, nil
}

type linuxPlatform struct {
	epoller *console.Epoller
	streams stream.Manager
}

func (p *linuxPlatform) CopyConsole(ctx context.Context, cons console.Console, id, stdin, stdout, stderr string, wg *sync.WaitGroup) (console.Console, error) {
	if p.epoller == nil {
		return nil, errors.New("uninitialized epoller")
	}

	epollConsole, err := p.epoller.Add(cons)
	if err != nil {
		return nil, err
	}

	var cwg sync.WaitGroup
	cstdin, err := p.setupStdinCopy(ctx, stdin, epollConsole, &cwg)
	if err != nil {
		return nil, err
	}

	uri, err := url.Parse(stdout)
	if err != nil {
		return nil, fmt.Errorf("unable to parse stdout uri: %w", err)
	}

	switch uri.Scheme {
	case "stream":
		if err := p.copyToStream(stdout, epollConsole, wg, &cwg); err != nil {
			return nil, err
		}
	case "binary":
		if err := p.copyToBinary(ctx, uri, id, epollConsole, wg, &cwg); err != nil {
			return nil, err
		}
	default:
		if err := p.copyToFifo(ctx, stdout, epollConsole, wg, &cwg); err != nil {
			return nil, err
		}
	}

	var result console.Console
	if cstdin != nil {
		result = &closingConsole{
			EpollConsole: epollConsole,
			closeStdin:   cstdin,
		}
	} else {
		result = epollConsole
	}

	return result, nil
}

// setupStdinCopy sets up stdin copying if stdin is specified.
// Returns the stdin closer (may be nil) and any error.
func (p *linuxPlatform) setupStdinCopy(ctx context.Context, stdin string, epollConsole *console.EpollConsole, cwg *sync.WaitGroup) (io.Closer, error) {
	if stdin == "" {
		return nil, nil
	}

	var in io.ReadCloser
	var err error

	if s, ok := strings.CutPrefix(stdin, "stream://"); ok {
		sid, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return nil, err
		}
		in, err = p.streams.Get(uint32(sid))
		if err != nil {
			return nil, err
		}
	} else {
		in, err = fifo.OpenFifo(ctx, stdin, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
	}

	cwg.Add(1)
	go func() {
		cwg.Done()
		bp := iobuf.Get()
		defer iobuf.Put(bp)
		n, err := io.CopyBuffer(epollConsole, in, *bp)
		log.L.WithFields(log.Fields{
			"bytes": n,
			"error": err,
		}).Debug("console stdin copy finished")
		if err != nil {
			log.L.WithError(err).Debug("console stdin copy error")
		}
		if err := in.Close(); err != nil {
			log.L.WithError(err).Debug("console stdin close error")
		}
	}()

	return in, nil
}

// copyToStream copies console output to a stream:// URI.
func (p *linuxPlatform) copyToStream(stdout string, epollConsole *console.EpollConsole, wg, cwg *sync.WaitGroup) error {
	sid, err := strconv.ParseUint(strings.TrimPrefix(stdout, "stream://"), 10, 32)
	if err != nil {
		return err
	}
	out, err := p.streams.Get(uint32(sid))
	if err != nil {
		return err
	}
	wg.Add(1)
	cwg.Add(1)
	go func() {
		cwg.Done()
		buf := iobuf.Get()
		defer iobuf.Put(buf)
		if _, err := io.CopyBuffer(out, epollConsole, *buf); err != nil {
			log.L.WithError(err).Debug("console copy error")
		}
		if err := out.Close(); err != nil {
			log.L.WithError(err).Debug("console close error")
		}
		wg.Done()
	}()
	cwg.Wait()
	return nil
}

// copyToBinary copies console output to a binary:// URI logging binary.
func (p *linuxPlatform) copyToBinary(ctx context.Context, uri *url.URL, id string, epollConsole *console.EpollConsole, wg, cwg *sync.WaitGroup) error {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	cmd := process.NewBinaryCmd(ctx, uri, id, ns)

	// In case of unexpected errors during logging binary start, close open pipes
	var filesToClose []*os.File
	success := false
	defer func() {
		if !success {
			process.CloseFiles(filesToClose...)
		}
	}()

	// Create pipe to be used by logging binary for Stdout
	outR, outW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipes: %w", err)
	}
	filesToClose = append(filesToClose, outR)

	// Stderr is created for logging binary but unused when terminal is true
	serrR, _, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipes: %w", err)
	}
	filesToClose = append(filesToClose, serrR)

	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	filesToClose = append(filesToClose, r)

	cmd.ExtraFiles = append(cmd.ExtraFiles, outR, serrR, w)

	wg.Add(1)
	cwg.Add(1)
	go func() {
		cwg.Done()
		if _, err := io.Copy(outW, epollConsole); err != nil {
			log.L.WithError(err).Debug("console copy error")
		}
		if err := outW.Close(); err != nil {
			log.L.WithError(err).Debug("console close error")
		}
		wg.Done()
	}()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start logging binary process: %w", err)
	}

	// Close our side of the pipe after start
	if err := w.Close(); err != nil {
		return fmt.Errorf("failed to close write pipe after start: %w", err)
	}

	// Wait for the logging binary to be ready
	b := make([]byte, 1)
	if _, err := r.Read(b); err != nil && err != io.EOF {
		return fmt.Errorf("failed to read from logging binary: %w", err)
	}
	cwg.Wait()
	success = true

	return nil
}

// copyToFifo copies console output to a FIFO file.
func (p *linuxPlatform) copyToFifo(ctx context.Context, stdout string, epollConsole *console.EpollConsole, wg, cwg *sync.WaitGroup) error {
	outw, err := fifo.OpenFifo(ctx, stdout, syscall.O_WRONLY, 0)
	if err != nil {
		return err
	}
	outr, err := fifo.OpenFifo(ctx, stdout, syscall.O_RDONLY, 0)
	if err != nil {
		return err
	}
	wg.Add(1)
	cwg.Add(1)
	go func() {
		cwg.Done()
		buf := iobuf.Get()
		defer iobuf.Put(buf)
		if _, err := io.CopyBuffer(outw, epollConsole, *buf); err != nil {
			log.L.WithError(err).Debug("console copy error")
		}

		if err := outw.Close(); err != nil {
			log.L.WithError(err).Debug("console close error")
		}
		if err := outr.Close(); err != nil {
			log.L.WithError(err).Debug("console close error")
		}
		wg.Done()
	}()
	cwg.Wait()
	return nil
}

func (p *linuxPlatform) ShutdownConsole(ctx context.Context, cons console.Console) error {
	if p.epoller == nil {
		return errors.New("uninitialized epoller")
	}
	epollConsole, ok := cons.(interface {
		Shutdown(closeConsole func(int) error) error
	})
	if !ok {
		return fmt.Errorf("expected EpollConsole, got %#v", cons)
	}
	return epollConsole.Shutdown(p.epoller.CloseConsole)
}

func (p *linuxPlatform) Close() error {
	return p.epoller.Close()
}

type closingConsole struct {
	*console.EpollConsole

	closeStdin io.Closer
}

func (c *closingConsole) StdinCloser() io.Closer {
	return c.closeStdin
}
