package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"github.com/shiroha-a/mk/internal/config"
	"github.com/shiroha-a/mk/internal/core/cache"
	"github.com/shiroha-a/mk/internal/core/chart"
	corefederation "github.com/shiroha-a/mk/internal/core/federation"
	corenote "github.com/shiroha-a/mk/internal/core/note"
	corerole "github.com/shiroha-a/mk/internal/core/role"
	"github.com/shiroha-a/mk/internal/misc/password"
	"github.com/shiroha-a/mk/internal/misc/redact"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
	queuemetrics "github.com/shiroha-a/mk/internal/queue/metrics"
	"github.com/shiroha-a/mk/internal/queue/runtimestats"
	"github.com/shiroha-a/mk/internal/repository"
	mksentry "github.com/shiroha-a/mk/internal/sentry"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"github.com/shiroha-a/mk/plugin"
	"gorm.io/gorm"
)

// Server represents the HTTP server.
type Server struct {
	echo *echo.Echo
	// userRepo は CachedUserRepository でラップ済みの共有 instance。
	// auth middleware と setupRoutes の両方からこれを使うことで、片側の
	// mutation で他方の cache が stale 化するのを防ぐ (#300 3-3)。
	userRepo       repository.UserRepository
	config         *config.Config
	db             *gorm.DB
	redis          *cache.RedisClients
	auth           *middleware.AuthMiddleware
	queueDriver    driver.Driver
	queueClient    *queue.Client
	queueServer    *queue.Server
	queueScheduler *queue.Scheduler
	queueInspector *queue.Inspector
	// queueMetrics は /metrics endpoint と auto-scale controller の双方が
	// 共有する Prometheus metric bundle (#1122 で declare、#1125 で
	// controller が ScaleEventsTotal を increment)。EnableMetrics=false でも
	// auto-scale が活きていれば counter は内部 increment するが、register
	// されないので外部公開はされない。
	queueMetrics *queuemetrics.Metrics

	// queueRuntimeStats は admin UI 向けの短期ランタイム統計 (worker 遅延 /
	// auto-scale 履歴)。/metrics は無認証公開で LB ACL 必須のため admin から
	// 読めない。その穴を埋めるプロセスローカルな snapshot (#2277)。
	queueRuntimeStats *runtimestats.Recorder
	// startedAt は admin/server-metrics が返す uptime の起点 (#2395)。
	startedAt time.Time
	// peerDeps はプラグイン同士の通信 (#2537) に要る一式。setupRoutes が
	// 署名・解決・ブロック判定を組み立ててから setupPlugins に渡す。
	// 未配線 (nil) のときは peer を宣言したプラグインも受け口を張らない。
	peerDeps *pluginPeerDeps
	// role は「このプロセスが Web を担うか、ジョブキューを担うか」(#2459)。
	// 既定 (RoleBoth) は env 未設定時の従来どおりの挙動。setupRoutes は
	// role を見て背景処理を出し分けるので、New より前に決まっている必要がある。
	role config.ProcessRole
	// queueOnlyServer は RoleQueue のときだけ立てる /healthz 用の最小 server。
	// Shutdown で閉じるために保持する。
	queueOnlyServer *http.Server

	// pluginRoles はプラグインのルートで権限を判定するための参照 (#2477)。
	// setupRoutes で roleService を作った後に入る。
	pluginRoles middleware.RoleChecker
	// roleService は HTTP と queue が共有する process-local policy registry。
	// 有効な plugin provider の登録は起動時だけ行い、enabled の変更には
	// 再起動を要求する。
	roleService *corerole.Service

	// pluginSetupErr はプラグイン登録の失敗を New まで運ぶ (#2478)。
	// setupRoutes は巨大で戻り値を持たないため、signature を変えずに
	// 伝播させる。**起動は必ず失敗させる**: 登録できなかったプラグインを
	// 黙って無効のまま動かすと、機能が消えた原因が分からない。
	pluginSetupErr error
	// wiringErr は「未配線でも動いてしまう」依存の検査結果を New まで運ぶ (#2682)。
	// wiringChecked は検査が実際に走ったかどうか。**ゼロ値が「未実行」= 起動
	// 失敗になるようにしてある。** 別に種を撒く方式だと、その 1 行を消すだけで
	// 検査を無効化できてしまい、しかもテストは緑のままだった
	// (#2682 review L-A)。
	wiringChecked bool
	wiringErr     error
	autoscale     *autoscaleRunner
	chartMgmt     *chart.ManagementService
	// mediaProxySecret は internal media proxy URL の HMAC 鍵。config に
	// 明示設定が無ければ DB (instance_secret) の生成値を使うため、New() で
	// 一度だけ解決して保持する。
	mediaProxySecret []byte

	// deliverSvc は federation deliver service への参照。本番では queue
	// 経由で deliver を enqueue するが、test (#780) で queue を bypass する
	// ための SetSyncDeliverHookForTest を呼べるよう参照を保持する。
	deliverSvc *corefederation.DeliverService

	// shutdownHooks は Shutdown() 時に queue / HTTP echo より先に呼ばれる。
	// ctx-aware にすることで graceful drain (例: hashtag service の
	// in-flight worker、#727) を hook で wire できる。シンプルな
	// stop/close 系 hook は引数 _ で受け流すだけ。
	shutdownHooks []func(context.Context)
	shutdownOnce  sync.Once
	shutdownErr   error

	// reactionFlushWorkerはqueue roleの定期enqueue loop。field化はconstructor
	// rollbackをsleep無しで検証し、起動前にownershipを登録するため。
	reactionFlushWorker   func(context.Context, func() error)
	pluginGoStarter       func(func())
	pluginGoGate          *pluginGoGate
	beforePluginGoRelease func()
}

// registerShutdownHook registers fn to be invoked during Shutdown.
// Hooks run in registration order before the queue / echo shutdown.
// ctx は Shutdown() の caller から伝播され、graceful drain の deadline
// として使える (#764)。
func (s *Server) registerShutdownHook(fn func(context.Context)) {
	s.shutdownHooks = append(s.shutdownHooks, fn)
}

func (s *Server) startConstructionWorker(run func(context.Context)) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var once sync.Once
	s.registerShutdownHook(func(shutdownCtx context.Context) {
		once.Do(cancel)
		select {
		case <-done:
		case <-shutdownCtx.Done():
		}
	})
	go func() {
		defer close(done)
		run(ctx)
	}()
}

func runReactionFlushWorker(ctx context.Context, enqueue func() error) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			_ = enqueue()
		}
	}
}

