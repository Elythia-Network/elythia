package server

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	corehashtag "github.com/elythia-network/elythia/internal/core/hashtag"
	coreinstance "github.com/elythia-network/elythia/internal/core/instance"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/pluginstore"
	"github.com/elythia-network/elythia/plugin"
)

func TestLifecycle_AddRejectsInvalidParts(t *testing.T) {
	stop := func(context.Context) {}
	tests := []struct {
		name    string
		add     func(l *lifecycle)
		wantErr string
		want    []string
	}{
		{
			name: "valid parts are kept in order",
			add: func(l *lifecycle) {
				l.add("b", nil, stop)
				l.add("a", nil, stop)
			},
			want: []string{"b", "a"},
		},
		{
			name:    "empty name",
			add:     func(l *lifecycle) { l.add("", nil, stop) },
			wantErr: "a part has no name",
			// 名前が無くても止めはする (構築の後始末で止めるため)。
			want: []string{""},
		},
		{
			name:    "nil stop",
			add:     func(l *lifecycle) { l.add("a", nil, nil) },
			wantErr: `part "a" has no stop`,
			want:    []string{},
		},
		{
			name: "duplicate name",
			add: func(l *lifecycle) {
				l.add("a", nil, stop)
				l.add("b", nil, stop)
				l.add("a", nil, stop)
			},
			wantErr: `part "a" is added twice`,
			// 重複した部品も一覧に入れ、後始末で止める。
			want: []string{"a", "b", "a"},
		},
		{
			name: "the first error is kept",
			add: func(l *lifecycle) {
				l.add("a", nil, stop)
				l.add("a", nil, stop)
				l.add("", nil, stop)
			},
			wantErr: `part "a" is added twice`,
			want:    []string{"a", "a", ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var l lifecycle
			tt.add(&l)
			assert.Equal(t, tt.want, l.names())
			if tt.wantErr == "" {
				assert.NoError(t, l.err)
				return
			}
			require.ErrorIs(t, l.err, errLifecyclePart)
			assert.ErrorContains(t, l.err, tt.wantErr)
		})
	}
}

func TestLifecycle_StopAllRunsEveryPartInOrderWithTheCtx(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "shutdown")
	var got []string
	var l lifecycle
	for _, name := range []string{"first", "second", "third"} {
		l.add(name, nil, func(c context.Context) {
			assert.Equal(t, "shutdown", c.Value(key{}), "part %s did not get the shutdown ctx", name)
			got = append(got, name)
		})
	}
	l.stopAll(ctx)
	assert.Equal(t, []string{"first", "second", "third"}, got)
}

// lifecycleRecorder replaces the stop of every part of s with one that
// records the part's name, checks it received ctxKey, and then calls the
// original stop.
type lifecycleRecorder struct {
	mu     sync.Mutex
	called []string
	noCtx  []string
}

type lifecycleCtxKey struct{}

func (r *lifecycleRecorder) wrap(s *Server) {
	for i := range s.lifecycle.parts {
		p := s.lifecycle.parts[i]
		s.lifecycle.parts[i].stop = func(ctx context.Context) {
			r.mu.Lock()
			r.called = append(r.called, p.name)
			if ctx.Value(lifecycleCtxKey{}) == nil {
				r.noCtx = append(r.noCtx, p.name)
			}
			r.mu.Unlock()
			p.stop(ctx)
		}
	}
}

func (r *lifecycleRecorder) result() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.called...), append([]string(nil), r.noCtx...)
}

