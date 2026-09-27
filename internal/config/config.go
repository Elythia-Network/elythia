package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/shiroha-a/mk/internal/misc/password"
	"github.com/spf13/viper"
)

// MkGoVersion is the mk-go version. Override at build time via:
//
//	go build -ldflags "-X github.com/shiroha-a/mk/internal/config.MkGoVersion=1.4.0"
var MkGoVersion = "1.4.0"

// MisskeyVersion is the compatible Misskey version. Override at build time via:
//
//	go build -ldflags "-X github.com/shiroha-a/mk/internal/config.MisskeyVersion=2026.9.1"
var MisskeyVersion = "2026.9.1"

// MkGoCommit is the source revision this binary was built from (short hash).
//
// **空になることがある。** 埋めるのはビルド側 (`make build` / Dockerfile の
// build-arg) で、`go build ./...` や `go run` では入らない。読む側は空を
// 「不明」として扱い、表示しないこと。Override at build time via:
//
//	go build -ldflags "-X github.com/shiroha-a/mk/internal/config.MkGoCommit=abc1234"
var MkGoCommit = ""

// MkGoFrontendVersion is the version of the fork frontend bundled with this
// build (`third_party/misskey`), typically its git tag such as "2026.9.0-mk.3".
//
// **frontend を bind mount で差し替えている構成では実物とずれうる。** これが
// 名乗るのは「このバイナリをビルドしたときの submodule pin」で、`make uds-rebuild`
// のように両方を同時にビルドする経路でしか一致は保証されない。MkGoCommit と同じく
// 空になりうる。Override at build time via:
//
//	go build -ldflags "-X github.com/shiroha-a/mk/internal/config.MkGoFrontendVersion=2026.9.0-mk.3"
var MkGoFrontendVersion = ""

// MkGoRepositoryURL is the canonical source repository of mk-go itself.
//
// AGPL-3.0 section 13 で求められる「動いているコードに対応するソース」の案内は
// meta.repositoryUrl (operator が改変版を指せる) が担うが、その既定値と
// nodeinfo の software.repository はどちらも mk-go 本体を指すため、値をここに
// 一本化する (#2700)。
const MkGoRepositoryURL = "https://github.com/shiroha-a/mk"

// MkGoFeedbackURL is the default destination of `meta.feedbackUrl`.
//
// **これは「ソフトウェアへのフィードバック先」** で、upstream が列 DEFAULT に
// `.../misskey/issues/new` を置いているのと同じ位置づけ。「このサーバーへの
// フィードバック」を受けたい operator は admin 画面の**ブランディング**で上書きする
// (`repositoryUrl` は「全般 → 情報」だが、`feedbackUrl` の入力欄はそちらに無い、#2891)。
//
// **`internal/repository` からは参照されない** — あちらは config に依存しない方針なので
// 値を持ち直しており、一致は `TestDefaultFeedbackURLMatchesConfig` が固定する。この定数は
// その突き合わせ先として置いてある (未使用に見えても消さないこと)。
//
// **`/issues/new` の組み立ては GitHub / Codeberg 前提。** ホストを移すときは
// `MkGoRepositoryURL` だけ変えても成立しない (GitLab 系は `/-/issues/new`)。
const MkGoFeedbackURL = MkGoRepositoryURL + "/issues/new"

// RedisOptions represents Redis connection configuration.
type RedisOptions struct {
	Host string `mapstructure:"host"`
	// Path is an ioredis-style alias for connecting via a UNIX domain socket.
	// Misskey TS は内部で ioredis を使っており、`example.yml` の "You can
	// specify more ioredis options..." 経由で `redis.path: /run/valkey.sock`
	// を書いている operator が一定数いる。同じ config を mk と共有する drop-in
	// 切替で詰まらないよう、Path が指定されていれば Host より優先して unix
	// socket 接続にする (#519)。
	Path     string `mapstructure:"path"`
	Port     int    `mapstructure:"port"`
	Family   int    `mapstructure:"family"`
	Pass     string `mapstructure:"pass"`
	DB       int    `mapstructure:"db"`
	Prefix   string `mapstructure:"prefix"`
	Username string `mapstructure:"username"`
	// Pool tuning (Go固有。Misskey TS設定にはない)
	PoolSize *int `mapstructure:"poolSize"`
}

// DBOptions represents PostgreSQL connection configuration.
type DBOptions struct {
	Host         string `mapstructure:"host"`
	Port         int    `mapstructure:"port"`
	DB           string `mapstructure:"db"`
	User         string `mapstructure:"user"`
	Pass         string `mapstructure:"pass"`
	DisableCache bool   `mapstructure:"disableCache"`
	// Extra mirrors upstream's db.extra (passed through to node-postgres).
	// mk-go only reads the TLS keys; see ResolveDBTLS. 値の型は YAML の
	// ままにする — string に weak decode すると bool の true が "1" になり、
	// upstream の object 形式 (ssl: { rejectUnauthorized: false }) は読めずに
	// 起動が落ちていた。
	Extra map[string]any `mapstructure:"extra"`
	// Pool tuning (Go固有。Misskey TS設定にはない)
	MaxOpenConns    *int `mapstructure:"maxOpenConns"`
	MaxIdleConns    *int `mapstructure:"maxIdleConns"`
	ConnMaxLifetime *int `mapstructure:"connMaxLifetime"` // 秒
	ConnMaxIdleTime *int `mapstructure:"connMaxIdleTime"` // 秒
}

// DBSlaveOptions represents a single read-replica PostgreSQL connection.
// Mirrors Misskey's `dbSlaves: [{host, port, db, user, pass}]` YAML entries.
// SSL/extra options are inherited from the primary DBOptions at DSN build time.
type DBSlaveOptions struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
	DB   string `mapstructure:"db"`
	User string `mapstructure:"user"`
	Pass string `mapstructure:"pass"`
}

// MeilisearchOptions represents Meilisearch configuration.
type MeilisearchOptions struct {
	Host   string `mapstructure:"host"`
	Port   string `mapstructure:"port"`
	APIKey string `mapstructure:"apiKey"`
	SSL    bool   `mapstructure:"ssl"`
	Index  string `mapstructure:"index"`
	Scope  string `mapstructure:"scope"`
}

// FulltextSearchOptions represents fulltext search configuration.
type FulltextSearchOptions struct {
	Provider string `mapstructure:"provider"`
}

