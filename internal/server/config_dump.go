package server

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver/mkqdriver"
)

// ConfigDump is the resolved view of an instance's configuration.
//
// **単に設定を再表示するだけなら価値が薄い。** 中心は Effective で、設定値
// そのものではないが実際に効く値を出す (#2469)。
type ConfigDump struct {
	// Settings are resolved config values (YAML + MK_* env + defaults).
	Settings []ConfigEntry `json:"settings"`
	// Effective are values derived from the settings that the operator
	// cannot read off the config file.
	Effective []ConfigEntry `json:"effective"`
	// Warnings call out combinations that behave differently than the
	// setting names suggest.
	Warnings []string `json:"warnings,omitempty"`
}

// ConfigEntry is one line of the dump.
type ConfigEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Note explains where the value came from or what it implies.
	Note string `json:"note,omitempty"`
}

// redactedPlaceholder marks a secret that is set. **値そのものは決して出さない。**
// 「未設定」と「設定済み」の区別は診断に要るので、有無だけは示す。
const (
	redactedPlaceholder = "<設定済み (マスク)>"
	unsetPlaceholder    = "<未設定>"
)

// orUnset renders an empty optional as an explicit marker. 空欄のまま出すと
// 「設定を出し忘れた」のか「未設定」なのか読み手に分からない。
func orUnset(v string) string {
	if v == "" {
		return unsetPlaceholder
	}
	return v
}

// secretValue renders a secret without leaking it.
func secretValue(v string) string {
	if v == "" {
		return unsetPlaceholder
	}
	return redactedPlaceholder
}

