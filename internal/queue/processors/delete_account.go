package processors

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
	"github.com/shiroha-a/mk/internal/repository"
)

// DeleteAccountProcessor cascades through a user's related rows (notes,
// drive files, follow graph) after delete-account has flipped the soft-delete
// flags. For a non-soft (local) deletion it then physically removes the user
// row so the account fully disappears; for a soft (remote) deletion the row is
// kept as a tombstone to prevent resurrection on re-federation (#2230).
type DeleteAccountProcessor struct {
	noteRepo      repository.NoteRepository
	driveFileRepo repository.DriveFileRepository
	followingRepo repository.FollowingRepository
	// userRepo は Soft=false (local) 時の user 行物理削除に使う optional 依存 (#2230)。
	// 未配線なら hard delete を skip し従来の soft 削除のままになる。
	userRepo repository.UserRepository
	// pageRepo はページを 1 件ずつ消して、参照するノートの pageCount を減らすのに
	// 使う (#3293)。user 行の削除の CASCADE に任せると pageCount が減らない。
	pageRepo repository.PageRepository
	// pushCache は sw_subscription 行を消した後に購読キャッシュを捨てるのに使う。
	// 捨てないと、削除後に作られた通知が Redis の 1 時間 TTL が切れるまで
	// 消したはずの購読へ push され続ける。
	pushCache PushSubscriptionCacheInvalidator
}

// PushSubscriptionCacheInvalidator drops the cached push subscriptions of a
// user so that the next Web Push delivery re-reads them from the database.
// *webpush.SubscriptionCache satisfies it.
type PushSubscriptionCacheInvalidator interface {
	Invalidate(ctx context.Context, userID string)
}

// SetPushSubscriptionCache wires the cache that Web Push delivery reads
// subscriptions from. It must be the same instance the WebPushProcessor uses;
// nil disables invalidation.
func (p *DeleteAccountProcessor) SetPushSubscriptionCache(c PushSubscriptionCacheInvalidator) {
	p.pushCache = c
}

// HasPushSubscriptionCache reports whether SetPushSubscriptionCache was wired.
func (p *DeleteAccountProcessor) HasPushSubscriptionCache() bool { return p.pushCache != nil }

// pushCacheInvalidateTimeout bounds the Redis call made to drop the cached
// push subscriptions.
const pushCacheInvalidateTimeout = 5 * time.Second

// invalidatePushSubscriptions drops the cached push subscriptions of userID
// after its sw_subscription rows were deleted.
//
// 行を消した後に job の ctx が cancel されると Redis の Del が失敗し、Invalidate は
// エラーを返さないので job は完了扱いのままキャッシュが最大 1 時間残る。
// 行の削除はもう確定しているので、cancel を切り離した ctx に上限だけ付けて呼ぶ。
func (p *DeleteAccountProcessor) invalidatePushSubscriptions(ctx context.Context, userID string) {
	if p.pushCache == nil {
		return
	}
	local, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushCacheInvalidateTimeout)
	defer cancel()
	p.pushCache.Invalidate(local, userID)
}

// SetPageRepo wires the PageRepository used to delete the user's pages one by
// one so that the notes they reference get their pageCount decremented (#3293).
func (p *DeleteAccountProcessor) SetPageRepo(r repository.PageRepository) {
	p.pageRepo = r
}

// HasPageRepo reports whether SetPageRepo was wired.
func (p *DeleteAccountProcessor) HasPageRepo() bool { return p.pageRepo != nil }

// SetUserRepo wires the UserRepository used to physically delete the user row
// for non-soft (local) account deletion (#2230). nil keeps the soft behavior.
func (p *DeleteAccountProcessor) SetUserRepo(r repository.UserRepository) {
	p.userRepo = r
}

// NewDeleteAccountProcessor wires the processor with the repositories it
// needs. Any nil repository causes that deletion phase to be skipped, so
// tests and partial-start configurations do not need to supply the full
// repository set.
func NewDeleteAccountProcessor(noteRepo repository.NoteRepository, driveFileRepo repository.DriveFileRepository, followingRepo repository.FollowingRepository) *DeleteAccountProcessor {
	return &DeleteAccountProcessor{
		noteRepo:      noteRepo,
		driveFileRepo: driveFileRepo,
		followingRepo: followingRepo,
	}
}

// deleteAccountNoteBatchSize controls how many notes are deleted per loop
// iteration. Kept small enough that one batch finishes well within the
// driver's default per-task timeout and the processor can check
// ctx.Err() between batches.
const deleteAccountNoteBatchSize = 100

// deleteAccountBatchSleep throttles DB I/O between note batches to avoid
// saturating the write path for accounts with very large note histories.
const deleteAccountBatchSleep = 250 * time.Millisecond

