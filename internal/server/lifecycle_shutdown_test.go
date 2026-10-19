package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"

	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/elythia-network/elythia/plugin"
)

// TestSupervisor_ShutdownDrainsBeforeClosingTheDB checks the order of the
// process shutdown: new requests get 503 at once, the requests in flight
// finish on the generation, and only then is the DB pool closed (#3463
// review H1). 差し替えずに止めると、処理中のリクエストが閉じたプールに当たる。
func TestSupervisor_ShutdownDrainsBeforeClosingTheDB(t *testing.T) {
	f := newFakeGenerations(t)
	sup, err := NewSupervisor(&config.Config{}, nil, f.deps(5*time.Second))
	require.NoError(t, err)

	slowDone := make(chan int, 1)
	go func() {
		code, _ := getBody(t, sup, "/slow")
		slowDone <- code
	}()
	<-f.entered

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- sup.Shutdown(context.Background()) }()
	require.Eventually(t, func() bool {
		code, _ := getBody(t, sup, "/healthz")
		return code == http.StatusServiceUnavailable
	}, 5*time.Second, 5*time.Millisecond, "停止中に届いたリクエストが古い世代へ行っている")
	select {
	case <-shutdownDone:
		t.Fatal("処理中のリクエストを待たずに Shutdown が終わった")
	case <-time.After(100 * time.Millisecond):
	}
	assert.Zero(t, f.closed.Load(), "処理中のリクエストがあるのに DB を閉じた")

	close(f.release)
	assert.Equal(t, http.StatusOK, <-slowDone)
	require.NoError(t, <-shutdownDone)
	assert.EqualValues(t, 1, f.closed.Load())
}

// TestSupervisor_ShutdownRunsHooksEvenWithSlowRequests reproduces #3463
// review H1': a request that outlives the shutdown budget must not keep the
// shutdown hooks (chart SaveAll, buffer flushes, queue stop) from running,
// and they must get time to run.
func TestSupervisor_ShutdownRunsHooksEvenWithSlowRequests(t *testing.T) {
	f := newFakeGenerations(t)
	hook := make(chan time.Duration, 1)
	f.hook = func(ctx context.Context) {
		deadline, _ := ctx.Deadline()
		hook <- time.Until(deadline)
	}
	sup, err := NewSupervisor(&config.Config{}, nil, f.deps(5*time.Second))
	require.NoError(t, err)
	defer close(f.release)
	go func() { _, _ = getBody(t, sup, "/slow") }()
	<-f.entered

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_ = sup.Shutdown(ctx)
	select {
	case left := <-hook:
		// 待ちの段は全体の 1/3 (0.5 秒) まで。hook には残りの約 1 秒がある。
		assert.Greater(t, left, 700*time.Millisecond, "shutdown hook に時間が残っていない")
	default:
		t.Fatal("処理中のリクエストが残ったまま、shutdown hook を呼ばずに返った")
	}
	assert.Less(t, time.Since(start), 2*time.Second, "Shutdown が期限を大きく超えた")
	assert.EqualValues(t, 1, f.closed.Load(), "DB を閉じていない")
}

// TestSupervisor_ShutdownWithAnExpiredContextStillRunsHooks checks that
// Shutdown starts the background stop even when its ctx is already done
// (#3463 review H1' / L6). Shutdown は停止を始めた所で返るので、hook の呼び出しは
// 期限付きで待つ (直後に見ると、hook を呼ぶ goroutine と競争する)。
func TestSupervisor_ShutdownWithAnExpiredContextStillRunsHooks(t *testing.T) {
	f := newFakeGenerations(t)
	called := make(chan struct{}, 1)
	f.hook = func(context.Context) { called <- struct{}{} }
	sup, err := NewSupervisor(&config.Config{}, nil, f.deps(5*time.Second))
	require.NoError(t, err)
	defer close(f.release)
	go func() { _, _ = getBody(t, sup, "/slow") }()
	<-f.entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = sup.Shutdown(ctx)
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx が切れていたので、shutdown hook を呼ばなかった")
	}
}

