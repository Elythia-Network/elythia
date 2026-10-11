package restoreop

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/maintmode"
)

// hookMaint wraps the maintenance store to fail chosen calls.
type hookMaint struct {
	*maintmode.Store
	mu      sync.Mutex
	failGet error
	failRep error
	failSet func(phase string) error
	// failGets fails that many Gets before they succeed again.
	failGets int
	// afterEnter runs after a successful Enter; its error is returned.
	afterEnter func() error
	// failEnter fails Enter without writing.
	failEnter error
	// failClear fails Clear.
	failClear error
	// late is a state written right after the next Get or Clear returns,
	// like an Enter whose write arrives late.
	late *maintmode.State
}

// writeLate applies the late write once.
func (h *hookMaint) writeLate(ctx context.Context) {
	h.mu.Lock()
	st := h.late
	h.late = nil
	h.mu.Unlock()
	if st != nil {
		_ = h.Store.Enter(ctx, *st)
	}
}

func (h *hookMaint) Clear(ctx context.Context, opID string) error {
	h.mu.Lock()
	err := h.failClear
	h.mu.Unlock()
	if err != nil {
		return err
	}
	defer h.writeLate(ctx)
	return h.Store.Clear(ctx, opID)
}

func (h *hookMaint) Enter(ctx context.Context, st maintmode.State) error {
	h.mu.Lock()
	ferr := h.failEnter
	h.mu.Unlock()
	if ferr != nil {
		return ferr
	}
	if err := h.Store.Enter(ctx, st); err != nil {
		return err
	}
	h.mu.Lock()
	f := h.afterEnter
	h.mu.Unlock()
	if f != nil {
		return f()
	}
	return nil
}

func (h *hookMaint) Get(ctx context.Context) (*maintmode.State, error) {
	h.mu.Lock()
	err := h.failGet
	if err == nil && h.failGets > 0 {
		h.failGets--
		err = errors.New("redis: i/o timeout")
	}
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	defer h.writeLate(ctx)
	return h.Store.Get(ctx)
}

func (h *hookMaint) Reports(ctx context.Context) ([]maintmode.ProcReport, error) {
	if h.failRep != nil {
		return nil, h.failRep
	}
	return h.Store.Reports(ctx)
}

func (h *hookMaint) SetPhase(ctx context.Context, opID, phase string) error {
	h.mu.Lock()
	f := h.failSet
	h.mu.Unlock()
	if f != nil {
		if err := f(phase); err != nil {
			return err
		}
	}
	return h.Store.SetPhase(ctx, opID, phase)
}

func withHook(h **hookMaint) func(*Options) {
	return func(o *Options) {
		*h = &hookMaint{Store: o.Maint.(*maintmode.Store)}
		o.Maint = *h
	}
}

func TestRestore_PrepareStoreErrors(t *testing.T) {
	ctx := context.Background()
	req := PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"}
	var h *hookMaint
	e := newEnv(t, withHook(&h))
	h.failGet = errors.New("redis down")
	_, _, err := e.m.Prepare(ctx, req)
	assert.ErrorContains(t, err, "redis down")
	h.failGet = nil
	h.failRep = errors.New("reports down")
	_, _, err = e.m.Prepare(ctx, req)
	assert.ErrorContains(t, err, "reports down")
	h.failRep = nil

	// 印だけが残り、記録が無い。勝手に消さずに知らせる。
	require.NoError(t, e.rdb.Set(ctx, e.store.CurrentKey(), "restore-gone", 0).Err())
	_, _, err = e.m.Prepare(ctx, req)
	assert.ErrorContains(t, err, "has no record")
	require.NoError(t, e.rdb.Del(ctx, e.store.CurrentKey()).Err())

	// Redis が落ちていれば、記録を作れずに断る (何も作らない)。
	e.mr.Close()
	_, _, err = e.m.Prepare(ctx, req)
	assert.Error(t, err)
	assert.False(t, e.slot.isHeld())
}

