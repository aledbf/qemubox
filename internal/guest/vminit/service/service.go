//go:build linux

// Package service provides TTRPC service initialization and management for vminitd.
package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/log"
	"github.com/containerd/otelttrpc"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	"github.com/mdlayher/vsock"

	"github.com/spin-stack/spinbox/internal/guest/vminit"
	"github.com/spin-stack/spinbox/internal/guest/vminit/boottime"
	"github.com/spin-stack/spinbox/internal/guest/vminit/config"
)

// ttrpcService allows TTRPC services to be registered with the underlying server.
type ttrpcService interface {
	RegisterTTRPC(server *ttrpc.Server) error
}

// Service wraps a TTRPC server and vsock listener.
type Service struct {
	l      net.Listener
	server *ttrpc.Server

	// port is kept so the listener can be rebuilt: a restored VM has its vsock
	// device replaced under it, and the service has to bind the same port on
	// whatever device arrives next. See Run.
	port uint32
}

const (
	// relistenTimeout bounds how long the service waits for a vsock device after
	// losing the one it was serving on. Generous against the seconds a PCIe
	// hot-plug takes, short enough that a VM whose device never returns fails
	// rather than hangs.
	relistenTimeout = 60 * time.Second

	// relistenInterval is how often binding is retried while no device is there.
	relistenInterval = 5 * time.Millisecond

	// cidWatchInterval is how often the guest checks whether its CID changed
	// under it. Cheap - one ioctl - and it only has to beat the host's patience
	// while it waits for the restored VM to answer.
	cidWatchInterval = 20 * time.Millisecond
)

// Runnable represents a service that can be run.
type Runnable interface {
	Run(ctx context.Context) error
}

// New creates a new TTRPC service with plugin loading.
func New(ctx context.Context, cfg *config.ServiceConfig) (Runnable, error) {
	var (
		initializedPlugins = plugin.NewPluginSet()
		disabledPlugins    = map[string]struct{}{}
	)

	// Build disabled plugins map from config
	if len(cfg.DisabledPlugins) > 0 {
		for _, p := range cfg.DisabledPlugins {
			disabledPlugins[p] = struct{}{}
		}
	}

	// Listen on whatever CID this VM currently has, rather than binding to the
	// one the kernel command line named at boot. The two are the same for a VM
	// that booted normally, and they are not the same for a VM restored from a
	// template: the guest carries the template's CID in its command line, while
	// its vhost-vsock device was given a fresh one. Binding to the stale CID
	// leaves the listener unreachable, and the host times out waiting for a
	// guest that is running and healthy.
	l, err := vsock.Listen(uint32(cfg.RPCPort), &vsock.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to listen on vsock port %d: %w", cfg.RPCPort, err)
	}
	log.G(ctx).WithFields(log.Fields{
		"cid":  cfg.VSockContextID,
		"port": cfg.RPCPort,
	}).Debug("listening on vsock for RPC connections")
	boottime.LogReady(ctx, "vsock-listen")
	cfg.Shutdown.RegisterCallback(func(ctx context.Context) error {
		return l.Close()
	})

	ts, err := ttrpc.NewServer(
		ttrpc.WithUnaryServerInterceptor(otelttrpc.UnaryServerInterceptor()),
	)
	if err != nil {
		return nil, err
	}
	cfg.Shutdown.RegisterCallback(ts.Shutdown)

	registry.Register(&plugin.Registration{
		Type: cplugins.InternalPlugin,
		ID:   "shutdown",
		InitFn: func(ic *plugin.InitContext) (any, error) {
			return cfg.Shutdown, nil
		},
	})

	for _, reg := range registry.Graph(func(*plugin.Registration) bool { return false }) {
		id := reg.URI()
		if _, ok := disabledPlugins[id]; ok {
			log.G(ctx).WithField("plugin_id", id).Info("plugin is disabled, skipping load")
			continue
		}

		log.G(ctx).WithField("plugin_id", id).Debug("loading plugin")

		ic := plugin.NewContext(ctx, initializedPlugins, nil)

		if reg.Config != nil {
			// Apply plugin-specific configuration from config file if available
			if pluginCfg, ok := cfg.PluginConfigs[id]; ok {
				// Attempt to merge plugin config
				// This uses reflection to set fields, assuming Config is a pointer to struct
				if err := config.ApplyPluginConfig(reg.Config, pluginCfg); err != nil {
					return nil, fmt.Errorf("failed to apply plugin configuration for %s: %w", id, err)
				}
			}

			if vc, ok := reg.Config.(interface{ SetVsock(cid uint32, port uint32) }); ok {
				if reg.Type == vminit.StreamingPlugin {
					vc.SetVsock(uint32(cfg.VSockContextID), uint32(cfg.StreamPort))
				}
			}

			ic.Config = reg.Config
		}

		p := reg.Init(ic)
		if err := initializedPlugins.Add(p); err != nil {
			return nil, fmt.Errorf("could not add plugin result to plugin set: %w", err)
		}

		instance, err := p.Instance()
		if err != nil {
			if plugin.IsSkipPlugin(err) {
				log.G(ctx).WithFields(log.Fields{"error": err, "plugin_id": id}).Info("skipping plugin load")
				continue
			}

			return nil, fmt.Errorf("failed to load plugin %s: %w", id, err)
		}

		if s, ok := instance.(ttrpcService); ok {
			if err := s.RegisterTTRPC(ts); err != nil {
				return nil, fmt.Errorf("failed to register TTRPC service %s: %w", id, err)
			}
		}
	}

	return &Service{
		l:      l,
		server: ts,
		port:   uint32(cfg.RPCPort),
	}, nil
}