// TestSupervisor_ShutdownIsBoundedWhileASwitchHoldsTheLock checks that a
// maintenance switch holding the lock does not keep Shutdown past its budget
// (#3463 review L6).
func TestSupervisor_ShutdownIsBoundedWhileASwitchHoldsTheLock(t *testing.T) {
	f := newFakeGenerations(t)
	sup, err := NewSupervisor(&config.Config{}, nil, f.deps(5*time.Second))
	require.NoError(t, err)
	sup.genMu.Lock() // メンテナンスの切り替えが長く掛かっている状態
	defer sup.genMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_ = sup.Shutdown(ctx)
	// 切り替えの終わりを待つのは、待ちの段の上限 (全体の 1/3) まで。
	assert.Less(t, time.Since(start), 1200*time.Millisecond)
}

func TestNewShutdownBudget(t *testing.T) {
	now := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(9*time.Second))
	defer cancel()
	b := newShutdownBudget(ctx, now)
	assert.Equal(t, now.Add(3*time.Second), b.drainUntil)
	assert.Equal(t, now.Add(9*time.Second), b.deadline)
	b = newShutdownBudget(context.Background(), now)
	assert.Equal(t, now.Add(shutdownBudgetDefault), b.deadline)
	// 期限切れ・取り消し済みなら待たず、背景処理の停止には最低限の時間を渡す。
	expired, cancel2 := context.WithDeadline(context.Background(), now.Add(-time.Second))
	defer cancel2()
	b = newShutdownBudget(expired, now)
	assert.Equal(t, now, b.drainUntil)
	assert.Equal(t, now.Add(shutdownStopMin), b.deadline)
	canceled, cancel3 := context.WithCancel(context.Background())
	cancel3()
	b = newShutdownBudget(canceled, now)
	assert.Equal(t, now, b.drainUntil)
	assert.Equal(t, now.Add(shutdownStopMin), b.deadline)
}

// TestServer_StopBackgroundSends1012AndLeavesTimeForHooks checks that
// stopping a generation closes the WebSocket connections with close code
// 1012, and that a connection that does not go away (a client that stopped
// reading) does not eat the time of the shutdown hooks (#3463 review M1 / M3
// / L3).
func TestServer_StopBackgroundSends1012AndLeavesTimeForHooks(t *testing.T) {
	m := stream.NewManager(nil, nil)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		go m.Accept(conn, &model.User{ID: "alice"}, nil, "")
	}))
	t.Cleanup(ts.Close)
	dial := func() *websocket.Conn {
		ws, _, err := (&websocket.Dialer{HandshakeTimeout: 5 * time.Second}).Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ws.Close() })
		return ws
	}
	ws := dial()
	require.Eventually(t, func() bool { return m.Count() == 1 }, 5*time.Second, 10*time.Millisecond)
	// 読まなくなったクライアント。大きな送信で書き込みを詰まらせ、close frame を
	// 送れない (=登録が外れない) 状態にする。
	_ = dial()
	require.Eventually(t, func() bool { return m.Count() == 2 }, 5*time.Second, 10*time.Millisecond)
	stuck := m.Get("2")
	require.NotNil(t, stuck)
	big := strings.Repeat("x", 4<<20)
	for range 16 {
		require.NoError(t, stuck.Send(big))
	}

	s := &Server{echo: echo.New(), config: &config.Config{}, role: config.RoleServer, streamManager: m}
	hookHadTime := make(chan time.Duration, 1)
	s.addPart("testPart", nil, func(ctx context.Context) {
		deadline, _ := ctx.Deadline()
		hookHadTime <- time.Until(deadline)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, s.StopBackground(ctx))
	assert.Greater(t, <-hookHadTime, 1500*time.Millisecond, "streaming の待ちが shutdown hook の時間を食い潰した")

	require.NoError(t, ws.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, _, err := ws.ReadMessage()
	var ce *websocket.CloseError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, 1012, ce.Code)
}

