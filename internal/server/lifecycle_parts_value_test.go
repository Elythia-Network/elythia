package server

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/deliveryhealth"
	"github.com/elythia-network/elythia/internal/core/fedrule"
	corehashtag "github.com/elythia-network/elythia/internal/core/hashtag"
	coreinstance "github.com/elythia-network/elythia/internal/core/instance"
	"github.com/elythia-network/elythia/internal/pluginstore"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/paramshadow"
	"github.com/elythia-network/elythia/internal/stream"
)

// partCalls records the calls made on one fake part, in order.
type partCalls struct {
	srv   *Server
	calls []string
	// name and order, when set, record the start of this part into a list
	// shared by the parts of one test, so the test can see the start order.
	name  string
	order *[]string
}

type partCtxKey struct{}

// ctxTag names the ctx a method received: the value of partCtxKey, or
// "none" when it has none.
func ctxTag(ctx context.Context) string {
	if v, ok := ctx.Value(partCtxKey{}).(string); ok {
		return v
	}
	return "none"
}

// started records a start, with the number of parts already in the list, so
// a test can tell whether the part was started before every part was added.
func (c *partCalls) started(ctx string) {
	c.calls = append(c.calls, fmt.Sprintf("Start(%s) listed=%d", ctx, len(c.srv.lifecycle.parts)))
	if c.order != nil {
		*c.order = append(*c.order, c.name)
	}
}

func (c *partCalls) record(call string) { c.calls = append(c.calls, call) }

type fakeStartCloser struct{ *partCalls }

func (f fakeStartCloser) Start(ctx context.Context) { f.started(ctxTag(ctx)) }
func (f fakeStartCloser) Close()                    { f.record("Close()") }

type fakeStartStopper struct{ *partCalls }

func (f fakeStartStopper) Start(ctx context.Context) { f.started(ctxTag(ctx)) }
func (f fakeStartStopper) Stop(ctx context.Context)  { f.record("Stop(" + ctxTag(ctx) + ")") }

type fakePlainStartStopper struct{ *partCalls }

func (f fakePlainStartStopper) Start() { f.started("-") }
func (f fakePlainStartStopper) Stop()  { f.record("Stop()") }

type fakeCtxCloser struct{ *partCalls }

func (f fakeCtxCloser) Close(ctx context.Context) { f.record("Close(" + ctxTag(ctx) + ")") }

type fakeErrCloser struct{ *partCalls }

func (f fakeErrCloser) Close() error {
	f.record("Close() error")
	return errors.New("close failed")
}

