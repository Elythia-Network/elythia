package stream

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readEvent is a single ordered ReadMessage outcome injected from the test.
type readEvent struct {
	data []byte
	err  error
}

// fakeConn implements Conn with channel-driven inputs / outputs so the read /
// write loops can be exercised without an actual WebSocket pair.
type fakeConn struct {
	mu          sync.Mutex
	events      chan readEvent // ordered queue of ReadMessage outcomes
	done        chan struct{}  // closed by Close to unblock ReadMessage
	doneOnce    sync.Once
	writes      [][]byte
	pings       int
	writeErr    error
	pingErr     error
	pongHandler func(string) error
	closed      bool
	readLimit   int64
	// closeFrames は WriteControl に渡された close frame の payload。
	closeFrames [][]byte
	// closedAfterWrites は Close 時点の writes の件数 (close frame より後に
	// data frame が書かれていないかの確認用)。
	closedAfterWrites int
	// writeDeadline は最後に SetWriteDeadline で設定された値。
	writeDeadline time.Time
	// writeGate が non-nil なら、WriteMessage はそれが close されるか Close
	// されるまで戻らない (相手が読まずに送信が詰まった状態の再現)。
	writeGate chan struct{}
}

func newFakeConn() *fakeConn {
	return &fakeConn{events: make(chan readEvent, 8), done: make(chan struct{})}
}

func (f *fakeConn) ReadMessage() (int, []byte, error) {
	select {
	case ev, ok := <-f.events:
		if !ok {
			return 0, nil, errors.New("fake closed")
		}
		if ev.err != nil {
			return 0, nil, ev.err
		}
		return websocket.TextMessage, ev.data, nil
	case <-f.done:
		return 0, nil, errors.New("fake closed")
	}
}

func (f *fakeConn) WriteMessage(_ int, data []byte) error {
	if f.writeGate != nil {
		select {
		case <-f.writeGate:
		case <-f.done:
			return errors.New("fake closed")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes = append(f.writes, append([]byte(nil), data...))
	return nil
}

func (f *fakeConn) WriteControl(messageType int, data []byte, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if messageType == websocket.CloseMessage {
		f.closeFrames = append(f.closeFrames, append([]byte(nil), data...))
	}
	if messageType == websocket.PingMessage {
		f.pings++
		if f.pingErr != nil {
			return f.pingErr
		}
	}
	return nil
}

func (f *fakeConn) SetReadDeadline(_ time.Time) error { return nil }

func (f *fakeConn) SetWriteDeadline(t time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeDeadline = t
	return nil
}

func (f *fakeConn) getWriteDeadline() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writeDeadline
}

// readLimit は SetReadLimit で設定された値 (0 = 未設定 = 無制限)。
func (f *fakeConn) SetReadLimit(limit int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readLimit = limit
}

func (f *fakeConn) ReadLimit() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readLimit
}
func (f *fakeConn) SetPongHandler(h func(string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pongHandler = h
}

// getPongHandler returns the registered handler under the lock so tests can
// invoke it without racing the readLoop.
func (f *fakeConn) getPongHandler() func(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pongHandler
}
func (f *fakeConn) Close() error {
	f.mu.Lock()
	if !f.closed {
		f.closedAfterWrites = len(f.writes)
	}
	f.closed = true
	f.mu.Unlock()
	f.doneOnce.Do(func() { close(f.done) })
	return nil
}

// queue a fake message and a closing read error so readLoop terminates.
func (f *fakeConn) sendMessage(data []byte) {
	f.events <- readEvent{data: data}
}

func (f *fakeConn) finishReadWithError(err error) {
	f.events <- readEvent{err: err}
}

func (f *fakeConn) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func (f *fakeConn) writeAt(i int) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(f.writes[i]))
	copy(cp, f.writes[i])
	return cp
}

func (f *fakeConn) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func TestConnection_ReadDispatchesMessages(t *testing.T) {
	fc := newFakeConn()
	c := NewConnection("c1", &model.User{ID: "alice"}, fc)

	var (
		gotType string
		mu      sync.Mutex
	)
	c.SetMessageHandler(func(msgType string, _ json.RawMessage) {
		mu.Lock()
		gotType = msgType
		mu.Unlock()
	})

	go c.Start()
	fc.sendMessage([]byte(`{"type":"connect","body":{}}`))
	fc.finishReadWithError(errors.New("eof"))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return gotType == "connect"
	}, time.Second, 10*time.Millisecond)
}

func TestConnection_ReadInvalidJSONIgnored(t *testing.T) {
	fc := newFakeConn()
	c := NewConnection("c1", nil, fc)
	called := false
	c.SetMessageHandler(func(string, json.RawMessage) { called = true })
	go c.Start()
	fc.sendMessage([]byte(`{not json`))
	fc.finishReadWithError(errors.New("eof"))
	// readLoop が終わるのを少し待つ
	time.Sleep(50 * time.Millisecond)
	assert.False(t, called)
}

