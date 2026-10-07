// Package processors hosts queue task handlers consumed by the queue worker.
//
// This file implements the scheduled-note publish path (#1040). It is a thin
// orchestration layer: load the draft, materialise it into a note via the
// shared note.CreateService, then delete the draft. Notification on
// success / failure is intentionally out of scope for the first PR and may be
// added in a follow-up once mk-go gains the upstream `scheduledNotePosted` /
// `scheduledNotePostFailed` notification types.
package processors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/repository"
)

// ScheduledNoteDraftRepo is the narrow subset of NoteDraftRepository required
// by PostScheduledNoteProcessor. Defined as a separate interface so test
// fixtures can swap a minimal stub without touching the full repository.
type ScheduledNoteDraftRepo interface {
	FindByID(id string) (*model.NoteDraft, error)
	Delete(id, userID string) (int64, error)
}

// ScheduledNoteLock provides app-level idempotency for scheduled note publish.
// queue の at-least-once delivery で job が二度 fire しても TryAcquire が
// 1 度しか true を返さないことで重複 publish を防止する (#1045 Phase 2-A)。
//
// DB schema 不変な実装 (Redis SETNX + TTL 等) を選ぶことで upstream Misskey
// TS の `note_draft` schema と完全互換のまま重複 publish 耐性を確保する。
//
// 実装は nil 可 (= 失敗時 / 未配線時は TryAcquire が常に true を返す挙動と
// 等価で、Phase 1 の旧経路と互換)。production は redis-backed 実装を必ず
// 配線するべき。
type ScheduledNoteLock interface {
	// TryAcquire returns (true, nil) when this caller is the first to claim
	// the lock for draftID (= publish に進める), or (false, nil) when another
	// retry already holds it (= silent skip). err != nil なら lock backend
	// 自体の障害なので caller は retry に任せる。
	//
	// **取るのは publish の直前** (#3121)。job が retry されるように
	// なったので、lookup の失敗より前で取ると「失敗 → lock を握ったまま
	// 終了 → TTL (5 分) のあいだ retry が `ok=false` で silent skip」に
	// なり、**障害が「already being published」という事実と逆の Info 1 行で
	// 成功扱いになる** (#3116 が潰した形の再発)。解放の口を足して落とす案は
	// 採らない — 解放が 1 度失敗するだけで同じ結末になるうえ、TTL より初回の
	// backoff (約 60 秒) のほうが短いので TTL は救いにならない。
	TryAcquire(ctx context.Context, draftID string) (bool, error)
}

// ScheduledNoteUserRepo abstracts the single user lookup needed to call
// note.CreateService.Create with a fully-populated *model.User.
type ScheduledNoteUserRepo interface {
	FindByID(id string) (*model.User, error)
}

// ScheduledNotePublisher abstracts note.CreateService so callers can pass a
// real *note.CreateService in production or a stub in tests.
type ScheduledNotePublisher interface {
	Create(in note.CreateInput) (*model.Note, error)
}

// ScheduledNoteNotifier dispatches the scheduled note publish result to the
// notification system. upstream Misskey TS の \`NotificationService.create\`
// と同 contract で、posted (= 成功時 noteId) / postFailed (= 失敗時
// noteDraftId) の 2 種を発火する (#1045 Phase 2-B)。nil の場合は notification
// skip (= Phase 2-A 時点の slog のみ挙動と互換)。
type ScheduledNoteNotifier interface {
	Create(ctx context.Context, in notification.CreateInput) (*notification.Notification, error)
}

// PostScheduledNoteProcessor publishes a note from a previously-stored
// `note_draft` row whose `isActuallyScheduled=true` and `scheduledAt` has
// arrived. upstream `PostScheduledNoteProcessorService` の Go port (#1040)。
type PostScheduledNoteProcessor struct {
	drafts    ScheduledNoteDraftRepo
	users     ScheduledNoteUserRepo
	publisher ScheduledNotePublisher
	// lock は idempotency 用 (#1045 Phase 2-A)。nil の場合は lock skip = Phase 1
	// と同等の at-least-once 挙動 (= test fixture / 未配線 path)。
	lock ScheduledNoteLock
	// notifier は publish 結果通知用 (#1045 Phase 2-B)。nil で発火 skip。
	notifier ScheduledNoteNotifier
}

// NewPostScheduledNoteProcessor wires the processor with its dependencies.
func NewPostScheduledNoteProcessor(drafts ScheduledNoteDraftRepo, users ScheduledNoteUserRepo, publisher ScheduledNotePublisher) *PostScheduledNoteProcessor {
	return &PostScheduledNoteProcessor{drafts: drafts, users: users, publisher: publisher}
}