// TestServer_RegistersAPartFromItsValue checks each registration that builds
// the start and the stop of a part from its value (#3701): it does not start
// the part when it adds it; startAll calls the start once, in list order and
// with the ctx the registration was given (#3703); it keeps the value itself
// as the part; and the stop calls that value's method, passing the shutdown
// ctx only to the methods that take one.
func TestServer_RegistersAPartFromItsValue(t *testing.T) {
	startCtx := context.WithValue(context.Background(), partCtxKey{}, "start")
	shutdownCtx := context.WithValue(context.Background(), partCtxKey{}, "shutdown")
	tests := []struct {
		name string
		// register adds two parts, "a" and "b", of the same type.
		register func(s *Server, a, b *partCalls) (any, any)
		// start is the call startAll makes, with %d for the number of parts
		// listed when it is made ("" when the part has nothing to start).
		start string
		stop  string
	}{
		{
			name: "Start(ctx) and Close()",
			register: func(s *Server, a, b *partCalls) (any, any) {
				pa, pb := fakeStartCloser{a}, fakeStartCloser{b}
				s.addStartCloser(startCtx, "a", pa)
				s.addStartCloser(startCtx, "b", pb)
				return pa, pb
			},
			start: "Start(start) listed=%d",
			stop:  "Close()",
		},
		{
			name: "Start(ctx) and Stop(ctx)",
			register: func(s *Server, a, b *partCalls) (any, any) {
				pa, pb := fakeStartStopper{a}, fakeStartStopper{b}
				s.addStartStopper(startCtx, "a", pa)
				s.addStartStopper(startCtx, "b", pb)
				return pa, pb
			},
			start: "Start(start) listed=%d",
			stop:  "Stop(shutdown)",
		},
		{
			name: "Start() and Stop()",
			register: func(s *Server, a, b *partCalls) (any, any) {
				pa, pb := fakePlainStartStopper{a}, fakePlainStartStopper{b}
				s.addPlainStartStopper("a", pa)
				s.addPlainStartStopper("b", pb)
				return pa, pb
			},
			start: "Start(-) listed=%d",
			stop:  "Stop()",
		},
		{
			name: "Close(ctx)",
			register: func(s *Server, a, b *partCalls) (any, any) {
				pa, pb := fakeCtxCloser{a}, fakeCtxCloser{b}
				s.addCloser("a", pa)
				s.addCloser("b", pb)
				return pa, pb
			},
			stop: "Close(shutdown)",
		},
		{
			name: "Close() error",
			register: func(s *Server, a, b *partCalls) (any, any) {
				pa, pb := fakeErrCloser{a}, fakeErrCloser{b}
				s.addErrCloser("a", pa)
				s.addErrCloser("b", pb)
				return pa, pb
			},
			stop: "Close() error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			var order []string
			a := &partCalls{srv: s, name: "a", order: &order}
			b := &partCalls{srv: s, name: "b", order: &order}
			pa, pb := tt.register(s, a, b)
			require.NoError(t, s.lifecycle.err)
			assert.Equal(t, []string{"a", "b"}, s.lifecycle.names())
			// 登録では起動しない(#3703)。
			assert.Empty(t, a.calls, "a was started when it was added")
			assert.Empty(t, b.calls, "b was started when it was added")

			// 起動はstartAllで1回だけ、一覧の順に呼ぶ。どちらも2つの登録の後。
			require.NoError(t, s.lifecycle.startAll())
			require.NoError(t, s.lifecycle.startAll())
			var wantA, wantB, wantOrder []string
			if tt.start != "" {
				wantA = []string{fmt.Sprintf(tt.start, 2)}
				wantB = []string{fmt.Sprintf(tt.start, 2)}
				wantOrder = []string{"a", "b"}
			}
			assert.Equal(t, wantA, a.calls, "startAll did not start a once")
			assert.Equal(t, wantB, b.calls, "startAll did not start b once")
			assert.Equal(t, wantOrder, order, "startAll did not start the parts in list order")

			va, _ := s.lifecycle.partValue("a")
			vb, _ := s.lifecycle.partValue("b")
			assert.Equal(t, pa, va, "part a does not hold the registered value")
			assert.Equal(t, pb, vb, "part b does not hold the registered value")
			for _, part := range s.lifecycle.parts {
				assert.Truef(t, part.fromValue, "part %q is not marked as added from its value", part.name)
			}

			// 止めると、登録したその値のメソッドが 1 回ずつ呼ばれる。
			require.NoError(t, s.lifecycle.stopAll(shutdownCtx))
			assert.Equal(t, append(wantA, tt.stop), a.calls, "stopping did not stop a itself")
			assert.Equal(t, append(wantB, tt.stop), b.calls, "stopping did not stop b itself")
			// Close() のエラーは捨てる。一覧の登録のエラーにもしない。
			assert.NoError(t, s.lifecycle.err)
		})
	}
}

