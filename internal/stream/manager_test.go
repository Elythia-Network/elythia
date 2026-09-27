package stream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatID(t *testing.T) {
	assert.Equal(t, "0", formatID(0))
	assert.Equal(t, "1", formatID(1))
	assert.Equal(t, "f", formatID(15))
	assert.Equal(t, "10", formatID(16))
	assert.Equal(t, "ff", formatID(255))
}

func TestManager_AllocateIDIncrements(t *testing.T) {
	m := NewManager(nil, nil)
	a := m.allocateID()
	b := m.allocateID()
	assert.NotEqual(t, a, b)
}

func TestManager_RegisterUnregister(t *testing.T) {
	m := NewManager(nil, nil)
	c := NewConnection("c1", nil, newFakeConn())
	m.register(c)
	assert.Equal(t, 1, m.Count())
	assert.Equal(t, c, m.Get("c1"))
	m.unregister("c1")
	assert.Equal(t, 0, m.Count())
	assert.Nil(t, m.Get("c1"))
}

func TestManager_ShutdownClosesEverything(t *testing.T) {
	m := NewManager(nil, nil)
	fc := newFakeConn()
	c := NewConnection("c1", nil, fc)
	m.register(c)
	m.Shutdown()
	assert.Equal(t, 0, m.Count())
	assert.True(t, fc.isClosed())
}

func TestManager_AcceptRegistersAndUnregistersOnClose(t *testing.T) {
	// Accept は *websocket.Conn を要求するが、connection_test の fakeConn を
	// 直接渡すために register/unregister の経路だけテストする。
	m := NewManager(nil, nil)
	fc := newFakeConn()
	id := m.allocateID()
	c := NewConnection(id, nil, fc)
	c.SetCloseHandler(func() { m.unregister(id) })
	m.register(c)
	assert.Equal(t, 1, m.Count())

	c.Close()
	assert.Equal(t, 0, m.Count())
}

// TestManager_AcceptViaHTTPTestServer exercises the Accept path with a real
// *websocket.Conn pair. Accept は内部で readLoop をブロックするので別 goroutine
// で起動し、close 後に登録が解除されることを確認する。
func TestManager_AcceptViaHTTPTestServer(t *testing.T) {
	m := NewManager(nil, nil)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		go m.Accept(conn, &model.User{ID: "alice"}, nil, "")
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: time.Second}
	conn, _, err := dialer.Dial(url, nil)
	require.NoError(t, err)

	require.Eventually(t, func() bool { return m.Count() == 1 }, time.Second, 10*time.Millisecond)
	_ = conn.Close()
	require.Eventually(t, func() bool { return m.Count() == 0 }, time.Second, 10*time.Millisecond)
}

// stubMuteBlockLookup records the userID Accept passes so the wiring can be
// asserted without inspecting the (otherwise un-addressable) Connection.
type stubMuteBlockLookup struct {
	mu         sync.Mutex
	calledWith string
	snap       *MuteBlockSnapshot
}

func (s *stubMuteBlockLookup) MuteBlockSnapshotForUser(userID string) *MuteBlockSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calledWith = userID
	return s.snap
}

func (s *stubMuteBlockLookup) called() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calledWith
}

// TestManager_AcceptInvokesMuteBlockLookup verifies Accept fetches and attaches
// the viewer's mute/block snapshot at connection setup (#1711).
func TestManager_AcceptInvokesMuteBlockLookup(t *testing.T) {
	m := NewManager(nil, nil)
	lookup := &stubMuteBlockLookup{snap: &MuteBlockSnapshot{Muting: map[string]struct{}{"x": {}}}}
	m.SetMuteBlockSnapshotLookup(lookup)

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		go m.Accept(conn, &model.User{ID: "alice"}, nil, "")
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: time.Second}
	conn, _, err := dialer.Dial(url, nil)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	require.Eventually(t, func() bool { return lookup.called() == "alice" }, time.Second, 10*time.Millisecond)
}

// #1942: Accept は policy provider を接続確立時に呼び、Connection に policies を attach する。
type stubPolicyProvider struct {
	mu         sync.Mutex
	calledWith string
}

func (s *stubPolicyProvider) GetUserPolicies(userID string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calledWith = userID
	return map[string]any{"ltlAvailable": false}
}
func (s *stubPolicyProvider) called() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calledWith
}

func TestManager_AcceptInvokesPolicyProvider(t *testing.T) {
	m := NewManager(nil, nil)
	provider := &stubPolicyProvider{}
	m.SetPolicyProvider(provider)

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		go m.Accept(conn, &model.User{ID: "alice"}, nil, "")
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: time.Second}
	conn, _, err := dialer.Dial(url, nil)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	require.Eventually(t, func() bool { return provider.called() == "alice" }, time.Second, 10*time.Millisecond)
}

