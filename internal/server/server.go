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

	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/core/event"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/pluginaccount"
	"github.com/elythia-network/elythia/internal/core/pluginsecret"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/misc/password"
	"github.com/elythia-network/elythia/internal/misc/redact"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	queuemetrics "github.com/elythia-network/elythia/internal/queue/metrics"
	"github.com/elythia-network/elythia/internal/queue/runtimestats"
	"github.com/elythia-network/elythia/internal/repository"
	mksentry "github.com/elythia-network/elythia/internal/sentry"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/elythia-network/elythia/plugin"
	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"gorm.io/gorm"
)

// Server represents the HTTP server.
type Server struct {
	echo *echo.Echo
	// userRepo は CachedUserRepository でラップ済みの共有 instance。
	// auth middleware と setupRoutes の両方からこれを使うことで、片側の
	// mutation で他方の cache が stale 化するのを防ぐ (#300 3-3)。
	userRepo repository.UserRepository
	// userCache は userRepo の中身 (同じもの)。他のプロセスとキャッシュの
	// 無効化をやりとりする配線 (#3664) が、具体的な型を要る。
	userCache *repository.CachedUserRepository
	// crossProcessCaches は、他のプロセスと無効化をやりとりする部品 (#3664)。
	// 配線の検査が、受けたプロセスで実際に捨てた (読み直した) ことを見るために
	// 持つ。本体のコードからは使わない。
	crossProcessCaches crossProcessCaches
	// relationCache は閲覧者ごとのミュート・ブロック・フォローの一覧 (#3607)。
	// 関係の repository と userRepo の書き込みがここを捨てる。
	relationCache  *repository.RelationCache
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
	// streamManager / streamPubSub / internalPubSub は世代ごとに作り直すもの
	// (#3463)。setupRoutes が入れ、StopBackground が閉じる。以前は setupRoutes の
	// ローカル変数で、止める口が無かった。
	streamManager  *stream.Manager
	streamPubSub   *event.PubSubService
	internalPubSub *event.PubSubService

	// pluginRoles はプラグインのルートで権限を判定するための参照 (#2477)。
	// setupRoutes で roleService を作った後に入る。
	pluginRoles middleware.RoleChecker
	// pluginAccounts はプラグインが管理するアカウント (#3468) の操作。
	// setupRoutes が setupPlugins より前に入れる。nil のときは、各プラグインの
	// ctx.Accounts() が呼ぶとエラーを返す実装になる。
	pluginAccounts *pluginaccount.Service
	// pluginSecrets はプラグインの秘密の値 (#3470) を暗号化して出し入れする。
	// setupRoutes で setupPlugins より前に入る。nil のときは秘密の値の入力口を
	// 張らず、プラグインには鍵の無い口 (呼ぶと ErrSecretsUnavailable) を渡す。
	pluginSecrets *pluginsecret.Service
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
	// passwordHasherは設定のbcryptCostから作ったHasher(#3780)。
	// setupRoutesがパスワードをハッシュする部品へ渡す。
	passwordHasher password.Hasher

	// deliverSvc は federation deliver service への参照。本番では queue
	// 経由で deliver を enqueue するが、test (#780) で queue を bypass する
	// ための SetSyncDeliverHookForTest を呼べるよう参照を保持する。
	deliverSvc *corefederation.DeliverService

	// lifecycle は、この世代の部品のうち停止が要るものの名前付きの一覧 (#3699)。
	// Shutdown() 時に queue の worker を止め始めた後 (上限内に止まらなかった worker
	// は並行して走る)、chart と HTTP echo より先に、登録した順に止める。
	// ctx を渡すので graceful drain (例: hashtag service の in-flight worker、
	// #727) もここで止められる。テストが組む Server のリテラルでも使えるよう、
	// ポインタではなく値で持つ (ゼロ値が空の一覧)。
	lifecycle lifecycle
	// caches は、この世代のキャッシュの名前付きの一覧 (#3721)。登録した順に捨てる。
	// 本番ではまだ誰も捨てない (段階 (6) で使う)。lifecycle と同じく値で持つ。
	caches       cacheList
	shutdownOnce sync.Once
	shutdownErr  error
	// startOnce makes StartBackground and StartBackgroundForTest start the
	// generation once, whichever is called first; later calls of either
	// return startErr, the result of the first (#3703, #3709).
	startOnce sync.Once
	startErr  error
	// runMu orders the start of the generation against its stop (#3709). The
	// start holds it for its whole run; the stop (shutdown) and the rollback
	// of a failed construction (cleanupConstruction) take it only to set
	// stopped, before they stop anything.
	//
	// 起動の間は握ったままにする。起動は部品のgoroutineを立てるほか、cronの登録
	// (registerSchedulerJobsの11個の登録の関数)と撤去(PruneUnregistered)でRedisを
	// context.Background()のまま呼ぶので、その分だけ長くかかりうる。本番の経路では
	// StartBackgroundとStopBackgroundをどちらもSupervisorのgenMuの下で呼ぶので、
	// 固まった起動は#3709の前から停止を止めていた(Supervisor.ShutdownはlockBeforeで
	// 見切る)。runMuが本番に足す待ちは無い。走っている起動を待つのは、
	// Server.StopBackgroundを直接呼ぶもの(テスト)だけで、その待ちはctxを見ない。
	// 停止は印を立てる間だけ握り、部品やqueueを止めて待つ間は握らない。起動が握ったまま
	// 取るのは一覧のmutex・chartの管理のmutex・queueのdriverの中のmutexで、停止の側が
	// runMuを握ったままそれらを待つことは無いので、待ち合いにならない。
	runMu sync.Mutex
	// stopped is set, under runMu, once the stop or the rollback of the
	// generation has begun. A start that sees it starts nothing and returns
	// errGenerationStopped.
	stopped bool
	// queueStarted is set, under runMu, once the start has started the queue
	// worker (#3713). stopQueue then shuts the worker and the scheduler down
	// whatever the role, so what StartBackgroundForTest started on a server
	// role stops too.
	//
	// 停止(stopQueue)はrunMuを取らずに読む。停止は先にmarkStoppedでrunMuを取り、
	// 済んだ起動はそれより前にrunMuを放しているので、起動の書き込みは停止の読みより
	// 前に起きる(happens-before)。markStoppedより後の起動は何も起動せず、書かない。
	queueStarted bool

	// outboundTr は外向きの HTTP で共有する SSRF-safe な Transport
	// (outboundTransport)。初めて使うときに 1 度だけ作る (#3591)。
	outboundTrOnce sync.Once
	outboundTr     *http.Transport

	// reactionFlushWorkerは全部の役割で回すリアクションのtickのloop。tickごとに
	// metaから設定を読み直し、flushを積むのはqueue roleだけ(#3663)。field化は
	// constructor rollbackをsleep無しで検証し、起動前にownershipを登録するため。
	reactionFlushWorker func(context.Context, func() error)
	// reactionCountWriter は世代のリアクションの書き込み先 (#3663)。テストが
	// 取りこぼしたプロセスの状態を作るために読む。
	reactionCountWriter *corereaction.SwitchableWriter
	// background は、この世代で応答の後などに投げっぱなしで起動する goroutine
	// (投稿後の hook、サインインの記録、メールなど) を数える箱 (#3661)。
	// 世代の停止 (shutdown) で上限付きで待ち、待ちの終わり近くで ctx を切る。
	background            *bgwork.Group
	pluginGoStarter       func(func())
	pluginGoGate          *pluginGoGate
	beforePluginGoRelease func()
	// metaUpdatedHandled is a test hook called after this process has handled
	// one internal:metaUpdated message (#3663). 本番では nil。
	//
	// テストが「別のプロセスの通知 1 回」で効くことを、処理の終わりを待って
	// 確かめるために使う。目印を同じトピックへ流して待つと購読が 2 回走り、
	// 購読の中の順序の誤りを見逃す。
	//
	// **購読の最後 (全部の処理の後) で呼ぶこと。** 前で呼ぶと、テストが処理の
	// 終わる前に検査を始める。
	metaUpdatedHandled atomic.Pointer[func(payload []byte)]
}

