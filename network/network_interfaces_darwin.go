//go:build darwin

package network

import "fmt"

// NetworkOperator stub for Darwin
type NetworkOperator interface{}

// NFTablesOperator stub for Darwin
type NFTablesOperator interface{}

// DefaultNetworkOperator stub for Darwin
type DefaultNetworkOperator struct{}

// NewDefaultNetworkOperator creates a stub operator (Darwin only)
func NewDefaultNetworkOperator() NetworkOperator {
	return &DefaultNetworkOperator{}
}

// DefaultNFTablesOperator stub for Darwin
type DefaultNFTablesOperator struct{}

// IptablesChecker stub for Darwin
type IptablesChecker struct{}

// NewIptablesChecker creates a stub checker (Darwin only)
func NewIptablesChecker() (*IptablesChecker, error) {
	return nil, fmt.Errorf("not supported on darwin")
}
