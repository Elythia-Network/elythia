package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
)

// TestSupervisor_ShutdownAfterFailedListenKeepsSocketPath checks that the
// Shutdown serve runs after a start failure (#3633) does not remove what is
// at the socket path when Start never listened there: ListenUnixSocket
// refused to remove a non-socket file, and Shutdown must not remove it either.
// The generation is still stopped and its DB closed.
func TestSupervisor_ShutdownAfterFailedListenKeepsSocketPath(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "")
	t.Setenv(config.EnvOnlyQueue, "")
	path := filepath.Join(t.TempDir(), "mk.sock")
	require.NoError(t, os.WriteFile(path, []byte("not a socket"), 0o600))
	f := newFakeGenerations(t)
	sup, err := NewSupervisor(&config.Config{Socket: path}, nil, f.deps(time.Second))
	require.NoError(t, err)

	require.ErrorContains(t, sup.Start(), "refusing to remove non-socket path")
	require.NoError(t, sup.Shutdown(context.Background()))

	b, err := os.ReadFile(path)
	require.NoError(t, err, "Shutdown removed a file it did not create")
	assert.Equal(t, "not a socket", string(b))
	assert.EqualValues(t, 1, f.closed.Load())
}