// TestLifecycle_StartAllRecoversAPanickingStart checks that a start that
// panics makes startAll return an error naming the part, that the parts after
// it are not started (not even by a later startAll), and that stopAll still
// stops every part, the one that panicked and the ones never started
// included (#3703).
func TestLifecycle_StartAllRecoversAPanickingStart(t *testing.T) {
	s := &Server{}
	var order []string
	a := &partCalls{srv: s, name: "a", order: &order}
	b := &partCalls{srv: s, name: "b", order: &order}
	c := &partCalls{srv: s, name: "c", order: &order}
	s.addStartCloser(context.Background(), "a", fakeStartCloser{a})
	s.addStartCloser(context.Background(), "b", panickingStartCloser{fakeStartCloser{b}})
	s.addStartCloser(context.Background(), "c", fakeStartCloser{c})
	require.NoError(t, s.lifecycle.err)

	err := s.lifecycle.startAll()
	require.ErrorIs(t, err, errLifecycleStart)
	assert.ErrorContains(t, err, `part "b" panicked: start failed`)
	assert.Equal(t, []string{"a"}, order, "startAll started a part after the one that panicked")
	require.NoError(t, s.lifecycle.startAll())
	assert.Equal(t, []string{"a"}, order, "a second startAll started the parts again")

	require.NoError(t, s.lifecycle.stopAll(context.Background()))
	assert.Equal(t, []string{"Start(none) listed=3", "Close()"}, a.calls)
	assert.Equal(t, []string{"Close()"}, b.calls)
	assert.Equal(t, []string{"Close()"}, c.calls)
}

type panickingStartCloser struct{ fakeStartCloser }

func (panickingStartCloser) Start(context.Context) { panic("start failed") }

// TestLifecycle_StartAllStartsNothingAfterStopAll checks that the parts of a
// generation that has begun to stop are not started: a start after the stop
// would leave a goroutine nobody stops.
func TestLifecycle_StartAllStartsNothingAfterStopAll(t *testing.T) {
	s := &Server{}
	a := &partCalls{srv: s}
	s.addStartCloser(context.Background(), "a", fakeStartCloser{a})
	require.NoError(t, s.lifecycle.stopAll(context.Background()))
	require.NoError(t, s.lifecycle.startAll())
	assert.Equal(t, []string{"Close()"}, a.calls)
}

// TestServer_WorkerRunsOnlyOnceStarted checks the worker part of
// addWorker: adding it does not run it, startAll runs it once,
// and the stop of a worker that never started returns at once, even with a
// shutdown ctx that never expires (#3703).
func TestServer_WorkerRunsOnlyOnceStarted(t *testing.T) {
	t.Run("not started", func(t *testing.T) {
		s := &Server{}
		var runs atomic.Int32
		s.addWorker("w", func(context.Context) { runs.Add(1) })
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			assert.NoError(t, s.lifecycle.stopAll(context.Background()))
		}()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("the stop of a worker that never started waited for it")
		}
		require.NoError(t, s.lifecycle.startAll())
		assert.Zero(t, runs.Load(), "the worker ran without being started, or after its stop")
	})
	t.Run("started", func(t *testing.T) {
		s := &Server{}
		running := make(chan struct{})
		var runs atomic.Int32
		s.addWorker("w", func(ctx context.Context) {
			runs.Add(1)
			close(running)
			<-ctx.Done()
		})
		select {
		case <-running:
			t.Fatal("the worker ran when it was added")
		case <-time.After(50 * time.Millisecond):
		}
		require.NoError(t, s.lifecycle.startAll())
		require.NoError(t, s.lifecycle.startAll())
		<-running
		require.NoError(t, s.lifecycle.stopAll(context.Background()))
		assert.EqualValues(t, 1, runs.Load())
	})
}

// TestServer_AddPartDoesNotMarkThePartAsFromValue checks that a part added
// with a stop of its own (lifecycle.add, which the test helper addPart
// calls), even one that has a shape of its own, is not marked as added from
// its value and is not refused at run time. partsNotFromValue relies on the
// mark (#3717: the check is in TestNewServer_ShutdownStopsThePartsInTheirOrder).
func TestServer_AddPartDoesNotMarkThePartAsFromValue(t *testing.T) {
	s := &Server{}
	s.addPart("closure", fakeStartCloser{}, func(context.Context) {})
	require.NoError(t, s.lifecycle.err)
	require.Len(t, s.lifecycle.parts, 1)
	assert.False(t, s.lifecycle.parts[0].fromValue)
}

