package processors

import (
	"context"

	"github.com/elythia-network/elythia/internal/core/reaction"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// ReactionFlushProcessor handles the periodic reaction buffer flush task.
type ReactionFlushProcessor struct {
	writer reaction.ReactionCountWriter
}

// NewReactionFlushProcessor creates the processor.
//
// NOTE (#1780): upstream BakeBufferedReactionsProcessorService は process() 毎に
// meta.enableReactionsBuffering を再評価して無効ならスキップするが、mk-go は
// buffered/direct の writer 型を起動時に固定する設計で、ここで meta を見て Flush を
// skip すると「起動時 buffering=ON → 実行時 OFF」のとき buffered writer が drain
// されず Redis に delta が滞留する。よって本 processor は meta を見ず常に Flush する
// (buffering OFF 起動時は direct writer で Flush が no-op)。実行時の buffering
// hot-toggle (writer 差し替え + 即時 bake) は別 feature として未対応。
func NewReactionFlushProcessor(writer reaction.ReactionCountWriter) *ReactionFlushProcessor {
	return &ReactionFlushProcessor{writer: writer}
}

// Handle flushes all buffered reaction counts to the database.
func (p *ReactionFlushProcessor) Handle(ctx context.Context, _ driver.Task) error {
	return p.writer.Flush(ctx)
}
