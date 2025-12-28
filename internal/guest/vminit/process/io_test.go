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
	ctx := namespaces.WithNamespace(context.Background(), "test")
	uri, _ := url.Parse("binary:///bin/echo?test")

	before := descriptorCount(t)

	io, err := NewBinaryIO(ctx, "1", uri)
	if err != nil {
		t.Fatal(err)
	}

	err = io.Close()
	if err != nil {
		t.Fatal(err)
	}

	after := descriptorCount(t)
	if before != after-1 { // one descriptor must be closed from shim logger side
		t.Fatalf("some descriptors weren't closed (%d != %d -1)", before, after)
	}
}

func TestNewBinaryIOCleanup(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")
	uri, _ := url.Parse("binary:///not/existing")

	before := descriptorCount(t)
	_, err := NewBinaryIO(ctx, "2", uri)
	if err == nil {
		t.Fatal("error expected for invalid binary")
	}

	after := descriptorCount(t)
	if before != after {
		t.Fatalf("some descriptors weren't closed (%d != %d)", before, after)
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
