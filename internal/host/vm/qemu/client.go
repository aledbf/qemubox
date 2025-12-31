//go:build linux

package qemu

import (
	"context"
	"fmt"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	"github.com/mdlayher/vsock"

	"github.com/aledbf/qemubox/containerd/internal/host/vm"
)

func (q *Instance) Client() (*ttrpc.Client, error) {
	if q.getState() != vmStateRunning {
		return nil, fmt.Errorf("vm not running: %w", errdefs.ErrFailedPrecondition)
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	return q.client, nil
}

// DialClient creates a short-lived TTRPC client for one-off RPCs.
// The caller must close the returned client.
func (q *Instance) DialClient(ctx context.Context) (*ttrpc.Client, error) {
	if q.getState() != vmStateRunning {
		return nil, fmt.Errorf("vm not running: %w", errdefs.ErrFailedPrecondition)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	conn, err := vsock.Dial(vsockCID, vsockRPCPort, nil)
	if err != nil {
		return nil, err
	}
	log.G(ctx).Debug("qemu: vsock dialed for TTRPC")

	if err := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := pingTTRPC(conn); err != nil {
		log.G(ctx).WithError(err).Debug("qemu: TTRPC ping failed")
		_ = conn.Close()
		return nil, err
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}

	log.G(ctx).Debug("qemu: TTRPC ping ok, client ready")
	return ttrpc.NewClient(conn), nil
}

// QMPClient returns the QMP client for controlling the VM
func (q *Instance) QMPClient() *qmpClient {
	// Return nil if VM is shutdown
	if q.getState() == vmStateShutdown {
		return nil
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	return q.qmpClient
}

// CPUHotplugger returns an interface for CPU hotplug operations
func (q *Instance) CPUHotplugger() (vm.CPUHotplugger, error) {
	if q.getState() == vmStateShutdown {
		return nil, fmt.Errorf("vm shutdown: %w", errdefs.ErrFailedPrecondition)
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	return q.qmpClient, nil
}