// lifecyclePartTypes is the dynamic type of the part value of each name
// (pluginStorage:<name> is looked up as pluginStorage:).
//
// この表は、名前が別の種類の値を持つ取り違え (serverStatsPub に hashtagService を渡すなど) を
// 落とす。**同じ型の部品どうしの名前の取り違えは落とせない** (deliveryHealth と inboxHealth は
// どちらも *deliveryhealth.Service、ほかに 4 つの worker はどれも nil)。値から作る登録
// (startPartWithClose など、#3701) では part と停止は同じ値を指すが、addPart で登録する部品では、
// 部品の値と停止の関数の対応は型でも実行時でも保証していない。
var lifecyclePartTypes = map[string]string{
	"pollExpiryWorker":            "<nil>",
	"fedRuleHits":                 "*fedrule.HitStore",
	"metadataRefresher":           "*instance.MetadataRefresher",
	"instanceTouchBuffer":         "*instance.TouchBuffer",
	"sigCapBuffer":                "*instance.CapabilityBuffer",
	"reactionFlushWorker":         "<nil>",
	"instanceCounterBuffer":       "*instance.CounterBuffer",
	"deliveryHealth":              "*deliveryhealth.Service",
	"inboxHealth":                 "*deliveryhealth.Service",
	"hashtagService":              "*hashtag.Service",
	"retentionAggregateAtStartup": "<nil>",
	"paramShadowRecorder":         "*paramshadow.Recorder",
	"emojiInvalidationQueue":      "*repository.EmojiInvalidationQueue",
	"userCacheLink":               "<nil>",
	"serverStatsPub":              "*stream.ServerStatsPublisher",
	"queueStatsPub":               "*stream.QueueStatsPublisher",
	"pluginStorage:":              "*pluginstore.Store",
}

// assertPartTypes checks the dynamic type of every part value of s against
// lifecyclePartTypes.
func assertPartTypes(t *testing.T, s *Server) {
	t.Helper()
	for _, p := range s.lifecycle.parts {
		key := p.name
		if strings.HasPrefix(key, "pluginStorage:") {
			key = "pluginStorage:"
		}
		want, ok := lifecyclePartTypes[key]
		if !assert.Truef(t, ok, "part %q has no entry in lifecyclePartTypes", p.name) {
			continue
		}
		assert.Equalf(t, want, fmt.Sprintf("%T", p.part), "part %q holds a value of another type", p.name)
	}
}

// addPartShapeAllowlist lists the parts that newServer may add by addPart
// although their value has a shape that a registration from the value takes
// (valueShape). The value is the reason the part keeps its closure stop.
//
// 今は空。closure のまま残した 8 つ (docs/architecture.md 4.3) は、どれも形を満たさない。
// 足すときは、なぜ値から作る登録にしないか (closure の停止が何をするか) を書く。
var addPartShapeAllowlist = map[string]string{}

// addPartsInspected names the parts added by addPart with a value that
// addPartShapeViolations has to see in at least one combination. It is the lower
// bound of the check: with an empty allowlist, a check that inspects nothing
// would pass as well.
var addPartsInspected = []string{"fedRuleHits", "hashtagService", "emojiInvalidationQueue", "paramShadowRecorder"}

// addPartShapeViolations returns a message for each of parts that was added
// by addPart although its value has a shape valueShape knows and its name is
// not in allowlist. It records the parts it inspected (added by addPart with
// a value) and the allowlist entries it used.
func addPartShapeViolations(parts []lifecyclePart, allowlist map[string]string, inspected, used map[string]bool) []string {
	var out []string
	for _, p := range parts {
		if p.fromValue || p.part == nil {
			continue
		}
		inspected[p.name] = true
		use := valueShape(p.part)
		if use == "" {
			continue
		}
		if _, ok := allowlist[p.name]; ok {
			used[p.name] = true
			continue
		}
		out = append(out, fmt.Sprintf("part %q (%T) is added by addPart, but its value can be added by %s", p.name, p.part, use))
	}
	return out
}

// sharedPartValues returns a message for each part of parts that holds the
// same value as an earlier part, and for each part whose value is not a
// pointer.
//
// 値から作る登録では、値が起動と停止を決める。同じ値を 2 回渡すと、もう片方の部品は
// 起動も停止もされない (#3701 のレビュー L-1)。部品の値は同一性で比べるので、ポインタに限る。
// **ポインタでない値を持つ部品は、それだけでここに出る。** 値の型 (struct のまま渡すもの) の
// 部品を足すときは、比べ方から見直す。
func sharedPartValues(parts []lifecyclePart) []string {
	var out []string
	seen := map[any]string{}
	for _, p := range parts {
		if p.part == nil {
			continue
		}
		if reflect.ValueOf(p.part).Kind() != reflect.Pointer {
			out = append(out, fmt.Sprintf("part %q holds a %T, not a pointer", p.name, p.part))
			continue
		}
		if prev, ok := seen[p.part]; ok {
			out = append(out, fmt.Sprintf("parts %q and %q hold the same value (%T)", prev, p.name, p.part))
			continue
		}
		seen[p.part] = p.name
	}
	return out
}