// buildIPExtractor returns an echo.IPExtractor that wraps Echo's standard
// XFF extractor with a UDS-safe fallback. Echo 標準の ExtractIPFromXFFHeader
// (echo/ip.go:252) は req.RemoteAddr が空文字 (UNIX domain socket 経由のとき
// net.SplitHostPort が "" を返す) のとき XFF 逆順走査で net.ParseIP("") が
// nil → directIP="" を早期 return してしまい、c.RealIP() が常に空文字を
// 返す。signin record / user_ip 記録 / rate-limit が破壊されるため (#703)、
// inner が空文字を返したケースのみ extractIPFallback で補う。
//
// 通常 (TCP listen) 経路では inner が valid IP を返すため fallback は走らず、
// 既存挙動と完全に互換。
//
// 信頼するのは trusted だけ。Echo の ExtractIPFromXFFHeader は既定で
// loopback / link-local / private を信頼するので、それを明示的に外す —
// 外さないと、運営者が trustProxy を CDN の範囲だけに絞っても private
// アドレスからの XFF が信頼され続ける。trusted が空なら XFF を一切見ず
// 接続元アドレスを返す (UDS の fallback だけは例外。extractIPFallback 参照)。
func buildIPExtractor(trusted []*net.IPNet) echo.IPExtractor {
	opts := make([]echo.TrustOption, 0, len(trusted)+3)
	opts = append(opts, echo.TrustLoopback(false), echo.TrustLinkLocal(false), echo.TrustPrivateNet(false))
	for _, n := range trusted {
		opts = append(opts, echo.TrustIPRange(n))
	}
	inner := echo.ExtractIPFromXFFHeader(opts...)
	var warned atomic.Bool
	return func(req *http.Request) string {
		if !warned.Load() {
			warnUntrustedLocalProxy(req, trusted, &warned)
		}
		if ip := inner(req); ip != "" {
			return ip
		}
		return extractIPFallback(req, trusted)
	}
}

// localProxyRanges are the loopback and private (unique-local) ranges a
// reverse proxy on the same host or a container network connects from.
var localProxyRanges = func() []*net.IPNet {
	cidrs := []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		nets = append(nets, n)
	}
	return nets
}()

