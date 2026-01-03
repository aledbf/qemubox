// Package events provides event exchange utilities for vminit.
package events

import (
	"github.com/containerd/containerd/v2/core/events/exchange"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.EventPlugin,
		ID:   "exchange",
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			return NewExchange(), nil
		},
	})
}

// Exchange is an alias to containerd's event exchange implementation.
type Exchange = exchange.Exchange

// NewExchange returns a new event Exchange.
func NewExchange() *Exchange {
	return exchange.NewExchange()
}
