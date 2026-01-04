package task

import (
	"context"
	"sync"
	"time"

	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
)

// ConnectionManager provides centralized management of the TTRPC client connection
// to the VM. It implements connection caching, lazy initialization, and automatic
// reconnection to minimize vsock dial attempts.
//
// The vsock layer can return ENODEV when establishing new connections due to timing
// issues with CID assignment or VM vsock device initialization. By reusing a single
// cached connection for all task RPCs, we avoid triggering these transient errors.
//
// Thread-safe for concurrent access.
type ConnectionManager struct {
	mu     sync.RWMutex
	client *ttrpc.Client

	// dialFn creates a new TTRPC client connection (single attempt).
	dialFn func(context.Context) (*ttrpc.Client, error)

	// dialRetryFn creates a new TTRPC client with retry logic for transient errors.
	dialRetryFn func(context.Context, time.Duration) (*ttrpc.Client, error)

	// closed indicates the manager has been shut down.
	closed bool
}

// NewConnectionManager creates a connection manager with the given dial functions.
//
// dialFn: Single-attempt dial (used when we expect connection to succeed immediately)
// dialRetryFn: Dial with retries (used when reconnecting after failures)
func NewConnectionManager(
	dialFn func(context.Context) (*ttrpc.Client, error),
	dialRetryFn func(context.Context, time.Duration) (*ttrpc.Client, error),
) *ConnectionManager {
	return &ConnectionManager{
		dialFn:      dialFn,
		dialRetryFn: dialRetryFn,
	}
}

// GetClient returns a cached or newly-created TTRPC client.
//
// If a cached client exists, it is returned immediately. Otherwise, a new
// connection is established using the retry dial function.
//
// The returned client is owned by the ConnectionManager - callers must NOT close it.
// Use Close() to shut down the connection when done.
func (m *ConnectionManager) GetClient(ctx context.Context) (*ttrpc.Client, error) {
	// Fast path: check for existing client with read lock
	m.mu.RLock()
	if m.client != nil && !m.closed {
		client := m.client
		m.mu.RUnlock()
		return client, nil
	}
	m.mu.RUnlock()

	return m.getOrCreateClient(ctx)
}

// getOrCreateClient establishes a new connection if needed.
// Must be called when read lock check fails.
func (m *ConnectionManager) getOrCreateClient(ctx context.Context) (*ttrpc.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock (another goroutine may have created it)
	if m.closed {
		return nil, context.Canceled
	}
	if m.client != nil {
		return m.client, nil
	}

	log.G(ctx).Debug("connmanager: dialing new task client")
	client, err := m.dialRetryFn(ctx, taskClientRetryTimeout)
	if err != nil {
		log.G(ctx).WithError(err).Error("connmanager: failed to dial task client")
		return nil, err
	}

	log.G(ctx).Debug("connmanager: task client connected")
	m.client = client
	return client, nil
}

// SetClient stores an externally-created client.
//
// This is used during Create() when the initial connection is established
// as part of the VM boot sequence. The manager takes ownership of the client.
//
// If a client already exists, the old one is closed and replaced.
func (m *ConnectionManager) SetClient(client *ttrpc.Client) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		// Manager is closed, don't accept new clients
		if client != nil {
			client.Close()
		}
		return
	}

	// Close existing client if different
	if m.client != nil && m.client != client {
		log.L.Debug("connmanager: replacing existing client")
		m.client.Close()
	}

	m.client = client
	log.L.Debug("connmanager: client set")
}

// ClearClient removes the cached client without closing it.
//
// This is used when the caller needs to take ownership of the client
// for special handling (e.g., when Create fails and we want to close
// the client in a specific way).
//
// Returns the client that was cleared, or nil if none was cached.
func (m *ConnectionManager) ClearClient() *ttrpc.Client {
	m.mu.Lock()
	defer m.mu.Unlock()

	client := m.client
	m.client = nil
	return client
}

// Invalidate clears the cached client, forcing the next GetClient call
// to establish a new connection. This should be called when the cached
// connection is detected to be stale (e.g., write errors).
//
// The old client is closed before being cleared.
func (m *ConnectionManager) Invalidate(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.client != nil {
		log.G(ctx).Debug("connmanager: invalidating stale client")
		m.client.Close()
		m.client = nil
	}
}

// Close shuts down the connection manager and closes the cached client.
//
// After Close is called, GetClient will return an error.
// Safe to call multiple times.
func (m *ConnectionManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}
	m.closed = true

	if m.client != nil {
		log.L.Debug("connmanager: closing task client")
		err := m.client.Close()
		m.client = nil
		return err
	}

	return nil
}

// HasClient returns true if a client is currently cached.
// Useful for debugging and testing.
func (m *ConnectionManager) HasClient() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.client != nil && !m.closed
}