func netsOverlap(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

func containsIP(nets []*net.IPNet, ip net.IP) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// trustsLocalProxyRange reports whether any trusted range overlaps a
// loopback or private range.
func trustsLocalProxyRange(trusted []*net.IPNet) bool {
	for _, t := range trusted {
		for _, l := range localProxyRanges {
			if netsOverlap(t, l) {
				return true
			}
		}
	}
	return false
}

// warnTrustProxyWithoutLocalRanges logs a startup warning when trustProxy
// trusts no loopback / private address.
//
// 以前は Echo の既定で loopback / private が常に信頼されていたので、
// trustProxy に CDN の範囲だけを書き、nginx は Docker bridge や同一ホストから
// 繋ぐ構成が動いていた。今は書いた範囲しか信頼しないので、その構成は
// 全利用者の IP が nginx のアドレスになり、rate limit のバケットを共有して
// 429 が多発し、IP 履歴も 1 アドレスに集まる。エラーにはしない —
// 前段を置かずに直接公開している構成ではこれが正しい設定なので。
//
// UNIX ソケットで待ち受けているときは出さない。接続元アドレスが無く、
// extractIPFallback が XFF を trusted で絞って読むので、この問題は起きない。
func warnTrustProxyWithoutLocalRanges(trusted []*net.IPNet, socket string) {
	if socket != "" || trustsLocalProxyRange(trusted) {
		return
	}
	slog.Warn("trustProxy trusts no loopback or private address; if the reverse proxy in front of mk-go connects from one (nginx on the same host or a Docker network), every client will be seen as the proxy address",
		"trustProxy", formatNets(trusted),
		"hint", "add the proxy address, or `uniquelocal` (private ranges) / `loopback`, to trustProxy")
}

// warnUntrustedLocalProxy logs once when a request carrying
// X-Forwarded-For arrives directly from an untrusted loopback / private
// address, which usually means the reverse proxy is missing from trustProxy.
//
// ログが溢れないよう warned で 1 回に絞る (extractor はプロセスで 1 つなので
// 実質プロセスに 1 回)。起動時の警告だけだと、trustProxy に private の範囲を
// 含めていても実際の proxy がその外にいる構成 (別の bridge network 等) を
// 取りこぼすので、実際に届いたリクエストでも見る。
func warnUntrustedLocalProxy(req *http.Request, trusted []*net.IPNet, warned *atomic.Bool) {
	if len(req.Header.Values(echo.HeaderXForwardedFor)) == 0 {
		return
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return
	}
	ip := net.ParseIP(host)
	if ip == nil || !containsIP(localProxyRanges, ip) || containsIP(trusted, ip) {
		return
	}
	if !warned.CompareAndSwap(false, true) {
		return
	}
	slog.Warn("received X-Forwarded-For from a loopback or private address that trustProxy does not trust; the client IP is taken as this address (logged once per process)",
		"remoteAddr", ip.String(),
		"hint", "if this is your reverse proxy, add it (or `uniquelocal` / `loopback`) to trustProxy")
}

func formatNets(nets []*net.IPNet) []string {
	out := make([]string, 0, len(nets))
	for _, n := range nets {
		out = append(out, n.String())
	}
	return out
}

// extractIPFallback recovers a client IP for requests where Echo's standard
// extractor would return empty (typically UNIX domain socket connections
// with no RemoteAddr, behind nginx + UDS). XFF を逆順に走査し trusted ranges
// の外で最初に当たる解析可能な IP を返す。XFF が無ければ X-Real-IP に fallback。
// 結果が空文字となる場合 (header が一切無い) は "" を返し、上位の機能
// (signin record / user_ip / rate-limit) は空のまま記録する。
func extractIPFallback(req *http.Request, trusted []*net.IPNet) string {
	cleanCandidate := func(s string) string {
		s = strings.TrimSpace(s)
		s = strings.TrimPrefix(s, "[")
		s = strings.TrimSuffix(s, "]")
		return s
	}
	isTrusted := func(ip net.IP) bool {
		for _, n := range trusted {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}

	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		// untrusted side (client 側) を逆順走査で見つける
		for i := len(parts) - 1; i >= 0; i-- {
			candidate := cleanCandidate(parts[i])
			ip := net.ParseIP(candidate)
			if ip == nil {
				continue
			}
			if !isTrusted(ip) {
				return ip.String()
			}
		}
		// 全部 trusted ranges 内 (典型: client→nginx→mkgo が全部 private) なら
		// 一番左の解析可能な IP を返す。XFF[0] は upstream proxy が書く client IP。
		for _, p := range parts {
			candidate := cleanCandidate(p)
			if net.ParseIP(candidate) != nil {
				return candidate
			}
		}
	}
	if r := cleanCandidate(req.Header.Get("X-Real-IP")); r != "" {
		return r
	}
	return ""
}

// noCORSPaths lists the API paths that must not be usable from a cross-origin
// browser (#2953).
//
// **フォロー一覧を一括で抜いて CSV にし、インポートに食わせる収集**を
// ブラウザから行えなくする。対象は `POST` + `Content-Type: application/json`
// なので**単純リクエストではなくプリフライトが必須**で、CORS ヘッダを出さなけ
// れば越境のブラウザは実リクエストに到達できない。
//
// **壁ではない。** サーバー側プロキシを 1 つ挟めば CORS は無関係になる。狙う
// 費用を上げるための措置で、レート制限 (`ratelimit_defs.go` の同じ 2 つ) と
// 対になっている。
//
// **同一オリジンは影響を受けない。** ブラウザは同一オリジンの応答に CORS 検査
// を適用しないので、ヘッダを出さなくても同梱のフロントエンドは動く (叩き先は
// `window.location.origin + '/api'`)。**Origin の値は見ない** — 同一オリジンの
// POST にもブラウザは Origin を付けるので「Origin があれば越境」は誤りだし、
// `cfg.URL` と突き合わせる形は逆プロキシや別ドメイン運用で壊れる。
//
// **Origin を持たないクライアント (ネイティブアプリ / CLI / 連合) は無影響。**
// 応答ヘッダを見ないため。
var noCORSPaths = map[string]bool{
	"/api/users/following": true,
	"/api/users/followers": true,
	// **signin 系は応答を越境で読ませない。**
	//
	// `ACAO: *` だと、悪意あるサイトが訪問者のブラウザからこれらを叩いて
	// **応答を読める**。読めることで (a) ユーザー名の存在判定 (404 と 200)、
	// (b) パスワード正誤と 2FA 有無のオラクル (403 / `next:"totp"` /
	// `finished:true`)、(c) 成功時のセッショントークンの奪取 が、**訪問者の
	// IP に分散したまま**成立する。1 IP あたりのレート制限を訪問者数ぶん
	// 掛け算できてしまう。
	//
	// upstream は `SigninApiService` / `SigninWithPasskeyApiService` が
	// `Access-Control-Allow-Origin` を `config.url` に**明示的に上書き**して
	// いる (グローバルは `origin: '*'` なので、意図的な上書き)。
	//
	// **同一オリジンのフロントエンドは影響を受けない** — ブラウザは同一
	// オリジンの応答に CORS 検査を適用しない。ネイティブアプリや CLI も
	// 応答ヘッダを見ないので無影響。
	"/api/signin":                 true,
	"/api/signin-flow":            true,
	"/api/signin-with-passkey":    true,
	"/api/signup":                 true,
	"/api/signup-pending":         true,
	"/api/reset-password":         true,
	"/api/request-reset-password": true,
}

// corsRestrictedPath reports whether the path must not advertise CORS.
//
// **AP のコレクションも塞ぐ (レビュー M-1)。** `/users/<id>/following` は
// `s.echo` 直付けで `/api` グループの外にあり、**同じフォローグラフを
// `ACAO: *`・レート制限なし・プリフライト不要 (単純リクエスト) で返す**
// (本番で実測)。`/api` 側だけ費用を上げても、隣に無料の経路が残っていては
// 意味がない。
//
// **連合は壊れない。** AP の取得はサーバー間なので応答ヘッダを見ない。
// ブラウザがこのパスを開く経路は SPA へのフォールバック (同一オリジンの
// 画面遷移) で、CORS の対象外。
func corsRestrictedPath(p string) bool {
	if noCORSPaths[p] {
		return true
	}
	// `/users/<id>/following` / `/followers`。id は可変なので前方 + 後方一致で
	// 見る。広く取っても catchall が空を返すだけで無害な側に倒れる。
	if strings.HasPrefix(p, "/users/") {
		return strings.HasSuffix(p, "/following") || strings.HasSuffix(p, "/followers")
	}
	return false
}

// corsMiddleware returns the global CORS middleware.
// Shared with cors_test.go so production and tests stay in sync.
func corsMiddleware() echo.MiddlewareFunc {
	inner := echomw.CORSWithConfig(echomw.CORSConfig{
		// discovery endpoint は upstream が専用の hook で
		// `Access-Control-Allow-Headers: Accept` だけを広告する。ここで
		// グローバル CORS を通すと preflight に Origin/Content-Type/
		// Authorization まで載って乖離するので除外する (handler 側が
		// setDiscoveryCORS で必要なヘッダを全て付ける)。
		Skipper: func(c echo.Context) bool {
			return strings.HasPrefix(c.Request().URL.Path, "/.well-known/")
		},
		AllowOrigins: []string{"*"},
		AllowMethods: []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions},
		AllowHeaders: []string{echo.HeaderOrigin, echo.HeaderContentType, echo.HeaderAccept, echo.HeaderAuthorization},
	})
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		wrapped := inner(next)
		return func(c echo.Context) error {
			// **`c.Request().URL.Path` で見る。** グローバル middleware の
			// 時点では `c.Path()` が `/api/*` に解決されていることがある。
			if !corsRestrictedPath(c.Request().URL.Path) {
				return wrapped(c)
			}
			// **`Vary: Origin` は自分で戻す。** echo の CORS は Skipper の
			// **後**で `Vary` を付けるので、迂回した経路では落ちる (実測:
			// `/.well-known/` の応答には元から `Vary: Origin` が無い)。
			//
			// **`Set` ではなく `Add`。** 現状 CORS より前に `Vary` を付ける
			// middleware は無い (gzip は後段) ので `Set` でも同じ結果になるが、
			// 順序を入れ替えたときに他の `Vary` を消さないため。
			c.Response().Header().Add(echo.HeaderVary, echo.HeaderOrigin)
			// **プリフライトはここで打ち切る。** 通すと `api.Any("/*")` の
			// catchall が拾って `200 + {}` を返し、プリフライトのたびに
			// 「unimplemented API endpoint」の警告ログを吐く。auth の
			// `io.ReadAll` やレート制限にも OPTIONS が流れる。
			//
			// **拒否はしない (204)。** CORS ヘッダが無い時点でブラウザは
			// 止まるので 403 にしても挙動は変わらず、Origin を付けてくる
			// 非ブラウザのクライアントを壊す側にだけ倒れる。
			if c.Request().Method == http.MethodOptions {
				return c.NoContent(http.StatusNoContent)
			}
			return next(c)
		}
	}
}

