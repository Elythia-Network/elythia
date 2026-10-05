package server

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
)

// secretBearingConfig builds a config where every secret-carrying field holds a
// distinctive value, so a leak is unmistakable.
func secretBearingConfig() *config.Config {
	return &config.Config{
		URL:  "https://example.com",
		Port: 3000,
		ID:   "aidx",
		DB: config.DBOptions{
			Host: "db.internal", Port: 5432, DB: "misskey", User: "mk",
			Pass: "LEAK-db-pass",
		},
		Redis:             config.RedisOptions{Host: "r", Port: 6379, Pass: "LEAK-redis-pass"},
		RedisForJobQueue:  config.RedisOptions{Host: "r", Port: 6379},
		RedisForPubsub:    config.RedisOptions{Host: "r", Port: 6379},
		RedisForTimelines: config.RedisOptions{Host: "r", Port: 6379},
		RedisForReactions: config.RedisOptions{Host: "r", Port: 6379},
		SetupPassword:     "LEAK-setup-password",
		MediaProxySecret:  []byte("LEAK-media-proxy-secret"),
		Proxy:             "http://user:LEAK-proxy-pass@proxy.internal:3128",
		ProxySMTP:         "http://user:LEAK-smtp-pass@smtp-proxy.internal:3128",
	}
}

// **秘密が出力に混じらないことを固定する。** 診断のための機能が認証情報の
// 漏洩経路になってはいけない。新しい設定項目を dump に足すときは、ここに
// 値を積んで落ちないことを確かめること。
func TestBuildConfigDump_NeverLeaksSecrets(t *testing.T) {
	d := BuildConfigDump(secretBearingConfig(), config.RoleBoth)
	rendered := RenderConfigDump(d)

	for _, secret := range []string{
		"LEAK-db-pass", "LEAK-redis-pass", "LEAK-setup-password",
		"LEAK-media-proxy-secret", "LEAK-proxy-pass", "LEAK-smtp-pass",
	} {
		assert.NotContainsf(t, rendered, secret, "%s が出力に漏れている", secret)
	}
}

// 有無は分かるようにする。「未設定」と「設定済み」の区別は診断に要る。
func TestBuildConfigDump_ShowsWhetherSecretsAreSet(t *testing.T) {
	set := RenderConfigDump(BuildConfigDump(secretBearingConfig(), config.RoleBoth))
	assert.Contains(t, set, redactedPlaceholder)

	bare := &config.Config{URL: "https://example.com", Port: 3000}
	unset := RenderConfigDump(BuildConfigDump(bare, config.RoleBoth))
	assert.Contains(t, unset, unsetPlaceholder)
}

// proxy の host は診断に要るので残し、認証情報だけ落とす。
func TestRedactURLUserinfo(t *testing.T) {
	assert.Equal(t, "http://<マスク>@proxy.internal:3128",
		redactURLUserinfo("http://user:pw@proxy.internal:3128"))
	// userinfo が無ければそのまま。
	assert.Equal(t, "http://proxy.internal:3128", redactURLUserinfo("http://proxy.internal:3128"))
	assert.Equal(t, unsetPlaceholder, redactURLUserinfo(""))
	// parse できない値でも落とさない (診断が止まる方が困る)。
	assert.NotEmpty(t, redactURLUserinfo("://bad"))
}

