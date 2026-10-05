package processors

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/repository"
)

// CleanRemoteNotesCursorKey is the Redis key (after the instance prefix)
// holding the cleaning cursor. upstream と同じ名前なので、TS と入れ替えても
// 同じ位置から続く。
const CleanRemoteNotesCursorKey = "cleanRemoteNotes:cursor"

// CursorStore persists the cleaning cursor across runs.
type CursorStore interface {
	// Get returns the stored cursor, or "" when none is stored.
	Get(ctx context.Context) (string, error)
	Set(ctx context.Context, cursor string) error
	Del(ctx context.Context) error
}

// RedisCursorStore is a CursorStore on a single Redis key.
type RedisCursorStore struct {
	Client *redis.Client
	Key    string
}

// Get implements CursorStore.
func (s RedisCursorStore) Get(ctx context.Context) (string, error) {
	v, err := s.Client.Get(ctx, s.Key).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

// Set implements CursorStore.
func (s RedisCursorStore) Set(ctx context.Context, cursor string) error {
	return s.Client.Set(ctx, s.Key, cursor, 0).Err()
}

// Del implements CursorStore.
func (s RedisCursorStore) Del(ctx context.Context) error {
	return s.Client.Del(ctx, s.Key).Err()
}

// CleanRemoteNotesConfig holds the settings for the cleaning job.
type CleanRemoteNotesConfig struct {
	Enabled              bool
	ExpiryDays           int
	MaxProcessingMinutes int
}

// CleanRemoteNotesProcessor handles the periodic remote notes cleaning task.
type CleanRemoteNotesProcessor struct {
	noteRepo repository.NoteRepository
	// cfg は毎回読み直す。**起動時に固定すると、運営者が管理画面で止めても
	// プロセスを再起動するまで削除が走り続ける** — 不可逆な操作に対する唯一の
	// 停止手段が効かない。姉妹ジョブ (`orphan_user_cleanup.go`) は同じ理由で
	// 既にこの形になっている。upstream もループの各反復で再評価する。
	cfg func() CleanRemoteNotesConfig
	// cursor はジョブをまたいで走査位置を持つ (#17957)。nil なら 1 回の実行の
	// 中だけで前に進む。
	cursor CursorStore
}

// SetCursorStore makes the processor resume from where the previous run
// stopped.
//
// **カーソルを保存しないと、消せないツリーを毎回最初から読み直す。**
// maxProcessingMinutes で打ち切られる規模の instance では、毎晩同じ先頭部分を
// 見て終わり、その後ろに一度も届かない (upstream 2026.9.1 #17957)。
func (p *CleanRemoteNotesProcessor) SetCursorStore(s CursorStore) {
	p.cursor = s
}

// NewCleanRemoteNotesProcessor creates the processor.
func NewCleanRemoteNotesProcessor(noteRepo repository.NoteRepository, cfg func() CleanRemoteNotesConfig) *CleanRemoteNotesProcessor {
	if cfg == nil {
		cfg = func() CleanRemoteNotesConfig { return CleanRemoteNotesConfig{} }
	}
	return &CleanRemoteNotesProcessor{noteRepo: noteRepo, cfg: cfg}
}

// Handle implements driver.HandlerFunc. バッチ削除を maxProcessingMinutes の
// 範囲で繰り返し実行する。100件/バッチで DB 負荷を平準化。
func (p *CleanRemoteNotesProcessor) Handle(ctx context.Context, _ driver.Task) error {
	cfg := p.cfg()
	if !cfg.Enabled {
		slog.Debug("clean-remote-notes: disabled, skipping")
		return nil
	}

	expiryDays := cfg.ExpiryDays
	if expiryDays <= 0 {
		expiryDays = 90
	}
	maxDuration := time.Duration(cfg.MaxProcessingMinutes) * time.Minute
	if maxDuration <= 0 {
		maxDuration = 60 * time.Minute
	}

	deadline := time.Now().Add(maxDuration)
	const batchSize = 100
	var totalDeleted int64

	// **Redis が読めなくても先頭から始めるだけで、不整合は起きない** (upstream と同じ)。
	cursor := ""
	if p.cursor != nil {
		c, err := p.cursor.Get(ctx)
		if err != nil {
			slog.Warn("clean-remote-notes: cannot read the cursor, starting from the beginning", "error", err)
		} else {
			cursor = c
		}
	}

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			break
		}
		// **各反復で有効/無効を読み直す。** 実行中に運営者が止めたら、そこで
		// 止まる (upstream の CleanRemoteNotesProcessorService と同じ)。
		if !p.cfg().Enabled {
			slog.Info("clean-remote-notes: disabled during run, stopping")
			break
		}
		deleted, scanned, lastRootID, err := p.noteRepo.DeleteExpiredRemoteNotesAfter(expiryDays, batchSize, cursor)
		if err != nil {
			slog.Error("clean-remote-notes: batch delete failed", "error", err)
			return err
		}
		totalDeleted += deleted
		// **判定は見た根の数で行う。** 消した行数 (子を含む) と比べると単位が
		// 合わず、消せないツリーが並んだところで打ち切ってしまう。
		if scanned < batchSize {
			// 末尾まで来たので、次回は先頭から。クリップやお気に入りを外されて
			// 後から消せるようになったノートを拾い直すため (upstream と同じ)。
			p.storeCursor(ctx, "")
			break
		}
		cursor = lastRootID
		// **バッチごとに保存する。** 時間切れで打ち切られても、そこまでの
		// 前進が次回に引き継がれる。
		p.storeCursor(ctx, cursor)
		// I/O 平準化のため短い sleep を挟む
		time.Sleep(500 * time.Millisecond)
	}

	if totalDeleted > 0 {
		slog.Info("clean-remote-notes: completed", "deleted", totalDeleted)
	}
	return nil
}

// storeCursor saves (or with "" clears) the cursor. 失敗しても次回が先頭から
// 始まるだけなので、ログに残して続ける。
func (p *CleanRemoteNotesProcessor) storeCursor(ctx context.Context, cursor string) {
	if p.cursor == nil {
		return
	}
	var err error
	if cursor == "" {
		err = p.cursor.Del(ctx)
	} else {
		err = p.cursor.Set(ctx, cursor)
	}
	if err != nil {
		slog.Warn("clean-remote-notes: cannot save the cursor", "error", err)
	}
}
