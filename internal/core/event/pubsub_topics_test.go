package event

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// numSub returns how many subscribers Redis counts on the channel of svc.
func numSub(t *testing.T, svc *PubSubService, channel string) int64 {
	t.Helper()
	key := svc.channel(channel)
	m, err := testRedis.Client.PubSubNumSub(context.Background(), key).Result()
	require.NoError(t, err)
	return m[key]
}

// publishRaw publishes on the channel of svc and returns how many subscribers
// Redis sent the message to.
func publishRaw(t *testing.T, svc *PubSubService, channel string) int64 {
	t.Helper()
	n, err := testRedis.Client.Publish(context.Background(), svc.channel(channel), `"x"`).Result()
	require.NoError(t, err)
	return n
}

// TestPubSubService_OnlyTopicsWithSubscribersReachTheProcess pins #3991: Redis
// sends the process only the messages of topics somebody subscribed to.
//
// PSUBSCRIBE で `<prefix>*` を受けていた頃は、購読者のいない宛先の PUBLISH も
// Redis がこのプロセスへ送り、enqueue で捨てていた。届けた先の数は PUBLISH の
// 戻り値 (Redis が送った購読者の数) で見る。
func TestPubSubService_OnlyTopicsWithSubscribersReachTheProcess(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "only3991:")
	defer svc.Close()
	ctx := context.Background()

	var got atomic.Int32
	unsub := svc.Subscribe(ctx, "watched", func([]byte) { got.Add(1) })

	assert.Equal(t, int64(1), numSub(t, svc, "watched"))
	assert.Zero(t, numSub(t, svc, "unwatched"))
	assert.Zero(t, publishRaw(t, svc, "unwatched"), "購読者のいないトピックのメッセージまで受け取った")
	assert.Equal(t, int64(1), publishRaw(t, svc, "watched"))
	require.Eventually(t, func() bool { return got.Load() == 1 }, 2*time.Second, 10*time.Millisecond)

	// 同じトピックを何人が購読しても、Redis の購読は 1 つ。
	unsub2 := svc.Subscribe(ctx, "watched", func([]byte) {})
	assert.Equal(t, int64(1), numSub(t, svc, "watched"))
	unsub2()
	assert.Equal(t, int64(1), numSub(t, svc, "watched"), "購読者が残っている間は購読を外さないこと")

	// 最後の 1 人が抜けたら外す。UNSUBSCRIBE は解除の関数の中で送るので、
	// 戻った後に送った NUMSUB より先に Redis で実行される (同じ順に届くとは
	// 限らないので Eventually で待つ)。
	unsub()
	require.Eventually(t, func() bool { return numSub(t, svc, "watched") == 0 }, 2*time.Second, 10*time.Millisecond,
		"最後の購読者が抜けても Redis の購読が残っている")
	assert.Zero(t, publishRaw(t, svc, "watched"))
}

// TestPubSubService_SubscribeWaitsForRedis pins that Subscribe returns only
// after Redis applied the SUBSCRIBE, so a message published right after it
// returns is not lost.
//
// PSUBSCRIBE で全部受けていた頃は、張った後の Subscribe は戻った時点で全てを
// 受けていた。トピックごとの SUBSCRIBE で戻りを待たないと、戻った直後の PUBLISH が
// SUBSCRIBE より先に実行されうる。巡り合わせに頼らず確かめるため、CLIENT PAUSE で
// Redis にコマンドの実行を止めさせ、その間に Subscribe が戻らないことを見る。
func TestPubSubService_SubscribeWaitsForRedis(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "wait3991:")
	defer svc.Close()
	ctx := context.Background()

	// 接続を先に張っておく (止めている間の接続の確立を測らない)。
	_ = svc.Subscribe(ctx, "warm", func([]byte) {})

	const pause = 400 * time.Millisecond
	require.NoError(t, testRedis.Client.Do(ctx, "CLIENT", "PAUSE", pause.Milliseconds(), "ALL").Err())
	start := time.Now()
	var got atomic.Int32
	_ = svc.Subscribe(ctx, "fresh", func([]byte) { got.Add(1) })
	// 2 人目も、1 人目の SUBSCRIBE が効くまで待つ。
	_ = svc.Subscribe(ctx, "fresh", func([]byte) { got.Add(1) })
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, pause-100*time.Millisecond,
		"Redis が SUBSCRIBE を実行する前に Subscribe が戻った (%s)", elapsed)

	assert.Equal(t, int64(1), publishRaw(t, svc, "fresh"), "Subscribe が戻った直後の PUBLISH が届かない")
	require.Eventually(t, func() bool { return got.Load() == 2 }, 2*time.Second, 10*time.Millisecond)
}

