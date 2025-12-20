package main

import (
	"context"

	_ "github.com/aledbf/beacon/containerd/shim"
	"github.com/aledbf/beacon/containerd/shim/manager"
	"github.com/containerd/containerd/v2/pkg/shim"
)

func main() {
	//nolint:staticcheck // shim.Run ignores the context on this build.
	ctx := context.Background()
	shim.Run(ctx, manager.NewShimManager("io.containerd.beaconbox.v1"))
}