func TestRestore_SomeoneEndsTheMaintenanceBeforeHeld(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.HeldTimeout = 10 * time.Second })
	e.process("p1")
	e.db.mu.Lock()
	e.db.conns[testDB] = 1
	e.db.mu.Unlock()
	rec, _ := e.prepare()
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	require.Eventually(t, func() bool { st := e.maintenance(); return st != nil && st.OpID == rec.OpID }, 2*time.Second, 5*time.Millisecond)
	require.NoError(t, e.maint.Clear(context.Background(), rec.OpID))
	got := e.waitStep(rec.OpID, StepAborted)
	assert.Contains(t, got.Error, "ended by someone else")
	assert.Equal(t, "current", e.db.content(testDB))
}

func TestRestore_StoppedWhileWaitingThenRecovered(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.HeldTimeout = time.Hour })
	e.process("p1")
	e.db.mu.Lock()
	e.db.conns[testDB] = 1
	e.db.mu.Unlock()
	rec, _ := e.prepare()
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	require.Eventually(t, func() bool { st := e.maintenance(); return st != nil && st.OpID == rec.OpID }, 2*time.Second, 5*time.Millisecond)
	// サービスが止められた。記録は entering のまま、メンテナンスも残る。
	e.stop()
	got, err := e.store.Get(context.Background(), rec.OpID)
	require.NoError(t, err)
	assert.Equal(t, StepEntering, got.Step)
	assert.NotNil(t, e.maintenance())
	// 起動し直すと、名前を入れ替えていないので元の DB で戻す。
	e.db.mu.Lock()
	e.db.conns[testDB] = 0
	e.db.mu.Unlock()
	e.start()
	e.waitStep(rec.OpID, StepAborted)
	assert.Equal(t, "current", e.db.content(testDB))
	assert.NotContains(t, e.db.names(), rec.Target)
	require.Eventually(t, func() bool { return e.maintenance() == nil }, 2*time.Second, 5*time.Millisecond)
}

func TestRestore_StoppedWhileReady(t *testing.T) {
	e := newEnv(t)
	rec, _ := e.prepare()
	e.stop()
	got, err := e.store.Get(context.Background(), rec.OpID)
	require.NoError(t, err)
	assert.Equal(t, StepReady, got.Step, "stopping does not drop; the next start does")
	e.start()
	e.waitStep(rec.OpID, StepAborted)
	assert.NotContains(t, e.db.names(), rec.Target)
}

func TestRestore_ResumeCannotBeRequested(t *testing.T) {
	var h *hookMaint
	e := newEnv(t, withHook(&h))
	e.process("p1")
	rec, _ := e.prepare()
	var once sync.Once
	h.failSet = func(phase string) error {
		var err error
		if phase == maintmode.PhaseResuming {
			once.Do(func() { err = errors.New("redis blip") })
		}
		return err
	}
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	got := e.waitStep(rec.OpID, StepReverted)
	assert.Contains(t, got.Error, "redis blip")
	assert.Equal(t, "current", e.db.content(testDB))
}

func TestRestore_RevertCannotSwapBack(t *testing.T) {
	// 戻した後の検査が落ち、そのうえ元へ戻す入れ替えも断られた (接続が戻ってきた)。
	e := newEnv(t)
	e.process("p1")
	e.opts.Check = func(context.Context) error {
		e.db.mu.Lock()
		e.db.conns[testDB] = 1
		e.db.mu.Unlock()
		return errors.New("doctor failed")
	}
	e.stop()
	e.start()
	rec, _ := e.prepare()
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	got := e.waitStep(rec.OpID, StepUnknown)
	assert.Contains(t, got.Error, "put "+rec.Before+" back")
	assert.Equal(t, "restored:20261010T120000Z", e.db.content(testDB), "nothing guessed")
	assert.NotNil(t, e.maintenance())
}

