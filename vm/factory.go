package vm

import (
	"context"
	"fmt"
	"os"

	"github.com/containerd/log"
)

// VMType identifies the VMM backend
type VMType string

const (
	VMTypeQEMU VMType = "qemu"
)

// Factory creates VM instances for a specific VMM backend
type Factory interface {
	NewInstance(ctx context.Context, containerID, stateDir string, cfg *VMResourceConfig) (Instance, error)
}

// GetVMType determines which VMM to use.
// Always returns QEMU as it is the only supported VMM backend.
func GetVMType() VMType {
	if vmm := os.Getenv("BEACON_VMM"); vmm != "" && vmm != "qemu" {
		log.L.WithField("vmm", vmm).Warn("only QEMU is supported, ignoring BEACON_VMM value")
	}
	return VMTypeQEMU
}

// NewFactory creates a factory for the specified VMM type.
// This function is implemented by registering factories from each VMM package.
func NewFactory(ctx context.Context, vmmType VMType) (Factory, error) {
	factory, ok := factories[vmmType]
	if !ok {
		return nil, fmt.Errorf("unknown VMM type: %s (available: %v)", vmmType, registeredTypes())
	}

	log.G(ctx).WithField("vmm", vmmType).Info("selected VMM backend")
	return factory, nil
}

// factories holds registered VMM factory implementations
var factories = make(map[VMType]Factory)

// Register registers a VMM factory implementation.
// This is called by init() functions in each VMM package.
func Register(vmmType VMType, factory Factory) {
	if _, exists := factories[vmmType]; exists {
		panic(fmt.Sprintf("VMM factory already registered: %s", vmmType))
	}
	factories[vmmType] = factory
}

// registeredTypes returns a list of registered VMM types for error messages
func registeredTypes() []VMType {
	types := make([]VMType, 0, len(factories))
	for t := range factories {
		types = append(types, t)
	}
	return types
}
