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

	nextID         atomic.Uint64
	pending        map[uint64]chan *qmpResponse
	mu             sync.Mutex
	closed         atomic.Bool
	commandTimeout time.Duration // Timeout for QMP commands (default: 5 seconds)
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

// SetCommandTimeout sets the timeout for QMP commands
// If not set or set to 0, defaults to 5 seconds
func (q *QMPClient) SetCommandTimeout(timeout time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.commandTimeout = timeout
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

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to QMP socket: %w", err)
	}

	qmp := &QMPClient{
		conn:           conn,
		scanner:        bufio.NewScanner(conn),
		encoder:        json.NewEncoder(conn),
		pending:        make(map[uint64]chan *qmpResponse),
		commandTimeout: 5 * time.Second, // Default timeout
	}
	// QMP can emit large JSON objects; ensure we don't drop events due to scanner limits.
	qmp.scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	// Read QMP greeting
	if !qmp.scanner.Scan() {
		_ = conn.Close()
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
		_ = conn.Close()
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
		_ = conn.Close()
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

	// Use configured timeout (default: 5 seconds)
	timeout := q.commandTimeout
	if timeout == 0 {
		timeout = 5 * time.Second
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
	case <-time.After(timeout):
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		return fmt.Errorf("timeout (%v) waiting for QMP response to %s", timeout, command)
	}
}

type qmpEventHandler func(logger *log.Entry, data map[string]interface{})

var qmpEventHandlers = map[string]qmpEventHandler{
	"SHUTDOWN": func(logger *log.Entry, data map[string]interface{}) {
		reason := qmpStringField(data, "reason")
		logger.WithField("reason", reason).Info("qemu: guest initiated shutdown")
	},
	"POWERDOWN": func(logger *log.Entry, data map[string]interface{}) {
		logger.Info("qemu: ACPI powerdown event received")
	},
	"RESET": func(logger *log.Entry, data map[string]interface{}) {
		logger.Warn("qemu: guest reset/reboot detected")
	},
	"STOP": func(logger *log.Entry, data map[string]interface{}) {
		logger.Debug("qemu: VM execution paused")
	},
	"RESUME": func(logger *log.Entry, data map[string]interface{}) {
		logger.Debug("qemu: VM execution resumed")
	},
	"DEVICE_DELETED": func(logger *log.Entry, data map[string]interface{}) {
		deviceID := qmpStringField(data, "device")
		logger.WithField("device", deviceID).Debug("qemu: device removed")
	},
	"NIC_RX_FILTER_CHANGED": func(logger *log.Entry, data map[string]interface{}) {
		nicName := qmpStringField(data, "name")
		logger.WithField("nic", nicName).Debug("qemu: NIC RX filter changed")
	},
	"WATCHDOG": func(logger *log.Entry, data map[string]interface{}) {
		action := qmpStringField(data, "action")
		logger.WithField("action", action).Warn("qemu: watchdog timer expired")
	},
	"GUEST_PANICKED": func(logger *log.Entry, data map[string]interface{}) {
		logger.Error("qemu: guest kernel panic detected")
	},
	"BLOCK_IO_ERROR": func(logger *log.Entry, data map[string]interface{}) {
		device := qmpStringField(data, "device")
		operation := qmpStringField(data, "operation")
		logger.WithFields(log.Fields{
			"device":    device,
			"operation": operation,
		}).Error("qemu: block I/O error")
	},
}

func qmpStringField(data map[string]interface{}, key string) string {
	if data == nil {
		return "unknown"
	}
	if value, ok := data[key].(string); ok {
		return value
	}
	return "unknown"
}

// handleEvent processes QMP asynchronous events with structured logging
func (q *QMPClient) handleEvent(ctx context.Context, resp *qmpResponse) {
	logger := log.G(ctx).WithFields(log.Fields{
		"event": resp.Event,
		"data":  resp.Data,
	})

	handler, ok := qmpEventHandlers[resp.Event]
	if !ok {
		logger.Debug("qemu: QMP event received")
		return
	}
	handler(logger, resp.Data)
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
			q.handleEvent(ctx, &resp)
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
		// After quit command or Close(), connection errors are expected
		if q.closed.Load() {
			log.G(ctx).WithError(err).Trace("qemu: QMP connection closed")
		} else {
			log.G(ctx).WithError(err).Debug("qemu: QMP scanner error")
		}
	}
}

// SendCtrlAltDelete sends CTRL+ALT+DELETE key sequence to the VM
// This is more reliable than ACPI powerdown for some Linux distributions
func (q *QMPClient) SendCtrlAltDelete(ctx context.Context) error {
	// Send CTRL+ALT+DELETE key sequence via QMP
	keys := []interface{}{
		map[string]interface{}{"type": "qcode", "data": "ctrl"},
		map[string]interface{}{"type": "qcode", "data": "alt"},
		map[string]interface{}{"type": "qcode", "data": "delete"},
	}
	return q.execute(ctx, "send-key", map[string]interface{}{
		"keys": keys,
	})
}