// TestNew_FailedConstructionClosesItsSubscriptions checks that a server that
// fails to build releases the Redis subscriptions setupRoutes opened. 作り直しの
// 失敗はメンテナンスの後で繰り返すので、漏れると試すたびに増える (#3463 review M2)。
//
// **PubSubService はパターン (PSUBSCRIBE) で購読するので、PubSubNumPat で数える。**
// 以前は PubSubChannels("*") で数えていたが、miniredis はパターンの購読をそこに
// 出さないので、cleanupConstruction の Close を消しても通っていた。前提 (購読を
// 張ってから失敗している) も、失敗させる時点のパターンの数で直接確かめる。
func TestNew_FailedConstructionClosesItsSubscriptions(t *testing.T) {
	db, clients, mr, cfg := constructionFixture(t)
	restoreProcessGlobals(t)
	var built *Server
	patternsAtFailure := -1
	fail := failingRoutesWith(func() { patternsAtFailure = mr.PubSubNumPat() })
	_, err := newServer(cfg, db, clients, []plugin.Definition{fail}, noopStorage, func(s *Server) { built = s })
	require.Error(t, err)
	require.Positive(t, patternsAtFailure, "前提: 購読を張ってから失敗している")
	// 待った後の数を出すため EventuallyWithT で読む (Eventually の引数は待つ前に評価される)。
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Zero(c, mr.PubSubNumPat(), "失敗した構築の購読が残っている")
	}, 5*time.Second, 20*time.Millisecond)
	// 失敗した構築の箱も閉じている (#3661)。使われない世代の goroutine を起動しない。
	assert.False(t, built.background.Go(func(context.Context) {}), "失敗した構築の箱が閉じていない")
}

// TestNewServer_ConstructionIsSerialized checks that two constructions in one
// process do not overlap: newServer replaces process-wide state while it
// wires the routes, so a second one must wait for the first.
func TestNewServer_ConstructionIsSerialized(t *testing.T) {
	db, clients, _, cfg := constructionFixture(t)
	restoreProcessGlobals(t)
	firstIn := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondIn := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = newServer(cfg, db, clients, []plugin.Definition{failingRoutes()}, noopStorage, func(*Server) {
			close(firstIn)
			<-releaseFirst
		})
	}()
	<-firstIn
	go func() {
		defer wg.Done()
		_, _ = newServer(cfg, db, clients, []plugin.Definition{failingRoutes()}, noopStorage, func(*Server) { close(secondIn) })
	}()
	select {
	case <-secondIn:
		t.Fatal("1 つ目の構築の途中で、2 つ目の構築が始まった")
	case <-time.After(300 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case <-secondIn:
	case <-time.After(10 * time.Second):
		t.Fatal("1 つ目が終わっても、2 つ目の構築が始まらない")
	}
	wg.Wait()
}

// failingRoutes is a plugin whose Routes fails, so a construction stops
// right after setupRoutes has opened its subscriptions.
func failingRoutes() plugin.Definition { return failingRoutesWith(nil) }

// failingRoutesWith is failingRoutes that calls onFail (when non-nil) just
// before Routes fails.
func failingRoutesWith(onFail func()) plugin.Definition {
	return plugin.Definition{
		Name: "fails-to-route", APIVersion: plugin.APIVersion,
		Routes: func(plugin.Context, plugin.Router) error {
			if onFail != nil {
				onFail()
			}
			return errors.New("boom")
		},
	}
}

// constructionFixture returns what newServer needs: a migrated test DB, Redis
// clients on a miniredis and a config pointing at it.
func constructionFixture(t *testing.T) (*gorm.DB, *cache.RedisClients, *miniredis.Miniredis, *config.Config) {
	t.Helper()
	t.Setenv(config.EnvOnlyServer, "")
	t.Setenv(config.EnvOnlyQueue, "")
	db, err := testutil.OpenTestDB()
	require.NoError(t, err)
	testutil.ApplyMigrations(db)

	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	clients := &cache.RedisClients{Default: rc, Pubsub: rc, JobQueue: rc, Timelines: rc, Reactions: rc}
	port, err := strconv.Atoi(mr.Port())
	require.NoError(t, err)
	ro := config.RedisOptions{Host: mr.Host(), Port: port}
	cfg := &config.Config{
		URL: "http://example.test", Host: "example.test", Hostname: "example.test",
		Scheme: "http", WsScheme: "ws", ID: "aidx", TestMode: true,
		JobQueueDriver: "mkq", MediaProxySecret: []byte("test-secret"),
		Redis: ro, RedisForPubsub: ro, RedisForJobQueue: ro, RedisForTimelines: ro, RedisForReactions: ro,
	}
	return db, clients, mr, cfg
}