// TestPartChecks_ReportViolations is the positive control of
// addPartShapeViolations and sharedPartValues: each reports a part list built
// by hand that breaks its rule, and stays quiet on one that keeps it.
func TestPartChecks_ReportViolations(t *testing.T) {
	stop := func(context.Context) {}
	shaped := fakeStartCloser{&partCalls{}}
	t.Run("a part with a shape added by addPart", func(t *testing.T) {
		s := &Server{}
		s.addPart("closure", shaped, stop)
		s.addPart("noShape", &struct{ n int }{}, stop)
		s.addPart("worker", nil, stop)
		s.startPartWithClose(context.Background(), "fromValue", fakeStartCloser{&partCalls{srv: s}})
		inspected, used := map[string]bool{}, map[string]bool{}
		got := addPartShapeViolations(s.lifecycle.parts, nil, inspected, used)
		assert.Equal(t, []string{`part "closure" (server.fakeStartCloser) is added by addPart, but its value can be added by startPartWithClose`}, got)
		assert.Equal(t, map[string]bool{"closure": true, "noShape": true}, inspected)
		assert.Empty(t, used)

		// 許可リストに載せると報告せず、使ったことを記録する。
		got = addPartShapeViolations(s.lifecycle.parts, map[string]string{"closure": "test"}, inspected, used)
		assert.Empty(t, got)
		assert.Equal(t, map[string]bool{"closure": true}, used)
	})
	t.Run("two parts with the same pointer", func(t *testing.T) {
		v := &struct{ n int }{}
		var l lifecycle
		l.add("a", v, stop)
		l.add("b", &struct{ n int }{}, stop)
		l.add("c", v, stop)
		l.add("worker", nil, stop)
		l.add("other", nil, stop)
		assert.Equal(t, []string{`parts "a" and "c" hold the same value (*struct { n int })`}, sharedPartValues(l.parts))
	})
	t.Run("a part whose value is not a pointer", func(t *testing.T) {
		var l lifecycle
		l.add("value", struct{ n int }{}, stop)
		assert.Equal(t, []string{`part "value" holds a struct { n int }, not a pointer`}, sharedPartValues(l.parts))
	})
}

