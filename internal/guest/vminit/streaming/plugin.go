// Package streaming provides the vminit vsock streaming service.
package streaming

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/containerd/containerd/v2/pkg/shutdown"
	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/mdlayher/vsock"

	"github.com/spin-stack/spinbox/internal/guest/vminit"
)

type serviceConfig struct {
	ContextID uint32
	Port      uint32
}

func (config *serviceConfig) SetVsock(cid, port uint32) {
	config.ContextID = cid
	config.Port = port
}

func init() {
	registry.Register(&plugin.Registration{
		Type: vminit.StreamingPlugin,
		ID:   "vsock",
		Requires: []plugin.Type{
			cplugins.InternalPlugin,
		},
		Config: &serviceConfig{},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			ss, err := ic.GetByID(cplugins.InternalPlugin, "shutdown")
			if err != nil {
				return nil, err
			}
			config, ok := ic.Config.(*serviceConfig)
			if !ok {
				return nil, fmt.Errorf("unexpected config type %T", ic.Config)
			}
			l, err := vsock.ListenContextID(config.ContextID, config.Port, &vsock.Config{})
			if err != nil {
				return nil, fmt.Errorf("failed to listen on vsock port %d with context id %d: %w", config.Port, config.ContextID, err)
			}

			s := &service{
				l:       l,
				streams: make(map[uint32]net.Conn),
			}

			shutdownSvc, ok := ss.(shutdown.Service)
			if !ok {
				return nil, fmt.Errorf("unexpected shutdown service type %T", ss)
			}
			shutdownSvc.RegisterCallback(s.Shutdown)

			go s.Run()

			return s, nil
		},
	})
}

type service struct {
	mu sync.Mutex
	l  net.Listener

	streams map[uint32]net.Conn
}

func (s *service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var errs []error

	// Close all connections
	for _, conn := range s.streams {
		if err := conn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close connection: %w", err))
		}
	}

	if s.l != nil {
		if err := s.l.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close listener: %w", err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (s *service) Run() {
	ctx := context.Background()
	for {
		conn, err := s.l.Accept()
		if err != nil {
			// Distinguish between normal shutdown and unexpected errors
			if errors.Is(err, net.ErrClosed) {
				log.G(ctx).Debug("streaming listener closed, stopping accept loop")
				return
			}
			log.G(ctx).WithError(err).Error("unexpected error accepting streaming connection")
			return
		}
		var b [4]byte
		if _, err := conn.Read(b[:]); err != nil {
			_ = conn.Close()
			continue // Error reading stream ID, close connection
		}

		s.mu.Lock()
		sid := binary.BigEndian.Uint32(b[:])
		if _, ok := s.streams[sid]; ok {
			s.mu.Unlock()
			log.G(ctx).WithField("stream_id", sid).Warn("stream ID collision detected, closing duplicate connection")
			_ = conn.Close()
			continue
		}
		s.streams[sid] = streamConn{
			Conn: conn,
			sid:  sid,
			s:    s,
		}
		s.mu.Unlock()
		if _, err := conn.Write(b[:]); err != nil {
			_ = conn.Close()
			continue
		}
	}
}

func (s *service) Get(id uint32) (io.ReadWriteCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.streams[id]
	if !ok {
		return nil, fmt.Errorf("stream %d not found: %w", id, errdefs.ErrNotFound)
	}
	return c, nil
}

type streamConn struct {
	net.Conn
	sid uint32
	s   *service
}

func (sc streamConn) Close() error {
	// Remove from map first, then close connection without holding mutex.
	// Closing a network connection can block (TCP handshake, pending writes),
	// so we must not hold the mutex during this I/O operation.
	sc.s.mu.Lock()
	delete(sc.s.streams, sc.sid)
	sc.s.mu.Unlock()

	if err := sc.Conn.Close(); err != nil {
		return fmt.Errorf("failed to close connection: %w", err)
	}

	return nil
}
