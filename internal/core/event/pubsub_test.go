package event

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/redis/go-redis/v9"
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

	// 内部のPubSubを手動で閉じてからCloseを呼ぶ→ps.Close()がエラーを返す
	svc.mu.Lock()
	ps := svc.ps
	svc.mu.Unlock()
	require.NotNil(t, ps)
	_ = ps.Close()

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

// 最後の購読者が外れたらトピックを忘れること (枠を漏らさない)。
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
	require.True(t, stillPresent, "購読者が残っている間はトピックを残すこと")

	c2()
	svc.mu.Lock()
	_, goneNow := svc.subs[svc.channel("reap")]
	svc.mu.Unlock()
	assert.False(t, goneNow, "最後の購読者が外れたらトピックを忘れること")
	assert.Equal(t, 0, svc.SubscriberCount("reap"))
}

// 同じトピックに N 人が居てもトピックの枠は 1 つだけであること。
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
	assert.Equal(t, 1, n, "トピックの枠は 1 つ")
	assert.Equal(t, 5, svc.SubscriberCount("fanout"))
}

// pubsubClients returns how many pub/sub connections the test Redis holds.
func pubsubClients(t *testing.T) int {
	t.Helper()
	out, err := testRedis.Client.Do(context.Background(), "CLIENT", "LIST", "TYPE", "pubsub").Text()
	require.NoError(t, err)
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// トピックをいくつ購読しても Redis の接続は増えないこと。
//
// トピックごとに go-redis の PubSub (= 専用の TCP 接続) を張っていた頃は、
// 購読するトピックの数だけ Redis の接続が増えた。
func TestPubSubService_TopicsShareOneRedisConnection(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "conncount:")
	ctx := context.Background()
	defer svc.Close()

	before := pubsubClients(t)
	var got atomic.Int32
	for i := 0; i < 50; i++ {
		_ = svc.Subscribe(ctx, fmt.Sprintf("topic-%d", i), func([]byte) { got.Add(1) })
	}
	time.Sleep(200 * time.Millisecond)
	after := pubsubClients(t)
	assert.LessOrEqual(t, after-before, 1, "50 トピックの購読で Redis の pub/sub 接続が %d 本増えた", after-before)

	// 1 本の購読でも、各トピックへは届く。
	require.NoError(t, svc.Publish(ctx, "topic-7", map[string]int{"n": 1}))
	require.NoError(t, svc.Publish(ctx, "topic-42", map[string]int{"n": 2}))
	assert.Eventually(t, func() bool { return got.Load() == 2 }, 2*time.Second, 20*time.Millisecond)
}

// 遅いトピックが他のトピックの配信を止めないこと。
//
// 受信は全トピックで 1 本なので、受信の goroutine でハンドラを待つと、遅い
// 購読者 1 人 (重いミュートの評価など) で全員の配信が止まる。
func TestPubSubService_SlowTopicDoesNotBlockOthers(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "slow:")
	ctx := context.Background()
	defer svc.Close()

	release := make(chan struct{})
	defer close(release)
	var slowEntered atomic.Bool
	_ = svc.Subscribe(ctx, "slow", func([]byte) {
		slowEntered.Store(true)
		<-release
	})
	var fastGot atomic.Int32
	_ = svc.Subscribe(ctx, "fast", func([]byte) { fastGot.Add(1) })
	time.Sleep(100 * time.Millisecond)

	require.NoError(t, svc.Publish(ctx, "slow", 1))
	require.Eventually(t, slowEntered.Load, 2*time.Second, 10*time.Millisecond)
	require.NoError(t, svc.Publish(ctx, "fast", 1))
	assert.Eventually(t, func() bool { return fastGot.Load() == 1 }, 2*time.Second, 10*time.Millisecond,
		"遅いトピックのハンドラが止まっている間も、別のトピックへは届くこと")
}

// 1 つのトピックの中では、届いた順に配ること。
func TestPubSubService_PreservesOrderWithinTopic(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "order:")
	ctx := context.Background()
	defer svc.Close()

	var mu sync.Mutex
	var seen []int
	_ = svc.Subscribe(ctx, "seq", func(data []byte) {
		var n int
		require.NoError(t, json.Unmarshal(data, &n))
		mu.Lock()
		seen = append(seen, n)
		mu.Unlock()
	})
	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 200; i++ {
		require.NoError(t, svc.Publish(ctx, "seq", i))
	}
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == 200
	}, 3*time.Second, 20*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	for i, n := range seen {
		require.Equal(t, i, n, "順序が入れ替わった")
	}
}

