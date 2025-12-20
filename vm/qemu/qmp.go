package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/log"
)

// QMPClient implements QEMU Machine Protocol (QMP) client.
// QMP is a JSON-RPC protocol for controlling QEMU via a Unix socket.
type QMPClient struct {
	conn    net.Conn
	scanner *bufio.Scanner
	encoder *json.Encoder

	nextID  atomic.Uint64
	pending map[uint64]chan *qmpResponse
	mu      sync.Mutex
	closed  atomic.Bool
}

type qmpCommand struct {
	Execute   string                 `json:"execute"`
	Arguments map[string]interface{} `json:"arguments,omitempty"`
	ID        uint64                 `json:"id,omitempty"`
}

type qmpResponse struct {
	Return interface{}            `json:"return,omitempty"`
	Error  *qmpError              `json:"error,omitempty"`
	ID     uint64                 `json:"id,omitempty"`
	Event  string                 `json:"event,omitempty"`
	Data   map[string]interface{} `json:"data,omitempty"`
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

// NewQMPClient creates a QMP client and performs initial handshake.
// The QMP protocol requires:
// 1. Read greeting message from server
// 2. Send qmp_capabilities command to enter command mode
// 3. Start event loop for asynchronous events
func NewQMPClient(ctx context.Context, socketPath string) (*QMPClient, error) {
	// Wait for socket to appear
	if err := waitForSocket(ctx, socketPath, vmStartTimeout); err != nil {
		return nil, fmt.Errorf("QMP socket not available: %w", err)
	}

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to QMP socket: %w", err)
	}

	qmp := &QMPClient{
		conn:    conn,
		scanner: bufio.NewScanner(conn),
		encoder: json.NewEncoder(conn),
		pending: make(map[uint64]chan *qmpResponse),
	}
	// QMP can emit large JSON objects; ensure we don't drop events due to scanner limits.
	qmp.scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	// Read QMP greeting
	if !qmp.scanner.Scan() {
		conn.Close()
		return nil, fmt.Errorf("failed to read QMP greeting")
	}

	var greeting struct {
		QMP struct {
			Version struct {
				QEMU struct {
					Major int `json:"major"`
					Minor int `json:"minor"`
					Micro int `json:"micro"`
				} `json:"qemu"`
			} `json:"version"`
			Capabilities []string `json:"capabilities"`
		} `json:"QMP"`
	}

	if err := json.Unmarshal(qmp.scanner.Bytes(), &greeting); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to parse QMP greeting: %w", err)
	}

	log.G(ctx).WithFields(log.Fields{
		"major": greeting.QMP.Version.QEMU.Major,
		"minor": greeting.QMP.Version.QEMU.Minor,
		"micro": greeting.QMP.Version.QEMU.Micro,
	}).Debug("qemu: connected to QMP")

	// Start event loop in background BEFORE sending commands
	go qmp.eventLoop(ctx)

	// Enter command mode
	if err := qmp.execute(ctx, "qmp_capabilities", nil); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to negotiate QMP capabilities: %w", err)
	}

	return qmp, nil
}

// execute sends a QMP command and waits for response
func (q *QMPClient) execute(ctx context.Context, command string, args map[string]interface{}) error {
	if q.closed.Load() {
		return fmt.Errorf("QMP client closed")
	}

	id := q.nextID.Add(1)

	respChan := make(chan *qmpResponse, 1)
	q.mu.Lock()
	if q.pending == nil {
		q.mu.Unlock()
		return fmt.Errorf("QMP client closed")
	}
	q.pending[id] = respChan
	q.mu.Unlock()

	cmd := qmpCommand{
		Execute:   command,
		Arguments: args,
		ID:        id,
	}

	if err := q.encoder.Encode(cmd); err != nil {
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		return fmt.Errorf("failed to send QMP command %s: %w", command, err)
	}

	// Wait for response with timeout
	select {
	case resp := <-respChan:
		if resp == nil {
			return fmt.Errorf("QMP response channel closed for %s", command)
		}
		if resp.Error != nil {
			return fmt.Errorf("QMP error for %s: %s: %s", command, resp.Error.Class, resp.Error.Desc)
		}
		return nil
	case <-ctx.Done():
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		return ctx.Err()
	case <-time.After(5 * time.Second):
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		return fmt.Errorf("timeout waiting for QMP response to %s", command)
	}
}

// eventLoop processes QMP messages (responses and events)
func (q *QMPClient) eventLoop(ctx context.Context) {
	for q.scanner.Scan() {
		if q.closed.Load() {
			return
		}

		var resp qmpResponse
		if err := json.Unmarshal(q.scanner.Bytes(), &resp); err != nil {
			log.G(ctx).WithError(err).Warn("qemu: failed to parse QMP message")
			continue
		}

		// Handle asynchronous events (no ID)
		if resp.Event != "" {
			log.G(ctx).WithFields(log.Fields{
				"event": resp.Event,
				"data":  resp.Data,
			}).Info("qemu: QMP event received")

			// Handle guest-initiated shutdown/reboot events
			// These are informational - the process monitor will detect when QEMU exits
			// and trigger cleanup. Shutdown() coordinates all shutdown actions.
			switch resp.Event {
			case "SHUTDOWN", "POWERDOWN", "RESET":
				log.G(ctx).WithFields(log.Fields{
					"event": resp.Event,
					"data":  resp.Data,
				}).Debug("qemu: received shutdown event from guest")
			}
			continue
		}

		// Handle command responses
		q.mu.Lock()
		ch, ok := q.pending[resp.ID]
		if ok {
			delete(q.pending, resp.ID)
		}
		q.mu.Unlock()

		if ok {
			select {
			case ch <- &resp:
			default:
				// Channel closed or nobody waiting
			}
		}
	}

	// Scanner stopped (connection closed or error)
	if err := q.scanner.Err(); err != nil {
		log.G(ctx).WithError(err).Debug("qemu: QMP scanner error")
	}
}

