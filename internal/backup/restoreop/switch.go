package restoreop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/maintmode"
)

// stopping reports whether the service is stopping.
//
// 止められた段は、失敗として扱わずに記録を進めないまま返る。失敗として扱うと、
// サービスを止めただけで中止・戻す・諦める (unknown) へ進んでしまう。記録は最後に
// 書いた段のまま残り、起動し直したときに Recover が名前と状態から続きを決める。
// 確かめるのは、失敗を受けて記録を進める入口 (abortBeforeSwitch・revert・giveUp)
// にまとめる。どの段の失敗も、このどれかを通る。
func stopping(ctx context.Context) bool { return ctx.Err() != nil }

// runJudge runs the judge until the returned function is called.
func (m *Manager) runJudge(ctx context.Context) func() {
	if m.o.RunJudge == nil {
		return func() {}
	}
	jctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.o.RunJudge(jctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// waitResult is how a wait on the maintenance state ended.
type waitResult int

const (
	waitReached waitResult = iota
	// waitGone: the state is gone or belongs to another operation.
	waitGone
	waitTimeout
	waitCancelled
)

// waitPhase waits until the maintenance of opID is at phase.
func (m *Manager) waitPhase(ctx context.Context, opID, phase string, timeout time.Duration) waitResult {
	deadline := m.now().Add(timeout)
	for {
		st, err := m.o.Maint.Get(ctx)
		switch {
		case err != nil:
			// 読めないだけなら待ち続ける (Redis の瞬断)。
		case st == nil || st.OpID != opID:
			return waitGone
		case st.Phase == phase:
			return waitReached
		}
		if !m.now().Before(deadline) {
			return waitTimeout
		}
		select {
		case <-ctx.Done():
			return waitCancelled
		case <-time.After(m.o.Poll):
		}
	}
}

// waitCleared waits until the maintenance of opID is over (the judge clears
// it once every process serves again).
func (m *Manager) waitCleared(ctx context.Context, opID string, timeout time.Duration) waitResult {
	deadline := m.now().Add(timeout)
	for {
		st, err := m.o.Maint.Get(ctx)
		if err == nil && (st == nil || st.OpID != opID) {
			return waitReached
		}
		if !m.now().Before(deadline) {
			return waitTimeout
		}
		select {
		case <-ctx.Done():
			return waitCancelled
		case <-time.After(m.o.Poll):
		}
	}
}

// switchOver runs the confirmed operation: C3 to C10 of the design.
//
// ctx はサービスの寿命。止められたら (waitCancelled) 何もせずに返る。記録は
// 最後に書いた段のまま残り、起動し直したときに Recover が続きを決める。
func (m *Manager) switchOver(ctx context.Context, rec *Record) {
	stopJudge := m.runJudge(ctx)
	defer stopJudge()

	// C3: メンテナンスに入れる。
	err := m.o.Maint.Enter(ctx, maintmode.State{
		OpID: rec.OpID, Kind: maintmode.KindRestore, RequestedBy: rec.UserID, StartedAt: m.now(),
	})
	if err != nil {
		// 書き込みは済んだのに応答を読めなかっただけのことがある。読み直して、この操作の
		// 状態が残っていれば戻してから中止する (残したまま aborted にすると、誰も外さない)。
		st, gerr := m.o.Maint.Get(ctx)
		if gerr != nil {
			m.giveUp(ctx, rec, StepEntering, fmt.Errorf("enter the maintenance: %w; then read it: %v", err, gerr))
			return
		}
		m.abortBeforeSwitch(ctx, rec, fmt.Errorf("enter the maintenance: %w", err), st != nil && st.OpID == rec.OpID)
		return
	}
	m.journal(ctx, rec.OpID, StepEntering, "waiting for every process to let go of %s", rec.Database)

	// C4: held を待つ。held は判定役が pg_stat_activity で数えた結果。
	switch m.waitPhase(ctx, rec.OpID, maintmode.PhaseHeld, m.o.HeldTimeout) {
	case waitCancelled:
		return
	case waitGone:
		m.abortBeforeSwitch(ctx, rec, errors.New("the maintenance was ended by someone else"), false)
		return
	case waitTimeout:
		m.abortBeforeSwitch(ctx, rec, fmt.Errorf("the processes did not let go of the database within %s", m.o.HeldTimeout), true)
		return
	}
	m.journal(ctx, rec.OpID, StepEntering, "held; switching the databases")

	// C5: 名前を入れ替える。SwitchDatabase は入れ替える直前に、もう一度
	// pg_stat_activity で接続が無いことを確かめる (held は根拠にしない)。
	err = m.o.DB.SwitchDatabase(ctx, m.o.MaintenanceDB, rec.Database, rec.Before, rec.Target)
	switch {
	case errors.Is(err, backup.ErrRestoreSwitchUnknown):
		m.giveUp(ctx, rec, StepEntering, err)
		return
	case err != nil:
		m.abortBeforeSwitch(ctx, rec, err, true)
		return
	}
	m.record(ctx, rec.OpID, StepEntering, StepSwitched)
	m.journal(ctx, rec.OpID, StepSwitched, "%s is now the restored database; the previous one is %s", rec.Database, rec.Before)

	// C6 / C7: Redis の後始末と検査。
	if err := m.cleanAndCheck(ctx); err != nil {
		m.revert(ctx, rec, StepSwitched, err)
		return
	}
	// C8
	m.modlog(ctx, rec.Database, rec, "switch")

	// C9 / C10
	m.record(ctx, rec.OpID, StepSwitched, StepResuming)
	if err := m.o.Maint.SetPhase(ctx, rec.OpID, maintmode.PhaseResuming); err != nil {
		m.revert(ctx, rec, StepResuming, fmt.Errorf("resume: %w", err))
		return
	}
	m.awaitResume(ctx, rec)
}

// awaitResume waits for the processes to serve on the restored database and
// puts the previous one back when they do not within ResumeTimeout (A3).
func (m *Manager) awaitResume(ctx context.Context, rec *Record) {
	m.journal(ctx, rec.OpID, StepResuming, "waiting for every process to serve again")
	switch m.waitCleared(ctx, rec.OpID, m.o.ResumeTimeout) {
	case waitCancelled:
		return
	case waitTimeout:
		m.revert(ctx, rec, StepResuming, fmt.Errorf("the processes did not serve again within %s", m.o.ResumeTimeout))
		return
	}
	m.record(ctx, rec.OpID, StepResuming, StepDone)
	m.journal(ctx, rec.OpID, StepDone, "restored generation %s; the previous database is kept as %s", rec.GenerationID, rec.Before)
}

func (m *Manager) cleanAndCheck(ctx context.Context) error {
	if m.o.CleanRedis != nil {
		if err := m.o.CleanRedis(ctx); err != nil {
			return fmt.Errorf("clean redis: %w", err)
		}
	}
	if m.o.Check != nil {
		if err := m.o.Check(ctx); err != nil {
			return fmt.Errorf("check the restored database: %w", err)
		}
	}
	return nil
}

// abortBeforeSwitch ends an operation whose names were not swapped (A1): the
// processes resume on the current database and the new one is dropped.
func (m *Manager) abortBeforeSwitch(ctx context.Context, rec *Record, cause error, inMaintenance bool) {
	if stopping(ctx) {
		return
	}
	m.journal(ctx, rec.OpID, StepEntering, "not switching: %v", cause)
	m.record(ctx, rec.OpID, "", StepAborting, "error", cause.Error())
	if inMaintenance {
		m.resumeCurrent(ctx, rec)
		if stopping(ctx) {
			// aborting のまま残す。Recover がもう一度戻してから消す。
			return
		}
	}
	m.dropTarget(ctx, rec)
	m.record(ctx, rec.OpID, StepAborting, StepAborted)
	m.journal(ctx, rec.OpID, StepAborted, "the current database %s was not changed", rec.Database)
}

// resumeCurrent lets the processes come back and waits for them (best
// effort: they keep retrying on their own).
func (m *Manager) resumeCurrent(ctx context.Context, rec *Record) {
	if err := m.o.Maint.SetPhase(ctx, rec.OpID, maintmode.PhaseResuming); err != nil {
		if !errors.Is(err, maintmode.ErrNotActive) && !stopping(ctx) {
			m.journal(ctx, rec.OpID, "", "could not end the maintenance: %v", err)
		}
		return
	}
	if m.waitCleared(ctx, rec.OpID, m.o.ResumeTimeout) == waitTimeout {
		m.journal(ctx, rec.OpID, "", "the processes have not served again within %s; the maintenance stays at resuming and they keep retrying", m.o.ResumeTimeout)
	}
}

// revert puts the previous database back after the names were swapped (A2,
// and A3 when the processes already came back).
func (m *Manager) revert(ctx context.Context, rec *Record, from string, cause error) {
	if stopping(ctx) {
		return
	}
	m.journal(ctx, rec.OpID, from, "putting the previous database back: %v", cause)
	aside := rec.Aside
	if aside == "" {
		aside = rec.Database + "_rolled_back_" + m.now().UTC().Format("20060102150405")
	}
	m.record(ctx, rec.OpID, "", StepReverting, "aside", aside, "error", cause.Error())
	rec.Aside = aside
	m.revertSwitch(ctx, rec)
}

// revertSwitch swaps the names back once every process has let go again.
func (m *Manager) revertSwitch(ctx context.Context, rec *Record) {
	if !m.holdAgain(ctx, rec) {
		return
	}
	err := m.o.DB.SwitchDatabase(ctx, m.o.MaintenanceDB, rec.Database, rec.Aside, rec.Before)
	if err != nil {
		m.giveUp(ctx, rec, StepReverting, fmt.Errorf("put %s back: %w", rec.Before, err))
		return
	}
	m.finishRevert(ctx, rec)
}

// holdAgain brings the maintenance of rec back to held. It reports false
// when it gave up (or the service is stopping).
func (m *Manager) holdAgain(ctx context.Context, rec *Record) bool {
	st, err := m.o.Maint.Get(ctx)
	if err != nil {
		m.giveUp(ctx, rec, StepReverting, fmt.Errorf("read the maintenance: %w", err))
		return false
	}
	if st == nil || st.OpID != rec.OpID {
		// 状態が消えている。プロセスは戻した DB で動いているので、ここで入れ替えると
		// 書き込みを失う。運営者に任せる。
		m.giveUp(ctx, rec, StepReverting, errors.New("the maintenance is no longer active, so the processes may be serving the restored database"))
		return false
	}
	if st.Phase != maintmode.PhaseHeld {
		if st.Phase == maintmode.PhaseResuming {
			if err := m.o.Maint.SetPhase(ctx, rec.OpID, maintmode.PhaseEntering); err != nil {
				m.giveUp(ctx, rec, StepReverting, fmt.Errorf("enter the maintenance again: %w", err))
				return false
			}
		}
		switch m.waitPhase(ctx, rec.OpID, maintmode.PhaseHeld, m.o.HeldTimeout) {
		case waitCancelled:
			return false
		case waitReached:
		default:
			m.giveUp(ctx, rec, StepReverting, fmt.Errorf("the processes did not let go of the restored database within %s", m.o.HeldTimeout))
			return false
		}
	}
	return true
}

// finishRevert cleans Redis again and lets the processes resume on the
// previous database.
//
// 再開 (resuming) を先に出してから reverted を書く。逆だと、その間で止められた
// ときに最終の段だけが残り、メンテナンスが外れなくなる。
func (m *Manager) finishRevert(ctx context.Context, rec *Record) {
	st, err := m.o.Maint.Get(ctx)
	if err != nil {
		m.giveUp(ctx, rec, StepReverting, fmt.Errorf("read the maintenance: %w", err))
		return
	}
	held := st != nil && st.OpID == rec.OpID && st.Phase != maintmode.PhaseResuming
	m.journal(ctx, rec.OpID, StepReverting, "%s is the previous database again; the restored one is kept as %s", rec.Database, rec.Aside)
	if held && m.o.CleanRedis != nil {
		// 戻した DB で動いた間に Redis に積まれたものを消す。プロセスが元の DB で
		// 動き始めた後 (resuming や状態が消えた後) に消すと、元の DB の分まで消す
		// ので、止まっている間だけにする。失敗しても元の DB へは戻っているので、
		// 止めずに記録だけ残す。
		if err := m.o.CleanRedis(ctx); err != nil {
			m.journal(ctx, rec.OpID, StepReverting, "could not clean redis: %v", err)
		}
	}
	if held {
		if err := m.o.Maint.SetPhase(ctx, rec.OpID, maintmode.PhaseResuming); err != nil && !errors.Is(err, maintmode.ErrNotActive) {
			m.giveUp(ctx, rec, StepReverting, fmt.Errorf("resume: %w", err))
			return
		}
	}
	m.record(ctx, rec.OpID, StepReverting, StepReverted)
	if m.waitCleared(ctx, rec.OpID, m.o.ResumeTimeout) == waitTimeout {
		m.journal(ctx, rec.OpID, "", "the processes have not served again within %s; the maintenance stays at resuming and they keep retrying", m.o.ResumeTimeout)
	}
	m.journal(ctx, rec.OpID, StepReverted, "reverted to the previous database")
}

// giveUp stops and leaves the decision to an operator (it cannot tell the
// state of the names, or cannot get the processes off the database).
//
// 勧める手順は、メンテナンスが残っているかで変える。残っていないときに
// -rollback だけを勧めると、プロセスが動いている DB の名前を変えさせることになる
// (接続があれば PostgreSQL が拒むが、運営者が接続を切ってから試しかねない)。
func (m *Manager) giveUp(ctx context.Context, rec *Record, from string, cause error) {
	if stopping(ctx) {
		return
	}
	names := fmt.Sprintf("Check pg_database for %s, %s, %s and %s", rec.Database, rec.Target, rec.Before, rec.Aside)
	var msg string
	st, err := m.o.Maint.Get(ctx)
	switch {
	case err != nil:
		msg = fmt.Sprintf("restore %s stopped: %v. It could not read the maintenance state (%v); check it with elythia maintenance status. %s. "+
			"With the maintenance on, put the right database under %s (elythia backup restore -rollback), then end the maintenance (elythia maintenance exit)",
			rec.OpID, cause, err, names, rec.Database)
	case st != nil && st.OpID == rec.OpID:
		msg = fmt.Sprintf("restore %s stopped and left the maintenance on: %v. %s, "+
			"put the right database under %s (elythia backup restore -rollback), then end the maintenance (elythia maintenance exit)",
			rec.OpID, cause, names, rec.Database)
	default:
		msg = fmt.Sprintf("restore %s stopped: %v. The maintenance is not on, so the processes serve the database named %s now. %s. "+
			"To put another database under %s, enter the maintenance first (elythia maintenance enter), then elythia backup restore -rollback, then elythia maintenance exit",
			rec.OpID, cause, rec.Database, names, rec.Database)
	}
	m.record(ctx, rec.OpID, "", StepUnknown, "error", cause.Error())
	m.journal(ctx, rec.OpID, from, "%s", msg)
	m.log.Error("backup: " + msg)
	if m.o.Notify != nil {
		m.o.Notify(context.WithoutCancel(ctx), msg)
	}
}
