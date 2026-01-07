//go:build linux

package cni

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/containerd/log"
)

// Sentinel errors for CNI operations.
// Use errors.Is() to check for these error categories.
var (
	// ErrResourceConflict indicates CNI resources already exist (veth, IP allocation).
	ErrResourceConflict = errors.New("CNI resource conflict")

	// ErrIPAMExhausted indicates no IPs available in pool.
	ErrIPAMExhausted = errors.New("IPAM pool exhausted")

	// ErrNetNSNotFound indicates network namespace doesn't exist.
	ErrNetNSNotFound = errors.New("network namespace not found")

	// ErrTAPNotCreated indicates tc-redirect-tap didn't create a TAP device.
	ErrTAPNotCreated = errors.New("TAP device not created by CNI")

	// ErrInvalidResult indicates the CNI result is missing required fields.
	ErrInvalidResult = errors.New("invalid CNI result")

	// ErrIPAMLeak indicates IPAM cleanup did not release the IP allocation.
	ErrIPAMLeak = errors.New("IPAM leak detected")
)

// Error wraps a CNI plugin error with classification.
// This allows callers to use errors.Is() to check error categories
// without parsing error strings.
type Error struct {
	Plugin    string // e.g., "bridge", "host-local", "tc-redirect-tap"
	Operation string // "ADD" or "DEL"
	Cause     error
	Category  error // One of the sentinel errors above, or nil if unknown
}

func (e *Error) Error() string {
	if e.Plugin != "" {
		return fmt.Sprintf("CNI %s %s: %v", e.Plugin, e.Operation, e.Cause)
	}
	return fmt.Sprintf("CNI %s: %v", e.Operation, e.Cause)
}

func (e *Error) Unwrap() error {
	return e.Cause
}

// Is implements errors.Is for category matching.
func (e *Error) Is(target error) bool {
	if e.Category != nil && errors.Is(e.Category, target) {
		return true
	}
	return false
}

// ClassifyError wraps a raw CNI error with classification.
// This centralizes error string parsing so callers can use typed errors.
//
// Classification is case-insensitive. Unclassified errors are logged at debug
// level to help identify new error patterns from CNI plugins.
func ClassifyError(ctx context.Context, operation, network string, err error) error {
	if err == nil {
		return nil
	}

	cniErr := &Error{
		Plugin:    network,
		Operation: operation,
		Cause:     err,
	}

	// Case-insensitive matching for CNI error messages
	msg := strings.ToLower(err.Error())

	switch {
	case strings.Contains(msg, "duplicate allocation"):
		cniErr.Category = ErrResourceConflict
	case strings.Contains(msg, "no ips available") ||
		strings.Contains(msg, "no ip addresses available"):
		cniErr.Category = ErrIPAMExhausted
	case (strings.Contains(msg, "already exists") || strings.Contains(msg, "file exists")) &&
		(strings.Contains(msg, "veth") || strings.Contains(msg, "tap") ||
			strings.Contains(msg, "peer") || strings.Contains(msg, "interface")):
		cniErr.Category = ErrResourceConflict
	case strings.Contains(msg, "network namespace") && strings.Contains(msg, "not found"):
		cniErr.Category = ErrNetNSNotFound
	default:
		// Log unclassified errors to help identify new patterns
		log.G(ctx).WithFields(log.Fields{
			"operation": operation,
			"network":   network,
			"error":     err.Error(),
		}).Debug("unclassified CNI error - consider adding classification")
	}

	return cniErr
}
