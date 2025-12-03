//go:build darwin

package task

import (
	"context"
	"fmt"

	"github.com/aledbf/beacon/containerd/network"
	"github.com/aledbf/beacon/containerd/vm"
	"github.com/aledbf/beacon/containerd/vm/cloudhypervisor"
)

// setupNetworking is a stub for Darwin
func setupNetworking(ctx context.Context, nm network.NetworkManagerInterface, vmi *cloudhypervisor.Instance, id, netnsPath string) (*vm.NetworkConfig, error) {
	return nil, fmt.Errorf("not supported on darwin")
}
