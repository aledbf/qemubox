//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
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

	"github.com/aledbf/qemubox/containerd/internal/guest/vminit"
	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/systools"

	_ "github.com/aledbf/qemubox/containerd/internal/guest/services"
	_ "github.com/aledbf/qemubox/containerd/internal/guest/vminit/events"
	_ "github.com/aledbf/qemubox/containerd/internal/guest/vminit/streaming"
)

// loadConfig loads configuration from a JSON file and merges it with the provided config.
// Command-line flags take precedence over file configuration.
func loadConfig(path string, config *ServiceConfig, setFlags map[string]bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	// Store flag values before unmarshaling
	flagDebug := config.Debug
	flagRPCPort := config.RPCPort
	flagStreamPort := config.StreamPort
	flagVSockContextID := config.VSockContextID

	if err := json.Unmarshal(data, config); err != nil {
		return fmt.Errorf("failed to parse config file: %w", err)
	}

	// Restore flag values that were explicitly set by the user
	// This ensures flags override config file
	if setFlags["debug"] {
		config.Debug = flagDebug
	}
	if setFlags["vsock-rpc-port"] {
		config.RPCPort = flagRPCPort
	}
	if setFlags["vsock-stream-port"] {
		config.StreamPort = flagStreamPort
	}
	if setFlags["vsock-cid"] {
		config.VSockContextID = flagVSockContextID
	}

	return nil
}

// applyPluginConfig applies configuration map to a plugin config struct.
//
// This uses JSON marshal/unmarshal as a type-safe conversion mechanism:
// - Handles type conversions (string to int, etc.) via JSON codec
// - Validates field types and values during unmarshal
// - Works with any plugin config struct without reflection complexity
// - Respects JSON tags for field mapping
//
// Trade-off: Slightly slower than direct assignment, but safer and more maintainable.
func applyPluginConfig(pluginConfig any, configMap map[string]any) error {
	if pluginConfig == nil {
		return fmt.Errorf("plugin config is nil")
	}

	// Marshal the config map to JSON
	data, err := json.Marshal(configMap)
	if err != nil {
		return fmt.Errorf("failed to marshal config map: %w", err)
	}

	// Unmarshal into the plugin config struct
	if err := json.Unmarshal(data, pluginConfig); err != nil {
		return fmt.Errorf("failed to unmarshal into plugin config: %w", err)
	}

	return nil
}

func main() {
	var (
		config     ServiceConfig
		configFile string
	)
	flag.StringVar(&configFile, "config", "", "Path to configuration file")
	flag.BoolVar(&config.Debug, "debug", false, "Debug log level")
	flag.IntVar(&config.RPCPort, "vsock-rpc-port", 1025, "vsock port to listen for rpc on")
	flag.IntVar(&config.StreamPort, "vsock-stream-port", 1026, "vsock port to listen for streams on")
	flag.IntVar(&config.VSockContextID, "vsock-cid", 3, "vsock context ID for vsock listen")
	args := os.Args[1:]

	if err := flag.CommandLine.Parse(args); err != nil {
		log.L.WithError(err).Fatal("failed to parse flags")
	}

	// Track which flags were explicitly set by the user
	setFlags := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) {
		setFlags[f.Name] = true
	})

	// Load configuration file if provided
	if configFile != "" {
		if err := loadConfig(configFile, &config, setFlags); err != nil {
			log.L.WithError(err).Fatalf("failed to load config from %s", configFile)
		}
	}

	if config.Debug {
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

	log.G(ctx).WithField("args", args).WithField("env", os.Environ()).Debug("starting vminitd")

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

	if err := run(ctx, config); err != nil {
		log.G(ctx).WithError(err).Error("exiting with error")
	}
}

func run(ctx context.Context, config ServiceConfig) error {
	t1 := time.Now()

	ctx, config.Shutdown = shutdown.WithShutdown(ctx)

	if err := systemInit(ctx); err != nil {
		return err
	}

	if config.Debug {
		systools.DumpInfo(ctx)
	}

	service, err := New(ctx, config)
	if err != nil {
		return err
	}

	log.G(ctx).WithField("t", time.Since(t1)).Debug("initialized vminitd")

	// Limit GOMAXPROCS for VM environment to prevent scheduler overhead
	// Cap at 2 to improve cache locality, but respect available CPUs
	maxProcs := min(runtime.NumCPU(), 2)
	runtime.GOMAXPROCS(maxProcs)
	log.G(ctx).WithField("GOMAXPROCS", maxProcs).Debug("configured Go runtime")

	serviceErr := make(chan error, 1)
	go func() {
		serviceErr <- service.Run(ctx)
	}()

	s := make(chan os.Signal, 1)
	signal.Notify(s, unix.SIGINT, unix.SIGTERM, unix.SIGHUP, unix.SIGQUIT, unix.SIGCHLD)
	for {
		select {
		case <-config.Shutdown.Done():
			shutdownErr := config.Shutdown.Err()
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
				config.Shutdown.Shutdown()
			default:
				log.G(ctx).WithField("signal", sig).Debug("received unhandled signal")
			}
		}
	}
}

