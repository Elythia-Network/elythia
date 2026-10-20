package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/labstack/echo/v4"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/core/event"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// captureSlog sends the default slog logger, as JSON, to a buffer until the
// test ends (lockedBuffer is in trust_proxy_wiring_test.go).
//
// slogの既定はプロセス全体で共有されるので、t.Cleanupで戻す(CLAUDE.md Section 4)。
func captureSlog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// stopPanicLogMsg is the message stopAll logs for a stop that panicked, as
// it appears in the JSON log.
const stopPanicLogMsg = `"msg":"server: a lifecycle part panicked while stopping; stopping the rest"`

// panickedParts returns the errors stopAll joined, one per panicked part.
func panickedParts(t *testing.T, err error) []error {
	t.Helper()
	joined, ok := err.(interface{ Unwrap() []error })
	require.True(t, ok, "the error does not join the errors of the parts: %T", err)
	return joined.Unwrap()
}

// TestLifecycle_StopAllRecoversAPanickingStop checks that when the stop of a
// part panics, stopAll logs it with the part's name, the panic value and the
// stack, still stops every later part in order with the same ctx, and returns
// one error per panicked part (#3707).
func TestLifecycle_StopAllRecoversAPanickingStop(t *testing.T) {
	logs := captureSlog(t)
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "shutdown")
	var got []string
	var l lifecycle
	for _, name := range []string{"first", "second", "third", "fourth", "fifth"} {
		l.add(name, nil, func(c context.Context) {
			assert.Equal(t, "shutdown", c.Value(key{}), "part %s did not get the shutdown ctx", name)
			got = append(got, name)
			if name == "second" || name == "fourth" {
				panic(name + " failed")
			}
		})
	}
	err := l.stopAll(ctx)
	assert.Equal(t, []string{"first", "second", "third", "fourth", "fifth"}, got)
	require.ErrorIs(t, err, errLifecycleStop)
	parts := panickedParts(t, err)
	require.Len(t, parts, 2)
	assert.ErrorIs(t, parts[0], errLifecycleStop)
	assert.ErrorContains(t, parts[0], `part "second" panicked: second failed`)
	assert.ErrorIs(t, parts[1], errLifecycleStop)
	assert.ErrorContains(t, parts[1], `part "fourth" panicked: fourth failed`)

	out := logs.String()
	for _, name := range []string{"second", "fourth"} {
		assert.Contains(t, out, `"part":"`+name+`"`)
		assert.Contains(t, out, `"panic":"`+name+` failed"`)
	}
	// スタックはrecoverした部品の停止の位置を含む。
	assert.Contains(t, out, `"stack":"goroutine `)
	assert.Contains(t, out, "TestLifecycle_StopAllRecoversAPanickingStop")
	assert.Equal(t, 2, strings.Count(out, stopPanicLogMsg))
}

// closeRecordingDriver is a driver.Driver whose Close records itself in a log
// shared with the parts of a test. Other Driver methods are not used by the
// stop of a server-role generation and panic.
type closeRecordingDriver struct {
	driver.Driver
	log *[]string
}

func (d *closeRecordingDriver) Close() error {
	*d.log = append(*d.log, "queueDriver.Close")
	return nil
}

// panicStopServer returns a server-role generation whose parts first, the
// ones in panicking (whose stops panic after recording) and last record their
// stop in the returned log, whose queue driver records its Close there too,
// and whose internalPubSub holds a pattern subscription on mr.
func panicStopServer(t *testing.T, mr *miniredis.Miniredis, names []string, panicking map[string]bool) (*Server, *[]string) {
	t.Helper()
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	ps := event.NewPubSubService(rc, "test:")
	ps.Subscribe(context.Background(), "topic", func([]byte) {})
	require.Eventually(t, func() bool { return mr.PubSubNumPat() == 1 }, 5*time.Second, 10*time.Millisecond,
		"前提: 購読を張っている")
	log := &[]string{}
	s := &Server{
		echo: echo.New(), config: &config.Config{}, role: config.RoleServer,
		queueDriver: &closeRecordingDriver{log: log}, internalPubSub: ps,
	}
	for _, name := range names {
		s.addPart(name, nil, func(context.Context) {
			*log = append(*log, name)
			if panicking[name] {
				panic(name + " failed")
			}
		})
	}
	return s, log
}

