// Package resources handles VM resource configuration and calculation.
// It extracts resource requirements from OCI specs and computes VM resource limits.
package resources

import (
	"bufio"
	"context"
	"fmt"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"

	"github.com/containerd/log"
	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/aledbf/qemubox/containerd/internal/host/vm"
)

// ConfigInfo provides additional context about resource configuration decisions.
type ConfigInfo struct {
	HasExplicitCPULimit    bool
	HasExplicitMemoryLimit bool
	HostCPUs               int
	HostMemory             int64
}

// ComputeConfig calculates VM resource configuration from an OCI spec.
// It returns the resource config and additional context about the decisions made.
func ComputeConfig(ctx context.Context, spec *specs.Spec) (*vm.VMResourceConfig, ConfigInfo) {
	// Extract resource requests from OCI spec
	cpuRequest := extractCPURequest(spec)
	memoryRequest := extractMemoryRequest(spec)

	// Get host resource limits
	hostCPUs := getHostCPUCount()
	hostMemory, err := getHostMemoryTotal()
	if err != nil {
		// Can't determine host memory - use conservative default
		// 4GB is reasonable for most scenarios and won't cause OOM
		const conservativeDefault = 4 * 1024 * 1024 * 1024
		log.G(ctx).WithError(err).WithField("default_gb", 4).
			Error("failed to get host memory total, using conservative 4GB default")
		hostMemory = conservativeDefault
	}

	// Align memory values to 128MB for virtio-mem requirement
	const virtioMemAlignment = 128 * 1024 * 1024 // 128MB
	memoryRequest = alignMemory(memoryRequest, virtioMemAlignment)
	hostMemory = alignMemory(hostMemory, virtioMemAlignment)

	// Calculate smart resource limits for better overcommit:
	// - If container has explicit CPU limit, use that for MaxCPUs (capped at host)
	// - If no limit, allow access to all host CPUs for maximum flexibility
	// - If container has explicit memory limit, use 2x for hotplug headroom (capped at host)
	// - If no limit, allow access to all host memory for maximum flexibility
	maxCPUs := hostCPUs
	memoryHotplugSize := hostMemory

	// Check if explicit limits were set (vs defaults)
	hasExplicitCPULimit := spec.Linux != nil &&
		spec.Linux.Resources != nil &&
		spec.Linux.Resources.CPU != nil &&
		(spec.Linux.Resources.CPU.Quota != nil || spec.Linux.Resources.CPU.Cpus != "")

	hasExplicitMemoryLimit := spec.Linux != nil &&
		spec.Linux.Resources != nil &&
		spec.Linux.Resources.Memory != nil &&
		spec.Linux.Resources.Memory.Limit != nil

	if hasExplicitCPULimit {
		// Container has explicit CPU limit - cap MaxCPUs to the request
		// This prevents wasting CPU scheduling slots on containers that don't need them
		maxCPUs = min(cpuRequest, hostCPUs)
	}

	if hasExplicitMemoryLimit {
		// Container has explicit memory limit - set hotplug to 2x for headroom
		// This allows some burst capacity while preventing unlimited growth
		memoryHotplugSize = min(memoryRequest*2, hostMemory)
		memoryHotplugSize = alignMemory(memoryHotplugSize, virtioMemAlignment)
	}

	resourceCfg := &vm.VMResourceConfig{
		BootCPUs:          cpuRequest,
		MaxCPUs:           maxCPUs,
		MemorySize:        memoryRequest,
		MemoryHotplugSize: memoryHotplugSize,
	}

	info := ConfigInfo{
		HasExplicitCPULimit:    hasExplicitCPULimit,
		HasExplicitMemoryLimit: hasExplicitMemoryLimit,
		HostCPUs:               hostCPUs,
		HostMemory:             hostMemory,
	}

	return resourceCfg, info
}

// extractCPURequest extracts the CPU request from the OCI spec.
// Returns the number of vCPUs requested, defaulting to 1 if not specified.
func extractCPURequest(spec *specs.Spec) int {
	if spec == nil || spec.Linux == nil || spec.Linux.Resources == nil || spec.Linux.Resources.CPU == nil {
		return 1 // Default to 1 vCPU (improved from 2 for better overcommit)
	}

	cpu := spec.Linux.Resources.CPU

	// CPU.Quota and CPU.Period define CPU limits in microseconds
	// For example: Quota=200000, Period=100000 means 2 CPUs (200000/100000 = 2)
	if cpu.Quota != nil && cpu.Period != nil && *cpu.Period > 0 {
		cpus := int(*cpu.Quota / int64(*cpu.Period))
		if cpus > 0 {
			return cpus
		}
		// If quota is set but results in <1 CPU, give it 1 vCPU
		// (fractional CPU will be enforced by cgroups within the VM)
		return 1
	}

	// Fallback: check CPU.Cpus (cpuset format like "0-3" or "0,1,2,3")
	// This is less common but may be present
	if cpu.Cpus != "" {
		// Simple heuristic: count commas + 1, or parse ranges
		// For now, just return 1 as this requires more complex parsing
		return 1
	}

	return 1 // Default to 1 vCPU
}

// extractMemoryRequest extracts the memory request from the OCI spec.
// Returns the memory limit in bytes, defaulting to 512MB if not specified.
func extractMemoryRequest(spec *specs.Spec) int64 {
	const defaultMemory = 512 * 1024 * 1024 // 512MB default

	if spec == nil || spec.Linux == nil || spec.Linux.Resources == nil || spec.Linux.Resources.Memory == nil {
		return defaultMemory
	}

	mem := spec.Linux.Resources.Memory

	// Memory.Limit defines the memory limit in bytes
	if mem.Limit != nil && *mem.Limit > 0 {
		return *mem.Limit
	}

	return defaultMemory
}

// alignMemory rounds up the given memory value to the nearest multiple of alignment.
// This is required for virtio-mem which needs memory sizes aligned to 128MB.
func alignMemory(memory, alignment int64) int64 {
	if memory%alignment == 0 {
		return memory
	}
	return ((memory / alignment) + 1) * alignment
}

// getHostCPUCount returns the total number of CPUs available on the host.
func getHostCPUCount() int {
	return goruntime.NumCPU()
}

// getHostMemoryTotal returns the total physical memory available on the host in bytes.
// Reads from /proc/meminfo on Linux.
func getHostMemoryTotal() (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("failed to open /proc/meminfo: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.L.WithError(err).Warn("failed to close /proc/meminfo")
		}
	}()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			// Format: "MemTotal:       16384000 kB"
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseInt(fields[1], 10, 64)
				if err != nil {
					return 0, fmt.Errorf("failed to parse MemTotal value: %w", err)
				}
				// Convert from KB to bytes
				return kb * 1024, nil
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("error reading /proc/meminfo: %w", err)
	}

	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}
