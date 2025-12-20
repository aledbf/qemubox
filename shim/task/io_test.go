package task

import (
	"context"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/stdio"
)

// mockStreamCreator implements streamCreator for testing
type mockStreamCreator struct {
	streamID uint32
	conn     *mockConn
}

func (m *mockStreamCreator) StartStream(ctx context.Context) (uint32, net.Conn, error) {
	m.streamID++
	m.conn = &mockConn{}
	return m.streamID, m.conn, nil
}

// mockConn implements net.Conn for testing
type mockConn struct {
	closed bool
}

func (m *mockConn) Read(b []byte) (int, error)         { return 0, nil }
func (m *mockConn) Write(b []byte) (int, error)        { return len(b), nil }
func (m *mockConn) Close() error                       { m.closed = true; return nil }
func (m *mockConn) LocalAddr() net.Addr                { return nil }
func (m *mockConn) RemoteAddr() net.Addr               { return nil }
func (m *mockConn) SetDeadline(t time.Time) error      { return nil }
func (m *mockConn) SetReadDeadline(t time.Time) error  { return nil }
func (m *mockConn) SetWriteDeadline(t time.Time) error { return nil }

// TestParseStdioURI tests URI parsing for different I/O schemes
func TestParseStdioURI(t *testing.T) {
	testCases := []struct {
		name       string
		uri        string
		wantScheme string
		wantErr    bool
	}{
		{
			name:       "fifo scheme",
			uri:        "fifo:///run/containerd/test.fifo",
			wantScheme: "fifo",
			wantErr:    false,
		},
		{
			name:       "file scheme",
			uri:        "file:///var/log/container.log",
			wantScheme: "file",
			wantErr:    false,
		},
		{
			name:       "binary scheme",
			uri:        "binary:///usr/bin/logger",
			wantScheme: "binary",
			wantErr:    false,
		},
		{
			name:       "pipe scheme",
			uri:        "pipe://",
			wantScheme: "pipe",
			wantErr:    false,
		},
		{
			name:       "stream scheme",
			uri:        "stream://123",
			wantScheme: "stream",
			wantErr:    false,
		},
		{
			name:       "default to fifo",
			uri:        "/run/containerd/test.fifo",
			wantScheme: "fifo",
			wantErr:    false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.uri)
			if err != nil {
				if !tc.wantErr {
					t.Fatalf("unexpected error parsing URI: %v", err)
				}
				return
			}
			if tc.wantErr {
				t.Fatal("expected error but got none")
			}

			if u.Scheme == "" {
				u.Scheme = defaultScheme
			}

			if u.Scheme != tc.wantScheme {
				t.Errorf("got scheme %q, want %q", u.Scheme, tc.wantScheme)
			}
		})
	}
}

// TestBinarySchemeSupport tests that binary scheme is recognized
func TestBinarySchemeSupport(t *testing.T) {
	svc := &service{}
	ss := &mockStreamCreator{}

	sio := stdio.Stdio{
		Stdin:  "",
		Stdout: "binary:///usr/bin/logger",
		Stderr: "binary:///usr/bin/logger",
	}

	ctx := context.Background()
	pio, cleanup, err := svc.forwardIO(ctx, ss, sio)

	if err != nil {
		t.Fatalf("binary scheme should be supported, got error: %v", err)
	}

	if pio.Stdout == "" {
		t.Error("expected stdout to be set for binary scheme")
	}

	if cleanup != nil {
		defer cleanup(ctx)
	}
}

// TestPipeSchemeSupport tests that pipe scheme is recognized
func TestPipeSchemeSupport(t *testing.T) {
	svc := &service{}
	ss := &mockStreamCreator{}

	sio := stdio.Stdio{
		Stdin:  "",
		Stdout: "pipe://",
		Stderr: "pipe://",
	}

	ctx := context.Background()
	pio, cleanup, err := svc.forwardIO(ctx, ss, sio)

	if err != nil {
		t.Fatalf("pipe scheme should be supported, got error: %v", err)
	}

	if pio.Stdout == "" {
		t.Error("expected stdout to be set for pipe scheme")
	}

	if cleanup != nil {
		defer cleanup(ctx)
	}
}

// TestStreamSchemePassthrough tests that stream scheme passes through
func TestStreamSchemePassthrough(t *testing.T) {
	svc := &service{}
	ss := &mockStreamCreator{}

	sio := stdio.Stdio{
		Stdin:  "",
		Stdout: "stream://123",
		Stderr: "stream://456",
	}

	ctx := context.Background()
	pio, cleanup, err := svc.forwardIO(ctx, ss, sio)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Stream scheme should pass through unchanged
	if pio.Stdout != sio.Stdout {
		t.Errorf("expected stdout %q, got %q", sio.Stdout, pio.Stdout)
	}
	if pio.Stderr != sio.Stderr {
		t.Errorf("expected stderr %q, got %q", sio.Stderr, pio.Stderr)
	}

	// No cleanup should be needed for stream scheme
	if cleanup != nil {
		t.Error("expected nil cleanup for stream scheme")
	}
}

// TestUnsupportedScheme tests that unsupported schemes return error
func TestUnsupportedScheme(t *testing.T) {
	svc := &service{}
	ss := &mockStreamCreator{}

	sio := stdio.Stdio{
		Stdin:  "",
		Stdout: "unsupported://invalid",
		Stderr: "",
	}

	ctx := context.Background()
	_, _, err := svc.forwardIO(ctx, ss, sio)

	if err == nil {
		t.Fatal("expected error for unsupported scheme, got nil")
	}
}

// TestNullStdio tests that null/empty stdio is handled correctly
func TestNullStdio(t *testing.T) {
	svc := &service{}
	ss := &mockStreamCreator{}

	sio := stdio.Stdio{
		Stdin:  "",
		Stdout: "",
		Stderr: "",
	}

	ctx := context.Background()
	pio, cleanup, err := svc.forwardIO(ctx, ss, sio)

	if err != nil {
		t.Fatalf("unexpected error for null stdio: %v", err)
	}

	if !pio.IsNull() {
		t.Error("expected null stdio to remain null")
	}

	if cleanup != nil {
		t.Error("expected nil cleanup for null stdio")
	}
}