// Shutdown gracefully shuts down the VM using ACPI powerdown
func (q *QMPClient) Shutdown(ctx context.Context) error {
	return q.execute(ctx, "system_powerdown", nil)
}

// Quit instructs QEMU to exit immediately
func (q *QMPClient) Quit(ctx context.Context) error {
	return q.execute(ctx, "quit", nil)
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

// HotpluggableCPU describes an available CPU hotplug slot.
type HotpluggableCPU struct {
	Type       string                 `json:"type"`
	QOMPath    string                 `json:"qom-path"`
	Props      map[string]interface{} `json:"props"`
	VCPUsCount int                    `json:"vcpus-count"`
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

// QueryHotpluggableCPUs returns available CPU hotplug slots.
func (q *QMPClient) QueryHotpluggableCPUs(ctx context.Context) ([]HotpluggableCPU, error) {
	if q.closed.Load() {
		return nil, fmt.Errorf("QMP client closed")
	}

	id := q.nextID.Add(1)
	respChan := make(chan *qmpResponse, 1)

	q.mu.Lock()
	q.pending[id] = respChan
	q.mu.Unlock()

	cmd := qmpCommand{
		Execute: "query-hotpluggable-cpus",
		ID:      id,
	}

	if err := q.encoder.Encode(cmd); err != nil {
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		return nil, fmt.Errorf("failed to send query-hotpluggable-cpus: %w", err)
	}

	select {
	case resp := <-respChan:
		if resp.Error != nil {
			return nil, fmt.Errorf("QMP error for query-hotpluggable-cpus: %s: %s", resp.Error.Class, resp.Error.Desc)
		}

		var cpus []HotpluggableCPU
		returnBytes, err := json.Marshal(resp.Return)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal hotpluggable CPU info: %w", err)
		}

		if err := json.Unmarshal(returnBytes, &cpus); err != nil {
			return nil, fmt.Errorf("failed to parse hotpluggable CPU info: %w", err)
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
		return nil, fmt.Errorf("timeout waiting for query-hotpluggable-cpus response")
	}
}

// HotplugCPU adds a new vCPU to the running VM
// cpuID should be the next available CPU index (e.g., if you have CPUs 0-1, use cpuID=2)
func (q *QMPClient) HotplugCPU(ctx context.Context, cpuID int) error {
	beforeCount := -1
	if cpus, err := q.QueryCPUs(ctx); err == nil {
		beforeCount = len(cpus)
	}

	driver := "host-x86_64-cpu"
	args := map[string]interface{}{
		"id":        fmt.Sprintf("cpu%d", cpuID),
		"socket-id": 0,
		"core-id":   cpuID,
		"thread-id": 0,
	}

	if cpus, err := q.QueryHotpluggableCPUs(ctx); err == nil {
		if len(cpus) == 0 {
			log.G(ctx).WithField("cpu_id", cpuID).
				Warn("qemu: no hotpluggable CPU slots reported by QEMU")
			return fmt.Errorf("no hotpluggable CPU slots reported by QEMU")
		}
		if match := matchHotpluggableCPU(cpus, cpuID); match != nil {
			driver = match.Type
			args = map[string]interface{}{
				"id": fmt.Sprintf("cpu%d", cpuID),
			}
			for k, v := range match.Props {
				args[k] = v
			}
			log.G(ctx).WithFields(log.Fields{
				"cpu_id":   cpuID,
				"driver":   driver,
				"props":    match.Props,
				"qom_path": match.QOMPath,
			}).Debug("qemu: using hotpluggable CPU slot")
		} else {
			log.G(ctx).WithFields(log.Fields{
				"cpu_id": cpuID,
				"count":  len(cpus),
			}).Debug("qemu: no matching hotpluggable CPU slot; using default props")
		}
	} else {
		log.G(ctx).WithFields(log.Fields{
			"cpu_id": cpuID,
			"error":  err,
		}).Debug("qemu: failed to query hotpluggable CPUs; using default props")
	}

	log.G(ctx).WithFields(log.Fields{
		"cpu_id":    cpuID,
		"driver":    driver,
		"socket_id": args["socket-id"],
		"core_id":   args["core-id"],
		"thread_id": args["thread-id"],
	}).Debug("qemu: hotplugging vCPU")

	if err := q.DeviceAdd(ctx, driver, args); err != nil {
		return err
	}

	if beforeCount >= 0 {
		if cpus, err := q.QueryCPUs(ctx); err == nil {
			if len(cpus) <= beforeCount {
				log.G(ctx).WithFields(log.Fields{
					"cpu_id":       cpuID,
					"before_count": beforeCount,
					"after_count":  len(cpus),
				}).Warn("qemu: device_add did not increase CPU count")
				return fmt.Errorf("device_add did not increase CPU count")
			}
		}
	}

	return nil
}

func matchHotpluggableCPU(cpus []HotpluggableCPU, cpuID int) *HotpluggableCPU {
	var fallback *HotpluggableCPU
	for i := range cpus {
		if cpus[i].QOMPath != "" {
			continue
		}
		props := cpus[i].Props
		if props == nil {
			continue
		}
		coreID, ok := intFromProp(props["core-id"])
		if ok && coreID == cpuID {
			return &cpus[i]
		}
		if fallback == nil {
			fallback = &cpus[i]
		}
	}
	return fallback
}

func intFromProp(value interface{}) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case uint32:
		return int(v), true
	case uint64:
		return int(v), true
	default:
		return 0, false
	}
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

// MemoryDeviceInfo represents a hotplugged memory device
type MemoryDeviceInfo struct {
	Type string                 `json:"type"` // "dimm" or "virtio-mem"
	Data map[string]interface{} `json:"data"`
}

// MemorySizeSummary from query-memory-size-summary
type MemorySizeSummary struct {
	BaseMemory    int64 `json:"base-memory"`    // Boot memory in bytes
	PluggedMemory int64 `json:"plugged-memory"` // Hotplugged memory in bytes
}

// QueryMemoryDevices returns all hotplugged memory devices
func (q *QMPClient) QueryMemoryDevices(ctx context.Context) ([]MemoryDeviceInfo, error) {
	if q.closed.Load() {
		return nil, fmt.Errorf("QMP client closed")
	}

	id := q.nextID.Add(1)
	respChan := make(chan *qmpResponse, 1)

	q.mu.Lock()
	q.pending[id] = respChan
	q.mu.Unlock()

	cmd := qmpCommand{
		Execute: "query-memory-devices",
		ID:      id,
	}

	if err := q.encoder.Encode(cmd); err != nil {
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		return nil, fmt.Errorf("failed to send query-memory-devices: %w", err)
	}

	select {
	case resp := <-respChan:
		if resp.Error != nil {
			return nil, fmt.Errorf("QMP error for query-memory-devices: %s: %s", resp.Error.Class, resp.Error.Desc)
		}

		var devices []MemoryDeviceInfo
		if resp.Return == nil {
			return devices, nil
		}

		returnBytes, err := json.Marshal(resp.Return)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal memory devices: %w", err)
		}

		if err := json.Unmarshal(returnBytes, &devices); err != nil {
			return nil, fmt.Errorf("failed to parse memory devices: %w", err)
		}

		return devices, nil

	case <-ctx.Done():
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		return nil, ctx.Err()

	case <-time.After(5 * time.Second):
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		return nil, fmt.Errorf("timeout waiting for query-memory-devices response")
	}
}

