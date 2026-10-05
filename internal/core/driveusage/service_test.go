package driveusage

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRepo counts calls and can block so the singleflight behaviour is
// observable without sleeping.
type fakeRepo struct {
	calls atomic.Int64
	// topNSeen records the argument of the last call.
	topNSeen atomic.Int64
	err      error
	// gate, when non-nil, is waited on inside Breakdown.
	gate chan struct{}
}

func (f *fakeRepo) Breakdown(topN int) (*repository.DriveUsageBreakdown, error) {
	f.calls.Add(1)
	f.topNSeen.Store(int64(topN))
	if f.gate != nil {
		<-f.gate
	}
	if f.err != nil {
		return nil, f.err
	}
	return &repository.DriveUsageBreakdown{
		ByKind: []repository.DriveUsageKindRow{{
			Kind:             repository.DriveUsageKindAttachment,
			Origin:           repository.DriveUsageOriginLocal,
			DriveUsageBucket: repository.DriveUsageBucket{Count: 1, Size: 10},
		}},
	}, nil
}

// fixedClock advances only when the test says so.
type fixedClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fixedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func newTestService(repo repository.DriveUsageRepository, ttl time.Duration) (*Service, *fixedClock) {
	s := NewService(repo, ttl, 7)
	clock := &fixedClock{at: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)}
	s.now = clock.now
	return s, clock
}

func TestService_ComputesAndCaches(t *testing.T) {
	repo := &fakeRepo{}
	s, _ := newTestService(repo, time.Minute)

	first, err := s.Breakdown(false)
	require.NoError(t, err)
	assert.False(t, first.Cached, "初回は計算したのに Cached が立っている")
	assert.Equal(t, 7, first.TopN)
	assert.Equal(t, time.Minute, first.TTL)
	assert.Equal(t, int64(7), repo.topNSeen.Load())
	assert.Equal(t, int64(10), first.Breakdown.Total().Size)

	second, err := s.Breakdown(false)
	require.NoError(t, err)
	assert.True(t, second.Cached, "TTL 内なのに再計算している")
	assert.Equal(t, int64(1), repo.calls.Load(), "TTL 内で集計が 2 回走っている")
	assert.Equal(t, first.CalculatedAt, second.CalculatedAt)
}

func TestService_RecomputesAfterTTL(t *testing.T) {
	repo := &fakeRepo{}
	s, clock := newTestService(repo, time.Minute)

	_, err := s.Breakdown(false)
	require.NoError(t, err)

	// 境界ちょうどは期限切れ扱い (>= で判定している)。
	clock.advance(time.Minute)
	res, err := s.Breakdown(false)
	require.NoError(t, err)
	assert.False(t, res.Cached)
	assert.Equal(t, int64(2), repo.calls.Load())

	// 境界の手前は使い回す。
	clock.advance(time.Minute - time.Nanosecond)
	res, err = s.Breakdown(false)
	require.NoError(t, err)
	assert.True(t, res.Cached)
	assert.Equal(t, int64(2), repo.calls.Load())
}

func TestService_ForceRecalcBypassesCache(t *testing.T) {
	repo := &fakeRepo{}
	s, _ := newTestService(repo, time.Hour)

	_, err := s.Breakdown(false)
	require.NoError(t, err)

	res, err := s.Breakdown(true)
	require.NoError(t, err)
	assert.False(t, res.Cached, "強制再計算なのに使い回している")
	assert.Equal(t, int64(2), repo.calls.Load())

	// 強制再計算の結果は次の呼び出しから使い回される。
	res, err = s.Breakdown(false)
	require.NoError(t, err)
	assert.True(t, res.Cached)
	assert.Equal(t, int64(2), repo.calls.Load())
}

// ttl <= 0 は「キャッシュしない」。config で 0 を渡した構成が黙って永久
// キャッシュにならないこと。
func TestService_ZeroTTLDisablesCaching(t *testing.T) {
	repo := &fakeRepo{}
	s, _ := newTestService(repo, 0)

	for i := 0; i < 3; i++ {
		res, err := s.Breakdown(false)
		require.NoError(t, err)
		assert.False(t, res.Cached)
		assert.Zero(t, res.TTL)
	}
	assert.Equal(t, int64(3), repo.calls.Load())
}

func TestService_DefaultsTopN(t *testing.T) {
	repo := &fakeRepo{}
	s := NewService(repo, time.Minute, 0)
	assert.Equal(t, DefaultTopN, s.TopN())
	assert.Equal(t, time.Minute, s.TTL())

	res, err := s.Breakdown(false)
	require.NoError(t, err)
	assert.Equal(t, DefaultTopN, res.TopN)
	assert.Equal(t, int64(DefaultTopN), repo.topNSeen.Load())
}