// SentryOptions mirrors the subset of `Sentry.NodeOptions` from the upstream
// Misskey YAML schema that maps cleanly onto Go's sentry-go SDK. Fields not
// covered (e.g. integration callbacks) are ignored on the Go backend.
type SentryOptions struct {
	DSN              string  `mapstructure:"dsn"`
	Environment      string  `mapstructure:"environment"`
	Release          string  `mapstructure:"release"`
	SampleRate       float64 `mapstructure:"sampleRate"`
	TracesSampleRate float64 `mapstructure:"tracesSampleRate"`
	Debug            bool    `mapstructure:"debug"`
	ServerName       string  `mapstructure:"serverName"`
}

// SentryBackendOptions mirrors `sentryForBackend` from Misskey config YAML.
// EnableNodeProfiling refers to a Node-only Sentry integration; on the Go
// backend it is parsed for compatibility but reported as a no-op at boot.
type SentryBackendOptions struct {
	EnableNodeProfiling bool          `mapstructure:"enableNodeProfiling"`
	Options             SentryOptions `mapstructure:"options"`
}

// LoggingOptions represents logging configuration.
type LoggingOptions struct {
	SQL *SQLLoggingOptions `mapstructure:"sql"`
}

// SQLLoggingOptions represents SQL logging configuration.
type SQLLoggingOptions struct {
	DisableQueryTruncation bool `mapstructure:"disableQueryTruncation"`
	EnableQueryParamLog    bool `mapstructure:"enableQueryParamLogging"`
}