// **リミッタは queue 単位。worker 数を掛けない** (#2669)。
//
// 掛けた値を出していると、読んだ operator が狙ったレートを worker 数で
// 割って設定し直し、配送が 1/N に絞られる。#2640 で server.go と docs は
// 直ったのに dump だけ古い理解が残っていた。
func TestBuildConfigDump_RateIsPerQueueNotPerWorker(t *testing.T) {
	four, hundred := 4, 100
	cfg := &config.Config{
		URL: "https://example.com", Port: 3000,
		DeliverJobConcurrency: &four,
		DeliverJobPerSec:      &hundred,
	}
	d := BuildConfigDump(cfg, config.RoleBoth)

	var rate, note string
	for _, e := range d.Effective {
		if e.Key == "rate: deliver" {
			rate, note = e.Value, e.Note
		}
	}
	assert.Equal(t, "100 jobs/sec", rate, "設定値そのままが queue 全体の上限")
	assert.Contains(t, note, "worker 数を増やしても変わらない")

	// **worker 数を掛けた値がどこにも出ないこと。** value だけ見ていると、
	// 同じ嘘を note や warning に移すだけで通ってしまう (実際に mutation で
	// 両方すり抜けた)。3 つとも塞ぐ。
	assert.NotContains(t, rate, "400")
	assert.NotContains(t, note, "400", "note にも掛け算を書かない")
	assert.Empty(t, d.Warnings, "worker 数由来の警告は出さない")
}

// worker 数を変えても rate 行は動かない。
func TestBuildConfigDump_RateUnaffectedByWorkerCount(t *testing.T) {
	hundred := 100
	rateFor := func(workers int) string {
		cfg := &config.Config{
			URL: "https://example.com", Port: 3000,
			DeliverJobConcurrency: &workers,
			DeliverJobPerSec:      &hundred,
		}
		for _, e := range BuildConfigDump(cfg, config.RoleBoth).Effective {
			if e.Key == "rate: deliver" {
				return e.Value
			}
		}
		return ""
	}
	assert.Equal(t, rateFor(1), rateFor(16), "worker 数はリミッタに影響しない")
	assert.Equal(t, "100 jobs/sec", rateFor(16))
}

// リミッタは queue ごとに 1 本の Redis キーなので、**プロセスを跨いで**
// 効くし worker 数にも比例しない。driver を 1 つにした (#2985) ので出し分けも
// 無くなったが、#2669 が直した誤りはどれも「note に嘘の条件を足す」形だった
// ので、嘘のほうを禁止語で固定しておく。
//
// **値も併せて固定する。** note だけ見ていると、値に worker 数を掛ける変更が
// 誰にも気づかれずに通る (#2669 の mutation で実際にすり抜けた)。
func TestBuildConfigDump_RateNoteIsDriverIndependent(t *testing.T) {
	four, hundred := 4, 100
	entry := func(driverName string) ConfigEntry {
		cfg := &config.Config{
			URL: "https://example.com", Port: 3000,
			JobQueueDriver:        driverName,
			DeliverJobConcurrency: &four,
			DeliverJobPerSec:      &hundred,
		}
		for _, e := range BuildConfigDump(cfg, config.RoleBoth).Effective {
			if e.Key == "rate: deliver" {
				return e
			}
		}
		return ConfigEntry{}
	}

	for _, driverName := range []string{"", "mkq"} {
		e := entry(driverName)
		assert.Equal(t, "100 jobs/sec", e.Value,
			"driver %q でも設定値そのまま (worker 数を掛けない)", driverName)
		assert.NotContains(t, e.Note, "400", "driver %q", driverName)

		// 「プロセス内 / ごと / 単位 / あたり」はどれも同じ誤り。真である
		// 「プロセスを跨いで効く」は通すので `プロセス` 全体は禁じない。
		assert.NotRegexp(t, `プロセス(内|ごと|単位|あたり)`, e.Note, "driver %q", driverName)
		// starvation は共有 worker pool を持つ driver の性質だった。mkq は
		// pull レイヤなので起きない。
		assert.NotContains(t, e.Note, "worker pool", "driver %q", driverName)
	}

	// 既定が mkq であることは、文言に依存せず行ごと比較して固定する。
	assert.Equal(t, entry("mkq"), entry(""), "既定は mkq と同じ行になる")
}

// rate 未設定なら rate 行を出さない。
func TestBuildConfigDump_NoRateEntryWhenUnset(t *testing.T) {
	d := BuildConfigDump(&config.Config{URL: "https://example.com", Port: 3000}, config.RoleBoth)
	for _, e := range d.Effective {
		assert.NotContains(t, e.Key, "rate: ")
	}
	assert.Empty(t, d.Warnings)
}

