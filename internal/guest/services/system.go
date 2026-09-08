//go:build linux

package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	"golang.org/x/sys/unix"

	api "github.com/spin-stack/spinbox/api/spinbox/services/system/v1"
	"github.com/spin-stack/spinbox/internal/guest/vminit/devices"
	guestsystem "github.com/spin-stack/spinbox/internal/guest/vminit/system"
	"github.com/spin-stack/spinbox/internal/version"
)

const (
	// Sysfs file values
	sysfsOnline  = "1"
	sysfsOffline = "0"

	// Timeout for waiting for sysfs files to appear after hotplug
	sysfsWaitTimeout = 2 * time.Second

	// File permissions
	sysfsFilePerms    = 0600
	featuresDirPerms  = 0750
	featuresFilePerms = 0600
)

type systemService struct {
	// frozenMu guards frozen, the mount points frozen by FreezeFilesystems and
	// awaiting ThawFilesystems.
	frozenMu sync.Mutex
	frozen   []string
}

var prepareShutdown = guestsystem.Cleanup

var _ api.TTRPCSystemServiceService = &systemService{}

func init() {
	registry.Register(&plugin.Registration{
		Type:   cplugins.TTRPCPlugin,
		ID:     "system",
		InitFn: initFunc,
	})
}

func initFunc(ic *plugin.InitContext) (interface{}, error) {
	s := &systemService{}
	// Write runtime features to a file for the shim manager to read
	if err := s.writeRuntimeFeatures(); err != nil {
		// Non-fatal - log but continue
		log.G(ic.Context).WithError(err).Warn("failed to write runtime features")
	}
	return s, nil
}

func (s *systemService) RegisterTTRPC(server *ttrpc.Server) error {
	api.RegisterTTRPCSystemServiceService(server, s)
	return nil
}

// readSysfsValue reads and trims a value from a sysfs file.
func readSysfsValue(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// writeSysfsValue writes a value to a sysfs file.
func writeSysfsValue(path, value string) error {
	return os.WriteFile(path, []byte(value), sysfsFilePerms)
}

// waitForSysfsFile waits for a sysfs file to appear using inotify.
// This is more efficient than polling - the kernel notifies us when the file is created.
// Returns nil if the file exists or appears within the timeout.
func waitForSysfsFile(ctx context.Context, path string, timeout time.Duration) error {
	// Fast path: check if file already exists
	if _, err := os.Stat(path); err == nil {
		return nil
	}

	dir := filepath.Dir(path)
	base := filepath.Base(path)

	// Create inotify instance
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		// Fall back to polling if inotify fails
		return waitForSysfsFilePoll(ctx, path, timeout)
	}
	defer unix.Close(fd)

	// Watch parent directory for CREATE events
	_, err = unix.InotifyAddWatch(fd, dir, unix.IN_CREATE|unix.IN_MOVED_TO)
	if err != nil {
		// Directory might not exist yet - fall back to polling
		return waitForSysfsFilePoll(ctx, path, timeout)
	}

	// Check again after setting up watch (file might have appeared between first check and watch setup)
	if _, err := os.Stat(path); err == nil {
		return nil
	}

	// Wait for inotify events with timeout
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 4096)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		// Use poll to wait for events with timeout
		pollFds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		pollTimeout := int(remaining.Milliseconds())
		if pollTimeout > 100 {
			pollTimeout = 100 // Check context every 100ms
		}

		n, err := unix.Poll(pollFds, pollTimeout)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("poll failed: %w", err)
		}

		if n == 0 {
			// Timeout on this poll iteration, check file and continue
			if _, err := os.Stat(path); err == nil {
				return nil
			}
			continue
		}

		// Read inotify events
		nread, err := unix.Read(fd, buf)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			return fmt.Errorf("read inotify: %w", err)
		}

		// Parse events
		for offset := 0; offset < nread; {
			// #nosec G103 -- unsafe.Pointer is required to parse inotify events from the kernel
			event := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))
			nameLen := int(event.Len)
			if nameLen > 0 {
				name := string(buf[offset+unix.SizeofInotifyEvent : offset+unix.SizeofInotifyEvent+nameLen])
				name = strings.TrimRight(name, "\x00")
				if name == base {
					// Our file was created
					return nil
				}
			}
			offset += unix.SizeofInotifyEvent + nameLen
		}
	}

	return fmt.Errorf("timeout waiting for %s", path)
}

// waitForSysfsFilePoll is a fallback polling implementation when inotify is unavailable.
func waitForSysfsFilePoll(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	backoff := 5 * time.Millisecond
	maxBackoff := 100 * time.Millisecond

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if _, err := os.Stat(path); err == nil {
			return nil
		}

		time.Sleep(backoff)
		backoff = min(backoff*2, maxBackoff)
	}

	return fmt.Errorf("timeout waiting for %s", path)
}

func (s *systemService) Info(ctx context.Context, _ *api.InfoRequest) (*api.InfoResponse, error) {
	v, err := os.ReadFile("/proc/version")
	if err != nil && !os.IsNotExist(err) {
		return nil, errgrpc.ToGRPC(err)
	}
	return &api.InfoResponse{
		Version:       version.Short(),
		KernelVersion: string(v),
	}, nil
}

func (s *systemService) PrepareShutdown(ctx context.Context, _ *api.PrepareShutdownRequest) (*api.PrepareShutdownResponse, error) {
	prepareShutdown(ctx)
	return &api.PrepareShutdownResponse{}, nil
}

