//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/pkg/sys/reaper"
	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/log"
	"github.com/containerd/otelttrpc"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

	"github.com/aledbf/beacon/containerd/vminit"
	"github.com/aledbf/beacon/containerd/vminit/systools"

	_ "github.com/aledbf/beacon/containerd/api/services/bundle/v1"
	_ "github.com/aledbf/beacon/containerd/api/services/system/v1"

	_ "github.com/aledbf/beacon/containerd/services"
	_ "github.com/aledbf/beacon/containerd/vminit/events"
	_ "github.com/aledbf/beacon/containerd/vminit/streaming"
)

func main() {
	t1 := time.Now()
	var (
		config ServiceConfig
		dev    = flag.Bool("dev", false, "Development mode with graceful exit")
	)
	flag.BoolVar(&config.Debug, "debug", true, "Debug log level")
	flag.IntVar(&config.RPCPort, "vsock-rpc-port", 1025, "vsock port to listen for rpc on")
	flag.IntVar(&config.StreamPort, "vsock-stream-port", 1026, "vsock port to listen for streams on")
	flag.IntVar(&config.VSockContextID, "vsock-cid", 3, "vsock context ID for vsock listen")
	args := os.Args[1:]
	// Strip "tsi_hijack" added by libkrun
	if len(args) > 0 && args[0] == "tsi_hijack" {
		args = args[1:]
	}
	flag.CommandLine.Parse(args)

	/*
		c, err := os.OpenFile("/dev/console", os.O_WRONLY, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to open /dev/console: %v\n", err)
			os.Exit(1)
		}
		defer c.Close()
		log.L.Logger.SetOutput(c)
	*/
	var err error

	if *dev || config.Debug {
		log.SetLevel("debug")
	}

	ctx := context.Background()

	log.G(ctx).WithField("args", args).WithField("env", os.Environ()).Debug("starting vminitd")

	defer func() {
		if err != nil {
			log.G(ctx).WithError(err).Error("exiting with error")
		} else if p := recover(); p != nil {
			log.G(ctx).WithField("panic", p).Error("recovered from panic")
		} else {
			log.G(ctx).Debug("exiting cleanly")
		}

		if !*dev {
			// Trigger VM shutdown via reboot syscall
			// This will cause Cloud Hypervisor to exit cleanly
			log.G(ctx).Info("powering off VM")
			if err := unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
				log.G(ctx).WithError(err).Error("failed to power off VM")
			}
		}
	}()

	ctx, config.Shutdown = shutdown.WithShutdown(ctx)
	err = systemInit(ctx, config)
	if err != nil {
		return
	}

	if config.Debug {
		systools.DumpInfo(ctx)
	}

	service, err := New(ctx, config)
	if err != nil {
		return
	}

	log.G(ctx).WithField("t", time.Since(t1)).Debug("initialized vminitd")

	runtime.GOMAXPROCS(2)

	serviceErr := make(chan error, 1)
	go func() {
		serviceErr <- service.Run(ctx)
	}()

	s := make(chan os.Signal, 16)
	signal.Notify(s, unix.SIGKILL, unix.SIGINT, unix.SIGTERM, unix.SIGHUP, unix.SIGQUIT, unix.SIGCHLD)
	for {
		select {
		case <-config.Shutdown.Done():
			if err := config.Shutdown.Err(); err != nil && !errors.Is(err, shutdown.ErrShutdown) {
				log.G(ctx).WithError(err).Error("shutdown error")
			}
			return
		case err := <-serviceErr:
			log.G(ctx).WithError(err).Error("service exited")
			return
		case sig := <-s:
			switch sig {
			case unix.SIGCHLD:
				if err := reaper.Reap(); err != nil {
					log.G(ctx).WithError(err).Error("failed to reap child process")
				} else {
					log.G(ctx).Debug("reaped child process")
				}
			case unix.SIGKILL, unix.SIGINT, unix.SIGTERM, unix.SIGQUIT:
				config.Shutdown.Shutdown()
				log.G(ctx).WithField("signal", sig).Info("received shutdown signal")
			default:
				log.G(ctx).WithField("signal", sig).Debug("received unhandled signal")
			}
		}

	}

}

// systemInit initializes the system and returns a function to renew DHCP
// leases.
func systemInit(ctx context.Context, config ServiceConfig) error {
	if err := systemMounts(); err != nil {
		return err
	}

	// Wait for virtio block devices to appear
	// This is necessary because the kernel may not have probed all virtio devices yet
	if err := waitForBlockDevices(ctx); err != nil {
		log.G(ctx).WithError(err).Warn("failed to wait for block devices")
		// Don't fail - devices might appear later or not be needed
	}

	if err := setupCgroupControl(); err != nil {
		return err
	}

	if err := os.Mkdir("/etc", 0755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("failed to create /etc: %w", err)
	}

	return nil
}

