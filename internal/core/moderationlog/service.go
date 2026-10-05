package moderationlog

import (
	"context"
	"encoding/json"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"gorm.io/datatypes"
)

// Service writes and reads moderation log rows. Writes are asynchronous
// (fire-and-forget): Log returns immediately, the actual DB insert and
// JSON encoding happen in a recovered goroutine, and any failure is
// observed via slog.WarnContext rather than propagated. Audit logging
// must never block or fail the moderation action itself.
//
// The Misskey TS counterpart
// (packages/backend/src/core/ModerationLogService.ts) is invoked the
// same way — no await on log() — and we mirror that contract.
//
// Reads (List) wrap the underlying repository so that admin handlers
// can depend on a single Service rather than the raw repository,
// matching the write side and making it easier to future-proof the
// boundary (caching, filtering, etc.).
type Service struct {
	repo  repository.ModerationLogRepository
	idGen id.Generator
}

// New constructs a Service. Either dependency may be nil during tests
// or partial wiring; in that case Log degrades to a silent no-op and
// List returns an empty result without error.
func New(repo repository.ModerationLogRepository, idGen id.Generator) *Service {
	return &Service{repo: repo, idGen: idGen}
}

// Log records a moderation action performed by moderatorID. The info
// map is JSON-encoded into moderation_log.info; key names must match
// Misskey TS (e.g. "userId", "userUsername", "userHost") because the
// shared frontend reads them by exact field name.
//
// ctx is forwarded to slog.*Context so trace/correlation attributes
// flow into audit observability. It is intentionally NOT used to cancel
// the goroutine: cancelling an audit write because the originating
// request was cancelled would silently lose entries, which defeats the
// purpose of audit logging.
//
// Log never blocks and never returns errors. Nil receiver / unwired
// deps / JSON encoding failures / repo errors / panics inside the
// goroutine all surface as warn-level logs.
//
// Caller contract: do NOT mutate `info` after calling Log; the map is
// referenced (not deep-copied) by the goroutine that performs the
// JSON marshal.
func (s *Service) Log(ctx context.Context, moderatorID string, t LogType, info map[string]any) {
	if s == nil || s.repo == nil || s.idGen == nil {
		return
	}
	if info == nil {
		info = map[string]any{}
	}
	safeGo(ctx, func() {
		infoBytes, err := json.Marshal(info)
		if err != nil {
			slog.WarnContext(ctx, "moderation log: marshal info failed",
				"type", t, "moderatorId", moderatorID, "err", err)
			return
		}
		entry := &model.ModerationLog{
			ID:     s.idGen.Generate(time.Now()),
			UserID: moderatorID,
			Type:   string(t),
			Info:   datatypes.JSON(infoBytes),
		}
		if err := s.repo.Create(entry); err != nil {
			slog.WarnContext(ctx, "moderation log: write failed",
				"type", t, "moderatorId", moderatorID, "err", err)
		}
	})
}

// Entry pairs a LogType with its info payload for batched writes via
// Service.LogMany. Used by bulk admin operations (#671) so callers can hand
// over N records and have them inserted in a single goroutine + single
// multi-row INSERT, instead of fanning out N goroutines via Log.
type Entry struct {
	Type LogType
	Info map[string]any
}

// LogMany batches Log writes for bulk admin operations. Produces a single
// goroutine + a single multi-row INSERT (via repo.CreateMany), regardless of
// how many entries were submitted. Same fail-fast / fail-loud semantics as
// Log: never blocks, never returns errors, problems surface as warn-level
// logs. Empty / nil entries slice is a silent no-op.
//
// 巨大 bulk (例: 数千件の emoji 一括削除) でも N goroutine + N INSERT に
// fan-out しないことが本 API の存在意義 (#671)。callers は Log を per-item
// で叩く代わりに Entry slice を組み立てて 1 回だけ呼ぶ。
//
// Caller contract: do NOT mutate any of the entries' Info maps after calling
// LogMany; the maps are referenced (not deep-copied) by the goroutine that
// performs the JSON marshal.
func (s *Service) LogMany(ctx context.Context, moderatorID string, entries []Entry) {
	if s == nil || s.repo == nil || s.idGen == nil {
		return
	}
	if len(entries) == 0 {
		return
	}
	safeGo(ctx, func() {
		now := time.Now()
		rows := make([]*model.ModerationLog, 0, len(entries))
		for _, e := range entries {
			info := e.Info
			if info == nil {
				info = map[string]any{}
			}
			infoBytes, err := json.Marshal(info)
			if err != nil {
				slog.WarnContext(ctx, "moderation log: marshal info failed (LogMany)",
					"type", e.Type, "moderatorId", moderatorID, "err", err)
				// 1 件 marshal 失敗 → そのエントリだけ skip して残りは insert
				// (audit log は best-effort; 一部失敗で全件丸捨ては避ける)。
				continue
			}
			rows = append(rows, &model.ModerationLog{
				ID:     s.idGen.Generate(now),
				UserID: moderatorID,
				Type:   string(e.Type),
				Info:   datatypes.JSON(infoBytes),
			})
		}
		if len(rows) == 0 {
			return
		}
		if err := s.repo.CreateMany(rows); err != nil {
			slog.WarnContext(ctx, "moderation log: batch write failed",
				"count", len(rows), "moderatorId", moderatorID, "err", err)
		}
	})
}

// List returns the most recent moderation log entries (ordered DESC by
// id), bounded by limit/offset. Always returns a non-nil slice on
// success so callers can hand the result to JSON encoders without an
// extra nil-to-empty-slice dance (a nil slice would marshal as `null`,
// not `[]`, and the frontend expects a JSON array). Nil receiver /
// nil repo also returns an empty slice so the read endpoint degrades
// gracefully when the service has not been wired.
func (s *Service) List(filter model.ModerationLogFilter) ([]*model.ModerationLog, error) {
	if s == nil || s.repo == nil {
		return []*model.ModerationLog{}, nil
	}
	logs, err := s.repo.List(filter)
	if err != nil {
		return nil, err
	}
	if logs == nil {
		return []*model.ModerationLog{}, nil
	}
	return logs, nil
}

// safeGo runs fn in a goroutine, recovering from panics to keep audit-log
// writes from taking down the request handler thread. Mirrors the private
// helper of the same name in internal/core/note/note_create_service.go;
// duplicated here intentionally to avoid leaking goroutine helpers across
// package boundaries.
func safeGo(ctx context.Context, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				// 後続 phase で 30+ handler から呼ばれるようになると
				// "どこで panic したか" が原因切り分けに必須になる。
				// debug.Stack の出力サイズはせいぜい数 KB で warn 頻度
				// は production では極めて低いはずなので常時出す。
				slog.WarnContext(ctx, "moderation log: goroutine panic",
					"panic", r,
					"stack", string(debug.Stack()))
			}
		}()
		fn()
	}()
}
