//go:build linux

package runc

import (
	"context"

	cgroupsv2 "github.com/containerd/cgroups/v3/cgroup2"
	"github.com/containerd/cgroups/v3/cgroup2/stats"
	"github.com/containerd/log"
	"github.com/moby/sys/userns"
)

// CgroupManager abstracts cgroup v2 operations.
// This interface provides a clean API boundary between the task service
// and cgroup v2 implementation, enabling testing via mocks.
//
// Note: Only cgroup v2 (unified mode) is supported. The vminit service
// explicitly rejects non-unified cgroup modes at startup.
type CgroupManager interface {
	// Stats returns cgroup v2 statistics
	Stats(ctx context.Context) (*stats.Metrics, error)

	// EnableControllers enables all available cgroup controllers
	EnableControllers(ctx context.Context) error
}

// cgroupManager implements CgroupManager for cgroup v2
type cgroupManager struct {
	manager *cgroupsv2.Manager
}

// NewCgroupManager creates a new cgroup v2 manager
func NewCgroupManager(mgr *cgroupsv2.Manager) CgroupManager {
	return &cgroupManager{manager: mgr}
}

func (m *cgroupManager) Stats(ctx context.Context) (*stats.Metrics, error) {
	return m.manager.Stat()
}

func (m *cgroupManager) EnableControllers(ctx context.Context) error {
	allControllers, err := m.manager.RootControllers()
	if err != nil {
		log.G(ctx).WithError(err).Error("failed to get root controllers")
		return err
	}

	if err := m.manager.ToggleControllers(allControllers, cgroupsv2.Enable); err != nil {
		if userns.RunningInUserNS() {
			log.G(ctx).WithError(err).Debugf("failed to enable controllers (%v)", allControllers)
		} else {
			log.G(ctx).WithError(err).Errorf("failed to enable controllers (%v)", allControllers)
		}
		return err
	}

	return nil
}

// LoadProcessCgroup loads the cgroup for a given PID and returns a CgroupManager.
// Only cgroup v2 (unified mode) is supported.
func LoadProcessCgroup(ctx context.Context, pid int) (CgroupManager, error) {
	g, err := cgroupsv2.PidGroupPath(pid)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("loading cgroup2 for %d", pid)
		return nil, err
	}

	mgr, err := cgroupsv2.Load(g)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("loading cgroup2 for %d", pid)
		return nil, err
	}

	return NewCgroupManager(mgr), nil
}
