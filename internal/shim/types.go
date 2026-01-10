// Package shim contains types and plugin definitions used by the spinbox shim.
package shim

import "github.com/containerd/plugin"

const (
	// TTRPCPlugin implements a ttrpc service
	TTRPCPlugin plugin.Type = "io.containerd.ttrpc.v1"

	// StreamingPlugin implements a stream manager
	StreamingPlugin plugin.Type = "spinbox.streaming.v1"
)
