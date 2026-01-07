//go:build linux

package network

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManager(t *testing.T) {
	t.Run("New returns linuxManager", func(t *testing.T) {
		mgr := New()
		require.NotNil(t, mgr)
		_, ok := mgr.(*linuxManager)
		assert.True(t, ok)
	})

	t.Run("newManager returns linuxManager", func(t *testing.T) {
		mgr := newManager()
		require.NotNil(t, mgr)
		_, ok := mgr.(*linuxManager)
		assert.True(t, ok)
	})
}

func TestResolveHostDNSServers(t *testing.T) {
	// Results depend on the host's /etc/resolv.conf
	servers := resolveHostDNSServers(context.Background())

	for _, s := range servers {
		assert.NotEmpty(t, s, "each server should be a valid address")
	}
}