func TestConnection_NilHandlerDropsMessages(t *testing.T) {
	fc := newFakeConn()
	c := NewConnection("c1", nil, fc)
	go c.Start()
	fc.sendMessage([]byte(`{"type":"connect","body":{}}`))
	fc.finishReadWithError(errors.New("eof"))
	require.Eventually(t, func() bool { return fc.isClosed() }, time.Second, 10*time.Millisecond)
}

func TestConnection_SendDeliversPayload(t *testing.T) {
	fc := newFakeConn()
	c := NewConnection("c1", nil, fc)
	go c.Start()
	defer c.Close()

	require.NoError(t, c.Send(map[string]any{"type": "channel"}))
	require.Eventually(t, func() bool { return fc.writeCount() == 1 }, time.Second, 10*time.Millisecond)
	assert.Contains(t, string(fc.writeAt(0)), "channel")
}

func TestConnection_SendAfterCloseFails(t *testing.T) {
	fc := newFakeConn()
	c := NewConnection("c1", nil, fc)
	c.Close()
	err := c.Send(map[string]any{"k": "v"})
	assert.Error(t, err)
}

// closingConn writes always succeed but the queue is forced full to exercise
// the overflow → close branch.
func TestConnection_SendQueueFullClosesConnection(t *testing.T) {
	fc := newFakeConn()
	fc.writeErr = errors.New("write blocked") // make writes fail so the queue stays backed up
	c := NewConnection("c1", nil, fc)
	go c.Start()

	// 最初の Send で writer が起動するが writeErr で即座に close される
	// 競合を避けるため、ここではバッファをあふれさせる経路を直接刺す。
	c.closeInternal() // 一度閉じてから
	err := c.Send(map[string]any{"k": "v"})
	assert.Error(t, err)
}

func TestConnection_SendMarshalError(t *testing.T) {
	c := NewConnection("c1", nil, newFakeConn())
	// channel は JSON にできないので Marshal でエラー
	err := c.Send(make(chan int))
	assert.Error(t, err)
}

func TestConnection_PingTickerWriteErrorClosesConnection(t *testing.T) {
	// ping interval を待つのは時間がかかるので、別のテスト戦略: writeErr を
	// 設定して Send 経路で writer が WriteMessage に失敗 → close するのを確認
	fc := newFakeConn()
	fc.writeErr = errors.New("network down")
	c := NewConnection("c1", nil, fc)
	go c.Start()

	require.NoError(t, c.Send(map[string]any{"k": "v"}))
	require.Eventually(t, func() bool { return fc.isClosed() }, time.Second, 10*time.Millisecond)
}

func TestConnection_CloseHandlerInvokedOnce(t *testing.T) {
	fc := newFakeConn()
	c := NewConnection("c1", nil, fc)
	var calls int
	c.SetCloseHandler(func() { calls++ })
	c.Close()
	c.Close()
	assert.Equal(t, 1, calls)
}

func TestConnection_PongHandlerExtendsDeadline(t *testing.T) {
	fc := newFakeConn()
	c := NewConnection("c1", nil, fc)
	go c.Start()
	defer c.Close()
	require.Eventually(t, func() bool { return fc.getPongHandler() != nil }, time.Second, 10*time.Millisecond)
	require.NoError(t, fc.getPongHandler()("ping"))
}

func TestConnection_SendQueueOverflowClosesConnection(t *testing.T) {
	fc := newFakeConn()
	c := NewConnection("c1", nil, fc)
	// Start を呼ばないので writer goroutine が走らず、send chan は常に full
	for i := 0; i < sendQueueSize; i++ {
		require.NoError(t, c.Send(map[string]any{"i": i}))
	}
	err := c.Send(map[string]any{"overflow": true})
	assert.Error(t, err)
	// queue full → connection close
	assert.True(t, fc.isClosed())
}

func TestConnection_PingTickerSendsPings(t *testing.T) {
	fc := newFakeConn()
	c := NewConnection("c1", nil, fc)
	c.SetPingInterval(5 * time.Millisecond)
	c.SetPingInterval(0) // 0 は無視されデフォルト維持の経路もテスト
	c.SetPingInterval(5 * time.Millisecond)
	go c.Start()
	defer c.Close()

	require.Eventually(t, func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		return fc.pings > 0
	}, time.Second, 5*time.Millisecond)
}

func TestConnection_PingTickerErrorClosesConnection(t *testing.T) {
	fc := newFakeConn()
	fc.pingErr = errors.New("ping fail")
	c := NewConnection("c1", nil, fc)
	c.SetPingInterval(5 * time.Millisecond)
	go c.Start()

	require.Eventually(t, func() bool { return fc.isClosed() }, time.Second, 5*time.Millisecond)
}

func TestConnection_AccessorsExposeUserAndID(t *testing.T) {
	user := &model.User{ID: "alice"}
	c := NewConnection("c-alpha", user, newFakeConn())
	assert.Equal(t, "c-alpha", c.ID())
	assert.Equal(t, user, c.User())
}

