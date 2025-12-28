package main

import (
	"context"
	"fmt"
	"os"

	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/log"

	"github.com/aledbf/qemubox/containerd/internal/config"
	_ "github.com/aledbf/qemubox/containerd/internal/shim"
	"github.com/aledbf/qemubox/containerd/internal/shim/manager"
)

func main() {
	// Load configuration first - fail fast if config is missing or invalid
	cfg, err := config.Get()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Failed to load qemubox configuration: %v\n", err)
		fmt.Fprintf(os.Stderr, "\nPlease create a configuration file at /etc/qemubox/config.json\n")
		fmt.Fprintf(os.Stderr, "See examples/config.json for a template with default values.\n")
		fmt.Fprintf(os.Stderr, "\nAlternatively, set QEMUBOX_CONFIG to specify a custom config file location.\n")
		os.Exit(1)
	}

	// Set log level from configuration
	setShimLogLevel(cfg)

	ctx := context.Background()
	shim.Run(ctx, manager.NewShimManager("io.containerd.qemubox.v1"))
}

func setShimLogLevel(cfg *config.Config) {
	if cfg.Runtime.ShimDebug {
		_ = log.SetLevel("debug")
		log.L.Info("debug logging enabled via configuration")
		return
	}
	_ = log.SetLevel("info")
}
