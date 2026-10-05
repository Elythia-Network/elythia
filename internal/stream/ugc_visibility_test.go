package stream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upstream Connection.onNoteStreamMessage: 未ログインの接続には
// ugcVisibilityForVisitor が none のとき noteUpdated を送らない。local / all と
// 認証済み接続には送る。
func TestDispatcher_ForwardNoteEvent_UGCVisibilityForVisitor(t *testing.T) {
	cases := []struct {
		name   string
		user   *model.User
		policy string
		wired  bool
		want   bool
	}{
		{name: "anon none dropped", policy: "none", wired: true, want: false},
		{name: "anon local forwarded", policy: "local", wired: true, want: true},
		{name: "anon all forwarded", policy: "all", wired: true, want: true},
		{name: "anon unwired forwarded", wired: false, want: true},
		{name: "signed-in none forwarded", user: &model.User{ID: "alice"}, policy: "none", wired: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeConn()
			conn := NewConnection("test", tc.user, fc)
			go conn.Start()
			defer conn.Close()
			d := NewDispatcher(conn, nil, newStubBus())
			if tc.wired {
				policy := tc.policy
				d.SetUGCVisibilityLookup(func() string { return policy })
			}

			d.forwardNoteEvent("n1", []byte(`{"type":"reacted","body":{"reaction":":smile:","userId":"u1"}}`))
			// 落とした場合も「まだ書かれていないだけ」と区別するため、後ろに
			// 目印を 1 通送り、最初の 1 通がどちらかを見る。
			require.NoError(t, conn.Send(map[string]any{"type": "sentinel"}))

			require.Eventually(t, func() bool { return fc.writeCount() >= 1 }, time.Second, 5*time.Millisecond)
			fc.mu.Lock()
			first := append([]byte(nil), fc.writes[0]...)
			fc.mu.Unlock()
			var outer struct {
				Type string `json:"type"`
			}
			require.NoError(t, json.Unmarshal(first, &outer))
			if tc.want {
				assert.Equal(t, "noteUpdated", outer.Type)
			} else {
				assert.Equal(t, "sentinel", outer.Type)
			}
		})
	}
}

// Manager は lookup を接続ごとの Dispatcher へ渡し、channel からは毎回その時点の
// 値が見える (接続時に焼き込まない)。
func TestManager_AcceptWiresUGCVisibilityLookupLive(t *testing.T) {
	var policy atomic.Value
	policy.Store("none")

	var mu sync.Mutex
	var captured ChannelContext
	registry := NewRegistry()
	registry.Register("test", func(ctx ChannelContext) Channel {
		mu.Lock()
		captured = ctx
		mu.Unlock()
		return &fakeChannel{ctx: ctx}
	})
	m := NewManager(registry, newStubBus())
	m.SetUGCVisibilityLookup(func() string { return policy.Load().(string) })

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		go m.Accept(conn, nil, nil, "")
	}))
	defer srv.Close()

	dialer := websocket.Dialer{HandshakeTimeout: time.Second}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.WriteMessage(websocket.TextMessage,
		[]byte(`{"type":"connect","body":{"id":"abc","channel":"test"}}`)))

	var ctx ChannelContext
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		ctx = captured
		return ctx != nil
	}, time.Second, 10*time.Millisecond)
	assert.Equal(t, "none", ctx.UGCVisibilityForVisitor())
	policy.Store("all")
	assert.Equal(t, "all", ctx.UGCVisibilityForVisitor(), "管理画面での変更が既存の接続にも効くこと")
}
