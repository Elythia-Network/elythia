package reaction

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/repository"
)

// countingNoteRepo records IncrementReaction calls; it is safe for concurrent use.
type countingNoteRepo struct {
	repository.NoteRepository
	mu     sync.Mutex
	counts map[string]map[string]int
}

func newCountingNoteRepo() *countingNoteRepo {
	return &countingNoteRepo{counts: map[string]map[string]int{}}
}

func (r *countingNoteRepo) IncrementReaction(noteID, reaction string, delta int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts[noteID] == nil {
		r.counts[noteID] = map[string]int{}
	}
	// 本物の IncrementReaction と同じく、0 以下になったら key を消す
	// (repository/note.go)。0 未満を覚えないことが #3663 の順序の問題の原因。
	if c := r.counts[noteID][reaction] + delta; c <= 0 {
		delete(r.counts[noteID], reaction)
	} else {
		r.counts[noteID][reaction] = c
	}
	return nil
}

func (r *countingNoteRepo) count(noteID, reaction string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[noteID][reaction]
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newSwitchableForTest(t *testing.T, buffering bool) (*SwitchableWriter, *countingNoteRepo, *miniredis.Miniredis, *fakeClock) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	repo := newCountingNoteRepo()
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	w := NewSwitchableWriter(rdb, repo, buffering)
	// 期限の計算を固定の時計で行う。コンストラクタの期限も同じ時計で置き直す。
	w.now = clock.now
	w.drainUntil.Store(0)
	if !buffering {
		w.drainUntil.Store(clock.now().Add(BufferDrainWindow).UnixNano())
	}
	return w, repo, mr, clock
}

func TestSwitchableWriter_IncrementFollowsMode(t *testing.T) {
	ctx := context.Background()
	w, repo, mr, _ := newSwitchableForTest(t, false)
	require.False(t, w.Buffering())

	require.NoError(t, w.Increment("n1", "a", 1))
	assert.Equal(t, 1, repo.count("n1", "a"))
	assert.False(t, mr.Exists(bufferKeyPrefix+"n1"))

	w.SetBuffering(true)
	require.True(t, w.Buffering())
	require.NoError(t, w.Increment("n1", "a", 1))
	assert.Equal(t, 1, repo.count("n1", "a"), "buffered increments must not reach the DB")
	buf, err := w.GetBuffered(ctx, "n1")
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"a": 1}, buf)
}

// 有効 → 無効: 溜まった分は消さずに読みへ足し続け、Flush で 1 回だけ DB へ移す。
func TestSwitchableWriter_TurningOffKeepsAndDrainsTheBuffer(t *testing.T) {
	ctx := context.Background()
	w, repo, mr, clock := newSwitchableForTest(t, true)
	require.NoError(t, w.Increment("n1", "a", 1))
	require.NoError(t, w.Increment("n1", "a", 1))

	w.SetBuffering(false)
	require.NoError(t, w.Increment("n2", "a", 1))
	assert.Equal(t, 1, repo.count("n2", "a"), "after turning off, increments go to the DB")
	assert.Equal(t, 0, repo.count("n1", "a"), "another note's buffer is left for the flush")

	clock.advance(BufferDrainWindow - time.Second)
	require.True(t, w.NeedsFlush())
	buf, err := w.GetBuffered(ctx, "n1")
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"a": 2}, buf, "reads keep merging the leftover buffer")
	many, err := w.GetBufferedMany(ctx, []string{"n1"})
	require.NoError(t, err)
	assert.Equal(t, map[string]map[string]int64{"n1": {"a": 2}}, many)

	require.NoError(t, w.Flush(ctx))
	require.NoError(t, w.Flush(ctx))
	assert.Equal(t, 2, repo.count("n1", "a"), "each increment is applied exactly once")
	assert.False(t, mr.Exists(bufferKeyPrefix+"n1"))
}