// Source represents the raw YAML configuration file structure.
type Source struct {
	URL         string `mapstructure:"url"`
	Port        int    `mapstructure:"port"`
	Socket      string `mapstructure:"socket"`
	ChmodSocket string `mapstructure:"chmodSocket"`
	DisableHSTS bool   `mapstructure:"disableHsts"`
	// BcryptCost は account password のハッシュ強度 (work factor)。
	// 未設定なら password.DefaultCost。範囲外の値は警告して既定に落とす。
	BcryptCost *int `mapstructure:"bcryptCost"`
	// NoteHookConcurrency は投稿後のベストエフォートフックを種別ごとに
	// 何本まで同時に走らせるか。未設定なら GOMAXPROCS x 2。
	NoteHookConcurrency *int `mapstructure:"noteHookConcurrency"`
	// MediaProxyConcurrency は media proxy の画像処理 (decode/resize/encode)
	// を同時に何本走らせてよいか (#3032)。未設定なら GOMAXPROCS / 2 (最低 1)。
	// 同時に走る本数がそのまま同時に確保される中間バッファの本数になるので、
	// これがプロキシのピークメモリの上限を決める。
	MediaProxyConcurrency *int `mapstructure:"mediaProxyConcurrency"`
	// QueueIdlePollSeconds はジョブが無いときに worker が marker を待つ秒数の
	// 下限。空振りのたびに mkq が待ちを倍にし 30 秒で頭打ちにするので、
	// これは初回の待ちにあたる。未設定なら mkq の既定。mkq driver のみ有効。
	QueueIdlePollSeconds *int `mapstructure:"queueIdlePollSeconds"`
	// QueueStuckWorkerSeconds は 1 件の job の handler が何秒戻ってこなければ
	// その worker を「キューの worker 数に数えない」扱いにするか (#2657)。
	// 未設定ならキューごとの既定 (deliver / inbox / relationship / push /
	// webhook は 30 分、export などの batch 系は追跡しない)。正値を入れると全キューにその値が効き、負値で機能ごと無効に
	// なる。mkq driver のみ有効。
	QueueStuckWorkerSeconds *int `mapstructure:"queueStuckWorkerSeconds"`
	// QueueHandlerDeadlineSeconds は 1 件の job の handler を何秒待ってから
	// 諦めて worker を返すか (#2658)。**超過しても handler は止まらない** —
	// 待つのをやめるだけで、その goroutine は走り続ける。未設定なら 1 時間。
	// 1 job が分単位かかるのが正常な batch 系 (export / cleanRemoteFiles /
	// deleteAccount 等) は task type 側で対象外にしてあり、この値の影響を
	// 受けない。負値で機能ごと無効。mkq driver のみ有効。
	QueueHandlerDeadlineSeconds *int  `mapstructure:"queueHandlerDeadlineSeconds"`
	EnableIPRateLimit           *bool `mapstructure:"enableIpRateLimit"`
	// DisableEndpointRateLimits, when true, drops the per-endpoint rate
	// limit table entirely. Intended for benchmarking parity with Misskey TS
	// `NODE_ENV=development`. Never enable in production. Default false.
	DisableEndpointRateLimits *bool                  `mapstructure:"disableEndpointRateLimits"`
	DB                        DBOptions              `mapstructure:"db"`
	DBReplications            bool                   `mapstructure:"dbReplications"`
	DBSlaves                  []DBSlaveOptions       `mapstructure:"dbSlaves"`
	Redis                     RedisOptions           `mapstructure:"redis"`
	RedisForPubsub            *RedisOptions          `mapstructure:"redisForPubsub"`
	RedisForJobQueue          *RedisOptions          `mapstructure:"redisForJobQueue"`
	RedisForTimelines         *RedisOptions          `mapstructure:"redisForTimelines"`
	RedisForReactions         *RedisOptions          `mapstructure:"redisForReactions"`
	FulltextSearch            *FulltextSearchOptions `mapstructure:"fulltextSearch"`
	Meilisearch               *MeilisearchOptions    `mapstructure:"meilisearch"`
	SetupPassword             string                 `mapstructure:"setupPassword"`
	Proxy                     string                 `mapstructure:"proxy"`
	ProxySMTP                 string                 `mapstructure:"proxySmtp"`
	ProxyBypassHosts          []string               `mapstructure:"proxyBypassHosts"`
	AllowedPrivateNetworks    []string               `mapstructure:"allowedPrivateNetworks"`
	MaxFileSize               *int64                 `mapstructure:"maxFileSize"`
	ClusterLimit              *int                   `mapstructure:"clusterLimit"`
	ID                        string                 `mapstructure:"id"`
	OutgoingAddress           string                 `mapstructure:"outgoingAddress"`
	OutgoingAddressFamily     string                 `mapstructure:"outgoingAddressFamily"`

	DeliverJobConcurrency      *int `mapstructure:"deliverJobConcurrency"`
	InboxJobConcurrency        *int `mapstructure:"inboxJobConcurrency"`
	RelationshipJobConcurrency *int `mapstructure:"relationshipJobConcurrency"`
	DeliverJobPerSec           *int `mapstructure:"deliverJobPerSec"`
	InboxJobPerSec             *int `mapstructure:"inboxJobPerSec"`
	RelationshipJobPerSec      *int `mapstructure:"relationshipJobPerSec"`
	DeliverJobMaxAttempts      *int `mapstructure:"deliverJobMaxAttempts"`
	InboxJobMaxAttempts        *int `mapstructure:"inboxJobMaxAttempts"`
	// `<queue>JobKeepFailed` は failed bucket retention の最大件数。
	// 0 = retention 無し (= 蓄積し続ける) なので、operator が明示的に
	// 「全部残す」運用にしたい場合のみ 0 を設定する。未指定なら mk-go
	// の internal default (defaultKeepFailed) が適用される。
	DeliverJobKeepFailed *int `mapstructure:"deliverJobKeepFailed"`
	InboxJobKeepFailed   *int `mapstructure:"inboxJobKeepFailed"`

	// `<queue>JobKeepCompleted` は completed bucket retention の最大
	// 件数。意味的には KeepFailed と同じ「件数で上限を切る」。0 で
	// 蓄積無制限 (= UDS 観測で実害があった BullMQ default 挙動) に
	// opt-out 可能。未指定なら mk-go internal default (TS と同じ 30)
	// が適用される (#1193)。
	DeliverJobKeepCompleted *int `mapstructure:"deliverJobKeepCompleted"`
	InboxJobKeepCompleted   *int `mapstructure:"inboxJobKeepCompleted"`

	// JobQueueAutoScale toggles the AIMD auto-scale controller (#1120).
	// Default false. When true, queues without an explicit
	// `<queue>JobConcurrency` are managed by the controller within
	// [MinWorkers, MaxWorkers]. See docs/design/auto-scale-job-workers.md.
	JobQueueAutoScale bool `mapstructure:"jobQueueAutoScale"`

	// FrontendContentSecurityPolicy controls the CSP applied to the SPA
	// shell: "off" (default) / "report-only" / "enforce" (#2425)。
	//
	// **upstream Misskey には frontend HTML への CSP が無い。** mk-go 独自の
	// hardening なので opt-in にしてある。report-only は何もブロックしないが、
	// 違反があると全利用者のブラウザ console に出るため、頼んでいない operator
	// に配らないよう既定は off。
	FrontendContentSecurityPolicy string `mapstructure:"frontendContentSecurityPolicy"`
	// CrossOriginOpenerPolicy controls the `Cross-Origin-Opener-Policy`
	// header: "off" (default) / "same-origin-allow-popups" / "same-origin".
	// upstream は既定で出さない。既定を off にしてあるのは、外部アプリが認証
	// ページをポップアップで開く形の連携を切りうるため。
	CrossOriginOpenerPolicy string `mapstructure:"crossOriginOpenerPolicy"`

	// MaxWorkers is the per-queue worker upper bound used by the
	// auto-scale controller. Default: runtime.NumCPU() * 16.
	MaxWorkers *int `mapstructure:"maxWorkers"`

	// MinWorkers is the per-queue worker lower bound. Default: 4.
	MinWorkers *int `mapstructure:"minWorkers"`

	// MaxWorkersGlobal, when set, caps the sum of workers across all
	// auto-scaled queues for this process. Default: unset (no cap; each
	// queue scales up to MaxWorkers independently). Use only in
	// multi-pod deployments where per-process budget matters.
	MaxWorkersGlobal *int `mapstructure:"maxWorkersGlobal"`

	// AutoScaleCooldownSeconds is the minimum interval between scale
	// events per queue. Default: 1 second (per ADR §3.4).
	AutoScaleCooldownSeconds *int `mapstructure:"autoScaleCooldownSeconds"`

	// JobQueueDriver selects the worker / inspector implementation
	// behind internal/queue. "mkq" (the only driver) uses the
	// BullMQ-compatible shiroha-a/mkq library. Empty / unset = "mkq".
	// The legacy "asynq" value was removed in #2985 and now fails at
	// startup with a migration hint.
	JobQueueDriver string `mapstructure:"jobQueueDriver"`

	MediaProxy              string `mapstructure:"mediaProxy"`
	MediaProxySecret        string `mapstructure:"mediaProxySecret"`
	VideoThumbnailGenerator string `mapstructure:"videoThumbnailGenerator"`
	// VideoThumbnailGeneratorMode picks the wire used to ask the generator:
	//   "post" (default, mk-go) — POST <gen>/thumbnail with multipart/form-data
	//                             (`file` field). nekonoverse/video-thumb と同
	//                             仕様。SSRF surface を generator 側に持ち
	//                             込まない。
	//   "get"                   — GET <gen>/thumbnail.webp?thumbnail=1&url=<URL>
	//                             Misskey TS の videoThumbnailGenerator 仕様
	//                             に互換 (#637 review)。
	VideoThumbnailGeneratorMode string `mapstructure:"videoThumbnailGeneratorMode"`
	// NSFWDetectorURL は外部 NSFW 判定 service の endpoint。空なら自動判定
	// disabled で手動 isSensitive フラグのみ機能する (#406)。mk-go は
	// backend に ML runtime を抱えない方針なので、operator が任意の互換
	// service を立てて URL を指定する。Wire: POST body=image/video bytes,
	// Content-Type=<MIME>, response JSON `{"score": float64}` (0..1)。
	NSFWDetectorURL string `mapstructure:"nsfwDetectorUrl"`
	// NSFWDetectorAuthHeader は detector への 1 行 HTTP header。例:
	// "Authorization: Bearer xxxxx" / "X-API-Key: xxxxx"。SaaS detector
	// (Cloudflare Workers AI / AWS Rekognition 等) を thin proxy なしで
	// 直接呼ぶための設定 (#751)。空なら header 注入なし。
	NSFWDetectorAuthHeader string `mapstructure:"nsfwDetectorAuthHeader"`
	// NSFWDetectorTimeout は detector への 1 リクエストあたりの timeout
	// (Go duration、例: "60s" / "2m")。0 / 未指定なら 30s (#751)。
	NSFWDetectorTimeout time.Duration   `mapstructure:"nsfwDetectorTimeout"`
	Logging             *LoggingOptions `mapstructure:"logging"`

	PerChannelMaxNoteCacheCount  *int   `mapstructure:"perChannelMaxNoteCacheCount"`
	PerUserNotificationsMaxCount *int   `mapstructure:"perUserNotificationsMaxCount"`
	DeactivateAntennaThreshold   *int   `mapstructure:"deactivateAntennaThreshold"`
	PidFile                      string `mapstructure:"pidFile"`

	// TrustProxy lists the reverse proxies whose X-Forwarded-For entries are
	// trusted. Accepts a list (or comma-separated string) of CIDRs, bare IPs
	// and the proxy-addr names loopback / linklocal / uniquelocal, or false
	// to trust none. Unset means DefaultTrustProxy (TS-compatible). See
	// resolveTrustProxy. 型は any にしてある — []string だと bool の true が
	// weak decode で ["1"] になり、元の値の形を区別できない。
	TrustProxy any `mapstructure:"trustProxy"`

	// TestMode enables destructive test-only endpoints such as /api/reset-db.
	// Must never be enabled in production. Can be overridden via MK_TESTMODE=1.
	TestMode bool `mapstructure:"testMode"`

	// Dev serves the frontend from the Vite dev server instead of built
	// assets. Must never be enabled in production. Overridable via MK_DEV=1.
	//
	// **TestMode とは独立させる (#2477)。** TestMode は /api/reset-db のような
	// 破壊的エンドポイントを開けるフラグなので、フロントエンドを dev server から
	// 配信したいだけの用途で有効化させるわけにはいかない。
	Dev bool `mapstructure:"dev"`

	// Plugins holds per-plugin settings, keyed by plugin name (#2482).
	//
	// 中身の形はプラグインごとに違うので map のまま持つ。`enabled` だけは
	// mk-go が解釈し (未設定は有効)、残りはプラグインへそのまま渡す。
	Plugins map[string]map[string]any `mapstructure:"plugins"`
	// EffectivePolicyProviderCacheEntries is the maximum number of successful
	// policy results retained by each provider. Non-positive values use 10000.
	EffectivePolicyProviderCacheEntries *int `mapstructure:"effectivePolicyProviderCacheEntries"`

	// EnablePprof registers net/http/pprof handlers at /debug/pprof/*.
	// For local profiling only; must not be enabled in production.
	EnablePprof bool `mapstructure:"enablePprof"`

	// EnableTimelineCache caches first-page timeline responses per-viewer for
	// a short TTL, skipping DB hydration + packing + encode on hit. Opt-in
	// (default false) because it introduces up to TimelineCacheTTLSeconds of
	// staleness (a freshly posted note may not appear in the REST response
	// for a few seconds; the WebSocket stream prepends it in the meantime).
	EnableTimelineCache bool `mapstructure:"enableTimelineCache"`
	// TimelineCacheTTLSeconds is the TTL for EnableTimelineCache. Defaults to
	// 3 when unset (<= 0).
	TimelineCacheTTLSeconds int `mapstructure:"timelineCacheTtlSeconds"`

	// EnableMetrics registers a Prometheus /metrics endpoint exposing the
	// job-queue metrics defined in internal/queue/metrics (worker count /
	// queue depth / scale events). Defaults to false. Operators that
	// scrape from grafana / HPA should set true and gate access via
	// nginx / load-balancer ACLs since the endpoint is unauthenticated.
	// See docs/design/auto-scale-job-workers.md §6.1 for the metric
	// catalog.
	EnableMetrics bool `mapstructure:"enableMetrics"`

	// SentryForBackend enables Sentry error tracking for the Go server. Nil
	// disables Sentry entirely (production default).
	SentryForBackend *SentryBackendOptions `mapstructure:"sentryForBackend"`

	// SentryForFrontend is captured for Misskey YAML compatibility but the Go
	// backend itself does not consume it; downstream API handlers may forward
	// it to the frontend bundle. map[string]any keeps the YAML shape as-is.
	SentryForFrontend map[string]any `mapstructure:"sentryForFrontend"`

	// PublishTarballInsteadOfProvideRepositoryURL mirrors the upstream Misskey
	// YAML flag. When true, the frontend treats the release as a tarball
	// distribution rather than pointing at the source repository URL. Exposed
	// via /api/meta.providesTarball.
	PublishTarballInsteadOfProvideRepositoryURL bool `mapstructure:"publishTarballInsteadOfProvideRepositoryUrl"`
}