// accessLogConfig returns the access log middleware config.
//
// **request URI は query を含む**ので、そのまま出すと `?i=<token>` の形で有効な
// credential がアクセスログに残る (redact package の doc 参照)。`redact.URI` で
// 秘密パラメータの**値だけ**を伏せる。**この配線は必ずテストで固定すること**
// (`TestAccessLogConfig_RedactsToken`) — 壊れても外から見えないので、無検証だと
// 書き換えで黙って credential が出る側に倒れる。
//
// 出力先を引数に取るのはテストと共有するため。`gzipConfig` と同じ意図だが、
// `RequestLoggerConfig` は `Output` を持たず `LogValuesFunc` の中で自分で書くので
// writer を渡す形になる。
func accessLogConfig(out io.Writer) echomw.RequestLoggerConfig {
	return echomw.RequestLoggerConfig{
		LogMethod:  true,
		LogStatus:  true,
		LogLatency: true,
		// **`HandleError` を立てる。** 既定 (false) だと `c.Error(err)` が呼ばれず
		// `res.Status` が更新されないので、handler が素の error を返したときに
		// **クライアントには 500 を返しながらログには 200 と書く**。旧実装は
		// `c.Error(err)` を先に呼んでから status を読んでいた (実測で 17 ケース中
		// 4 ケースが食い違い、これを立てると 17/17 一致する)。
		HandleError: true,
		// `LogURI` は `req.RequestURI` をそのまま入れる (query 込み)。自分で
		// `c.Request().RequestURI` を読んでも同じだが、フラグと使う値を対応させておく。
		LogURI: true,
		LogValuesFunc: func(_ echo.Context, v echomw.RequestLoggerValues) error {
			// 旧 `LoggerWithConfig` の
			// `${time_rfc3339} ${method} ${custom} ${status} ${latency_human}` と
			// 同じ並び・同じ書式。**時刻は `v.StartTime` ではなく `time.Now()`** —
			// 旧実装の `${time_rfc3339}` が書き込み時点を出していたのに合わせる
			// (`StartTime` はリクエスト開始時刻なので値がずれる)。
			_, err := fmt.Fprintf(out, "%s %s %s %d %s\n",
				time.Now().Format(time.RFC3339),
				v.Method,
				redact.URI(v.URI),
				v.Status,
				v.Latency.String(),
			)
			return err
		},
	}
}

// gzipConfig returns the GzipConfig used by the global middleware stack.
// Shared with gzip_test.go so production and tests stay in sync.
func gzipConfig() echomw.GzipConfig {
	// MinLength=1024 で小さい body の gzip overhead を回避し、/streaming は
	// WebSocket frame を壊さないよう Skipper で除外する (#413 Phase 3 #12)。
	return echomw.GzipConfig{
		Skipper: func(c echo.Context) bool {
			// WebSocket upgrade 経路 (/streaming) は frame 単位の bidirectional
			// 通信で gzip すると壊れる。
			return c.Path() == "/streaming"
		},
		Level:     gzip.DefaultCompression,
		MinLength: 1024,
	}
}

// Server timeouts. Go の http.Server はゼロ値が「無制限」なので、明示的に
// 設定しないと slow header / slow body / idle connection を送り続けるだけで
// file descriptor と goroutine を占有できてしまう。
//
// 値は upstream (Node) の既定に合わせてある。upstream も fastify 側で明示
// 設定はしておらず Node の既定に依拠しているので、そこに揃えるのが drop-in
// として自然で、かつ実運用で通っている値でもある。
//
//	headersTimeout 60s  -> ReadHeaderTimeout
//	requestTimeout 300s -> ReadTimeout
const (
	serverReadHeaderTimeout = 60 * time.Second
	serverReadTimeout       = 300 * time.Second
	// keep-alive の idle 上限。Node の既定 (keepAliveTimeout 5s) より長く
	// とってある。前段 nginx が upstream connection を keepalive で使い回す
	// 構成では 5s だと張り直しが頻発して割に合わない。
	serverIdleTimeout = 120 * time.Second
	// Go の既定 (DefaultMaxHeaderBytes) と同じ 1MiB。既定でも無制限では
	// ないが、明示しておかないと「header に上限があるか」をコードから
	// 読み取れない。
	serverMaxHeaderBytes = 1 << 20
)

// resolveMediaProxySecret returns the HMAC key used to sign internal media
// proxy URLs.
//
// 設定に `mediaProxySecret` があればそれを使う。無ければ DB (instance_secret)
// に永続化した乱数を使い、初回起動時に生成する。
//
// 以前は未設定時にインスタンス URL から導出していたが、URL は公開情報なので
// 誰でも同じ鍵を計算できた。mediaproxy の Authorize は署名を allowlist より
// **先に**見るため、署名を偽造できると allowlist ごと迂回でき、mk-go の
// media proxy が upstream と同じ「任意の公開 URL を取得する開いたプロキシ」に
// 退化していた。
//
// 起動ごとのメモリ生成では足りない。署名した URL を別プロセスが検証する構成が
// 成り立たなくなるし、role icon や announcement image のように allowlist に
// 載らない URL は再起動をまたいだ時点で 401 になる (allowlist にある
// avatar / drive / emoji / instance icon は fallback で救われるので、
// 壊れ方が経路ごとに変わって切り分けづらい)。
//
// DB から取れない場合は起動を止める。ここで弱い鍵に fallback すると、直した
// はずの迂回経路が黙って戻る。
func resolveMediaProxySecret(cfg *config.Config, db *gorm.DB) ([]byte, error) {
	if len(cfg.MediaProxySecret) > 0 {
		return cfg.MediaProxySecret, nil
	}
	secret, err := repository.NewInstanceSecretRepository(db).
		GetOrCreate(repository.MediaProxySecretKey)
	if err != nil {
		return nil, fmt.Errorf("resolve media proxy secret: %w", err)
	}
	slog.Info("media proxy secret: using the generated instance secret; set mediaProxySecret in config to manage it yourself")
	return secret, nil
}

// applyServerTimeouts sets read/idle timeouts and the header size cap on srv.
//
// WriteTimeout is deliberately left unset:
//   - streaming (WebSocket) と drive のファイル配信・media proxy は応答が
//     長時間に及ぶ。固定値を置くと正常な応答が途中で切れる
//   - WebSocket 側は gorilla が Upgrade 時に deadline を解除し、その後は
//     internal/stream が pong ベースで自前の deadline を張るので、server の
//     ReadTimeout は WebSocket を殺さない
func applyServerTimeouts(srv *http.Server) {
	if srv == nil {
		return
	}
	srv.ReadHeaderTimeout = serverReadHeaderTimeout
	srv.ReadTimeout = serverReadTimeout
	srv.IdleTimeout = serverIdleTimeout
	srv.MaxHeaderBytes = serverMaxHeaderBytes
}