// SetLock wires an app-level idempotency lock. nil disables (= Phase 1 互換)。
// production では Redis-backed 実装を配線して重複 publish を防止する
// (#1045 Phase 2-A)。
// HasLock reports whether the idempotency lock is wired.
//
// 未配線だと Handle が lock 取得を飛ばして**常に publish する**。queue は
// at-least-once なので、同じ job が二度 fire したときに予約投稿が 2 回
// 出て scheduledNotePosted も 2 回飛ぶ。冪等性を handler 側の性質に
// 頼らず保証するための lock なので、inbox の replay guard と同じ
// 一回性の tier として起動時に検査する (#2682 review M-C)。
func (p *PostScheduledNoteProcessor) HasLock() bool { return p.lock != nil }

func (p *PostScheduledNoteProcessor) SetLock(l ScheduledNoteLock) {
	p.lock = l
}

// SetNotifier wires the notification dispatcher used to fire
// `scheduledNotePosted` / `scheduledNotePostFailed` on publish completion
// (#1045 Phase 2-B)。nil disables (= notification skip)、production では
// core/notification.Service を配線する。
func (p *PostScheduledNoteProcessor) SetNotifier(n ScheduledNoteNotifier) {
	p.notifier = n
}

// logNotificationErr classifies a notification dispatch error and emits the
// appropriate log level. graceful shutdown / deadline cancel は normal な
// 運用シナリオなので Warn に出さず Debug に格下げする (= log noise を抑え、
// 真の notification backend 障害だけ Warn で観測する、#1045 Phase 2-B
// follow-up)。
func logNotificationErr(action string, draftID string, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		slog.Debug("scheduled note notification skipped during shutdown",
			"action", action, "noteDraftId", draftID, "err", err)
		return
	}
	slog.Warn("scheduled note notification failed",
		"action", action, "noteDraftId", draftID, "err", err)
}