// 有効の間に付け、無効にしてから flush の前に取り消す。DB は 0 未満を覚えない
// ので、取り消しの -1 を先に DB へ書くと消え、後の flush の +1 だけが残る。
// 直接書く前にその note の溜まった分を移すので、数は 0 に戻る。
func TestSwitchableWriter_UnreactAfterTurningOffLeavesNoPhantom(t *testing.T) {
	ctx := context.Background()
	w, repo, mr, _ := newSwitchableForTest(t, true)
	require.NoError(t, w.Increment("n1", "a", 1))

	w.SetBuffering(false)
	require.NoError(t, w.Increment("n1", "a", -1))
	assert.Equal(t, 0, repo.count("n1", "a"))
	assert.False(t, mr.Exists(bufferKeyPrefix+"n1"), "the buffered count is moved before the direct write")

	require.NoError(t, w.Flush(ctx))
	assert.Equal(t, 0, repo.count("n1", "a"))
}

// 窓を過ぎてからの直接の書き込みは、Redis を引かない (残りは flush に任せる)。
func TestSwitchableWriter_DirectWriteAfterWindowDoesNotTouchRedis(t *testing.T) {
	w, repo, mr, clock := newSwitchableForTest(t, false)
	clock.advance(BufferDrainWindow)
	mr.HSet(bufferKeyPrefix+"n1", "a", "3")
	require.NoError(t, w.Increment("n1", "a", 1))
	assert.Equal(t, 1, repo.count("n1", "a"))
	assert.True(t, mr.Exists(bufferKeyPrefix+"n1"))
}

// 窓の長さの意味のある範囲。下限は、無効の通知を取りこぼしたプロセスが
// meta のキャッシュの期限の後の tick で読み直すまで (それまで溜め続ける)、
// 他のプロセスが移してから書くこと。上限は、無効なのに読みと書き込みで Redis を
// 引く時間と、flush の遅れを抑えること (#3663)。
func TestBufferDrainWindowBounds(t *testing.T) {
	assert.Greater(t, BufferDrainWindow, repository.MetaCacheTTL+FlushInterval,
		"a process that missed the disable re-reads meta only after the meta cache expires and the next tick")
	assert.LessOrEqual(t, BufferDrainWindow, 10*time.Minute,
		"Redis is consulted while disabled only for a bounded time")
	assert.LessOrEqual(t, FlushInterval, time.Minute, "buffered counts reach the DB promptly")
	assert.GreaterOrEqual(t, FlushInterval, 25*time.Second,
		"EnqueueReactionFlush dedupes for 25s; a shorter interval enqueues twice")
}

// 無効の間の flush が残りを見つけたら窓を延ばす。queue が遅れても、通知を
// 取りこぼして溜め続けるプロセスがあっても、残りが無くなるまで flush を積む。
func TestSwitchableWriter_FlushThatFindsCountsExtendsTheWindow(t *testing.T) {
	ctx := context.Background()
	w, repo, mr, clock := newSwitchableForTest(t, false)
	clock.advance(BufferDrainWindow - time.Second)
	mr.HSet(bufferKeyPrefix+"n1", "a", "2")
	require.NoError(t, w.Flush(ctx))
	assert.Equal(t, 2, repo.count("n1", "a"))

	clock.advance(BufferDrainWindow - time.Second)
	require.True(t, w.NeedsFlush(), "a flush that found counts extends the window")

	require.NoError(t, w.Flush(ctx))
	clock.advance(time.Second)
	assert.False(t, w.NeedsFlush(), "an empty flush does not extend it")
}

// 有効の間の flush は窓に触らない (無効にした時点から数える)。
func TestSwitchableWriter_FlushWhileBufferingKeepsWindow(t *testing.T) {
	ctx := context.Background()
	w, _, mr, _ := newSwitchableForTest(t, true)
	mr.HSet(bufferKeyPrefix+"n1", "a", "2")
	require.NoError(t, w.Flush(ctx))
	assert.Zero(t, w.drainUntil.Load())
}

