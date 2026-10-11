package maintmode

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/pgactivity"
)

func newTestStore(t *testing.T) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	// 閉じた miniredis を使うエラーのテストで、再試行の待ちに時間を取られないようにする。
	c := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = c.Close() })
	return NewStore(c, nil, "pfx:"), mr
}

func TestStore_EnterGetClear(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()

	st, err := s.Get(ctx)
	require.NoError(t, err)
	assert.Nil(t, st)
	assert.False(t, st.Holding())

	started := time.Date(2026, 10, 10, 1, 2, 3, 0, time.UTC)
	lease := started.Add(time.Minute)
	require.NoError(t, s.Enter(ctx, State{
		OpID: "op-1", Kind: KindManual, Reason: "重い migration", RequestedBy: "u1",
		StartedAt: started, LeaseUntil: lease,
	}))
	assert.True(t, mr.Exists("pfx:elythia:maintenance"), "キーに接頭辞が付いていない")

	st, err = s.Get(ctx)
	require.NoError(t, err)
	require.NotNil(t, st)
	assert.Equal(t, State{
		Phase: PhaseEntering, OpID: "op-1", Kind: KindManual, Reason: "重い migration",
		RequestedBy: "u1", StartedAt: started, LeaseUntil: lease,
	}, *st)
	assert.True(t, st.Holding())

	// 2 人目は上書きできない。
	assert.ErrorIs(t, s.Enter(ctx, State{OpID: "op-2"}), ErrActive)

	require.NoError(t, s.SetPhase(ctx, "op-1", PhaseHeld))
	st, _ = s.Get(ctx)
	assert.Equal(t, PhaseHeld, st.Phase)
	assert.True(t, st.Holding())
	assert.ErrorIs(t, s.SetPhase(ctx, "op-x", PhaseResuming), ErrOpMismatch)
	require.NoError(t, s.SetPhase(ctx, "", PhaseResuming))
	st, _ = s.Get(ctx)
	assert.False(t, st.Holding(), "resuming はメンテナンスを抜ける合図")

	assert.ErrorIs(t, s.Clear(ctx, "op-x"), ErrOpMismatch)
	require.NoError(t, s.Clear(ctx, "op-1"))
	st, err = s.Get(ctx)
	require.NoError(t, err)
	assert.Nil(t, st)
	// 無いものを消しても失敗しない。
	require.NoError(t, s.Clear(ctx, "op-1"))
	assert.ErrorIs(t, s.SetPhase(ctx, "", PhaseHeld), ErrNotActive)
}

func TestStore_EnterFillsDefaults(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	before := time.Now().Add(-time.Second)
	require.NoError(t, s.Enter(ctx, State{}))
	st, err := s.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, PhaseEntering, st.Phase)
	assert.True(t, st.StartedAt.After(before))
	assert.True(t, st.LeaseUntil.IsZero())
}

// TestStore_HashWithoutPhaseIsHolding checks the fail-safe reading of a
// half-written or damaged hash.
func TestStore_HashWithoutPhaseIsHolding(t *testing.T) {
	s, mr := newTestStore(t)
	mr.HSet("pfx:elythia:maintenance", "reason", "x")
	st, err := s.Get(context.Background())
	require.NoError(t, err)
	assert.True(t, st.Holding())
	assert.Equal(t, PhaseEntering, st.Phase)
	// 壊れた時刻は零値として読む。
	mr.HSet("pfx:elythia:maintenance", "startedAt", "garbage")
	st, err = s.Get(context.Background())
	require.NoError(t, err)
	assert.True(t, st.StartedAt.IsZero())
}

