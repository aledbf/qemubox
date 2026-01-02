package stdio

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

// mockReadCloser wraps a bytes.Buffer to implement io.ReadCloser
type mockReadCloser struct {
	*bytes.Buffer
	closed bool
}

func (m *mockReadCloser) Close() error {
	m.closed = true
	return nil
}

// blockingReader is a reader that blocks until closed
type blockingReader struct {
	data   chan []byte
	closed chan struct{}
}

func newBlockingReader() *blockingReader {
	return &blockingReader{
		data:   make(chan []byte, 10),
		closed: make(chan struct{}),
	}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	select {
	case data := <-r.data:
		n := copy(p, data)
		return n, nil
	case <-r.closed:
		return 0, io.EOF
	}
}

func (r *blockingReader) Write(data []byte) {
	select {
	case r.data <- data:
	default:
	}
}

func (r *blockingReader) Close() error {
	close(r.closed)
	return nil
}

func TestManagerRegisterUnregister(t *testing.T) {
	m := NewManager()

	stdout := newBlockingReader()
	stderr := newBlockingReader()
	stdin := &mockWriteCloser{}

	m.Register("container1", "", stdin, stdout, stderr)

	if !m.HasProcess("container1", "") {
		t.Error("expected process to be registered")
	}

	stdout.Close()
	stderr.Close()

	// Allow fan-out goroutines to finish
	time.Sleep(50 * time.Millisecond)

	m.Unregister("container1", "")

	if m.HasProcess("container1", "") {
		t.Error("expected process to be unregistered")
	}
}

func TestManagerSubscribeStdout(t *testing.T) {
	m := NewManager()

	stdout := newBlockingReader()
	stdin := &mockWriteCloser{}

	m.Register("container1", "", stdin, stdout, nil)
	defer m.Unregister("container1", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, done, err := m.SubscribeStdout(ctx, "container1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer done()

	// Send some data
	testData := []byte("hello world")
	stdout.Write(testData)

	// Wait for data
	select {
	case data := <-ch:
		if !bytes.Equal(data.Data, testData) {
			t.Errorf("expected %q, got %q", testData, data.Data)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for data")
	}

	// Close stdout and expect EOF
	stdout.Close()

	select {
	case data := <-ch:
		if !data.EOF {
			t.Error("expected EOF")
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for EOF")
	}
}

func TestManagerSubscribeToExitedProcess(t *testing.T) {
	m := NewManager()

	stdout := newBlockingReader()
	stdin := &mockWriteCloser{}

	m.Register("container1", "", stdin, stdout, nil)

	// Write some data before closing
	stdout.Write([]byte("buffered data"))
	stdout.Close()

	// Allow fan-out goroutines to finish
	time.Sleep(50 * time.Millisecond)

	m.Unregister("container1", "")

	// Subscribe after process exited - should get ErrNotFound
	ctx := context.Background()
	_, _, err := m.SubscribeStdout(ctx, "container1", "")
	if err == nil {
		t.Error("expected error when subscribing to unregistered process")
	}
}

func TestManagerBufferedOutput(t *testing.T) {
	m := NewManager()

	stdout := newBlockingReader()
	stdin := &mockWriteCloser{}

	m.Register("container1", "", stdin, stdout, nil)
	defer func() {
		// Close stdout to allow fan-out goroutines to exit
		stdout.Close()
		time.Sleep(50 * time.Millisecond)
		m.Unregister("container1", "")
	}()

	// Write data before any subscriber
	testData := []byte("buffered before subscribe")
	stdout.Write(testData)

	// Allow fan-out to buffer the data
	time.Sleep(50 * time.Millisecond)

	ctx := context.Background()
	ch, done, err := m.SubscribeStdout(ctx, "container1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer done()

	// Should receive buffered data
	select {
	case data := <-ch:
		if !bytes.Equal(data.Data, testData) {
			t.Errorf("expected %q, got %q", testData, data.Data)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("timeout waiting for buffered data")
	}
}

func TestManagerConcurrentSubscribers(t *testing.T) {
	m := NewManager()

	stdout := newBlockingReader()
	stdin := &mockWriteCloser{}

	m.Register("container1", "", stdin, stdout, nil)
	defer func() {
		stdout.Close()
		time.Sleep(50 * time.Millisecond)
		m.Unregister("container1", "")
	}()

	ctx := context.Background()
	const numSubscribers = 5

	var wg sync.WaitGroup
	received := make([]bool, numSubscribers)

	for i := 0; i < numSubscribers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ch, done, err := m.SubscribeStdout(ctx, "container1", "")
			if err != nil {
				t.Errorf("subscriber %d: unexpected error: %v", idx, err)
				return
			}
			defer done()

			select {
			case data := <-ch:
				if bytes.Equal(data.Data, []byte("broadcast")) {
					received[idx] = true
				}
			case <-time.After(time.Second):
				t.Errorf("subscriber %d: timeout", idx)
			}
		}(i)
	}

	// Give subscribers time to register
	time.Sleep(50 * time.Millisecond)

	// Send data that all subscribers should receive
	stdout.Write([]byte("broadcast"))

	wg.Wait()

	for i, got := range received {
		if !got {
			t.Errorf("subscriber %d did not receive data", i)
		}
	}
}

func TestManagerWaitForIOComplete(t *testing.T) {
	m := NewManager()

	stdout := newBlockingReader()
	stdin := &mockWriteCloser{}

	m.Register("container1", "", stdin, stdout, nil)
	defer func() {
		// Note: stdout is already closed in the test
		time.Sleep(50 * time.Millisecond)
		m.Unregister("container1", "")
	}()

	ctx := context.Background()
	ch, done, err := m.SubscribeStdout(ctx, "container1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Close stdout to signal EOF - this will send EOF to subscriber
	stdout.Close()

	// Drain until we see EOF, then call done()
	// Use select with timeout to avoid blocking forever
	go func() {
		timeout := time.After(5 * time.Second)
		for {
			select {
			case data, ok := <-ch:
				if !ok || data.EOF {
					done() // Signal completion when we see EOF or channel close
					return
				}
			case <-timeout:
				done() // Safety timeout
				return
			}
		}
	}()

	// Give time for EOF to be delivered and done() to be called
	time.Sleep(100 * time.Millisecond)

	// WaitForIOComplete should return quickly now
	start := time.Now()
	m.WaitForIOComplete("container1", "")
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("WaitForIOComplete took too long: %v", elapsed)
	}
}

func TestManagerDroppedStats(t *testing.T) {
	m := NewManager()

	chunks, bytes := m.DroppedStats()
	if chunks != 0 || bytes != 0 {
		t.Errorf("expected zero stats, got chunks=%d, bytes=%d", chunks, bytes)
	}
}

// mockWriteCloser is a simple mock for io.WriteCloser
type mockWriteCloser struct {
	bytes.Buffer
	closed bool
}

func (m *mockWriteCloser) Close() error {
	m.closed = true
	return nil
}