func runReactionFlushWorker(ctx context.Context, enqueue func() error) {
	ticker := time.NewTicker(corereaction.FlushInterval)
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

// selfEncodedRoutes are the routes whose handlers negotiate Content-Encoding
// themselves, so the global gzip middleware must not wrap them (it does not
// look at an existing Content-Encoding and would compress the body again).
//
//   - `/vite/*` / `/embed_vite/*`: 事前圧縮の .br / .gz を返し、無ければ
//     自前で gzip する (serveFrontendBuild、dev では frontendDynamicGzip)
//   - `/api/emojis`: 一覧が変わるまで gzip 済みの本文を使い回す (#3593)
var selfEncodedRoutes = map[string]bool{
	"/vite/*":       true,
	"/embed_vite/*": true,
	"/api/emojis":   true,
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
			// /backup-download は数 GB の dump をそのまま渡す。gzip し直すと CPU を
			// 使い、Content-Length が消えて進捗が出ず、Range の応答まで包まれる。
			return c.Path() == "/streaming" || c.Path() == "/backup-download" || selfEncodedRoutes[c.Path()]
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

// constructionMu serializes server construction. newServer replaces
// process-wide state (the setters TestProcessGlobalsAreRestored lists, e.g.
// entity.Set* / notehide.Set*) while it wires the routes.
//
// 本番では 1 プロセスに Supervisor は 1 つで、世代の作り直しは genMu で直列に
// なる。それでも、同じプロセスで 2 つの構築が重なると (2 つの Supervisor を立てる
// テストで実際に起きた) グローバルへの書き込みが競合するので、構築ごと直列にする。
// 構築は起動時とメンテナンスを抜けるときにしか走らないので、待ちは問題にならない。
var constructionMu sync.Mutex

func newServer(cfg *config.Config, db *gorm.DB, redis *cache.RedisClients, plugins []plugin.Definition, openStorage pluginStorageOpener, beforeSetup ...func(*Server)) (*Server, error) {
	constructionMu.Lock()
	defer constructionMu.Unlock()
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

	// パスワードのハッシュ強度。使う部品(signup・signin・パスワードの変更と
	// リセット)へsetupRoutesが渡す(#3780)。0と範囲外の扱いはHasherFromConfigに
	// ある。起動のログはここで出し、出る位置を変えない。
	passwordHasher := password.HasherFromConfig(cfg.BcryptCost)
	if passwordHasher.Cost() != password.DefaultCost {
		slog.Info("bcrypt cost overridden", "cost", passwordHasher.Cost(), "default", password.DefaultCost)
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
	// body の無い API 呼び出しを本家と同じく INVALID_PARAM にする (apiBinder 参照)。
	e.Binder = &apiBinder{}

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
	//
	// relationCache は userRepo より先に作る。HardDeleteUser の CASCADE で消える
	// 関係の行を、userRepo の側から捨てるため (#3607)。
	relationCache := repository.NewRelationCache(db)
	userRepo := repository.NewCachedUserRepository(repository.NewUserRepositoryWithRelationCache(db, relationCache))
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

	// 本家の requireFile endpoint (drive/files/create) は、token を読む前に
	// multipart の file を取り出し、取れなければ本文の無い 400 を返す
	// (ApiCallService.ts の handleMultipartRequest)。無効な token の 401 より
	// 先に答えるため、auth.Authenticate より前に置く (#3330)。
	e.Use(middleware.RequireMultipartFile(multipartUploadRoutes...))

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
		userCache:      userRepo,
		relationCache:  relationCache,
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
		background:          bgwork.New(),
		passwordHasher:      passwordHasher,
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
		// 構築に失敗した世代は使われないが、後から起動を呼ばれても何も起動しないよう、
		// 停止と同じ印を立てる(#3709)。段の表の外に置くので、段のpanicで飛ばない。
		s.markStopped()
		// エラーを返す口は無い。段のpanicはrunShutdownStepsが段ごとに記録し、一覧の
		// 部品の停止のpanicはstopAllが部品ごとに記録するので、ここでは重ねて記録しない
		// (#3707、#3711)。
		_ = runShutdownSteps(context.Background(), s.cleanupSteps())
	})
}

// cleanupSteps returns the steps of rolling back a failed construction, in
// the order they run (docs/architecture.md 4.3). It is run with
// context.Background().
func (s *Server) cleanupSteps() []shutdownStep {
	return []shutdownStep{
		{name: "pluginGoGate", run: func(context.Context) error {
			if s.pluginGoGate != nil {
				s.pluginGoGate.discard()
			}
			return nil
		}},
		{name: "lifecycle", run: func(ctx context.Context) error {
			// 一覧の部品は、構築では起動していない(#3703)。起動していない部品の停止は
			// 待たずに返る(docs/architecture.md 4.3)。停止のpanicはstopAllが部品ごとに
			// 回収して記録するので、後の段は続く(#3707)。エラーはcleanupConstructionが
			// 捨てる(記録はstopAllの1部品1行で足りる)。
			return s.lifecycle.stopAll(ctx)
		}},
		{name: "background", run: func(ctx context.Context) error {
			// 構築の途中で起動したものがあれば止める (#3661)。構築に失敗した世代は
			// 使われないので、待ちは短くてよい。
			bgCtx, bgCancel := context.WithTimeout(ctx, time.Second)
			s.background.Stop(bgCtx)
			bgCancel()
			return nil
		}},
		{name: "releaseParts", run: func(context.Context) error {
			s.lifecycle.parts = nil
			// キャッシュの一覧も、失敗した世代のハンドラへの参照を手放す (#3721)。
			s.caches.parts = nil
			return nil
		}},
		{name: "queueDriver", run: func(context.Context) error {
			if s.queueDriver != nil {
				if err := s.queueDriver.Close(); err != nil {
					slog.Warn("queue driver close failed during server construction rollback", "err", err)
				}
				s.queueDriver = nil
			}
			return nil
		}},
		{name: "streaming", run: func(ctx context.Context) error {
			// setupRoutes が作った購読も閉じる。メンテナンスの後の作り直しは失敗すると
			// 繰り返すので、閉じないと試すたびに Redis の接続と goroutine が漏れる (#3463)。
			if s.streamManager != nil {
				closeCtx, cancel := context.WithTimeout(ctx, time.Second)
				s.streamManager.CloseAll(closeCtx, stream.ServiceRestartCloseCode, "server restarting")
				cancel()
			}
			return nil
		}},
		{name: "pubsub", run: func(context.Context) error {
			s.closePubSubs()
			return nil
		}},
	}
}

// closePubSubs closes the Redis subscriptions of this generation.
func (s *Server) closePubSubs() {
	for _, ps := range []*event.PubSubService{s.internalPubSub, s.streamPubSub} {
		if ps != nil {
			_ = ps.Close()
		}
	}
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
// `elythia dump-routes` から呼ばれ、tools/apicompat が Misskey TS の
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

// StartBackgroundForTest starts the named parts of the generation, the queue
// worker and the optional auto-scale and scheduler, without launching the
// HTTP listener. Unlike StartBackground it does not look at the process role
// and does not start the chart management.
//
// It goes through the same once as StartBackground (#3709): only the first
// call of either starts anything, and later calls of either return the
// result of the first. Once StopBackground has begun, it starts nothing and
// returns errGenerationStopped.
//
// StopBackground stops the queue worker, auto-scale and scheduler it started
// whatever the process role (#3713), as it does for StartBackground.
//
// 用途: e2e_federation 系のテストで `httptest.Server` 経由で echo handler を
// 外部 listener にぶら下げつつ、deliver / inbox 処理など async queue 経路も
// 動かしたい場合 (#435)。本番経路 (Server.Start) は HTTP listener を含めて
// すべて起動するが、テストでは echo を test 側が制御するためここから queue
// 部分だけ抽出する。
//
// production code から呼ばないこと。
func (s *Server) StartBackgroundForTest() error {
	return s.startOnceWith(backgroundStart{anyRole: true})
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
		{"resendQuoteRequests", s.queueScheduler.RegisterResendQuoteRequestsJob},
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

// StartBackground launches the background work of this server generation, in
// this order: the named parts of the lifecycle list in list order (#3703),
// the queue worker, auto-scale and scheduler (when the process role runs the
// queue) and the chart save loop. It does not listen; the Supervisor owns the
// listener and routes requests to Handler / ServeHandler.
//
// Only the first call of StartBackground or StartBackgroundForTest starts
// anything; later calls of either return its result. When it returns an
// error, the caller stops the generation with StopBackground, which also
// stops the parts that did not start. Once StopBackground has begun (or the
// construction failed), it starts nothing and returns errGenerationStopped;
// a start that is running when StopBackground begins finishes first, and
// StopBackground then stops what it started (#3709).
//
// listener を Server から外したのは、メンテナンスで Server を作り直しても
// listener を張ったままにするため (#3463)。
func (s *Server) StartBackground() error {
	return s.startOnceWith(backgroundStart{startChart: true})
}

// errGenerationStopped is returned by StartBackground and
// StartBackgroundForTest when the stop of the generation (or the rollback of
// its failed construction) began before the start (#3709).
var errGenerationStopped = errors.New("server: the generation has been stopped")

// backgroundStart is what a start of the generation starts besides the parts
// of the lifecycle list and the queue worker, auto-scale and scheduler.
type backgroundStart struct {
	// anyRole starts the queue worker, auto-scale and scheduler whatever the
	// process role (StartBackgroundForTest). Otherwise they start only when
	// the role runs the queue.
	anyRole bool
	// startChart starts the chart save loop (StartBackground).
	startChart bool
}

// startOnceWith starts the generation, with the differences in what, unless
// it was started already (it then returns the result of the first start) or
// its stop has begun (errGenerationStopped).
//
// StartBackgroundForTestもstartOnceを通す(#3709)。通さないと、StartBackgroundと
// 両方を呼んだときにqueueのStartを2回呼ぶ。
func (s *Server) startOnceWith(what backgroundStart) error {
	s.startOnce.Do(func() {
		s.startErr = s.startBackground(what)
	})
	return s.startErr
}

func (s *Server) startBackground(what backgroundStart) error {
	// 起動の間は印のmutexを握る(#3709)。停止は始めに印を立てるのにこのmutexを取るので、
	// 起動の途中から止め始めることは無く、止める側は起動が済んでから、起動したものを
	// すべて止める。印が先に立っていれば、止める人の居ないworkerを残さないよう何も
	// 起動しない(queueのdriverも閉じた後でありうる)。
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.stopped {
		return errGenerationStopped
	}
	// 部品(バッファ、配送の健全性、statsのpublisher、構築時のworkerなど)を、queueより
	// 先に一覧の順で起動する(#3703)。queueのworkerのジョブがバッファ・健全性・連合の
	// ルールの記録へ書くので、書き手より先に、溜めたものをflushする側を動かす。
	// #3703より前は構築の途中で起動していたので、queue・chartより先という順は変わらない。
	// 部品の起動が失敗したら(panic)、queueもchartも起動せずに返す。止めるのは呼び出し元。
	if err := s.lifecycle.startAll(); err != nil {
		return err
	}
	// queue 側 (worker / autoscale / scheduler) は RoleServer では起動しない。
	// Web ノードもジョブを **enqueue** はするので queueClient は生かしたまま、
	// 消費側だけ止める (#2459)。StartBackgroundForTestはroleを見ずに起動する。
	if what.anyRole || s.role.RunsQueue() {
		if err := s.queueServer.Start(); err != nil {
			return fmt.Errorf("start queue worker: %w", err)
		}
		// workerを起動したら、続くautoscaleの起動が失敗しても止めるよう、ここで記録する
		// (#3713)。roleがqueueを回さない世代(StartBackgroundForTest)の停止は、これを見て
		// workerとschedulerを止める。
		s.queueStarted = true
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
	// 自分で flush しないと失われる。StartBackgroundForTestは起動しない。
	if what.startChart && s.chartMgmt != nil {
		if err := s.chartMgmt.Start(context.Background()); err != nil {
			slog.Warn("chart management service start failed", "err", err)
		}
	}
	return nil
}

// ServeHandler returns the handler the listener should serve for this
// process role: the full router for a node that runs the server, or the
// minimal /healthz (+ /metrics) mux for a queue-only node.
func (s *Server) ServeHandler() http.Handler {
	if !s.role.RunsServer() {
		return s.queueOnlyMux()
	}
	return s.echo
}

// Shutdown stops this server generation: streaming connections, the queue
// worker and every background service such as the chart management loop.
// It is the same as StopBackground and safe to call more than once.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.StopBackground(ctx)
}

// StopBackground stops everything StartBackground and the constructor
// started, closes the streaming connections with a 1012 close frame and
// closes this generation's Redis subscriptions. It does not close the DB
// pool or the Redis clients, which the caller owns. Only the first call does
// the work; later calls return its result.
//
// It first marks the generation stopped, after a start that is running has
// finished; StartBackground and StartBackgroundForTest called after that
// start nothing and return errGenerationStopped (#3709).
//
// The stop runs the named steps of shutdownSteps in order. When a step
// panics, the panic is logged and the later steps still run (#3711); when
// the stop of a part of the lifecycle list panics, the panic is logged and
// the rest of the list still stops (#3707). The errors of the stop are one
// error per panicked step (wrapping errShutdownStep and naming the step),
// the error of the lifecycle list (one error per panicked part, wrapping
// errLifecycleStop) and the error of echo's shutdown. It returns nil when
// there is none, the error itself when there is exactly one (so a stop
// without a panic returns echo's error as is), and otherwise their
// errors.Join in step order (see runShutdownSteps).
func (s *Server) StopBackground(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		s.shutdownErr = s.shutdown(ctx)
	})
	return s.shutdownErr
}

// streamCloseMax bounds how long stopping a generation waits for the
// streaming connections to close.
const streamCloseMax = 3 * time.Second

// streamCloseContext derives the deadline of the streaming close wait: at
// most streamCloseMax and at most a third of what is left of ctx, so the
// shutdown hooks keep most of the time.
func streamCloseContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return shutdownStepContext(ctx, streamCloseMax)
}