func TestSwitchableWriter_DrainWindowExpires(t *testing.T) {
	ctx := context.Background()
	w, _, mr, clock := newSwitchableForTest(t, true)
	require.True(t, w.NeedsFlush())
	w.SetBuffering(false)
	clock.advance(BufferDrainWindow)
	assert.False(t, w.NeedsFlush())

	// 期限を過ぎたら、無効のときは Redis を引かない。
	mr.HSet(bufferKeyPrefix+"n1", "a", "5")
	buf, err := w.GetBuffered(ctx, "n1")
	require.NoError(t, err)
	assert.Nil(t, buf)
	many, err := w.GetBufferedMany(ctx, []string{"n1"})
	require.NoError(t, err)
	assert.Nil(t, many)

	// 同じ値を渡し直しても期限は延びない (自プロセスの更新は 2 回届く)。
	w.SetBuffering(false)
	assert.False(t, w.NeedsFlush())
}

// 無効で始めても、前の世代が溜めた分を移すために BufferDrainWindow の間は flush する。
func TestSwitchableWriter_StartingOffDrainsLeftovers(t *testing.T) {
	ctx := context.Background()
	w, repo, mr, clock := newSwitchableForTest(t, false)
	mr.HSet(bufferKeyPrefix+"n1", "a", "4")
	require.True(t, w.NeedsFlush())
	buf, err := w.GetBuffered(ctx, "n1")
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"a": 4}, buf)
	require.NoError(t, w.Flush(ctx))
	assert.Equal(t, 4, repo.count("n1", "a"))
	clock.advance(BufferDrainWindow)
	assert.False(t, w.NeedsFlush())
}

func TestNewSwitchableWriter_InitialWindow(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	off := NewSwitchableWriter(rdb, newCountingNoteRepo(), false)
	assert.True(t, off.NeedsFlush())
	assert.WithinDuration(t, time.Now().Add(BufferDrainWindow), time.Unix(0, off.drainUntil.Load()), 5*time.Second)
	on := NewSwitchableWriter(rdb, newCountingNoteRepo(), true)
	assert.True(t, on.NeedsFlush())
	assert.Zero(t, on.drainUntil.Load())
}

// 切り替えと flush が並行しても、取りこぼしも二重の加算も起きない。
func TestSwitchableWriter_ConcurrentToggleAndFlush(t *testing.T) {
	ctx := context.Background()
	w, repo, _, _ := newSwitchableForTest(t, false)
	const writers, perWriter = 8, 200

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var bg sync.WaitGroup
	bg.Add(2)
	go func() {
		defer bg.Done()
		on := false
		for {
			select {
			case <-stop:
				return
			default:
				on = !on
				w.SetBuffering(on)
			}
		}
	}()
	go func() {
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				assert.NoError(t, w.Flush(ctx))
			}
		}
	}()
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				assert.NoError(t, w.Increment("n1", "a", 1))
			}
		}()
	}
	wg.Wait()
	close(stop)
	bg.Wait()
	require.NoError(t, w.Flush(ctx))
	assert.Equal(t, writers*perWriter, repo.count("n1", "a"))
}

// SCAN が失敗しても窓は延ばさない。延ばすと Redis が落ちている間ずっと窓が
// 閉じず、無効なのに書き込みと読みのたびに落ちた Redis を引く。
func TestSwitchableWriter_FlushScanFailureDoesNotExtendTheWindow(t *testing.T) {
	ctx := context.Background()
	w, _, mr, clock := newSwitchableForTest(t, false)
	clock.advance(BufferDrainWindow - time.Second)
	mr.Close()
	require.Error(t, w.Flush(ctx))
	clock.advance(time.Second)
	assert.False(t, w.NeedsFlush())
}

func TestSwitchableWriter_SetClock(t *testing.T) {
	w, _, _, _ := newSwitchableForTest(t, false)
	w.SetClock(func() time.Time { return time.Unix(1_900_000_000, 0) })
	assert.False(t, w.NeedsFlush())
}
