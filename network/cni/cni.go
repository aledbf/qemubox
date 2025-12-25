//go:build linux

package cni

import (
	"context"
	"fmt"

	"github.com/containernetworking/cni/libcni"
	current "github.com/containernetworking/cni/pkg/types/100"
)

// CNIManager manages CNI plugin execution for VM networking.
type CNIManager struct {
	confDir string
	binDir  string

	// CNI library instance
	cniConfig libcni.CNI
}

// NewCNIManager creates a new CNI manager.
// It will auto-discover CNI network configuration from confDir.
func NewCNIManager(confDir, binDir string) (*CNIManager, error) {
	if confDir == "" {
		return nil, fmt.Errorf("CNI conf directory cannot be empty")
	}
	if binDir == "" {
		return nil, fmt.Errorf("CNI bin directory cannot be empty")
	}

	return &CNIManager{
		confDir:   confDir,
		binDir:    binDir,
		cniConfig: libcni.NewCNIConfig([]string{binDir}, nil),
	}, nil
}

// Setup executes the CNI plugin chain to configure networking for a VM.
// It returns a CNIResult containing the TAP device name and network configuration.
func (m *CNIManager) Setup(ctx context.Context, vmID string, netns string) (*CNIResult, error) {
	// Load network configuration
	netConfList, err := m.loadNetworkConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load CNI network config: %w", err)
	}

	// Execute CNI plugin chain
	result, err := m.execPluginChain(ctx, vmID, netns, netConfList)
	if err != nil {
		return nil, fmt.Errorf("failed to execute CNI plugin chain: %w", err)
	}

	// Parse the result
	cniResult, err := ParseCNIResultWithNetNS(result, netns)
	if err != nil {
		// Clean up on parse failure
		_ = m.Teardown(ctx, vmID, netns)
		return nil, fmt.Errorf("failed to parse CNI result: %w", err)
	}

	return cniResult, nil
}

// Teardown executes the CNI plugin chain to clean up networking for a VM.
func (m *CNIManager) Teardown(ctx context.Context, vmID string, netns string) error {
	// Load network configuration
	netConfList, err := m.loadNetworkConfig()
	if err != nil {
		return fmt.Errorf("failed to load CNI network config: %w", err)
	}

	// Create runtime configuration
	rt := &libcni.RuntimeConf{
		ContainerID: vmID,
		NetNS:       netns,
		IfName:      "eth0",
	}

	// Execute DEL operation
	if err := m.cniConfig.DelNetworkList(ctx, netConfList, rt); err != nil {
		return fmt.Errorf("failed to delete CNI network: %w", err)
	}

	return nil
}

// loadNetworkConfig loads the CNI network configuration from the conf directory.
// It auto-discovers the first available .conflist file (sorted lexicographically).
func (m *CNIManager) loadNetworkConfig() (*libcni.NetworkConfigList, error) {
	// Get all CNI config files from the directory
	files, err := libcni.ConfFiles(m.confDir, []string{".conflist", ".conf"})
	if err != nil {
		return nil, fmt.Errorf("failed to read CNI config files from %s: %w", m.confDir, err)
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no CNI configuration files found in %s", m.confDir)
	}

	// Files are returned sorted lexicographically, use the first one
	// This follows standard CNI practice where files are named like:
	// 10-mynet.conflist, 20-othernet.conflist, etc.
	confFile := files[0]

	// Load the network configuration
	netConfList, err := libcni.ConfListFromFile(confFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load CNI config from %s: %w", confFile, err)
	}

	return netConfList, nil
}

// execPluginChain executes the CNI plugin chain and returns the result.
func (m *CNIManager) execPluginChain(ctx context.Context, vmID string, netns string, netConfList *libcni.NetworkConfigList) (*current.Result, error) {
	// Create runtime configuration
	rt := &libcni.RuntimeConf{
		ContainerID: vmID,
		NetNS:       netns,
		IfName:      "eth0",
	}

	// Execute ADD operation
	result, err := m.cniConfig.AddNetworkList(ctx, netConfList, rt)
	if err != nil {
		return nil, fmt.Errorf("failed to add CNI network: %w", err)
	}

	// Convert to current version
	currentResult, err := current.NewResultFromResult(result)
	if err != nil {
		// Clean up on conversion failure
		_ = m.cniConfig.DelNetworkList(ctx, netConfList, rt)
		return nil, fmt.Errorf("failed to convert CNI result: %w", err)
	}

	return currentResult, nil
}

// GetCNIVersion returns the CNI spec version supported by this implementation.
func (m *CNIManager) GetCNIVersion() string {
	return current.ImplementedSpecVersion
}
