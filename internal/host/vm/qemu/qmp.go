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

	"github.com/aledbf/qemubox/containerd/internal/host/vm"
)

// qmpClient implements QEMU Machine Protocol (QMP) client.
// QMP is a JSON-RPC protocol for controlling QEMU via a Unix socket.
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

// SetCommandTimeout sets the timeout for QMP commands
// If not set or set to 0, defaults to 5 seconds
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

type qmpEventHandler func(logger *log.Entry, data map[string]any)

var qmpEventHandlers = map[string]qmpEventHandler{
	"SHUTDOWN": func(logger *log.Entry, data map[string]any) {
		reason := qmpStringField(data, "reason")
		logger.WithField("reason", reason).Info("qemu: guest initiated shutdown")
	},
	"POWERDOWN": func(logger *log.Entry, data map[string]any) {
		logger.Info("qemu: ACPI powerdown event received")
	},
	"RESET": func(logger *log.Entry, data map[string]any) {
		logger.Warn("qemu: guest reset/reboot detected")
	},
	"STOP": func(logger *log.Entry, data map[string]any) {
		logger.Debug("qemu: VM execution paused")
	},
	"RESUME": func(logger *log.Entry, data map[string]any) {
		logger.Debug("qemu: VM execution resumed")
	},
	"DEVICE_DELETED": func(logger *log.Entry, data map[string]any) {
		deviceID := qmpStringField(data, "device")
		logger.WithField("device", deviceID).Debug("qemu: device removed")
	},
	"NIC_RX_FILTER_CHANGED": func(logger *log.Entry, data map[string]any) {
		nicName := qmpStringField(data, "name")
		logger.WithField("nic", nicName).Debug("qemu: NIC RX filter changed")
	},
	"WATCHDOG": func(logger *log.Entry, data map[string]any) {
		action := qmpStringField(data, "action")
		logger.WithField("action", action).Warn("qemu: watchdog timer expired")
	},
	"GUEST_PANICKED": func(logger *log.Entry, data map[string]any) {
		logger.Error("qemu: guest kernel panic detected")
	},
	"BLOCK_IO_ERROR": func(logger *log.Entry, data map[string]any) {
		device := qmpStringField(data, "device")
		operation := qmpStringField(data, "operation")
		logger.WithFields(log.Fields{
			"device":    device,
			"operation": operation,
		}).Error("qemu: block I/O error")
	},
}

func qmpStringField(data map[string]any, key string) string {
	if data == nil {
		return "unknown"
	}
	if value, ok := data[key].(string); ok {
		return value
	}
	return "unknown"
}

// handleEvent processes QMP asynchronous events with structured logging
func (q *qmpClient) handleEvent(ctx context.Context, resp *qmpResponse) {
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
func (q *qmpClient) eventLoop(ctx context.Context) {
	defer close(q.eventLoopDone)

	if q.events == nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-q.events:
			if !ok {
				return
			}
			if q.closed.Load() {
				return
			}
			q.handleEvent(ctx, &qmpResponse{
				Event: ev.Event,
				Data:  ev.Data,
			})
		}
	}
}

