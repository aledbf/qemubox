//go:build darwin

package task

import (
	"context"
	"fmt"

	"github.com/aledbf/qemubox/containerd/internal/host/network"
	"github.com/aledbf/qemubox/containerd/internal/host/vm"
)

// setupNetworking is a stub for Darwin
func setupNetworking(ctx context.Context, nm network.NetworkManager, vmi vm.Instance, id, netnsPath string) (*vm.NetworkConfig, error) {
	return nil, fmt.Errorf("not supported on darwin")
}
