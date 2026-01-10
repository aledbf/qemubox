//go:build darwin

// Package task implements the containerd task service for spinbox runtime.
// This file provides stub definitions for Darwin.
// The actual implementation is only available on Linux.
package task

import (
	"context"
	"fmt"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
)

// NewTaskService returns an error on Darwin (not supported).
func NewTaskService(ctx context.Context, publisher shim.Publisher, sd shutdown.Service) (taskAPI.TTRPCTaskService, error) {
	return nil, fmt.Errorf("task service not supported on darwin")
}