// New creates a new Server. Returns an error when the queue driver
// fails to initialise (e.g. mkq driver failing to PING Redis at
// startup).
func New(cfg *config.Config, db *gorm.DB, redis *cache.RedisClients) (*Server, error) {
	return newServer(cfg, db, redis, plugin.Registered(), nil)
}

func newServer(cfg *config.Config, db *gorm.DB, redis *cache.RedisClients, plugins []plugin.Definition, openStorage pluginStorageOpener, beforeSetup ...func(*Server)) (*Server, error) {
	// 役割は環境変数で決まる (#2459)。矛盾した指定はここで落として、
	// 「起動はしたがジョブが処理されない」形の失敗にしない。
	role, err := config.ResolveProcessRole()
	if err != nil {
		return nil, err
	}
	if role != config.RoleBoth {
		slog.Info("process role selected", "role", string(role),
			"runsServer", role.RunsServer(), "runsQueue", role.RunsQueue())
	}
	logDevModeBanner(cfg)

	// パスワードのハッシュ強度をプロセス全体に反映する。
	//
	// **ゼロ値は「未設定」として扱う。** config.resolve を通れば必ず範囲内の値が
	// 入るが、Config を構造体リテラルで組む呼び出し元 (テスト / ツール) では 0 の
	// まま来る。そこで起動を失敗させると、この項目を知らない呼び出し元が全部壊れる。
	bcryptCost := cfg.BcryptCost
	if bcryptCost == 0 {
		bcryptCost = password.DefaultCost
	}
	if err := password.SetCost(bcryptCost); err != nil {
		// resolve を通っていれば到達しない。通っていない呼び出し元のために
		// 既定へ落として続行する。
		slog.Warn("bcrypt cost が不正なので既定値を使います", "value", bcryptCost, "err", err)
		bcryptCost = password.DefaultCost
		_ = password.SetCost(bcryptCost)
	}
	if bcryptCost != password.DefaultCost {
		slog.Info("bcrypt cost overridden", "cost", bcryptCost, "default", password.DefaultCost)
	}

	// 投稿後のベストエフォートフックの同時実行数。**絞らないと応答のテールが
	// 伸びる** (負荷時に数百 goroutine が GOMAXPROCS 個の P を奪い合い、応答を
	// 返す goroutine もその列に並ぶ)。0 なら core/note 側の既定が入る。
	corenote.SetHookConcurrency(cfg.NoteHookConcurrency)
	if cfg.NoteHookConcurrency > 0 {
		slog.Info("note hook concurrency overridden", "perKind", cfg.NoteHookConcurrency)
	}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	applyServerTimeouts(e.Server)
	// TLSServer は使っていない (TLS 終端は前段の reverse proxy) が、将来
	// e.StartTLS を使ったときに timeout 無しの server が生えるのを防ぐ。
	applyServerTimeouts(e.TLSServer)
	// 全 endpoint の c.JSON / c.Bind を共通 serializer に通す。実体は stdlib
	// encoding/json (fastJSONSerializer の doc 参照: goccy は #542 の panic で
	// revert、高速 encoder 化は #1142 で見送り)。
	e.JSONSerializer = fastJSONSerializer{}

	// trustProxyからIPExtractorを構成。詳細は buildIPExtractor のコメント。
	// **常に設定する。** 未設定のまま残すと Echo の RealIP は XFF の最左を
	// 無条件に信頼するので、trustProxy の解釈に失敗した / 空だったときに
	// 誰でも IP を詐称できる状態へ倒れる (以前はそうなっていた)。
	trustedNets, err := config.ParseTrustProxy(cfg.TrustProxy)
	if err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	e.IPExtractor = buildIPExtractor(trustedNets)
	warnTrustProxyWithoutLocalRanges(trustedNets, cfg.Socket)

	// Global middleware
	e.Use(echomw.Recover())
	// Echo は Content-Type の charset を `UTF-8` (大文字) で出すが、本家
	// (Fastify) は `utf-8` (小文字) を返す。HTTP 的には case-insensitive でも、
	// 完全一致で分岐するクライアントが実在する (本家 e2e の simpleGet は
	// `text/html; charset=utf-8` に一致しないと body をパースしない) ため、
	// drop-in 互換として本家の表記に揃える。
	e.Use(lowercaseCharset)
	// Sentry middleware は Recover の直後に置く: panic を hub に送ったあと
	// Recover に巻き戻し、5xx の最終整形は echo に任せる。
	e.Use(mksentry.Middleware(cfg))
	e.Use(echomw.RequestID())
	// 設定は accessLogConfig() に集約してテストと共有する (gzipConfig と同じ形)。
	//
	// **エラーを上位へ返さない。** 旧 `LoggerWithConfig` は名前付き戻り値が後続の
	// 代入で上書きされる副作用で handler のエラーを飲んでいた。
	// `RequestLoggerWithConfig` は `return err` するので、そのままだと外側の Sentry
	// middleware が 404 / 405 まで capture し始める — **未認証で誰でも叩ける経路**
	// なので、存在しないパスへ POST を投げるだけで quota を焼ける (`sampleRate` の
	// 既定は 1.0 で全部送る)。`HandleError: true` でレスポンスは確定させたうえで、
	// ここで握って旧挙動に揃える。**飲むこと自体の是非は別途** — 直すなら
	// Sentry 側を「5xx だけ capture」にするのが筋で、この移行の範囲ではない。
	accessLog := echomw.RequestLoggerWithConfig(accessLogConfig(os.Stdout))
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		logged := accessLog(next)
		return func(c echo.Context) error {
			_ = logged(c)
			return nil
		}
	})
	e.Use(corsMiddleware())
	// gzip response compression (#413 Phase 3 #12)。Misskey TS は nginx
	// 前段で gzip するのが定石だが、mk-go は単体運用も想定するので app 側
	// で提供する。設定は gzipConfig() に集約してテストと共有。
	e.Use(echomw.GzipWithConfig(gzipConfig()))

	// CachedUserRepository を一度だけ構築して auth middleware と
	// setupRoutes (services) が同じ cache を共有するようにする (#300 3-3)。
	userRepo := repository.NewCachedUserRepository(repository.NewUserRepository(db))
	accessTokenRepo := repository.NewAccessTokenRepository(db)

	// body size 制限は auth.Authenticate より前に置く: auth は token 抽出のため
	// body を io.ReadAll するので、後に置くと巨大 body が auth で先に読まれて
	// bypass される (#1958 / #2075)。/api → 1MiB / inbox → 64KiB / multipart 除外。
	e.Use(middleware.BodyLimitByPath(cfg.MaxFileSize, peerBodyLimitsByPath(plugins, cfg.Plugins)))
	// クリックジャッキング防止。upstream が ClientServerService の
	// onRequest hook で付けている X-Frame-Options: DENY に相当する。
	e.Use(middleware.FrameGuard())
	// 外部リンク遷移で閲覧中の URL が path ごと漏れるのを防ぐ (#2404)。
	// upstream には無い mk-go 独自の hardening。
	e.Use(middleware.ReferrerPolicy())
	// MIME sniffing を止める (#2782)。**drive とプラグイン proxy にしか付いて
	// いなかった** ので SPA shell も API も素通しだった。これも upstream には無い。
	e.Use(middleware.NoSniff())
	// 使わないブラウザ機能 (カメラ / マイク / 位置情報 / 支払い) を落とす (#2782)。
	// 同じく upstream には無い。fullscreen は動画プレイヤーが使うので落とさない。
	e.Use(middleware.PermissionsPolicy())
	// upstream ServerService と同じ HSTS。**disableHsts を設定として読んで
	// いたのに header を出していなかった**ので、TS から切り替えると黙って
	// 消えていた。https でない構成では付けない。
	e.Use(middleware.HSTS(cfg.URL, cfg.DisableHSTS))
	// Cross-Origin-Opener-Policy (既定 off)。**不正な値は黙って無視されるので、
	// ここで気づけるようにする** — 設定したつもりで効いていない状態が一番厄介。
	if !middleware.ValidCOOPMode(cfg.CrossOriginOpenerPolicy) {
		slog.Warn("crossOriginOpenerPolicy が不正なので無視します",
			"value", cfg.CrossOriginOpenerPolicy,
			"accepted", []string{middleware.COOPOff, middleware.COOPSameOriginAllowPopups, middleware.COOPSameOrigin})
	}
	e.Use(middleware.COOP(cfg.CrossOriginOpenerPolicy))

	// WWW-Authenticate は auth.Authenticate より外側に置く。auth は無効 token に
	// 対して自分で 401 を書くので、内側 (api グループ) に置くと middleware まで
	// 到達せずヘッダが付かない。/streaming の 401 も同じ経路なので、ここに
	// 置けば両方カバーできる。JSON の error body が無い応答では何もしない。
	e.Use(middleware.WWWAuthenticate())

	auth := middleware.NewAuthMiddleware(userRepo, accessTokenRepo)
	e.Use(auth.Authenticate())

	// queue driver セットアップ。Host が UNIX domain socket パス ("/" 始まり)
	// のときは driver 内部で Network を unix に切り替える。
	// プラグイン専用のキュー (#2818)。**driver を作る前に決める** — worker が
	// 見るキューの一覧は構築時に固定される。
	pluginQueues := pluginJobQueueNames(plugins, cfg.Plugins)
	queueDriver, err := buildQueueDriver(context.Background(), cfg, pluginQueues)
	if err != nil {
		return nil, fmt.Errorf("server: build queue driver: %w", err)
	}
	queueClient := queue.NewClient(queueDriver)
	applyClientPolicies(queueClient, cfg)
	queueServer := queue.NewServer(queueDriver)
	queueScheduler := queue.NewScheduler(queueDriver)
	queueInspector := queue.NewInspector(queueDriver)

	s := &Server{
		echo:           e,
		userRepo:       userRepo,
		config:         cfg,
		db:             db,
		redis:          redis,
		auth:           auth,
		queueDriver:    queueDriver,
		queueClient:    queueClient,
		queueServer:    queueServer,
		queueScheduler: queueScheduler,
		queueInspector: queueInspector,
		queueMetrics:   newQueueMetrics(queueDriver),

		// admin UI 向けの短期ランタイム統計 (#2277)。config に関係なく常に
		// 有効。ring buffer 固定長なのでメモリは queue 数 × 数 KB。
		queueRuntimeStats: runtimestats.New(),

		// admin/server-metrics の uptime 起点 (#2395)。プロセス生成時刻なので
		// Server 生成時に確定させる。
		startedAt: time.Now(),

		// role は New の入口で解決済み (#2459)。setupRoutes がこれを見て
		// 背景処理を出し分ける。
		role:                role,
		reactionFlushWorker: runReactionFlushWorker,
	}
	for _, fn := range beforeSetup {
		fn(s)
	}

	// driver の dispatch hook を Prometheus と runtimestats の両方へ配る。
	// SetObserver は Start 前に呼ぶ必要がある (Start が closure に snapshot する)。
	if setter, ok := queueDriver.Server().(interface{ SetObserver(driver.Observer) }); ok {
		s.queueRuntimeStatsObserver(setter)
	}

	mediaProxySecret, err := resolveMediaProxySecret(cfg, db)
	if err != nil {
		// 弱い鍵に fallback すると、直したはずの allowlist 迂回が黙って戻る。
		s.cleanupConstruction()
		return nil, err
	}
	s.mediaProxySecret = mediaProxySecret

	if openStorage == nil {
		openStorage = s.dbBackedStorage()
	}
	s.setupRoutes(plugins, openStorage)
	if err := s.constructionError(); err != nil {
		s.cleanupConstruction()
		return nil, err
	}

	// setupRoutesの後にfallibleな初期化は置かない。pluginの保留workは完成した
	// route treeだけを観測できる状態で、return直前にreleaseする。
	if s.beforePluginGoRelease != nil {
		s.beforePluginGoRelease()
	}
	if s.pluginGoGate != nil {
		s.pluginGoGate.open()
	}
	return s, nil
}

