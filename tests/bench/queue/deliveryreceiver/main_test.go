package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunReceiverRejectsInvalidStartupConfiguration(t *testing.T) {
	unusedListener := func(*http.Server) error {
		t.Fatal("listener should not be called for invalid configuration")
		return nil
	}

	require.Error(t, runReceiver([]string{"-unknown"}, unusedListener))

	missingPath := filepath.Join(t.TempDir(), "missing.json")
	require.ErrorContains(t, runReceiver([]string{"-keys", missingPath}, unusedListener), "open public keys")

	invalidJSON := filepath.Join(t.TempDir(), "invalid.json")
	require.NoError(t, os.WriteFile(invalidJSON, []byte("{"), 0o600))
	require.ErrorContains(t, runReceiver([]string{"-host", "receiver.example", "-keys", invalidJSON}, unusedListener), "decode public keys")

	require.ErrorContains(t, runReceiver([]string{"-host", " "}, unusedListener), "receiver host is required")
}

func TestRunReceiverLoadsKeysAndBuildsServer(t *testing.T) {
	_, publicPEM, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	keyID := "https://sender.example/users/bench#main-key"
	keysJSON, err := json.Marshal(map[string]string{keyID: publicPEM})
	require.NoError(t, err)
	keysPath := filepath.Join(t.TempDir(), "keys.json")
	require.NoError(t, os.WriteFile(keysPath, keysJSON, 0o600))

	listenErr := errors.New("listener stopped")
	called := false
	err = runReceiver([]string{"-addr", "127.0.0.1:0", "-host", "receiver.example", "-keys", keysPath}, func(server *http.Server) error {
		called = true
		assert.Equal(t, "127.0.0.1:0", server.Addr)
		assert.Equal(t, 10*time.Second, server.ReadHeaderTimeout)
		fixture, ok := server.Handler.(*receiver)
		if assert.True(t, ok) {
			assert.Equal(t, "receiver.example", fixture.host)
			assert.Equal(t, publicPEM, fixture.publicKeys[keyID])
		}
		return listenErr
	})
	require.True(t, called)
	require.ErrorIs(t, err, listenErr)
}

func TestRunReceiverAndExitHandlesListenerResults(t *testing.T) {
	fatalCalls := 0
	fatalf := func(format string, args ...any) { fatalCalls++ }
	listenerError := errors.New("listen failed")
	listen := func(*http.Server) error { return listenerError }

	runReceiverAndExit([]string{"-host", "receiver.example"}, listen, fatalf)
	assert.Equal(t, 1, fatalCalls)

	runReceiverAndExit([]string{"-host", "receiver.example"}, func(*http.Server) error { return http.ErrServerClosed }, fatalf)
	runReceiverAndExit([]string{"-host", "receiver.example"}, func(*http.Server) error { return nil }, fatalf)
	assert.Equal(t, 1, fatalCalls)
}
