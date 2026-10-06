package mkqdriver

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/elythia-network/mkq"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// Scheduler implements driver.Scheduler over mkq's
// UpsertSchedulePattern API. mkq stores schedule registrations as a
// per-queue ZSET; subsequent Register calls with the same scheduleID
// idempotently replace the existing entry ("register-once" semantics).
//
// Limitations:
//
//   - mkq's scheduled-fire job inherits its BullMQ Job.name from the
//     queue name, not the scheduled task type. Server.dispatchHandler
//     reads framedPayload.Type instead of Job.name to work around
//     that — admin UI listings still show the queue name in the
//     Job.name column for scheduled fires.
//
//   - Retention (driver.WithKeepCompleted / WithKeepCompletedAge /
//     WithKeepFailed / WithKeepFailedAge) IS honoured since mkq v1.2.0
//     (mkq#109): it is translated to mkq's WithSchedule* options, which
//     write it both to the scheduler template (read by a BullMQ TS
//     worker) and to every iteration's job opts.
//
//   - mkq's ScheduleOption set does NOT cover other per-fire job options
//     like attempts / unique, so driver.WithMaxRetry / driver.WithUnique /
//     driver.WithProcessIn passed to Register are dropped. **これは
//     現状の呼び出し方では実害が無い** (#2405):
//
//     WithUnique が防ぎたい「同じ cron tick の二重実行」は、mkq が
//     発火 job に決定的な ID (`repeat:<scheduleID>:<nextMillis>`) を
//     振ることで構造的に防がれている。updateJobScheduler-12.lua が
//     `EXISTS` で弾き、重複時は `duplicated` イベントを記録する。
//     さらに `producerId == currentDelayedJobId` の判定により、直前の
//     発火を処理した worker だけが次を積める。
//
//     WithMaxRetry は mk-go の cron が全て 0 (リトライ無し) を渡して
//     おり、mkq の既定 MaxAttempts も 0 なので結果が同じ。
//     WithProcessIn は現在どの cron も渡していない。
//
//     したがって「この driver では cron が壊れる」ことは無い。将来
//     リトライさせたい cron や、mkq の dedup が効かない粒度で dedup
//     したい cron が出てきたら、その時点で bridge を検討する。
type Scheduler struct {
	driver *Driver

	mu sync.Mutex
	// attempted は Register を試みた (成功したかは問わない) schedule ID を
	// キューごとに持つ。PruneUnregistered はこれに無いものだけを消す。
	attempted map[string]map[string]struct{}
	// failed は Register が失敗した回数。1 件でもあれば撤去しない。
	failed int
}

// Register schedules taskType to fire on the given cron pattern. The
// scheduleID is taken from taskType so re-registering the same task
// at a different cron replaces (rather than duplicates) the entry.
//
// Retention options are honoured (see scheduleRetentionOptions).
// driver.WithMaxRetry, driver.WithUnique, and driver.WithProcessIn
// are accepted (the driver.Scheduler interface passes them through) but
// are NOT honoured — see the Scheduler doc-comment for why that is
// currently harmless.
//
// 以前はこれらが渡されるたびに起動時 warning を出していたが、実害が無い
// のに毎起動 11 件出て本物の警告を埋めるため落とした (#2405)。
func (s *Scheduler) Register(cronspec, taskType string, payload []byte, opts ...driver.EnqueueOption) error {
	o := driver.ApplyEnqueueOptions(opts)
	if o.Queue == "" {
		s.countFailure()
		return fmt.Errorf("mkqdriver: Scheduler.Register requires WithQueue (taskType=%q)", taskType)
	}
	q := s.driver.queueFor(o.Queue)
	if q == nil {
		s.countFailure()
		return fmt.Errorf("mkqdriver: unknown queue %q (taskType=%q)", o.Queue, taskType)
	}
	// **失敗しても「試みた」として記録する。** 一時的な Redis の失敗で登録
	// できなかった cron まで PruneUnregistered が消すと、再起動するまでその
	// cron が止まる。消すのは「今回のコードが登録しようともしなかったもの」だけ。
	s.mu.Lock()
	if s.attempted == nil {
		s.attempted = make(map[string]map[string]struct{})
	}
	if s.attempted[o.Queue] == nil {
		s.attempted[o.Queue] = make(map[string]struct{})
	}
	s.attempted[o.Queue][taskType] = struct{}{}
	s.mu.Unlock()

	framed := framedPayload{Type: taskType, Body: payload}
	if err := q.UpsertSchedulePattern(context.Background(), taskType, cronspec, framed, scheduleRetentionOptions(o)...); err != nil {
		s.countFailure()
		return err
	}
	return nil
}

