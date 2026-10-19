package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
