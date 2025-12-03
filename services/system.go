package services

import (
	"context"
	"os"

	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	emptypb "google.golang.org/protobuf/types/known/emptypb"

	api "github.com/aledbf/beacon/containerd/api/services/system/v1"
)

const (
	// TTRPCPlugin implements a ttrpc service
	TTRPCPlugin plugin.Type = "io.containerd.ttrpc.v1"
)

var _ api.TTRPCSystemService = &service{}

func init() {
	registry.Register(&plugin.Registration{
		Type:   TTRPCPlugin,
		ID:     "system",
		InitFn: initFunc,
	})
}

func initFunc(ic *plugin.InitContext) (interface{}, error) {
	return &service{}, nil
}

func (s *service) Info(ctx context.Context, _ *emptypb.Empty) (*api.InfoResponse, error) {
	v, err := os.ReadFile("/proc/version")
	if err != nil && !os.IsNotExist(err) {
		return nil, errgrpc.ToGRPC(err)
	}
	return &api.InfoResponse{
		Version:       "dev",
		KernelVersion: string(v),
	}, nil
}