func TestRestore_RevertCannotHoldAgain(t *testing.T) {
	// 戻した DB で再開できず (A3)、入り直しても手放されない。
	e := newEnv(t)
	p := e.process("p1")
	p.setFailBack(true)
	rec, _ := e.prepare()
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	// 接続は戻す処理に入る前 (再開を待つ間) に残す。reverting を見てからでは、判定役が
	// 先に held にして入れ替えに進むことがある。
	e.waitStep(rec.OpID, StepResuming)
	e.db.mu.Lock()
	e.db.conns[testDB] = 1
	e.db.mu.Unlock()
	got := e.waitStep(rec.OpID, StepUnknown)
	assert.Contains(t, got.Error, "did not let go of the restored database")
	assert.Equal(t, "restored:20261010T120000Z", e.db.content(testDB))
}

func TestRestore_RevertCannotEnterAgain(t *testing.T) {
	var h *hookMaint
	e := newEnv(t, withHook(&h))
	p := e.process("p1")
	p.setFailBack(true)
	h.failSet = func(phase string) error {
		if phase == maintmode.PhaseEntering {
			return errors.New("cannot write")
		}
		return nil
	}
	rec, _ := e.prepare()
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	got := e.waitStep(rec.OpID, StepUnknown)
	assert.Contains(t, got.Error, "enter the maintenance again")
}

func TestRestore_RevertCannotReadTheMaintenance(t *testing.T) {
	var h *hookMaint
	e := newEnv(t, withHook(&h))
	e.process("p1")
	e.db.fail["check"] = errors.New("doctor failed")
	e.opts.CleanRedis = func(context.Context) error {
		h.mu.Lock()
		h.failGet = errors.New("redis down")
		h.mu.Unlock()
		return nil
	}
	e.stop()
	e.start()
	rec, _ := e.prepare()
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	got := e.waitStep(rec.OpID, StepUnknown)
	assert.Contains(t, got.Error, "read the maintenance")
}

