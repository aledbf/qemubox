//go:build linux

package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/log"
	qmpapi "github.com/digitalocean/go-qemu/qmp"
)

// qmpClient implements QEMU Machine Protocol (QMP) client.
// QMP is a JSON-RPC protocol for controlling QEMU via a Unix socket.
//
// Thread safety: qmpClient is safe for concurrent use. Commands are serialized
// via the underlying SocketMonitor, and the closed flag uses atomic operations.
//
// Lifecycle: Create with newQMPClient(), use for commands, close with Close().
// The eventLoop goroutine runs until Close() is called or context is cancelled.
type qmpClient struct {
	monitor *qmpapi.SocketMonitor
	events  <-chan qmpapi.Event

	mu             sync.Mutex
	closed         atomic.Bool
	commandTimeout time.Duration // Timeout for QMP commands (default: 5 seconds)

	// eventLoopDone is closed when the eventLoop goroutine exits.
	// This allows Close() to wait for proper cleanup.
	eventLoopDone chan struct{}
}

type qmpResponse struct {
	Return any             `json:"return,omitempty"`
	Error  *qmpError       `json:"error,omitempty"`
	ID     json.RawMessage `json:"id,omitempty"`
	Event  string          `json:"event,omitempty"`
	Data   map[string]any  `json:"data,omitempty"`
}

type qmpError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

// qmpStatus matches the response from the query-status command.
type qmpStatus struct {
	Status     string `json:"status"`
	Singlestep bool   `json:"singlestep"`
	Running    bool   `json:"running"`
}

// SetCommandTimeout sets the timeout for QMP commands.
// If not set or set to 0, defaults to 5 seconds.
func (q *qmpClient) SetCommandTimeout(timeout time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.commandTimeout = timeout
}

// newQMPClient creates a QMP client and performs initial handshake.
// The QMP protocol requires:
// 1. Read greeting message from server
// 2. Send qmp_capabilities command to enter command mode
// 3. Start event loop for asynchronous events
//
// The returned client owns a background goroutine (eventLoop) that must be
// cleaned up by calling Close().
func newQMPClient(ctx context.Context, socketPath string) (*qmpClient, error) {
	// Wait for socket to appear
	if err := waitForSocket(ctx, socketPath, vmStartTimeout); err != nil {
		return nil, fmt.Errorf("QMP socket not available: %w", err)
	}

	monitor, err := qmpapi.NewSocketMonitor("unix", socketPath, qmpDefaultTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to QMP socket: %w", err)
	}

	if err := monitor.Connect(); err != nil {
		_ = monitor.Disconnect()
		return nil, fmt.Errorf("failed to negotiate QMP capabilities: %w", err)
	}

	log.G(ctx).WithFields(log.Fields{
		"major": monitor.Version.QEMU.Major,
		"minor": monitor.Version.QEMU.Minor,
		"micro": monitor.Version.QEMU.Micro,
	}).Debug("qemu: connected to QMP")

	eventCtx := context.WithoutCancel(ctx)
	events, err := monitor.Events(eventCtx)
	if err != nil && !errors.Is(err, qmpapi.ErrEventsNotSupported) {
		_ = monitor.Disconnect()
		return nil, fmt.Errorf("failed to subscribe to QMP events: %w", err)
	}

	qmp := &qmpClient{
		monitor:        monitor,
		events:         events,
		commandTimeout: qmpDefaultTimeout,
		eventLoopDone:  make(chan struct{}),
	}

	// Start event loop in background AFTER connecting
	go qmp.eventLoop(eventCtx)

	return qmp, nil
}

// execute sends a QMP command and waits for response.
// Returns the response and any error. Most callers ignore the response.
func (q *qmpClient) execute(ctx context.Context, command string, args map[string]any) (*qmpResponse, error) {
	return q.sendCommand(ctx, command, args)
}

// qmpQuery is a generic helper that sends a QMP query command and parses
// the response into the specified type. This eliminates code duplication
// across query methods like QueryStatus, QueryMemoryDevices, etc.
func qmpQuery[T any](q *qmpClient, ctx context.Context, command string) (T, error) {
	var result T
	resp, err := q.sendCommand(ctx, command, nil)
	if err != nil {
		return result, err
	}
	if resp.Return == nil {
		return result, nil
	}
	returnBytes, err := json.Marshal(resp.Return)
	if err != nil {
		return result, fmt.Errorf("failed to marshal %s response: %w", command, err)
	}
	if err := json.Unmarshal(returnBytes, &result); err != nil {
		return result, fmt.Errorf("failed to parse %s response: %w", command, err)
	}
	return result, nil
}

