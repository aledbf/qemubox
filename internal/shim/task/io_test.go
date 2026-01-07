package task

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/ttrpc"

	"github.com/aledbf/qemubox/containerd/internal/host/vm"
)

var errNotImplemented = errors.New("not implemented")

// mockVMInstance implements vm.Instance for testing
type mockVMInstance struct {
	streamID uint32
	conn     *mockConn
}

func (m *mockVMInstance) AddDisk(ctx context.Context, blockID, mountPath string, opts ...vm.MountOpt) error {
	return nil
}

func (m *mockVMInstance) AddTAPNIC(ctx context.Context, tapName string, mac net.HardwareAddr) error {
	return nil
}

func (m *mockVMInstance) AddNIC(ctx context.Context, endpoint string, mac net.HardwareAddr, mode vm.NetworkMode, features, flags uint32) error {
	return nil
}

func (m *mockVMInstance) Start(ctx context.Context, opts ...vm.StartOpt) error {
	return nil
}

func (m *mockVMInstance) Shutdown(ctx context.Context) error {
	return nil
}

func (m *mockVMInstance) Client() (*ttrpc.Client, error) {
	return nil, errNotImplemented
}

func (m *mockVMInstance) DialClient(ctx context.Context) (*ttrpc.Client, error) {
	return nil, errNotImplemented
}

func (m *mockVMInstance) StartStream(ctx context.Context) (uint32, net.Conn, error) {
	m.streamID++
	m.conn = &mockConn{}
	return m.streamID, m.conn, nil
}

func (m *mockVMInstance) VMInfo() vm.VMInfo {
	return vm.VMInfo{}
}

func (m *mockVMInstance) CPUHotplugger() (vm.CPUHotplugger, error) {
	return nil, errNotImplemented
}

// mockConn implements net.Conn for testing
type mockConn struct {
	closed bool
}

func (m *mockConn) Read(b []byte) (int, error)         { return 0, io.EOF }
func (m *mockConn) Write(b []byte) (int, error)        { return len(b), nil }
func (m *mockConn) Close() error                       { m.closed = true; return nil }
func (m *mockConn) LocalAddr() net.Addr                { return nil }
func (m *mockConn) RemoteAddr() net.Addr               { return nil }
func (m *mockConn) SetDeadline(t time.Time) error      { return nil }
func (m *mockConn) SetReadDeadline(t time.Time) error  { return nil }
func (m *mockConn) SetWriteDeadline(t time.Time) error { return nil }

func withTempCwd(t *testing.T, fn func(tmp string)) {
	t.Helper()
	tmp := t.TempDir()
	t.Chdir(tmp)
	fn(tmp)
}

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
	withTempCwd(t, func(tmp string) {
		binaryDir := filepath.Join(tmp, "binary:")
		if err := os.MkdirAll(binaryDir, 0750); err != nil {
			t.Fatalf("mkdir binary dir: %v", err)
		}
		binaryPath := filepath.Join(binaryDir, "logger")
		if err := os.WriteFile(binaryPath, []byte{}, 0600); err != nil {
			t.Fatalf("write binary: %v", err)
		}

		svc := &service{}
		ss := &mockVMInstance{}

		sio := stdio.Stdio{
			Stdin:  "",
			Stdout: "binary://logger",
			Stderr: "binary://logger",
		}

		ctx := context.Background()
		pio, forwarder, err := svc.forwardIO(ctx, ss, sio)

		if err != nil {
			t.Fatalf("binary scheme should be supported, got error: %v", err)
		}

		if pio.Stdout == "" {
			t.Error("expected stdout to be set for binary scheme")
		}

		if forwarder != nil {
			defer func() {
				_ = forwarder.Shutdown(ctx)
			}()
		}
	})
}

// TestPipeSchemeSupport tests that pipe scheme is recognized
func TestPipeSchemeSupport(t *testing.T) {
	withTempCwd(t, func(tmp string) {
		pipeDir := filepath.Join(tmp, "pipe:")
		if err := os.MkdirAll(pipeDir, 0750); err != nil {
			t.Fatalf("mkdir pipe dir: %v", err)
		}
		pipePath := filepath.Join(pipeDir, "stdout")
		if err := syscall.Mkfifo(pipePath, 0600); err != nil {
			t.Fatalf("mkfifo: %v", err)
		}
		reader, err := os.OpenFile(pipePath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatalf("open fifo reader: %v", err)
		}
		defer reader.Close()

		svc := &service{}
		ss := &mockVMInstance{}

		sio := stdio.Stdio{
			Stdin:  "",
			Stdout: "pipe://stdout",
			Stderr: "pipe://stdout",
		}

		ctx := context.Background()
		pio, forwarder, err := svc.forwardIO(ctx, ss, sio)

		if err != nil {
			t.Fatalf("pipe scheme should be supported, got error: %v", err)
		}

		if pio.Stdout == "" {
			t.Error("expected stdout to be set for pipe scheme")
		}

		if forwarder != nil {
			defer func() {
				_ = forwarder.Shutdown(ctx)
			}()
		}
	})
}

func TestForwardIO(t *testing.T) {
	tests := []struct {
		name          string
		sio           stdio.Stdio
		wantErr       bool
		checkPassthru bool // check stdout/stderr pass through unchanged
		checkNull     bool // check IsNull()
		checkNoopFwd  bool // check forwarder is noopForwarder
	}{
		{
			name: "stream scheme passes through",
			sio: stdio.Stdio{
				Stdout: "stream://123",
				Stderr: "stream://456",
			},
			checkPassthru: true,
		},
		{
			name: "unsupported scheme returns error",
			sio: stdio.Stdio{
				Stdout: "unsupported://invalid",
			},
			wantErr: true,
		},
		{
			name:         "null stdio returns noopForwarder",
			sio:          stdio.Stdio{},
			checkNull:    true,
			checkNoopFwd: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &service{}
			ss := &mockVMInstance{}
			ctx := context.Background()

			pio, forwarder, err := svc.forwardIO(ctx, ss, tt.sio)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.checkPassthru {
				if pio.Stdout != tt.sio.Stdout {
					t.Errorf("stdout = %q, want %q", pio.Stdout, tt.sio.Stdout)
				}
				if pio.Stderr != tt.sio.Stderr {
					t.Errorf("stderr = %q, want %q", pio.Stderr, tt.sio.Stderr)
				}
			}

			if tt.checkNull && !pio.IsNull() {
				t.Error("expected null stdio")
			}

			if tt.checkNoopFwd {
				if forwarder == nil {
					t.Error("expected noopForwarder (Null Object Pattern)")
				} else if _, ok := forwarder.(*noopForwarder); !ok {
					t.Errorf("expected noopForwarder, got %T", forwarder)
				}
			}

			if forwarder != nil {
				_ = forwarder.Shutdown(ctx)
			}
		})
	}
}

func TestForwardIOUsesDirectForFileScheme(t *testing.T) {
	withTempCwd(t, func(tmp string) {
		svc := &service{}
		ss := &mockVMInstance{}

		logPath := filepath.Join(tmp, "out.log")
		sio := stdio.Stdio{
			Stdin:    "",
			Stdout:   "file://" + logPath,
			Stderr:   "file://" + logPath,
			Terminal: false,
		}

		ctx := context.Background()
		_, forwarder, err := svc.forwardIOWithIDs(ctx, ss, "cid", "", sio)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := forwarder.(*directForwarder); !ok {
			t.Fatalf("expected direct forwarder, got %T", forwarder)
		}
	})
}