// TestNewServer_ShutdownStopsThePartsInTheirOrder pins the stop order of the
// named parts (#3699) for each combination of the process role and the
// settings that change what setupRoutes registers, on a server built by
// newServer. It also checks that StopBackground calls every listed part, in
// that order and with its ctx, and the type of each part value
// (lifecyclePartTypes). It also checks that no two parts hold the same value
// and that no part with a shape of its own comes through addPart
// (sharedPartValues and addPartShapeViolations, #3701).
//
// 並びは #3699 の前の登録の順 (無名の hook の順) と、組ごとに突き合わせて同じで
// あることを確かめてある。部品を足す・動かすときは、この表と
// docs/architecture.md 4.3 の一覧を一緒に直す。
func TestNewServer_ShutdownStopsThePartsInTheirOrder(t *testing.T) {
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	tests := []struct {
		name        string
		role        config.ProcessRole
		noInspector bool
		noShadow    bool
		plugins     []string
		want        []string
	}{
		{
			name: "both roles",
			role: config.RoleBoth,
			want: []string{
				"pollExpiryWorker", "fedRuleHits", "metadataRefresher", "instanceTouchBuffer",
				"sigCapBuffer", "reactionFlushWorker", "instanceCounterBuffer", "deliveryHealth",
				"inboxHealth", "hashtagService", "retentionAggregateAtStartup", "paramShadowRecorder",
				"emojiInvalidationQueue", "userCacheLink", "serverStatsPub", "queueStatsPub",
			},
		},
		{
			name: "server role",
			role: config.RoleServer,
			want: []string{
				"fedRuleHits", "metadataRefresher", "instanceTouchBuffer",
				"sigCapBuffer", "reactionFlushWorker", "instanceCounterBuffer",
				"hashtagService", "paramShadowRecorder",
				"emojiInvalidationQueue", "userCacheLink", "serverStatsPub", "queueStatsPub",
			},
		},
		{
			name: "queue role",
			role: config.RoleQueue,
			want: []string{
				"pollExpiryWorker", "fedRuleHits", "metadataRefresher", "instanceTouchBuffer",
				"sigCapBuffer", "reactionFlushWorker", "instanceCounterBuffer", "deliveryHealth",
				"inboxHealth", "hashtagService", "retentionAggregateAtStartup", "paramShadowRecorder",
				"emojiInvalidationQueue", "userCacheLink",
			},
		},
		{
			name:        "server role without a queue inspector",
			role:        config.RoleServer,
			noInspector: true,
			want: []string{
				"fedRuleHits", "metadataRefresher", "instanceTouchBuffer",
				"sigCapBuffer", "reactionFlushWorker", "instanceCounterBuffer",
				"hashtagService", "paramShadowRecorder",
				"emojiInvalidationQueue", "userCacheLink", "serverStatsPub",
			},
		},
		{
			name:     "both roles with param shadow disabled",
			role:     config.RoleBoth,
			noShadow: true,
			want: []string{
				"pollExpiryWorker", "fedRuleHits", "metadataRefresher", "instanceTouchBuffer",
				"sigCapBuffer", "reactionFlushWorker", "instanceCounterBuffer", "deliveryHealth",
				"inboxHealth", "hashtagService", "retentionAggregateAtStartup",
				"emojiInvalidationQueue", "userCacheLink", "serverStatsPub", "queueStatsPub",
			},
		},
		{
			name:    "both roles with plugin storage",
			role:    config.RoleBoth,
			plugins: []string{"lifecyclea", "lifecycleb"},
			want: []string{
				"pollExpiryWorker", "fedRuleHits", "metadataRefresher", "instanceTouchBuffer",
				"sigCapBuffer", "reactionFlushWorker", "instanceCounterBuffer", "deliveryHealth",
				"inboxHealth", "hashtagService", "retentionAggregateAtStartup", "paramShadowRecorder",
				"emojiInvalidationQueue", "userCacheLink", "serverStatsPub", "queueStatsPub",
				"pluginStorage:lifecyclea", "pluginStorage:lifecycleb",
			},
		},
		{
			name:     "queue role with plugin storage and param shadow disabled",
			role:     config.RoleQueue,
			noShadow: true,
			plugins:  []string{"lifecyclea"},
			want: []string{
				"pollExpiryWorker", "fedRuleHits", "metadataRefresher", "instanceTouchBuffer",
				"sigCapBuffer", "reactionFlushWorker", "instanceCounterBuffer", "deliveryHealth",
				"inboxHealth", "hashtagService", "retentionAggregateAtStartup",
				"emojiInvalidationQueue", "userCacheLink", "pluginStorage:lifecyclea",
			},
		},
	}
	// 型の表の行が、どの組の並びにも出てこなければ落とす。部品を消したり名前を
	// 変えたりしたときに、表の古い行が残って緑のままになるのを防ぐ。
	seen := map[string]bool{}
	for _, tt := range tests {
		for _, name := range tt.want {
			if strings.HasPrefix(name, "pluginStorage:") {
				name = "pluginStorage:"
			}
			seen[name] = true
		}
	}
	for name := range lifecyclePartTypes {
		assert.Truef(t, seen[name], "lifecyclePartTypes has %q, which no combination lists", name)
	}
	inspected := map[string]bool{}
	used := map[string]bool{}
	fromValue := map[string]bool{}
	// 始まった組の数。-run で組を絞ったときは、下の組をまたぐ検査をしない。
	// 数えるのは subtest の先頭で行う。途中で return や Skip をした組も数え、
	// 組をまたぐ検査が黙って省かれないようにする (#3701 のレビュー 3 周目)。
	ran := 0
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ran++
			db, clients, _, cfg := constructionFixture(t)
			restoreProcessGlobals(t)
			switch tt.role {
			case config.RoleServer:
				t.Setenv(config.EnvOnlyServer, "1")
			case config.RoleQueue:
				t.Setenv(config.EnvOnlyQueue, "1")
			}
			cfg.DisableParamShadow = tt.noShadow
			var defs []plugin.Definition
			openStorage := noopStorage
			if len(tt.plugins) > 0 {
				// 本番と同じ dbBackedStorage を通す (nil を渡すと newServer が選ぶ)。
				cfg.DB = testDBConfig(t).DB
				openStorage = nil
				for _, name := range tt.plugins {
					dropPluginSchema(t, cfg, name)
					t.Cleanup(func() { dropPluginSchema(t, cfg, name) })
					defs = append(defs, plugin.Definition{
						Name: name, APIVersion: plugin.APIVersion,
						Routes: func(plugin.Context, plugin.Router) error { return nil },
					})
				}
			}
			srv, err := newServer(cfg, db, clients, defs, openStorage, func(s *Server) {
				if tt.noInspector {
					s.queueInspector = nil
				}
			})
			require.NoError(t, err)
			require.Equal(t, tt.role, srv.role)
			assert.Equal(t, tt.want, srv.lifecycle.names())
			assertPartTypes(t, srv)
			for _, v := range sharedPartValues(srv.lifecycle.parts) {
				t.Error(v)
			}
			for _, v := range addPartShapeViolations(srv.lifecycle.parts, addPartShapeAllowlist, inspected, used) {
				t.Error(v)
			}
			for _, p := range srv.lifecycle.parts {
				if p.fromValue {
					fromValue[p.name] = true
				}
			}

			var rec lifecycleRecorder
			rec.wrap(srv)
			// 構築では一覧を起動せず、StartBackgroundが一覧の順に1回ずつ起動する(#3703)。
			// 2回呼んでも2回目は何も起動しない。起動の差し替えは構築の後なので、構築の
			// 途中の起動は一覧の印(started)で見る。一覧を通らない起動は
			// TestNewServer_ConstructionStartsNoPartが見る。
			assert.False(t, srv.lifecycle.started, "construction started the parts of the list")
			started := wrapStarts(srv)
			var wantStarted []string
			for _, name := range tt.want {
				if partsStartedByStartBackground[name] {
					wantStarted = append(wantStarted, name)
				}
			}
			require.NoError(t, srv.StartBackground())
			require.NoError(t, srv.StartBackground())
			assert.Equal(t, wantStarted, started(), "StartBackground did not start the parts with a start once, in list order")
			ctx := context.WithValue(context.Background(), lifecycleCtxKey{}, true)
			require.NoError(t, srv.StopBackground(ctx))
			called, noCtx := rec.result()
			assert.Equal(t, tt.want, called, "StopBackground did not stop the listed parts in order")
			assert.Empty(t, noCtx, "these parts did not get the shutdown ctx")
		})
	}
	if ran != len(tests) {
		t.Logf("%d of %d combinations ran; skipping the checks across all of them", ran, len(tests))
		return
	}
	// 許可リストの使われていない項目を落とす。
	for name := range addPartShapeAllowlist {
		assert.Truef(t, used[name], "addPartShapeAllowlist has %q, which no combination adds by addPart with a shape", name)
	}
	// 検査が部品を見たことの下限。許可リストが空のときは上の検査が空振りするので、
	// closure の部品を名指しで要求し、値から作る印が実際に付いていることも見る。
	for _, name := range addPartsInspected {
		assert.Truef(t, inspected[name], "addPartShapeViolations did not inspect %q", name)
	}
	for _, name := range []string{
		"metadataRefresher", "instanceTouchBuffer", "sigCapBuffer", "instanceCounterBuffer",
		"deliveryHealth", "inboxHealth", "serverStatsPub", "queueStatsPub", "pluginStorage:lifecyclea",
	} {
		assert.Truef(t, fromValue[name], "part %q is not marked as added from its value in any combination", name)
	}
}

