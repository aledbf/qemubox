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

	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/process"
	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/stream"
	"github.com/aledbf/qemubox/containerd/internal/iobuf"
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
	var cstdin io.Closer
	var result console.Console
	var cwg sync.WaitGroup
	if stdin != "" {
		var in io.ReadCloser
		if s, ok := strings.CutPrefix(stdin, "stream://"); ok {
			var sid uint64
			sid, err = strconv.ParseUint(s, 10, 32)
			if err != nil {
				return nil, err
			}
			in, err = p.streams.Get(uint32(sid))
			cstdin = in
		} else {
			in, err = fifo.OpenFifo(ctx, stdin, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
			cstdin = in
		}
		if err != nil {
			return nil, err
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
			// Close only the stdin reader when EOF/error occurs.
			// Do NOT close the console itself - it's needed for:
			// 1. Resize operations (ResizePty)
			// 2. Continued output streaming
			// 3. Multiple attach/detach cycles
			// The console will be properly closed when the process exits.
			if err := in.Close(); err != nil {
				log.L.WithError(err).Debug("console stdin close error")
			}
		}()
	}

	uri, err := url.Parse(stdout)
	if err != nil {
		return nil, fmt.Errorf("unable to parse stdout uri: %w", err)
	}

	switch uri.Scheme {
	case "stream":
		sid, err := strconv.ParseUint(strings.TrimPrefix(stdout, "stream://"), 10, 32)
		if err != nil {
			return nil, err
		}
		out, err := p.streams.Get(uint32(sid))
		if err != nil {
			return nil, err
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
	case "binary":
		ns, err := namespaces.NamespaceRequired(ctx)
		if err != nil {
			return nil, err
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
			return nil, fmt.Errorf("failed to create stdout pipes: %w", err)
		}
		filesToClose = append(filesToClose, outR)

		// Stderr is created for logging binary but unused when terminal is true
		serrR, _, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("failed to create stderr pipes: %w", err)
		}
		filesToClose = append(filesToClose, serrR)

		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
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
			return nil, fmt.Errorf("failed to start logging binary process: %w", err)
		}

		// Close our side of the pipe after start
		if err := w.Close(); err != nil {
			return nil, fmt.Errorf("failed to close write pipe after start: %w", err)
		}

		// Wait for the logging binary to be ready
		b := make([]byte, 1)
		if _, err := r.Read(b); err != nil && err != io.EOF {
			return nil, fmt.Errorf("failed to read from logging binary: %w", err)
		}
		cwg.Wait()
		success = true

	default:
		outw, err := fifo.OpenFifo(ctx, stdout, syscall.O_WRONLY, 0)
		if err != nil {
			return nil, err
		}
		outr, err := fifo.OpenFifo(ctx, stdout, syscall.O_RDONLY, 0)
		if err != nil {
			return nil, err
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
	}
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