// 既定値と明示指定を区別する。どこから来た値かが分からないと診断に使えない。
func TestBuildConfigDump_MarksExplicitOverrides(t *testing.T) {
	eight := 8
	cfg := &config.Config{URL: "https://example.com", Port: 3000, InboxJobConcurrency: &eight}
	d := BuildConfigDump(cfg, config.RoleBoth)

	notes := map[string]string{}
	for _, e := range d.Effective {
		notes[e.Key] = e.Note
	}
	assert.Equal(t, "明示指定", notes["worker: inbox"])
	assert.Equal(t, "既定値", notes["worker: deliver"])
}

// role が反映されること (#2459)。
func TestBuildConfigDump_ShowsProcessRole(t *testing.T) {
	for _, tc := range []struct {
		role config.ProcessRole
		want string
	}{
		{config.RoleBoth, "both"},
		{config.RoleServer, "server"},
		{config.RoleQueue, "queue"},
	} {
		d := BuildConfigDump(&config.Config{URL: "u", Port: 1}, tc.role)
		var got string
		for _, e := range d.Effective {
			if e.Key == "process role" {
				got = e.Value
			}
		}
		assert.Equal(t, tc.want, got)
	}
}

// socket 指定時は port を出さない (使われないので出すと誤解を招く)。
func TestBuildConfigDump_SocketHidesPort(t *testing.T) {
	d := BuildConfigDump(&config.Config{URL: "u", Port: 3000, Socket: "/run/mk.sock"}, config.RoleBoth)
	keys := map[string]bool{}
	for _, e := range d.Settings {
		keys[e.Key] = true
	}
	assert.True(t, keys["socket"])
	assert.False(t, keys["port"])
}

// clusterLimit は mk-go では no-op。設定されていたらその旨を示す。
func TestBuildConfigDump_ClusterLimitIsMarkedNoOp(t *testing.T) {
	n := 4
	d := BuildConfigDump(&config.Config{URL: "u", Port: 1, ClusterLimit: &n}, config.RoleBoth)
	var note string
	for _, e := range d.Effective {
		if e.Key == "clusterLimit" {
			note = e.Note
		}
	}
	assert.Contains(t, note, "no-op")
}

// TestBuildConfigDump_StuckAndDeadlineRows pins the diagnostic rows added for
// #2657 / #2658. **上限は autoscale の有無で変わる** — 管理下のキューは
// maxWorkers まで伸びるので、設定値で語ると過小申告になる。
func TestBuildConfigDump_StuckAndDeadlineRows(t *testing.T) {
	maxW := 128
	minW := 4
	cfg := &config.Config{
		URL:               "https://example.com",
		JobQueueDriver:    "mkq",
		JobQueueAutoScale: true,
		MaxWorkers:        &maxW,
		MinWorkers:        &minW,
	}
	d := BuildConfigDump(cfg, config.RoleBoth)

	find := func(key string) (string, string, bool) {
		for _, e := range d.Effective {
			if e.Key == key {
				return e.Value, e.Note, true
			}
		}
		return "", "", false
	}

	// 既定値は driver から取る (dump だけ嘘になるのを防ぐ)。
	v, _, ok := find("handler 期限")
	require.True(t, ok)
	assert.Equal(t, "1h0m0s (既定)", v)

	// inbox は隔離対象。autoscale 管理下なので上限は maxWorkers 基準。
	v, note, ok := find("stuck 検出: inbox")
	require.True(t, ok)
	assert.Equal(t, "30m0s", v)
	assert.Contains(t, note, "maxWorkers 基準")
	assert.Contains(t, note, "144") // 128 + max(16,4)

	// export は隔離対象外だが、放棄の予算は効く。こちらも autoscale 管理下。
	v, note, ok = find("stuck 検出: export")
	require.True(t, ok)
	assert.Equal(t, "無効", v)
	assert.Contains(t, note, "handler の期限による漏れ")
	assert.Contains(t, note, "132") // 128 + max(2,4)

	// maintenance は autoscale 管理外なので設定値基準。
	_, note, ok = find("stuck 検出: maintenance")
	require.True(t, ok)
	assert.Contains(t, note, "6") // 2 + max(2,4)
	assert.NotContains(t, note, "maxWorkers 基準")
}