// SendCtrlAltDelete sends CTRL+ALT+DELETE key sequence to the VM
// This is more reliable than ACPI powerdown for some Linux distributions
func (q *qmpClient) SendCtrlAltDelete(ctx context.Context) error {
	// Send CTRL+ALT+DELETE key sequence via QMP
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

// Shutdown gracefully shuts down the VM using ACPI powerdown
func (q *qmpClient) Shutdown(ctx context.Context) error {
	_, err := q.execute(ctx, "system_powerdown", nil)
	return err
}

// Quit instructs QEMU to exit immediately
func (q *qmpClient) Quit(ctx context.Context) error {
	_, err := q.execute(ctx, "quit", nil)
	return err
}

// QueryStatus returns the current VM status (running, paused, shutdown, etc).
func (q *qmpClient) QueryStatus(ctx context.Context) (*qmpStatus, error) {
	return qmpQuery[*qmpStatus](q, ctx, "query-status")
}

// DeviceAdd hotplugs a device
func (q *qmpClient) DeviceAdd(ctx context.Context, driver string, args map[string]any) error {
	if args == nil {
		args = make(map[string]any)
	}
	args["driver"] = driver
	_, err := q.execute(ctx, "device_add", args)
	return err
}

// DeviceDelete removes a device
func (q *qmpClient) DeviceDelete(ctx context.Context, deviceID string) error {
	_, err := q.execute(ctx, "device_del", map[string]any{
		"id": deviceID,
	})
	return err
}

// HotpluggableCPU describes an available CPU hotplug slot.
type HotpluggableCPU struct {
	Type       string         `json:"type"`
	QOMPath    string         `json:"qom-path"`
	Props      map[string]any `json:"props"`
	VCPUsCount int            `json:"vcpus-count"`
}

// QueryCPUs returns information about all vCPUs in the VM
// Uses query-cpus-fast which is more efficient than query-cpus
func (q *qmpClient) QueryCPUs(ctx context.Context) ([]vm.CPUInfo, error) {
	return qmpQuery[[]vm.CPUInfo](q, ctx, "query-cpus-fast")
}

// QueryHotpluggableCPUs returns available CPU hotplug slots.
func (q *qmpClient) QueryHotpluggableCPUs(ctx context.Context) ([]HotpluggableCPU, error) {
	return qmpQuery[[]HotpluggableCPU](q, ctx, "query-hotpluggable-cpus")
}

// HotplugCPU adds a new vCPU to the running VM
// cpuID should be the next available CPU index (e.g., if you have CPUs 0-1, use cpuID=2)
func (q *qmpClient) HotplugCPU(ctx context.Context, cpuID int) error {
	beforeCount := -1
	if cpus, err := q.QueryCPUs(ctx); err == nil {
		beforeCount = len(cpus)
	}

	driver := "host-x86_64-cpu"
	args := map[string]any{
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
			args = map[string]any{
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

func intFromProp(value any) (int, bool) {
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
func (q *qmpClient) UnplugCPU(ctx context.Context, cpuID int) error {
	deviceID := fmt.Sprintf("cpu%d", cpuID)

	log.G(ctx).WithFields(log.Fields{
		"cpu_id":    cpuID,
		"device_id": deviceID,
	}).Debug("qemu: unplugging vCPU")

	return q.DeviceDelete(ctx, deviceID)
}

// MemoryDeviceInfo represents a hotplugged memory device
type MemoryDeviceInfo struct {
	Type string         `json:"type"` // "dimm" or "virtio-mem"
	Data map[string]any `json:"data"`
}

// QueryMemoryDevices returns all hotplugged memory devices
func (q *qmpClient) QueryMemoryDevices(ctx context.Context) ([]MemoryDeviceInfo, error) {
	return qmpQuery[[]MemoryDeviceInfo](q, ctx, "query-memory-devices")
}

// QueryMemorySizeSummary returns memory usage summary
func (q *qmpClient) QueryMemorySizeSummary(ctx context.Context) (*MemorySizeSummary, error) {
	return qmpQuery[*MemorySizeSummary](q, ctx, "query-memory-size-summary")
}

// ObjectAdd adds a QEMU object (e.g., memory backend)
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

// ObjectDel removes a QEMU object
func (q *qmpClient) ObjectDel(ctx context.Context, objID string) error {
	_, err := q.execute(ctx, "object-del", map[string]any{
		"id": objID,
	})
	return err
}

// HotplugMemory adds memory to the VM using pc-dimm
// slotID: memory slot index (0-7 based on -m slots=8)
// sizeBytes: memory size in bytes (must be 128MB aligned)
func (q *qmpClient) HotplugMemory(ctx context.Context, slotID int, sizeBytes int64) error {
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
	backendArgs := map[string]any{
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
	dimmArgs := map[string]any{
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
				"slot_id":    slotID,
				"added_mb":   sizeBytes / (1024 * 1024),
				"total_mb":   (afterSummary.BaseMemory + afterSummary.PluggedMemory) / (1024 * 1024),
				"plugged_mb": afterSummary.PluggedMemory / (1024 * 1024),
			}).Info("qemu: memory hotplug successful")
		}
	}

	return nil
}

// UnplugMemory removes memory from the VM
// slotID: memory slot to remove
// Note: Memory hot-unplug requires guest kernel support (CONFIG_MEMORY_HOTREMOVE=y)
// and the memory must be offline in the guest before removal
func (q *qmpClient) UnplugMemory(ctx context.Context, slotID int) error {
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
// This helper reduces duplicate closed-check boilerplate across methods.
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
