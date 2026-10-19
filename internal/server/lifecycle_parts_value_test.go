package server

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

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
// a test can tell whether the part was started before it was added.
func (c *partCalls) started(ctx string) {
	c.calls = append(c.calls, fmt.Sprintf("Start(%s) listed=%d", ctx, len(c.srv.lifecycle.parts)))
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
// the start and the stop of a part from its value (#3701): it calls the start
// once, right away and before the part is added, with the ctx it was given;
// it keeps the value itself as the part; and the stop calls that value's
// method, passing the shutdown ctx only to the methods that take one.
func TestServer_RegistersAPartFromItsValue(t *testing.T) {
	startCtx := context.WithValue(context.Background(), partCtxKey{}, "start")
	shutdownCtx := context.WithValue(context.Background(), partCtxKey{}, "shutdown")
	tests := []struct {
		name string
		// register adds two parts, "a" and "b", of the same type.
		register func(s *Server, a, b *partCalls) (any, any)
		// start is the call the registration makes, with %d for the number of
		// parts listed before it ("" when it does not start the part).
		start string
		stop  string
	}{
		{
			name: "Start(ctx) and Close()",
			register: func(s *Server, a, b *partCalls) (any, any) {
				pa, pb := fakeStartCloser{a}, fakeStartCloser{b}
				s.startPartWithClose(startCtx, "a", pa)
				s.startPartWithClose(startCtx, "b", pb)
				return pa, pb
			},
			start: "Start(start) listed=%d",
			stop:  "Close()",
		},
		{
			name: "Start(ctx) and Stop(ctx)",
			register: func(s *Server, a, b *partCalls) (any, any) {
				pa, pb := fakeStartStopper{a}, fakeStartStopper{b}
				s.startPartWithStop(startCtx, "a", pa)
				s.startPartWithStop(startCtx, "b", pb)
				return pa, pb
			},
			start: "Start(start) listed=%d",
			stop:  "Stop(shutdown)",
		},
		{
			name: "Start() and Stop()",
			register: func(s *Server, a, b *partCalls) (any, any) {
				pa, pb := fakePlainStartStopper{a}, fakePlainStartStopper{b}
				s.startPartWithoutCtx("a", pa)
				s.startPartWithoutCtx("b", pb)
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
			a, b := &partCalls{srv: s}, &partCalls{srv: s}
			pa, pb := tt.register(s, a, b)
			require.NoError(t, s.lifecycle.err)
			assert.Equal(t, []string{"a", "b"}, s.lifecycle.names())

			// 起動は登録したその場で 1 回だけ、登録の前に呼ぶ。b の起動は a の登録の後。
			var wantA, wantB []string
			if tt.start != "" {
				wantA = []string{fmt.Sprintf(tt.start, 0)}
				wantB = []string{fmt.Sprintf(tt.start, 1)}
			}
			assert.Equal(t, wantA, a.calls, "a was not started once, before it was added")
			assert.Equal(t, wantB, b.calls, "b was not started once, before it was added")

			va, _ := s.lifecycle.partValue("a")
			vb, _ := s.lifecycle.partValue("b")
			assert.Equal(t, pa, va, "part a does not hold the registered value")
			assert.Equal(t, pb, vb, "part b does not hold the registered value")
			for _, part := range s.lifecycle.parts {
				assert.Truef(t, part.fromValue, "part %q is not marked as added from its value", part.name)
			}

			// 止めると、登録したその値のメソッドが 1 回ずつ呼ばれる。
			s.lifecycle.stopAll(shutdownCtx)
			assert.Equal(t, append(wantA, tt.stop), a.calls, "stopping did not stop a itself")
			assert.Equal(t, append(wantB, tt.stop), b.calls, "stopping did not stop b itself")
			// Close() のエラーは捨てる。一覧の登録のエラーにもしない。
			assert.NoError(t, s.lifecycle.err)
		})
	}
}

// TestServer_StartPartDoesNotAddAPartWhoseStartPanics pins the order of a
// registration that starts the part: the start runs before the part is added,
// so a part whose start panics is not stopped by the rollback.
func TestServer_StartPartDoesNotAddAPartWhoseStartPanics(t *testing.T) {
	s := &Server{}
	p := fakeStartCloser{&partCalls{srv: s}}
	require.Panics(t, func() {
		s.startPartWithClose(context.Background(), "a", panickingStartCloser{p})
	})
	assert.Empty(t, s.lifecycle.names())
}

type panickingStartCloser struct{ fakeStartCloser }

func (panickingStartCloser) Start(context.Context) { panic("start failed") }

// TestServer_AddPartDoesNotMarkThePartAsFromValue checks that a part added by
// addPart, even one that has a shape of its own, is not marked as added from
// its value, and that addPart does not refuse it at run time (#3701 review
// L-2: the check is in TestNewServer_ShutdownStopsThePartsInTheirOrder).
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
	{"startPartWithClose", "startCloser", func(v any) bool { _, ok := v.(startCloser); return ok }},
	{"startPartWithStop", "startStopper", func(v any) bool { _, ok := v.(startStopper); return ok }},
	{"startPartWithoutCtx", "plainStartStopper", func(v any) bool { _, ok := v.(plainStartStopper); return ok }},
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

// TestValueShapes_CoverEveryRegistrationFromValue reads lifecycle_parts.go and
// lists every method of Server that adds a part through addFromValue, with
// the interface type of the parameter it passes as the value. valueShapes has
// to list exactly those, so a new shape added to the registrations cannot be
// missed by the check of addPart (addPartShapeViolations).
//
// 構文木で読むのは、形を判定する関数が interface の一覧を実行時に取れないため。
// 本体の登録を足して valueShapes に足し忘れると、addPart の検査がその形を見逃す。
// 読むのは lifecycle_parts.go の Server のメソッドだけなので、登録は Server の
// メソッドとしてこのファイルに置くこと。別のファイルのメソッドと、型パラメータを
// 持つ自由関数で書いた登録は拾えない (#3701 のレビュー 3 周目、#3369 に記録)。
func TestValueShapes_CoverEveryRegistrationFromValue(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "lifecycle_parts.go", nil, 0)
	require.NoError(t, err)
	got := map[string]string{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		params := map[string]string{}
		for _, field := range fn.Type.Params.List {
			ident, ok := field.Type.(*ast.Ident)
			if !ok {
				continue
			}
			for _, n := range field.Names {
				params[n.Name] = ident.Name
			}
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "addFromValue" || len(call.Args) != 3 {
				return true
			}
			arg, ok := call.Args[1].(*ast.Ident)
			if !assert.Truef(t, ok, "%s passes a value to addFromValue that is not a parameter", fn.Name.Name) {
				return true
			}
			got[fn.Name.Name] = params[arg.Name]
			return true
		})
	}
	// 何も拾えなかったら落とす (書式が変わって抽出が空振りしたとき)。
	require.NotEmpty(t, got, "found no registration that calls addFromValue in lifecycle_parts.go")
	want := map[string]string{}
	for _, sh := range valueShapes {
		want[sh.registration] = sh.iface
	}
	assert.Equal(t, want, got, "valueShapes does not list the registrations of lifecycle_parts.go")
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
		{name: "fake Start(ctx) and Close()", part: fakeStartCloser{}, use: "startPartWithClose"},
		{name: "fake Start(ctx) and Stop(ctx)", part: fakeStartStopper{}, use: "startPartWithStop"},
		{name: "fake Start() and Stop()", part: fakePlainStartStopper{}, use: "startPartWithoutCtx"},
		{name: "fake Close(ctx)", part: fakeCtxCloser{}, use: "addCloser"},
		{name: "fake Close() error", part: fakeErrCloser{}, use: "addErrCloser"},

		// 値から作る登録で足す 9 つの部品の型 (8 つ。deliveryHealth と inboxHealth は同じ型)。
		// pluginstore.Store だけは setupRoutes ではなく dbBackedStorage が登録する。
		{name: "TouchBuffer", part: (*coreinstance.TouchBuffer)(nil), use: "startPartWithClose"},
		{name: "CapabilityBuffer", part: (*coreinstance.CapabilityBuffer)(nil), use: "startPartWithClose"},
		{name: "CounterBuffer", part: (*coreinstance.CounterBuffer)(nil), use: "startPartWithClose"},
		{name: "deliveryhealth.Service", part: (*deliveryhealth.Service)(nil), use: "startPartWithStop"},
		{name: "ServerStatsPublisher", part: (*stream.ServerStatsPublisher)(nil), use: "startPartWithoutCtx"},
		{name: "QueueStatsPublisher", part: (*stream.QueueStatsPublisher)(nil), use: "startPartWithoutCtx"},
		{name: "MetadataRefresher", part: (*coreinstance.MetadataRefresher)(nil), use: "addCloser"},
		{name: "pluginstore.Store", part: (*pluginstore.Store)(nil), use: "addErrCloser"},

		// closure でしか止められないので addPart で足す型と、値を持たない worker。
		{name: "fedrule.HitStore", part: (*fedrule.HitStore)(nil)},
		{name: "hashtag.Service", part: (*corehashtag.Service)(nil)},
		{name: "EmojiInvalidationQueue", part: (*repository.EmojiInvalidationQueue)(nil)},
		{name: "paramshadow.Recorder", part: (*paramshadow.Recorder)(nil)},
		{name: "no value", part: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.use, valueShape(tt.part))
		})
	}
}