// Handle implements driver.HandlerFunc.
//
// 部分実行 (notes だけ消えた等) の状態で nil を返すと driver は task 成功扱い
// で MaxRetry が効かず、さらに Unique(24h) によって admin も 24 時間再エンキュー
// できない。操作はすべて冪等 (既に消えた行は 0 件影響) なので、ctx 切れ / repo
// エラーはそのまま返して driver の retry に任せる。
func (p *DeleteAccountProcessor) Handle(ctx context.Context, t driver.Task) error {
	payload, err := queue.DecodeDeleteAccountPayload(t.Payload())
	if err != nil {
		return fmt.Errorf("decode delete-account payload: %w: %w", err, driver.ErrSkipRetry)
	}
	if payload.UserID == "" {
		return fmt.Errorf("delete-account: userId is required: %w", driver.ErrSkipRetry)
	}
	if !payload.Soft && payload.PreserveAccount {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.userRepo == nil {
			return fmt.Errorf("delete-account: retained account credential cleanup is not wired")
		}
		// 長いコンテンツ削除より先に失効し、失敗時はジョブを再試行する。
		if err := p.userRepo.RevokeDeletedLocalCredentials(payload.UserID); err != nil {
			return fmt.Errorf("delete-account: revoke retained account credentials: %w", err)
		}
		p.invalidatePushSubscriptions(ctx, payload.UserID)
	}

	if err := p.deleteNotes(ctx, payload.UserID); err != nil {
		return err
	}

	if err := p.deletePages(ctx, payload.UserID); err != nil {
		return err
	}

	if p.driveFileRepo != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		deleted, err := p.driveFileRepo.DeleteByUser(payload.UserID)
		if err != nil {
			slog.Error("delete-account: drive purge failed",
				"userId", payload.UserID, "err", err)
			return err
		}
		if deleted > 0 {
			slog.Info("delete-account: drive files deleted",
				"userId", payload.UserID, "count", deleted)
		}
	}

	if p.followingRepo != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		deleted, err := p.followingRepo.DeleteAllByUser(payload.UserID)
		if err != nil {
			slog.Error("delete-account: following purge failed",
				"userId", payload.UserID, "err", err)
			return err
		}
		if deleted > 0 {
			slog.Info("delete-account: following rows deleted",
				"userId", payload.UserID, "count", deleted)
		}
	}

	// #2230: local user (Soft=false) は user 行を物理削除する。FK ON DELETE CASCADE で
	// profile / keypair / 残りの従属行も消えるため、論理削除フラグだけ立った「凍結状態の
	// tombstone」が残らずアカウントが完全に消える。remote user (Soft=true) は再連合での
	// 復活を防ぐため行を残す (upstream DeleteAccountProcessorService の soft 分岐と同じ)。
	if !payload.Soft && !payload.PreserveAccount && p.userRepo != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.userRepo.HardDeleteUser(payload.UserID); err != nil {
			slog.Error("delete-account: user row purge failed",
				"userId", payload.UserID, "err", err)
			return err
		}
		// sw_subscription は user 行の CASCADE で消えるので、購読キャッシュも捨てる。
		p.invalidatePushSubscriptions(ctx, payload.UserID)
		slog.Info("delete-account: user row deleted", "userId", payload.UserID)
	}
	return nil
}

// deleteNotes loops DeleteByUserBatch so that cancellation checkpoints and
// throttling live here instead of in the repository. 大量ノート保有ユーザー
// (10万件規模) に備えて各 batch の後で ctx.Err() を確認し、途中終了した場合は
// その error を返して driver に retry させる。
func (p *DeleteAccountProcessor) deleteNotes(ctx context.Context, userID string) error {
	if p.noteRepo == nil {
		return nil
	}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			slog.Warn("delete-account: note purge interrupted",
				"userId", userID, "deleted", total, "err", err)
			return err
		}
		deleted, err := p.noteRepo.DeleteByUserBatch(userID, deleteAccountNoteBatchSize)
		if err != nil {
			slog.Error("delete-account: note purge failed",
				"userId", userID, "err", err)
			return err
		}
		total += deleted
		if deleted < int64(deleteAccountNoteBatchSize) {
			break
		}
		// I/O 平準化の短い sleep。ctx が切れたら cancel error を返して retry 扱い。
		select {
		case <-ctx.Done():
			err := ctx.Err()
			slog.Warn("delete-account: note purge canceled during pacing",
				"userId", userID, "deleted", total, "err", err)
			return err
		case <-time.After(deleteAccountBatchSleep):
		}
	}
	if total > 0 {
		slog.Info("delete-account: notes deleted",
			"userId", userID, "count", total)
	}
	return nil
}

// deletePageBatchSize bounds how many pages are listed per loop iteration.
const deletePageBatchSize = 100

// deletePages deletes the user's pages through PageRepository.Delete so the
// notes they reference get their pageCount decremented. upstream
// DeleteAccountProcessorService も同じ理由でページを 1 件ずつ PageService.delete
// で消している。
func (p *DeleteAccountProcessor) deletePages(ctx context.Context, userID string) error {
	if p.pageRepo == nil {
		return nil
	}
	var total int
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pages, err := p.pageRepo.ListByUser(userID, "", "", deletePageBatchSize, 0)
		if err != nil {
			slog.Error("delete-account: page listing failed", "userId", userID, "err", err)
			return err
		}
		if len(pages) == 0 {
			break
		}
		for _, pg := range pages {
			if err := p.pageRepo.Delete(pg); err != nil {
				slog.Error("delete-account: page deletion failed",
					"userId", userID, "pageId", pg.ID, "err", err)
				return err
			}
		}
		total += len(pages)
	}
	if total > 0 {
		slog.Info("delete-account: pages deleted", "userId", userID, "count", total)
	}
	return nil
}
