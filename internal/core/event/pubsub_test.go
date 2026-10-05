package event

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testRedis *testutil.TestRedis

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	testRedis, err = testutil.SetupRedis(ctx)
	if err != nil {
		log.Fatalf("failed to setup redis: %v", err)
	}

	code := m.Run()

	testRedis.Teardown(ctx)
	os.Exit(code)
}

func TestPubSubService_PublishSubscribe(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "test:")
	ctx := context.Background()
	defer svc.Close()

	type msg struct {
		Text string `json:"text"`
	}

	var mu sync.Mutex
	var received []msg

	svc.Subscribe(ctx, "chan1", func(data []byte) {
		var m msg
		if err := json.Unmarshal(data, &m); err == nil {
			mu.Lock()
			received = append(received, m)
			mu.Unlock()
		}
	})

	// サブスクリプションが確立されるのを待つ
	time.Sleep(100 * time.Millisecond)

	require.NoError(t, svc.Publish(ctx, "chan1", msg{Text: "hello"}))
	require.NoError(t, svc.Publish(ctx, "chan1", msg{Text: "world"}))

	// メッセージの受信を待つ
	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) >= 2
	}, 2*time.Second, 50*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "hello", received[0].Text)
	assert.Equal(t, "world", received[1].Text)
}

func TestPubSubService_Unsubscribe(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "test:")
	ctx := context.Background()
	defer svc.Close()

	var count int
	var mu sync.Mutex

	cancel := svc.Subscribe(ctx, "chan2", func(data []byte) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	time.Sleep(100 * time.Millisecond)

	cancel()

	// Unsubscribe後のメッセージは受信されない
	time.Sleep(50 * time.Millisecond)
	_ = svc.Publish(ctx, "chan2", "after_unsub")

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	assert.Equal(t, 0, count)
	mu.Unlock()
}

func TestPubSubService_MultipleChannels(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "test:")
	ctx := context.Background()
	defer svc.Close()

	var mu sync.Mutex
	results := make(map[string]string)

	svc.Subscribe(ctx, "a", func(data []byte) {
		mu.Lock()
		results["a"] = string(data)
		mu.Unlock()
	})
	svc.Subscribe(ctx, "b", func(data []byte) {
		mu.Lock()
		results["b"] = string(data)
		mu.Unlock()
	})

	time.Sleep(100 * time.Millisecond)

	require.NoError(t, svc.Publish(ctx, "a", "msg_a"))
	require.NoError(t, svc.Publish(ctx, "b", "msg_b"))

	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(results) >= 2
	}, 2*time.Second, 50*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, results["a"], "msg_a")
	assert.Contains(t, results["b"], "msg_b")
}

func TestPubSubService_Cancel_Idempotent(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "test:")
	ctx := context.Background()
	defer svc.Close()

	cancel := svc.Subscribe(ctx, "idem", func([]byte) {})
	assert.Equal(t, 1, svc.SubscriberCount("idem"))

	// 解除ハンドルは何度呼んでも安全。
	cancel()
	cancel()
	assert.Equal(t, 0, svc.SubscriberCount("idem"))
}

func TestPubSubService_Close_WithSubscriptions(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "test:")
	ctx := context.Background()

	_ = svc.Subscribe(ctx, "close1", func(data []byte) {})
	_ = svc.Subscribe(ctx, "close2", func(data []byte) {})

	time.Sleep(100 * time.Millisecond)

	err := svc.Close()
	assert.NoError(t, err)
}

func TestPubSubService_Close_AlreadyClosed(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "test:")
	ctx := context.Background()

	_ = svc.Subscribe(ctx, "doublecl", func(data []byte) {})
	time.Sleep(100 * time.Millisecond)

	// 内部のPubSubを手動で閉じてからCloseを呼ぶ→sub.Close()がエラーを返す
	svc.mu.Lock()
	for _, ts := range svc.subs {
		_ = ts.sub.Close()
	}
	svc.mu.Unlock()

	// svc.Close()は内部でsub.Close()エラーをログに出すが、nilを返す
	err := svc.Close()
	assert.NoError(t, err)
}

func TestPubSubService_Publish_MarshalError(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "test:")
	ctx := context.Background()
	defer svc.Close()

	// chanがJSONにmarshalできない
	err := svc.Publish(ctx, "ch", make(chan int))
	assert.Error(t, err)
}

// crossWorkerFakeMeta is a minimal MetaRepository inner used to count DB fetches
// in the cross-worker invalidation e2e (#1740).
type crossWorkerFakeMeta struct {
	fetchCount atomic.Int32
	meta       *model.Meta
}

func (r *crossWorkerFakeMeta) Fetch() (*model.Meta, error) {
	r.fetchCount.Add(1)
	return r.meta, nil
}
func (r *crossWorkerFakeMeta) Update(map[string]any) error { return nil }
func (r *crossWorkerFakeMeta) EnsureInitial(string) error  { return nil }