// 失敗はキャッシュしない。焼き付くと、直った後も TTL のあいだエラーのままになる。
func TestService_ErrorIsNotCached(t *testing.T) {
	repo := &fakeRepo{err: errors.New("db down")}
	s, _ := newTestService(repo, time.Hour)

	res, err := s.Breakdown(false)
	require.Error(t, err)
	assert.Nil(t, res)

	repo.err = nil
	res, err = s.Breakdown(false)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.Cached)
	assert.Equal(t, int64(2), repo.calls.Load())
}

// 同時に来た要求は 1 本に畳む。畳まないと管理画面の連打で走査が積み上がる。
//
// **形は internal/activitypub の singleflight テスト (#789) に倣う。** 追従
// goroutine が `group.Do` に到達したことを sleep 無しで待つ手段が無いので、
// (1) 呼び出し直前にカウンタを進め、(2) 全員が到達してから登録が終わるだけの
// 猶予を置き、(3) 閾値は並行数の 1/4 と寛容に取る。畳みが完全に壊れれば
// calls == concurrentCallers になるので検出力は落ちない。
func TestService_ConcurrentCallsCollapse(t *testing.T) {
	const (
		concurrentCallers  = 16
		enterTimeout       = 2 * time.Second
		singleflightSettle = 20 * time.Millisecond
		maxCalls           = concurrentCallers / 4
	)
	repo := &fakeRepo{gate: make(chan struct{})}
	s, _ := newTestService(repo, time.Hour)

	var wg sync.WaitGroup
	var entered atomic.Int64
	results := make([]*Result, concurrentCallers)
	errs := make([]error, concurrentCallers)
	for i := 0; i < concurrentCallers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			entered.Add(1)
			results[i], errs[i] = s.Breakdown(false)
		}(i)
	}

	deadline := time.Now().Add(enterTimeout)
	for entered.Load() < concurrentCallers {
		require.True(t, time.Now().Before(deadline), "呼び出しが出揃わない")
		time.Sleep(time.Millisecond)
	}
	time.Sleep(singleflightSettle)
	close(repo.gate)
	wg.Wait()

	seen := make(map[*Result]bool, concurrentCallers)
	for i := 0; i < concurrentCallers; i++ {
		require.NoError(t, errs[i])
		require.NotNil(t, results[i])
		assert.Equal(t, int64(10), results[i].Breakdown.Total().Size)
		// **追従者どうしも別の構造体を受け取る。** group.Do は全員に同じ
		// ポインタを返すので、複製していないとここで重複する。
		assert.False(t, seen[results[i]], "待機者が同じ *Result を共有している")
		seen[results[i]] = true
	}
	assert.LessOrEqual(t, repo.calls.Load(), int64(maxCalls),
		"同時要求が畳まれていない (%d 並行で %d 回走った)", concurrentCallers, repo.calls.Load())
}

// 返した Result を呼び出し側が書き換えても、保存済みスナップショットは汚れない。
//
// **計算経路とキャッシュ経路の両方を見る。** 計算した側が保存したのと同じポインタを
// 返すと、singleflight の追従者を含めた全員がキャッシュ本体を掴むことになる。
func TestService_ReturnedResultIsACopy(t *testing.T) {
	repo := &fakeRepo{}
	s, _ := newTestService(repo, time.Hour)

	computed, err := s.Breakdown(false)
	require.NoError(t, err)
	computed.TopN = 111
	computed.Cached = true

	cached, err := s.Breakdown(false)
	require.NoError(t, err)
	require.True(t, cached.Cached)
	assert.Equal(t, 7, cached.TopN, "計算経路の返り値を書き換えたらキャッシュが汚れた")
	cached.Cached = false
	cached.TopN = 999

	again, err := s.Breakdown(false)
	require.NoError(t, err)
	assert.True(t, again.Cached, "保存済みスナップショットが書き換えられている")
	assert.Equal(t, 7, again.TopN)
}

// Elapsed は集計に掛かった時間。0 のままだと「一瞬で終わった」と誤読される。
func TestService_ElapsedIsMeasured(t *testing.T) {
	clock := &fixedClock{at: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)}
	repo := &fakeRepo{}
	s := NewService(repo, time.Hour, 7)
	s.now = clock.now
	// 集計の中で時計を進める。
	repo.gate = make(chan struct{})
	close(repo.gate)
	s.repo = &clockAdvancingRepo{inner: repo, clock: clock, by: 250 * time.Millisecond}

	res, err := s.Breakdown(false)
	require.NoError(t, err)
	assert.Equal(t, 250*time.Millisecond, res.Elapsed)
	assert.Equal(t, clock.now().Add(-250*time.Millisecond), res.CalculatedAt)
}

type clockAdvancingRepo struct {
	inner *fakeRepo
	clock *fixedClock
	by    time.Duration
}

func (r *clockAdvancingRepo) Breakdown(topN int) (*repository.DriveUsageBreakdown, error) {
	out, err := r.inner.Breakdown(topN)
	r.clock.advance(r.by)
	return out, err
}