// systemInit initializes the system
func systemInit(ctx context.Context) error {
	if err := systemMounts(); err != nil {
		return err
	}

	// Configure CTRL+ALT+DELETE to send SIGINT to init instead of immediately rebooting
	// This allows vminitd to catch the signal and perform a clean shutdown
	// Default behavior (1) causes immediate kernel reboot without notifying init
	if err := os.WriteFile("/proc/sys/kernel/ctrl-alt-del", []byte("0"), 0644); err != nil {
		// In production, unexpected reboots could be a security concern
		// Log at error level but continue - the setting may not be available in all kernels
		log.G(ctx).WithError(err).Error("failed to configure ctrl-alt-del behavior - VM may reboot unexpectedly on CTRL+ALT+DEL")
	}

	// Wait for virtio block devices to appear
	// This is necessary because the kernel may not have probed all virtio devices yet
	// Not fatal if devices don't appear - they might appear later or not be needed
	waitForBlockDevices(ctx)

	if err := setupCgroupControl(); err != nil {
		return err
	}

	// #nosec G301 -- /etc must be world-readable inside the VM.
	if err := os.Mkdir("/etc", 0755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("failed to create /etc: %w", err)
	}

	// Configure DNS from kernel command line
	if err := configureDNS(ctx); err != nil {
		log.G(ctx).WithError(err).Warn("failed to configure DNS, continuing anyway")
	}

	return nil
}

// findVirtioBlockDevices finds virtio block devices in /sys/block
func findVirtioBlockDevices() ([]string, error) {
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil, err
	}

	var devices []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "vd") {
			devices = append(devices, entry.Name())
		}
	}
	return devices, nil
}

// waitForDevNodes polls for device nodes to appear in /dev
// Returns true if all device nodes are ready, false otherwise
func waitForDevNodes(ctx context.Context, devices []string) bool {
	for range 10 {
		var devNodes []string
		for _, dev := range devices {
			devPath := "/dev/" + dev
			if _, err := os.Stat(devPath); err == nil {
				devNodes = append(devNodes, devPath)
			}
		}

		if len(devNodes) == len(devices) {
			log.G(ctx).WithField("dev_nodes", devNodes).Info("virtio block device nodes ready")
			return true
		}

		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// waitForBlockDevices waits for virtio block devices to appear in /dev
// The kernel needs time to probe PCI devices and create device nodes
// This is a best-effort operation - if devices don't appear, we continue anyway
func waitForBlockDevices(ctx context.Context) {
	timeout := 5 * time.Second
	pollInterval := 10 * time.Millisecond

	log.G(ctx).Debug("waiting for virtio block devices to appear")

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		// Check /sys/block for all block devices
		vdDevices, err := findVirtioBlockDevices()
		if err == nil && len(vdDevices) > 0 {
			log.G(ctx).WithField("devices", vdDevices).Info("found virtio block devices in /sys/block")

			// Poll for /dev nodes to appear
			// udev may need time to create device nodes after kernel detection
			if waitForDevNodes(ctx, vdDevices) {
				return
			}
		}

		select {
		case <-ctx.Done():
			log.G(ctx).Warn("timeout waiting for virtio block devices, continuing anyway")
			return
		case <-ticker.C:
			// continue polling
		}
	}
}

func systemMounts() error {
	// Create /lib if it doesn't exist (needed for modules)
	// #nosec G301 -- /lib must be world-readable inside the VM.
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
			Source:  "tmpfs",
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
	// #nosec G306 -- kernel-managed cgroup control file expects 0644.
	return os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+cpu +cpuset +io +memory +pids"), 0644)
}

// configureDNS parses DNS servers from kernel ip= parameter and writes /etc/resolv.conf
// The kernel ip= parameter format is:
// ip=<client-ip>:<server-ip>:<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>:<dns0-ip>:<dns1-ip>
func configureDNS(ctx context.Context) error {
	// Read kernel command line
	cmdlineBytes, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return fmt.Errorf("failed to read /proc/cmdline: %w", err)
	}

	cmdline := string(cmdlineBytes)
	log.G(ctx).WithField("cmdline", cmdline).Debug("parsing kernel command line for DNS config")

	// Parse ip= parameter
	var nameservers []string
	for param := range strings.FieldsSeq(cmdline) {
		if ipParam, ok := strings.CutPrefix(param, "ip="); ok {
			// Split by colons: client-ip:server-ip:gw-ip:netmask:hostname:device:autoconf:dns0-ip:dns1-ip
			parts := strings.Split(ipParam, ":")

			// DNS servers are at index 7 and 8 (0-indexed)
			// Format: ip=<client-ip>:<server-ip>:<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>:<dns0-ip>:<dns1-ip>
			//         0           1           2      3         4          5        6           7         8
			if len(parts) > 7 && parts[7] != "" {
				nameservers = append(nameservers, parts[7])
			}
			if len(parts) > 8 && parts[8] != "" {
				nameservers = append(nameservers, parts[8])
			}
			break
		}
	}

	if len(nameservers) == 0 {
		log.G(ctx).Debug("no DNS servers found in kernel ip= parameter")
		return nil
	}

	// Build resolv.conf content
	var resolvConf strings.Builder
	for _, ns := range nameservers {
		fmt.Fprintf(&resolvConf, "nameserver %s\n", ns)
	}

	// Write /etc/resolv.conf
	// #nosec G306 -- /etc/resolv.conf must be world-readable for non-root processes.
	if err := os.WriteFile("/etc/resolv.conf", []byte(resolvConf.String()), 0644); err != nil {
		return fmt.Errorf("failed to write /etc/resolv.conf: %w", err)
	}

	log.G(ctx).WithField("nameservers", nameservers).Info("configured DNS resolvers from kernel ip= parameter")
	return nil
}

