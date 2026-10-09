package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Jobs registers a plugin's background work.
//
// ジョブ名は `plugin:<プラグイン名>:<ジョブ名>` として queue 上の task type に
// なり、`plugin:<プラグイン名>` という専用のキューで動く。本体の task type と
// 衝突しないよう、名前空間は mk-go 側で付ける。
//
// 任意のタイミングで積むには [Context.Queue] を使う。**Routes からも呼べる**
// ので、HTTP ハンドラの中で重い処理を後回しにできる。
type Jobs interface {
	// Handle registers the handler for a named job.
	Handle(name string, h JobHandler)

	// Schedule runs the named job on a cron expression (5-field, UTC).
	// The job must also be registered with Handle.
	//
	// 定期取得のような用途。任意のタイミングで積むなら [Context.Queue]、
	// プロセス内で完結してよい非同期処理なら [Context.Go]。
	Schedule(cron string, name string, payload any)
}

// JobHandler processes one job. Returning an error fails the job.
//
// 再試行は既定で無し。[Queue.Enqueue] に [WithMaxAttempts] を渡したものだけが
// 再試行される (cron は常に再試行しない)。
//
// 再試行しても直らない失敗 (相手が 4xx を返した、など) は [NoRetry] で包んで
// 返すと、残りの回数があってもそこで打ち切る。
//
// **ctx を尊重すること。** 1 回の実行には既定 1 時間の上限があり
// (#2658、queueHandlerDeadlineSeconds。プラグインのジョブにも同じ値が効く)、
// 超えると mk-go は待つのをやめる。
// ctx を見ていればそこで正常終了できるが、無視していると goroutine が
// 残り続ける (Go では goroutine を殺せない)。詳細は
// docs/plugins/authoring.md。
type JobHandler func(ctx context.Context, payload json.RawMessage) error

// ErrNoRetry marks an error from a [JobHandler] or a [NotificationHandler] as
// permanent: the host does not retry the job, even if attempts remain.
//
// errors.Is で判定するので、`fmt.Errorf("...: %w", plugin.ErrNoRetry)` の
// ように包んでもよい。通常は [NoRetry] を使う。
var ErrNoRetry = errors.New("plugin: 再試行しない")

// NoRetry wraps err so that the host does not retry the job (see
// [ErrNoRetry]). It returns nil when err is nil.
//
// 再試行は同じ入力でもう一度呼ぶだけなので、入力が原因の失敗 (投稿が消えて
// いる、権限が無い、など) を何度繰り返しても直らない。そういう失敗はこれで
// 包み、外部 API の一時的な失敗 (5xx、429、timeout) は包まずに返す。
func NoRetry(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrNoRetry, err)
}