// Shutdown gracefully shuts down the VM
func (q *QMPClient) Shutdown(ctx context.Context) error {
	return q.execute(ctx, "system_powerdown", nil)
}

// QueryStatus returns the current VM status (running, paused, shutdown, etc).
func (q *QMPClient) QueryStatus(ctx context.Context) (*qmpStatus, error) {
	if q.closed.Load() {
		return nil, fmt.Errorf("QMP client closed")
	}

	id := q.nextID.Add(1)
	respChan := make(chan *qmpResponse, 1)

	q.mu.Lock()
	q.pending[id] = respChan
	q.mu.Unlock()

	cmd := qmpCommand{
		Execute: "query-status",
		ID:      id,
	}

	if err := q.encoder.Encode(cmd); err != nil {
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		return nil, fmt.Errorf("failed to send query-status: %w", err)
	}

	select {
	case resp := <-respChan:
		if resp.Error != nil {
			return nil, fmt.Errorf("QMP error for query-status: %s: %s", resp.Error.Class, resp.Error.Desc)
		}
		var status qmpStatus
		returnBytes, err := json.Marshal(resp.Return)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal status: %w", err)
		}
		if err := json.Unmarshal(returnBytes, &status); err != nil {
			return nil, fmt.Errorf("failed to parse status: %w", err)
		}
		return &status, nil
	case <-ctx.Done():
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		return nil, ctx.Err()
	case <-time.After(5 * time.Second):
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		return nil, fmt.Errorf("timeout waiting for query-status response")
	}
}

// DeviceAdd hotplugs a device
func (q *QMPClient) DeviceAdd(ctx context.Context, driver string, args map[string]interface{}) error {
	if args == nil {
		args = make(map[string]interface{})
	}
	args["driver"] = driver
	return q.execute(ctx, "device_add", args)
}

// DeviceDelete removes a device
func (q *QMPClient) DeviceDelete(ctx context.Context, deviceID string) error {
	return q.execute(ctx, "device_del", map[string]interface{}{
		"id": deviceID,
	})
}

// CPUInfo represents information about a single vCPU
type CPUInfo struct {
	CPUIndex int    `json:"cpu-index"`
	QOMPath  string `json:"qom-path"`
	Thread   int    `json:"thread-id"`
	Target   string `json:"target"`
}

// QueryCPUs returns information about all vCPUs in the VM
// Uses query-cpus-fast which is more efficient than query-cpus
func (q *QMPClient) QueryCPUs(ctx context.Context) ([]CPUInfo, error) {
	if q.closed.Load() {
		return nil, fmt.Errorf("QMP client closed")
	}

	id := q.nextID.Add(1)
	respChan := make(chan *qmpResponse, 1)

	q.mu.Lock()
	q.pending[id] = respChan
	q.mu.Unlock()

	cmd := qmpCommand{
		Execute: "query-cpus-fast",
		ID:      id,
	}

	if err := q.encoder.Encode(cmd); err != nil {
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		return nil, fmt.Errorf("failed to send query-cpus-fast: %w", err)
	}

	// Wait for response with timeout
	select {
	case resp := <-respChan:
		if resp.Error != nil {
			return nil, fmt.Errorf("QMP error for query-cpus-fast: %s: %s", resp.Error.Class, resp.Error.Desc)
		}

		// Parse the return value as array of CPUInfo
		var cpus []CPUInfo
		returnBytes, err := json.Marshal(resp.Return)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal CPU info: %w", err)
		}

		if err := json.Unmarshal(returnBytes, &cpus); err != nil {
			return nil, fmt.Errorf("failed to parse CPU info: %w", err)
		}

		return cpus, nil

	case <-ctx.Done():
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		return nil, ctx.Err()

	case <-time.After(5 * time.Second):
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		return nil, fmt.Errorf("timeout waiting for query-cpus-fast response")
	}
}

// HotplugCPU adds a new vCPU to the running VM
// cpuID should be the next available CPU index (e.g., if you have CPUs 0-1, use cpuID=2)
func (q *QMPClient) HotplugCPU(ctx context.Context, cpuID int) error {
	args := map[string]interface{}{
		"driver": "host-x86_64-cpu",
		"id":     fmt.Sprintf("cpu%d", cpuID),
	}

	log.G(ctx).WithFields(log.Fields{
		"cpu_id": cpuID,
		"driver": "host-x86_64-cpu",
	}).Debug("qemu: hotplugging vCPU")

	return q.DeviceAdd(ctx, "host-x86_64-cpu", args)
}

// UnplugCPU removes a vCPU from the running VM
// Note: CPU hot-unplug requires guest kernel support (CONFIG_HOTPLUG_CPU=y)
// and the CPU must be offline in the guest before removal
func (q *QMPClient) UnplugCPU(ctx context.Context, cpuID int) error {
	deviceID := fmt.Sprintf("cpu%d", cpuID)

	log.G(ctx).WithFields(log.Fields{
		"cpu_id":    cpuID,
		"device_id": deviceID,
	}).Debug("qemu: unplugging vCPU")

	return q.DeviceDelete(ctx, deviceID)
}

// Close closes the QMP connection
func (q *QMPClient) Close() error {
	if !q.closed.CompareAndSwap(false, true) {
		return nil // Already closed
	}

	q.mu.Lock()
	// Close all pending channels
	for _, ch := range q.pending {
		close(ch)
	}
	q.pending = nil
	q.mu.Unlock()

	return q.conn.Close()
}
