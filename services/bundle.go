package services

import (
	"context"
	"os"
	"path/filepath"

	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"

	api "github.com/aledbf/beacon/containerd/api/services/bundle/v1"
	"github.com/aledbf/beacon/containerd/vminit/bundle"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: cplugins.TTRPCPlugin,
		ID:   "bundle",
		Requires: []plugin.Type{
			cplugins.EventPlugin,
			cplugins.InternalPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			if err := os.MkdirAll(bundle.RootDir, 0755); err != nil {
				return nil, err
			}
			return &service{
				bundleRoot: bundle.RootDir,
			}, nil
		},
	})
}

type service struct {
	bundleRoot string
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	api.RegisterTTRPCBundleService(server, s)
	return nil
}

func (s *service) Create(ctx context.Context, r *api.CreateRequest) (*api.CreateResponse, error) {
	d := filepath.Join(s.bundleRoot, r.ID)
	if err := os.Mkdir(d, 0755); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	log.G(ctx).Infof("Creating bundle at %s", d)
	if err := os.Mkdir(filepath.Join(d, "rootfs"), 0755); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	for f, b := range r.Files {
		if err := os.WriteFile(filepath.Join(d, f), b, 0644); err != nil {
			return nil, errgrpc.ToGRPC(err)
		}
	}
	return &api.CreateResponse{
		Bundle: d,
	}, nil
}
