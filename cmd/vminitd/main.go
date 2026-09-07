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

	"github.com/spin-stack/spinbox/internal/guest/vminit/boottime"
	"github.com/spin-stack/spinbox/internal/guest/vminit/config"
	"github.com/spin-stack/spinbox/internal/guest/vminit/service"
	"github.com/spin-stack/spinbox/internal/guest/vminit/system"
	"github.com/spin-stack/spinbox/internal/guest/vminit/systools"

	_ "github.com/spin-stack/spinbox/internal/guest/services"
	_ "github.com/spin-stack/spinbox/internal/guest/vminit/events"
	_ "github.com/spin-stack/spinbox/internal/guest/vminit/streaming"
)

const (
	// maxGOMAXPROCS limits scheduler overhead in VM environment.
	// Value of 2 provides parallelism while maintaining cache locality.
	maxGOMAXPROCS = 2
)

func main() {
	// Stamp PID1 entry against CLOCK_BOOTTIME as early as possible: this is the
	// clean kernel-boot number (kernel start -> init exec).
	boottime.LogReady(context.Background(), "pid1-entry")

	cfg, setFlags, configFile, err := config.ParseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, config.ErrVersionRequested) {
			os.Exit(0)
		}
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

		// Perform pre-shutdown cleanup to remove temporary files and unmount
		// filesystems. This ensures snapshots don't contain stale container data.
		system.Cleanup(ctx)

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

	// Boot profiling: attribute userspace boot phases to VMINITD_PROFILE lines
	// (the companion to the kernel's initcall_debug). No-op unless the
	// spin.profile cmdline marker is present (debug-boot mode).
	prof := system.NewBootProfiler(t1)

	ctx, cfg.Shutdown = shutdown.WithShutdown(ctx)

	if err := system.Initialize(ctx, prof); err != nil {
		return err
	}
	boottime.LogReady(ctx, "system-init")

	// Take this VM's identity from the kernel command line. A VM that boots is
	// the only kind that can: a VM restored from a template inherits the
	// template's command line, which names another VM's address, another VM's
	// disks and another VM's CID, and is told who it is over RPC instead - see
	// the Configure call in api/services/system/v1.
	//
	// Being told is the normal case now and this is the exception, so it is
	// best-effort in the same way the steps inside it always were: a VM that
	// cannot read its command line should still come up and serve RPC, because
	// serving RPC is how it can be told anything at all.
	if id, err := system.FromCmdline(ctx); err != nil {
		log.G(ctx).WithError(err).Warn("cannot read this VM's identity from the kernel command line")
	} else if err := system.Apply(ctx, id); err != nil {
		log.G(ctx).WithError(err).Error("failed to apply this VM's identity")
	}
	prof.Mark(ctx, "identity")
	boottime.LogReady(ctx, "identity")

	if cfg.Debug {
		systools.DumpInfo(ctx)
	}

	svc, err := service.New(ctx, cfg)
	if err != nil {
		return err
	}
	prof.Mark(ctx, "service-new")

	// Boot profiling: dump the complete kernel initcall profile from /dev/kmsg
	// (includes the early ACPI/PCI initcalls the hvc0 console hides). No-op
	// unless spin.profile is set.
	system.DumpKernelBootProfile(ctx)

	log.G(ctx).WithField("t", time.Since(t1)).Debug("initialized vminitd")

	// Note: The supervisor agent is now started after container creation
	// (in runc/container.go) with the overlay root path passed via env var.
	// This ensures the supervisor has access to the container's rootfs.

	// Limit GOMAXPROCS for VM environment to prevent scheduler overhead
	// Cap at maxGOMAXPROCS to improve cache locality, but respect available CPUs
	maxProcs := min(runtime.NumCPU(), maxGOMAXPROCS)
	runtime.GOMAXPROCS(maxProcs)
	log.G(ctx).WithField("GOMAXPROCS", maxProcs).Debug("configured Go runtime")

	serviceErr := make(chan error, 1)
	go func() {
		serviceErr <- svc.Run(ctx)
	}()

	s := make(chan os.Signal, 32)
	signal.Notify(s, unix.SIGINT, unix.SIGTERM, unix.SIGQUIT, unix.SIGCHLD)
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
			}
		}
	}
}