// Config represents the resolved application configuration.
//
// **この型を JSON へ marshal しないこと。** struct tag を 1 つも持たないので、Go の
// フィールド名がそのまま wire の形になる (`ProxySMTP` のような名前が出る)。しかも
// `SetupPassword` / `DB.Pass` / `MediaProxySecret` のような秘密も一緒に出るので、
// 1 行足すだけで設定一式が漏れる。設定を外へ見せる経路は
// `internal/server/config_dump.go` だけにしてあり、あちらはキーをリテラルで書き
// (= フィールド名の変更が wire に波及しない)、秘密は `secretValue()` でマスクする。
type Config struct {
	Version string
	URL     string
	Port    int
	// Socket, if non-empty, is a path to a UNIX domain socket that the HTTP
	// server listens on instead of the TCP port.
	Socket string
	// ChmodSocket is the mode (as a string like "770") applied to the UNIX
	// domain socket file after bind. Ignored when Socket is empty or when
	// the value cannot be parsed as octal.
	ChmodSocket string
	Host        string
	Hostname    string
	Scheme      string
	WsScheme    string
	WsURL       string
	APIURL      string
	AuthURL     string
	DriveURL    string

	DisableHSTS bool
	BcryptCost  int
	// NoteHookConcurrency: 0 なら実行時に既定 (GOMAXPROCS x 2) を使う。
	NoteHookConcurrency int
	// MediaProxyConcurrency: 0 なら実行時に既定 (GOMAXPROCS / 2、最低 1) を使う。
	MediaProxyConcurrency int
	// QueueIdlePollSeconds: 0 なら driver 既定。
	QueueIdlePollSeconds int
	// QueueStuckWorkerSeconds: 0 ならキューごとの既定、負値で無効。
	QueueStuckWorkerSeconds int
	// QueueHandlerDeadlineSeconds: 0 なら driver 既定、負値で無効。
	QueueHandlerDeadlineSeconds int
	EnableIPRateLimit           bool
	DisableEndpointRateLimits   bool
	SetupPassword               string

	DB             DBOptions
	DBReplications bool
	DBSlaves       []DBSlaveOptions

	Redis             RedisOptions
	RedisForPubsub    RedisOptions
	RedisForJobQueue  RedisOptions
	RedisForTimelines RedisOptions
	RedisForReactions RedisOptions

	FulltextSearch *FulltextSearchOptions
	Meilisearch    *MeilisearchOptions

	ID string

	Proxy                  string
	ProxySMTP              string
	ProxyBypassHosts       []string
	AllowedPrivateNetworks []string

	MaxFileSize           int64
	ClusterLimit          *int
	OutgoingAddress       string
	OutgoingAddressFamily string

	DeliverJobConcurrency      *int
	InboxJobConcurrency        *int
	RelationshipJobConcurrency *int
	DeliverJobPerSec           *int
	InboxJobPerSec             *int
	RelationshipJobPerSec      *int
	DeliverJobMaxAttempts      *int
	InboxJobMaxAttempts        *int
	DeliverJobKeepFailed       *int
	InboxJobKeepFailed         *int
	DeliverJobKeepCompleted    *int
	InboxJobKeepCompleted      *int

	// Auto-scale knobs (#1120 tracker). See docs/design/auto-scale-job-workers.md.
	JobQueueAutoScale bool
	// FrontendContentSecurityPolicy: "off" / "report-only" / "enforce" (#2425)。
	FrontendContentSecurityPolicy string
	// CrossOriginOpenerPolicy: "off" / "same-origin-allow-popups" / "same-origin"。
	CrossOriginOpenerPolicy  string
	MaxWorkers               *int
	MinWorkers               *int
	MaxWorkersGlobal         *int
	AutoScaleCooldownSeconds *int

	// JobQueueDriver is always "mkq" (#2985 removed the legacy asynq
	// driver). Kept as a string so an unknown value is still
	// distinguishable from the normalised default at the wiring layer.
	JobQueueDriver string

	MediaProxy                   string
	ExternalMediaProxyEnabled    bool
	MediaProxySecret             []byte
	VideoThumbnailGenerator      string
	VideoThumbnailGeneratorMode  string
	NSFWDetectorURL              string
	NSFWDetectorAuthHeader       string
	NSFWDetectorTimeout          time.Duration
	UserAgent                    string
	PerChannelMaxNoteCacheCount  int
	PerUserNotificationsMaxCount int
	DeactivateAntennaThreshold   int
	PidFile                      string
	Logging                      *LoggingOptions

	// TrustProxy is the normalized list of trusted reverse proxy CIDRs.
	// nil means "not resolved" and is treated as DefaultTrustProxy; a non-nil
	// empty slice means no proxy is trusted (X-Forwarded-For is ignored).
	TrustProxy []string

	// TestMode enables destructive test-only endpoints such as /api/reset-db.
	// Must never be enabled in production. Can be overridden via MK_TESTMODE=1.
	TestMode bool

	// Dev serves the frontend from the Vite dev server instead of built
	// assets. Must never be enabled in production. Overridable via MK_DEV=1.
	// See the source struct doc for why this is separate from TestMode.
	Dev bool

	// Plugins holds per-plugin settings, keyed by plugin name (#2482).
	Plugins map[string]map[string]any
	// EffectivePolicyProviderCacheEntries limits successful cached results per
	// provider. The resolved value is always positive.
	EffectivePolicyProviderCacheEntries int

	// EnablePprof registers net/http/pprof handlers at /debug/pprof/*.
	EnablePprof bool

	// EnableTimelineCache caches first-page timeline responses per-viewer for
	// TimelineCacheTTLSeconds. Opt-in; see the source struct doc for the
	// staleness trade-off.
	EnableTimelineCache bool
	// TimelineCacheTTLSeconds is the TTL for EnableTimelineCache (default 3).
	TimelineCacheTTLSeconds int

	// EnableMetrics registers the Prometheus /metrics endpoint. See
	// internal/queue/metrics for the exposed metric catalog.
	EnableMetrics bool

	// SentryForBackend, when non-nil, enables Sentry error tracking for the
	// Go server.
	SentryForBackend *SentryBackendOptions

	// SentryForFrontend is forwarded as-is to clients that need it (parsed
	// for YAML compatibility; the Go backend itself does not consume it).
	SentryForFrontend map[string]any

	// PublishTarballInsteadOfProvideRepositoryURL is exposed to clients via
	// /api/meta.providesTarball. See the Source struct for details.
	PublishTarballInsteadOfProvideRepositoryURL bool
}