// FreezeFilesystems freezes the container's writable filesystem(s) (FIFREEZE)
// so the backing rwlayer image can be read consistently while the VM runs.
func (s *systemService) FreezeFilesystems(ctx context.Context, _ *api.FreezeFilesystemsRequest) (*api.FreezeFilesystemsResponse, error) {
	frozen, err := guestsystem.FreezeWritableFilesystems(ctx)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	s.frozenMu.Lock()
	s.frozen = frozen
	s.frozenMu.Unlock()

	return &api.FreezeFilesystemsResponse{Frozen: frozen}, nil
}

// ThawFilesystems thaws filesystems previously frozen by FreezeFilesystems.
// It is idempotent: thawing when nothing is frozen is a no-op.
func (s *systemService) ThawFilesystems(ctx context.Context, _ *api.ThawFilesystemsRequest) (*api.ThawFilesystemsResponse, error) {
	s.frozenMu.Lock()
	paths := s.frozen
	s.frozen = nil
	s.frozenMu.Unlock()

	if err := guestsystem.ThawFilesystems(ctx, paths); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return &api.ThawFilesystemsResponse{}, nil
}

func (s *systemService) OfflineCPU(ctx context.Context, req *api.OfflineCPURequest) (*api.OfflineCPUResponse, error) {
	cpuID := req.GetCpuID()
	if cpuID == 0 {
		return nil, errgrpc.ToGRPCf(errdefs.ErrInvalidArgument,
			"cpu %d cannot be offlined (boot processor)", cpuID)
	}

	path := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/online", cpuID)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "cpu %d not present", cpuID)
		}
		return nil, errgrpc.ToGRPC(err)
	}

	// Check if already offline
	value, err := readSysfsValue(path)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	if value == sysfsOffline {
		return &api.OfflineCPUResponse{}, nil
	}

	// Offline the CPU
	if err := writeSysfsValue(path, sysfsOffline); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	log.G(ctx).WithField("cpu_id", cpuID).Debug("CPU offlined successfully")
	return &api.OfflineCPUResponse{}, nil
}

func (s *systemService) OnlineCPU(ctx context.Context, req *api.OnlineCPURequest) (*api.OnlineCPUResponse, error) {
	cpuID := req.GetCpuID()
	if cpuID == 0 {
		// CPU 0 is always online (boot processor)
		return &api.OnlineCPUResponse{}, nil
	}

	path := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/online", cpuID)

	// Wait for sysfs file to appear (kernel may need time after hotplug)
	if err := waitForSysfsFile(ctx, path, sysfsWaitTimeout); err != nil {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "cpu %d: %v", cpuID, err)
	}

	// Check if already online
	value, err := readSysfsValue(path)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	if value == sysfsOnline {
		return &api.OnlineCPUResponse{}, nil // Already online
	}

	// Write "1" to online the CPU
	if err := writeSysfsValue(path, sysfsOnline); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	log.G(ctx).WithField("cpu_id", cpuID).Debug("CPU onlined successfully")
	return &api.OnlineCPUResponse{}, nil
}

// writeRuntimeFeatures writes the runtime features to a well-known location
// that can be read by the shim manager
func (s *systemService) writeRuntimeFeatures() error {
	features := map[string]string{
		"containerd.io/runtime-allow-mounts": "mkdir/*,format/*,erofs,ext4",
		"containerd.io/runtime-type":         "vm",
		"containerd.io/vm-type":              "microvm",
	}

	featuresDir := "/run/vminitd"
	if err := os.MkdirAll(featuresDir, featuresDirPerms); err != nil {
		return err
	}

	data, err := json.Marshal(features)
	if err != nil {
		return err
	}

	featuresFile := filepath.Join(featuresDir, "features.json")
	return os.WriteFile(featuresFile, data, featuresFilePerms)
}

// RescanPCI re-enumerates the PCI bus and waits for the container's disks.
//
// It exists for restored VMs. A template is frozen with no container disks - it
// does not know which container it will become - so the guest inside it
// enumerated a bus without them. The disks are cold-plugged onto the restored
// VM's command line, and this is the host telling the guest to go and look.
func (s *systemService) RescanPCI(ctx context.Context, req *api.RescanPCIRequest) (*api.RescanPCIResponse, error) {
	found, err := devices.RescanPCI(ctx, int(req.GetExpectedBlockDevices()))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, errgrpc.ToGRPC(fmt.Errorf("%w: %w", errdefs.ErrUnavailable, err))
		}
		return nil, errgrpc.ToGRPC(err)
	}
	return &api.RescanPCIResponse{BlockDevices: found}, nil
}

// Configure gives this VM the identity it cannot read for itself.
//
// A VM restored from a template carries the template's kernel command line, and
// every line of it describes a different machine. This is the host saying who
// this VM actually is, over the channel that is already up 3 ms after the
// restore resumes.
func (s *systemService) Configure(ctx context.Context, req *api.ConfigureRequest) (*api.ConfigureResponse, error) {
	id := guestsystem.Identity{
		BlockDevices: int(req.GetExpectedBlockDevices()),
		ExtrasDisk:   req.GetExtrasDisk(),
		Restored:     req.GetRestored(),
	}
	if n := req.GetNetwork(); n != nil {
		id.Network = &guestsystem.NetworkIdentity{
			Device:        n.GetDevice(),
			MAC:           n.GetMac(),
			IP:            n.GetIp(),
			Netmask:       n.GetNetmask(),
			Gateway:       n.GetGateway(),
			DNS:           n.GetDns(),
			MetadataRoute: n.GetMetadataRoute(),
		}
	}

	if err := guestsystem.Apply(ctx, id); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, errgrpc.ToGRPC(fmt.Errorf("%w: %w", errdefs.ErrUnavailable, err))
		}
		return nil, errgrpc.ToGRPC(err)
	}
	return &api.ConfigureResponse{}, nil
}
