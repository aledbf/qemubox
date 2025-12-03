//go:build darwin

package manager

import (
	"github.com/containerd/containerd/v2/pkg/shim"
)

// NewShimManager is a stub for Darwin
func NewShimManager(name string) shim.Manager {
	panic("shim manager not supported on darwin")
}
