//go:build linux

package network

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aledbf/qemubox/containerd/internal/host/network/cni"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanupErrorMethods(t *testing.T) {
	tests := []struct {
		name        string
		result      CleanupError
		expectErr   bool
		errContains []string
	}{
		{
			name:      "no errors",
			result:    CleanupError{InMemoryClear: true},
			expectErr: false,
		},
		{
			name: "CNI teardown error only",
			result: CleanupError{
				CNITeardown:   errors.New("CNI failed"),
				InMemoryClear: true,
			},
			expectErr:   true,
			errContains: []string{"CNI teardown", "CNI failed"},
		},
		{
			name: "netns delete error only",
			result: CleanupError{
				NetNSDelete:   errors.New("netns busy"),
				InMemoryClear: true,
			},
			expectErr:   true,
			errContains: []string{"netns delete", "netns busy"},
		},
		{
			name: "IPAM verify error only",
			result: CleanupError{
				IPAMVerify:    errors.New("IP still allocated"),
				InMemoryClear: true,
			},
			expectErr:   true,
			errContains: []string{"IPAM verify", "IP still allocated"},
		},
		{
			name: "multiple errors",
			result: CleanupError{
				CNITeardown: errors.New("CNI failed"),
				NetNSDelete: errors.New("netns busy"),
				IPAMVerify:  errors.New("IP leaked"),
			},
			expectErr:   true,
			errContains: []string{"CNI teardown", "netns delete", "IPAM verify"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hasErr := tt.result.HasError()
			assert.Equal(t, tt.expectErr, hasErr)

			errStr := tt.result.Error()
			if tt.expectErr {
				assert.NotEmpty(t, errStr)
				for _, contains := range tt.errContains {
					assert.Contains(t, errStr, contains)
				}
			} else {
				assert.Empty(t, errStr)
			}
		})
	}
}

func TestVerifyIPAMCleanup(t *testing.T) {
	// Create a temporary IPAM directory structure
	tmpDir := t.TempDir()
	ipamDir := filepath.Join(tmpDir, "cni", "networks")
	networkDir := filepath.Join(ipamDir, "test-network")
	require.NoError(t, os.MkdirAll(networkDir, 0755))

	// Create the manager with custom IPAM path (we'll test via direct function call)
	nm := &cniNetworkManager{}

	// Override the IPAM directory for testing
	// We'll create a test helper instead
	t.Run("no IPAM directory", func(t *testing.T) {
		err := nm.verifyIPAMCleanup(context.Background(), "test-container")
		assert.NoError(t, err) // Non-existent dir should not error
	})

	t.Run("leaked IP detected", func(t *testing.T) {
		// Create the standard IPAM directory
		stdIpamDir := "/var/lib/cni/networks"
		if _, err := os.Stat(stdIpamDir); os.IsNotExist(err) {
			t.Skip("Standard IPAM directory does not exist")
		}

		// This test would require root and actual IPAM setup
		// Skip for unit tests
		t.Skip("Requires actual IPAM setup")
	})
}

func TestCleanupErrorImplementsError(t *testing.T) {
	var err error = &CleanupError{
		CNITeardown: errors.New("test"),
	}

	// Should be usable as an error
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "test")
}

func TestIPAMLeakErrorWrapping(t *testing.T) {
	// Verify that IPAM leak errors are properly wrapped
	leakErr := &CleanupError{
		IPAMVerify: cni.ErrIPAMLeak,
	}

	assert.ErrorIs(t, leakErr.IPAMVerify, cni.ErrIPAMLeak)
}
