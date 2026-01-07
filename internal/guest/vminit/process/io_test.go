//go:build linux

package process

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/pkg/namespaces"
)

func TestNewBinaryIO(t *testing.T) {
	tests := []struct {
		name           string
		uri            string
		wantErr        bool
		fdDeltaOnClose int // expected fd count change after close (0 for errors, -1 for success)
	}{
		{
			name:           "valid binary",
			uri:            "binary:///bin/echo?test",
			wantErr:        false,
			fdDeltaOnClose: -1, // one descriptor closed from shim logger side
		},
		{
			name:           "invalid binary cleans up",
			uri:            "binary:///not/existing",
			wantErr:        true,
			fdDeltaOnClose: 0, // all descriptors cleaned up on error
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := namespaces.WithNamespace(context.Background(), "test")
			uri, _ := url.Parse(tt.uri)

			before := descriptorCount(t)

			io, err := NewBinaryIO(ctx, t.Name(), uri)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				after := descriptorCount(t)
				if before != after {
					t.Fatalf("descriptors leaked on error (%d != %d)", before, after)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if err := io.Close(); err != nil {
				t.Fatalf("Close failed: %v", err)
			}

			after := descriptorCount(t)
			if before != after+tt.fdDeltaOnClose {
				t.Fatalf("fd count mismatch: before=%d, after=%d, expected delta=%d", before, after, tt.fdDeltaOnClose)
			}
		})
	}
}

func descriptorCount(t *testing.T) int {
	t.Helper()
	const dir = "/proc/self/fd"
	files, _ := os.ReadDir(dir)

	// Go 1.23+ uses pidfd instead of PID for processes started by a user,
	// if possible (see https://go.dev/cl/570036). As a side effect, every
	// os.StartProcess or os.FindProcess call results in an extra opened
	// file descriptor, which is only closed in p.Wait or p.Release.
	//
	// To retain compatibility with previous Go versions (or Go 1.23+
	// behavior on older kernels), let's not count pidfds.
	//
	// Note: If the proposal to check for internal file descriptors
	// (https://go.dev/issues/67639) is accepted, we can use that
	// instead to detect internal fds in use by the Go runtime.
	count := 0
	for _, file := range files {
		sym, err := os.Readlink(filepath.Join(dir, file.Name()))
		// Either pidfd:[70517] or anon_inode:[pidfd] (on Linux 5.4).
		if err == nil && strings.Contains(sym, "pidfd") {
			continue
		}
		count++
	}

	return count
}