// QueryMemorySizeSummary returns memory usage summary
func (q *QMPClient) QueryMemorySizeSummary(ctx context.Context) (*MemorySizeSummary, error) {
	if q.closed.Load() {
		return nil, fmt.Errorf("QMP client closed")
	}

	id := q.nextID.Add(1)
	respChan := make(chan *qmpResponse, 1)

	q.mu.Lock()
	q.pending[id] = respChan
	q.mu.Unlock()

	cmd := qmpCommand{
		Execute: "query-memory-size-summary",
		ID:      id,
	}

	if err := q.encoder.Encode(cmd); err != nil {
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		return nil, fmt.Errorf("failed to send query-memory-size-summary: %w", err)
	}

	select {
	case resp := <-respChan:
		if resp.Error != nil {
			return nil, fmt.Errorf("QMP error for query-memory-size-summary: %s: %s", resp.Error.Class, resp.Error.Desc)
		}

		var summary MemorySizeSummary
		if resp.Return == nil {
			return &summary, nil
		}

		returnBytes, err := json.Marshal(resp.Return)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal memory summary: %w", err)
		}

		if err := json.Unmarshal(returnBytes, &summary); err != nil {
			return nil, fmt.Errorf("failed to parse memory summary: %w", err)
		}

		return &summary, nil

	case <-ctx.Done():
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		return nil, ctx.Err()

	case <-time.After(5 * time.Second):
		q.mu.Lock()
		delete(q.pending, id)
		q.mu.Unlock()
		return nil, fmt.Errorf("timeout waiting for query-memory-size-summary response")
	}
}

// ObjectAdd adds a QEMU object (e.g., memory backend)
func (q *QMPClient) ObjectAdd(ctx context.Context, qomType, objID string, args map[string]interface{}) error {
	arguments := map[string]interface{}{
		"qom-type": qomType,
		"id":       objID,
	}
	for k, v := range args {
		arguments[k] = v
	}

	return q.execute(ctx, "object-add", arguments)
}