// queueStopMax bounds how long stopping a generation waits for the queue
// worker before it runs the shutdown hooks.
const queueStopMax = 3 * time.Second

// lateChartSaveMax is the least time given to the chart save after a queue
// worker that did not stop within the queue wait of stopQueue has finally
// stopped (see lateSaveContext).
//
// ここに来るのは、worker が min(queueStopMax, 残りの 1/3) 以内に止まらなかった
// ときで、止まるまでの待ちの上限は driver の側 (mkq は 30 秒) にある。止まった
// 時点で停止の ctx は切れていることがあり、切れた ctx では保存が DB に届かない。
// 上限が要るのは、メンテナンスの作り直しが genMu を握ったままここを通るため。
// SIGTERM では、Supervisor.Shutdown が期限で返った後にプロセスが終われば、この
// 保存も途中で切られる。
const lateChartSaveMax = 2 * time.Second

// lateSaveContext derives the context of the chart save after a late queue
// worker: ctx without its cancellation, with the later of ctx's deadline and
// now+lateChartSaveMax. ctx に時間が残っていれば、それを 2 秒で切らずに使う。
func lateSaveContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(lateChartSaveMax)
	if d, ok := ctx.Deadline(); ok && d.After(deadline) {
		deadline = d
	}
	return context.WithDeadline(context.WithoutCancel(ctx), deadline)
}

