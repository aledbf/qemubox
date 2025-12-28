package vminit

import (
	"fmt"

	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/bundle"
	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/stream"
	"github.com/aledbf/qemubox/containerd/internal/guest/vminit/task"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: cplugins.TTRPCPlugin,
		ID:   "task",
		Requires: []plugin.Type{
			cplugins.EventPlugin,
			cplugins.InternalPlugin,
			StreamingPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			pp, err := ic.GetSingle(cplugins.EventPlugin)
			if err != nil {
				return nil, err
			}
			ss, err := ic.GetByID(cplugins.InternalPlugin, "shutdown")
			if err != nil {
				return nil, err
			}
			sm, err := ic.GetByID(StreamingPlugin, "vsock")
			if err != nil {
				return nil, err
			}
			publisher, ok := pp.(events.Publisher)
			if !ok {
				return nil, fmt.Errorf("unexpected event publisher type %T", pp)
			}
			shutdownSvc, ok := ss.(shutdown.Service)
			if !ok {
				return nil, fmt.Errorf("unexpected shutdown service type %T", ss)
			}
			streamMgr, ok := sm.(stream.Manager)
			if !ok {
				return nil, fmt.Errorf("unexpected stream manager type %T", sm)
			}
			return task.NewTaskService(ic.Context, bundle.RootDir, publisher, shutdownSvc, streamMgr)
		},
	})
}