// redactURLUserinfo strips credentials embedded in a proxy URL.
//
// proxy は `http://user:pass@host:port` の形を取りうる。そのまま出すと認証情報が
// 漏れるので userinfo だけ落とす (host は診断に要るので残す)。
func redactURLUserinfo(raw string) string {
	if raw == "" {
		return unsetPlaceholder
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	// `u.User = url.User(...)` だと percent-encode されて読みにくくなるので、
	// scheme と host だけを組み直す。userinfo は**丸ごと落とす** (ユーザー名
	// だけでも認証情報の一部なので残さない)。
	return u.Scheme + "://<マスク>@" + u.Host + u.Path
}

// BuildConfigDump assembles the dump for cfg.
//
// **算出は本番と同じ関数を使う。** 実効値のために別実装を起こすと、表示と実際が
// 食い違ったときにどちらが正しいか分からなくなる。
func BuildConfigDump(cfg *config.Config, role config.ProcessRole) ConfigDump {
	d := ConfigDump{}
	add := func(dst *[]ConfigEntry, key, value, note string) {
		*dst = append(*dst, ConfigEntry{Key: key, Value: value, Note: note})
	}

	add(&d.Settings, "url", cfg.URL, "")
	if cfg.Socket != "" {
		add(&d.Settings, "socket", cfg.Socket, "UDS で listen する (port は使わない)")
	} else {
		add(&d.Settings, "port", fmt.Sprintf("%d", cfg.Port), "")
	}
	add(&d.Settings, "id", cfg.ID, "")
	add(&d.Settings, "jobQueueDriver", driverName(cfg), "")

	add(&d.Settings, "db.host", cfg.DB.Host, "")
	add(&d.Settings, "db.port", fmt.Sprintf("%d", cfg.DB.Port), "")
	add(&d.Settings, "db.db", cfg.DB.DB, "")
	add(&d.Settings, "db.user", cfg.DB.User, "")
	add(&d.Settings, "db.pass", secretValue(cfg.DB.Pass), "")

	addRedis(&d.Settings, "redis", cfg.Redis)
	addRedis(&d.Settings, "redisForJobQueue", cfg.RedisForJobQueue)
	addRedis(&d.Settings, "redisForPubsub", cfg.RedisForPubsub)
	addRedis(&d.Settings, "redisForTimelines", cfg.RedisForTimelines)
	addRedis(&d.Settings, "redisForReactions", cfg.RedisForReactions)

	add(&d.Settings, "setupPassword", secretValue(cfg.SetupPassword), "")
	add(&d.Settings, "mediaProxySecret", secretValue(string(cfg.MediaProxySecret)),
		"未設定なら DB の instance_secret から導出する")
	add(&d.Settings, "pluginSecretKey", secretValue(string(cfg.PluginSecretKey)),
		"未設定ならプラグインの秘密の値を保存できない (#3470)")
	add(&d.Settings, "proxy", redactURLUserinfo(cfg.Proxy), "")
	add(&d.Settings, "proxySmtp", redactURLUserinfo(cfg.ProxySMTP), "")
	add(&d.Settings, "frontendContentSecurityPolicy", orUnset(cfg.FrontendContentSecurityPolicy), "")
	add(&d.Settings, "enableMetrics", fmt.Sprintf("%t", cfg.EnableMetrics), "")
	add(&d.Settings, "testMode", fmt.Sprintf("%t", cfg.TestMode), "")
	add(&d.Settings, "dev", fmt.Sprintf("%t", cfg.Dev), "")

	// --- 実効値 ---

	add(&d.Effective, "process role", string(role), roleNote(role))

	// **設定名からは読めない実効値 (#2477)。** dev では frontend が
	// ビルド成果物ではなく Vite dev server から配信される。本番で有効になって
	// いると frontend が丸ごと落ちるので、診断で必ず見えるようにする。
	if cfg.Dev {
		add(&d.Effective, "frontend 配信元", "Vite dev server ("+viteDevServerURL+")",
			"dev モード。ビルド成果物があっても使わない")
		d.Warnings = append(d.Warnings,
			"dev が有効です。frontend は Vite dev server から配信されるため、dev server を起動していないと frontend が表示されません (本番では無効にしてください)")
	} else {
		add(&d.Effective, "frontend 配信元", "ビルド成果物", "")
	}

	queues := append([]string(nil), mkqdriver.QueueNames...)
	sort.Strings(queues)
	override := perQueueConcurrencyFromConfig(cfg)
	rates := perQueueRatesFromConfig(cfg)
	conc := mkqdriver.ResolveQueueConcurrency(queues, override)

	for _, q := range queues {
		note := "既定値"
		if _, ok := override[q]; ok {
			note = "明示指定"
		}
		add(&d.Effective, "worker: "+q, fmt.Sprintf("%d", conc[q]), note)
	}

	// **リミッタは queue 単位で効く。worker 数を掛けない。**
	//
	// mkq はリミッタの実体が BullMQ 互換の `bull:<queue>:limiter` という
	// queue ごとに 1 本の Redis キーで、pool 内の全 Worker がそれを INCR
	// する。合計は設定値のままで、worker 数には比例しない。
	//
	// ここには以前 `設定値 x worker 数` を「実際の上限」として出す実装と
	// warning があったが**誤りだった** (#2669)。#2640 で server.go と
	// docs は直っていたのに、この dump だけ古い理解が残っていた。
	// 読んだ operator は狙ったレートを worker 数で割って設定し直すので、
	// **配送が 1/N に絞られる**。
	for _, q := range queues {
		r, ok := rates[q]
		if !ok || r <= 0 {
			continue
		}
		add(&d.Effective, "rate: "+q, fmt.Sprintf("%d jobs/sec", r),
			"queue 全体の上限。worker 数を増やしても変わらない")
	}

	// 詰まり検出はキューごとに効いたり効かなかったりするうえ、効いている
	// キューでは worker が設定値を超えて走りうる (#2657)。設定ファイルを
	// 読んだだけでは分からないので出す。
	stuck := stuckWorkerAfter(cfg)
	managed := map[string]bool{}
	if cfg.JobQueueAutoScale {
		names, _ := autoScaledQueues(cfg)
		for _, q := range names {
			managed[q] = true
		}
	}
	_, autoMax := resolveAutoScaleBounds(cfg)
	for _, q := range queues {
		// 実 worker 数の上限は「到達した最大 worker 数 + 漏れの許容幅」。
		// autoscale 管理下なら到達しうる最大は maxWorkers なので、設定値で
		// 語ると過小申告になる。peakDesired は起動時に設定値から始まり単調
		// 非減少なので、maxWorkers が設定値より小さい構成では設定値が上限。
		//
		// **隔離の有無で分岐させない。** 漏れの予算は放棄した handler にも
		// 効くので、隔離が無効なキューでも同じ上限が要る (#2658)。
		peak := conc[q]
		managedNote := ""
		if managed[q] && autoMax > peak {
			peak = autoMax
			managedNote = " (autoscale 管理下なので maxWorkers 基準)"
		}
		ceiling := peak + mkqdriver.QuarantineHeadroomFor(conc[q])

		th := mkqdriver.StuckWorkerThreshold(q, stuck)
		if th <= 0 {
			// **隔離が無効でも漏れの予算は効く** (#2658)。放棄した handler が
			// 積もれば、このキューの worker も 0 まで縮んで止まる。
			add(&d.Effective, "stuck 検出: "+q, "無効",
				fmt.Sprintf("隔離は無効 (長い batch job が正常なキュー、または queueStuckWorkerSeconds が負値)。ただし handler の期限による漏れは最大 %d 本まで許容し、達すると worker を 0 にする%s",
					ceiling, managedNote))
			continue
		}
		note := "超過した worker は勘定から外して差し替える" + managedNote
		add(&d.Effective, "stuck 検出: "+q, th.String(),
			fmt.Sprintf("%s。実 worker 数は最大 %d 本 (放棄した handler と共用の枠)", note, ceiling))
	}

	deadline := handlerDeadline(cfg)
	switch {
	case deadline < 0:
		add(&d.Effective, "handler 期限", "無効",
			"queueHandlerDeadlineSeconds が負値。戻らない handler は worker を永久に失う")
	case deadline == 0:
		// 既定値をここに直書きしない (driver 側を変えたときに dump だけ嘘になる)。
		// 免除表に無い task type なら既定が返る。
		add(&d.Effective, "handler 期限",
			mkqdriver.HandlerDeadlineFor(queue.TaskTypeInbox, 0).String()+" (既定)",
			"batch 系 task type (export / cleanRemoteFiles / deleteAccount 等) は対象外")
	default:
		add(&d.Effective, "handler 期限", deadline.String(),
			"明示指定。batch 系 task type は対象外")
	}

	add(&d.Effective, "redis pool (job queue)",
		fmt.Sprintf("%d", mkqdriver.WorkerPoolSize(queues, override, stuck)),
		"poolSize 未指定時の自動サイジング結果 (stuck 検出の許容ぶんを含む)")

	addPluginSettings(&d.Effective, cfg)

	add(&d.Effective, "maxFileSize", fmt.Sprintf("%d bytes", cfg.MaxFileSize), "")
	if cfg.ClusterLimit != nil {
		add(&d.Effective, "clusterLimit", fmt.Sprintf("%d", *cfg.ClusterLimit),
			"mk-go では **no-op**。goroutine で処理するので Node の cluster に相当する概念が無い")
	}
	return d
}

// addPluginSettings renders the `plugins:` section (#2482).
//
// **`enabled` 以外の値は既定でマスクする。** プラグインの設定キーは mk-go から
// 見て未知なので、どれが秘密かを判別できない。既定を「出す」にすると、
// プラグインが増えるたびに漏洩の機会が生まれる。
//
// キー名と「設定されているか」は診断に要るので残す (値だけを隠す)。
func addPluginSettings(dst *[]ConfigEntry, cfg *config.Config) {
	if len(cfg.Plugins) == 0 {
		return
	}
	names := make([]string, 0, len(cfg.Plugins))
	for name := range cfg.Plugins {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		settings := cfg.Plugins[name]

		state := "有効"
		if !pluginEnabled(settings) {
			state = "無効 (enabled: false)"
		}
		*dst = append(*dst, ConfigEntry{Key: "plugin: " + name, Value: state})

		keys := make([]string, 0, len(settings))
		for k := range settings {
			if k != enabledKey {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			*dst = append(*dst, ConfigEntry{
				Key:   "  " + name + "." + k,
				Value: redactedPlaceholder,
				Note:  "プラグインの設定は既定でマスクする",
			})
		}
	}
}

// driverName normalises the empty job queue driver to its default.
func driverName(cfg *config.Config) string {
	if cfg.JobQueueDriver == "" {
		return "mkq (既定)"
	}
	return cfg.JobQueueDriver
}

// roleNote explains what the role implies.
func roleNote(role config.ProcessRole) string {
	switch role {
	case config.RoleServer:
		return "HTTP のみ。ジョブは積むが処理しない (MK_ONLY_SERVER)"
	case config.RoleQueue:
		return "ジョブのみ。API は生えず /healthz だけ応答する (MK_ONLY_QUEUE)"
	default:
		return "1 プロセスで HTTP とジョブの両方を担う"
	}
}

// addRedis appends the connection fields for one Redis role.
func addRedis(dst *[]ConfigEntry, prefix string, o config.RedisOptions) {
	target := fmt.Sprintf("%s:%d", o.Host, o.Port)
	if o.Path != "" {
		target = o.Path + " (UDS)"
	}
	note := ""
	if o.Prefix != "" {
		note = "prefix=" + o.Prefix
	}
	*dst = append(*dst, ConfigEntry{Key: prefix, Value: target, Note: note})
	if o.Pass != "" {
		*dst = append(*dst, ConfigEntry{Key: prefix + ".pass", Value: redactedPlaceholder})
	}
}

// RenderConfigDump formats the dump for a terminal.
func RenderConfigDump(d ConfigDump) string {
	var b strings.Builder
	section := func(title string, entries []ConfigEntry) {
		if len(entries) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s\n", title)
		width := 0
		for _, e := range entries {
			if len(e.Key) > width {
				width = len(e.Key)
			}
		}
		for _, e := range entries {
			fmt.Fprintf(&b, "  %-*s  %s", width, e.Key, e.Value)
			if e.Note != "" {
				fmt.Fprintf(&b, "   (%s)", e.Note)
			}
			b.WriteString("\n")
		}
	}
	section("設定値", d.Settings)
	section("実効値", d.Effective)
	if len(d.Warnings) > 0 {
		b.WriteString("\n注意\n")
		for _, w := range d.Warnings {
			fmt.Fprintf(&b, "  - %s\n", w)
		}
	}
	b.WriteString("\n")
	return b.String()
}
