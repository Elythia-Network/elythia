package note

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// 既定は GOMAXPROCS 連動。**固定値にすると、コア数の違うホストで意味が変わる。**
func TestNewHookSlots_Default(t *testing.T) {
	slots := newHookSlots(0)
	want := runtime.GOMAXPROCS(0) * defaultHookConcurrencyFactor
	if got := cap(slots[hookFanout]); got != want {
		t.Fatalf("fanout の枠 %d, want %d", got, want)
	}
	if got := cap(slots[hookOther]); got != want {
		t.Fatalf("other の枠 %d, want %d", got, want)
	}
}

// **0 枠を作らせない。** 容量 0 の channel だと送信が永久にブロックし、
// フックが 1 本も走らなくなる。
func TestNewHookSlots_NeverZero(t *testing.T) {
	for _, n := range []int{-1, 0} {
		if got := cap(newHookSlots(n)[hookFanout]); got < 1 {
			t.Fatalf("n=%d で枠が %d になった", n, got)
		}
	}
}

// 枠はCreateServiceが持つ(#3780)。配線しなければ既定、SetHookConcurrencyで
// 設定の数になる。
func TestCreateService_HookConcurrency(t *testing.T) {
	def := runtime.GOMAXPROCS(0) * defaultHookConcurrencyFactor
	tests := []struct {
		name string
		set  func(s *CreateService)
		want int
	}{
		{name: "unwired uses the default", set: func(*CreateService) {}, want: def},
		{name: "configured count", set: func(s *CreateService) { s.SetHookConcurrency(def + 3) }, want: def + 3},
		{name: "zero selects the default", set: func(s *CreateService) { s.SetHookConcurrency(0) }, want: def},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewCreateService(nil, nil, nil, nil)
			tt.set(s)
			if got := s.HookConcurrency(); got != tt.want {
				t.Fatalf("HookConcurrency()=%d, want %d", got, tt.want)
			}
			// 両方の種類が同じ数であること。
			if got := cap(s.hookSlots[hookOther]); got != tt.want {
				t.Fatalf("other の枠 %d, want %d", got, tt.want)
			}
		})
	}
}

// 同時に走る本数が上限を超えないこと。**これが効いていないと、絞りを入れた
// 意味が無い。**
func TestSafeGoKind_BoundsConcurrency(t *testing.T) {
	const limit = 3
	slots := newHookSlots(limit)

	var running, peak int64
	var wg sync.WaitGroup
	release := make(chan struct{})

	for range 30 {
		wg.Add(1)
		safeGoKind(nil, slots, hookFanout, func() {
			defer wg.Done()
			cur := atomic.AddInt64(&running, 1)
			for {
				old := atomic.LoadInt64(&peak)
				if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
					break
				}
			}
			<-release
			atomic.AddInt64(&running, -1)
		})
	}
	// 上限まで詰まるのを待ってから解放する。
	deadline := time.After(5 * time.Second)
	for atomic.LoadInt64(&running) < limit {
		select {
		case <-deadline:
			t.Fatalf("上限まで走らない (running=%d)", atomic.LoadInt64(&running))
		default:
			runtime.Gosched()
		}
	}
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(&peak); got > limit {
		t.Fatalf("同時実行が %d まで伸びた, 上限 %d", got, limit)
	}
}

// **fanout の枠が他のフックに奪われないこと。** 1 つの semaphore を共有すると、
// 他のフックが枠を握っている間 fanout が動けず、投稿がタイムラインに出るまでの
// 時間が壊れる (実測で中央値 5.7 秒 / 25 回中 20 回が 15 秒以内に届かず)。
func TestSafeGoKind_FanoutNotStarvedByOtherHooks(t *testing.T) {
	slots := newHookSlots(2)

	blocked := make(chan struct{})
	occupied := make(chan struct{}, 2)
	// other の枠を上限まで占有する。
	for range 2 {
		safeGoKind(nil, slots, hookOther, func() {
			occupied <- struct{}{}
			<-blocked
		})
	}
	for range 2 {
		select {
		case <-occupied:
		case <-time.After(5 * time.Second):
			t.Fatal("other のフックが起動しない")
		}
	}

	// other が全枠を握っていても fanout は走れること。
	ran := make(chan struct{})
	safeGoKind(nil, slots, hookFanout, func() { close(ran) })
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		close(blocked)
		t.Fatal("other に枠を奪われて fanout が走らない")
	}
	close(blocked)
}

// panic を拾っても枠を返すこと。**返さないと 1 回の panic で枠が 1 つ減り、
// 積み重なると fanout が止まる。**
func TestSafeGoKind_ReleasesSlotOnPanic(t *testing.T) {
	slots := newHookSlots(1)

	done := make(chan struct{})
	safeGoKind(nil, slots, hookFanout, func() { defer close(done); panic("boom") })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("最初のフックが走らない")
	}

	// 枠が返っていれば次が走る。
	ran := make(chan struct{})
	safeGoKind(nil, slots, hookFanout, func() { close(ran) })
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("panic で枠が返っていない")
	}
}

