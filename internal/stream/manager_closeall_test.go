package stream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// dialManager serves m over a real WebSocket and returns a dialer for it.
func dialManager(t *testing.T, m *Manager) func() *websocket.Conn {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		go m.Accept(conn, &model.User{ID: "alice"}, nil, "")
	}))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	return func() *websocket.Conn {
		conn, _, err := (&websocket.Dialer{HandshakeTimeout: time.Second}).Dial(url, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
}

// readCloseCode reads until the peer closes and returns the close code.
func readCloseCode(t *testing.T, conn *websocket.Conn) int {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		var ce *websocket.CloseError
		if assert.ErrorAs(t, err, &ce) {
			return ce.Code
		}
		return 0
	}
}

// TestManager_CloseAllSendsCloseFrameAndUnsubscribes covers what a server
// generation needs when it stops for maintenance (#3463): every client gets
// a 1012 close frame (Shutdown would let them see 1006), every process-wide
// bus subscription is removed — relation reload included, which Shutdown
// leaves behind — and CloseAll waits until the connections unregistered.
func TestManager_CloseAllSendsCloseFrameAndUnsubscribes(t *testing.T) {
	bus := newStubBus()
	m := NewManager(nil, bus, nil, nil, nil, nil)
	m.SubscribeWordMuteReload()
	m.SubscribeRelationReload()
	m.SubscribeBroadcast()
	m.SubscribeStreamRevoke()
	require.NotEmpty(t, bus.subs)

	dial := dialManager(t, m)
	a, b := dial(), dial()
	require.Eventually(t, func() bool { return m.Count() == 2 }, time.Second, 10*time.Millisecond)

	left := m.CloseAll(context.Background(), ServiceRestartCloseCode, "server restarting")
	assert.Zero(t, left)
	assert.Zero(t, m.Count())
	assert.Equal(t, ServiceRestartCloseCode, readCloseCode(t, a))
	assert.Equal(t, ServiceRestartCloseCode, readCloseCode(t, b))
	bus.mu.Lock()
	assert.Empty(t, bus.subs, "CloseAll の後も購読が残っている")
	bus.mu.Unlock()

	// CloseAll の後に upgrade を終えた接続も、登録されたまま残らない。
	late := dial()
	assert.Equal(t, ServiceRestartCloseCode, readCloseCode(t, late))
	require.Eventually(t, func() bool { return m.Count() == 0 }, 7*time.Second, 10*time.Millisecond)
}

// TestManager_CloseAllReportsWhatIsLeftWhenCtxEnds checks the bounded wait:
// a connection that never unregisters is reported instead of blocking.
func TestManager_CloseAllReportsWhatIsLeftWhenCtxEnds(t *testing.T) {
	m := NewManager(nil, nil, nil, nil, nil, nil)
	// close handler で unregister しない接続 (Accept を通らない) を置く。
	m.register(NewConnection("stuck", nil, newFakeConn()))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	assert.Equal(t, 1, m.CloseAll(ctx, ServiceRestartCloseCode, ""))
}
