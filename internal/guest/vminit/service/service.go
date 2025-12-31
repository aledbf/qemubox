//go:build linux

// Package service provides TTRPC service initialization and management for vminitd.
package service

import (
	"context"
	"fmt"
	"net"

	"github.com/containerd/log"
	"github.com/containerd/otelttrpc"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/mdlayher/vsock"

	"github.com/aledbf/qemubox/containerd/internal/guest/vminit"
	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/config"
)

// ttrpcService allows TTRPC services to be registered with the underlying server.
type ttrpcService interface {
	RegisterTTRPC(server *ttrpc.Server) error
}

// Service wraps a TTRPC server and vsock listener.
type Service struct {
	l      net.Listener
	server *ttrpc.Server
}

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

	l, err := vsock.ListenContextID(uint32(cfg.VSockContextID), uint32(cfg.RPCPort), &vsock.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to listen on vsock port %d with context id %d: %w", cfg.RPCPort, cfg.VSockContextID, err)
	}
	log.G(ctx).WithFields(log.Fields{
		"cid":  cfg.VSockContextID,
		"port": cfg.RPCPort,
	}).Info("listening on vsock for RPC connections")
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

		log.G(ctx).WithField("plugin_id", id).Info("loading plugin")

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
				log.G(ctx).WithFields(log.Fields{"error": err, "plugin_id": id}).Info("skip loading plugin")
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
	}, nil
}

// Run starts the TTRPC server and blocks until it exits.
func (s *Service) Run(ctx context.Context) error {
	log.G(ctx).Info("starting TTRPC server")
	err := s.server.Serve(ctx, s.l)
	if err != nil {
		log.G(ctx).WithError(err).Error("TTRPC server exited with error")
	} else {
		log.G(ctx).Info("TTRPC server exited cleanly")
	}
	return err
}
