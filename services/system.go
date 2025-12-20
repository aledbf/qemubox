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

	api "github.com/aledbf/beacon/containerd/api/services/system/v1"
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
