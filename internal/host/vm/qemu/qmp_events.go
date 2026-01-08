//go:build linux

package qemu

import (
	"context"

	"github.com/containerd/log"
)

// qmpEventHandler processes a specific QMP event type.
type qmpEventHandler func(logger *log.Entry, data map[string]any)

// qmpEventHandlers maps event names to their handlers.
// Events not in this map are logged at debug level with no special processing.
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

// handleEvent processes QMP asynchronous events with structured logging.
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

// eventLoop processes QMP asynchronous events.
//
// Lifecycle: This goroutine is started by newQMPClient and runs until:
// - The context is cancelled (VM shutdown)
// - The events channel is closed (monitor disconnected)
// - The client is marked as closed
//
// When exiting, it closes eventLoopDone to signal Close() that cleanup is complete.
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
