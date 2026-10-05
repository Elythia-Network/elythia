package mkqdriver

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/shiroha-a/mkq"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// toMkqAddOptions translates a driver.EnqueueOptions into the
// equivalent []mkq.AddOption list. Unique semantics map to mkq's
// WithUnique (BullMQ deduplication with TTL keyed by queue + taskType
// + payload hash), and MaxRetry maps to WithAttempts (driver exposes
// MaxRetry=N = 初回に加えて N 回、mkq / BullMQ exposes Attempts=N+1 =
// 総試行回数)。
//
// The taskType is needed both as the BullMQ Job.name (so the admin
// UI can identify the task in lists) and as part of the unique key.
// The queue name plus payload bytes are folded into the unique key
// so that the same task type enqueued to different queues, or with
// different payloads, is NOT silently collapsed.
func toMkqAddOptions(o driver.EnqueueOptions, taskType string, payload []byte) []mkq.AddOption {
	out := make([]mkq.AddOption, 0, 4)
	if taskType != "" {
		out = append(out, mkq.WithJobName(taskType))
	}
	if o.MaxRetrySet {
		// driver の MaxRetry(N) は「初回 + N 回 retry」= 合計 N+1 attempts。
		// mkq の WithAttempts は合計 attempts なので +1 する。
		out = append(out, mkq.WithAttempts(o.MaxRetry+1))
	}
	// retry backoff: 未設定 (BackoffType=="") なら mkq は backoff 無し
	// (= 遅延0で即時 retry) になる。deliver / inbox は EnqueueDeliver /
	// EnqueueInbox が backoff を必ず付けて hammering を防ぐ (#1405)。
	//
	// "custom" は BullMQ settings.backoffStrategy 経路: opts JSON には
	// `{type:"custom"}` だけ保存し、delay は worker 側で登録した
	// CustomBackoffFunc (= Misskey httpRelatedBackoff) が算出する (#1406,
	// mkq#67)。built-in の fixed / exponential は delay から driver-side で
	// 計算される。
	switch o.BackoffType {
	case driver.BackoffCustom:
		out = append(out, mkq.WithBackoff(mkq.CustomBackoff()))
	case driver.BackoffExponential:
		if o.BackoffDelay > 0 {
			out = append(out, mkq.WithBackoff(mkq.ExponentialBackoff(o.BackoffDelay)))
		}
	case driver.BackoffFixed:
		if o.BackoffDelay > 0 {
			out = append(out, mkq.WithBackoff(mkq.FixedBackoff(o.BackoffDelay)))
		}
	}
	if o.UniqueTTL > 0 {
		// dedup の単位は (queue, type, payload) — queue / payload が違えば
		// 別ジョブとして許容する。mkq.WithUnique は明示 ID 必須なので
		// queue + taskType + payload hash を結合した文字列を使う。
		//
		// 実際の効き方:
		//   - cleanRemoteNotes / reactionFlush: payload は nil → hash 同値
		//     → queue+taskType ベースで dedup
		//   - deleteAccount: payload に UserID を含む → 異なる user の
		//     job は異なる hash → 独立して enqueue される
		//   - chart cron: payload は nil → 上と同様
		//   - 同 type を異 queue に投げる将来の caller も独立に dedup される
		//     (現状は mk-go 内に該当経路なし)
		//
		// payload は事実上 immutable の前提 (mk-go 内部からの enqueue
		// 経路はすべて構造体を JSON marshal してから渡す) なので、
		// hash 衝突は SHA-256 衝突と同レベルの確率で起きるだけ。
		out = append(out, mkq.WithUnique(uniqueKey(o.Queue, taskType, payload), o.UniqueTTL))
	}
	if o.ProcessIn > 0 {
		out = append(out, mkq.WithDelay(o.ProcessIn))
	}
	if o.KeepFailedSet && o.KeepFailed > 0 {
		// driver-neutral semantic では `WithKeepFailed(0)` = 「retention
		// 無し (= unlimited 蓄積、従来挙動)」だが、mkq native の
		// `WithKeepFailed(0)` は **逆に「failed bucket 即時削除」**
		// (`removeOnFail: true` 相当) で意味が真逆。両 semantic を揃える
		// ため、value=0 のときは mkq に渡さない (= mkq が WithKeepFailed
		// 未呼び出し時の default = unlimited を踏ませる)。
		// value>0 のときだけ N 件 retention として直訳する。BullMQ の
		// `removeOnFail: N` 互換 (#1184)。
		out = append(out, mkq.WithKeepFailed(o.KeepFailed))
	}
	if o.KeepCompletedSet && o.KeepCompleted > 0 {
		// `WithKeepFailed` と同じ 0-skip semantic (上のコメント参照)。
		// mkq native の `WithKeepCompleted(0)` は「即時削除」で
		// driver-neutral の「unlimited 蓄積」と真逆なので skip する。
		// BullMQ の `removeOnComplete: N` 互換 (#1193)。
		out = append(out, mkq.WithKeepCompleted(o.KeepCompleted))
	}
	if o.KeepCompletedAge > 0 {
		// age-based retention は count と独立に効く。両方指定すれば
		// BullMQ TS の `removeOnComplete: {age: 7d, count: 30}` 互換に
		// なる (どちらの条件に引っ掛かっても prune される)。
		out = append(out, mkq.WithKeepCompletedAge(o.KeepCompletedAge))
	}
	if o.KeepFailedAge > 0 {
		out = append(out, mkq.WithKeepFailedAge(o.KeepFailedAge))
	}
	return out
}

// uniqueKey builds a stable mkq dedup key from the queue name, task
// type, and payload bytes. The first 16 hex chars (8 bytes) of
// SHA-256 are enough to keep the BullMQ key short while making
// collision astronomically unlikely for the volumes mk-go realistically
// sees.
func uniqueKey(queue, taskType string, payload []byte) string {
	h := sha256.Sum256(payload)
	return "queue:" + queue + ":" + taskType + ":" + hex.EncodeToString(h[:8])
}