// lateBackgroundWaitMin is the least wait of the late path for the
// goroutines of the generation, even when the shutdown ctx has expired.
const lateBackgroundWaitMin = time.Second

// lateBackgroundWait is how long the late path of the shutdown (after a queue
// worker that did not stop in time) waits for the goroutines of the
// generation: a quarter of what is left of ctx like bgwork.Group.Stop, but at
// least lateBackgroundWaitMin and at most bgwork.MaxWait.
//
// 下限を置くのは、ここでは停止の期限が切れていることが多いから。止まらなかった
// worker が起動した hook は、ここで待たないと閉じた DB に当たる。
func lateBackgroundWait(ctx context.Context) time.Duration {
	limit := bgwork.MaxWait
	if deadline, ok := ctx.Deadline(); ok {
		limit = min(limit, time.Until(deadline)/4)
	}
	return max(limit, lateBackgroundWaitMin)
}

// shutdownStepContext derives the deadline of one waiting step of the
// shutdown: at most limit and at most a third of what is left of ctx.
func shutdownStepContext(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok {
		limit = min(limit, time.Until(deadline)/3)
	}
	return context.WithTimeout(ctx, limit)
}

// stopQueue stops the auto-scale controller, the scheduler and the queue
// worker in the background and waits for them at most queueStopMax (and a
// third of what is left of ctx). It returns a channel closed once they have
// stopped, and whether they stopped within the wait.
func (s *Server) stopQueue(ctx context.Context) (<-chan struct{}, bool) {
	done := make(chan struct{})
	// 止めるものが無い構成(MK_ONLY_SERVERでautoscaleも無く、queueを起動していない)は
	// 待たない。期限の切れたctxでは、下のselectが止まっているqueueを「止まらなかった」と
	// 取り違え、誤った警告と2回目のchartの保存を出しうる。
	// workerとschedulerを止めるのは、roleがqueueを回すとき(起動していない世代も
	// #3713より前と同じ)と、起動がworkerを起動したとき(#3713)。queueStartedは
	// runMuを取らずに読む(queueStartedのコメント)。
	stopWorker := s.role.RunsQueue() || s.queueStarted
	if s.autoscale == nil && !stopWorker {
		close(done)
		return done, true
	}
	go func() {
		defer close(done)
		// queueServer.Shutdown 前に controller を停止 (Resize 呼びの停止 = pool に
		// 余計な変更が走らない状態で queueServer の pool を畳める)。Shutdown ctx を
		// 伝播することで autoscale の goroutine drain にも graceful deadline が効く。
		s.autoscale.Stop(ctx)
		// RoleServerでは(StartBackgroundForTestを除き)schedulerもworkerも起動していない。
		// mkq driverはどちらのShutdownも未起動で安全だが、「起動済み」を前提にしたdriverへ
		// 差し替わっても壊れないようroleで守る(#2459)。起動を記録した世代はroleにかかわらず
		// 止める(#3713)。
		if stopWorker {
			if s.queueScheduler != nil {
				s.queueScheduler.Shutdown()
			}
			s.queueServer.Shutdown()
		}
	}()
	waitCtx, cancel := shutdownStepContext(ctx, queueStopMax)
	defer cancel()
	select {
	case <-done:
		return done, true
	case <-waitCtx.Done():
		// 止まらない worker は、キャンセルを見ない handler が掴んでいる。待ち続けると
		// 後の hook (バッファの flush や chart の保存) の時間が無くなるので先へ進む。
		// バッファは閉じた後の書き込みをその場で書くので、この worker の書き込みも
		// 残る。chart は queue が止まった後でもう一度保存する (lateChartSaveMax)。
		slog.Warn("queue worker did not stop before the shutdown hooks; continuing")
		return done, false
	}
}