// TestPubSubService_DeliversRightAfterSubscribe subscribes many fresh topics
// and publishes on each as soon as Subscribe returns, without sleeping.
func TestPubSubService_DeliversRightAfterSubscribe(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "fresh3991:")
	defer svc.Close()
	ctx := context.Background()

	const topics = 200
	var got atomic.Int32
	for i := 0; i < topics; i++ {
		ch := fmt.Sprintf("t%d", i)
		_ = svc.Subscribe(ctx, ch, func([]byte) { got.Add(1) })
		require.NoError(t, svc.Publish(ctx, ch, i))
	}
	assert.Eventually(t, func() bool { return got.Load() == topics }, 3*time.Second, 10*time.Millisecond,
		"Subscribe の直後の PUBLISH が届かなかった")
}

// TestPubSubService_ResubscribeRace pins that racing last-unsubscribes and
// first-subscribes of one topic leave Redis subscribed exactly when somebody
// is subscribed in the process.
//
// 判断 (トピックを作る・消す) と送信 (SUBSCRIBE / UNSUBSCRIBE) の順がずれると、
// 「UNSUBSCRIBE → SUBSCRIBE」と決めたのに「SUBSCRIBE → UNSUBSCRIBE」と送り、
// 購読者がいるのに Redis の購読が外れたままになる (逆なら、誰もいないのに残る)。
func TestPubSubService_ResubscribeRace(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "race3991:")
	defer svc.Close()
	ctx := context.Background()

	for round := 0; round < 20; round++ {
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 25; i++ {
					unsub := svc.Subscribe(ctx, "hot", func([]byte) {})
					unsub()
				}
			}()
		}
		wg.Wait()

		// 誰もいないなら、Redis の購読も無い。
		require.Eventually(t, func() bool { return numSub(t, svc, "hot") == 0 }, 2*time.Second, 10*time.Millisecond,
			"round %d: 購読者がいないのに Redis の購読が残った", round)

		// 次の購読者には届く。
		var got atomic.Int32
		unsub := svc.Subscribe(ctx, "hot", func([]byte) { got.Add(1) })
		require.Equal(t, int64(1), numSub(t, svc, "hot"), "round %d: 購読者がいるのに Redis の購読が外れている", round)
		require.NoError(t, svc.Publish(ctx, "hot", round))
		require.Eventually(t, func() bool { return got.Load() == 1 }, 2*time.Second, 10*time.Millisecond, "round %d", round)
		unsub()
	}
}