// countFailure records a failed Register so PruneUnregistered skips. どの
// 失敗も数える — 数え漏れた経路で呼び出し側が打ち切ると、後ろの cron が
// 「試みなかった」扱いで消える。
func (s *Scheduler) countFailure() {
	s.mu.Lock()
	s.failed++
	s.mu.Unlock()
}

// PruneUnregistered removes the schedules stored for the driver's queues
// that this process did not try to Register. See driver.Scheduler.
//
// **1 件も登録を試みていなければ何もしない。** worker を持たないロールや、
// 登録より前に呼ばれた場合に全部消すのを防ぐ。
//
// **登録が 1 件でも失敗していたら撤去しない (エラーを返す)。** 呼び出し側には
// 最初の失敗で return するもの (`RegisterChartJobs`) があり、失敗の後ろの
// cron は「試みなかった」ことになる。そのまま消すと、一時的な Redis の失敗で
// 正当な cron が再起動まで止まる。撤去は次の起動に回せば足りる。
//
// **queue ノードが複数あるときは、ビルドと設定を揃えること。** どのノードも
// 自分が登録しなかったものを消すので、旧いビルドのノードは新しい cron を消し、
// プラグインを未設定にしたノードは他のノードが登録したそのプラグインの cron を
// 消す (消えた cron は、それを登録するノードが次に起動したときに戻る)。
//
// 走査するのは driver が定義しているキューだけ (drop-in で同じ Redis を使う
// TS の `system` キューなどは触らない)。**既に積まれた次回分 (delayed) は
// 消さない** — mkq の RemoveSchedule は template と repeat のエントリだけを
// 消すので、撤去したスケジューラも最大 1 回は発火する。その後は template が
// 無いので次を積まない。
func (s *Scheduler) PruneUnregistered() ([]string, error) {
	s.mu.Lock()
	failed := s.failed
	attempted := make(map[string]map[string]struct{}, len(s.attempted))
	for q, ids := range s.attempted {
		attempted[q] = maps.Clone(ids)
	}
	s.mu.Unlock()
	if len(attempted) == 0 {
		return nil, nil
	}
	if failed > 0 {
		return nil, fmt.Errorf("mkqdriver: skipped pruning schedules because %d registration(s) failed", failed)
	}

	ctx := context.Background()
	names := make([]string, 0, len(s.driver.queues))
	for name := range s.driver.queues {
		names = append(names, name)
	}
	slices.Sort(names)

	var removed []string
	var errs []error
	for _, name := range names {
		ids, err := s.driver.rdb.ZRange(ctx, s.driver.repeatKey(name), 0, -1).Result()
		if err != nil {
			errs = append(errs, fmt.Errorf("mkqdriver: list schedules %q: %w", name, err))
			continue
		}
		for _, id := range ids {
			if _, ok := attempted[name][id]; ok {
				continue
			}
			if err := s.driver.queues[name].RemoveSchedule(ctx, id); err != nil {
				errs = append(errs, fmt.Errorf("mkqdriver: remove schedule %s/%s: %w", name, id, err))
				continue
			}
			removed = append(removed, name+"/"+id)
		}
	}
	return removed, errors.Join(errs...)
}

// scheduleRetentionOptions translates the driver's retention options into
// mkq's schedule-side equivalents, with the same guards as the Add path
// (option.go): counts and ages only when positive.
//
// **件数 0 は渡さない。** driver の意味では `WithKeepCompleted(0)` は「件数で
// 制限しない」だが、mkq の `WithScheduleKeepCompleted(0)` は「完了したら即
// 削除」で逆になる (Add 側の `mkqdriver/option.go` と同じ 0-skip)。
func scheduleRetentionOptions(o driver.EnqueueOptions) []mkq.ScheduleOption {
	var out []mkq.ScheduleOption
	if o.KeepCompletedSet && o.KeepCompleted > 0 {
		out = append(out, mkq.WithScheduleKeepCompleted(o.KeepCompleted))
	}
	if o.KeepCompletedAge > 0 {
		out = append(out, mkq.WithScheduleKeepCompletedAge(o.KeepCompletedAge))
	}
	if o.KeepFailedSet && o.KeepFailed > 0 {
		out = append(out, mkq.WithScheduleKeepFailed(o.KeepFailed))
	}
	if o.KeepFailedAge > 0 {
		out = append(out, mkq.WithScheduleKeepFailedAge(o.KeepFailedAge))
	}
	return out
}

// Start is a no-op for mkq — schedules are evaluated lazily by the
// Worker dispatch loop on every prefetch. The method exists to
// satisfy the driver.Scheduler interface.
func (s *Scheduler) Start() error { return nil }

// Shutdown is a no-op for mkq — scheduled entries are persisted in
// Redis and survive driver restarts; nothing to release on the
// scheduler side.
func (s *Scheduler) Shutdown() {}