// markStopped sets the stopped flag under runMu. A start that is running
// holds runMu, so markStopped waits for it to finish (#3709).
func (s *Server) markStopped() {
	s.runMu.Lock()
	s.stopped = true
	s.runMu.Unlock()
}

func (s *Server) shutdown(ctx context.Context) error {
	// 何かを止めるより先に、起動しない印を立てる(#3709)。走っている起動があれば、
	// 印を立てる前にそれが済むのを待つので、下の手順はその起動が起動したものも止める。
	// 止め終わってから立てると、その間に起動したworkerを止める人が居なくなる。
	// 段の表の外に置くので、段のpanicで飛ぶことは無い(#3711)。
	s.markStopped()
	return runShutdownSteps(ctx, s.shutdownSteps())
}

// shutdownSteps returns the steps of stopping the generation, in the order
// they run (docs/architecture.md 4.3). Each call returns a new table whose
// steps share the result of the queue step: the background step and the
// lateQueue step read whether the queue worker stopped in time.
//
// When the queue step panics, the queue is taken as not stopped in time:
// the background step does nothing, and the lateQueue step collects the
// goroutines and saves the chart again without waiting for the queue,
// since there is no channel to wait on.
//
// queueの段がpanicしたときは、workerが止まったかが分からない。止まらなかったものとして
// 扱い、箱の回収を一覧とchartの後(lateQueueの段)へ回す。止まったものとして扱うと、
// 止まっていないworkerが箱へ足した分を捨てうる。待つchannelは無い(stopQueueが
// 返していない)ので、lateQueueの段は待たない。nilのchannelを待つと永久に止まる。
func (s *Server) shutdownSteps() []shutdownStep {
	var (
		queueDone    <-chan struct{}
		queueStopped bool
	)
	return []shutdownStep{
		{name: "streaming", run: func(ctx context.Context) error {
			// streaming を最初に閉じる。接続の close handler は DB を触る
			// (lastActiveDate など) ので、DB を閉じる前に終わらせる。close frame を
			// 送るので、クライアントは 1006 ではなく 1012 を受けて繋ぎ直す (#3463)。
			//
			// **待ちには別の上限を付ける。** 待ちが ctx を使い切ると、後に続く shutdown
			// hook (chart の SaveAll や buffer の flush) の時間が 0 になる。
			if s.streamManager != nil {
				closeCtx, cancel := streamCloseContext(ctx)
				left := s.streamManager.CloseAll(closeCtx, stream.ServiceRestartCloseCode, "server restarting")
				cancel()
				if left > 0 {
					slog.Warn("streaming connections did not close before the deadline", "left", left)
				}
			}
			return nil
		}},
		{name: "queue", run: func(ctx context.Context) error {
			// **queue の worker と scheduler を、hook と chart より先に止める (#3662)。**
			// worker のジョブはバッファ (TouchBuffer など) と chart へ書くので、先に
			// それらを閉じると、止まるまでに終わったジョブの書き込みが誰にも flush
			// されずに消える。hook どうしは登録順のまま走らせる。後に登録した hook の
			// 処理が、先に登録した hook の閉じるものを使う組は無い (#3662 で全件を確認)。
			queueDone, queueStopped = s.stopQueue(ctx)
			return nil
		}},
		{name: "background", run: func(ctx context.Context) error {
			// 投げっぱなしの goroutine (投稿後の hook、サインインの記録、メールなど) を
			// 回収する (#3661)。hook と chart より前に置くのは、箱の goroutine が書く
			// バッファと chart の分を、flush と保存に含めるため。queue の後に置くのは、
			// 受信の処理 (inbox) が止まるまで hook を起動し続けるため。待ちには上限があり、
			// 止まらない goroutine は残して進む (DB を閉じた後に失敗する)。
			// queue が上限内に止まらなかったときは、止まらなかった worker がまだ箱へ
			// 足しうるので、閉じるのは lateQueue の段の <-queueDone の後にする (先に
			// 閉じると、その worker のジョブが起動する fanout・アンテナ・通知が捨てられる)。
			if queueStopped {
				s.background.Stop(ctx)
			}
			return nil
		}},
		{name: "lifecycle", run: func(ctx context.Context) error {
			// 部品の一覧 (#3699) を登録順に止める。publisher goroutine の clean 停止と、
			// graceful drain が必要な service (hashtag #727 等) の Shutdown(ctx) を
			// 兼ねる。ctx を渡すので timeout 共有が可能 (#764)。
			// 部品の停止のpanicはstopAllが回収してエラーにするので、後の段はpanicがあっても
			// 走る(#3707)。エラーは段のエラーとして返し、echoのエラーとまとめる。
			return s.lifecycle.stopAll(ctx)
		}},
		{name: "chart", run: func(ctx context.Context) error {
			if s.chartMgmt != nil {
				s.chartMgmt.Stop(ctx)
			}
			return nil
		}},
		{name: "lateQueue", run: func(ctx context.Context) error {
			// 上限内に止まらなかった worker は、driver を閉じる前にここで待つ (以前と
			// 同じく、待ちの上限は driver の側にある)。止まるまでに chart へ足された分を
			// 保存する。
			if queueStopped {
				return nil
			}
			// queueの段がpanicしたときはchannelが無いので待たない(shutdownStepsのGoDoc)。
			if queueDone != nil {
				<-queueDone
			}
			// 箱を先に回収してから保存する。ここでは停止の期限が切れていることが多い
			// ので、箱の待ちは ctx の取り消しに縛られない上限 (lateBackgroundWait) で
			// 取る。保存の期限 (lateSaveContext) は待った後に取るので、保存の時間は
			// 箱の待ちに削られない (#3661 のレビュー2周目 L-3)。
			s.background.StopWithin(lateBackgroundWait(ctx))
			saveCtx, cancel := lateSaveContext(ctx)
			if s.chartMgmt != nil {
				_ = s.chartMgmt.SaveAll(saveCtx)
			}
			cancel()
			return nil
		}},
		{name: "queueDriver", run: func(context.Context) error {
			// queueClient.Close を直接呼ばないこと。queueDriver.Close が
			// Client / Inspector を含むサブコンポーネントの Close を一括処理する。
			// mkq driver は Client.Close が no-op で接続は driver 本体が持つので、
			// 閉じ口は driver.Close の一本に統一する。
			if s.queueDriver != nil {
				if err := s.queueDriver.Close(); err != nil {
					slog.Warn("queue driver close failed", "err", err)
				}
			}
			return nil
		}},
		{name: "pubsub", run: func(context.Context) error {
			// この世代の Redis 購読を閉じる。internal: の購読は解除の関数を捨てて
			// いるので、個別ではなく pubsub ごと閉じる (#3463)。Publish はクライアントを
			// 直接使うので、閉じた後も (古い世代の残りの処理が) 送ることはできる。
			s.closePubSubs()
			return nil
		}},
		{name: "echo", run: func(ctx context.Context) error {
			// listener は Supervisor が持つ。echo は listen していないので、Shutdown は
			// echo が自分で張った server を持っていた場合 (テストなど) にだけ効く。
			// 戻り値は段のエラーとして返す。ほかに失敗した段が無ければ、runShutdownStepsは
			// これをそのまま返す(#3707より前と同じ値)。
			if s.echo != nil {
				return s.echo.Shutdown(ctx)
			}
			return nil
		}},
	}
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
