// Package mounts provides platform-specific mount setup for VMs.
// It handles transforming OCI mounts to VM-compatible formats.
package mounts

import (
	"context"

	"github.com/containerd/containerd/api/types"

	"github.com/aledbf/qemubox/containerd/internal/host/vm"
)

// Manager handles platform-specific mount operations.
type Manager interface {
	// Setup prepares mounts for use inside the VM.
	// It transforms host mounts into VM-compatible formats using virtio-blk devices.
	Setup(ctx context.Context, vmi vm.Instance, id string, rootfsMounts []*types.Mount) (SetupResult, error)
}

// SetupResult carries the transformed mounts plus any cleanup required on delete.
type SetupResult struct {
	Mounts  []*types.Mount
	Cleanup func(context.Context) error
}

// New creates a platform-specific mount manager.
// Returns the appropriate implementation for the current OS.
func New() Manager {
	return newManager()
}
