package main

import (
	"context"
	"fmt"
	"os"

	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/log"

	"github.com/spin-stack/spinbox/internal/config"
	"github.com/spin-stack/spinbox/internal/shim/manager"

	// Register shim plugin with containerd runtime
	_ "github.com/spin-stack/spinbox/internal/shim"
)

func main() {
	// Load configuration first - fail fast if config is missing or invalid
	_, err := config.Get()
	if err != nil {
		// Use structured logging for the error (consistent with vminitd)
		log.L.WithError(err).Error("failed to load spinbox configuration")
		// Print user-friendly guidance to stderr
		fmt.Fprintln(os.Stderr, "\nPlease create a configuration file at /etc/spinbox/config.json")
		fmt.Fprintln(os.Stderr, "See examples/config.json for a template with default values.")
		fmt.Fprintln(os.Stderr, "\nAlternatively, set SPINBOX_CONFIG to specify a custom config file location.")
		os.Exit(1)
	}

	// Log level is controlled by containerd configuration, not the shim
	ctx := context.Background()
	shim.Run(ctx, manager.NewShimManager("io.containerd.spinbox.v1"))
}
