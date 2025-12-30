package resources

import (
	"context"
	"fmt"

	cgroup2stats "github.com/containerd/cgroups/v3/cgroup2/stats"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	"github.com/containerd/typeurl/v2"

	systemAPI "github.com/aledbf/qemubox/containerd/api/services/system/v1"
)

// closeClient closes a TTRPC client and logs any errors.
func closeClient(ctx context.Context, client *ttrpc.Client, operation string) {
	if err := client.Close(); err != nil {
		log.G(ctx).WithError(err).WithField("operation", operation).
			Warn("failed to close TTRPC client")
	}
}

// getCPUStats retrieves CPU usage statistics from the container via TTRPC.
func getCPUStats(ctx context.Context, dialClient func(context.Context) (*ttrpc.Client, error), containerID string) (uint64, uint64, error) {
	vmc, err := dialClient(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer closeClient(ctx, vmc, "CPU stats")
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	resp, err := tc.Stats(ctx, &taskAPI.StatsRequest{ID: containerID})
	if err != nil {
		return 0, 0, err
	}
	if resp.GetStats() == nil {
		return 0, 0, fmt.Errorf("container %s: missing CPU stats payload", containerID)
	}

	var metrics cgroup2stats.Metrics
	if err := typeurl.UnmarshalTo(resp.Stats, &metrics); err != nil {
		return 0, 0, fmt.Errorf("container %s: failed to unmarshal stats: %w", containerID, err)
	}

	cpu := metrics.GetCPU()
	if cpu == nil {
		return 0, 0, fmt.Errorf("container %s: missing CPU stats in metrics", containerID)
	}

	return cpu.GetUsageUsec(), cpu.GetThrottledUsec(), nil
}

// offlineCPU takes a CPU offline in the guest VM.
func offlineCPU(ctx context.Context, dialClient func(context.Context) (*ttrpc.Client, error), cpuID int) error {
	vmc, err := dialClient(ctx)
	if err != nil {
		return err
	}
	defer closeClient(ctx, vmc, "CPU offline")
	client := systemAPI.NewTTRPCSystemClient(vmc)
	_, err = client.OfflineCPU(ctx, &systemAPI.OfflineCPURequest{CpuID: uint32(cpuID)})
	return err
}

// onlineCPU brings a CPU online in the guest VM.
func onlineCPU(ctx context.Context, dialClient func(context.Context) (*ttrpc.Client, error), cpuID int) error {
	vmc, err := dialClient(ctx)
	if err != nil {
		return err
	}
	defer closeClient(ctx, vmc, "CPU online")
	client := systemAPI.NewTTRPCSystemClient(vmc)
	_, err = client.OnlineCPU(ctx, &systemAPI.OnlineCPURequest{CpuID: uint32(cpuID)})
	return err
}

// getMemoryStats retrieves memory usage statistics from the container via TTRPC.
func getMemoryStats(ctx context.Context, dialClient func(context.Context) (*ttrpc.Client, error), containerID string) (int64, error) {
	vmc, err := dialClient(ctx)
	if err != nil {
		return 0, err
	}
	defer closeClient(ctx, vmc, "memory stats")
	tc := taskAPI.NewTTRPCTaskClient(vmc)
	resp, err := tc.Stats(ctx, &taskAPI.StatsRequest{ID: containerID})
	if err != nil {
		return 0, err
	}
	if resp.GetStats() == nil {
		return 0, fmt.Errorf("container %s: missing memory stats payload", containerID)
	}

	var metrics cgroup2stats.Metrics
	if err := typeurl.UnmarshalTo(resp.Stats, &metrics); err != nil {
		return 0, fmt.Errorf("container %s: failed to unmarshal stats: %w", containerID, err)
	}

	mem := metrics.GetMemory()
	if mem == nil {
		return 0, fmt.Errorf("container %s: missing memory stats in metrics", containerID)
	}

	return int64(mem.GetUsage()), nil
}

// offlineMemory takes memory offline in the guest VM.
func offlineMemory(ctx context.Context, dialClient func(context.Context) (*ttrpc.Client, error), memoryID int) error {
	vmc, err := dialClient(ctx)
	if err != nil {
		return err
	}
	defer closeClient(ctx, vmc, "memory offline")
	client := systemAPI.NewTTRPCSystemClient(vmc)
	_, err = client.OfflineMemory(ctx, &systemAPI.OfflineMemoryRequest{MemoryID: uint32(memoryID)})
	return err
}

// onlineMemory brings memory online in the guest VM.
func onlineMemory(ctx context.Context, dialClient func(context.Context) (*ttrpc.Client, error), memoryID int) error {
	vmc, err := dialClient(ctx)
	if err != nil {
		return err
	}
	defer closeClient(ctx, vmc, "memory online")
	client := systemAPI.NewTTRPCSystemClient(vmc)
	_, err = client.OnlineMemory(ctx, &systemAPI.OnlineMemoryRequest{MemoryID: uint32(memoryID)})
	return err
}