// TestNewServer_FailedConstructionStopsTheRegisteredParts checks that the
// rollback of a construction that fails (here in the plugin setup, after
// every built-in part has been added) stops the parts added so far, in their
// order.
func TestNewServer_FailedConstructionStopsTheRegisteredParts(t *testing.T) {
	db, clients, _, cfg := constructionFixture(t)
	restoreProcessGlobals(t)
	var built *Server
	var rec lifecycleRecorder
	var listed []string
	fail := failingRoutesWith(func() {
		listed = built.lifecycle.names()
		rec.wrap(built)
	})
	// 後始末はcontext.Background()で止めるので、起動していないworkerの停止がdoneを
	// 待つと返らない(#3703)。上限を置き、超えたら落とす。
	errc := make(chan error, 1)
	go func() {
		_, err := newServer(cfg, db, clients, []plugin.Definition{fail}, noopStorage, func(s *Server) { built = s })
		errc <- err
	}()
	var err error
	select {
	case err = <-errc:
	case <-time.After(30 * time.Second):
		t.Fatal("the rollback of a failed construction did not return: a stop waits for a part that never started")
	}
	require.Error(t, err)
	require.Contains(t, listed, "userCacheLink", "前提: 部品を登録してから失敗している")
	called, noCtx := rec.result()
	assert.Equal(t, listed, called, "the rollback did not stop the listed parts in order")
	// 後始末は context.Background() で止めるので、目印の値は無い。
	assert.Equal(t, listed, noCtx)
}

