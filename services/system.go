package services

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	emptypb "google.golang.org/protobuf/types/known/emptypb"

	api "github.com/aledbf/qemubox/containerd/api/services/system/v1"
)

const (
	// TTRPCPlugin implements a ttrpc service
	TTRPCPlugin plugin.Type = "io.containerd.ttrpc.v1"
)

type systemService struct{}

var _ api.TTRPCSystemService = &systemService{}

func init() {
	registry.Register(&plugin.Registration{
		Type:   TTRPCPlugin,
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
		return nil, errgrpc.ToGRPCf(errdefs.ErrInvalidArgument, "cpu 0 cannot be offlined")
	}

	path := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/online", cpuID)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "cpu %d not present", cpuID)
		}
		return nil, errgrpc.ToGRPC(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	if strings.TrimSpace(string(data)) == "0" {
		return &emptypb.Empty{}, nil
	}

	if err := os.WriteFile(path, []byte("0"), 0644); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	return &emptypb.Empty{}, nil
}

func (s *systemService) OnlineCPU(ctx context.Context, req *api.OnlineCPURequest) (*emptypb.Empty, error) {
	cpuID := req.GetCpuID()
	if cpuID == 0 {
		// CPU 0 is always online (boot processor)
		return &emptypb.Empty{}, nil
	}

	path := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/online", cpuID)

	// Retry logic: kernel may need time to create sysfs files after hotplug
	// Wait up to 1 second with exponential backoff
	maxRetries := 10
	var lastErr error
	for retry := 0; retry < maxRetries; retry++ {
		if retry > 0 {
			// Exponential backoff: 10ms, 20ms, 40ms, 80ms, 160ms, 320ms (total ~1s)
			delay := time.Duration(10<<uint(retry-1)) * time.Millisecond
			time.Sleep(delay)
		}

		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				lastErr = err
				continue // Retry
			}
			return nil, errgrpc.ToGRPC(err)
		}

		// Check if already online
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, errgrpc.ToGRPC(err)
		}
		if strings.TrimSpace(string(data)) == "1" {
			if retry > 0 {
				log.G(ctx).WithFields(log.Fields{
					"cpu_id": cpuID,
					"retry":  retry,
				}).Debug("CPU already online (auto-onlined)")
			}
			return &emptypb.Empty{}, nil
		}

		// Write "1" to online the CPU
		if err := os.WriteFile(path, []byte("1"), 0644); err != nil {
			return nil, errgrpc.ToGRPC(err)
		}

		log.G(ctx).WithFields(log.Fields{
			"cpu_id": cpuID,
			"retry":  retry,
		}).Debug("CPU onlined successfully")

		return &emptypb.Empty{}, nil
	}

	// All retries exhausted
	return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "cpu %d not present after %d retries: %v", cpuID, maxRetries, lastErr)
}

func (s *systemService) OfflineMemory(ctx context.Context, req *api.OfflineMemoryRequest) (*emptypb.Empty, error) {
	memoryID := req.GetMemoryID()

	// Check if auto_online is enabled (if so, can't offline)
	autoOnlineData, err := os.ReadFile("/sys/devices/system/memory/auto_online_blocks")
	if err == nil {
		autoOnline := strings.TrimSpace(string(autoOnlineData))
		if autoOnline == "online" {
			log.G(ctx).WithField("memory_id", memoryID).
				Debug("memory auto-online enabled, cannot offline blocks")
			return &emptypb.Empty{}, nil // Non-fatal
		}
	}

	path := fmt.Sprintf("/sys/devices/system/memory/memory%d/online", memoryID)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound, "memory block %d not present", memoryID)
		}
		return nil, errgrpc.ToGRPC(err)
	}

	// Check if already offline
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	if strings.TrimSpace(string(data)) == "0" {
		return &emptypb.Empty{}, nil
	}

	// Check if block is removable
	removablePath := fmt.Sprintf("/sys/devices/system/memory/memory%d/removable", memoryID)
	if removableData, err := os.ReadFile(removablePath); err == nil {
		if strings.TrimSpace(string(removableData)) == "0" {
			log.G(ctx).WithField("memory_id", memoryID).
				Warn("memory block is not removable (kernel in use)")
			return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition,
				"memory block %d is not removable (kernel in use)", memoryID)
		}
	}

	// Offline the block
	if err := os.WriteFile(path, []byte("0"), 0644); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	log.G(ctx).WithField("memory_id", memoryID).Debug("memory block offlined successfully")

	return &emptypb.Empty{}, nil
}

func (s *systemService) OnlineMemory(ctx context.Context, req *api.OnlineMemoryRequest) (*emptypb.Empty, error) {
	memoryID := req.GetMemoryID()

	// Check auto_online setting
	autoOnlineData, err := os.ReadFile("/sys/devices/system/memory/auto_online_blocks")
	if err == nil {
		autoOnline := strings.TrimSpace(string(autoOnlineData))
		if autoOnline == "online" {
			// Memory will auto-online, just verify it happened
			time.Sleep(100 * time.Millisecond)
			path := fmt.Sprintf("/sys/devices/system/memory/memory%d/online", memoryID)
			if data, err := os.ReadFile(path); err == nil {
				if strings.TrimSpace(string(data)) == "1" {
					log.G(ctx).WithField("memory_id", memoryID).Debug("memory auto-onlined")
					return &emptypb.Empty{}, nil
				}
			}
		}
	}

	// Explicit online (similar to OnlineCPU with retry logic)
	path := fmt.Sprintf("/sys/devices/system/memory/memory%d/online", memoryID)

	// Retry logic: kernel may need time to create sysfs files after hotplug
	// Wait up to 1 second with exponential backoff
	maxRetries := 10
	var lastErr error
	for retry := 0; retry < maxRetries; retry++ {
		if retry > 0 {
			// Exponential backoff: 10ms, 20ms, 40ms, 80ms, 160ms, 320ms (total ~1s)
			delay := time.Duration(10<<uint(retry-1)) * time.Millisecond
			time.Sleep(delay)
		}

		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				lastErr = err
				continue // Retry
			}
			return nil, errgrpc.ToGRPC(err)
		}

		// Check if already online
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, errgrpc.ToGRPC(err)
		}
		if strings.TrimSpace(string(data)) == "1" {
			if retry > 0 {
				log.G(ctx).WithFields(log.Fields{
					"memory_id": memoryID,
					"retry":     retry,
				}).Debug("memory already online (auto-onlined)")
			}
			return &emptypb.Empty{}, nil
		}

		// Write "1" to online the memory
		if err := os.WriteFile(path, []byte("1"), 0644); err != nil {
			return nil, errgrpc.ToGRPC(err)
		}

		log.G(ctx).WithFields(log.Fields{
			"memory_id": memoryID,
			"retry":     retry,
		}).Debug("memory onlined successfully")

		return &emptypb.Empty{}, nil
	}

	// All retries exhausted
	return nil, errgrpc.ToGRPCf(errdefs.ErrNotFound,
		"memory block %d not present after %d retries: %v", memoryID, maxRetries, lastErr)
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
	if err := os.MkdirAll(featuresDir, 0750); err != nil {
		return err
	}

	data, err := json.Marshal(features)
	if err != nil {
		return err
	}

	featuresFile := filepath.Join(featuresDir, "features.json")
	return os.WriteFile(featuresFile, data, 0600)
}
