package queue

import (
	"context"
	"fmt"
	"regexp"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

/*
 * プラグイン専用のキュー (#2818)。
 *
 * **プラグインごとに分ける。** 相乗りさせると、1 つのプラグインが詰まった
 * ときに他のプラグインと本体のジョブが巻き添えになる。名前を分けておけば
 * admin/queue から per-queue に一時停止も再開もできる。
 *
 * cron (#2478) はこれまで maintenance queue に相乗りしていたが、同じ理由で
 * こちらへ移す。
 */

// PluginQueuePrefix namespaces every plugin queue.
//
// **`:` を含めるのが要点。** プラグイン名は小文字英数字とハイフンに限られる
// (plugin.validName) ので、この接頭辞を持つ名前は本体のキューと衝突しない。
const PluginQueuePrefix = "plugin:"

// pluginNamePattern bounds what may become part of a queue key.
//
// **キュー名は Redis のキーになる**ので、ここでも形を確かめる。plugin 側の
// 検証 (plugin.validName) を通ったものは必ずここも通るが、本パッケージは
// plugin を import しない (レイヤが逆になる) ため独立して持つ。
//
// **一致はしない。真の上位集合。** validName は末尾ハイフンと 32 文字超も
// 弾くが、こちらは通す。取りこぼしはあっても誤って拒否はしない側なので、
// この向きの緩さは意図的 — 厳しくすると plugin 側の規則を 2 箇所で保つ
// ことになり、片方だけ変えたときに正当なプラグインが黙って積めなくなる。
var pluginNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// pluginJobNamePattern bounds a job name so the task type stays parseable.
//
// プラグイン名より緩い (大文字とアンダースコアを許す) のは、ジョブ名は
// Redis のキーにならず、プラグイン作者が自分の都合で付けるものだから。
var pluginJobNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// **AllQueueNames には入れない。** あちらは mk-go が常に持つキューの一覧で、
// プラグインのキューは構成で変わる。名前が要る側は driver の Inspector
// (実際に worker が見ているキュー) を引くか、PluginQueuePrefix で判定すること
// — 静的な一覧に混ぜると「設定に無いプラグインのタブが出る」ことになる。

// PluginQueueName returns the queue a plugin's jobs live in.
func PluginQueueName(plugin string) string { return PluginQueuePrefix + plugin }

// PluginTaskType namespaces one job so it cannot collide with mk-go's own
// task types, or with another plugin's.
//
// **enqueue 側と handler 側で同じものを使う。** 別々に組み立てると、片方を
// 変えたときにジョブが「処理者なし」で捨てられる。
func PluginTaskType(plugin, job string) string { return PluginQueuePrefix + plugin + ":" + job }

// PluginQueueNames returns the queue names for the given plugins, skipping
// names that cannot be part of a queue key.
func PluginQueueNames(plugins []string) []string {
	if len(plugins) == 0 {
		return nil
	}
	out := make([]string, 0, len(plugins))
	for _, p := range plugins {
		if !pluginNamePattern.MatchString(p) {
			continue
		}
		out = append(out, PluginQueueName(p))
	}
	if len(out) == 0 {
		// **nil を返す。** driver は len(QueueNames)==0 で判定するので空 slice
		// でも動くが、呼び出し側が `!= nil` で「プラグインがある」と読むのを
		// 防ぐ (実際 pluginJobQueueNames のテストがそう書いている)。
		return nil
	}
	return out
}

// PluginPeerJobName is the reserved job name the host uses for peer delivery.
//
// **`_` 始まりはプラグインが名乗れない** (pluginJobNamePattern が弾く) ので、
// プラグインのジョブと衝突しない。peer のパスが `_peer` 予約なのと同じ考え方。
const PluginPeerJobName = "_peer"

// PluginPeerTaskType is the task type for one peer delivery attempt (#2819).
func PluginPeerTaskType(plugin string) string { return PluginTaskType(plugin, PluginPeerJobName) }

// EnqueuePluginPeer schedules one peer delivery.
//
// **再送はキューに任せる (#2819)。** プロセス内の time.Sleep だと再起動を
// またげず、デプロイのたびに送信中のものが消える。
func (c *Client) EnqueuePluginPeer(ctx context.Context, plugin string, body []byte, opts ...driver.EnqueueOption) error {
	if !pluginNamePattern.MatchString(plugin) {
		return fmt.Errorf("queue: プラグイン名 %q が不正です", plugin)
	}
	// **再試行の既定を明示する。** 渡し忘れると mkq の既定は 0 回 (= 再試行
	// 無し) になる (EnqueuePlugin と同じ理由)。
	// **retention を付ける (#1193 の再発防止)。** 本体の enqueue helper は全て
	// `retentionOpts` を前置しているが、プラグインの 2 経路だけ付けていなかった。
	// 出さないと driver 既定 = 無制限保持になり、`_peer` 送信のたびに completed
	// ジョブが Redis へ永久に積まれる (peer のエンベロープはプラグインが決める
	// 任意の本文で、利用者のノートを載せうる)。
	//
	// **cron 発火はここを通らない。** プラグインの定期実行は
	// `Scheduler.RegisterPluginJob` 経由で、retention は `Scheduler.register` が
	// 付ける (7 日の期限。mkq v1.2.0 / mkq#109)。
	base := []driver.EnqueueOption{
		driver.WithQueue(PluginQueueName(plugin)),
		driver.WithMaxRetry(0),
	}
	base = append(base, c.retentionOpts(PluginQueueName(plugin))...)
	all := append(base, opts...)
	return c.inner.Enqueue(ctx, PluginPeerTaskType(plugin), body, all...)
}

// EnqueuePlugin adds a job to a plugin's own queue.
//
// **再試行は既定で無し。** 冪等かどうかはプラグインしか知らないので、黙って
// 有効にはしない (plugin.WithMaxAttempts で明示する)。
func (c *Client) EnqueuePlugin(ctx context.Context, plugin, job string, body []byte, opts ...driver.EnqueueOption) error {
	if !pluginNamePattern.MatchString(plugin) {
		return fmt.Errorf("queue: プラグイン名 %q が不正です", plugin)
	}
	if !pluginJobNamePattern.MatchString(job) {
		// **task type の名前空間を保つ。** 空白や `:` を通すと、別のジョブや
		// 本体の task type と見分けが付かない文字列になる。
		return fmt.Errorf("queue: ジョブ名 %q が不正です (使えるのは英数字とハイフン・アンダースコアのみ)", job)
	}
	// **retention を付ける (#1193 の再発防止)。** 本体の enqueue helper は全て
	// `retentionOpts` を前置しているが、プラグインの 2 経路だけ付けていなかった。
	// 出さないと driver 既定 = 無制限保持になり、cron 発火と `_peer` 送信の
	// たびに completed ジョブが Redis へ永久に積まれる (peer のエンベロープは
	// プラグインが決める任意の本文で、利用者のノートを載せうる)。
	base := []driver.EnqueueOption{
		driver.WithQueue(PluginQueueName(plugin)),
		driver.WithMaxRetry(0),
	}
	base = append(base, c.retentionOpts(PluginQueueName(plugin))...)
	all := append(base, opts...)
	return c.inner.Enqueue(ctx, PluginTaskType(plugin, job), body, all...)
}
