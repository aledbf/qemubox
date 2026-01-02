// Package stdio provides RPC-based I/O management for container processes.
// It maintains per-process I/O pipes and supports multiple subscribers
// for attach/detach functionality.
package stdio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"
)

const (
	// subscriberChannelBuffer is the buffer size for subscriber output channels.
	// This provides some slack to avoid blocking the fan-out goroutine while
	// the RPC stream sends data.
	subscriberChannelBuffer = 64

	// maxBufferedBytes is the maximum bytes to buffer per stream for late subscribers.
	// Older data is discarded when this limit is exceeded.
	maxBufferedBytes = 256 * 1024
)

// processKey uniquely identifies a process within a container.
type processKey struct {
	containerID string
	execID      string
}

// subscriber represents a client subscribed to output streams.
type subscriber struct {
	ch     chan OutputData
	cancel context.CancelFunc
	done   chan struct{} // Closed when the subscriber's RPC stream finishes
}

// OutputData represents a chunk of output data sent to subscribers.
type OutputData struct {
	Data []byte
	EOF  bool
}

// processIO holds the I/O state for a single process.
type processIO struct {
	mu sync.Mutex

	// stdin is the writer to the process's stdin pipe.
	stdin       io.WriteCloser
	stdinClosed bool

	// stdout/stderr readers from the process.
	stdoutReader io.Reader
	stderrReader io.Reader

	// Subscribers for fan-out.
	stdoutSubs []*subscriber
	stderrSubs []*subscriber

	// Buffered output for late subscribers.
	stdoutBuf      []OutputData
	stderrBuf      []OutputData
	stdoutBufBytes int
	stderrBufBytes int

	// Process lifecycle.
	exited   bool
	exitChan chan struct{}

	// Goroutine synchronization.
	wg sync.WaitGroup // Tracks fanOutReader goroutines

	// Subscriber stream synchronization.
	// Tracks active RPC subscriber streams so we can wait for them to finish
	// sending all data before signaling I/O complete.
	subscriberWg sync.WaitGroup
}

// Manager maintains I/O state for all container processes.
// It supports multiple subscribers per process for attach functionality.
type Manager struct {
	mu        sync.RWMutex
	processes map[processKey]*processIO
}

// NewManager creates a new I/O manager.
func NewManager() *Manager {
	return &Manager{
		processes: make(map[processKey]*processIO),
	}
}

// Register registers a new process with its I/O pipes.
// The manager takes ownership of the pipes and will close them when the process exits.
func (m *Manager) Register(containerID, execID string, stdin io.WriteCloser, stdout, stderr io.Reader) {
	key := processKey{containerID: containerID, execID: execID}

	pio := &processIO{
		stdin:        stdin,
		stdoutReader: stdout,
		stderrReader: stderr,
		exitChan:     make(chan struct{}),
	}

	m.mu.Lock()
	m.processes[key] = pio
	m.mu.Unlock()

	// Start fan-out goroutines for stdout and stderr.
	if stdout != nil {
		pio.wg.Add(1)
		go m.fanOutReader(containerID, execID, stdout, pio, stdoutConfig)
	}
	if stderr != nil {
		pio.wg.Add(1)
		go m.fanOutReader(containerID, execID, stderr, pio, stderrConfig)
	}

	log.L.WithField("container", containerID).WithField("exec", execID).Debug("registered process I/O")
}

// streamConfig holds configuration for a single output stream (stdout or stderr).
type streamConfig struct {
	name    string
	getSubs func(*processIO) *[]*subscriber
	buffer  func(*Manager, *processIO, OutputData)
}

var (
	stdoutConfig = streamConfig{
		name:    "stdout",
		getSubs: func(p *processIO) *[]*subscriber { return &p.stdoutSubs },
		buffer:  (*Manager).bufferStdout,
	}
	stderrConfig = streamConfig{
		name:    "stderr",
		getSubs: func(p *processIO) *[]*subscriber { return &p.stderrSubs },
		buffer:  (*Manager).bufferStderr,
	}
)

