package events

import (
	"context"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/containerd/v2/pkg/protobuf"
	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	"github.com/containerd/typeurl/v2"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/aledbf/beacon/containerd/api/services/vmevents/v1"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: cplugins.TTRPCPlugin,
		ID:   "vmevents",
		Requires: []plugin.Type{
			cplugins.EventPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			// Get the event exchange plugin
			p, err := ic.GetByID(cplugins.EventPlugin, "exchange")
			if err != nil {
				return nil, err
			}
			exchange, ok := p.(Subscriber)
			if !ok {
				return nil, plugin.ErrSkipPlugin
			}
			return NewService(exchange), nil
		},
	})
}

// Subscriber provides access to the event stream.
type Subscriber interface {
	Subscribe(ctx context.Context, topics ...string) (<-chan *events.Envelope, <-chan error)
}

type service struct {
	sub Subscriber
}

// NewService returns a TTRPC-backed events service.
func NewService(s Subscriber) *service {
	return &service{
		sub: s,
	}
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	vmevents.RegisterTTRPCEventsService(server, s)
	return nil
}

func (s *service) Stream(ctx context.Context, _ *emptypb.Empty, ss vmevents.TTRPCEvents_StreamServer) error {
	events, errs := s.sub.Subscribe(ctx)
	for {
		select {
		case event := <-events:
			if err := ss.Send(toProto(event)); err != nil {
				return err
			}
		case err := <-errs:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func toProto(env *events.Envelope) *types.Envelope {
	return &types.Envelope{
		Timestamp: protobuf.ToTimestamp(env.Timestamp),
		Namespace: env.Namespace,
		Topic:     env.Topic,
		Event:     typeurl.MarshalProto(env.Event),
	}
}
