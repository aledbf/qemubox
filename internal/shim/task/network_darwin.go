//go:build darwin

package task

import (
	"context"
	"fmt"

	"github.com/aledbf/qemubox/containerd/internal/host/network"
)

func initNetworkManager(ctx context.Context) (network.NetworkManager, error) {
	_ = ctx
	return nil, fmt.Errorf("network manager not supported on darwin")
}
