package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// lifecyclePart is one named part of a server generation that has to be
// stopped when the generation stops (#3699), and started by StartBackground
// when it has a start (#3703).
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
	// start starts the part, or is nil for a part that has nothing to start
	// (a part that only has to be stopped). StartBackground calls it once,
	// in list order (lifecycle.startAll).
	start func()
	// stop stops the part. It receives the shutdown ctx, which bounds any
	// graceful drain the part does. It must be safe to call when start has
	// not run: a generation whose construction or StartBackground failed is
	// stopped with parts that never started (#3703).
	stop func(context.Context)
	// fromValue reports that the part was added by a registration that
	// builds its stop from part (startPartWithClose and the others, #3701),
	// not by addPart. Production code never reads it; tests use it to find
	// a part that could be added from its value but went through addPart.
	fromValue bool
}

// lifecycle is the ordered list of the named parts of one server generation
// (#3666 stage 3, #3699). Parts are started in the order they were added, by
// StartBackground before the queue worker (#3703), and stopped in the same
// order (FIFO), after the queue worker and the goroutines of the generation
// and before the chart management, the queue driver and the Redis
// subscriptions (see Server.StartBackground, Server.shutdown and
// docs/architecture.md 4.3).
//
// The zero value is an empty list ready to use. Building a generation only
// adds the parts; no part with a start runs until startAll. One part without
// a start is the exception: setupRoutes runs emojiInvalidationQueue in a go
// statement during construction and adds it with its stop only.
//
// emojiInvalidationQueueを一覧の起動へ移すかは、次の段で決める(#3666に記録)。
//
// 構築では作って登録するだけにし、起動はStartBackgroundに寄せる(#3703)。構築だけして
// 起動しない使い方(dump-routes、テストのnewServer)で部品が動かないようにし、段階(6)で
// 作り直す範囲を部品の単位に絞れるよう、作ることと動かすことを分ける。
type lifecycle struct {
	parts []lifecyclePart
	// err is the first invalid registration (an empty name, a nil stop or a
	// name added twice). newServer fails the construction with it.
	err error

	// mu guards started and stopped, so that the parts are started at most
	// once and no part is started after stopAll has begun.
	//
	// 止め始めた後に起動すると、止める人の居ないgoroutineが残る。起動(startAll)は
	// goroutineを立てるだけで短いので、起動の間は握ったままでよい。
	mu      sync.Mutex
	started bool
	stopped bool
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
	l.addWithStart(name, part, nil, stop)
}

// addWithStart appends a part named name, started by start (nil when there is
// nothing to start) and stopped by stop, with the checks of add. It reports
// whether the part was appended (a part with a nil stop is not).
func (l *lifecycle) addWithStart(name string, part any, start func(), stop func(context.Context)) bool {
	switch {
	case name == "":
		l.fail(fmt.Errorf("%w: a part has no name", errLifecyclePart))
	case stop == nil:
		l.fail(fmt.Errorf("%w: part %q has no stop", errLifecyclePart, name))
	case l.has(name):
		l.fail(fmt.Errorf("%w: part %q is added twice", errLifecyclePart, name))
	}
	if stop == nil {
		return false
	}
	l.parts = append(l.parts, lifecyclePart{name: name, part: part, start: start, stop: stop})
	return true
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

// errLifecycleStart is wrapped by the error of a part whose start panicked.
var errLifecycleStart = errors.New("server: a lifecycle part failed to start")

// startAll starts every part that has a start, in the order it was added.
// Only the first call starts anything, and none does once stopAll has begun.
// When a start panics, startAll recovers it, starts none of the parts after
// it and returns an error wrapping errLifecycleStart; the caller then stops
// the generation, which stops every part, started or not.
//
// panicを回収してエラーにするのは、StartBackgroundの失敗としてSupervisorに世代を
// 止めさせるため(#3703)。構築の中で起動していた頃は、panicは構築ごとプロセスを
// 落としていた。後の部品を起動しないのは、失敗した世代はすぐに止めるので、動かしても
// 止めるだけになるから。
func (l *lifecycle) startAll() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started || l.stopped {
		return nil
	}
	l.started = true
	for _, p := range l.parts {
		if p.start == nil {
			continue
		}
		if err := startPart(p); err != nil {
			return err
		}
	}
	return nil
}

// startPart calls the start of p, turning a panic into an error.
func startPart(p lifecyclePart) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: part %q panicked: %v", errLifecycleStart, p.name, r)
		}
	}()
	p.start()
	return nil
}

// stopAll stops every part in the order it was added, passing ctx to each,
// whether it was started or not. Once it has begun, startAll starts nothing.
func (l *lifecycle) stopAll(ctx context.Context) {
	l.mu.Lock()
	l.stopped = true
	parts := l.parts
	l.mu.Unlock()
	for _, p := range parts {
		p.stop(ctx)
	}
}