// ProvidesTarball reports whether this server serves a source tarball at
// /tarball/misskey-<version>.tar.gz.
//
// **常に false を返す。** upstream は ClientServerService が `built/tarball` を
// `/tarball/` に静的配信するが、mk-go にはそのルートが無い。**代わりに SPA の
// catchall (`GET /*`) が拾うので、404 にすらならず HTML が 200 で返る** — 設定を
// そのまま公開すると、frontend が出す「ソースコード (Tarball)」のリンクは
// `misskey-<version>.tar.gz` という名前の HTML をダウンロードさせる。AGPL 13 条の
// 案内としては、壊れた tarball を掴ませるより repositoryUrl だけのほうが正しい (#2700)。
//
// **frontend 側は 2 箇所ある** (`about-misskey.vue` の tarball リンクと
// `about.overview.vue` の `repositoryUrl || /tarball/...` フォールバック)。ルートを
// 実装したときはこのメソッドを直すだけでは足りず、mk-go 独自の `about-mkgo.vue` にも
// tarball の分岐を足す必要がある (現状は意図的に持っていない)。
func (c *Config) ProvidesTarball() bool {
	return false
}

const defaultMaxFileSize int64 = 262144000

// Load reads the Misskey YAML configuration and returns a resolved Config.
// Environment variables with the prefix MK_ override YAML values.
// Nested keys use underscore separation (e.g. MK_DB_HOST overrides db.host).
func Load(configPath string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(configPath)
	v.SetConfigType("yaml")

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// 環境変数によるオーバーライド
	v.SetEnvPrefix("MK")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// 主要な設定キーを明示的にバインド（Viperは既知のキーのみ環境変数を適用する）
	bindEnvKeys(v)

	var src Source
	if err := v.Unmarshal(&src); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	return resolve(&src)
}

