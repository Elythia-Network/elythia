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
// NOTE (#1780, #3663): upstream BakeBufferedReactionsProcessorService は process()
// 毎に meta.enableReactionsBuffering を再評価して無効ならスキップするが、ここでは
// meta を見ず常に Flush する。無効にした直後に飛ばすと、有効の間に溜まった
// delta が Redis に滞留する。設定に従うのは writer (corereaction.SwitchableWriter)
// の側で、その Flush は設定に関わらず Redis のバッファを DB へ移す。
func NewReactionFlushProcessor(writer reaction.ReactionCountWriter) *ReactionFlushProcessor {
	return &ReactionFlushProcessor{writer: writer}
}

// Handle flushes all buffered reaction counts to the database.
func (p *ReactionFlushProcessor) Handle(ctx context.Context, _ driver.Task) error {
	return p.writer.Flush(ctx)
}