// TestNewServer_DuplicatePartFailsTheConstruction checks that a name added
// twice fails newServer, and that the rollback still stops both parts.
func TestNewServer_DuplicatePartFailsTheConstruction(t *testing.T) {
	db, clients, _, cfg := constructionFixture(t)
	restoreProcessGlobals(t)
	stopped := 0
	_, err := newServer(cfg, db, clients, nil, noopStorage, func(s *Server) {
		s.addPart("userCacheLink", nil, func(context.Context) { stopped++ })
	})
	require.ErrorIs(t, err, errLifecyclePart)
	assert.ErrorContains(t, err, `part "userCacheLink" is added twice`)
	assert.Equal(t, 1, stopped, "the rollback did not stop the part added first")
}

// partValue returns the value of the part named name, and whether such a
// part was added.
func (l *lifecycle) partValue(name string) (any, bool) {
	for _, p := range l.parts {
		if p.name == name {
			return p.part, true
		}
	}
	return nil, false
}

// partOf returns the value of the part named name of s as a T.
func partOf[T any](t *testing.T, s *Server, name string) T {
	t.Helper()
	v, ok := s.lifecycle.partValue(name)
	require.Truef(t, ok, "part %q is not in the list", name)
	typed, ok := v.(T)
	require.Truef(t, ok, "part %q holds %T", name, v)
	return typed
}

func TestLifecycle_AddKeepsThePartValue(t *testing.T) {
	var l lifecycle
	buf := &struct{ n int }{}
	l.add("buffer", buf, func(context.Context) {})
	l.add("worker", nil, func(context.Context) {})
	v, ok := l.partValue("buffer")
	require.True(t, ok)
	assert.Same(t, buf, v)
	v, ok = l.partValue("worker")
	require.True(t, ok)
	assert.Nil(t, v)
	_, ok = l.partValue("missing")
	assert.False(t, ok)
}