// TestManager_AcceptDispatchesConnectMessages verifies that Accept wires the
// Dispatcher into the Connection so that client `connect` envelopes reach the
// registered Channel factory and the topic is registered on the bus.
func TestManager_AcceptDispatchesConnectMessages(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	holder := &fakeChannelHolder{}
	registry.Register("test", func(ctx ChannelContext) Channel {
		ch := &fakeChannel{ctx: ctx}
		holder.Value = ch
		return ch
	})
	m := NewManager(registry, bus)

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		go m.Accept(conn, &model.User{ID: "alice"}, nil, "")
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	dialer := websocket.Dialer{HandshakeTimeout: time.Second}
	conn, _, err := dialer.Dial(url, nil)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	require.NoError(t, conn.WriteMessage(websocket.TextMessage,
		[]byte(`{"type":"connect","body":{"id":"abc","channel":"test"}}`)))

	require.Eventually(t, func() bool {
		bus.mu.Lock()
		defer bus.mu.Unlock()
		return len(bus.subscribed) > 0
	}, time.Second, 10*time.Millisecond)
	require.NotNil(t, holder.Value)
}

// --- lastActiveDate tracking (onlineStatus の source) ---

type recordingLastActive struct {
	mu  sync.Mutex
	ids []string
}

func (r *recordingLastActive) RecordActive(userID string) {
	r.mu.Lock()
	r.ids = append(r.ids, userID)
	r.mu.Unlock()
}

func (r *recordingLastActive) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

// 接続時に 1 回記録し、stop 後は ticker が止まること。
func TestManager_TrackLastActiveRecordsOnConnect(t *testing.T) {
	m := NewManager(nil, nil)
	rec := &recordingLastActive{}
	m.SetLastActiveRecorder(rec)

	stop := m.trackLastActive(&model.User{ID: "u1"})
	assert.Equal(t, []string{"u1"}, rec.calls(), "接続時に 1 回記録する")
	stop()
	// stop 後に ticker goroutine が終了していること (二重 close で panic しない)。
	assert.NotPanics(t, func() { _ = m.trackLastActive(&model.User{ID: "u2"}) })
}

// 匿名接続 / recorder 未配線では何もしない。返る stop は安全に呼べる。
func TestManager_TrackLastActiveNoopCases(t *testing.T) {
	rec := &recordingLastActive{}

	unwired := NewManager(nil, nil)
	unwired.trackLastActive(&model.User{ID: "u1"})()
	assert.Empty(t, rec.calls())

	anon := NewManager(nil, nil)
	anon.SetLastActiveRecorder(rec)
	anon.trackLastActive(nil)()
	assert.Empty(t, rec.calls(), "匿名接続は記録しない")
}

// Accept が実際に scope を Connection へ載せること。**Accept を通す**のが要点で、
// connectionScopes と SetPermissions を別々に検査しても、その間の配線が
// 抜けていることは分からない (この bug がまさにその形で本番に入っていた)。
func TestManagerAccept_AttachesScopesThroughAcceptPath(t *testing.T) {
	cases := []struct {
		name     string
		scopes   []string
		wantChat bool
	}{
		{name: "limited app token cannot read chat", scopes: []string{"read:account"}, wantChat: false},
		{name: "app token with chat can", scopes: []string{"read:account", "read:chat"}, wantChat: true},
		{name: "no scopes cannot", scopes: []string{}, wantChat: false},
		{name: "unrestricted can", scopes: nil, wantChat: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, nil)
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				go m.Accept(conn, &model.User{ID: "alice"}, tc.scopes, "")
			}))
			defer srv.Close()

			dialer := websocket.Dialer{HandshakeTimeout: time.Second}
			client, _, err := dialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
			require.NoError(t, err)
			defer func() { _ = client.Close() }()

			require.Eventually(t, func() bool { return m.Count() == 1 }, time.Second, 10*time.Millisecond)

			m.mu.RLock()
			var got *Connection
			for _, c := range m.conns {
				got = c
			}
			m.mu.RUnlock()
			require.NotNil(t, got)

			assert.Equal(t, tc.wantChat, got.HasPermission("read:chat"),
				"Accept が scope を Connection へ載せていない")
		})
	}
}

// Connection 単体での判定。上の Accept 経路と合わせて、両端と配線の 3 点を押さえる。
func TestConnection_HasPermission(t *testing.T) {
	cases := []struct {
		name        string
		scopes      []string
		wantChat    bool
		wantAccount bool
	}{
		// app token: read:chat を持たないので chat channel は拒否される。
		{name: "limited app token", scopes: []string{"read:account"}, wantChat: false, wantAccount: true},
		{name: "app token with chat", scopes: []string{"read:account", "read:chat"}, wantChat: true, wantAccount: true},
		{name: "app token with no scopes", scopes: []string{}, wantChat: false, wantAccount: false},
		// nil = native login token / 匿名。従来どおり全許可。
		{name: "unrestricted", scopes: nil, wantChat: true, wantAccount: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewConnection("1", &model.User{ID: "alice"}, nil)
			if tc.scopes != nil {
				c.SetPermissions(tc.scopes)
			}
			assert.Equal(t, tc.wantChat, c.HasPermission("read:chat"),
				"read:chat の判定が想定と違う")
			assert.Equal(t, tc.wantAccount, c.HasPermission("read:account"))
		})
	}
}
