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

func TestCleanupResultMethods(t *testing.T) {
	tests := []struct {
		name        string
		result      CleanupResult
		expectErr   bool
		errContains []string
	}{
		{
			name:      "no errors",
			result:    CleanupResult{InMemoryClear: true},
			expectErr: false,
		},
		{
			name: "CNI teardown error only",
			result: CleanupResult{
				CNITeardown:   errors.New("CNI failed"),
				InMemoryClear: true,
			},
			expectErr:   true,
			errContains: []string{"CNI teardown", "CNI failed"},
		},
		{
			name: "netns delete error only",
			result: CleanupResult{
				NetNSDelete:   errors.New("netns busy"),
				InMemoryClear: true,
			},
			expectErr:   true,
			errContains: []string{"netns delete", "netns busy"},
		},
		{
			name: "IPAM verify error only",
			result: CleanupResult{
				IPAMVerify:    errors.New("IP still allocated"),
				InMemoryClear: true,
			},
			expectErr:   true,
			errContains: []string{"IPAM verify", "IP still allocated"},
		},
		{
			name: "multiple errors",
			result: CleanupResult{
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

			err := tt.result.Err()
			if tt.expectErr {
				require.Error(t, err)
				errStr := err.Error()
				for _, contains := range tt.errContains {
					assert.Contains(t, errStr, contains)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestVerifyIPAMCleanup(t *testing.T) {
	t.Run("no IPAM directory", func(t *testing.T) {
		nm := &cniNetworkManager{
			ipamDir: "/nonexistent/path/that/does/not/exist",
		}
		err := nm.verifyIPAMCleanup(context.Background(), "test-container")
		assert.NoError(t, err) // Non-existent dir should not error
	})

	t.Run("empty IPAM directory", func(t *testing.T) {
		tmpDir := t.TempDir()
		nm := &cniNetworkManager{
			ipamDir: tmpDir,
		}
		err := nm.verifyIPAMCleanup(context.Background(), "test-container")
		assert.NoError(t, err)
	})

	t.Run("leaked IP detected", func(t *testing.T) {
		// Create a temporary IPAM directory structure
		tmpDir := t.TempDir()
		networkDir := filepath.Join(tmpDir, "test-network")
		require.NoError(t, os.MkdirAll(networkDir, 0755))

		// Create an IP file with the container ID (simulates leaked allocation)
		ipFile := filepath.Join(networkDir, "10.88.0.5")
		require.NoError(t, os.WriteFile(ipFile, []byte("leaked-container-id"), 0644))

		nm := &cniNetworkManager{
			ipamDir: tmpDir,
		}

		// Should detect the leak
		err := nm.verifyIPAMCleanup(context.Background(), "leaked-container-id")
		require.Error(t, err)
		assert.ErrorIs(t, err, cni.ErrIPAMLeak)
		assert.Contains(t, err.Error(), "10.88.0.5")
		assert.Contains(t, err.Error(), "test-network")

		// Different container ID should not find a leak
		err = nm.verifyIPAMCleanup(context.Background(), "other-container")
		assert.NoError(t, err)
	})

	t.Run("ignores special files", func(t *testing.T) {
		tmpDir := t.TempDir()
		networkDir := filepath.Join(tmpDir, "test-network")
		require.NoError(t, os.MkdirAll(networkDir, 0755))

		// Create special files that should be ignored
		require.NoError(t, os.WriteFile(filepath.Join(networkDir, "last_reserved_ip"), []byte("test-container"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(networkDir, ".lock"), []byte("test-container"), 0644))

		nm := &cniNetworkManager{
			ipamDir: tmpDir,
		}

		// Should not detect a leak from special files
		err := nm.verifyIPAMCleanup(context.Background(), "test-container")
		assert.NoError(t, err)
	})
}

func TestCleanupResultErr(t *testing.T) {
	result := &CleanupResult{
		CNITeardown: errors.New("test"),
	}
	err := result.Err()

	// Should be usable as an error
	require.Error(t, err)
	assert.Contains(t, err.Error(), "test")

	// No error case
	okResult := &CleanupResult{InMemoryClear: true}
	assert.NoError(t, okResult.Err())
}

func TestIPAMLeakErrorWrapping(t *testing.T) {
	// Verify that IPAM leak errors are properly wrapped
	result := &CleanupResult{
		IPAMVerify: cni.ErrIPAMLeak,
	}

	assert.ErrorIs(t, result.IPAMVerify, cni.ErrIPAMLeak)
}