// TestServer_StopBackgroundGoesOnAfterAPanickingStop checks that when the
// stop of a part in the middle of the list panics, StopBackground still stops
// the later parts in order and runs the steps after the list (closing the
// queue driver and the Redis subscriptions), and returns an error naming every
// panicked part (#3707).
func TestServer_StopBackgroundGoesOnAfterAPanickingStop(t *testing.T) {
	captureSlog(t)
	mr := miniredis.RunT(t)
	s, log := panicStopServer(t, mr, []string{"first", "second", "third", "fourth"},
		map[string]bool{"second": true, "third": true})

	err := s.StopBackground(context.Background())
	assert.Equal(t, []string{"first", "second", "third", "fourth", "queueDriver.Close"}, *log)
	require.ErrorIs(t, err, errLifecycleStop)
	assert.ErrorContains(t, err, `part "second" panicked: second failed`)
	assert.ErrorContains(t, err, `part "third" panicked: third failed`)
	assert.NotContains(t, err.Error(), `"first"`)
	assert.NotContains(t, err.Error(), `"fourth"`)
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Zero(c, mr.PubSubNumPat(), "the Redis subscriptions were not closed after the panic")
	}, 5*time.Second, 20*time.Millisecond)
	// 2回目は1回目の結果を返し、止め直さない。
	assert.Equal(t, err, s.StopBackground(context.Background()))
	assert.Len(t, *log, 5)
}

// TestServer_StopBackgroundWithoutAPanic checks that a stop without a panic
// stops the parts in order, runs the steps after the list and returns nil, as
// before #3707.
func TestServer_StopBackgroundWithoutAPanic(t *testing.T) {
	mr := miniredis.RunT(t)
	s, log := panicStopServer(t, mr, []string{"first", "second", "third"}, nil)
	require.NoError(t, s.StopBackground(context.Background()))
	assert.Equal(t, []string{"first", "second", "third", "queueDriver.Close"}, *log)
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Zero(c, mr.PubSubNumPat())
	}, 5*time.Second, 20*time.Millisecond)
}

// serveBlockedRequest makes the http.Server of s.echo serve one request that
// does not return until the test ends, so that echo's Shutdown waits for it
// and fails with the error of its ctx.
func serveBlockedRequest(t *testing.T, s *Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	inHandler := make(chan struct{})
	release := make(chan struct{})
	s.echo.GET("/block", func(c echo.Context) error {
		close(inHandler)
		<-release
		return c.NoContent(http.StatusNoContent)
	})
	s.echo.Server.Handler = s.echo
	go func() { _ = s.echo.Server.Serve(ln) }()
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/block")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	t.Cleanup(func() {
		close(release)
		_ = s.echo.Server.Close()
	})
	select {
	case <-inHandler:
	case <-time.After(5 * time.Second):
		t.Fatal("前提: リクエストがhandlerに届かない")
	}
}

// TestServer_StopBackgroundJoinsThePanicWithTheEchoError checks that the
// error of StopBackground keeps the error of echo's Shutdown next to the
// errors of the panicked parts, and that without a panic it is echo's error
// itself, not wrapped (#3707).
func TestServer_StopBackgroundJoinsThePanicWithTheEchoError(t *testing.T) {
	captureSlog(t)
	for _, tc := range []struct {
		name      string
		panicking map[string]bool
	}{
		{name: "with a panic", panicking: map[string]bool{"second": true}},
		{name: "without a panic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			s, log := panicStopServer(t, mr, []string{"first", "second", "third"}, tc.panicking)
			serveBlockedRequest(t, s)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			err := s.StopBackground(ctx)
			assert.Equal(t, []string{"first", "second", "third", "queueDriver.Close"}, *log)
			require.ErrorIs(t, err, context.DeadlineExceeded, "the error of echo's Shutdown was dropped")
			if tc.panicking == nil {
				// panicが無いときは#3707より前と同じ値(包まないechoのエラー)を返す。
				assert.Equal(t, context.DeadlineExceeded, err)
				return
			}
			require.ErrorIs(t, err, errLifecycleStop)
			assert.ErrorContains(t, err, `part "second" panicked`)
		})
	}
}

