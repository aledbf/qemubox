//go:build linux

package cni

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestErrorClassification(t *testing.T) {
	tests := []struct {
		name        string
		errMsg      string
		expectedIs  error
		shouldNotBe []error
	}{
		{
			name:        "duplicate allocation error",
			errMsg:      "failed to add network: duplicate allocation",
			expectedIs:  ErrResourceConflict,
			shouldNotBe: []error{ErrIPAMExhausted, ErrNetNSNotFound},
		},
		{
			name:        "veth already exists",
			errMsg:      "failed to create veth pair: file exists for interface veth123",
			expectedIs:  ErrResourceConflict,
			shouldNotBe: []error{ErrIPAMExhausted},
		},
		{
			name:       "tap already exists",
			errMsg:     "failed to create tap device: already exists",
			expectedIs: ErrResourceConflict,
		},
		{
			name:       "peer interface exists",
			errMsg:     "peer interface already exists",
			expectedIs: ErrResourceConflict,
		},
		{
			name:        "no IPs available",
			errMsg:      "no ips available in range",
			expectedIs:  ErrIPAMExhausted,
			shouldNotBe: []error{ErrResourceConflict},
		},
		{
			name:       "no IP addresses available",
			errMsg:     "IPAM error: no IP addresses available in pool",
			expectedIs: ErrIPAMExhausted,
		},
		{
			name:       "network namespace not found",
			errMsg:     "failed to open network namespace /var/run/netns/test: not found",
			expectedIs: ErrNetNSNotFound,
		},
		{
			name:       "generic error - no classification",
			errMsg:     "some random error",
			expectedIs: nil,
		},
		{
			name:       "permission denied - no classification",
			errMsg:     "permission denied opening /var/run/netns/test",
			expectedIs: nil,
		},
	}

	ctx := context.Background()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rawErr := errors.New(tt.errMsg)
			classifiedErr := ClassifyError(ctx, "ADD", "test-net", rawErr)

			require.Error(t, classifiedErr)

			// Check Error() method
			var cniErr *Error
			require.ErrorAs(t, classifiedErr, &cniErr, "should be a *cni.Error")
			assert.Equal(t, "ADD", cniErr.Operation)
			assert.Equal(t, "test-net", cniErr.Plugin)

			// Check classification
			if tt.expectedIs != nil {
				assert.ErrorIs(t, classifiedErr, tt.expectedIs)
			}

			// Check it doesn't match other error types
			for _, shouldNot := range tt.shouldNotBe {
				assert.NotErrorIs(t, classifiedErr, shouldNot)
			}

			// Check Unwrap returns original error
			assert.ErrorIs(t, classifiedErr, rawErr)
		})
	}
}

func TestErrorNil(t *testing.T) {
	result := ClassifyError(context.Background(), "ADD", "test", nil)
	assert.NoError(t, result)
}

func TestErrorMethods(t *testing.T) {
	rawErr := fmt.Errorf("test error")
	cniErr := &Error{
		Plugin:    "bridge",
		Operation: "ADD",
		Cause:     rawErr,
		Category:  ErrResourceConflict,
	}

	// Test Error() formatting
	errStr := cniErr.Error()
	assert.Contains(t, errStr, "bridge")
	assert.Contains(t, errStr, "ADD")
	assert.Contains(t, errStr, "test error")

	// Test Unwrap()
	assert.Equal(t, rawErr, cniErr.Unwrap())

	// Test Is() with category
	assert.ErrorIs(t, cniErr, ErrResourceConflict)
	assert.NotErrorIs(t, cniErr, ErrIPAMExhausted)
}

func TestErrorWithoutPlugin(t *testing.T) {
	cniErr := &Error{
		Operation: "DEL",
		Cause:     fmt.Errorf("failed"),
	}

	errStr := cniErr.Error()
	assert.Contains(t, errStr, "DEL")
	assert.NotContains(t, errStr, "CNI  DEL") // Should not have double space
}

func TestErrorIsWithSentinels(t *testing.T) {
	conflictErr := &Error{
		Category: ErrResourceConflict,
		Cause:    fmt.Errorf("veth exists"),
	}
	exhaustedErr := &Error{
		Category: ErrIPAMExhausted,
		Cause:    fmt.Errorf("no ips"),
	}
	otherErr := fmt.Errorf("some other error")

	// Use errors.Is() directly as intended
	assert.ErrorIs(t, conflictErr, ErrResourceConflict)
	assert.NotErrorIs(t, otherErr, ErrResourceConflict)
	assert.NotErrorIs(t, conflictErr, ErrIPAMExhausted) // Not the other sentinel

	assert.ErrorIs(t, exhaustedErr, ErrIPAMExhausted)
	assert.NotErrorIs(t, otherErr, ErrIPAMExhausted)
	assert.NotErrorIs(t, exhaustedErr, ErrResourceConflict) // Not the other sentinel
}