// fanOutReader reads from a reader and distributes data to all subscribers.
func (m *Manager) fanOutReader(containerID, execID string, reader io.Reader, pio *processIO, cfg streamConfig) {
	defer pio.wg.Done()

	buf := make([]byte, 32*1024) // 32KB buffer
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])

			pio.mu.Lock()
			subs := *cfg.getSubs(pio)
			if len(subs) == 0 {
				cfg.buffer(m, pio, OutputData{Data: data})
			} else {
				for _, sub := range subs {
					select {
					case sub.ch <- OutputData{Data: data}:
					default:
						log.L.WithField("container", containerID).WithField("stream", cfg.name).Warn("dropping data for slow subscriber")
					}
				}
			}
			pio.mu.Unlock()
		}

		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.L.WithError(err).WithField("container", containerID).WithField("stream", cfg.name).Warn("error reading from process")
			}

			// Send or buffer EOF for subscribers.
			pio.mu.Lock()
			subs := *cfg.getSubs(pio)
			if len(subs) == 0 {
				cfg.buffer(m, pio, OutputData{EOF: true})
			} else {
				for _, sub := range subs {
					select {
					case sub.ch <- OutputData{EOF: true}:
					default:
					}
				}
			}
			pio.mu.Unlock()
			return
		}
	}
}

// Unregister removes a process from the manager.
// This should be called when the process exits.
func (m *Manager) Unregister(containerID, execID string) {
	key := processKey{containerID: containerID, execID: execID}

	m.mu.Lock()
	pio, ok := m.processes[key]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.processes, key)
	m.mu.Unlock()

	// Mark as exited first (prevents new subscribers).
	pio.mu.Lock()
	pio.exited = true
	close(pio.exitChan)
	pio.mu.Unlock()

	// Wait for fan-out goroutines to finish FIRST.
	// This ensures all output data and EOF are delivered to subscribers
	// before we close their channels.
	pio.wg.Wait()

	// Now safe to cancel and close subscriber channels.
	pio.mu.Lock()
	for _, sub := range pio.stdoutSubs {
		sub.cancel()
		close(sub.ch)
	}
	for _, sub := range pio.stderrSubs {
		sub.cancel()
		close(sub.ch)
	}
	pio.stdoutSubs = nil
	pio.stderrSubs = nil
	pio.mu.Unlock()

	// Close stdin if not already closed.
	if pio.stdin != nil && !pio.stdinClosed {
		_ = pio.stdin.Close()
	}

	log.L.WithField("container", containerID).WithField("exec", execID).Debug("unregistered process I/O")
}

// WriteStdin writes data to a process's stdin.
func (m *Manager) WriteStdin(containerID, execID string, data []byte) (int, error) {
	key := processKey{containerID: containerID, execID: execID}

	m.mu.RLock()
	pio, ok := m.processes[key]
	m.mu.RUnlock()

	if !ok {
		return 0, fmt.Errorf("process not found: %w", errdefs.ErrNotFound)
	}

	pio.mu.Lock()
	defer pio.mu.Unlock()

	if pio.stdinClosed {
		return 0, fmt.Errorf("stdin closed: %w", errdefs.ErrFailedPrecondition)
	}

	if pio.stdin == nil {
		return 0, fmt.Errorf("stdin not available: %w", errdefs.ErrFailedPrecondition)
	}

	n, err := pio.stdin.Write(data)
	if err != nil {
		return n, fmt.Errorf("write failed: %w", err)
	}

	return n, nil
}

// CloseStdin closes a process's stdin.
func (m *Manager) CloseStdin(containerID, execID string) error {
	key := processKey{containerID: containerID, execID: execID}

	m.mu.RLock()
	pio, ok := m.processes[key]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("process not found: %w", errdefs.ErrNotFound)
	}

	pio.mu.Lock()
	defer pio.mu.Unlock()

	if pio.stdinClosed {
		return fmt.Errorf("stdin already closed: %w", errdefs.ErrFailedPrecondition)
	}

	if pio.stdin != nil {
		if err := pio.stdin.Close(); err != nil {
			return fmt.Errorf("close failed: %w", err)
		}
	}
	pio.stdinClosed = true

	return nil
}