// TestNewServer_ShutdownStopsTheNamedParts checks, on a server built by
// newServer, that stopping the generation stops the value each name stands
// for, not only that a stop runs under that name (#3699 review L-1).
//
// 観測するのは、閉じた後の振る舞いが外から見える部品だけ。
//   - バッファ 3 つ: 閉じた後の書き込みは、溜めずにその場で DB へ書く。閉じていなければ
//     flush の間隔 (1 秒 / 60 秒 / 30 秒) まで書かない
//   - hashtagService: 書き込み中の worker を止めておくと、Shutdown(ctx) が期限まで待ち、
//     警告を出す。呼ばなければ警告は出ない
//   - プラグインの store: 閉じた後は DB のプールに Ping できない
func TestNewServer_ShutdownStopsTheNamedParts(t *testing.T) {
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	db, clients, _, cfg := constructionFixture(t)
	restoreProcessGlobals(t)
	const pluginName = "lifecyclestop"
	cfg.DB = testDBConfig(t).DB
	dropPluginSchema(t, cfg, pluginName)
	t.Cleanup(func() { dropPluginSchema(t, cfg, pluginName) })
	defs := []plugin.Definition{{
		Name: pluginName, APIVersion: plugin.APIVersion,
		Routes: func(plugin.Context, plugin.Router) error { return nil },
	}}
	srv, err := newServer(cfg, db, clients, defs, nil)
	require.NoError(t, err)
	// 本番と同じく起動してから止める(#3703)。起動しないと、バッファは起動していない側の
	// Close()しか通らず、起動した部品の止め方を見なくなる。
	require.NoError(t, srv.StartBackground())

	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	host := idGen.Generate(time.Now()) + ".lifecycle.example"
	tag := "lifecycle" + idGen.Generate(time.Now())
	require.NoError(t, db.Create(&model.Instance{
		ID: idGen.Generate(time.Now()), Host: host, FirstRetrievedAt: time.Now(),
	}).Error)
	t.Cleanup(func() {
		db.Where("host = ?", host).Delete(&model.Instance{})
		db.Where("host = ?", host).Delete(&model.InstanceSignatureCapability{})
		db.Exec(`DELETE FROM hashtag WHERE name = ?`, tag)
	})

	touch := partOf[*coreinstance.TouchBuffer](t, srv, "instanceTouchBuffer")
	sigCap := partOf[*coreinstance.CapabilityBuffer](t, srv, "sigCapBuffer")
	counter := partOf[*coreinstance.CounterBuffer](t, srv, "instanceCounterBuffer")
	hashtags := partOf[*corehashtag.Service](t, srv, "hashtagService")
	store := partOf[*pluginstore.Store](t, srv, "pluginStorage:"+pluginName)

	// hashtag の worker を、表の lock で書き込みの途中に止めておく。
	lock := db.Begin()
	require.NoError(t, lock.Error)
	require.NoError(t, lock.Exec(`LOCK TABLE hashtag IN ACCESS EXCLUSIVE MODE`).Error)
	released := false
	release := func() {
		if !released {
			released = true
			lock.Rollback()
			hashtags.WaitForPendingWrites()
		}
	}
	t.Cleanup(release)
	hashtags.OnNoteCreated(
		&model.Note{ID: idGen.Generate(time.Now()), Tags: model.StringArray{tag}, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: idGen.Generate(time.Now())},
	)

	logs := captureDefaultLog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	// 停止が ctx を無視する退行があると、hashtag の worker が lock で止まったまま
	// StopBackground が返らず、テストが go test の上限 (10 分) まで止まる。上限を
	// 置き、超えたら lock を外してから落とす。ctx は 1.5 秒で切れるので、-race の
	// CI でも 10 秒あれば返る (手元の -race で約 1.8 秒)。
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = srv.StopBackground(ctx)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		release()
		t.Fatal("StopBackground did not return after its ctx expired: a part's stop ignores the shutdown ctx")
	}
	release()
	assert.Contains(t, logs.String(), "hashtag service shutdown timed out",
		"the stop of hashtagService did not wait for its in-flight write")

	// 閉じたバッファは、その場で書く。
	require.NoError(t, touch.MarkRequestReceived(host))
	sigCap.ObserveInboundAlg(host, model.SignatureAlgEd25519)
	counter.Add(host, coreinstance.CounterNotesCount, 1)
	var inst model.Instance
	require.NoError(t, db.Where("host = ?", host).First(&inst).Error)
	assert.NotNil(t, inst.LatestRequestReceivedAt, "instanceTouchBuffer was not closed")
	assert.Equal(t, 1, inst.NotesCount, "instanceCounterBuffer was not closed")
	var caps int64
	require.NoError(t, db.Model(&model.InstanceSignatureCapability{}).Where("host = ?", host).Count(&caps).Error)
	assert.EqualValues(t, 1, caps, "sigCapBuffer was not closed")

	assert.Error(t, store.DB().PingContext(context.Background()), "the plugin storage was not closed")
}
