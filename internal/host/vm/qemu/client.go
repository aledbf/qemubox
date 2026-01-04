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
	vsockports "github.com/aledbf/qemubox/containerd/internal/vsock"
)

func (q *Instance) Client() (*ttrpc.Client, error) {
	if q.getState() != vmStateRunning {
		return nil, fmt.Errorf("vm not running: %w", errdefs.ErrFailedPrecondition)
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	return q.client, nil
}

// dialTimeout is the maximum time to wait for a vsock dial to complete.
// This prevents indefinite blocking if the kernel vsock layer is stuck.
const dialTimeout = 2 * time.Second

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

	// vsock.Dial doesn't respect context, so we wrap it with a timeout.
	// This prevents indefinite blocking if the kernel vsock layer is stuck.
	type dialResult struct {
		conn *vsock.Conn
		err  error
	}
	resultCh := make(chan dialResult, 1)
	go func() {
		conn, err := vsock.Dial(q.guestCID, vsockports.DefaultRPCPort, nil)
		resultCh <- dialResult{conn, err}
	}()

	var conn *vsock.Conn
	select {
	case <-ctx.Done():
		// Context canceled - the dial goroutine will complete eventually
		// and the connection (if any) will be garbage collected
		return nil, ctx.Err()
	case <-time.After(dialTimeout):
		return nil, fmt.Errorf("vsock dial timeout after %v", dialTimeout)
	case result := <-resultCh:
		if result.err != nil {
			return nil, result.err
		}
		conn = result.conn
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