// prefix の glob の特殊文字を逃がし、別のサービスのチャネルを受けないこと。
func TestPubSubService_PatternEscapesPrefix(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "a*[b]?\\:")
	assert.Equal(t, "a\\*\\[b\\]\\?\\\\:*", svc.pattern())

	ctx := context.Background()
	defer svc.Close()
	var got atomic.Int32
	_ = svc.Subscribe(ctx, "x", func([]byte) { got.Add(1) })
	time.Sleep(100 * time.Millisecond)
	// prefix を glob として読むと一致してしまうチャネル。配る先はチャネル名の
	// 完全一致で決まるので、受けたかどうかは Redis が数えた受け手の数で見る。
	n, err := testRedis.Client.Publish(ctx, "aZZbQ:x", "1").Result()
	require.NoError(t, err)
	assert.Zero(t, n, "prefix を glob として読んだチャネルまで受けた")
	require.NoError(t, svc.Publish(ctx, "x", 1))
	assert.Eventually(t, func() bool { return got.Load() == 1 }, 2*time.Second, 20*time.Millisecond)
}

// 配り待ちが上限を超えたら捨て、受信は止めないこと。
func TestPubSubService_FullTopicQueueDrops(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "full:")
	ctx := context.Background()
	defer svc.Close()

	release := make(chan struct{})
	var handled atomic.Int32
	_ = svc.Subscribe(ctx, "q", func([]byte) {
		<-release
		handled.Add(1)
	})
	// drain を 1 件目で止めた状態で、上限を超えて積む。
	for i := 0; i < topicQueueLimit+50; i++ {
		svc.enqueue(svc.channel("q"), []byte("1"))
	}
	svc.mu.Lock()
	pending := len(svc.subs[svc.channel("q")].pending)
	dropped := svc.subs[svc.channel("q")].dropped
	svc.mu.Unlock()
	close(release)
	// 配っている最中の 1 件を除き、上限を超えた分は捨てる (どの時点で drain が
	// 1 件目を取り出しても、捨てるのは 49 件以上)。
	assert.Equal(t, topicQueueLimit, pending)
	assert.GreaterOrEqual(t, dropped, uint64(49))
}

// 解除や Close の後は、待ち行列に残っていた分を古いハンドラへ配らないこと。
func TestPubSubService_NoDeliveryAfterUnsubscribeOrClose(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(svc *PubSubService, unsub func())
	}{
		{"unsubscribe", func(_ *PubSubService, unsub func()) { unsub() }},
		{"close", func(svc *PubSubService, _ func()) { _ = svc.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewPubSubService(testRedis.Client, "stale-"+tc.name+":")
			ctx := context.Background()
			defer svc.Close()

			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			var handled atomic.Int32
			unsub := svc.Subscribe(ctx, "q", func([]byte) {
				select {
				case entered <- struct{}{}:
				default:
				}
				<-release
				handled.Add(1)
			})
			// 10 件が積まれた状態から drain を始める (取り出し方の違いを
			// 時間の巡り合わせに左右されずに見る)。
			key := svc.channel("q")
			svc.mu.Lock()
			ts := svc.subs[key]
			for i := 0; i < 10; i++ {
				ts.pending = append(ts.pending, []byte("1"))
			}
			ts.draining = true
			svc.mu.Unlock()
			go svc.drain(key, ts)

			<-entered
			tc.stop(svc, unsub)
			close(release)
			time.Sleep(100 * time.Millisecond)
			assert.Equal(t, int32(1), handled.Load(), "止めた後も残りを配った")
		})
	}
}

func TestPubSubService_PublishMany(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "many:")
	ctx := context.Background()
	defer svc.Close()

	var mu sync.Mutex
	received := map[string][]string{}
	for _, ch := range []string{"a", "b", "c"} {
		svc.Subscribe(ctx, ch, func(data []byte) {
			mu.Lock()
			received[ch] = append(received[ch], string(data))
			mu.Unlock()
		})
	}
	time.Sleep(100 * time.Millisecond)

	assert.Nil(t, svc.PublishMany(ctx, []string{"a", "b"}, map[string]string{"n": "1"}))
	assert.Nil(t, svc.PublishMany(ctx, []string{"a", "b"}, json.RawMessage(`{"n":"2"}`)))
	assert.Nil(t, svc.PublishMany(ctx, nil, "ignored"))

	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received["a"]) >= 2 && len(received["b"]) >= 2
	}, 2*time.Second, 20*time.Millisecond)
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	want := []string{`{"n":"1"}`, `{"n":"2"}`}
	assert.Equal(t, want, received["a"], "same payload as Publish, in order")
	assert.Equal(t, want, received["b"])
	assert.Empty(t, received["c"], "only the listed channels")
}

func TestPubSubService_PublishManyErrors(t *testing.T) {
	ctx := context.Background()
	svc := NewPubSubService(testRedis.Client, "many:")
	errs := svc.PublishMany(ctx, []string{"a", "b"}, func() {})
	require.Len(t, errs, 2)
	assert.ErrorContains(t, errs[0], "pubsub marshal")
	assert.ErrorContains(t, errs[1], "pubsub marshal")

	// 接続できないときは、宛先ごとにエラーが返る。
	client := redis.NewClient(&redis.Options{Addr: testRedis.Addr})
	require.NoError(t, client.Close())
	closed := NewPubSubService(client, "many:")
	errs = closed.PublishMany(ctx, []string{"a", "b"}, "x")
	require.Len(t, errs, 2)
	assert.Error(t, errs[0])
	assert.Error(t, errs[1])
}
