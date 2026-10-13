package driver

import (
	"context"
	"log/slog"
)

// JobLogger appends one line to the log of the job being processed (BullMQ
// `job.log`). The admin queue page reads it back with "Load logs".
type JobLogger func(ctx context.Context, line string) error

type jobLoggerKey struct{}

// WithJobLogger returns a context that carries the logger of the running job.
// Drivers call it before invoking the handler.
func WithJobLogger(ctx context.Context, l JobLogger) context.Context {
	return context.WithValue(ctx, jobLoggerKey{}, l)
}

// JobLog appends line to the running job's log.
//
// **書けなくても処理は止めない。** 記録は運営者が後から読むためのもので、
// 書けないことを理由にジョブを失敗させると、掃除そのものが進まなくなる。
// driver がロガーを渡さないとき (テストや別の driver) は何もしない。
//
// **ハンドラの ctx のまま書く。** 取り消された後 (shutdown やロックの喪失) の
// 行は書けずに落ちる。取り消しを外して書くと、ロックを失った後に同じ job を
// 別の worker が拾ったとき、2 つの実行の行が 1 つのログに混ざる (mkq の
// addLog は job の有無しか見ない)。混ざるより欠けるほうを選ぶ。
func JobLog(ctx context.Context, line string) {
	l, ok := ctx.Value(jobLoggerKey{}).(JobLogger)
	if !ok || l == nil {
		return
	}
	if err := l(ctx, line); err != nil {
		slog.Debug("queue: cannot append to the job log", "error", err)
	}
}