// waitForBlockDevices waits for virtio block devices to appear in /dev
// The kernel needs time to probe PCI devices and create device nodes
func waitForBlockDevices(ctx context.Context) error {
	timeout := 5 * time.Second
	pollInterval := 50 * time.Millisecond
	deadline := time.Now().Add(timeout)

	log.G(ctx).Debug("waiting for virtio block devices to appear")

	for time.Now().Before(deadline) {
		// Check /sys/block for all block devices
		entries, err := os.ReadDir("/sys/block")
		if err == nil {
			var vdDevices []string
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "vd") {
					vdDevices = append(vdDevices, entry.Name())
				}
			}

			if len(vdDevices) > 0 {
				log.G(ctx).WithField("devices", vdDevices).Info("found virtio block devices in /sys/block")

				// Wait for /dev nodes to appear
				time.Sleep(100 * time.Millisecond)

				// Verify /dev nodes exist
				var devNodes []string
				for _, dev := range vdDevices {
					devPath := "/dev/" + dev
					if _, err := os.Stat(devPath); err == nil {
						devNodes = append(devNodes, devPath)
					}
				}
				log.G(ctx).WithField("dev_nodes", devNodes).Info("virtio block device nodes ready")

				if len(devNodes) > 0 {
					return nil
				}
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}

	log.G(ctx).Warn("timeout waiting for virtio block devices, continuing anyway")
	return fmt.Errorf("timeout waiting for block devices")
}

func systemMounts() error {
	// Create /lib if it doesn't exist (needed for modules)
	if err := os.MkdirAll("/lib", 0755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("failed to create /lib: %w", err)
	}

	return mount.All([]mount.Mount{
		{
			Type:    "proc",
			Source:  "proc",
			Target:  "/proc",
			Options: []string{"nosuid", "noexec", "nodev"},
		},
		{
			Type:    "sysfs",
			Source:  "sysfs",
			Target:  "/sys",
			Options: []string{"nosuid", "noexec", "nodev"},
		},
		{
			Type:   "cgroup2",
			Source: "none",
			Target: "/sys/fs/cgroup",
		},
		{
			Type:    "tmpfs",
			Source:  "tmpfs",
			Target:  "/run",
			Options: []string{"nosuid", "noexec", "nodev"},
		},
		{
			Type:    "tmpfs",
			Source:  "tmpsfs",
			Target:  "/tmp",
			Options: []string{"nosuid", "noexec", "nodev"},
		},
		{
			Type:    "devtmpfs",
			Source:  "devtmpsfs",
			Target:  "/dev",
			Options: []string{"nosuid", "noexec"},
		},
	}, "/")
}

func setupCgroupControl() error {
	return os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+cpu +cpuset +io +memory +pids"), 0644)
}

// ttrpcService allows TTRPC services to be registered with the underlying server
type ttrpcService interface {
	RegisterTTRPC(*ttrpc.Server) error
}

type service struct {
	l      net.Listener
	server *ttrpc.Server
}

type Runnable interface {
	Run(context.Context) error
}

type ServiceConfig struct {
	VSockContextID int
	RPCPort        int
	StreamPort     int
	Shutdown       shutdown.Service
	Debug          bool
}

func New(ctx context.Context, config ServiceConfig) (Runnable, error) {
	var (
		initializedPlugins = plugin.NewPluginSet()
		disabledPlugins    = map[string]struct{}{}
		// TODO: service config?
	)

	l, err := vsock.ListenContextID(uint32(config.VSockContextID), uint32(config.RPCPort), &vsock.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to listen on vsock port %d with context id %d: %w", config.RPCPort, config.VSockContextID, err)
	}
	config.Shutdown.RegisterCallback(func(ctx context.Context) error {
		return l.Close()
	})

	ts, err := ttrpc.NewServer(
		ttrpc.WithUnaryServerInterceptor(otelttrpc.UnaryServerInterceptor()),
	)
	if err != nil {
		return nil, err
	}
	config.Shutdown.RegisterCallback(ts.Shutdown)

	registry.Register(&plugin.Registration{
		Type: cplugins.InternalPlugin,
		ID:   "shutdown",
		InitFn: func(ic *plugin.InitContext) (any, error) {
			return config.Shutdown, nil
		},
	})

	// TODO: Allow disabling plugins
	//if len(cfg.DisabledPlugins) > 0 {
	//	for _, p := range cfg.DisabledPlugins {
	//		disabledPlugins[p] = struct{}{}
	//	}
	//}

	for _, reg := range registry.Graph(func(*plugin.Registration) bool { return false }) {
		id := reg.URI()
		if _, ok := disabledPlugins[id]; ok {
			log.G(ctx).WithField("plugin_id", id).Info("plugin is disabled, skipping load")
			continue
		}

		log.G(ctx).WithField("plugin_id", id).Info("loading plugin")

		ic := plugin.NewContext(ctx, initializedPlugins, nil)

		if reg.Config != nil {
			// TODO: Allow plugin config?
			if vc, ok := reg.Config.(interface{ SetVsock(uint32, uint32) }); ok {
				if reg.Type == vminit.StreamingPlugin {
					vc.SetVsock(uint32(config.VSockContextID), uint32(config.StreamPort))
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
			s.RegisterTTRPC(ts)
		}
	}

	return &service{
		l:      l,
		server: ts,
	}, nil
}

func (s *service) Run(ctx context.Context) error {
	return s.server.Serve(ctx, s.l)
}
