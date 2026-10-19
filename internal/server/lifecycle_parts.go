package server

import (
	"context"
	"errors"
	"fmt"
)

// lifecyclePart is one named part of a server generation that has to be
// stopped when the generation stops (#3699).
type lifecyclePart struct {
	// name identifies the part. It is unique within one generation and stays
	// the same across releases, so tests and logs can refer to it.
	name string
	// part is the value stop acts on (a buffer, a publisher, a service), or
	// nil for a worker started by startConstructionWorker, which has no
	// value of its own. Production code never reads it. Only tests read it,
	// to check that stopping the generation stopped that value and that each
	// name holds a value of the expected type.
	//
	// For a part added from its value (startPartWithClose and the others,
	// #3701), stop is built from part itself, so the two cannot disagree. For
	// a part added by addPart with its own stop, nothing ties part to stop,
	// neither the type system nor a check at run time.
	//
	// 停止の関数が名前どおりの部品を止めているかは、名前の並びだけでは
	// 分からない (#3699 のレビュー L-1)。テストが部品そのものを引けるようにする。
	// addPart で登録した部品どうしの取り違えは、同じ型なら型の表のテストでも落とせない。
	part any
	// stop stops the part. It receives the shutdown ctx, which bounds any
	// graceful drain the part does.
	stop func(context.Context)
}

// lifecycle is the ordered list of the named parts of one server generation
// (#3666 stage 3, #3699). Parts are stopped in the order they were added
// (FIFO), after the queue worker and the goroutines of the generation and
// before the chart management, the queue driver and the Redis subscriptions
// (see Server.shutdown and docs/architecture.md 4.3).
//
// The zero value is an empty list ready to use. Each part carries its stop
// (and the value it stops), not its start; it is started at the place it is
// built in setupRoutes, which this list does not move.
type lifecycle struct {
	parts []lifecyclePart
	// err is the first invalid registration (an empty name, a nil stop or a
	// name added twice). newServer fails the construction with it.
	err error
}

// errLifecyclePart is wrapped by every registration error of lifecycle.
var errLifecyclePart = errors.New("server: invalid lifecycle part")

// add appends a part named name whose stop is stop. part is the value stop
// acts on, or nil.
//
// 不正な登録 (名前が空、stop が nil、名前の重複) は構築の失敗にする (constructionError)。
// その場で panic しないのは、プラグインの store の名前のように実行時の値から作る
// 名前もあるため。重複した部品も一覧には入れる。構築に失敗した世代の後始末
// (cleanupConstruction) で、その部品も止めるため。
func (l *lifecycle) add(name string, part any, stop func(context.Context)) {
	switch {
	case name == "":
		l.fail(fmt.Errorf("%w: a part has no name", errLifecyclePart))
	case stop == nil:
		l.fail(fmt.Errorf("%w: part %q has no stop", errLifecyclePart, name))
	case l.has(name):
		l.fail(fmt.Errorf("%w: part %q is added twice", errLifecyclePart, name))
	}
	if stop == nil {
		return
	}
	l.parts = append(l.parts, lifecyclePart{name: name, part: part, stop: stop})
}

func (l *lifecycle) fail(err error) {
	if l.err == nil {
		l.err = err
	}
}

func (l *lifecycle) has(name string) bool {
	for _, p := range l.parts {
		if p.name == name {
			return true
		}
	}
	return false
}

// names returns the names of the parts in stop order.
func (l *lifecycle) names() []string {
	names := make([]string, len(l.parts))
	for i, p := range l.parts {
		names[i] = p.name
	}
	return names
}

// stopAll stops every part in the order it was added, passing ctx to each.
func (l *lifecycle) stopAll(ctx context.Context) {
	for _, p := range l.parts {
		p.stop(ctx)
	}
}

// addPart registers a named part of this generation to be stopped by
// Shutdown (and by the rollback of a failed construction). Parts are stopped
// in registration order (FIFO); see lifecycle. part is the value stop acts
// on (nil when there is none).
//
// It is for a part whose stop cannot be built from its value (a ctx to
// cancel, a goroutine to wait for, a warning to log). A part whose start and
// stop are its own methods is added by startPartWithClose, startPartWithStop,
// startPartWithoutCtx, addCloser or addErrCloser instead.
func (s *Server) addPart(name string, part any, stop func(context.Context)) {
	s.lifecycle.add(name, part, stop)
}

// The shapes of a part whose start and stop are its own methods (#3701).
// A part registered through one of the functions below is added with its
// stop built from the value itself, so the stop cannot act on another value.
type (
	// startCloser is started with a ctx and stopped by Close, which takes
	// no ctx (the instance buffers).
	startCloser interface {
		Start(ctx context.Context)
		Close()
	}
	// startStopper is started with a ctx and stopped by Stop with the
	// shutdown ctx (deliveryhealth.Service).
	startStopper interface {
		Start(ctx context.Context)
		Stop(ctx context.Context)
	}
	// plainStartStopper is started and stopped without a ctx (the stats
	// publishers of the stream).
	plainStartStopper interface {
		Start()
		Stop()
	}
	// ctxCloser is stopped by Close with the shutdown ctx and has no start
	// of its own (instance.MetadataRefresher).
	ctxCloser interface {
		Close(ctx context.Context)
	}
	// errCloser is stopped by Close, whose error is dropped, and has no start
	// of its own (pluginstore.Store).
	errCloser interface {
		Close() error
	}
)

// startPartWithClose starts p with ctx and then adds it as a part named
// name, stopped by p.Close(). The shutdown ctx is not passed to Close.
//
// 起動してから登録する (#3699 の前からの順)。起動が panic した部品は一覧に入らない。
func (s *Server) startPartWithClose(ctx context.Context, name string, p startCloser) {
	p.Start(ctx)
	s.lifecycle.add(name, p, func(context.Context) { p.Close() })
}

// startPartWithStop starts p with ctx and then adds it as a part named name,
// stopped by p.Stop with the shutdown ctx.
func (s *Server) startPartWithStop(ctx context.Context, name string, p startStopper) {
	p.Start(ctx)
	s.lifecycle.add(name, p, p.Stop)
}

// startPartWithoutCtx starts p and then adds it as a part named name,
// stopped by p.Stop(). Neither takes a ctx.
func (s *Server) startPartWithoutCtx(name string, p plainStartStopper) {
	p.Start()
	s.lifecycle.add(name, p, func(context.Context) { p.Stop() })
}

// addCloser adds p as a part named name, stopped by p.Close with the
// shutdown ctx. It does not start p.
func (s *Server) addCloser(name string, p ctxCloser) {
	s.lifecycle.add(name, p, p.Close)
}

// addErrCloser adds p as a part named name, stopped by p.Close(). The
// error of Close is dropped, and the shutdown ctx is not passed. It does not
// start p.
//
// エラーを捨てるのは #3699 の前の hook と同じ。停止の一覧はエラーを返す口を持たない。
func (s *Server) addErrCloser(name string, p errCloser) {
	s.lifecycle.add(name, p, func(context.Context) { _ = p.Close() })
}