// bindEnvKeys binds environment variables to known configuration keys.
func bindEnvKeys(v *viper.Viper) {
	keys := []string{
		"url", "port", "socket",
		"frontendContentSecurityPolicy",
		"db.host", "db.port", "db.db", "db.user", "db.pass",
		"redis.host", "redis.path", "redis.port", "redis.pass", "redis.db", "redis.username",
		"redis.family", "redis.prefix",
		"redisForPubsub.host", "redisForPubsub.path", "redisForPubsub.port", "redisForPubsub.pass",
		"redisForPubsub.family", "redisForPubsub.prefix", "redisForPubsub.db", "redisForPubsub.username",
		"redisForJobQueue.host", "redisForJobQueue.path", "redisForJobQueue.port", "redisForJobQueue.pass",
		"redisForJobQueue.family", "redisForJobQueue.prefix", "redisForJobQueue.db", "redisForJobQueue.username",
		"redisForTimelines.host", "redisForTimelines.path", "redisForTimelines.port", "redisForTimelines.pass",
		"redisForTimelines.family", "redisForTimelines.prefix", "redisForTimelines.db", "redisForTimelines.username",
		"redisForReactions.host", "redisForReactions.path", "redisForReactions.port", "redisForReactions.pass",
		"redisForReactions.family", "redisForReactions.prefix", "redisForReactions.db", "redisForReactions.username",
		"db.maxOpenConns", "db.maxIdleConns", "db.connMaxLifetime", "db.connMaxIdleTime",
		"redis.poolSize",
		"redisForPubsub.poolSize",
		"redisForJobQueue.poolSize",
		"redisForTimelines.poolSize",
		"redisForReactions.poolSize",
		"id", "maxFileSize",
		"mediaProxySecret",
		"testMode",
		"dev",
		"effectivePolicyProviderCacheEntries",
		// 運用/セキュリティ系: 既存ymlから環境変数オーバーライドできるようにする
		"trustProxy",
		"disableHsts",
		"bcryptCost",
		"noteHookConcurrency",
		"mediaProxyConcurrency",
		"queueIdlePollSeconds",
		"queueStuckWorkerSeconds",
		"queueHandlerDeadlineSeconds",
		"crossOriginOpenerPolicy",
		"enableIpRateLimit",
		"disableEndpointRateLimits",
		"mediaProxy",
		"logging.sql.disableQueryTruncation",
		"logging.sql.enableQueryParamLogging",
		"enablePprof",
		"enableTimelineCache",
		"timelineCacheTtlSeconds",
		"enableMetrics",
		"sentryForBackend.options.dsn",
		"sentryForBackend.options.environment",
		"jobQueueDriver",
		"jobQueueAutoScale",
		"maxWorkers",
		"minWorkers",
		"maxWorkersGlobal",
		"autoScaleCooldownSeconds",
	}
	for _, k := range keys {
		_ = v.BindEnv(k)
	}
}

