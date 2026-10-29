package plugin

import (
	"context"
	"time"
)

/*
 * プラグイン専用のジョブキュー (#2818)。
 *
 * **ライフサイクルは mk-go が持つ。** worker の起動・ロール判定
 * (MK_ONLY_SERVER / MK_ONLY_QUEUE)・停止時の待ち合わせ・実行時間の上限
 * (queueHandlerDeadlineSeconds) は本体の仕事で、プラグインが自分で worker を
 * 起こす経路は用意しない。プラグインの数だけ同じバグを書くことになるため。
 *
 * ジョブは `plugin:<プラグイン名>` という専用のキューに入る。admin/queue から
 * 見えるので、詰まっていることに運営者が気付ける。
 *
 * **mkq を直接は渡さない。** 本体の worker はペイロードを {type, bodyJson}
 * (JSON として埋め込めないものは {type, body}。#3614) で包んで type で
 * dispatch するので、素の mkq ハンドルから積んだジョブは
 * 処理者が見つからない。包み方は本体の enqueue と worker の間の契約であって、
 * プラグインに晒す面ではない。
 */

// Queue enqueues jobs for this plugin.
//
// 名前は [Jobs.Handle] に登録したものと同じものを使う。登録が無い名前で積むと、
// ジョブは処理者なしとして失敗する (再試行はしない)。
type Queue interface {
	// Enqueue adds a job to this plugin's queue.
	//
	// payload は JSON にできること。ハンドラには [JobHandler] の
	// json.RawMessage として渡る。
	//
	// name は英数字とハイフン・アンダースコアのみ (先頭は英数字)。task type の
	// 名前空間を保つため、空白や `:` は受け付けない。
	Enqueue(ctx context.Context, name string, payload any, opts ...EnqueueOption) error
}

// EnqueueOptions are the knobs [Queue.Enqueue] accepts.
//
// **ゼロ値が既定。** 遅延なし・再試行なし・重複排除なしで積む。
type EnqueueOptions struct {
	// Delay holds the job before it becomes runnable.
	Delay time.Duration
	// MaxAttempts is the total number of tries, including the first one.
	// 0 と 1 はどちらも「再試行しない」。
	MaxAttempts int
	// DedupTTL suppresses a job with the same name and payload within the
	// window. 0 は重複排除なし。
	DedupTTL time.Duration
}

// EnqueueOption configures one Enqueue call.
type EnqueueOption func(*EnqueueOptions)

// WithDelay makes the job runnable only after d.
func WithDelay(d time.Duration) EnqueueOption {
	return func(o *EnqueueOptions) { o.Delay = d }
}

// WithMaxAttempts sets the total number of tries, including the first.
//
// **既定は再試行しない。** 再試行はハンドラが冪等であることを前提にするので、
// 黙って有効にはしない。
//
// 頼むと**指数バックオフが自動で付く** (10 秒起点)。付けないと、落ちている
// 取得先を遅延 0 で連打したうえ delayed bucket にも滞在しない。
func WithMaxAttempts(n int) EnqueueOption {
	return func(o *EnqueueOptions) { o.MaxAttempts = n }
}

// WithDedup suppresses an identical job enqueued within ttl.
//
// 判定は「同じジョブ名 + 同じペイロード」。取り寄せの二重起動を防ぐ用途。
//
// **抑制されたときも Enqueue は nil を返す。** 積めたかどうかは区別できない
// ので、結果が要るなら payload に自前の ID を入れて状態を別に持つこと。
func WithDedup(ttl time.Duration) EnqueueOption {
	return func(o *EnqueueOptions) { o.DedupTTL = ttl }
}