// valueShapes lists, for each registration that builds the stop of a part
// from its value, the interface its value parameter has and a match on it.
// TestValueShapes_CoverEveryRegistrationFromValue keeps it in step with
// lifecycle_parts.go.
var valueShapes = []struct {
	registration string
	iface        string
	matches      func(any) bool
}{
	{"addStartCloser", "startCloser", func(v any) bool { _, ok := v.(startCloser); return ok }},
	{"addStartStopper", "startStopper", func(v any) bool { _, ok := v.(startStopper); return ok }},
	{"addPlainStartStopper", "plainStartStopper", func(v any) bool { _, ok := v.(plainStartStopper); return ok }},
	{"addCloser", "ctxCloser", func(v any) bool { _, ok := v.(ctxCloser); return ok }},
	{"addErrCloser", "errCloser", func(v any) bool { _, ok := v.(errCloser); return ok }},
}

// valueShape returns the registration that builds the start and the stop of
// part from part itself, or "" when part has none of those shapes.
func valueShape(part any) string {
	for _, sh := range valueShapes {
		if sh.matches(part) {
			return sh.registration
		}
	}
	return ""
}

// TestValueShapes_CoverEveryRegistrationFromValue reads every non-test file of
// the package and lists every function that adds a part through
// addFromValue, with the interface type of the parameter it passes as the
// value. valueShapes has to list exactly those, so a new shape added to the
// registrations cannot be missed by valueShape, which partsNotFromValue uses
// to name the registration a part can move to.
//
// 構文木で読むのは、形を判定する関数が interface の一覧を実行時に取れないため。
// 本体の登録を足して valueShapes に足し忘れると、valueShape がその形を知らないままになる。
// lifecycle_parts.go の Server のメソッドだけを読んでいたので、別のファイルの
// メソッドと、型パラメータを持つ自由関数で書いた登録を拾えなかった (#3369 I11)。
func TestValueShapes_CoverEveryRegistrationFromValue(t *testing.T) {
	got := registrationsFromValue(t, packageSources(t))
	// 何も拾えなかったら落とす (書式が変わって抽出が空振りしたとき)。
	require.NotEmpty(t, got, "found no registration that calls addFromValue in internal/server")
	want := map[string]string{}
	for _, sh := range valueShapes {
		want[sh.registration] = sh.iface
	}
	assert.Equal(t, want, got, "valueShapes does not list the registrations that call addFromValue")
}

// registrationsFromValue returns, for each function in files that calls
// addFromValue, the type name of the parameter it passes as the value. A
// type parameter is named by its constraint.
func registrationsFromValue(t *testing.T, files []packageSource) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	got := map[string]string{}
	for _, file := range files {
		f, err := parser.ParseFile(fset, file.name, file.src, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			constraints := map[string]string{}
			if fn.Type.TypeParams != nil {
				for _, field := range fn.Type.TypeParams.List {
					ident, ok := field.Type.(*ast.Ident)
					if !ok {
						continue
					}
					for _, n := range field.Names {
						constraints[n.Name] = ident.Name
					}
				}
			}
			params := map[string]string{}
			for _, field := range fn.Type.Params.List {
				ident, ok := field.Type.(*ast.Ident)
				if !ok {
					continue
				}
				typ := ident.Name
				if c, ok := constraints[typ]; ok {
					typ = c
				}
				for _, n := range field.Names {
					params[n.Name] = typ
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "addFromValue" || len(call.Args) != 4 {
					return true
				}
				arg, ok := call.Args[1].(*ast.Ident)
				if !assert.Truef(t, ok, "%s (%s) passes a value to addFromValue that is not a parameter", fn.Name.Name, file.name) {
					return true
				}
				got[fn.Name.Name] = params[arg.Name]
				return true
			})
		}
	}
	return got
}