func TestStore_ReportsExpireAndList(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Report(ctx, ProcReport{ID: "b", Role: "both", State: ProcDrained, OpenConns: 2, InFlight: 1, Error: "boom"}))
	require.NoError(t, s.Report(ctx, ProcReport{ID: "a", State: ProcRunning}))
	assert.Equal(t, ProcTTL, mr.TTL("pfx:elythia:maintenance:proc:a"))

	reps, err := s.Reports(ctx)
	require.NoError(t, err)
	require.Len(t, reps, 2)
	assert.Equal(t, "a", reps[0].ID)
	assert.Equal(t, "b", reps[1].ID)
	assert.Equal(t, ProcDrained, reps[1].State)
	assert.Equal(t, 2, reps[1].OpenConns)
	assert.EqualValues(t, 1, reps[1].InFlight)
	assert.Equal(t, "boom", reps[1].Error)
	assert.False(t, reps[1].UpdatedAt.IsZero())

	// 報告を書き直しても、前の欄が残らない。
	require.NoError(t, s.Report(ctx, ProcReport{ID: "b", State: ProcRunning}))
	reps, _ = s.Reports(ctx)
	assert.Empty(t, reps[1].Error)

	// 期限が切れたプロセスは一覧から消える。
	mr.FastForward(ProcTTL + time.Second)
	reps, err = s.Reports(ctx)
	require.NoError(t, err)
	assert.Empty(t, reps)

	require.NoError(t, s.Report(ctx, ProcReport{ID: "c"}))
	require.NoError(t, s.RemoveReport(ctx, "c"))
	reps, _ = s.Reports(ctx)
	assert.Empty(t, reps)
}

func TestStore_SubscribeDeliversHints(t *testing.T) {
	s, _ := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	hints := s.Subscribe(ctx)
	// 購読が張られるまで待つ (miniredis は SUBSCRIBE 前の publish を捨てる)。
	require.Eventually(t, func() bool {
		s.notify(context.Background())
		select {
		case <-hints:
			return true
		case <-time.After(20 * time.Millisecond):
			return false
		}
	}, 2*time.Second, 10*time.Millisecond)

	require.NoError(t, s.Enter(context.Background(), State{}))
	select {
	case <-hints:
	case <-time.After(2 * time.Second):
		t.Fatal("Enter did not notify")
	}
	cancel()
	require.Eventually(t, func() bool {
		select {
		case _, ok := <-hints:
			return !ok
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond, "ctx を閉じても購読が閉じない")
}

func TestStore_ErrorsSurface(t *testing.T) {
	s, mr := newTestStore(t)
	mr.Close()
	ctx := context.Background()
	_, err := s.Get(ctx)
	assert.Error(t, err)
	assert.Error(t, s.Enter(ctx, State{}))
	assert.Error(t, s.SetPhase(ctx, "", PhaseHeld))
	assert.Error(t, s.Clear(ctx, "op"))
	assert.Error(t, s.Clear(ctx, ""))
	assert.Error(t, s.Report(ctx, ProcReport{ID: "a"}))
	_, err = s.Reports(ctx)
	assert.Error(t, err)
}

func TestEvictsState(t *testing.T) {
	for policy, want := range map[string]bool{
		"noeviction": false, "volatile-lru": false, "volatile-ttl": false, "": false,
		"allkeys-lru": true, "allkeys-lfu": true, "allkeys-random": true,
	} {
		assert.Equal(t, want, EvictsState(policy), policy)
	}
}

// miniredis は CONFIG を受けない。マネージドの Redis と同じく、エラーとして返る。
func TestStore_EvictionPolicyErrorSurfaces(t *testing.T) {
	s, _ := newTestStore(t)
	_, err := s.EvictionPolicy(context.Background())
	assert.Error(t, err)
}

// TestStore_JudgeFieldsRoundTrip checks that what the judge writes is read
// back, and that broken values read as empty instead of failing.
func TestStore_JudgeFieldsRoundTrip(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
	at := time.Date(2026, 10, 11, 1, 2, 3, 0, time.UTC)
	b := Blocker{Server: "primary", Session: pgactivity.Session{PID: 7, User: "u", Application: "psql"}}
	require.NoError(t, s.progress(ctx, "op", at, []string{"p1(running)"}, []Blocker{b}, []Blocker{b}, "boom"))
	st, err := s.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, at, st.JudgedAt)
	assert.Equal(t, []string{"p1(running)"}, st.WaitingFor)
	assert.Equal(t, []Blocker{b}, st.Blockers)
	assert.Equal(t, []Blocker{b}, st.Terminated)
	assert.Equal(t, "boom", st.JudgeError)

	mr.HSet(s.StateKey(), "blockers", "{broken")
	st, err = s.Get(ctx)
	require.NoError(t, err)
	assert.Empty(t, st.Blockers)
}

// TestStore_JudgeWritesAreTiedToTheOpID checks that the judge's writes only
// land on the maintenance they were judged for, and only in the phase they
// expect (#3463 の設計第2版 P7)。
func TestStore_JudgeWritesAreTiedToTheOpID(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Millisecond)
	require.NoError(t, s.Enter(ctx, State{OpID: "new"}))

	moved, err := s.markHeld(ctx, "old", at, nil)
	require.NoError(t, err)
	assert.False(t, moved, "古い opId の判定で held にした")
	require.NoError(t, s.progress(ctx, "old", at, []string{"x"}, nil, nil, ""))
	st, _ := s.Get(ctx)
	assert.Equal(t, PhaseEntering, st.Phase)
	assert.Empty(t, st.WaitingFor, "古い opId の判定が進み具合を書いた")

	moved, err = s.markHeld(ctx, "new", at, nil)
	require.NoError(t, err)
	assert.True(t, moved)
	st, _ = s.Get(ctx)
	assert.Equal(t, PhaseHeld, st.Phase)
	assert.Equal(t, at, st.HeldAt)
	// held から held にはしない (entering のときだけ)。
	moved, err = s.markHeld(ctx, "new", at, nil)
	require.NoError(t, err)
	assert.False(t, moved)

	// resuming では進み具合を書かない。
	require.NoError(t, s.SetPhase(ctx, "new", PhaseResuming))
	require.NoError(t, s.progress(ctx, "new", at, []string{"y"}, nil, nil, ""))
	st, _ = s.Get(ctx)
	assert.Empty(t, st.WaitingFor)

	// resuming のときだけ、同じ opId なら消す。
	cleared, err := s.clearResumed(ctx, "old")
	require.NoError(t, err)
	assert.False(t, cleared)
	cleared, err = s.clearResumed(ctx, "new")
	require.NoError(t, err)
	assert.True(t, cleared)
	st, _ = s.Get(ctx)
	assert.Nil(t, st)

	require.NoError(t, s.Enter(ctx, State{OpID: "e"}))
	cleared, err = s.clearResumed(ctx, "e")
	require.NoError(t, err)
	assert.False(t, cleared, "entering のメンテナンスを抜けたことにした")
}