// TestStreamCloseContext pins the bound of the streaming close wait: at most
// streamCloseMax and a third of what is left, so the hooks keep the rest.
func TestStreamCloseContext(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx, c := streamCloseContext(parent)
	defer c()
	deadline, ok := ctx.Deadline()
	require.True(t, ok)
	assert.LessOrEqual(t, time.Until(deadline), time.Second+50*time.Millisecond)

	ctx2, c2 := streamCloseContext(context.Background())
	defer c2()
	deadline, ok = ctx2.Deadline()
	require.True(t, ok)
	assert.InDelta(t, float64(streamCloseMax), float64(time.Until(deadline)), float64(100*time.Millisecond))
}

// TestSupervisor_ShutdownReturnsAtItsDeadlineOnceHooksAreCalled checks that a
// hook that runs past the deadline does not hold Shutdown: once the
// background stop has been called, Shutdown returns when its ctx is done and
// the rest continues in the background (#3463 review L6).
func TestSupervisor_ShutdownReturnsAtItsDeadlineOnceHooksAreCalled(t *testing.T) {
	f := newFakeGenerations(t)
	hookDone := make(chan struct{})
	f.hook = func(context.Context) {
		time.Sleep(3 * time.Second) // ctx を見ない、遅い hook
		close(hookDone)
	}
	sup, err := NewSupervisor(&config.Config{}, nil, f.deps(5*time.Second))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	assert.ErrorIs(t, sup.Shutdown(ctx), context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 2500*time.Millisecond, "遅い hook の終わりまで Shutdown が返らない")
	<-hookDone
}

// TestSupervisor_ShutdownWaitsForAHandlerInstalledWhileItWaitedForTheLock
// fixes #3463 review L-3: a switch that held the lock could install the
// handler of a new generation after Shutdown routed requests to
// unavailable; Shutdown then closed the DB without waiting for the requests
// on that handler.
func TestSupervisor_ShutdownWaitsForAHandlerInstalledWhileItWaitedForTheLock(t *testing.T) {
	f := newFakeGenerations(t)
	sup, err := NewSupervisor(&config.Config{}, nil, f.deps(5*time.Second))
	require.NoError(t, err)
	sup.genMu.Lock() // メンテナンスを抜ける作り直しの途中
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- sup.Shutdown(context.Background()) }()
	require.Eventually(t, func() bool {
		code, _ := getBody(t, sup, "/healthz")
		return code == http.StatusServiceUnavailable
	}, 5*time.Second, 5*time.Millisecond)
	// 作り直しが新しい世代のハンドラーを差し込み、そこへリクエストが届く。
	sup.setHandler(sup.gen.srv.ServeHandler())
	slowDone := make(chan int, 1)
	go func() {
		code, _ := getBody(t, sup, "/slow")
		slowDone <- code
	}()
	recvWithin(t, f.entered, "遅いリクエストが世代に届かない")
	sup.genMu.Unlock()
	select {
	case <-shutdownDone:
		t.Fatal("差し込まれたハンドラーの処理中を待たずに Shutdown が終わった")
	case <-time.After(200 * time.Millisecond):
	}
	assert.Zero(t, f.closed.Load(), "処理中のリクエストがあるのに DB を閉じた")
	code, _ := getBody(t, sup, "/healthz")
	assert.Equal(t, http.StatusServiceUnavailable, code, "Shutdown がロックを取った後も新しい世代へ回している")
	// WebSocket の upgrade は止めた印を見ずに今のハンドラーへ渡るので、差し替え
	// 直していなければ新しい世代に届く。
	upgrade := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	upgrade.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	sup.ServeHTTP(rec, upgrade)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "ロックを取った後に unavailable へ差し替え直していない")
	close(f.release)
	assert.Equal(t, http.StatusOK, recvWithin(t, slowDone, "遅いリクエストが終わらない"))
	require.NoError(t, recvWithin(t, shutdownDone, "Shutdown が終わらない"))
	assert.EqualValues(t, 1, f.closed.Load())
}

// recvWithin receives from ch, failing the test (instead of hanging) after
// 10 seconds.
func recvWithin[T any](t *testing.T, ch <-chan T, msg string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal(msg)
	}
	var zero T
	return zero
}