func (q *qmpClient) sendCommand(ctx context.Context, command string, args map[string]any) (*qmpResponse, error) {
	if err := q.checkClosed(); err != nil {
		return nil, err
	}

	cmd := qmpapi.Command{
		Execute: command,
	}
	// Only set Args when non-nil to avoid "arguments": null in JSON.
	// QEMU requires arguments to be either absent or an object, not null.
	if args != nil {
		cmd.Args = args
	}

	payload, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to encode QMP command %s: %w", command, err)
	}

	// Use configured timeout
	timeout := q.getCommandTimeout()
	if timeout == 0 {
		timeout = qmpDefaultTimeout
	}

	respChan := make(chan *qmpResponse, 1)
	errChan := make(chan error, 1)
	go func() {
		raw, err := q.monitor.Run(payload)
		if err != nil {
			errChan <- err
			return
		}

		var resp qmpResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			errChan <- fmt.Errorf("failed to parse QMP response for %s: %w", command, err)
			return
		}
		if resp.Error != nil {
			errChan <- fmt.Errorf("QMP error for %s: %s: %s", command, resp.Error.Class, resp.Error.Desc)
			return
		}

		respChan <- &resp
	}()

	// Wait for response with timeout
	select {
	case resp := <-respChan:
		if resp == nil {
			return nil, fmt.Errorf("QMP response channel closed for %s", command)
		}
		return resp, nil
	case err := <-errChan:
		return nil, fmt.Errorf("failed to send QMP command %s: %w", command, err)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout (%v) waiting for QMP response to %s", timeout, command)
	}
}

// SendCtrlAltDelete sends CTRL+ALT+DELETE key sequence to the VM.
// This is more reliable than ACPI powerdown for some Linux distributions.
func (q *qmpClient) SendCtrlAltDelete(ctx context.Context) error {
	keys := []any{
		map[string]any{"type": "qcode", "data": "ctrl"},
		map[string]any{"type": "qcode", "data": "alt"},
		map[string]any{"type": "qcode", "data": "delete"},
	}
	_, err := q.execute(ctx, "send-key", map[string]any{
		"keys": keys,
	})
	return err
}

// Shutdown gracefully shuts down the VM using ACPI powerdown.
func (q *qmpClient) Shutdown(ctx context.Context) error {
	_, err := q.execute(ctx, "system_powerdown", nil)
	return err
}

// Quit instructs QEMU to exit immediately.
func (q *qmpClient) Quit(ctx context.Context) error {
	_, err := q.execute(ctx, "quit", nil)
	return err
}

// QueryStatus returns the current VM status (running, paused, shutdown, etc).
func (q *qmpClient) QueryStatus(ctx context.Context) (*qmpStatus, error) {
	return qmpQuery[*qmpStatus](q, ctx, "query-status")
}

// DeviceAdd hotplugs a device.
func (q *qmpClient) DeviceAdd(ctx context.Context, driver string, args map[string]any) error {
	if args == nil {
		args = make(map[string]any)
	}
	args["driver"] = driver
	_, err := q.execute(ctx, "device_add", args)
	return err
}

// DeviceDelete removes a device.
func (q *qmpClient) DeviceDelete(ctx context.Context, deviceID string) error {
	_, err := q.execute(ctx, "device_del", map[string]any{
		"id": deviceID,
	})
	return err
}

// ObjectAdd adds a QEMU object (e.g., memory backend).
func (q *qmpClient) ObjectAdd(ctx context.Context, qomType, objID string, args map[string]any) error {
	arguments := map[string]any{
		"qom-type": qomType,
		"id":       objID,
	}
	for k, v := range args {
		arguments[k] = v
	}

	_, err := q.execute(ctx, "object-add", arguments)
	return err
}

// ObjectDel removes a QEMU object.
func (q *qmpClient) ObjectDel(ctx context.Context, objID string) error {
	_, err := q.execute(ctx, "object-del", map[string]any{
		"id": objID,
	})
	return err
}

// Close closes the QMP connection.
//
// The shutdown sequence is carefully ordered to avoid races with eventLoop:
// 1. Mark as closed (prevents new commands)
// 2. Disconnect the monitor (closes the socket)
// 3. Wait for eventLoop to exit
func (q *qmpClient) Close() error {
	if !q.closed.CompareAndSwap(false, true) {
		return nil // Already closed
	}

	err := q.monitor.Disconnect()

	// Wait for eventLoop to exit with a timeout.
	select {
	case <-q.eventLoopDone:
		// eventLoop exited cleanly
	case <-time.After(100 * time.Millisecond):
		// Timeout - eventLoop may have already exited or is stuck.
	}

	return err
}

// checkClosed returns an error if the QMP client is closed.
func (q *qmpClient) checkClosed() error {
	if q.closed.Load() {
		return fmt.Errorf("QMP client closed")
	}
	return nil
}

func (q *qmpClient) getCommandTimeout() time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.commandTimeout
}