func TestRestore_AbortResumeTimesOut(t *testing.T) {
	// 中止 (A1) の後、プロセスが戻らない。待ちきれなくても DB は消し、記録は残す。
	e := newEnv(t)
	p := e.process("p1")
	e.db.mu.Lock()
	e.db.conns[testDB] = 1
	e.db.mu.Unlock()
	rec, _ := e.prepare()
	p.setFailBack(true)
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	e.waitStep(rec.OpID, StepAborted)
	journal, err := e.store.Journal(context.Background(), rec.OpID)
	require.NoError(t, err)
	found := false
	for _, j := range journal {
		if j.Message != "" && containsAll(j.Message, "have not served again") {
			found = true
		}
	}
	assert.True(t, found, "the journal says the processes have not come back")
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

func TestRestore_DropFailureIsJournaled(t *testing.T) {
	e := newEnv(t)
	e.opts.ModLog = func(context.Context, string, string, map[string]any) error { return errors.New("db down") }
	e.stop()
	e.start()
	e.db.fail["restoreInto"] = errors.New("pg_restore failed")
	e.db.fail["drop"] = errors.New("drop failed")
	rec, _, err := e.m.Prepare(context.Background(), PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"})
	require.NoError(t, err)
	e.waitStep(rec.OpID, StepFailed)
	journal, err := e.store.Journal(context.Background(), rec.OpID)
	require.NoError(t, err)
	var msgs []string
	for _, j := range journal {
		msgs = append(msgs, j.Message)
	}
	assert.Contains(t, msgs, "could not drop "+rec.Target+": drop failed")
}

func TestStatus(t *testing.T) {
	e := newEnv(t)
	rec, _ := e.prepare()
	ctx := context.Background()
	st, err := e.m.Status(ctx, rec)
	require.NoError(t, err)
	assert.Nil(t, st.Maintenance)
	assert.False(t, st.Final)
	require.NoError(t, e.maint.Enter(ctx, maintmode.State{OpID: "other", Kind: maintmode.KindManual}))
	st, err = e.m.Status(ctx, rec)
	require.NoError(t, err)
	assert.Nil(t, st.Maintenance, "another maintenance is not shown as this one's")
	e.mr.Close()
	_, err = e.m.Status(ctx, rec)
	assert.Error(t, err)
}

func TestRecordHelpers(t *testing.T) {
	assert.True(t, parseTime("bad").IsZero())
	assert.True(t, parseTime("").IsZero())
	assert.Equal(t, "", formatTime(time.Time{}))
	e := newEnv(t)
	ctx := context.Background()
	require.NoError(t, e.rdb.RPush(ctx, e.store.journal("x"), "not json", `{"step":"ready","message":"ok"}`).Err())
	j, err := e.store.Journal(ctx, "x")
	require.NoError(t, err)
	require.Len(t, j, 1)
	assert.Equal(t, "ok", j[0].Message)
	assert.ErrorIs(t, e.store.setStep(ctx, "x", "", StepDone, time.Now()), ErrNotFound)
}

func TestRestore_ExpiryLosesToAConfirmation(t *testing.T) {
	// 期限と確定が重なったら、先に段を進めた方だけが動く。
	e := newEnv(t, func(o *Options) { o.ReadyTimeout = 50 * time.Millisecond })
	rec, _, err := e.m.Prepare(context.Background(), PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"})
	require.NoError(t, err)
	e.waitStep(rec.OpID, StepReady)
	require.NoError(t, e.store.setStep(context.Background(), rec.OpID, StepReady, StepEntering, time.Now()))
	time.Sleep(150 * time.Millisecond)
	got, err := e.store.Get(context.Background(), rec.OpID)
	require.NoError(t, err)
	assert.Equal(t, StepEntering, got.Step)
	assert.Empty(t, e.db.drops, "the expiry does not drop a database another step owns")
}

func TestRestore_RecordFailureIsLogged(t *testing.T) {
	e := newEnv(t)
	// 記録の無い opId へ書いても、止まらずにログだけを残す。
	e.m.record(context.Background(), "restore-none", "", StepDone)
	e.m.journal(context.Background(), "restore-none", StepDone, "x")
	e.mr.Close()
	e.m.journal(context.Background(), "restore-none", StepDone, "redis is down")
	assert.Error(t, e.store.appendJournal(context.Background(), "restore-none", JournalEntry{}))
	_, err := e.store.Get(context.Background(), "restore-none")
	assert.Error(t, err)
	_, err = e.store.Journal(context.Background(), "restore-none")
	assert.Error(t, err)
	assert.Error(t, e.store.create(context.Background(), &Record{OpID: "restore-new"}))
}

func TestRestore_AbortResumeCannotBeRequested(t *testing.T) {
	// 中止 (A1) で resuming を書けなくても、作った DB は消す。
	var h *hookMaint
	e := newEnv(t, withHook(&h))
	e.process("p1")
	e.db.mu.Lock()
	e.db.conns[testDB] = 1
	e.db.mu.Unlock()
	h.failSet = func(phase string) error {
		if phase == maintmode.PhaseResuming {
			return errors.New("cannot write")
		}
		return nil
	}
	rec, _ := e.prepare()
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	e.waitStep(rec.OpID, StepAborted)
	assert.NotContains(t, e.db.names(), rec.Target)
	journal, err := e.store.Journal(context.Background(), rec.OpID)
	require.NoError(t, err)
	var msgs []string
	for _, j := range journal {
		msgs = append(msgs, j.Message)
	}
	assert.Contains(t, msgs, "could not end the maintenance: cannot write")
}

func TestRestore_AbortRaces(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	rec, _ := e.prepare()
	// 別の経路が先に段を進めた (期限や確定)。中止はそれを上書きしない。
	require.NoError(t, e.store.setStep(ctx, rec.OpID, StepReady, StepAborting, time.Now()))
	assert.ErrorIs(t, e.m.Abort(ctx, rec.OpID), ErrStep)
	// この実行のサービスが準備していない preparing の記録は中止できない。
	require.NoError(t, e.store.setStep(ctx, rec.OpID, "", StepPreparing, time.Now()))
	e.m.finishLive(rec.OpID)
	assert.ErrorIs(t, e.m.Abort(ctx, rec.OpID), ErrStep)
}
