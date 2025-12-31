//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/pkg/sys/reaper"
	"github.com/containerd/log"
	"golang.org/x/sys/unix"

	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/config"
	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/service"
	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/system"
	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/systools"

	_ "github.com/aledbf/qemubox/containerd/internal/guest/services"
	_ "github.com/aledbf/qemubox/containerd/internal/guest/vminit/events"
	_ "github.com/aledbf/qemubox/containerd/internal/guest/vminit/streaming"
)

const (
	// maxGOMAXPROCS limits scheduler overhead in VM environment.
	// Value of 2 provides parallelism while maintaining cache locality.
	maxGOMAXPROCS = 2
)

func main() {
	cfg, setFlags, configFile, err := config.ParseFlags(os.Args[1:])
	if err != nil {
		log.L.WithError(err).Fatal("failed to parse flags")
	}

	// Load configuration file if provided
	if configFile != "" {
		if err := config.LoadFromFile(configFile, cfg, setFlags); err != nil {
			log.L.WithError(err).Fatalf("failed to load config from %s", configFile)
		}
	}

	if cfg.Debug {
		if err := log.SetLevel("debug"); err != nil {
			log.L.WithError(err).Fatal("failed to set log level")
		}
	} else {
		// Prefer verbose logging in the minimal VM to ease debugging boot/mount issues.
		if err := log.SetLevel("info"); err != nil {
			log.L.WithError(err).Fatal("failed to set log level")
		}
	}

	ctx := context.Background()

	log.G(ctx).WithField("args", os.Args[1:]).WithField("env", os.Environ()).Debug("starting vminitd")

	defer func() {
		if p := recover(); p != nil {
			// For PID 1, we recover from panics to ensure clean VM shutdown
			// Include stack trace for debugging
			log.G(ctx).WithField("panic", p).WithField("stack", string(debug.Stack())).Error("recovered from panic")
		}

		// Trigger VM shutdown via reboot syscall
		// This will cause QEMU to exit cleanly
		log.G(ctx).Info("powering off VM")
		if err := unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
			log.G(ctx).WithError(err).Error("failed to power off VM")
		}
	}()

	if err := run(ctx, cfg); err != nil {
		log.G(ctx).WithError(err).Error("exiting with error")
	}
}

func run(ctx context.Context, cfg *config.ServiceConfig) error {
	t1 := time.Now()

	ctx, cfg.Shutdown = shutdown.WithShutdown(ctx)

	if err := system.Initialize(ctx); err != nil {
		return err
	}

	if cfg.Debug {
		systools.DumpInfo(ctx)
	}

	svc, err := service.New(ctx, cfg)
	if err != nil {
		return err
	}

	log.G(ctx).WithField("t", time.Since(t1)).Debug("initialized vminitd")

	// Limit GOMAXPROCS for VM environment to prevent scheduler overhead
	// Cap at maxGOMAXPROCS to improve cache locality, but respect available CPUs
	maxProcs := min(runtime.NumCPU(), maxGOMAXPROCS)
	runtime.GOMAXPROCS(maxProcs)
	log.G(ctx).WithField("GOMAXPROCS", maxProcs).Debug("configured Go runtime")

	serviceErr := make(chan error, 1)
	go func() {
		serviceErr <- svc.Run(ctx)
	}()

	s := make(chan os.Signal, 1)
	signal.Notify(s, unix.SIGINT, unix.SIGTERM, unix.SIGHUP, unix.SIGQUIT, unix.SIGCHLD)
	for {
		select {
		case <-cfg.Shutdown.Done():
			shutdownErr := cfg.Shutdown.Err()
			if shutdownErr != nil && !errors.Is(shutdownErr, shutdown.ErrShutdown) {
				log.G(ctx).WithError(shutdownErr).Error("vminitd shutdown triggered with error")
			} else {
				log.G(ctx).Info("vminitd shutdown triggered")
			}
			return nil
		case err := <-serviceErr:
			if err != nil {
				log.G(ctx).WithError(err).Error("TTRPC service exited with error, triggering shutdown")
			} else {
				log.G(ctx).Info("TTRPC service exited cleanly, triggering shutdown")
			}
			return err
		case sig := <-s:
			switch sig {
			case unix.SIGCHLD:
				if err := reaper.Reap(); err != nil {
					log.G(ctx).WithError(err).Error("failed to reap child process")
				} else {
					log.G(ctx).Debug("reaped child process")
				}
			case unix.SIGINT, unix.SIGTERM, unix.SIGQUIT:
				log.G(ctx).WithField("signal", sig).Info("received shutdown signal, triggering shutdown")
				cfg.Shutdown.Shutdown()
			default:
				log.G(ctx).WithField("signal", sig).Debug("received unhandled signal")
			}
		}
	}
}
