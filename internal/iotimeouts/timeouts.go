// Package iotimeouts defines coordinated timeout values for I/O operations.
//
// These timeouts are used across the shim (host) and vminit (guest) components
// to ensure proper coordination during process exit. Changing any of these values
// requires understanding the relationship between them.
//
// Timeout Hierarchy (from inner to outer):
//
//	Guest (vminit):
//	  subscriberWaitTimeout = 10s   // Wait for RPC streams to finish
//	                                // after fanOutReaders complete
//
//	Host (shim):
//	  ioWaitTimeout = 30s           // Wait for forwarder.WaitForComplete()
//	                                // Must be > subscriberWaitTimeout + network latency
//
// Data Flow on Process Exit:
//
//  1. Process exits in guest
//  2. fanOutReaders detect EOF, send to subscriber channels
//  3. Guest waits up to subscriberWaitTimeout for RPC streams to drain
//  4. Guest sends TaskExit event
//  5. Host receives TaskExit, waits up to ioWaitTimeout for forwarder
//  6. Host forwards TaskExit to containerd
//
// If the guest timeout fires first, the host will see the forwarder complete
// quickly (the RPC streams will have ended). The host timeout is a safety net
// for cases where the forwarder itself is stuck.
package iotimeouts

import "time"

const (
	// SubscriberWaitTimeout is the maximum time the guest waits for subscriber
	// RPC streams to finish after the process exits.
	//
	// This prevents WaitForIOComplete from blocking indefinitely if a subscriber
	// fails to call its done() function. Properly behaved subscribers should
	// complete quickly. If this timeout fires, investigate why subscribers aren't
	// signaling completion (likely a bug in the RPC stream handling).
	//
	// Used in: internal/guest/vminit/stdio/manager.go
	SubscriberWaitTimeout = 10 * time.Second

	// HostIOWaitTimeout is the maximum time the host shim waits for the I/O
	// forwarder to complete before forwarding TaskExit events.
	//
	// This must be greater than SubscriberWaitTimeout to account for:
	//   - Guest subscriber timeout (10s)
	//   - Network latency for RPC completion signal (~1s worst case)
	//   - Host-side FIFO flush time (~1s worst case)
	//   - Safety margin
	//
	// If this timeout fires, the exit event is forwarded anyway - it's better
	// to deliver the exit with potentially missing output than to block forever.
	//
	// Used in: internal/shim/task/service.go
	HostIOWaitTimeout = 30 * time.Second

	// SubscriberChannelBuffer is the buffer size for subscriber output channels.
	//
	// This buffer provides slack between the fan-out goroutine (which reads from
	// the process) and the RPC stream sender. Without buffering, a slow network
	// would block the fan-out, potentially causing the process to block on write().
	//
	// 64 chunks at ~32KB max per chunk = ~2MB theoretical max before blocking.
	// In practice, most chunks are much smaller. This handles typical burst
	// scenarios like a process dumping a stack trace or printing a large JSON blob.
	//
	// Used in: internal/guest/vminit/stdio/manager.go
	SubscriberChannelBuffer = 64

	// MaxBufferedBytes is the maximum bytes to buffer per stream for late subscribers.
	//
	// When no subscriber is attached, we buffer output so late subscribers
	// (e.g., `ctr attach` after container start) can see recent output. This is a
	// bounded ring buffer - older data is discarded when the limit is exceeded.
	//
	// 256KB is chosen to capture typical startup output (logs, banners, errors)
	// without consuming excessive memory per process. Long-running processes that
	// emit lots of output will lose old data, which is acceptable - this is for
	// convenience, not guaranteed delivery.
	//
	// Used in: internal/guest/vminit/stdio/manager.go
	MaxBufferedBytes = 256 * 1024

	// OutputRetryInitialDelay is the initial delay for output forwarding retry.
	//
	// Used in: internal/shim/task/rpcio.go
	OutputRetryInitialDelay = 100 * time.Millisecond

	// OutputRetryMaxDelay is the maximum delay between output forwarding retries.
	//
	// Used in: internal/shim/task/rpcio.go
	OutputRetryMaxDelay = 2 * time.Second
)