// firstAcceptListener fires onFirstAccept the first time a connection is
// accepted. Used to stamp VMINITD_READY phase=first-accept: the gap from the
// serve phase is how long the guest waited for the host to connect - the host
// readiness lag, measured guest-side.
type firstAcceptListener struct {
	net.Listener
	once          sync.Once
	onFirstAccept func()
}

func (f *firstAcceptListener) Accept() (net.Conn, error) {
	c, err := f.Listener.Accept()
	if err == nil && f.onFirstAccept != nil {
		f.once.Do(f.onFirstAccept)
	}
	return c, err
}

// Run starts the TTRPC server and blocks until it exits.
//
// It serves again if the listener dies under it, which happens when the vsock
// device is taken away: a VM restored from a template is handed a replacement
// device carrying a CID of its own, and the transport - and with it every socket
// on it - goes away while that swap is in flight. The guest is healthy
// throughout; it just has nothing to listen on for a moment, and the host cannot
// tell it to come back, because the channel it would use is the one that just
// left. So this waits for a transport to exist again and rebinds itself.
func (s *Service) Run(ctx context.Context) error {
	log.G(ctx).Info("starting TTRPC server")
	boottime.LogReady(ctx, "serve")
	rl := &rebindingListener{port: s.port, current: s.l, done: ctx.Done()}
	go rl.watchCID(ctx)
	l := &firstAcceptListener{
		Listener:      rl,
		onFirstAccept: func() { boottime.LogReady(ctx, "first-accept") },
	}
	err := s.server.Serve(ctx, l)
	if err != nil {
		log.G(ctx).WithError(err).Error("TTRPC server exited with error")
	} else {
		log.G(ctx).Info("TTRPC server exited cleanly")
	}
	return err
}

// rebindingListener keeps accepting across the disappearance of the vsock device
// underneath it.
//
// A VM restored from a template is handed a replacement vsock device carrying a
// CID of its own, and while that swap is in flight the transport goes away and
// takes every socket on it with it. The guest is healthy throughout and cannot
// be told to recover, because the channel that instruction would arrive on is
// the one that just left - so it recovers on its own.
//
// The rebinding happens here, below the RPC server, rather than around it: the
// server does not return from Serve when its listener fails, so a caller that
// waits for that to happen waits forever. It was written that way first, and the
// restored guest sat unreachable with nothing in its log.
type rebindingListener struct {
	port uint32

	// done is the shutdown signal rather than a stored context: Accept has no
	// context of its own to take, and it only needs to tell "the listener was
	// closed on purpose" from "the device went away".
	done <-chan struct{}

	mu      sync.Mutex
	current net.Listener
}

// watchCID closes the listener when this VM's CID changes, which is the only way
// the change becomes visible: a listener blocked in Accept on a transport that
// has been taken away is never woken - the kernel does not fail it, it simply
// stops delivering - so the guest cannot notice the swap by waiting for an
// error. It has to look. Closing the socket makes Accept return, and Accept
// rebinds on the device the VM has now.
func (r *rebindingListener) watchCID(ctx context.Context) {
	last, err := vsock.ContextID()
	if err != nil {
		log.G(ctx).WithError(err).Debug("cannot read local vsock CID; not watching for changes")
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(cidWatchInterval):
		}

		cid, err := vsock.ContextID()
		if err != nil || cid == last {
			// An error here means the device is gone at this instant, which the
			// next tick will see resolved one way or the other.
			continue
		}

		log.G(ctx).WithFields(log.Fields{"from": last, "to": cid}).
			Info("vsock CID changed; rebinding the RPC listener")
		last = cid
		_ = r.listener().Close()
	}
}

func (r *rebindingListener) listener() net.Listener {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current
}

func (r *rebindingListener) Accept() (net.Conn, error) {
	for {
		c, err := r.listener().Accept()
		if err == nil {
			return c, nil
		}
		select {
		case <-r.done:
			// Shutting down: the listener was closed on purpose.
			return nil, err
		default:
		}

		log.L.WithError(err).Warn("vsock listener failed; waiting for a device to bind to")
		if err := r.rebind(); err != nil {
			return nil, err
		}
	}
}

// rebind waits for a usable vsock transport and takes the port on it. Between
// the removal of one device and the arrival of the next there is nothing to bind
// to and vsock.Listen fails; that is the state this polls through.
func (r *rebindingListener) rebind() error {
	deadline := time.Now().Add(relistenTimeout)
	for {
		l, err := vsock.Listen(r.port, &vsock.Config{})
		if err == nil {
			r.mu.Lock()
			old := r.current
			r.current = l
			r.mu.Unlock()
			if old != nil {
				_ = old.Close()
			}
			log.L.WithField("port", r.port).Info("re-bound vsock listener after device change")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no vsock device to listen on after %s: %w", relistenTimeout, err)
		}
		select {
		case <-r.done:
			return errors.New("shutting down while waiting for a vsock device")
		case <-time.After(relistenInterval):
		}
	}
}

func (r *rebindingListener) Close() error   { return r.listener().Close() }
func (r *rebindingListener) Addr() net.Addr { return r.listener().Addr() }