func (s *Server) cleanupConstruction() {
	s.shutdownOnce.Do(func() {
		if s.pluginGoGate != nil {
			s.pluginGoGate.discard()
		}
		for _, hook := range s.shutdownHooks {
			hook(context.Background())
		}
		s.shutdownHooks = nil
		if s.queueDriver != nil {
			if err := s.queueDriver.Close(); err != nil {
				slog.Warn("queue driver close failed during server construction rollback", "err", err)
			}
			s.queueDriver = nil
		}
	})
}

// Handler returns the underlying http.Handler for use with httptest.
// E2Eテスト等でサーバーを外部から起動する場合に使う���
func (s *Server) Handler() http.Handler {
	return s.echo
}

// DumpedRoute represents a single registered Echo route, used by DumpRoutes
// to expose mk-go's HTTP surface for tooling (api-compat matrix etc).
type DumpedRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// DumpedRoutes is the JSON payload emitted by DumpRoutes.
type DumpedRoutes struct {
	MisskeyVersion string        `json:"misskeyVersion"`
	MkGoVersion    string        `json:"mkGoVersion"`
	Routes         []DumpedRoute `json:"routes"`
}

// DumpRoutes writes all registered Echo routes as JSON to w.
// `cmd/misskey -dump-routes` から呼ばれ、tools/apicompat が Misskey TS の
// endpoint 集合 (filename-derived + ApiServerService 直登録) と突き合わせる
// ための入力になる。echo 内部の "/*" catch-all 等は除外しない (caller 側で
// 正規化する想定)。出力 path は string sort で安定化。
//
// Phase 2 で TS の api.json ベースに切り替える際もこの method の出力形式は
// そのまま使える想定 (input source 側だけ差し替え)。
func (s *Server) DumpRoutes(w io.Writer) error {
	routes := s.echo.Routes()
	dumped := make([]DumpedRoute, 0, len(routes))
	for _, r := range routes {
		dumped = append(dumped, DumpedRoute{Method: r.Method, Path: r.Path})
	}
	sort.Slice(dumped, func(i, j int) bool {
		if dumped[i].Path != dumped[j].Path {
			return dumped[i].Path < dumped[j].Path
		}
		return dumped[i].Method < dumped[j].Method
	})
	payload := DumpedRoutes{
		MisskeyVersion: config.MisskeyVersion,
		MkGoVersion:    config.MkGoVersion,
		Routes:         dumped,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

// StartBackgroundForTest starts the queue worker (and optional
// scheduler / chart management) without launching the HTTP listener.
// 用途: e2e_federation 系のテストで `httptest.Server` 経由で echo handler を
// 外部 listener にぶら下げつつ、deliver / inbox 処理など async queue 経路も
// 動かしたい場合 (#435)。本番経路 (Server.Start) は HTTP listener を含めて
// すべて起動するが、テストでは echo を test 側が制御するためここから queue
// 部分だけ抽出する。
//
// production code から呼ばないこと。
func (s *Server) StartBackgroundForTest() error {
	if err := s.queueServer.Start(); err != nil {
		return fmt.Errorf("start queue worker: %w", err)
	}
	// queueServer.Start 後にのみ controller を起動する (Start 前は driver.Resize
	// が ErrResizeNotSupported を返す = no pool yet)。startAutoScale は
	// jobQueueAutoScale=false なら nil runner を返す cheap no-op。
	runner, err := startAutoScale(context.Background(), s.config, s.queueDriver, s.queueMetrics, s.queueRuntimeStats)
	if err != nil {
		return fmt.Errorf("start autoscale: %w", err)
	}
	s.autoscale = runner
	s.registerSchedulerJobs()
	return nil
}

// registerSchedulerJobs registers every periodic cron job and starts the
// scheduler. Called from both the normal and autoscale start paths so the
// cron set lives in one place (#1563): adding a job here covers both paths.
func (s *Server) registerSchedulerJobs() {
	if s.queueScheduler == nil {
		return
	}
	// Register エラーは観測可能にする (Register 失敗は scheduler driver 不具合
	// の signal)。1 job の失敗で他の登録を止めない。
	jobs := []struct {
		name string
		fn   func() error
	}{
		{"chart", s.queueScheduler.RegisterChartJobs},
		{"instance refresh", s.queueScheduler.RegisterInstanceRefreshJob},
		{"retention", s.queueScheduler.RegisterRetentionJob},
		{"checkExpiredMutings", s.queueScheduler.RegisterCheckExpiredMutingsJob},
		{"clean", s.queueScheduler.RegisterCleanJob},
		{"chunkedUploadGc", s.queueScheduler.RegisterChunkedUploadGCJob},
		{"cleanRemoteNotes", s.queueScheduler.RegisterCleanRemoteNotesJob},
		{"orphanUserCleanup", s.queueScheduler.RegisterOrphanUserCleanupJob},
		{"orphanAttachmentCleanup", s.queueScheduler.RegisterOrphanAttachmentCleanupJob},
		{"checkModeratorsActivity", s.queueScheduler.RegisterCheckModeratorsActivityJob},
	}
	for _, j := range jobs {
		if err := j.fn(); err != nil {
			slog.Warn("scheduler register failed", "job", j.name, "err", err)
		}
	}
	// **登録しなかった cron を撤去する (#3173)。** プラグインの cron は router の
	// 構築中 (setupPlugins) に登録済みなので、ここが全登録の後になる。登録先や
	// 名前を変えた cron の旧スケジューラは、消さないと永久に発火し続ける。
	removed, err := s.queueScheduler.PruneUnregistered()
	for _, r := range removed {
		slog.Info("scheduler: removed a schedule that is no longer registered", "schedule", r)
	}
	if err != nil {
		slog.Warn("scheduler prune failed", "err", err)
	}
	if err := s.queueScheduler.Start(); err != nil {
		slog.Warn("scheduler start failed", "err", err)
	}
}

// SetSyncDeliverHookForTest replaces the queued deliver enqueue with the
// supplied synchronous hook. e2e_federation 系テストで queue worker 経由の
// deliver が動かない/動かしたくないシナリオで、sign + HTTP POST を inline
// で実行する用途。fn=nil で本番経路 (queue) に戻る。
//
// production code から呼ばないこと (#780)。
func (s *Server) SetSyncDeliverHookForTest(fn func(payload queue.DeliverPayload) error) {
	if s.deliverSvc != nil {
		s.deliverSvc.SetSyncDeliverHookForTest(fn)
	}
}

// Start begins listening on the configured port (or UNIX domain socket) and
// launches the queue worker.
//
// If s.config.Socket is non-empty the HTTP server binds to that path instead
// of a TCP port. This matches Misskey 本家 YAML の `socket` / `chmodSocket`
// 設定と同じ運用感覚で使える。
func (s *Server) Start() error {
	// queue 側 (worker / autoscale / scheduler) は RoleServer では起動しない。
	// Web ノードもジョブを **enqueue** はするので queueClient は生かしたまま、
	// 消費側だけ止める (#2459)。
	if s.role.RunsQueue() {
		if err := s.queueServer.Start(); err != nil {
			return fmt.Errorf("start queue worker: %w", err)
		}
		// queueServer.Start 後にのみ controller を起動する (Start 前は driver.Resize
		// が ErrResizeNotSupported を返す = no pool yet)。startAutoScale は
		// jobQueueAutoScale=false なら nil runner を返す cheap no-op。
		runner, err := startAutoScale(context.Background(), s.config, s.queueDriver, s.queueMetrics, s.queueRuntimeStats)
		if err != nil {
			return fmt.Errorf("start autoscale: %w", err)
		}
		s.autoscale = runner
		s.registerSchedulerJobs()
	}
	// chart は **両方の role で回す**。メモリ上の集計バッファはプロセスごとに
	// 溜まり (API 経由の投稿は server、連合 inbox 由来は queue)、自分の分は
	// 自分で flush しないと失われる。
	if s.chartMgmt != nil {
		if err := s.chartMgmt.Start(context.Background()); err != nil {
			slog.Warn("chart management service start failed", "err", err)
		}
	}

	if !s.role.RunsServer() {
		return s.serveQueueOnly()
	}

	if s.config.Socket != "" {
		ln, err := config.ListenUnixSocket(s.config.Socket, s.config.ChmodSocket)
		if err != nil {
			return err
		}
		s.echo.Listener = ln
		slog.Info("starting Misskey server",
			"socket", s.config.Socket, "url", s.config.URL)
		// Echo.Start は内部で net.Listen してしまうので、ここでは Start では
		// なく Serve を使って既に張った listener を使う。
		if err := s.echo.Server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}

	addr := fmt.Sprintf(":%d", s.config.Port)
	slog.Info("starting Misskey server", "addr", addr, "url", s.config.URL)
	return s.echo.Start(addr)
}

// Shutdown gracefully shuts down the server, the queue worker and
// any background services such as the chart management loop.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		s.shutdownErr = s.shutdown(ctx)
	})
	return s.shutdownErr
}