// Handle implements the driver task handler signature. It decodes the payload,
// guards against drafts that no longer exist or were unscheduled before the
// trigger fired, materialises the note via the publisher, and finally deletes
// the draft row.
//
// Errors are returned to the queue so the driver retries; transient DB failures
// will be retried automatically. A draft that vanished (= user deleted it
// before fire time) is treated as a no-op (= return nil) — upstream behaves
// the same way (`if (draft == null || ...) return`)。
func (p *PostScheduledNoteProcessor) Handle(ctx context.Context, task driver.Task) error {
	payload, err := queue.DecodePostScheduledNotePayload(task.Payload())
	if err != nil {
		// **retry しない** (#3121)。attempts を積んだので、付けないと壊れた
		// payload が backoff 込みで 30 時間ほど再試行され続ける。inbox 側の
		// decode と同じ扱い。
		return fmt.Errorf("decode payload: %w: %w", err, driver.ErrSkipRetry)
	}
	draft, err := p.drafts.FindByID(payload.NoteDraftID)
	if err != nil && !repository.IsNotFound(err) {
		// **障害を成功として記録しない** (#3116)。以前は「retry しても解消しない」を
		// 種別を見ずに全ての error へ適用しており、DB 障害でも Info ログ 1 行で
		// 成功扱いになっていた。
		//
		// **retry される** (#3121)。`EnqueuePostScheduledNote` が policy から
		// attempts を積むようになり、publish に到達する前の失敗では直下で
		// lock を落とすので、retry が同じ draft を掴み直せる。
		// publish そのものの失敗は下で ack する (二重 publish / 二重通知を
		// 避けるための意図的な設計、#2106 L61)。
		slog.Error("scheduled note draft lookup failed",
			"noteDraftId", payload.NoteDraftID, "err", err)
		return fmt.Errorf("scheduled note: lookup draft: %w", err)
	}
	if err != nil {
		// Draft が消えていれば user 削除 / 手動 cancel / 既に publish 済み
		// のどれかで、いずれも retry しても解消しない。silent success で
		// 終わる (upstream と等価)。
		slog.Info("scheduled note draft missing, skipping",
			"noteDraftId", payload.NoteDraftID, "err", err)
		return nil
	}
	if draft.ScheduledAt == nil || !draft.IsActuallyScheduled {
		// Draft が unschedule された経路 (= 将来 DraftsUpdate で scheduledAt
		// クリア相当を許容する場合)。何もせず draft を残す。
		return nil
	}
	user, err := p.users.FindByID(draft.UserID)
	if err != nil {
		if repository.IsNotFound(err) {
			// **retry しても変わらない** (#3121)。`note_draft.userId` は
			// ON DELETE CASCADE なので普通は起きないが、起きたなら
			// 利用者ごと消えている。attempts を積んだ今、伝播させると
			// 空振りの retry を繰り返すだけになる。
			slog.Info("scheduled note draft user no longer exists, skipping",
				"noteDraftId", payload.NoteDraftID, "userId", draft.UserID)
			return nil
		}
		return fmt.Errorf("load draft user: %w", err)
	}
	if user == nil {
		// 防御的: nil ユーザを Create に流すと panic の元なので skip。
		return errors.New("scheduled note draft has nil user")
	}
	in := note.CreateInput{
		User:           user,
		Text:           draft.Text,
		CW:             draft.CW,
		Visibility:     model.NoteVisibility(draft.Visibility),
		VisibleUserIDs: draft.VisibleUserIDs,
		LocalOnly:      draft.LocalOnly,
		FileIDs:        draft.FileIDs,
		ReplyID:        draft.ReplyID,
		RenoteID:       draft.RenoteID,
		ChannelID:      draft.ChannelID,
	}
	if draft.ReactionAcceptance != nil {
		ra := *draft.ReactionAcceptance
		in.ReactionAcceptance = &ra
	}
	// Poll の復元 (#1045 Phase 2-D)。upstream
	// `PostScheduledNoteProcessorService.process` と同 logic で:
	//   - hasPoll=false なら Poll=nil (= 設定なし)
	//   - hasPoll=true なら PollChoices / PollMultiple / PollExpiresAt または
	//     PollExpiredAfter から `*time.Time` 期限を復元
	//   - PollExpiredAfter (= 経過 ms) 優先、無ければ PollExpiresAt (= 絶対時刻)
	//
	// 期限が上限 (note.MaxPollExpiresAtUnixMilli) を超える draft は、publish 失敗
	// として scheduledNotePostFailed を通知する。upstream も Date で表せない期限は
	// fetchAndCreate が落ちて同じ通知になる (#3443)。lock を取った後で扱うため、
	// ここでは控えるだけ。
	var pollErr error
	if draft.HasPoll {
		pollInput := &note.PollInput{
			Choices:  draft.PollChoices,
			Multiple: draft.PollMultiple,
		}
		if draft.PollExpiredAfter != nil {
			var exp time.Time
			exp, pollErr = note.PollExpiresAtAfter(time.Now(), *draft.PollExpiredAfter)
			pollInput.ExpiresAt = &exp
		} else if draft.PollExpiresAt != nil {
			exp := *draft.PollExpiresAt
			pollErr = note.ValidatePollExpiresAt(exp)
			pollInput.ExpiresAt = &exp
		}
		in.Poll = pollInput
	}
	// idempotency lock 取得 (#1045 Phase 2-A)。at-least-once delivery で job が
	// 二度 fire した場合、最初の 1 つが lock を保持し、後続は false を得て
	// silent skip する。lock 未配線なら常に true (= Phase 1 互換挙動、
	// production race 受容)。
	//
	// **ここまで下げてあるのは #3121。** 手前の lookup が失敗したときは lock を
	// 握っていないので、retry が素直に掴み直せる (`ScheduledNoteLock` の doc)。
	if p.lock != nil {
		ok, lockErr := p.lock.TryAcquire(ctx, payload.NoteDraftID)
		if lockErr != nil {
			// lock backend error は retry に任せる (queue が backoff で
			// 再試行する)。返した error は queue 側で log される。
			return fmt.Errorf("acquire scheduled note lock: %w", lockErr)
		}
		if !ok {
			// 他の試行が publish 済み / publish 中。本 caller は何もしない。
			slog.Info("scheduled note already being published, skipping",
				"noteDraftId", payload.NoteDraftID)
			return nil
		}
	}
	var publishedNote *model.Note
	if pollErr != nil {
		err = pollErr
	} else {
		publishedNote, err = p.publisher.Create(in)
	}
	if err != nil {
		// #2106 L61: upstream PostScheduledNoteProcessorService は publish 失敗時に
		// scheduledNotePostFailed 通知のみ行い rethrow せず正常終了する (retry しない)。
		// mk-go も error を返さず job を完了扱いにする。これにより lock TTL 内の silent
		// skip / TTL 超過後の二重 publish (二重通知) という潜在 retry バグを回避する。
		slog.Warn("scheduled note publish failed",
			"noteDraftId", payload.NoteDraftID, "userId", draft.UserID, "err", err)
		if p.notifier != nil {
			if _, nerr := p.notifier.Create(ctx, notification.CreateInput{
				NotifieeID: draft.UserID,
				Type:       notification.TypeScheduledNotePostFailed,
				Extra:      map[string]any{"noteDraftId": draft.ID},
			}); nerr != nil {
				logNotificationErr("postFailed", payload.NoteDraftID, nerr)
			}
		}
		return nil
	}
	// publish 成功通知 (#1045 Phase 2-B)。upstream は `noteId` を引数で渡し、
	// frontend が note を embed して表示する。
	if p.notifier != nil {
		if _, nerr := p.notifier.Create(ctx, notification.CreateInput{
			NotifieeID: draft.UserID,
			Type:       notification.TypeScheduledNotePosted,
			NoteID:     publishedNote.ID,
		}); nerr != nil {
			logNotificationErr("posted", payload.NoteDraftID, nerr)
		}
	}
	if _, err := p.drafts.Delete(draft.ID, draft.UserID); err != nil {
		// publish 成功 + draft 削除失敗時は draft が残るだけで二重 publish
		// は起きない — **ここから下は必ず nil を返す**ので job は retry されず
		// (#3121 で attempts を積んだ後も同じ)、lock も落としていないため
		// TTL 内の再発火は silent skip になる。log のみ。
		slog.Warn("scheduled note draft delete failed",
			"noteDraftId", payload.NoteDraftID, "err", err)
	}
	return nil
}