func resolve(src *Source) (*Config, error) {
	parsedURL, err := url.Parse(src.URL)
	if err != nil || parsedURL.Host == "" {
		return nil, fmt.Errorf("invalid url: %s", src.URL)
	}

	host := parsedURL.Host
	hostname := parsedURL.Hostname()
	scheme := strings.TrimSuffix(parsedURL.Scheme, ":")
	wsScheme := strings.Replace(scheme, "http", "ws", 1)

	maxFileSize := defaultMaxFileSize
	if src.MaxFileSize != nil {
		maxFileSize = *src.MaxFileSize
	}

	// **範囲外は既定に落として警告する。** 設定の書き間違いで cost 4 の
	// ハッシュを量産すると、気づいた時点で全員のパスワードが弱い。
	bcryptCost := password.DefaultCost
	// 0 以下は「未設定」として driver 既定に委ねる。
	queueIdlePollSeconds := 0
	if src.QueueIdlePollSeconds != nil && *src.QueueIdlePollSeconds > 0 {
		queueIdlePollSeconds = *src.QueueIdlePollSeconds
	}
	// **こちらは負値を素通しする。** 負値が「無効化」という意味を持つ
	// (queueIdlePollSeconds の 0 = 未設定とは扱いが違う)。
	queueStuckWorkerSeconds := 0
	if src.QueueStuckWorkerSeconds != nil {
		queueStuckWorkerSeconds = *src.QueueStuckWorkerSeconds
	}
	// こちらも負値が「無効化」なので素通しする。
	queueHandlerDeadlineSeconds := 0
	if src.QueueHandlerDeadlineSeconds != nil {
		queueHandlerDeadlineSeconds = *src.QueueHandlerDeadlineSeconds
	}

	// 0 以下は「未設定」として実行時の既定に委ねる。**負値を素通しすると
	// semaphore が容量 0 になり、フックが 1 本も走らなくなる。**
	noteHookConcurrency := 0
	if src.NoteHookConcurrency != nil {
		if *src.NoteHookConcurrency < 0 {
			slog.Warn("noteHookConcurrency が負なので既定値を使います", "value", *src.NoteHookConcurrency)
		} else {
			noteHookConcurrency = *src.NoteHookConcurrency
		}
	}

	// 同上。0 以下は実行時の既定 (GOMAXPROCS / 2) に委ねる。
	mediaProxyConcurrency := 0
	if src.MediaProxyConcurrency != nil {
		if *src.MediaProxyConcurrency < 0 {
			slog.Warn("mediaProxyConcurrency が負なので既定値を使います", "value", *src.MediaProxyConcurrency)
		} else {
			mediaProxyConcurrency = *src.MediaProxyConcurrency
		}
	}

	if src.BcryptCost != nil {
		if *src.BcryptCost < password.MinCost || *src.BcryptCost > password.MaxCost {
			slog.Warn("bcryptCost が範囲外なので既定値を使います",
				"value", *src.BcryptCost, "min", password.MinCost, "max", password.MaxCost,
				"default", password.DefaultCost)
		} else {
			bcryptCost = *src.BcryptCost
		}
	}

	enableIPRateLimit := true
	if src.EnableIPRateLimit != nil {
		enableIPRateLimit = *src.EnableIPRateLimit
	}
	disableEndpointRateLimits := false
	if src.DisableEndpointRateLimits != nil {
		disableEndpointRateLimits = *src.DisableEndpointRateLimits
	}

	redis := resolveRedis(src.Redis, host)

	perChannelMaxNoteCacheCount := 1000
	if src.PerChannelMaxNoteCacheCount != nil {
		perChannelMaxNoteCacheCount = *src.PerChannelMaxNoteCacheCount
	}
	perUserNotificationsMaxCount := 500
	if src.PerUserNotificationsMaxCount != nil {
		perUserNotificationsMaxCount = *src.PerUserNotificationsMaxCount
	}
	deactivateAntennaThreshold := 1000 * 60 * 60 * 24 * 7
	if src.DeactivateAntennaThreshold != nil {
		deactivateAntennaThreshold = *src.DeactivateAntennaThreshold
	}
	effectivePolicyProviderCacheEntries := 10_000
	if src.EffectivePolicyProviderCacheEntries != nil {
		if *src.EffectivePolicyProviderCacheEntries > 0 {
			effectivePolicyProviderCacheEntries = *src.EffectivePolicyProviderCacheEntries
		} else {
			slog.Warn("effectivePolicyProviderCacheEntries が正でないため既定値を使います",
				"value", *src.EffectivePolicyProviderCacheEntries, "default", 10_000)
		}
	}

	internalMediaProxy := fmt.Sprintf("%s://%s/proxy", scheme, host)
	mediaProxy := internalMediaProxy
	externalMediaProxyEnabled := false
	if src.MediaProxy != "" {
		mp := strings.TrimRight(src.MediaProxy, "/")
		if mp != internalMediaProxy {
			externalMediaProxyEnabled = true
		}
		mediaProxy = mp
	}

	mediaProxySecret := deriveMediaProxySecret(src)

	jobQueueDriver, err := resolveJobQueueDriver(src.JobQueueDriver)
	if err != nil {
		return nil, err
	}

	trustProxy, err := resolveTrustProxy(src.TrustProxy)
	if err != nil {
		return nil, err
	}

	// DSN() は Config から毎回導出するので、誤った db.extra はここで起動を
	// 止める (DSN 側で黙って既定に倒すと、平文 / 無検証で繋がってしまう)。
	if _, err := ResolveDBTLS(src.DB.Extra); err != nil {
		return nil, err
	}

	cfg := &Config{
		Version:     MisskeyVersion,
		URL:         parsedURL.Scheme + "://" + parsedURL.Host,
		Port:        src.Port,
		Socket:      src.Socket,
		ChmodSocket: src.ChmodSocket,
		Host:        host,
		Hostname:    hostname,
		Scheme:      scheme,
		WsScheme:    wsScheme,
		WsURL:       fmt.Sprintf("%s://%s", wsScheme, host),
		APIURL:      fmt.Sprintf("%s://%s/api", scheme, host),
		AuthURL:     fmt.Sprintf("%s://%s/auth", scheme, host),
		DriveURL:    fmt.Sprintf("%s://%s/files", scheme, host),

		DisableHSTS:                 src.DisableHSTS,
		BcryptCost:                  bcryptCost,
		NoteHookConcurrency:         noteHookConcurrency,
		MediaProxyConcurrency:       mediaProxyConcurrency,
		QueueIdlePollSeconds:        queueIdlePollSeconds,
		QueueStuckWorkerSeconds:     queueStuckWorkerSeconds,
		QueueHandlerDeadlineSeconds: queueHandlerDeadlineSeconds,
		EnableIPRateLimit:           enableIPRateLimit,
		DisableEndpointRateLimits:   disableEndpointRateLimits,
		SetupPassword:               src.SetupPassword,

		DB:             resolveDB(src.DB),
		DBReplications: src.DBReplications,
		DBSlaves:       resolveDBSlaves(src.DBSlaves),

		Redis:             redis,
		RedisForPubsub:    resolveRedisOrDefault(src.RedisForPubsub, redis, host),
		RedisForJobQueue:  resolveRedisOrDefault(src.RedisForJobQueue, redis, host),
		RedisForTimelines: resolveRedisOrDefault(src.RedisForTimelines, redis, host),
		RedisForReactions: resolveRedisOrDefault(src.RedisForReactions, redis, host),

		FulltextSearch: src.FulltextSearch,
		Meilisearch:    src.Meilisearch,

		ID: src.ID,

		Proxy:                  src.Proxy,
		ProxySMTP:              src.ProxySMTP,
		ProxyBypassHosts:       src.ProxyBypassHosts,
		AllowedPrivateNetworks: src.AllowedPrivateNetworks,

		MaxFileSize:           maxFileSize,
		ClusterLimit:          src.ClusterLimit,
		OutgoingAddress:       src.OutgoingAddress,
		OutgoingAddressFamily: src.OutgoingAddressFamily,

		DeliverJobConcurrency:         src.DeliverJobConcurrency,
		InboxJobConcurrency:           src.InboxJobConcurrency,
		RelationshipJobConcurrency:    src.RelationshipJobConcurrency,
		DeliverJobPerSec:              src.DeliverJobPerSec,
		InboxJobPerSec:                src.InboxJobPerSec,
		RelationshipJobPerSec:         src.RelationshipJobPerSec,
		DeliverJobMaxAttempts:         src.DeliverJobMaxAttempts,
		InboxJobMaxAttempts:           src.InboxJobMaxAttempts,
		DeliverJobKeepFailed:          src.DeliverJobKeepFailed,
		InboxJobKeepFailed:            src.InboxJobKeepFailed,
		DeliverJobKeepCompleted:       src.DeliverJobKeepCompleted,
		InboxJobKeepCompleted:         src.InboxJobKeepCompleted,
		JobQueueAutoScale:             src.JobQueueAutoScale,
		FrontendContentSecurityPolicy: src.FrontendContentSecurityPolicy,
		CrossOriginOpenerPolicy:       src.CrossOriginOpenerPolicy,
		MaxWorkers:                    src.MaxWorkers,
		MinWorkers:                    src.MinWorkers,
		MaxWorkersGlobal:              src.MaxWorkersGlobal,
		AutoScaleCooldownSeconds:      src.AutoScaleCooldownSeconds,
		JobQueueDriver:                jobQueueDriver,

		MediaProxy:                   mediaProxy,
		ExternalMediaProxyEnabled:    externalMediaProxyEnabled,
		MediaProxySecret:             mediaProxySecret,
		VideoThumbnailGenerator:      strings.TrimRight(src.VideoThumbnailGenerator, "/"),
		VideoThumbnailGeneratorMode:  normalizeVideoThumbMode(src.VideoThumbnailGeneratorMode),
		NSFWDetectorURL:              strings.TrimRight(src.NSFWDetectorURL, "/"),
		NSFWDetectorAuthHeader:       src.NSFWDetectorAuthHeader,
		NSFWDetectorTimeout:          src.NSFWDetectorTimeout,
		UserAgent:                    fmt.Sprintf("mk-go/%s (%s)", MkGoVersion, src.URL),
		PerChannelMaxNoteCacheCount:  perChannelMaxNoteCacheCount,
		PerUserNotificationsMaxCount: perUserNotificationsMaxCount,
		DeactivateAntennaThreshold:   deactivateAntennaThreshold,
		PidFile:                      src.PidFile,
		Logging:                      src.Logging,

		TrustProxy: trustProxy,

		TestMode:                            src.TestMode,
		Dev:                                 src.Dev,
		Plugins:                             src.Plugins,
		EffectivePolicyProviderCacheEntries: effectivePolicyProviderCacheEntries,
		EnablePprof:                         src.EnablePprof,
		EnableTimelineCache:                 src.EnableTimelineCache,
		TimelineCacheTTLSeconds:             src.TimelineCacheTTLSeconds,
		EnableMetrics:                       src.EnableMetrics,

		SentryForBackend:  src.SentryForBackend,
		SentryForFrontend: src.SentryForFrontend,

		PublishTarballInsteadOfProvideRepositoryURL: src.PublishTarballInsteadOfProvideRepositoryURL,
	}

	if cfg.TestMode {
		// TestMode は /api/reset-db のような破壊的エンドポイントを有効化する。
		// 本番で誤って有効化されていないか気付けるよう、起動時に強く警告する。
		slog.Warn("config: TestMode is enabled; destructive test endpoints (e.g. /api/reset-db) are active. DO NOT enable this in production.",
			"url", cfg.URL)
	}

	if cfg.EnablePprof {
		slog.Warn("config: EnablePprof is enabled; /debug/pprof/* endpoints expose runtime internals. DO NOT enable this in production.",
			"url", cfg.URL)
	}

	if cfg.PublishTarballInsteadOfProvideRepositoryURL {
		// 設定は読むが効かない。黙って無視すると operator は「tarball を配って
		// いる」と思ったまま、frontend が壊れたリンクを出す状態に気付けない。
		// **404 で気付ける類ではない** — SPA catchall が拾うので HTML が 200 で返る。
		slog.Warn("config: publishTarballInsteadOfProvideRepositoryUrl is enabled but mk-go does not serve /tarball/ (the SPA catchall would return HTML with a .tar.gz name); providesTarball is reported as false. Set meta.repositoryUrl instead.",
			"url", cfg.URL)
	}

	if cfg.DisableEndpointRateLimits {
		// per-endpoint rate limit table を完全に無効化する flag。bench /
		// test でしか使わない想定のため、production で有効化されていないか
		// 気付けるよう強く警告する (TestMode / EnablePprof と同じ位置に
		// まとめる、router 側でなく resolve 側で出すことで全 entrypoint
		// を確実にカバー)。
		slog.Warn("config: DisableEndpointRateLimits is enabled; per-endpoint rate limits will not enforce. DO NOT enable this in production.",
			"url", cfg.URL)
	}

	// HTTP socket と TCP port は両立しない。両方設定されていた場合は
	// socket を優先する旨警告ログを出し、運用者に気付けるようにする。
	if cfg.Socket != "" && cfg.Port != 0 {
		slog.Warn("config: both socket and port are set; socket takes precedence and port will be ignored",
			"socket", cfg.Socket, "port", cfg.Port)
	}

	return cfg, nil
}

