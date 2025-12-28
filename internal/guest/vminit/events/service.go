package events

import (
	"context"
	"io"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/containerd/v2/pkg/protobuf"
	cplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	"github.com/containerd/typeurl/v2"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/aledbf/qemubox/containerd/api/services/vmevents/v1"
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
	log.G(ctx).Info("vmevents stream opened")
	events, errs := s.sub.Subscribe(ctx)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				log.G(ctx).Warn("vmevents stream events channel closed")
				return io.EOF
			}
			if event == nil {
				log.G(ctx).Warn("vmevents stream received nil event")
				continue
			}
			if err := ss.Send(toProto(event)); err != nil {
				log.G(ctx).WithError(err).WithFields(log.Fields{
					"topic":     event.Topic,
					"namespace": event.Namespace,
				}).Warn("vmevents stream send failed")
				return err
			}
		case err, ok := <-errs:
			if !ok {
				log.G(ctx).Warn("vmevents stream error channel closed")
				return nil
			}
			if err != nil {
				log.G(ctx).WithError(err).Warn("vmevents stream error")
			} else {
				log.G(ctx).Warn("vmevents stream closed without error")
			}
			return err
		case <-ctx.Done():
			log.G(ctx).WithError(ctx.Err()).Warn("vmevents stream context done")
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