func (s *Server) shutdown(ctx context.Context) error {
	// 登録順に shutdown hook を走らせる。publisher goroutine の clean 停止と、
	// graceful drain が必要な service (hashtag #727 等) の Shutdown(ctx) を
	// 兼ねる。hook が ctx を取るので timeout 共有が可能 (#764)。
	for _, hook := range s.shutdownHooks {
		hook(ctx)
	}
	if s.chartMgmt != nil {
		s.chartMgmt.Stop(ctx)
	}
	// queueServer.Shutdown 前に controller を停止 (Resize 呼びの停止 = pool に
	// 余計な変更が走らない状態で queueServer の pool を畳める)。Shutdown ctx を
	// 伝播することで autoscale の goroutine drain にも graceful deadline が効く。
	s.autoscale.Stop(ctx)
	// RoleServer では scheduler も worker も起動していない。mkq driver は
	// どちらの Shutdown も未起動で安全だが、「起動済み」を前提にした driver へ
	// 差し替わっても壊れないよう role で守る (#2459)。
	if s.role.RunsQueue() {
		if s.queueScheduler != nil {
			s.queueScheduler.Shutdown()
		}
		s.queueServer.Shutdown()
	}
	// queueClient.Close を直接呼ばないこと。queueDriver.Close が
	// Client / Inspector を含むサブコンポーネントの Close を一括処理する。
	// mkq driver は Client.Close が no-op で接続は driver 本体が持つので、
	// 閉じ口は driver.Close の一本に統一する。
	if s.queueDriver != nil {
		if err := s.queueDriver.Close(); err != nil {
			slog.Warn("queue driver close failed", "err", err)
		}
	}
	// RoleQueue では echo を listen していないので、代わりに最小 server を閉じる。
	// echo.Shutdown 自体は listener 無しでも無害だが、閉じる対象を取り違えると
	// in-flight な /healthz が切られないまま抜ける。
	var err error
	if s.queueOnlyServer != nil {
		err = s.queueOnlyServer.Shutdown(ctx)
	} else {
		err = s.echo.Shutdown(ctx)
	}
	// UDS listen していた場合、Shutdown で net.Listener.Close() は呼ばれる
	// が、ソケットファイル自体は残るので明示的に unlink しておく。
	if rmErr := config.RemoveUnixSocket(s.config.Socket); rmErr != nil {
		slog.Warn("failed to remove socket file", "socket", s.config.Socket, "err", rmErr)
	}
	return err
}