// TestPubSubService_ReconnectResubscribes pins that after the subscription
// connection is dropped, go-redis subscribes again to exactly the topics that
// still have subscribers.
func TestPubSubService_ReconnectResubscribes(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "reconn3991:")
	defer svc.Close()
	ctx := context.Background()

	var got atomic.Int32
	_ = svc.Subscribe(ctx, "kept", func([]byte) { got.Add(1) })
	gone := svc.Subscribe(ctx, "gone", func([]byte) {})
	gone()

	killed, err := testRedis.Client.Do(ctx, "CLIENT", "KILL", "TYPE", "pubsub").Int()
	require.NoError(t, err)
	require.Positive(t, killed, "前提: 購読の接続を切った")

	// 繋ぎ直すと届く。
	require.Eventually(t, func() bool {
		if publishRaw(t, svc, "kept") == 0 {
			return false
		}
		return got.Load() > 0
	}, 10*time.Second, 50*time.Millisecond, "繋ぎ直した後に届かない")
	assert.Zero(t, numSub(t, svc, "gone"), "外したトピックまで張り直した")

	// 繋ぎ直した後の購読と解除も効く。
	var got2 atomic.Int32
	unsub := svc.Subscribe(ctx, "after", func([]byte) { got2.Add(1) })
	assert.Equal(t, int64(1), publishRaw(t, svc, "after"))
	require.Eventually(t, func() bool { return got2.Load() == 1 }, 2*time.Second, 10*time.Millisecond)
	unsub()
	require.Eventually(t, func() bool { return numSub(t, svc, "after") == 0 }, 2*time.Second, 10*time.Millisecond)
}

// TestPubSubService_SubscribeAfterCloseDoesNotBlock pins that Subscribe on a
// closed service returns at once and touches no Redis subscription.
func TestPubSubService_SubscribeAfterCloseDoesNotBlock(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "closed3991:")
	_ = svc.Subscribe(context.Background(), "a", func([]byte) {})
	require.NoError(t, svc.Close())

	done := make(chan struct{})
	go func() {
		unsub := svc.Subscribe(context.Background(), "b", func([]byte) {})
		unsub()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close の後の Subscribe が戻らない")
	}
	assert.Zero(t, numSub(t, svc, "b"))
}

// TestPubSubService_SubscribeDuringLastUnsubscribe pins the commands sent
// when a subscriber arrives between the decision to unsubscribe the last one
// and the UNSUBSCRIBE being sent.
//
// 送る内容を決めた後に来た購読を取りこぼすと、購読者がいるのに購読が外れた
// ままになる。判断と送信の間で呼ぶ hook の中で購読を足し、確実に割り込ませる。
func TestPubSubService_SubscribeDuringLastUnsubscribe(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "interleave3991:")
	defer svc.Close()
	ctx := context.Background()
	key := svc.channel("x")

	var got atomic.Int32
	var fired atomic.Bool
	subscribed := make(chan func(), 1)
	svc.beforeSync = func(_, unsubscribe []string) {
		if len(unsubscribe) != 1 || unsubscribe[0] != key || !fired.CompareAndSwap(false, true) {
			return
		}
		go func() {
			subscribed <- svc.Subscribe(ctx, "x", func([]byte) { got.Add(1) })
		}()
		// 登録が済み、送る内容の判断を待っている状態になってから送らせる。
		assert.Eventually(t, func() bool { return svc.SubscriberCount("x") == 1 }, time.Second, time.Millisecond)
	}
	first := svc.Subscribe(ctx, "x", func([]byte) {})
	first()
	var unsub func()
	select {
	case unsub = <-subscribed:
	case <-time.After(5 * time.Second):
		t.Fatal("割り込んだ Subscribe が戻らない")
	}
	defer unsub()
	require.True(t, fired.Load(), "前提: UNSUBSCRIBE の直前に割り込んだ")

	assert.Equal(t, int64(1), numSub(t, svc, "x"), "購読者がいるのに Redis の購読が外れた")
	require.NoError(t, svc.Publish(ctx, "x", 1))
	assert.Eventually(t, func() bool { return got.Load() == 1 }, 2*time.Second, 10*time.Millisecond)
}