// TestServer_FailedConstructionCleanupGoesOnAfterAPanickingStop checks that
// the rollback of a failed construction stops every part and closes the
// queue driver and the Redis subscriptions even when the stop of a part
// panics, without the panic leaving cleanupConstruction (#3707).
func TestServer_FailedConstructionCleanupGoesOnAfterAPanickingStop(t *testing.T) {
	logs := captureSlog(t)
	mr := miniredis.RunT(t)
	s, log := panicStopServer(t, mr, []string{"first", "second", "third"}, map[string]bool{"second": true})
	require.NotPanics(t, s.cleanupConstruction)
	assert.Equal(t, []string{"first", "second", "third", "queueDriver.Close"}, *log)
	assert.Nil(t, s.queueDriver)
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Zero(c, mr.PubSubNumPat(), "the Redis subscriptions were not closed after the panic")
	}, 5*time.Second, 20*time.Millisecond)
	// 記録はstopAllの1部品1行だけ(後始末で重ねて記録しない)。プロセス全体のloggerには
	// 並行するほかのテストの行も入るので、この変更の行とpanicの値を含む行だけを数える。
	assert.Equal(t, 1, strings.Count(logs.String(), stopPanicLogMsg))
	assert.Equal(t, 1, strings.Count(logs.String(), "second failed"), "the panic was logged more than once")
	assert.Contains(t, logs.String(), `"part":"second"`)
}

// TestSupervisor_StopGenerationGoesOnAfterAPanickingStop makes the stops of
// two parts of a generation the Supervisor built panic, after they did their
// work, and checks that the Supervisor stops every part in its order, reports
// the panics in StopReport.Err, closes the generation's DB connections and
// Redis subscriptions, and can build a new generation afterwards (#3707).
func TestSupervisor_StopGenerationGoesOnAfterAPanickingStop(t *testing.T) {
	captureSlog(t)
	env := newLifecycleEnv(t)
	deps := env.deps()
	var mu sync.Mutex
	var rec lifecycleRecorder
	var listed []string
	panicStop := true
	deps.NewServer = func(cfg *config.Config, db *gorm.DB, rc *cache.RedisClients) (*Server, error) {
		srv, err := newServer(cfg, db, rc, nil, noopStorage)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		if panicStop {
			listed = srv.lifecycle.names()
			// 部品の停止を済ませてからpanicさせる(途中まで止めてpanicする部品の代わり)。
			for i := range srv.lifecycle.parts {
				p := srv.lifecycle.parts[i]
				if p.name == "deliveryHealth" || p.name == "hashtagService" {
					srv.lifecycle.parts[i].stop = func(ctx context.Context) {
						p.stop(ctx)
						panic(p.name + " failed")
					}
				}
			}
			rec.wrap(srv)
		}
		return srv, nil
	}
	sup, err := NewSupervisor(env.cfg, env.redis, deps)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sup.Shutdown(context.Background()) })
	require.NoError(t, sup.gen.srv.StartBackground())

	rep := sup.stopGeneration(http.HandlerFunc(unavailable))
	require.ErrorIs(t, rep.Err, errLifecycleStop)
	assert.ErrorContains(t, rep.Err, `part "deliveryHealth" panicked: deliveryHealth failed`)
	assert.ErrorContains(t, rep.Err, `part "hashtagService" panicked: hashtagService failed`)
	mu.Lock()
	require.Contains(t, listed, "deliveryHealth", "前提: panicさせる部品が一覧にある")
	panicStop = false
	mu.Unlock()
	called, _ := rec.result()
	assert.Equal(t, listed, called, "the parts after the panicking ones were not stopped")
	env.requireNoBackends(t)
	env.requireNoPatterns(t, "the generation left its Redis subscriptions after the panic")

	// genMuを握ったまま抜けていないので、次の世代を作れる。
	require.NoError(t, sup.startGeneration())
	rep = sup.stopGeneration(http.HandlerFunc(unavailable))
	require.NoError(t, rep.Err)
}