// SubscribeStdout subscribes to a process's stdout stream.
// Returns a channel that receives output chunks and a done function.
// The caller MUST call the done function when finished processing the stream
// to signal that I/O is complete (this is required for WaitForIOComplete to work).
func (m *Manager) SubscribeStdout(ctx context.Context, containerID, execID string) (<-chan OutputData, func(), error) {
	return m.subscribe(ctx, containerID, execID, func(p *processIO) *[]*subscriber { return &p.stdoutSubs })
}

// SubscribeStderr subscribes to a process's stderr stream.
// Returns a channel that receives output chunks and a done function.
// The caller MUST call the done function when finished processing the stream
// to signal that I/O is complete (this is required for WaitForIOComplete to work).
func (m *Manager) SubscribeStderr(ctx context.Context, containerID, execID string) (<-chan OutputData, func(), error) {
	return m.subscribe(ctx, containerID, execID, func(p *processIO) *[]*subscriber { return &p.stderrSubs })
}

func (m *Manager) subscribe(ctx context.Context, containerID, execID string, getSubs func(*processIO) *[]*subscriber) (<-chan OutputData, func(), error) {
	key := processKey{containerID: containerID, execID: execID}

	m.mu.RLock()
	pio, ok := m.processes[key]
	m.mu.RUnlock()

	if !ok {
		return nil, nil, fmt.Errorf("process not found: %w", errdefs.ErrNotFound)
	}

	pio.mu.Lock()
	buffered := m.drainBufferLocked(pio, getSubs)

	if pio.exited {
		pio.mu.Unlock()
		return m.subscribeToExitedProcess(containerID, execID, buffered)
	}

	ch, doneFunc := m.createActiveSubscriber(ctx, containerID, execID, pio, getSubs, buffered)
	pio.mu.Unlock()

	m.sendBufferedData(containerID, ch, buffered)
	return ch, doneFunc, nil
}

// drainBufferLocked extracts buffered data for the stream. Must be called with pio.mu held.
func (m *Manager) drainBufferLocked(pio *processIO, getSubs func(*processIO) *[]*subscriber) []OutputData {
	var buffered []OutputData
	if getSubs(pio) == &pio.stdoutSubs {
		buffered = append(buffered, pio.stdoutBuf...)
		pio.stdoutBuf = nil
		pio.stdoutBufBytes = 0
	} else {
		buffered = append(buffered, pio.stderrBuf...)
		pio.stderrBuf = nil
		pio.stderrBufBytes = 0
	}
	return buffered
}

// subscribeToExitedProcess creates a channel with buffered data and EOF for a process that has already exited.
func (m *Manager) subscribeToExitedProcess(containerID, execID string, buffered []OutputData) (<-chan OutputData, func(), error) {
	ch := make(chan OutputData, len(buffered)+1)
	for _, data := range buffered {
		ch <- data
	}
	ch <- OutputData{EOF: true}
	close(ch)

	log.L.WithField("container", containerID).WithField("exec", execID).
		WithField("bufferedChunks", len(buffered)).Debug("late subscriber received buffered data (process already exited)")

	return ch, func() {}, nil
}

