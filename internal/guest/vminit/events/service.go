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

	"github.com/spin-stack/spinbox/api/services/vmevents/v1"
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

	// Add debug logging to track stream lifecycle
	defer func() {
		log.G(ctx).Info("vmevents stream handler exiting")
	}()

	eventCount := 0
	for {
		select {
		case event, ok := <-events:
			if !ok {
				log.G(ctx).WithField("events_sent", eventCount).Warn("vmevents stream events channel closed")
				return io.EOF
			}
			if event == nil {
				log.G(ctx).Warn("vmevents stream received nil event")
				continue
			}
			log.G(ctx).WithFields(log.Fields{
				"topic":     event.Topic,
				"namespace": event.Namespace,
				"event_num": eventCount,
			}).Debug("vmevents sending event")
			if err := ss.Send(toProto(event)); err != nil {
				log.G(ctx).WithError(err).WithFields(log.Fields{
					"topic":       event.Topic,
					"namespace":   event.Namespace,
					"events_sent": eventCount,
				}).Warn("vmevents stream send failed")
				return err
			}
			eventCount++
			log.G(ctx).WithField("event_num", eventCount).Debug("vmevents event sent successfully")
		case err, ok := <-errs:
			if !ok {
				log.G(ctx).WithField("events_sent", eventCount).Warn("vmevents stream error channel closed")
				return nil
			}
			if err != nil {
				log.G(ctx).WithError(err).WithField("events_sent", eventCount).Error("vmevents stream error from subscriber")
			} else {
				log.G(ctx).WithField("events_sent", eventCount).Warn("vmevents stream closed without error")
			}
			return err
		case <-ctx.Done():
			// Context cancellation is expected during shutdown - don't log as warning
			log.G(ctx).WithField("events_sent", eventCount).Debug("vmevents stream context cancelled")
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
