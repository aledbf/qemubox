package main

import (
	"context"
	"fmt"
	"os"

	"github.com/containerd/containerd/v2/pkg/shim"

	"github.com/aledbf/qemubox/containerd/internal/config"
	"github.com/aledbf/qemubox/containerd/internal/shim/manager"

	// Register shim plugin with containerd runtime
	_ "github.com/aledbf/qemubox/containerd/internal/shim"
)

func main() {
	// Load configuration first - fail fast if config is missing or invalid
	_, err := config.Get()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Failed to load qemubox configuration: %v\n", err)
		fmt.Fprintf(os.Stderr, "\nPlease create a configuration file at /etc/qemubox/config.json\n")
		fmt.Fprintf(os.Stderr, "See examples/config.json for a template with default values.\n")
		fmt.Fprintf(os.Stderr, "\nAlternatively, set QEMUBOX_CONFIG to specify a custom config file location.\n")
		os.Exit(1)
	}

	// Log level is controlled by containerd configuration, not the shim
	ctx := context.Background()
	shim.Run(ctx, manager.NewShimManager("io.containerd.qemubox.v1"))
}
