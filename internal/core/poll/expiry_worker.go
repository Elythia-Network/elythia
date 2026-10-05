package poll

import (
	"context"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// PollEndedNotifier is the minimal subset of core/notification.Service needed
// to send a `pollEnded` notification. 循環依存を避けるため interface で受け
// 取り、ExpiryWorker のテストで stub できるようにする。
type PollEndedNotifier interface {
	Create(ctx context.Context, in notification.CreateInput) (*notification.Notification, error)
}

// ExpiryWorker scans the poll table on a periodic ticker and sends
// `pollEnded` notifications to the author + local voters of polls whose
// expiresAt has passed (#690).
//
// Misskey TS は BullMQ delayed job で同等の動作を実現しているが、mk-go では
// queue infra を経由せず周期 scan で済ませる方針。partial index
// `IDX_poll_expired_unnotified` (migration 044) が WHERE expiresAt < NOW()
// AND notifiedAt IS NULL を効率化するため、空 scan のオーバーヘッドは小さい。
type ExpiryWorker struct {
	pollRepo     repository.PollRepository
	pollVoteRepo repository.PollVoteRepository
	noteRepo     repository.NoteRepository
	userRepo     repository.UserRepository
	notifier     PollEndedNotifier
	interval     time.Duration
	batchSize    int
	nowFn        func() time.Time
}

// NewExpiryWorker constructs an ExpiryWorker. interval は scan 間隔で 60s
// が運用デフォルト (poll 期限の精度として十分、空 scan のコストも小さい)。
// batchSize は 1 tick あたりに処理する poll 件数の上限 (大量 backlog 時に
// 1 tick で全部処理しないようにする防御)。
func NewExpiryWorker(
	pollRepo repository.PollRepository,
	pollVoteRepo repository.PollVoteRepository,
	noteRepo repository.NoteRepository,
	userRepo repository.UserRepository,
	notifier PollEndedNotifier,
	interval time.Duration,
	batchSize int,
) *ExpiryWorker {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	if batchSize <= 0 {
		batchSize = 100
	}
	return &ExpiryWorker{
		pollRepo:     pollRepo,
		pollVoteRepo: pollVoteRepo,
		noteRepo:     noteRepo,
		userRepo:     userRepo,
		notifier:     notifier,
		interval:     interval,
		batchSize:    batchSize,
		nowFn:        time.Now,
	}
}

// SetClock replaces the clock used by Tick / Run. テスト用。
func (w *ExpiryWorker) SetClock(now func() time.Time) {
	if now != nil {
		w.nowFn = now
	}
}

// Run starts the periodic scan loop. Returns when ctx is cancelled. Each
// tick calls Tick once; failures are logged and the loop continues so a
// single transient DB error does not stop notifications permanently.
func (w *ExpiryWorker) Run(ctx context.Context) {
	if w == nil || w.pollRepo == nil {
		return
	}
	// Process any backlog that accumulated while the server was down BEFORE
	// the first tick interval, so a restart doesn't delay notifications by
	// up to `interval` seconds.
	if err := w.Tick(ctx); err != nil {
		slog.WarnContext(ctx, "poll expiry worker: initial tick failed", "err", err)
	}
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.Tick(ctx); err != nil {
				slog.WarnContext(ctx, "poll expiry worker: tick failed", "err", err)
			}
		}
	}
}

// Tick runs one scan + notify cycle. Exposed so callers (and tests) can
// drive the worker synchronously. Returns the first error encountered;
// subsequent polls in the batch are still processed (best-effort).
func (w *ExpiryWorker) Tick(ctx context.Context) error {
	if w == nil || w.pollRepo == nil {
		return nil
	}
	now := w.nowFn()
	polls, err := w.pollRepo.ListExpiredUnnotified(now, w.batchSize)
	if err != nil {
		return err
	}
	for _, p := range polls {
		if err := ctx.Err(); err != nil {
			return err
		}
		w.notifyOne(ctx, p, now)
	}
	return nil
}

// notifyOne sends `pollEnded` notifications to the poll author + local
// voters and marks the poll as notified. Marks even when notifications fail
// partially so a stuck downstream doesn't stall the queue forever (audit
// log style: best-effort once, not retried).
func (w *ExpiryWorker) notifyOne(ctx context.Context, p *model.Poll, now time.Time) {
	if p == nil {
		return
	}
	// Collect target user IDs (author + voters) and dedup to avoid double
	// notifications when the author also voted on their own poll.
	targets := map[string]struct{}{p.UserID: {}}
	if w.pollVoteRepo != nil {
		votes, err := w.pollVoteRepo.ListByNoteID(p.NoteID)
		if err == nil {
			for _, v := range votes {
				targets[v.UserID] = struct{}{}
			}
		} else {
			slog.WarnContext(ctx, "poll expiry worker: list votes failed",
				"noteId", p.NoteID, "err", err)
		}
	}
	// Resolve note visibility once for the notification record (read path
	// uses noteVisibility to filter unread counts without a join).
	var noteVisibility string
	if w.noteRepo != nil {
		if n, err := w.noteRepo.FindByID(p.NoteID); err == nil && n != nil {
			noteVisibility = string(n.Visibility)
		}
	}
	for uid := range targets {
		// Skip remote voters (Misskey TS は host=NULL のローカル user に
		// 限定する; リモート user の通知は連合先側で発火する責任)。
		if w.userRepo != nil {
			u, err := w.userRepo.FindByID(uid)
			if err != nil || u == nil {
				continue
			}
			if u.Host != nil && *u.Host != "" {
				continue
			}
		}
		if w.notifier == nil {
			continue
		}
		if _, err := w.notifier.Create(ctx, notification.CreateInput{
			NotifieeID:     uid,
			Type:           notification.TypePollEnded,
			NoteID:         p.NoteID,
			NoteVisibility: noteVisibility,
		}); err != nil {
			slog.WarnContext(ctx, "poll expiry worker: notify failed",
				"noteId", p.NoteID, "userId", uid, "err", err)
		}
	}
	if err := w.pollRepo.MarkNotified(p.NoteID, now); err != nil {
		slog.WarnContext(ctx, "poll expiry worker: mark notified failed",
			"noteId", p.NoteID, "err", err)
	}
}