// addPart registers a named part of this generation to be stopped by
// Shutdown (and by the rollback of a failed construction). Parts are stopped
// in registration order (FIFO); see lifecycle. part is the value stop acts
// on (nil when there is none). The part has nothing to start; one that
// StartBackground has to start is added by addPartWithStart, by
// startConstructionWorker or by a registration from its value.
//
// It is for a part whose stop cannot be built from its value (a ctx to
// cancel, a goroutine to wait for, a warning to log). A part whose start and
// stop are its own methods is added by startPartWithClose, startPartWithStop,
// startPartWithoutCtx, addCloser or addErrCloser instead. Tests check that
// no part of setupRoutes with one of those shapes comes through addPart
// (docs/architecture.md 4.3); addPart itself does not refuse it.
//
// 実行時には拒まない。部品の型にメソッドを足しただけで本番の構築が失敗し、
// 案内どおりに直すと closure の停止 (時間切れの警告など) が失われるため (#3701 の
// レビュー L-2)。検査は newServer で作った一覧をテストで見る。
func (s *Server) addPart(name string, part any, stop func(context.Context)) {
	s.lifecycle.add(name, part, stop)
}

// addPartWithStart registers a named part like addPart, with start, which
// StartBackground calls once, in list order (#3703). stop must be safe to
// call when start has not run.
//
// 値から作る登録の形に合わない部品(ctxのcancelで止めるfedRuleHits)に使う。
func (s *Server) addPartWithStart(name string, part any, start func(), stop func(context.Context)) {
	s.lifecycle.addWithStart(name, part, start, stop)
}

// addFromValue adds a part whose start (nil when it has none) and stop are
// built from part itself, and marks it so (lifecyclePart.fromValue).
func (l *lifecycle) addFromValue(name string, part any, start func(), stop func(context.Context)) {
	if l.addWithStart(name, part, start, stop) {
		l.parts[len(l.parts)-1].fromValue = true
	}
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

// startPartWithClose adds p as a part named name, started by p.Start(ctx)
// when StartBackground starts the parts, and stopped by p.Close(). The
// shutdown ctx is not passed to Close. It does not start p itself.
//
// 名前のstartは「StartBackgroundで起動する部品を登録する」の意味(#3703より前は、
// 登録したその場で起動していた)。ctxは登録の時に受け取り、起動の時に渡す。
func (s *Server) startPartWithClose(ctx context.Context, name string, p startCloser) {
	s.lifecycle.addFromValue(name, p, func() { p.Start(ctx) }, func(context.Context) { p.Close() })
}

// startPartWithStop adds p as a part named name, started by p.Start(ctx)
// when StartBackground starts the parts, and stopped by p.Stop with the
// shutdown ctx. It does not start p itself.
func (s *Server) startPartWithStop(ctx context.Context, name string, p startStopper) {
	s.lifecycle.addFromValue(name, p, func() { p.Start(ctx) }, p.Stop)
}

// startPartWithoutCtx adds p as a part named name, started by p.Start() when
// StartBackground starts the parts, and stopped by p.Stop(). Neither takes a
// ctx. It does not start p itself.
func (s *Server) startPartWithoutCtx(name string, p plainStartStopper) {
	s.lifecycle.addFromValue(name, p, p.Start, func(context.Context) { p.Stop() })
}

// addCloser adds p as a part named name, stopped by p.Close with the
// shutdown ctx. It has nothing to start.
func (s *Server) addCloser(name string, p ctxCloser) {
	s.lifecycle.addFromValue(name, p, nil, p.Close)
}

// addErrCloser adds p as a part named name, stopped by p.Close(). The
// error of Close is dropped, and the shutdown ctx is not passed. It does not
// start p.
//
// エラーを捨てるのは #3699 の前の hook と同じ。停止の一覧はエラーを返す口を持たない。
func (s *Server) addErrCloser(name string, p errCloser) {
	s.lifecycle.addFromValue(name, p, nil, func(context.Context) { _ = p.Close() })
}

// startConstructionWorker adds a part named name that, when StartBackground
// starts the parts, runs run in a goroutine. Its stop cancels run's ctx and
// waits for run to return (at most until the shutdown ctx is done); when run
// was never started, the stop returns at once.
//
// 同じ関数から複数の部品を登録するので、名前は呼び出し元が渡す(#3699)。関数の名前は
// 構築の途中で起動していた頃(#3703より前)のまま。起動していないworkerの停止は待たずに
// 返す。doneは起動しないと閉じないので、待つと停止のctxが切れるまで固まる。
func (s *Server) startConstructionWorker(name string, run func(context.Context)) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var started atomic.Bool
	var once sync.Once
	s.lifecycle.addWithStart(name, nil, func() {
		started.Store(true)
		go func() {
			defer close(done)
			run(ctx)
		}()
	}, func(shutdownCtx context.Context) {
		once.Do(cancel)
		if !started.Load() {
			return
		}
		select {
		case <-done:
		case <-shutdownCtx.Done():
		}
	})
}