// TestBuildConfigDump_RateProductAppearsNowhere is the structural gate for
// #2669.
//
// **積は dump のどこにも出ないし、rate 行は設定値そのものしか出さない。**
// 行や field を 1 つずつ pin する方式では嘘の移設を追いきれなかった —
// round 1 で note へ、round 2 で driver ごとの値へ、round 3 で worker 行 /
// Warnings / 別 queue へ、round 4 で「特定の config 形でだけ掛ける」形へと、
// 毎回別の場所に同じ主張が復活した。**出力全体**と**行の集合そのもの**を
// 軸にする。
//
// config の形を表で回すのは、掛け算を「autoscale のときだけ」「concurrency を
// 明示していないときだけ」のように条件付きで戻す変更を捕まえるため。すぐ下の
// stuck 検出ループが実際に autoscale で分岐しているので、rate 側にも同じ分岐を
// 足すのは十分ありうる変更。
func TestBuildConfigDump_RateProductAppearsNowhere(t *testing.T) {
	dConc, iConc, rConc := 4, 16, 4
	// 積 404 / 1648 / 428 が他の設定値と衝突しない値を選ぶ。
	dRate, iRate, rRate := 101, 103, 107
	wantRows := map[string]string{
		"rate: deliver":      "101 jobs/sec",
		"rate: inbox":        "103 jobs/sec",
		"rate: relationship": "107 jobs/sec",
	}

	shapes := map[string]func(*config.Config){
		"concurrency 明示": func(c *config.Config) {
			c.DeliverJobConcurrency, c.InboxJobConcurrency, c.RelationshipJobConcurrency = &dConc, &iConc, &rConc
		},
		"concurrency 未指定": func(*config.Config) {},
		"autoscale 有効": func(c *config.Config) {
			c.JobQueueAutoScale = true
		},
		"queue 専用ロール": func(c *config.Config) {
			c.DeliverJobConcurrency = &dConc
		},
	}

	for shape, apply := range shapes {
		for _, driver := range []string{"", "mkq"} {
			t.Run(shape+"/"+driver, func(t *testing.T) {
				cfg := &config.Config{
					URL: "https://example.com", Port: 3000,
					JobQueueDriver:        driver,
					DeliverJobPerSec:      &dRate,
					InboxJobPerSec:        &iRate,
					RelationshipJobPerSec: &rRate,
				}
				apply(cfg)
				role := config.RoleBoth
				if shape == "queue 専用ロール" {
					role = config.RoleQueue
				}
				d := BuildConfigDump(cfg, role)
				rendered := RenderConfigDump(d)

				// **rate 行の集合そのものを固定する。** substring では
				// 「別名の行を足して積を載せる」「単位を変えて載せる」が
				// すり抜ける。
				got := map[string]string{}
				for _, e := range d.Effective {
					if strings.HasPrefix(e.Key, "rate: ") {
						got[e.Key] = e.Value
					}
				}
				assert.Equal(t, wantRows, got, "rate 行は設定値そのものだけ")

				// 積が dump のどこにも出ないこと (Warnings も
				// RenderConfigDump の出力に含まれる)。
				for _, product := range []string{"404", "1648", "428"} {
					assert.NotContains(t, rendered, product,
						"worker 数を掛けた値 %s が出ている", product)
				}
				// 数字を出さない言い換えも塞ぐ。
				for _, phrase := range []string{"worker ごと", "worker を増やせば", "本ぶんの枠"} {
					assert.NotContains(t, rendered, phrase)
				}
				// warning で言い直すのも塞ぐ。数値を含む嘘は上の積チェックが
				// 拾うので、ここは rate 設定名に言及する warning を禁じる。
				for _, w := range d.Warnings {
					assert.NotContains(t, w, "JobPerSec")
				}
			})
		}
	}
}
