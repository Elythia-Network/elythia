package restoreop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/maintmode"
)

// recoverOnStart finishes an operation an earlier run of the service left
// unfinished (it was stopped or crashed in the middle).
//
// 段の記録と pg_database の名前の組から、どこまで進んだかを決める。入れ替えの後で
// 落ちたなら元の DB へ戻す (v1 §7-8 の決定)。決められないときは何もせず、
// メンテナンスのまま運営者に渡す。
func (m *Manager) recoverOnStart(ctx context.Context, held func()) {
	if m.o.Recovering != nil {
		m.o.Recovering(true)
		defer m.o.Recovering(false)
	}
	release := held
	defer func() {
		if release != nil {
			release()
		}
	}()
	var rec *Record
	logged := false
	for {
		r, err := m.o.Store.Current(ctx)
		if err == nil {
			if r == nil || release != nil {
				rec = r
				break
			}
			// 枠を先に預かっていなければ (Redis に後から繋がった場合など)、取れるまで
			// 待つ。先に動いている job があり得るので、諦めない (諦めると、メンテナンスの
			// まま残る)。取れたら記録を読み直す: 待つ間に、この実行で受けた準備がその
			// 記録を作って終えていることがある (起動した直後に Current を読む前に
			// Prepare が来た場合)。この実行の操作は終わるまで枠を持つので、枠を取れた
			// 時点で残っている記録は、前の実行のものだけになる。
			if acquired, aerr := m.o.Acquire(); aerr == nil {
				release = acquired
				continue
			}
		} else {
			// 読めない間は読み直し続ける (1 回で諦めると、メンテナンスのまま残った操作を
			// 起動し直すまで誰も片付けない)。預かった枠は返し、取る・確かめるを止めない。
			if !logged {
				m.log.Error("backup: cannot read the unfinished restore; retrying", "err", err)
				logged = true
			}
			if release != nil {
				release()
				release = nil
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(m.o.Poll):
		}
	}
	if rec == nil {
		return
	}
	stopJudge := m.runJudge(ctx)
	defer stopJudge()
	m.journal(ctx, rec.OpID, rec.Step, "the backup service restarted at step %s; recovering", rec.Step)
	m.recoverRecord(ctx, rec)
	m.settle(ctx, rec.OpID)
}

func (m *Manager) recoverRecord(ctx context.Context, rec *Record) {
	if err := checkRecordNames(rec, m.o.Database); err != nil {
		m.giveUp(ctx, rec, rec.Step, err)
		return
	}
	// **この操作が入れ替えている印が無ければ、名前を入れ替えた後の回復をしない。**
	// 入れ替える直前に本番へ進行中の印を付け (switch)、終えるとき・運営者に渡す
	// ときに「終わった」に書き換える。Redis の記録を途中の段へ書き戻されても、
	// 印は Redis の外にあるので、終わった操作や別の操作の退避した DB では巻き戻さ
	// ない。退避した DB がまだ無い (入れ替える前) なら、従来どおりに進める。
	switch rec.Step {
	case StepEntering, StepSwitched, StepResuming, StepReverting:
		state, op, exists, err := m.o.DB.OperationMark(ctx, m.o.MaintenanceDB, rec.Before)
		if err != nil {
			m.giveUp(ctx, rec, rec.Step, fmt.Errorf("read the mark of %s: %w", rec.Before, err))
			return
		}
		switch {
		case !exists:
		case op == rec.OpID && state == backup.MarkFinished:
			if st, err := m.readMaintenance(ctx); err == nil && st != nil && st.OpID == rec.OpID {
				m.resumeCurrent(ctx, rec)
			}
			m.record(ctx, rec.OpID, rec.Step, StepDone)
			m.journal(ctx, rec.OpID, StepDone, "%s is marked as kept by this finished operation; not reverting", rec.Before)
			return
		case op == rec.OpID && state == backup.MarkActive:
		case op == rec.OpID && state == backup.MarkHanded:
			m.giveUp(ctx, rec, rec.Step, fmt.Errorf("this operation was already handed to the operator (%s is marked as handed)", rec.Before))
			return
		default:
			m.giveUp(ctx, rec, rec.Step, fmt.Errorf("%s is not marked as being switched out by this operation", rec.Before))
			return
		}
	}
	switch rec.Step {
	case StepPreparing, StepReady, StepAborting:
		// まだ名前を入れ替えていない。メンテナンスに入れたまま落ちていれば (中止の
		// 途中など) 元の DB で戻し、作った DB を消す。
		st, err := m.readMaintenance(ctx)
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
		m.journal(ctx, rec.OpID, final, "%s; the current database %s was not changed", targetFate(rec), rec.Database)
		return
	case StepResuming:
		// 名前の組が入れ替えた後のものか確かめる。記録だけを信じると、戻す処理の記録が
		// 書けずに resuming のまま残った場合に、元の DB で再開させて done と書く。
		has, err := m.o.DB.ExistingDatabases(ctx, m.o.MaintenanceDB, []string{rec.Database, rec.Target, rec.Before})
		if err != nil {
			m.giveUp(ctx, rec, rec.Step, fmt.Errorf("read pg_database: %w", err))
			return
		}
		if !swappedNames(rec, has) {
			m.giveUp(ctx, rec, rec.Step, errors.New("the database names do not match the recorded switch"))
			return
		}
		st, err := m.readMaintenance(ctx)
		if err != nil {
			m.giveUp(ctx, rec, rec.Step, fmt.Errorf("read the maintenance: %w", err))
			return
		}
		if st == nil || st.OpID != rec.OpID {
			// 判定役が状態を消した後で落ちた。全プロセスが戻した DB で動いている。
			m.markFinished(ctx, rec)
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
	swapped := swappedNames(rec, has)
	notSwapped := !has[rec.Before] && has[rec.Target] && has[rec.Database]
	switch rec.Step {
	case StepEntering:
		switch {
		case notSwapped:
			st, err := m.readMaintenance(ctx)
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

// swappedNames reports whether pg_database has the names as they are after
// the switch: the restored database under Database and the previous one
// kept as Before.
func swappedNames(rec *Record, has map[string]bool) bool {
	return has[rec.Before] && !has[rec.Target] && has[rec.Database]
}

// maintenanceReadAttempts is how often the recovery reads the maintenance
// state before it gives up on it.
const maintenanceReadAttempts = 5

// readMaintenance reads the maintenance state, trying again a few times
// (Poll apart).
//
// 一度読めなかっただけで運営者に渡すと、Redis の瞬断でもメンテナンスのまま止まる。
// 上限を付けるのは、読めないまま進めて作った DB を消すことはしないため。
func (m *Manager) readMaintenance(ctx context.Context) (*maintmode.State, error) {
	var err error
	for i := 0; i < maintenanceReadAttempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(m.o.Poll):
			}
		}
		var st *maintmode.State
		if st, err = m.o.Maint.Get(ctx); err == nil {
			return st, nil
		}
	}
	return nil, err
}