// TestPubSubService_CrossWorkerMetaInvalidation は #1740 の核となる合成を検証する:
// worker A が meta を更新すると internal:metaUpdated が publish され、worker B が
// 受信して自プロセスの CachedMetaRepository を invalidate し、次の Fetch で
// 再取得する (= cross-worker で default policy 変更等が伝播する)。
func TestPubSubService_CrossWorkerMetaInvalidation(t *testing.T) {
	ctx := context.Background()
	innerA := &crossWorkerFakeMeta{meta: &model.Meta{ID: "m1"}}
	innerB := &crossWorkerFakeMeta{meta: &model.Meta{ID: "m1"}}
	cachedA := repository.NewCachedMetaRepositoryWithTTL(innerA, time.Hour)
	cachedB := repository.NewCachedMetaRepositoryWithTTL(innerB, time.Hour)

	psA := NewPubSubService(testRedis.Client, "internal_e2e:")
	psB := NewPubSubService(testRedis.Client, "internal_e2e:")
	defer psA.Close()
	defer psB.Close()

	// worker A は更新時に metaUpdated を publish する。
	cachedA.SetInvalidationHook(func() { _ = psA.Publish(ctx, "metaUpdated", struct{}{}) })
	// 両 worker が購読し、受信で自 cache を invalidate する。
	psA.Subscribe(ctx, "metaUpdated", func([]byte) { cachedA.Invalidate() })
	psB.Subscribe(ctx, "metaUpdated", func([]byte) { cachedB.Invalidate() })
	time.Sleep(100 * time.Millisecond) // subscription 登録待ち

	// 両 cache を温める。
	_, _ = cachedA.Fetch()
	_, _ = cachedB.Fetch()
	require.Equal(t, int32(1), innerB.fetchCount.Load())

	// worker A が更新 → publish → worker B が invalidate。
	require.NoError(t, cachedA.Update(map[string]any{"name": "x"}))

	// B は受信後 cache が drop され、次の Fetch で再取得する。
	require.Eventually(t, func() bool {
		_, _ = cachedB.Fetch()
		return innerB.fetchCount.Load() > 1
	}, 3*time.Second, 20*time.Millisecond, "worker B should re-fetch after cross-worker metaUpdated")
}

// **同じトピックを複数が購読しているとき、片方の解除が他方を止めないこと。**
//
// これが #H-4 の本体。かつては購読ハンドルをトピック名だけでマップに入れて
// いたので、2 本目が 1 本目を上書きし、先に購読した側が解除すると後から
// 購読した側の配信が止まっていた。同じトピック (タイムライン / ハッシュタグ /
// チャンネル / ノート購読) は複数の接続が同時に購読する普通の状態なので、
// 攻撃者が居なくても 2 人目がタブを開いて 1 人目が閉じた瞬間に起きた。
func TestPubSubService_CancelDoesNotAffectOtherSubscribers(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "test:")
	ctx := context.Background()
	defer svc.Close()

	var mu sync.Mutex
	var gotA, gotB int

	cancelA := svc.Subscribe(ctx, "shared", func([]byte) {
		mu.Lock()
		gotA++
		mu.Unlock()
	})
	_ = svc.Subscribe(ctx, "shared", func([]byte) {
		mu.Lock()
		gotB++
		mu.Unlock()
	})
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, 2, svc.SubscriberCount("shared"))

	require.NoError(t, svc.Publish(ctx, "shared", "first"))
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	require.Equal(t, 1, gotA, "A が 1 通目を受け取ること")
	require.Equal(t, 1, gotB, "B が 1 通目を受け取ること")
	mu.Unlock()

	// **先に購読した A が解除する。** ここで B が巻き込まれてはいけない。
	cancelA()
	require.Equal(t, 1, svc.SubscriberCount("shared"))
	time.Sleep(100 * time.Millisecond)

	require.NoError(t, svc.Publish(ctx, "shared", "second"))
	require.NoError(t, svc.Publish(ctx, "shared", "third"))
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, gotA, "解除した A には届かないこと")
	assert.Equal(t, 3, gotB, "**B は受信し続けること** (他人の解除で止まらない)")
}

// 最後の購読者が外れたら Redis 購読も閉じること (接続を漏らさない)。
func TestPubSubService_LastCancelClosesRedisSubscription(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "test:")
	ctx := context.Background()
	defer svc.Close()

	c1 := svc.Subscribe(ctx, "reap", func([]byte) {})
	c2 := svc.Subscribe(ctx, "reap", func([]byte) {})
	time.Sleep(100 * time.Millisecond)

	svc.mu.Lock()
	_, present := svc.subs[svc.channel("reap")]
	svc.mu.Unlock()
	require.True(t, present)

	c1()
	svc.mu.Lock()
	_, stillPresent := svc.subs[svc.channel("reap")]
	svc.mu.Unlock()
	require.True(t, stillPresent, "購読者が残っている間は Redis 購読を閉じないこと")

	c2()
	svc.mu.Lock()
	_, goneNow := svc.subs[svc.channel("reap")]
	svc.mu.Unlock()
	assert.False(t, goneNow, "最後の購読者が外れたら Redis 購読を閉じること")
	assert.Equal(t, 0, svc.SubscriberCount("reap"))
}

// 同じトピックに N 人が居ても Redis 購読は 1 本だけであること。
func TestPubSubService_SingleRedisSubscriptionPerTopic(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "test:")
	ctx := context.Background()
	defer svc.Close()

	for i := 0; i < 5; i++ {
		_ = svc.Subscribe(ctx, "fanout", func([]byte) {})
	}
	time.Sleep(100 * time.Millisecond)

	svc.mu.Lock()
	n := len(svc.subs)
	svc.mu.Unlock()
	assert.Equal(t, 1, n, "トピックごとに Redis 購読は 1 本")
	assert.Equal(t, 5, svc.SubscriberCount("fanout"))
}