// 別のファイルのメソッドと、型パラメータを持つ自由関数の登録も拾うこと (#3369 I11)。
func TestRegistrationsFromValue_ReadsEveryFileAndFreeFunction(t *testing.T) {
	got := registrationsFromValue(t, []packageSource{
		{name: "lifecycle_parts.go", src: "package server\n\nfunc (s *Server) addA(name string, p aShape) { s.lifecycle.addFromValue(name, p, nil, nil) }\n"},
		{name: "other.go", src: "package server\n\nfunc (s *Server) addB(name string, p bShape) { s.lifecycle.addFromValue(name, p, nil, nil) }\n"},
		{name: "free.go", src: "package server\n\nfunc addC[T cShape](l *lifecycle, name string, p T) { l.addFromValue(name, p, nil, nil) }\n"},
	})
	assert.Equal(t, map[string]string{"addA": "aShape", "addB": "bShape", "addC": "cShape"}, got)
}

// TestValueShape_ClassifiesThePartTypes pins valueShape on fakes of the five
// shapes and on the types of the parts newServer adds, listed by name, so a
// part type that gains or loses a method shows up here.
func TestValueShape_ClassifiesThePartTypes(t *testing.T) {
	tests := []struct {
		name string
		part any
		// use is the registration valueShape names, or "" for none.
		use string
	}{
		{name: "fake Start(ctx) and Close()", part: fakeStartCloser{}, use: "addStartCloser"},
		{name: "fake Start(ctx) and Stop(ctx)", part: fakeStartStopper{}, use: "addStartStopper"},
		{name: "fake Start() and Stop()", part: fakePlainStartStopper{}, use: "addPlainStartStopper"},
		{name: "fake Close(ctx)", part: fakeCtxCloser{}, use: "addCloser"},
		{name: "fake Close() error", part: fakeErrCloser{}, use: "addErrCloser"},

		// 値から作る登録で足す13個の部品の型(12。deliveryHealthとinboxHealthは同じ型)。
		// pluginstore.Store だけは setupRoutes ではなく dbBackedStorage が登録する。
		// 最後の3つは#3717で停止の口を足し、closureの登録から移した。
		{name: "TouchBuffer", part: (*coreinstance.TouchBuffer)(nil), use: "addStartCloser"},
		{name: "CapabilityBuffer", part: (*coreinstance.CapabilityBuffer)(nil), use: "addStartCloser"},
		{name: "CounterBuffer", part: (*coreinstance.CounterBuffer)(nil), use: "addStartCloser"},
		{name: "deliveryhealth.Service", part: (*deliveryhealth.Service)(nil), use: "addStartStopper"},
		{name: "EmojiInvalidationQueue", part: (*repository.EmojiInvalidationQueue)(nil), use: "addStartStopper"},
		{name: "ServerStatsPublisher", part: (*stream.ServerStatsPublisher)(nil), use: "addPlainStartStopper"},
		{name: "QueueStatsPublisher", part: (*stream.QueueStatsPublisher)(nil), use: "addPlainStartStopper"},
		{name: "MetadataRefresher", part: (*coreinstance.MetadataRefresher)(nil), use: "addCloser"},
		{name: "pluginstore.Store", part: (*pluginstore.Store)(nil), use: "addErrCloser"},
		{name: "fedrule.HitStore", part: (*fedrule.HitStore)(nil), use: "addStartStopper"},
		{name: "hashtag.Service", part: (*corehashtag.Service)(nil), use: "addCloser"},
		{name: "paramshadow.Recorder", part: (*paramshadow.Recorder)(nil), use: "addCloser"},

		// 値を持たない worker。
		{name: "no value", part: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.use, valueShape(tt.part))
		})
	}
}
