package processors

import (
	"context"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// chunkedUploadGCBatch bounds one run so a large backlog cannot hold the
// maintenance worker for an unbounded time. The cron runs every 15 minutes, so
// a backlog drains over a few runs.
const chunkedUploadGCBatch = 200

// ChunkedUploadReclaimer aborts and deletes expired chunked upload sessions
// (satisfied by drive.Service).
type ChunkedUploadReclaimer interface {
	GCChunkedUploads(ctx context.Context, now time.Time, limit int) (int, error)
}

// ChunkedUploadGCProcessor reclaims expired chunked upload sessions (#2313).
//
// 未完了のマルチパートアップロードは放置すると課金され続けるので、TTL を過ぎた
// セッションは abort してから行を消す。abort に失敗した行はあえて残し、次回の
// 実行で再試行する (消すと追跡不能な孤児になる)。
type ChunkedUploadGCProcessor struct {
	svc ChunkedUploadReclaimer
}

// NewChunkedUploadGCProcessor constructs the processor. A nil service makes
// Handle a no-op, matching how the other maintenance processors treat unwired
// dependencies.
func NewChunkedUploadGCProcessor(svc ChunkedUploadReclaimer) *ChunkedUploadGCProcessor {
	return &ChunkedUploadGCProcessor{svc: svc}
}

// Handle implements the driver handler contract.
func (p *ChunkedUploadGCProcessor) Handle(ctx context.Context, _ driver.Task) error {
	if p.svc == nil {
		return nil
	}
	n, err := p.svc.GCChunkedUploads(ctx, time.Now(), chunkedUploadGCBatch)
	if err != nil {
		// MaxRetry(0) なので error を返しても再試行はされない。次の cron に
		// 任せて、ここではログだけ残す。
		slog.Warn("chunkedUploadGc: reclaim failed", "err", err)
		return err
	}
	if n > 0 {
		slog.Info("chunkedUploadGc: reclaimed expired sessions", "count", n)
	}
	return nil
}
