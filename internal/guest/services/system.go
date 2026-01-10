//go:build linux

package services

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	emptypb "google.golang.org/protobuf/types/known/emptypb"

	api "github.com/spin-stack/spinbox/api/services/system/v1"
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

type systemService struct{}

var _ api.TTRPCSystemService = &systemService{}

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
	api.RegisterTTRPCSystemService(server, s)
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

func (s *systemService) Info(ctx context.Context, _ *emptypb.Empty) (*api.InfoResponse, error) {
	v, err := os.ReadFile("/proc/version")
	if err != nil && !os.IsNotExist(err) {
		return nil, errgrpc.ToGRPC(err)
	}
	return &api.InfoResponse{
		Version:       "dev",
		KernelVersion: string(v),
	}, nil
}

func (s *systemService) OfflineCPU(ctx context.Context, req *api.OfflineCPURequest) (*emptypb.Empty, error) {
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
		return &emptypb.Empty{}, nil
	}

	// Offline the CPU
	if err := writeSysfsValue(path, sysfsOffline); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	log.G(ctx).WithField("cpu_id", cpuID).Debug("CPU offlined successfully")
	return &emptypb.Empty{}, nil
}

func (s *systemService) OnlineCPU(ctx context.Context, req *api.OnlineCPURequest) (*emptypb.Empty, error) {
	cpuID := req.GetCpuID()
	if cpuID == 0 {
		// CPU 0 is always online (boot processor)
		return &emptypb.Empty{}, nil
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
		return &emptypb.Empty{}, nil // Already online
	}

	// Write "1" to online the CPU
	if err := writeSysfsValue(path, sysfsOnline); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	log.G(ctx).WithField("cpu_id", cpuID).Debug("CPU onlined successfully")
	return &emptypb.Empty{}, nil
}

func (s *systemService) OfflineMemory(ctx context.Context, req *api.OfflineMemoryRequest) (*emptypb.Empty, error) {
	memoryID := req.GetMemoryID()

	// First, verify the memory block exists
	path := fmt.Sprintf("/sys/devices/system/memory/memory%d/online", memoryID)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "memory block %d not present", memoryID)
		}
		return nil, errgrpc.ToGRPC(err)
	}

	// Check if auto_online is enabled (if so, can't offline)
	if autoOnline, err := readSysfsValue("/sys/devices/system/memory/auto_online_blocks"); err == nil {
		if autoOnline == "online" {
			log.G(ctx).WithField("memory_id", memoryID).
				Debug("memory auto-online enabled, cannot offline blocks")
			return &emptypb.Empty{}, nil // Non-fatal
		}
	}

	// Check if already offline
	value, err := readSysfsValue(path)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	if value == sysfsOffline {
		return &emptypb.Empty{}, nil
	}

	// Check if block is removable
	removablePath := fmt.Sprintf("/sys/devices/system/memory/memory%d/removable", memoryID)
	if removable, err := readSysfsValue(removablePath); err == nil {
		if removable == sysfsOffline {
			log.G(ctx).WithField("memory_id", memoryID).
				Warn("memory block is not removable (kernel in use)")
			return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition,
				"memory block %d is not removable (kernel in use)", memoryID)
		}
	}

	// Offline the block
	if err := writeSysfsValue(path, sysfsOffline); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	log.G(ctx).WithField("memory_id", memoryID).Debug("memory block offlined successfully")
	return &emptypb.Empty{}, nil
}

func (s *systemService) OnlineMemory(ctx context.Context, req *api.OnlineMemoryRequest) (*emptypb.Empty, error) {
	memoryID := req.GetMemoryID()
	path := fmt.Sprintf("/sys/devices/system/memory/memory%d/online", memoryID)

	// Check auto_online setting
	if autoOnline, err := readSysfsValue("/sys/devices/system/memory/auto_online_blocks"); err == nil {
		if autoOnline == "online" {
			// Memory will auto-online, wait for it using inotify
			if err := waitForSysfsFile(ctx, path, sysfsWaitTimeout); err == nil {
				if value, err := readSysfsValue(path); err == nil && value == sysfsOnline {
					log.G(ctx).WithField("memory_id", memoryID).Debug("memory auto-onlined")
					return &emptypb.Empty{}, nil
				}
			}
		}
	}

	// Wait for sysfs file to appear (kernel may need time after hotplug)
	if err := waitForSysfsFile(ctx, path, sysfsWaitTimeout); err != nil {
		return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "memory block %d: %v", memoryID, err)
	}

	// Check if already online
	value, err := readSysfsValue(path)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	if value == sysfsOnline {
		return &emptypb.Empty{}, nil // Already online
	}

	// Write "1" to online the memory
	if err := writeSysfsValue(path, sysfsOnline); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	log.G(ctx).WithField("memory_id", memoryID).Debug("memory onlined successfully")
	return &emptypb.Empty{}, nil
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
