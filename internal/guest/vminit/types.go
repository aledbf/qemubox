// Package vminit contains types and plugin definitions used by vminit daemon.
package vminit

import "github.com/containerd/plugin"

const (
	// StreamingPlugin implements a stream manager
	StreamingPlugin plugin.Type = "spinbox.streaming.v1"
)