// TestPubSubService_ConcurrentSubscriberWaitsForFirst pins that a second
// subscriber of a topic whose SUBSCRIBE is still in flight also waits for it.
func TestPubSubService_ConcurrentSubscriberWaitsForFirst(t *testing.T) {
	svc := NewPubSubService(testRedis.Client, "second3991:")
	defer svc.Close()
	ctx := context.Background()
	_ = svc.Subscribe(ctx, "warm", func([]byte) {})

	const pause = 400 * time.Millisecond
	require.NoError(t, testRedis.Client.Do(ctx, "CLIENT", "PAUSE", pause.Milliseconds(), "ALL").Err())
	start := time.Now()
	go func() { _ = svc.Subscribe(ctx, "y", func([]byte) {}) }()
	// 1 人目が登録を済ませ、Redis の応答を待っている状態にしてから 2 人目を入れる。
	require.Eventually(t, func() bool { return svc.SubscriberCount("y") == 1 }, time.Second, time.Millisecond)
	var got atomic.Int32
	_ = svc.Subscribe(ctx, "y", func([]byte) { got.Add(1) })
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, pause-100*time.Millisecond,
		"1 人目の SUBSCRIBE が効く前に 2 人目の Subscribe が戻った (%s)", elapsed)
	assert.Equal(t, int64(1), publishRaw(t, svc, "y"))
	require.Eventually(t, func() bool { return got.Load() == 1 }, 2*time.Second, 10*time.Millisecond)
}

// failOnceConn fails the first write whose bytes contain needle.
type failOnceConn struct {
	net.Conn
	needle []byte
	failed *atomic.Bool
}

func (c *failOnceConn) Write(b []byte) (int, error) {
	if bytes.Contains(b, c.needle) && c.failed.CompareAndSwap(false, true) {
		_ = c.Conn.Close()
		return 0, errors.New("injected write failure")
	}
	return c.Conn.Write(b)
}

// TestPubSubService_LostSubscribeIsSentAgain pins that a SUBSCRIBE lost with
// its connection is sent again before Subscribe returns.
//
// go-redis の PubSub.Subscribe は送ってからチャネルを覚える。送信が失敗すると、
// その中で繋ぎ直して覚えているチャネルを張り直すが、送ろうとしたチャネルはまだ
// 覚えていないので張り直されない。印 (UNSUBSCRIBE) の応答は新しい接続で正常に
// 返るので、印だけを見ると購読が効いたと取り違える。
func TestPubSubService_LostSubscribeIsSentAgain(t *testing.T) {
	var failed atomic.Bool
	client := redis.NewClient(&redis.Options{
		Addr: testRedis.Addr,
		Dialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &failOnceConn{Conn: c, needle: []byte("victim3991"), failed: &failed}, nil
		},
	})
	t.Cleanup(func() { _ = client.Close() })
	svc := NewPubSubService(client, "lost3991:")
	defer svc.Close()
	ctx := context.Background()

	_ = svc.Subscribe(ctx, "warm", func([]byte) {})
	var got atomic.Int32
	start := time.Now()
	_ = svc.Subscribe(ctx, "victim3991", func([]byte) { got.Add(1) })
	elapsed := time.Since(start)
	require.True(t, failed.Load(), "前提: SUBSCRIBE の送信を失敗させた")
	// 送り直しの最初の間隔は短い。確かめる上限 (2 秒) まで待って戻ったのなら、
	// 送り直しが遅い。
	assert.Less(t, elapsed, 500*time.Millisecond, "抜けた SUBSCRIBE の送り直しが遅い (%s)", elapsed)

	assert.Equal(t, int64(1), numSub(t, svc, "victim3991"), "失った SUBSCRIBE を送り直さずに戻った")
	assert.Equal(t, int64(1), numSub(t, svc, "warm"), "繋ぎ直しで外れた")
	require.NoError(t, svc.Publish(ctx, "victim3991", 1))
	assert.Eventually(t, func() bool { return got.Load() == 1 }, 2*time.Second, 10*time.Millisecond)
}

// countingHandler counts the records logged at Warn or above whose message
// starts with "pubsub".
type countingHandler struct{ n *atomic.Int32 }

