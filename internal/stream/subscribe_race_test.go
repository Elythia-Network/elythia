package stream

import (
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingBus holds Subscribe open until the test releases it, making the
// window between "the lock was released" and "the cancel handle was stored"
// deterministic.
//
// 本番でこの窓が ns ではなく ms になるのは、`bus.Subscribe` が新しいトピック
// のとき **Redis へ同期 dial する**ため。
type blockingBus struct {
	mu      sync.Mutex
	live    int
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingBus() *blockingBus {
	return &blockingBus{entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingBus) Subscribe(topic string, handler func([]byte)) func() {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	b.mu.Lock()
	b.live++
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.live--
			b.mu.Unlock()
		})
	}
}

func (b *blockingBus) liveHandlers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.live
}

// **購読の確立中に接続が閉じても、解除が失われないこと。**
//
// `bus.Subscribe` はロックの外で呼ぶしかない (Redis へ dial する) ので、その
// 隙間に `CloseAll` が走ると、空のマップを見て「解除するものは無い」と判断し、
// 直後に書き込まれたハンドルが誰にも呼ばれなくなる。ハンドラと Redis 購読が
// プロセス寿命まで残る = メモリリーク。
//
// `CloseAll` は読み取りループ以外の goroutine からも呼ばれる — `Connection.Send`
// は送信キューが満杯だと `closeInternal()` を呼び、`Send` は fanout →
// チャンネル → pump goroutine の上で走る。`writeLoop` の書き込み / ping 失敗と
// `Manager.Shutdown` も別 goroutine。
func TestDispatcher_SubscribeCancelSurvivesConcurrentClose(t *testing.T) {
	bus := newBlockingBus()
	registry := NewRegistry()
	registry.Register("test", func(ctx ChannelContext) Channel { return &fakeChannel{ctx: ctx} })
	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	d := NewDispatcher(conn, registry, bus)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// fakeChannel の Init が topic-a を subscribe する。
		d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
	}()

	select {
	case <-bus.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe に入らない")
	}
	// 購読が確立する前に接続が閉じる。
	d.CloseAll()
	close(bus.release)
	<-done

	assert.Equal(t, 0, bus.liveHandlers(),
		"購読の確立中に閉じた接続のハンドラが残っている (解除が失われた)")
}

// **noteStream 側も同じ形。**
func TestDispatcher_SubNoteCancelSurvivesConcurrentClose(t *testing.T) {
	bus := newBlockingBus()
	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	d := NewDispatcher(conn, NewRegistry(), bus)
	d.SetNoteVisibilityChecker(allowAllNoteVisibility{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.HandleClientMessage("subNote", json.RawMessage(`{"id":"n1"}`))
	}()

	select {
	case <-bus.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe に入らない")
	}
	d.CloseAll()
	close(bus.release)
	<-done

	assert.Equal(t, 0, bus.liveHandlers(),
		"購読の確立中に閉じた接続の noteStream ハンドラが残っている")
}

// **確立した購読は普通に解除されること** (上の 2 つが「常に解除する」実装でも
// 緑にならないようにする)。
func TestDispatcher_SubscribeCancelRunsOnNormalClose(t *testing.T) {
	bus := newBlockingBus()
	close(bus.release) // 待たせない
	registry := NewRegistry()
	registry.Register("test", func(ctx ChannelContext) Channel { return &fakeChannel{ctx: ctx} })
	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	d := NewDispatcher(conn, registry, bus)

	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
	require.Equal(t, 1, bus.liveHandlers(), "購読が立っていること")
	d.CloseAll()
	assert.Equal(t, 0, bus.liveHandlers())
}

type allowAllNoteVisibility struct{}

func (allowAllNoteVisibility) RequireVisible(_ *model.User, id string) (*model.Note, error) {
	return &model.Note{ID: id}, nil
}

// **1 接続あたりの購読トピック数に上限があること。**
//
// チャンネル単位の上限 (`maxChannelsPerConnection` = 32) だけでは、ハッシュタグ
// のように 1 チャンネルで多数のトピックを購読するもの (最大 32) を積まれると
// **1 本の接続から distinct な Redis 購読を 1024 個**開ける。`PubSubService` は
// distinct トピックごとに Redis へ接続を張るので、`/streaming` を数本開くだけで
// valkey の `maxclients` に届き、インスタンス全体が新規接続を拒否する。
// **`/streaming` は未認証で張れて接続数の上限も無い。**
func TestDispatcher_CapsTopicsPerConnection(t *testing.T) {
	bus := newStubBus()
	registry := NewRegistry()
	registry.Register("test", func(ctx ChannelContext) Channel { return &fakeChannel{ctx: ctx} })
	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	d := NewDispatcher(conn, registry, bus)

	// チャンネルを 1 つ作り、そこから多数のトピックを購読させる。
	d.HandleClientMessage("connect", json.RawMessage(`{"id":"abc","channel":"test"}`))
	for i := 0; i < 500; i++ {
		d.subscribe("abc", "topic-"+strconv.Itoa(i))
	}

	d.mu.Lock()
	got := len(d.topics)
	d.mu.Unlock()
	// 期待値はリテラル (定数を参照すると上限を緩める変異と一緒に動く)。
	assert.LessOrEqual(t, got, 129,
		"1 接続が保持できるトピック数に上限が無い (Redis の接続数を食い潰せる)")
	assert.Greater(t, got, 100, "正当な利用者が張る数まで絞りすぎていないこと")
}