// ObjectDel removes a QEMU object
func (q *QMPClient) ObjectDel(ctx context.Context, objID string) error {
	return q.execute(ctx, "object-del", map[string]interface{}{
		"id": objID,
	})
}

// HotplugMemory adds memory to the VM using pc-dimm
// slotID: memory slot index (0-7 based on -m slots=8)
// sizeBytes: memory size in bytes (must be 128MB aligned)
func (q *QMPClient) HotplugMemory(ctx context.Context, slotID int, sizeBytes int64) error {
	// Validate 128MB alignment
	const alignmentMB = 128
	const alignmentBytes = alignmentMB * 1024 * 1024
	if sizeBytes%alignmentBytes != 0 {
		return fmt.Errorf("memory size must be %dMB aligned, got %d bytes", alignmentMB, sizeBytes)
	}

	backendID := fmt.Sprintf("mem%d", slotID)
	dimmID := fmt.Sprintf("dimm%d", slotID)

	// Query current state before adding
	beforeSummary, err := q.QueryMemorySizeSummary(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Warn("qemu: failed to query memory before hotplug")
	}

	// Step 1: Create memory backend object
	backendArgs := map[string]interface{}{
		"size": sizeBytes,
	}

	log.G(ctx).WithFields(log.Fields{
		"slot_id":    slotID,
		"size_bytes": sizeBytes,
		"size_mb":    sizeBytes / (1024 * 1024),
		"backend_id": backendID,
	}).Debug("qemu: creating memory backend")

	if err := q.ObjectAdd(ctx, "memory-backend-ram", backendID, backendArgs); err != nil {
		return fmt.Errorf("failed to create memory backend: %w", err)
	}

	// Step 2: Hotplug pc-dimm device
	dimmArgs := map[string]interface{}{
		"id":     dimmID,
		"memdev": backendID,
	}

	log.G(ctx).WithFields(log.Fields{
		"slot_id": slotID,
		"dimm_id": dimmID,
	}).Debug("qemu: hotplugging memory device")

	if err := q.DeviceAdd(ctx, "pc-dimm", dimmArgs); err != nil {
		// Cleanup backend on failure
		if delErr := q.ObjectDel(ctx, backendID); delErr != nil {
			log.G(ctx).WithError(delErr).Warn("qemu: failed to cleanup memory backend after device_add failure")
		}
		return fmt.Errorf("failed to hotplug memory device: %w", err)
	}

	// Verify memory was added
	if beforeSummary != nil {
		afterSummary, err := q.QueryMemorySizeSummary(ctx)
		if err == nil {
			if afterSummary.BaseMemory+afterSummary.PluggedMemory <= beforeSummary.BaseMemory+beforeSummary.PluggedMemory {
				log.G(ctx).WithFields(log.Fields{
					"slot_id":       slotID,
					"before_total":  beforeSummary.BaseMemory + beforeSummary.PluggedMemory,
					"after_total":   afterSummary.BaseMemory + afterSummary.PluggedMemory,
					"expected_size": sizeBytes,
				}).Warn("qemu: device_add did not increase memory size")
				return fmt.Errorf("device_add did not increase memory size")
			}
			log.G(ctx).WithFields(log.Fields{
				"slot_id":        slotID,
				"added_mb":       sizeBytes / (1024 * 1024),
				"total_mb":       (afterSummary.BaseMemory + afterSummary.PluggedMemory) / (1024 * 1024),
				"plugged_mb":     afterSummary.PluggedMemory / (1024 * 1024),
			}).Info("qemu: memory hotplug successful")
		}
	}

	return nil
}

// UnplugMemory removes memory from the VM
// slotID: memory slot to remove
// Note: Memory hot-unplug requires guest kernel support (CONFIG_MEMORY_HOTREMOVE=y)
// and the memory must be offline in the guest before removal
func (q *QMPClient) UnplugMemory(ctx context.Context, slotID int) error {
	dimmID := fmt.Sprintf("dimm%d", slotID)
	backendID := fmt.Sprintf("mem%d", slotID)

	log.G(ctx).WithFields(log.Fields{
		"slot_id": slotID,
		"dimm_id": dimmID,
	}).Debug("qemu: unplugging memory device")

	// Step 1: Remove device
	if err := q.DeviceDelete(ctx, dimmID); err != nil {
		return fmt.Errorf("failed to unplug memory device: %w", err)
	}

	// Step 2: Remove backend object
	// Note: QEMU may need time to complete device removal before backend deletion
	// We'll attempt to delete the backend, but it's not critical if it fails
	if err := q.ObjectDel(ctx, backendID); err != nil {
		log.G(ctx).WithError(err).WithField("backend_id", backendID).
			Warn("qemu: failed to delete memory backend (non-fatal)")
	}

	return nil
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
