//go:build linux

package qemu

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/mdlayher/vsock"
)

func (q *Instance) StartStream(ctx context.Context) (uint32, net.Conn, error) {
	if q.getState() != vmStateRunning {
		return 0, nil, fmt.Errorf("vm not running: %w", errdefs.ErrFailedPrecondition)
	}
	const timeIncrement = 10 * time.Millisecond
	for d := timeIncrement; d < time.Second; d += timeIncrement {
		// Generate unique stream ID
		sid := atomic.AddUint32(&q.streamC, 1)
		if sid == 0 {
			return 0, nil, fmt.Errorf("exhausted stream identifiers: %w", errdefs.ErrUnavailable)
		}

		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		default:
		}

		// Connect directly via vsock stream port
		conn, err := vsock.Dial(vsockCID, vsockStreamPort, nil)
		if err == nil {
			// Send stream ID to vminitd (4 bytes, big-endian)
			var vs [4]byte
			binary.BigEndian.PutUint32(vs[:], sid)
			if _, err := conn.Write(vs[:]); err != nil {
				_ = conn.Close()
				return 0, nil, fmt.Errorf("failed to write stream id: %w", err)
			}

			// Wait for stream ID acknowledgment from vminitd
			var streamAck [4]byte
			if _, err := io.ReadFull(conn, streamAck[:]); err != nil {
				_ = conn.Close()
				return 0, nil, fmt.Errorf("failed to read stream ack: %w", err)
			}

			if binary.BigEndian.Uint32(streamAck[:]) != sid {
				_ = conn.Close()
				return 0, nil, fmt.Errorf("stream ack mismatch")
			}

			return sid, conn, nil
		}

		time.Sleep(timeIncrement)
	}

	return 0, nil, fmt.Errorf("timeout waiting for stream server: %w", errdefs.ErrUnavailable)
}

// connectVsockRPC establishes a connection to the vsock RPC server (vminitd)
func (q *Instance) connectVsockRPC(ctx context.Context) (net.Conn, error) {
	log.G(ctx).WithFields(log.Fields{
		"cid":  vsockCID,
		"port": vsockRPCPort,
	}).Info("qemu: connecting to vsock RPC port")

	// Wait a bit for vminitd to fully initialize
	time.Sleep(500 * time.Millisecond)

	retryStart := time.Now()
	pingDeadline := 50 * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if time.Since(retryStart) > connectRetryTimeout {
			return nil, fmt.Errorf("timeout waiting for vminitd to accept connections")
		}

		// Connect directly via vsock using kernel's vhost-vsock driver
		conn, err := vsock.Dial(vsockCID, vsockRPCPort, nil)
		if err != nil {
			log.G(ctx).WithError(err).Debug("qemu: failed to dial vsock")
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// Try to ping the TTRPC server with a deadline
		if err := conn.SetReadDeadline(time.Now().Add(pingDeadline)); err != nil {
			log.G(ctx).WithError(err).Debug("qemu: failed to set ping deadline")
			_ = conn.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err := pingTTRPC(conn); err != nil {
			log.G(ctx).WithError(err).WithField("deadline", pingDeadline).Debug("qemu: TTRPC ping failed, retrying")
			_ = conn.Close()
			pingDeadline += 10 * time.Millisecond
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// Clear the deadline and verify connection is still alive
		if err := conn.SetReadDeadline(time.Time{}); err != nil {
			log.G(ctx).WithError(err).Debug("qemu: failed to clear ping deadline")
			_ = conn.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err := pingTTRPC(conn); err != nil {
			log.G(ctx).WithError(err).Debug("qemu: TTRPC ping failed after clearing deadline, retrying")
			_ = conn.Close()
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// Connection is ready
		log.G(ctx).WithField("retry_time", time.Since(retryStart)).Info("qemu: TTRPC connection established")
		return conn, nil
	}
}

// monitorGuestRPC periodically checks if the in-guest vminitd RPC server is reachable.
// If the server disappears (e.g., guest reboot/poweroff), log a warning for debugging.
// Shutdown() is responsible for coordinating all shutdown actions.
func (q *Instance) monitorGuestRPC(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()

	failures := 0
	for {
		if q.getState() == vmStateShutdown {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		conn, err := vsock.Dial(vsockCID, vsockRPCPort, nil)
		if err == nil {
			if err := conn.SetDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
				log.G(ctx).WithError(err).Debug("qemu: failed to set guest RPC deadline")
				_ = conn.Close()
				continue
			}
			if err := pingTTRPC(conn); err != nil {
				failures++
				log.G(ctx).WithError(err).WithField("failures", failures).Debug("qemu: guest RPC ping failed")
			} else {
				failures = 0
			}
			_ = conn.Close()
		} else {
			failures++
			log.G(ctx).WithError(err).WithField("failures", failures).Debug("qemu: guest RPC dial failed")
		}

		// Log when guest becomes unreachable (may indicate reboot or hang)
		if failures >= 2 {
			log.G(ctx).WithField("failures", failures).Warning("qemu: guest RPC unreachable for 1 second (may be rebooting or hung)")
			// Don't force quit - Shutdown() will handle timeouts
		}
	}
}

// Helper functions

// openTAPInNetNS opens a TAP device in the specified network namespace and returns
// its file descriptor. This allows QEMU (running in init netns for vhost-vsock) to
// attach to TAP devices that live in sandbox namespaces.
//
// This approach is inspired by Kata Containers and is cleaner than moving TAPs between
// namespaces: file descriptors are namespace-agnostic, so once opened, the FD can be
// used from any namespace.