// ttrpcService allows TTRPC services to be registered with the underlying server
type ttrpcService interface {
	RegisterTTRPC(server *ttrpc.Server) error
}

type service struct {
	l      net.Listener
	server *ttrpc.Server
}

type Runnable interface {
	Run(ctx context.Context) error
}

type ServiceConfig struct {
	VSockContextID  int                       `json:"vsock_context_id,omitempty"`
	RPCPort         int                       `json:"rpc_port,omitempty"`
	StreamPort      int                       `json:"stream_port,omitempty"`
	Shutdown        shutdown.Service          `json:"-"`
	Debug           bool                      `json:"debug,omitempty"`
	DisabledPlugins []string                  `json:"disabled_plugins,omitempty"`
	PluginConfigs   map[string]map[string]any `json:"plugin_configs,omitempty"`
}

func New(ctx context.Context, config ServiceConfig) (Runnable, error) {
	var (
		initializedPlugins = plugin.NewPluginSet()
		disabledPlugins    = map[string]struct{}{}
	)

	// Build disabled plugins map from config
	if len(config.DisabledPlugins) > 0 {
		for _, p := range config.DisabledPlugins {
			disabledPlugins[p] = struct{}{}
		}
	}

	l, err := vsock.ListenContextID(uint32(config.VSockContextID), uint32(config.RPCPort), &vsock.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to listen on vsock port %d with context id %d: %w", config.RPCPort, config.VSockContextID, err)
	}
	log.G(ctx).WithFields(log.Fields{
		"cid":  config.VSockContextID,
		"port": config.RPCPort,
	}).Info("listening on vsock for RPC connections")
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
			if pluginCfg, ok := config.PluginConfigs[id]; ok {
				// Attempt to merge plugin config
				// This uses reflection to set fields, assuming Config is a pointer to struct
				if err := applyPluginConfig(reg.Config, pluginCfg); err != nil {
					return nil, fmt.Errorf("failed to apply plugin configuration for %s: %w", id, err)
				}
			}

			if vc, ok := reg.Config.(interface{ SetVsock(cid uint32, port uint32) }); ok {
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
			if err := s.RegisterTTRPC(ts); err != nil {
				return nil, fmt.Errorf("failed to register TTRPC service %s: %w", id, err)
			}
		}
	}

	return &service{
		l:      l,
		server: ts,
	}, nil
}

func (s *service) Run(ctx context.Context) error {
	log.G(ctx).Info("starting TTRPC server")
	err := s.server.Serve(ctx, s.l)
	if err != nil {
		log.G(ctx).WithError(err).Error("TTRPC server exited with error")
	} else {
		log.G(ctx).Info("TTRPC server exited cleanly")
	}
	return err
}