// safeGo は other 扱い (既存の呼び出し側の互換)。
func TestSafeGo_UsesOtherKind(t *testing.T) {
	slots := newHookSlots(1)

	blocked := make(chan struct{})
	started := make(chan struct{})
	safeGo(nil, slots, func() { close(started); <-blocked })
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("safeGo が走らない")
	}

	// other が埋まっていても fanout は独立して走れる。
	ran := make(chan struct{})
	safeGoKind(nil, slots, hookFanout, func() { close(ran) })
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		close(blocked)
		t.Fatal("safeGo が fanout の枠を使っている")
	}
	close(blocked)
}

// 世代の停止の待ちの間に枠が空けば、枠を待っていたフックは走る (#3661 の
// レビュー M-2)。止めた瞬間に捨てると、間に合っていた配送や通知が消える。
func TestSafeGoKind_RunsAHookWhoseSlotFreesUpDuringTheStop(t *testing.T) {
	slots := newHookSlots(1)

	blocked := make(chan struct{})
	occupied := make(chan struct{})
	safeGoKind(nil, slots, hookOther, func() { close(occupied); <-blocked })
	<-occupied

	g := bgwork.New()
	var ran atomic.Bool
	safeGoKind(g, slots, hookOther, func() { ran.Store(true) })
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(blocked)
	}()
	if left := g.StopWithin(2 * time.Second); left != 0 {
		t.Fatalf("フックが終わらない (left=%d)", left)
	}
	if !ran.Load() {
		t.Fatal("待ちの間に枠が空いたのに、フックを捨てた")
	}
	if g.Dropped() != 0 {
		t.Fatalf("Dropped = %d, want 0", g.Dropped())
	}
}

// 待ちの終わり近くまで枠が空かなければ捨て、捨てたことを数える (#3661)。
// **捨てないと、止める側は上限まで待たされ、その後でフックが閉じた DB へ書きに行く。**
func TestSafeGoKind_DropsAHookWaitingForASlotWhenTheStopGivesUp(t *testing.T) {
	slots := newHookSlots(1)

	blocked := make(chan struct{})
	occupied := make(chan struct{})
	safeGoKind(nil, slots, hookOther, func() { close(occupied); <-blocked })
	<-occupied

	g := bgwork.New()
	var ran atomic.Bool
	safeGoKind(g, slots, hookOther, func() { ran.Store(true) })

	if left := g.StopWithin(400 * time.Millisecond); left != 0 {
		t.Fatalf("枠を待っているフックが、ctx が切れても戻らない (left=%d)", left)
	}
	close(blocked)
	time.Sleep(50 * time.Millisecond)
	if ran.Load() {
		t.Fatal("世代を止めた後にフックが走った")
	}
	if g.Dropped() != 1 {
		t.Fatalf("捨てたフックが数えられていない (Dropped = %d)", g.Dropped())
	}
}

// blockingFanoutHook counts the fanout hooks running at once and holds each
// one until release is closed.
type blockingFanoutHook struct {
	running atomic.Int64
	peak    atomic.Int64
	started chan struct{}
	release chan struct{}
}

func (h *blockingFanoutHook) OnNoteCreated(*model.Note, *model.User) {
	cur := h.running.Add(1)
	for {
		old := h.peak.Load()
		if cur <= old || h.peak.CompareAndSwap(old, cur) {
			break
		}
	}
	h.started <- struct{}{}
	<-h.release
	h.running.Add(-1)
}

// 投稿の作成は、CreateServiceが持つ枠でフックを走らせる(#3780)。**既定の枠を
// 使うと、設定の数が効かない。** 枠を1にして2つ投稿し、2本目のfanoutが1本目の
// 終わりまで待つことを見る。
func TestCreateService_RunsHooksWithinItsSlots(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc := NewCreateService(testutil.NewMockNoteRepository(), testutil.NewMockPollRepository(), idGen, nil)
	svc.SetHookConcurrency(1)
	hook := &blockingFanoutHook{started: make(chan struct{}, 2), release: make(chan struct{})}
	svc.SetFanoutHook(hook)

	for range 2 {
		text := "hello"
		_, err := svc.Create(CreateInput{User: &model.User{ID: "author"}, Text: &text, Visibility: model.NoteVisibilityPublic})
		require.NoError(t, err)
	}
	select {
	case <-hook.started:
	case <-time.After(5 * time.Second):
		close(hook.release)
		t.Fatal("fanout のフックが走らない")
	}
	// 枠が1なら、2本目は1本目が終わるまで始まらない。
	select {
	case <-hook.started:
		close(hook.release)
		t.Fatal("枠が1なのに2本目のfanoutが同時に走った")
	case <-time.After(200 * time.Millisecond):
	}
	close(hook.release)
	select {
	case <-hook.started:
	case <-time.After(5 * time.Second):
		t.Fatal("1本目が終わっても2本目のfanoutが走らない")
	}
	assert.Equal(t, int64(1), hook.peak.Load(), "同時に走ったfanoutの数")
}