// setChartManagement registers the chart management service so its
// save loop is started/stopped together with the HTTP server. Called
// from setupRoutes after the chart engines are constructed.
func (s *Server) setChartManagement(m *chart.ManagementService) {
	s.chartMgmt = m
}

// queueRuntimeStatsObserver wires the driver dispatch hook to both the
// Prometheus histograms and the admin-facing runtimestats recorder (#2277).
//
// 分離しているのは、Prometheus 側は長期の scrape 用 / runtimestats 側は
// admin UI が読む短期 snapshot 用で寿命が違うため。driver からは 1 本の
// Observer に見せる。
func (s *Server) queueRuntimeStatsObserver(setter interface{ SetObserver(driver.Observer) }) {
	obs := queuemetrics.MultiObserver{
		queuemetrics.NewObserver(s.queueMetrics),
		s.queueRuntimeStats,
	}
	setter.SetObserver(obs)
}

// lowercaseCharset rewrites `charset=UTF-8` to `charset=utf-8` in the
// Content-Type of every response.
//
// Echo は charset を大文字で出すが、本家 (Fastify) は小文字で返す。HTTP の
// 仕様上は case-insensitive なので実害は無いはずだが、Content-Type の完全一致
// で分岐するクライアントが実在する。本家の e2e ヘルパ (test/utils.ts の
// simpleGet) がまさにそれで、`text/html; charset=utf-8` に一致しないと HTML を
// パースせず body を null にしてしまう。drop-in 互換として表記を揃える。
func lowercaseCharset(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		res := c.Response()
		res.Before(func() {
			ct := res.Header().Get(echo.HeaderContentType)
			if fixed, ok := rewriteCharset(ct); ok {
				res.Header().Set(echo.HeaderContentType, fixed)
			}
		})
		return next(c)
	}
}

// rewriteCharset returns the Content-Type with an upper-case UTF-8 charset
// lowered, reporting whether a rewrite happened.
func rewriteCharset(ct string) (string, bool) {
	const upper = "charset=UTF-8"
	if idx := strings.Index(ct, upper); idx >= 0 {
		return ct[:idx] + "charset=utf-8" + ct[idx+len(upper):], true
	}
	// charset の無い application/json には付ける。本家 (Fastify) は JSON を
	// 常に `application/json; charset=utf-8` で返すので、Content-Type の完全
	// 一致で分岐するクライアントのために揃える。完全一致に限定して、他の
	// JSON 系 MIME (application/activity+json 等) には手を出さない。
	if ct == echo.MIMEApplicationJSON {
		// echo.MIMEApplicationJSONCharsetUTF8 は大文字 UTF-8 なので使わない。
		return echo.MIMEApplicationJSON + "; charset=utf-8", true
	}
	return ct, false
}