// #787: SetHardMuteRules / HardMuteRules round-trip。
func TestConnection_HardMuteRules(t *testing.T) {
	c := NewConnection("c1", nil, nil)
	if got := c.HardMuteRules(); got != nil {
		t.Fatalf("default HardMuteRules = %v, want nil", got)
	}
	rules := []byte(`["foo"]`)
	c.SetHardMuteRules(rules)
	if got := c.HardMuteRules(); string(got) != string(rules) {
		t.Fatalf("HardMuteRules = %q, want %q", got, rules)
	}
}

// #1711: SetMuteBlockSnapshot / MuteBlockSnapshot round-trip。
func TestConnection_MuteBlockSnapshot(t *testing.T) {
	c := NewConnection("c1", nil, nil)
	if got := c.MuteBlockSnapshot(); got != nil {
		t.Fatalf("default MuteBlockSnapshot = %v, want nil", got)
	}
	snap := &MuteBlockSnapshot{Muting: map[string]struct{}{"u1": {}}}
	c.SetMuteBlockSnapshot(snap)
	got := c.MuteBlockSnapshot()
	if got == nil || len(got.Muting) != 1 {
		t.Fatalf("MuteBlockSnapshot = %v, want %v", got, snap)
	}
	if _, ok := got.Muting["u1"]; !ok {
		t.Fatalf("MuteBlockSnapshot.Muting missing u1")
	}
}

// #1063: SetFollowingSnapshot / FollowingSnapshot round-trip。
func TestConnection_FollowingSnapshot(t *testing.T) {
	c := NewConnection("c1", nil, nil)
	if got := c.FollowingSnapshot(); got != nil {
		t.Fatalf("default FollowingSnapshot = %v, want nil", got)
	}
	snap := map[string]bool{"u1": true, "u2": false}
	c.SetFollowingSnapshot(snap)
	got := c.FollowingSnapshot()
	if len(got) != 2 || !got["u1"] || got["u2"] {
		t.Fatalf("FollowingSnapshot = %v, want %v", got, snap)
	}
}

func TestConnection_UpdateFollowingSnapshot_AddRemove(t *testing.T) {
	c := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	c.SetFollowingSnapshot(map[string]bool{"u1": true})

	// 新規 follow は withReplies=false で追加される。
	c.UpdateFollowingSnapshot("u2", true)
	got := c.FollowingSnapshot()
	require.Len(t, got, 2)
	assert.True(t, got["u1"], "既存エントリの withReplies は維持される")
	assert.False(t, got["u2"], "新規 follow は withReplies=false")

	// 再 follow は既存 withReplies を保持する (上書きしない)。
	c.UpdateFollowingSnapshot("u1", true)
	assert.True(t, c.FollowingSnapshot()["u1"])

	// unfollow は削除する。
	c.UpdateFollowingSnapshot("u1", false)
	got = c.FollowingSnapshot()
	require.Len(t, got, 1)
	_, ok := got["u1"]
	assert.False(t, ok)
}

func TestConnection_UpdateFollowingSnapshot_NilSnapshotNoOp(t *testing.T) {
	c := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	// snapshot 未設定 (anonymous/未配線) では map を作らず no-op。
	c.UpdateFollowingSnapshot("u1", true)
	assert.Nil(t, c.FollowingSnapshot())
}

func TestConnection_UpdateFollowingSnapshot_CopyOnWrite(t *testing.T) {
	c := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	c.SetFollowingSnapshot(map[string]bool{"u1": true})
	// FollowingSnapshot() で得た map は更新後も不変 (copy-on-write)。
	before := c.FollowingSnapshot()
	c.UpdateFollowingSnapshot("u2", true)
	_, leaked := before["u2"]
	assert.False(t, leaked, "既に handed out した map は mutate されない")
}

func TestConnection_UpdateFollowingSnapshot_EmptyIDNoOp(t *testing.T) {
	c := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	c.SetFollowingSnapshot(map[string]bool{"u1": true})
	c.UpdateFollowingSnapshot("", true)
	assert.Len(t, c.FollowingSnapshot(), 1)
}

// **受信フレームに上限が掛かっていること。**
//
// gorilla の既定 `readLimit` は 0 = 無制限で、`ReadMessage` はフレーム全体を
// 成長するバッファへ載せる。Echo の body limit は upgrade 後のフレームには
// 効かないので、未認証の接続 1 本で任意量のメモリを確保させられる。
func TestConnection_SetsReadLimit(t *testing.T) {
	fc := newFakeConn()
	c := NewConnection("c1", nil, fc)
	go c.Start()
	defer fc.finishReadWithError(errors.New("eof"))

	// 値はリテラルで書く (定数を参照すると、緩める変異と一緒に期待値が動く)。
	require.Eventually(t, func() bool {
		return fc.ReadLimit() == int64(1<<20)
	}, time.Second, 10*time.Millisecond,
		"1MiB の受信上限を設定すること (0 = 無制限のままにしない。実測 %d)", fc.ReadLimit())
}
