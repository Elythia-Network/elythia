package server

import (
	"log/slog"

	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	"github.com/elythia-network/elythia/internal/repository"
)

// reactionFlushTick runs one tick of the reaction flush loop: it re-reads
// meta.enableReactionsBuffering, then enqueues a flush when the buffer may hold
// counts. enqueue is nil outside the queue role (only the reload runs).
//
// **tick ごとに meta から読み直す (#3663)。** 切り替えは通常 metaUpdated で
// 届くが、pub/sub を取りこぼしたプロセスは通知でしか変わらないと二度と
// 揃わない。meta はキャッシュ (TTL 5 分) なので、取りこぼしてもその TTL で
// 揃う。読めなければ据え置く。
func reactionFlushTick(meta repository.MetaRepository, w *corereaction.SwitchableWriter, enqueue func() error) error {
	if m, err := meta.Fetch(); err != nil || m == nil {
		slog.Warn("reactions: buffering reload skipped on flush tick (meta unavailable)", "err", err)
	} else {
		w.SetBuffering(m.EnableReactionsBuffering)
	}
	if enqueue == nil || !w.NeedsFlush() {
		return nil
	}
	return enqueue()
}
