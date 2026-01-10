//go:build darwin

package network

import (
	"context"
	"fmt"

	"github.com/spin-stack/spinbox/internal/host/network"
	"github.com/spin-stack/spinbox/internal/host/vm"
)

type darwinManager struct{}

func newManager() Manager {
	return &darwinManager{}
}

func (m *darwinManager) InitNetworkManager(ctx context.Context) (network.NetworkManager, error) {
	return nil, fmt.Errorf("network manager not supported on darwin")
}

func (m *darwinManager) Setup(ctx context.Context, nm network.NetworkManager, vmi vm.Instance, containerID, netnsPath string) (*vm.NetworkConfig, error) {
	return nil, fmt.Errorf("networking not supported on darwin")
}