func (h countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn && strings.HasPrefix(r.Message, "pubsub") {
		h.n.Add(1)
	}
	return nil
}
func (h countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h countingHandler) WithGroup(string) slog.Handler      { return h }

// TestPubSubService_UnconfirmedRedisDoesNotStallSubscribe pins what Subscribe
// does while Redis does not answer: only the first new topic waits for the
// limit, the later ones return at once, the outage is logged once, and every
// topic is subscribed once Redis answers again.
//
// 待ち続けると、ハッシュタグのように順に何本も購読する接続や起動時の購読が、
// 本数 × 上限だけ止まる。
func TestPubSubService_UnconfirmedRedisDoesNotStallSubscribe(t *testing.T) {
	var warns atomic.Int32
	prev := slog.Default()
	slog.SetDefault(slog.New(countingHandler{n: &warns}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	svc := NewPubSubService(testRedis.Client, "stall3991:")
	svc.confirmTimeout = 300 * time.Millisecond
	defer svc.Close()
	ctx := context.Background()
	_ = svc.Subscribe(ctx, "warm", func([]byte) {})

	require.NoError(t, testRedis.Client.Do(ctx, "CLIENT", "PAUSE", 1500, "ALL").Err())
	var got atomic.Int32
	start := time.Now()
	_ = svc.Subscribe(ctx, "t0", func([]byte) { got.Add(1) })
	first := time.Since(start)
	assert.GreaterOrEqual(t, first, 250*time.Millisecond, "前提: 1 件目は確かめるのを待って諦める (%s)", first)
	for i := 1; i < 10; i++ {
		start := time.Now()
		_ = svc.Subscribe(ctx, fmt.Sprintf("t%d", i), func([]byte) { got.Add(1) })
		assert.Less(t, time.Since(start), 100*time.Millisecond, "Redis が応答しない間に %d 件目が待った", i)
	}

	// Redis が応答するようになったら、全部が購読され、待つ形に戻る。
	require.Eventually(t, func() bool {
		for i := 0; i < 10; i++ {
			if numSub(t, svc, fmt.Sprintf("t%d", i)) != 1 {
				return false
			}
		}
		return true
	}, 5*time.Second, 50*time.Millisecond, "繋がり直した後も購読されない")
	require.Eventually(t, func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		return !svc.degraded
	}, 5*time.Second, 20*time.Millisecond)
	for i := 0; i < 10; i++ {
		require.NoError(t, svc.Publish(ctx, fmt.Sprintf("t%d", i), i))
	}
	assert.Eventually(t, func() bool { return got.Load() == 10 }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, int32(1), warns.Load(), "障害の間のログは 1 回だけ")
}

// subscribeRejectedCalls returns rejected_calls of SUBSCRIBE from INFO.
func subscribeRejectedCalls(t *testing.T) int64 {
	t.Helper()
	info, err := testRedis.Client.Info(context.Background(), "commandstats").Result()
	require.NoError(t, err)
	for _, line := range strings.Split(info, "\r\n") {
		rest, ok := strings.CutPrefix(line, "cmdstat_subscribe:")
		if !ok {
			continue
		}
		for _, kv := range strings.Split(rest, ",") {
			if v, ok := strings.CutPrefix(kv, "rejected_calls="); ok {
				n, err := strconv.ParseInt(v, 10, 64)
				require.NoError(t, err)
				return n
			}
		}
	}
	return 0
}

// TestPubSubService_RejectedChannelDoesNotBlockOthers pins what happens when
// Redis refuses to subscribe one channel (an ACL that allows only some
// channels): the other channels are still subscribed, also when they were sent
// in the same SUBSCRIBE as the refused one, and the refused one is sent again
// with growing intervals instead of every few milliseconds.
//
// Redis は SUBSCRIBE をコマンドごと NOPERM で拒み、go-redis はそのエラーを
// 受信の側で読み捨てる。印は応答するので、拒まれたチャネルは「抜けた」と見える。
func TestPubSubService_RejectedChannelDoesNotBlockOthers(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, testRedis.Client.Do(ctx, "ACL", "SETUSER", "u3991", "on", ">pw3991", "resetchannels",
		"&acl3991:ok*", "+@all", "~*").Err())
	t.Cleanup(func() { _ = testRedis.Client.Do(context.Background(), "ACL", "DELUSER", "u3991").Err() })
	client := redis.NewClient(&redis.Options{Addr: testRedis.Addr, Username: "u3991", Password: "pw3991"})
	t.Cleanup(func() { _ = client.Close() })
	svc := NewPubSubService(client, "acl3991:")
	defer svc.Close()

	before := subscribeRejectedCalls(t)
	// 許されないチャネルと許されるチャネルを、同じ SUBSCRIBE にまとめて送らせる。
	// 先の周 (okwarm) を送る前に止め、その間に 2 つを登録する。
	var got atomic.Int32
	var together atomic.Bool
	svc.beforeSync = func(subscribe, _ []string) {
		if slices.Equal(subscribe, []string{"acl3991:okwarm"}) {
			assert.Eventually(t, func() bool {
				return svc.SubscriberCount("bad") == 1 && svc.SubscriberCount("ok1") == 1
			}, 2*time.Second, time.Millisecond)
		}
		if len(subscribe) == 2 && slices.Contains(subscribe, "acl3991:bad") && slices.Contains(subscribe, "acl3991:ok1") {
			together.Store(true)
		}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = svc.Subscribe(ctx, "okwarm", func([]byte) {})
	}()
	require.Eventually(t, func() bool { return svc.SubscriberCount("okwarm") == 1 }, time.Second, time.Millisecond)
	for _, ch := range []string{"bad", "ok1"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = svc.Subscribe(ctx, ch, func([]byte) { got.Add(1) })
		}()
	}
	wg.Wait()
	require.True(t, together.Load(), "前提: bad と ok1 を同じ SUBSCRIBE で送った")
	// bad の Subscribe は確かめられないまま上限で戻るが、Redis は印に応答している
	// ので、待たない形 (degraded) に落ちてはいけない。
	svc.mu.Lock()
	degraded := svc.degraded
	svc.mu.Unlock()
	assert.False(t, degraded, "Redis が応答しているのに degraded になった")
	_ = svc.Subscribe(ctx, "ok2", func([]byte) { got.Add(1) })

	assert.Equal(t, int64(1), numSub(t, svc, "ok1"), "拒まれたチャネルと一緒に送ったチャネルが張られない")
	assert.Equal(t, int64(1), numSub(t, svc, "ok2"), "拒まれたチャネルの後に購読したチャネルが張られない")
	assert.Zero(t, numSub(t, svc, "bad"))
	require.NoError(t, svc.Publish(ctx, "ok1", 1))
	require.NoError(t, svc.Publish(ctx, "ok2", 1))
	assert.Eventually(t, func() bool { return got.Load() == 2 }, 2*time.Second, 10*time.Millisecond)

	// 拒まれたチャネルの送り直しは、間隔を広げていく (50ms ごとなら 3 秒で 60 回)。
	// その間、別のトピックの購読と解除を続け、そのたびに起きる周でも拒まれる
	// チャネルを送り直さないことを見る。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		unsub := svc.Subscribe(ctx, "okchurn", func([]byte) {})
		unsub()
		time.Sleep(20 * time.Millisecond)
	}
	rejected := subscribeRejectedCalls(t) - before
	t.Logf("rejected SUBSCRIBE calls: %d", rejected)
	assert.Positive(t, rejected, "前提: Redis が SUBSCRIBE を拒んだ")
	assert.LessOrEqual(t, rejected, int64(15), "拒まれたチャネルを送り直し続けた (%d 回)", rejected)
}