func resolveDB(opts DBOptions) DBOptions {
	if opts.Port == 0 {
		opts.Port = 5432
	}
	return opts
}

func resolveDBSlaves(slaves []DBSlaveOptions) []DBSlaveOptions {
	for i := range slaves {
		if slaves[i].Port == 0 {
			slaves[i].Port = 5432
		}
	}
	return slaves
}

func resolveRedis(opts RedisOptions, host string) RedisOptions {
	if opts.Prefix == "" {
		opts.Prefix = host
	}
	return opts
}

// KeyPrefix returns the Redis key prefix to prepend to bare keys so that
// mk-go shares the same namespace as Misskey TS (TS uses ioredis `keyPrefix:
// <host>:` which gets applied to every key transparently). Empty Prefix
// returns "" for backwards compat with keys that were never prefixed.
//
// Consumers should prepend this to every Redis key they construct, e.g.
// `cfg.Redis.KeyPrefix() + "list:homeTimeline:" + userID`.
func (r RedisOptions) KeyPrefix() string {
	if r.Prefix == "" {
		return ""
	}
	return r.Prefix + ":"
}

// deriveMediaProxySecret returns the configured secret for HMAC-signed media
// proxy URLs, or nil when the operator did not set one.
//
// **URL から導出してはいけない。** 以前は未設定時に
// `sha256(url + "|mediaproxy")` を使っていたが、インスタンス URL は公開情報
// なので誰でも同じ鍵を計算でき、任意の URL に有効な署名を付けられた。
// mediaproxy の Authorize は署名を allowlist より先に見るので、署名を偽造
// できると allowlist ごと迂回でき、mk-go の media proxy が upstream と同じ
// 「任意の公開 URL を取得する開いたプロキシ」に退化していた。
//
// nil を返した場合は、DB に永続化した instance_secret から鍵を解決する
// (router 側で resolveMediaProxySecret が行う)。鍵はプロセス間・再起動を
// またいで安定している必要があるため、起動時のメモリ生成では足りない。
func deriveMediaProxySecret(src *Source) []byte {
	if src.MediaProxySecret != "" {
		return []byte(src.MediaProxySecret)
	}
	return nil
}

func resolveRedisOrDefault(opts *RedisOptions, fallback RedisOptions, host string) RedisOptions {
	if opts == nil {
		return fallback
	}
	return resolveRedis(*opts, host)
}

// resolveJobQueueDriver normalises the jobQueueDriver config value.
// Empty / whitespace falls back to "mkq", the only driver (#2985)。
// Unknown non-empty values return an error: silently swapping a typo
// (e.g. "mkqq") for the default hides operator intent, especially given
// that internal/server/queue_factory.go also rejects unknown driver
// names — surfacing the failure here keeps the two layers consistent
// and the boot log readable.
//
// **"asynq" だけは専用の文面で落とす。** 既定が mkq になった後も明示的に
// asynq を選んでいた運用は意図的なので、typo と同じ「未知の値」で片付けると
// 何が起きたのか分からない。かといって黙って mkq で起動するのも避ける —
// driver が変わると予約投稿の可否やレート上限の効き方まで変わるので、警告
// 1 行で流してよい変更ではない (#2985)。
//
// **移行が片道であることも書く。** asynq の未処理ジョブは `asynq:{<queue>}:*`
// に居り mkq (`bull:*`) からは見えない。新しいビルドは起動を拒むので、
// **後から捌く手段が無い** — 旧ビルドで空にしてから上げるしかない。それを
// 知らせる機会はこのエラーしか無いので、案内をここに入れる。
func resolveJobQueueDriver(raw string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "":
		return "mkq", nil
	case "mkq":
		return v, nil
	case "asynq":
		return "", fmt.Errorf("config: jobQueueDriver %q has been removed (#2985); set it to \"mkq\" or delete the line to use the default. "+
			"Migrating is one-way: jobs still queued under Redis keys \"asynq:{<queue>}:*\" are invisible to mkq (\"bull:*\"), "+
			"so drain them with the previous mk-go build before upgrading", raw)
	default:
		return "", fmt.Errorf("config: unknown jobQueueDriver %q (expected \"mkq\")", raw)
	}
}

// normalizeVideoThumbMode validates and lowercases the wire-mode hint for
// the video thumbnail generator. Unknown / empty defaults to "post"
// (nekonoverse/video-thumb compat). "get" picks the Misskey TS-style URL
// pattern.
func normalizeVideoThumbMode(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "get":
		return "get"
	default:
		return "post"
	}
}

// IsUnixSocketPath reports whether the given host string points to a UNIX
// domain socket path. The convention is that any absolute path (starts with
// "/") is treated as a socket. Empty strings and TCP hostnames return false.
//
// This helper is shared between the DB DSN builder, the Redis client factory
// and the HTTP server startup path so that all three sub-systems agree on
// what counts as a UDS address.
func IsUnixSocketPath(host string) bool {
	return strings.HasPrefix(host, "/")
}