// createActiveSubscriber creates a subscriber for a running process. Must be called with pio.mu held.
func (m *Manager) createActiveSubscriber(ctx context.Context, containerID, execID string, pio *processIO, getSubs func(*processIO) *[]*subscriber, buffered []OutputData) (chan OutputData, func()) {
	_, cancel := context.WithCancel(ctx)
	ch := make(chan OutputData, subscriberChannelBuffer)
	done := make(chan struct{})
	sub := &subscriber{ch: ch, cancel: cancel, done: done}

	subs := getSubs(pio)
	*subs = append(*subs, sub)
	pio.subscriberWg.Add(1)

	log.L.WithField("container", containerID).WithField("exec", execID).
		WithField("bufferedChunks", len(buffered)).Debug("subscriber registered")

	// Cleanup goroutine: wait for done, then clean up.
	go func() {
		<-done
		m.removeSubscriber(containerID, execID, sub, getSubs)
		pio.subscriberWg.Done()
	}()

	var doneOnce sync.Once
	doneFunc := func() {
		doneOnce.Do(func() { close(done) })
	}

	return ch, doneFunc
}

// sendBufferedData sends buffered data to the subscriber channel.
func (m *Manager) sendBufferedData(containerID string, ch chan OutputData, buffered []OutputData) {
	for _, data := range buffered {
		select {
		case ch <- data:
		default:
			log.L.WithField("container", containerID).Warn("dropping buffered data for slow subscriber")
		}
	}
}

func (m *Manager) removeSubscriber(containerID, execID string, sub *subscriber, getSubs func(*processIO) *[]*subscriber) {
	key := processKey{containerID: containerID, execID: execID}

	m.mu.RLock()
	pio, ok := m.processes[key]
	m.mu.RUnlock()

	if !ok {
		return
	}

	pio.mu.Lock()
	defer pio.mu.Unlock()

	subs := getSubs(pio)
	for i, s := range *subs {
		if s == sub {
			*subs = append((*subs)[:i], (*subs)[i+1:]...)
			break
		}
	}
}

// HasProcess checks if a process is registered.
func (m *Manager) HasProcess(containerID, execID string) bool {
	key := processKey{containerID: containerID, execID: execID}

	m.mu.RLock()
	_, ok := m.processes[key]
	m.mu.RUnlock()

	return ok
}

// WaitForIOComplete waits for all I/O to complete for the specified process.
// This waits for both:
// 1. fanOutReader goroutines to finish reading from process stdout/stderr
// 2. Subscriber RPC streams to finish sending data to the host
//
// This should be called before sending exit events to ensure all output has
// been fully transmitted to the host shim.
// Returns immediately if the process is not registered.
func (m *Manager) WaitForIOComplete(containerID, execID string) {
	key := processKey{containerID: containerID, execID: execID}

	m.mu.RLock()
	pio, ok := m.processes[key]
	m.mu.RUnlock()

	if !ok {
		return
	}

	// Wait for fanOutReader goroutines to finish reading all data
	pio.wg.Wait()
	log.L.WithField("container", containerID).WithField("exec", execID).Debug("fanOutReaders complete")

	// Wait for all subscriber RPC streams to finish sending data to host.
	// This is critical: the fanOutReaders may have finished reading and sent
	// data to subscriber channels, but the RPC streams might still be sending.
	pio.subscriberWg.Wait()
	log.L.WithField("container", containerID).WithField("exec", execID).Debug("I/O complete (all subscribers finished)")
}

func (m *Manager) bufferStdout(pio *processIO, data OutputData) {
	pio.stdoutBuf = append(pio.stdoutBuf, data)
	pio.stdoutBufBytes += len(data.Data)
	for pio.stdoutBufBytes > maxBufferedBytes && len(pio.stdoutBuf) > 0 {
		pio.stdoutBufBytes -= len(pio.stdoutBuf[0].Data)
		pio.stdoutBuf = pio.stdoutBuf[1:]
	}
}

func (m *Manager) bufferStderr(pio *processIO, data OutputData) {
	pio.stderrBuf = append(pio.stderrBuf, data)
	pio.stderrBufBytes += len(data.Data)
	for pio.stderrBufBytes > maxBufferedBytes && len(pio.stderrBuf) > 0 {
		pio.stderrBufBytes -= len(pio.stderrBuf[0].Data)
		pio.stderrBuf = pio.stderrBuf[1:]
	}
}