func TestStore_JudgeLock(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()
	holder, err := s.Judge(ctx)
	require.NoError(t, err)
	assert.Empty(t, holder)

	ok, err := s.acquireJudge(ctx, "a", JudgeLockTTL)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = s.acquireJudge(ctx, "b", JudgeLockTTL)
	require.NoError(t, err)
	assert.False(t, ok, "2 つ目の判定役がロックを取った")
	holder, _ = s.Judge(ctx)
	assert.Equal(t, "a", holder)

	// 持ち主が取り直すと延びる。
	mr.FastForward(JudgeLockTTL - time.Second)
	ok, err = s.acquireJudge(ctx, "a", JudgeLockTTL)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, JudgeLockTTL, mr.TTL(s.JudgeKey()))

	// 持ち主でない側は消せない。
	require.NoError(t, s.releaseJudge(ctx, "b"))
	holder, _ = s.Judge(ctx)
	assert.Equal(t, "a", holder)
	require.NoError(t, s.releaseJudge(ctx, "a"))
	holder, _ = s.Judge(ctx)
	assert.Empty(t, holder)

	// 期限が切れたら、ほかの判定役が取れる。
	ok, _ = s.acquireJudge(ctx, "a", JudgeLockTTL)
	require.True(t, ok)
	mr.FastForward(JudgeLockTTL + time.Second)
	ok, err = s.acquireJudge(ctx, "b", JudgeLockTTL)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestStore_JudgeErrorsSurface(t *testing.T) {
	s, mr := newTestStore(t)
	mr.Close()
	ctx := context.Background()
	_, err := s.acquireJudge(ctx, "a", time.Second)
	assert.Error(t, err)
	_, err = s.Judge(ctx)
	assert.Error(t, err)
	_, err = s.markHeld(ctx, "op", time.Now(), nil)
	assert.Error(t, err)
	_, err = s.clearResumed(ctx, "op")
	assert.Error(t, err)
	assert.Error(t, s.progress(ctx, "op", time.Now(), nil, nil, nil, ""))
}

func TestMustJSON(t *testing.T) {
	assert.Equal(t, "[]", mustJSON([]string(nil)))
	assert.Equal(t, `["a"]`, mustJSON([]string{"a"}))
	assert.Equal(t, "[]", mustJSON(func() {}))
}
