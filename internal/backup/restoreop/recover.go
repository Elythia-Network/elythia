package restoreop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/elythia-network/elythia/internal/maintmode"
)

// recoverOnStart finishes an operation an earlier run of the service left
// unfinished (it was stopped or crashed in the middle).
//
// 段の記録と pg_database の名前の組から、どこまで進んだかを決める。入れ替えの後で
// 落ちたなら元の DB へ戻す (v1 §7-8 の決定)。決められないときは何もせず、
// メンテナンスのまま運営者に渡す。
func (m *Manager) recoverOnStart(ctx context.Context) {
	rec, err := m.o.Store.Current(ctx)
	if err != nil {
		m.log.Error("backup: cannot read the unfinished restore", "err", err)
		return
	}
	if rec == nil {
		return
	}
	// 起動した直後は、検証されていない最新の世代を確かめる job が先に枠を取って
	// いることがある。取れるまで待つ (諦めると、メンテナンスのまま残る)。
	var release func()
	for {
		release, err = m.o.Acquire()
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(m.o.Poll):
		}
	}
	defer release()
	// 待つ間に、この実行で受けた準備がその記録を作って終えていることがある (起動
	// した直後に Current を読む前に Prepare が来た場合)。枠を取った後で読み直し、
	// 終わっていれば何もしない。この実行の操作は終わるまで枠を持つので、枠を取れた
	// 時点で残っている記録は、前の実行のものだけになる。
	rec, err = m.o.Store.Current(ctx)
	if err != nil {
		m.log.Error("backup: cannot read the unfinished restore", "err", err)
		return
	}
	if rec == nil {
		return
	}
	stopJudge := m.runJudge(ctx)
	defer stopJudge()
	m.journal(ctx, rec.OpID, rec.Step, "the backup service restarted at step %s; recovering", rec.Step)
	m.recoverRecord(ctx, rec)
}

func (m *Manager) recoverRecord(ctx context.Context, rec *Record) {
	switch rec.Step {
	case StepPreparing, StepReady, StepAborting:
		// まだ名前を入れ替えていない。メンテナンスに入れたまま落ちていれば (中止の
		// 途中など) 元の DB で戻し、作った DB を消す。
		st, err := m.o.Maint.Get(ctx)
		if err != nil {
			m.giveUp(ctx, rec, rec.Step, fmt.Errorf("read the maintenance: %w", err))
			return
		}
		if st != nil && st.OpID == rec.OpID {
			m.resumeCurrent(ctx, rec)
			if stopping(ctx) {
				return
			}
		}
		m.dropTarget(ctx, rec)
		final := StepAborted
		if rec.Step == StepPreparing {
			final = StepFailed
		}
		m.record(ctx, rec.OpID, rec.Step, final, "error", "the backup service restarted before the switch")
		m.journal(ctx, rec.OpID, final, "dropped %s; the current database %s was not changed", rec.Target, rec.Database)
		return
	case StepResuming:
		st, err := m.o.Maint.Get(ctx)
		if err != nil {
			m.giveUp(ctx, rec, rec.Step, fmt.Errorf("read the maintenance: %w", err))
			return
		}
		if st == nil || st.OpID != rec.OpID {
			// 判定役が状態を消した後で落ちた。全プロセスが戻した DB で動いている。
			m.record(ctx, rec.OpID, StepResuming, StepDone)
			m.journal(ctx, rec.OpID, StepDone, "the processes already serve the restored database")
			return
		}
		if st.Phase != maintmode.PhaseResuming {
			// resuming を書いた後、状態を resuming にする前に止められた。
			if err := m.o.Maint.SetPhase(ctx, rec.OpID, maintmode.PhaseResuming); err != nil {
				m.giveUp(ctx, rec, rec.Step, fmt.Errorf("resume: %w", err))
				return
			}
		}
		m.awaitResume(ctx, rec)
		return
	}

	has, err := m.o.DB.ExistingDatabases(ctx, m.o.MaintenanceDB, []string{rec.Database, rec.Target, rec.Before, rec.Aside})
	if err != nil {
		m.giveUp(ctx, rec, rec.Step, fmt.Errorf("read pg_database: %w", err))
		return
	}
	swapped := has[rec.Before] && !has[rec.Target] && has[rec.Database]
	notSwapped := !has[rec.Before] && has[rec.Target] && has[rec.Database]
	switch rec.Step {
	case StepEntering:
		switch {
		case notSwapped:
			st, err := m.o.Maint.Get(ctx)
			if err != nil {
				m.giveUp(ctx, rec, rec.Step, fmt.Errorf("read the maintenance: %w", err))
				return
			}
			inMaintenance := st != nil && st.OpID == rec.OpID
			m.abortBeforeSwitch(ctx, rec, errors.New("the backup service restarted before the switch"), inMaintenance)
		case swapped:
			m.revert(ctx, rec, StepEntering, errors.New("the backup service restarted after the switch"))
		default:
			m.giveUp(ctx, rec, rec.Step, errors.New("the database names match neither before nor after the switch"))
		}
	case StepSwitched:
		if !swapped {
			m.giveUp(ctx, rec, rec.Step, errors.New("the database names do not match the recorded switch"))
			return
		}
		m.revert(ctx, rec, StepSwitched, errors.New("the backup service restarted after the switch"))
	case StepReverting:
		switch {
		case rec.Aside != "" && has[rec.Aside] && !has[rec.Before] && has[rec.Database]:
			// 戻す入れ替えは済んでいる。
			m.finishRevert(ctx, rec)
		case swapped && !has[rec.Aside]:
			m.revertSwitch(ctx, rec)
		default:
			m.giveUp(ctx, rec, rec.Step, errors.New("the database names match neither before nor after putting the previous database back"))
		}
	default:
		m.giveUp(ctx, rec, rec.Step, fmt.Errorf("unexpected step %q", rec.Step))
	}
}
