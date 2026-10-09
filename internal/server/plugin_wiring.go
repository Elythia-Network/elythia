package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/pluginstore"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/plugin"
)

// pluginRoutePrefix namespaces every plugin endpoint.
//
// Misskey 本体のエンドポイント空間とは**必ず分ける**。upstream が将来同名の
// エンドポイントを追加したときに衝突すると、API 互換 (本プロジェクトの最優先
// 方針) が壊れるため。
const pluginRoutePrefix = "/plugin/"

var errPluginEffectivePolicySetup = errors.New("plugin effective policy provider setup failed")

// setupPlugins prepares every enabled plugin before activating any callbacks.
//
// ロールに応じて登録先を出し分ける (#2459)。HTTP を担わないプロセスがルートを
// 登録したり、キューを担わないプロセスがジョブを登録したりしないようにする。
// 全pluginのstorage/migration/effective policyを先に確定してからRoutes/Jobsを
// 呼ぶ。後続providerの失敗時に先行callbackがContext.Goで処理を開始し、constructor
// cleanupでcloseされたstorageへ触り続ける経路を作らないため。
// setup中のContext.Goも共通gateへ保留し、constructor完成時にnewServerがreleaseする。
//
// 一覧と storage の生成を引数で受けるのは、グローバルレジストリや実 DB を直接
// 触るとテストから差し替えられないため。plugin.Registered() を呼ぶのも、実
// 接続を開くのも呼び出し側の責務。
func (s *Server) setupPlugins(api *echo.Group, plugins []plugin.Definition, openStorage pluginStorageOpener) error {
	if len(plugins) == 0 {
		return nil
	}
	goGate := &pluginGoGate{}
	s.pluginGoGate = goGate
	setupSucceeded := false
	defer func() {
		if !setupSucceeded {
			goGate.discard()
		}
	}()
	type preparedPlugin struct {
		def  plugin.Definition
		ctx  *pluginContext
		peer *pluginPeer
	}
	prepared := make([]preparedPlugin, 0, len(plugins))
	for _, def := range plugins {
		settings := s.config.Plugins[def.Name]

		// **無効なプラグインは登録しない。** ビルドには含まれたままなので、
		// 問題が起きたプラグインを再ビルドせず再起動だけで止められる (#2482)。
		// 障害対応中に再ビルドと再デプロイを要求する形では使い物にならない。
		if !pluginEnabled(settings) {
			slog.Info("plugin disabled", "name", def.Name, "version", def.Version)
			continue
		}

		pctx := &pluginContext{
			name:    def.Name,
			goGate:  goGate,
			goStart: s.pluginGoStarter,
			logger: slog.With(
				slog.String("plugin", def.Name),
				slog.String("pluginVersion", def.Version),
			),
			api:    &pluginAPI{echo: s.echo, userRepo: s.userRepo, host: requestHostFor(s.config.URL)},
			config: pluginConfig(settings),
			// **名前はここで固定する (#3468)。** 他のプラグインのアカウントを
			// 操作できないようにする。
			accounts: newPluginAccounts(def.Name, s.pluginAccounts),
			// **共通の outbound 設定を通す (#3037)。** プラグインが自分で
			// `&http.Client{}` を作ると SSRF ガードも運営者の proxy 設定も
			// 効かず、そのプラグインだけがサーバーの素の IP で外へ出る。
			httpClient: s.outboundClient(pluginHTTPTimeout),
			// **プラグイン名で絞った口だけを渡す (#3470)。** 名前を受け取る
			// 口を渡すと、他のプラグインの値を名指しで読めてしまう。
			secrets: s.pluginSecretService().ForPlugin(def.Name),
		}

		// ストレージはロールに関係なく渡す。Routes からも Jobs からも使うため。
		//
		// **起動時に開く。** 遅延させると、接続できない設定でも起動してしまい、
		// 最初のリクエストまで問題が表面化しない。
		storage, err := openStorage(def.Name)
		if err != nil {
			return fmt.Errorf("plugin %q: %w", def.Name, err)
		}
		pctx.storage = storage

		// **ロールに関係なく、callback より先に適用する。** Routes / Jobs の
		// 中で呼ばせると、ロール分割したときに片方でしか走らず、queue 専用
		// プロセスがテーブルの無い schema でジョブを回すことになる。
		if len(def.Migrations) > 0 && storage != nil {
			if err := storage.Migrate(context.Background(), def.Migrations); err != nil {
				return fmt.Errorf("plugin %q: migration に失敗しました: %w", def.Name, err)
			}
		}

		// peer 経路。**宣言していないプラグインにも非 nil を渡す** (呼ぶと
		// エラーになる実装)。nil を返すと nil チェック漏れが panic になる。
		peerLimit, _ := peerBodyLimit(def, settings)
		// **ジョブを宣言していないプラグインには積ませない。** 専用キューを
		// 作らない (plugin_queue_names.go) ので、積めても誰も処理しない。
		// 呼んだ時点でエラーにして、黙って溜まる形にしない。
		pctx.queue = &pluginQueue{name: def.Name, client: s.queueClient, hasJobs: def.Jobs != nil}

		peer := &pluginPeer{
			name:    def.Name,
			peered:  def.Peered,
			deps:    s.peerDeps,
			logger:  pctx.logger,
			maxBody: peerLimit,
		}
		pctx.peer = peer

		// **ロールに関係なく呼ぶ (#2819)。** 送信の再送はキューに載るので、
		// 実際に POST するのは queue ロールのプロセスになる。OnReply を
		// Routes の中で登録していると、ロールを分割した構成で応答が届かない。
		if def.Peer != nil {
			if err := def.Peer(pctx, peer); err != nil {
				return fmt.Errorf("plugin %q: peer の登録に失敗しました: %w", def.Name, err)
			}
		}

		if def.EffectivePolicies != nil {
			if s.roleService == nil {
				return errPluginEffectivePolicySetup
			}
			reg, err := func() (reg plugin.EffectivePolicyRegistration, err error) {
				defer func() {
					if recover() != nil {
						err = errPluginEffectivePolicySetup
					}
				}()
				return def.EffectivePolicies(pctx, &roleEffectivePolicyInvalidator{service: s.roleService})
			}()
			if err != nil {
				return errPluginEffectivePolicySetup
			}
			if err := s.roleService.RegisterEffectivePolicyProvider(def.Name, reg); err != nil {
				return errPluginEffectivePolicySetup
			}
		}
		prepared = append(prepared, preparedPlugin{def: def, ctx: pctx, peer: peer})
	}

	secretPlugins := make([]plugin.Definition, 0, len(prepared))
	for _, p := range prepared {
		secretPlugins = append(secretPlugins, p.def)
	}
	warnPluginSecretsWithoutKey(secretPlugins, s.pluginSecretService().Available())

	for _, p := range prepared {
		def := p.def
		pctx := p.ctx
		peer := p.peer
		// **peer の受け口を Routes の有無に依存させない (#2822)。** nodeinfo の
		// 広告は Routes を見ないので、`Peered: true` + `Routes: nil` は宣言だけ
		// して受け取れないインスタンスになる。相手からは catchall の 200 + {} が
		// 返り、「プラグインが空の応答を返した」と区別が付かない。
		//
		// **group を作るのは 1 箇所に保つ。** `api.Group(` の数は
		// `TestAPICompatDoc_MatchesRouter` が固定している (サブグループ配下の
		// 登録は生成物との照合から見えないため)。
		needsRoutes := def.Routes != nil
		needsPeer := def.Peered && s.peerDeps != nil
		// 秘密の値の入力口 (#3470) は、宣言したプラグインにだけ張る。
		needsSecrets := len(def.Secrets) > 0 && s.pluginSecrets != nil
		if s.role.RunsServer() && (needsRoutes || needsPeer || needsSecrets) {
			// **第三者アプリのトークンを入れない (#3037)。** プラグインの
			// ルートには upstream の `kind` にあたる宣言が無く、本体は
			// `RequireScope` を配線できない。gate を置かないと
			// `read:account` だけの token で到達でき、プラグインが
			// `req.IsModerator()` で守っていても scope は効かない
			// (`AsUser` は対象利用者の native token を載せるので、そこから
			// 任意の endpoint へ抜けられる)。upstream が
			// 「`kind` 無し + 資格情報要」で app token を一律拒否するのと
			// 同じ側に倒す。
			group := api.Group(pluginRoutePrefix+def.Name, middleware.RejectAppToken())
			if needsRoutes {
				r := &pluginRouter{group: group, roles: s.pluginRoles}
				if err := def.Routes(pctx, r); err != nil {
					return fmt.Errorf("plugin %q: ルート登録に失敗しました: %w", def.Name, err)
				}
				if err := r.err; err != nil {
					return fmt.Errorf("plugin %q: ルート登録に失敗しました: %w", def.Name, err)
				}
			}
			// 予約パスは **プラグインの登録より後**に張る。先に張ると
			// プラグインが同じパスを登録できてしまい、受け口を奪える。
			if needsPeer {
				group.POST(peerPath, peer.echoHandler())
			}
			if needsSecrets {
				s.registerPluginSecretRoutes(group, def)
			}
		}
		// peer の送信を処理するのは queue ロール (#2819)。**Peered だけで
		// 登録する** — Jobs を持たないプラグインでも送信はするため。
		if def.Peered && s.peerDeps != nil && s.role.RunsQueue() {
			if s.queueServer == nil {
				return fmt.Errorf("plugin %q: peer の処理を登録できません (queue server が未配線です)", def.Name)
			}
			s.queueServer.Handle(queue.PluginPeerTaskType(def.Name), peer.peerJobHandler())
		}
		if def.Jobs != nil && s.role.RunsQueue() {
			j := &pluginJobs{name: def.Name, server: s.queueServer, scheduler: s.queueScheduler}
			if err := def.Jobs(pctx, j); err != nil {
				return fmt.Errorf("plugin %q: ジョブ登録に失敗しました: %w", def.Name, err)
			}
			if err := j.err; err != nil {
				return fmt.Errorf("plugin %q: ジョブ登録に失敗しました: %w", def.Name, err)
			}
		}
		// 通知の handler (#3469) もジョブと同じく queue ロールでだけ登録する。
		// 積むのはどのロールでも行う (router の pluginnotify.Dispatcher)。
		if def.Notifications != nil && s.role.RunsQueue() {
			if err := s.registerPluginNotifications(def, pctx); err != nil {
				return fmt.Errorf("plugin %q: 通知の handler の登録に失敗しました: %w", def.Name, err)
			}
		}

		// schema も出す。プラグインのデータがどこにあるかを、運営者が
		// ログだけで辿れるようにする (消したあとの残存データの説明に要る)。
		schema, _ := pluginstore.SchemaName(def.Name)
		// **登録漏れを知らせる (#2819)。** peer の登録を Routes の中に置いたまま
		// ロールを分割すると、そのロールでは Handle も OnReply も無いまま動く。
		// 症状は「応答が来ない」「相手に 501」で、どちらもこちら側には出ない。
		if def.Peered {
			if s.role.RunsServer() && peer.handlerFn() == nil {
				// **このロールでは Routes も既に走っている**ので、ここに
				// 来るのは「どこにも Handle を登録していない」場合だけ。
				// 相手には 501 が返る。
				slog.Warn("plugin peer: 受信ハンドラが登録されていません",
					"name", def.Name, "role", s.role,
					"hint", "Peer.Handle をどこかで呼ぶこと (推奨は Definition.Peer)")
			}
			if s.role.RunsQueue() && peer.replyFn() == nil {
				// 送信の POST は queue ロールで走るので、OnReply を Routes の
				// 中で登録しているとここで無い状態になる。応答が届かない。
				slog.Warn("plugin peer: このロールに応答ハンドラが登録されていません",
					"name", def.Name, "role", s.role,
					"hint", "Peer.OnReply を Definition.Peer の中で呼ぶこと (Routes の中だと queue ロールで登録されない)")
			}
		}

		slog.Info("plugin loaded",
			"name", def.Name, "version", def.Version,
			"routes", def.Routes != nil && s.role.RunsServer(),
			"peer", needsPeer,
			"secrets", len(def.Secrets),
			"jobs", def.Jobs != nil && s.role.RunsQueue(),
			"notifications", def.Notifications != nil && s.role.RunsQueue(),
			"migrations", len(def.Migrations),
			"schema", schema)
	}
	setupSucceeded = true
	return nil
}

type roleEffectivePolicyInvalidator struct {
	service *corerole.Service
}

func (i *roleEffectivePolicyInvalidator) InvalidateUser(ctx context.Context, userID string) error {
	return i.service.InvalidateUser(ctx, userID)
}

func (i *roleEffectivePolicyInvalidator) InvalidateRole(ctx context.Context, roleID string) error {
	return i.service.InvalidateRolePolicies(ctx, roleID)
}

// serverPluginInfos builds the admin-facing snapshot of compiled-in plugins
// (admin/server-plugins, #2497).
//
// Routes / Jobs / EffectivePolicies は**宣言**を写す (このプロセスで配線されたか
// ではない)。ロール分割 (#2459) でどのプロセスの管理画面から見ても、
// プラグインの構成としては同じものが出るようにする。
func serverPluginInfos(plugins []plugin.Definition, settings map[string]map[string]any) []apiadmin.ServerPluginInfo {
	infos := make([]apiadmin.ServerPluginInfo, 0, len(plugins))
	for _, def := range plugins {
		s := settings[def.Name]

		// **設定はキー名だけを出す。** どのキーが秘密かは判別できないので、
		// `elythia config-dump` と同じく値は既定で全部マスクする方針に合わせる。
		keys := make([]string, 0, len(s))
		for k := range s {
			if isReservedPluginKey(k) {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)

		schema, _ := pluginstore.SchemaName(def.Name)
		infos = append(infos, apiadmin.ServerPluginInfo{
			Name:              def.Name,
			Version:           def.Version,
			APIVersion:        def.APIVersion,
			Enabled:           pluginEnabled(s),
			Routes:            def.Routes != nil,
			Jobs:              def.Jobs != nil,
			EffectivePolicies: def.EffectivePolicies != nil,
			Migrations:        len(def.Migrations),
			Schema:            schema,
			ConfigKeys:        keys,
			Secrets:           secretNames(def.Secrets),
		})
	}
	return infos
}

// pluginOrphanSchemaLister returns the request-time orphan schema check for
// admin/server-plugins. 起動時 WARN (warnOrphanPluginData) と同じ判定だが、
// リクエスト時に問い合わせる — 起動後に DROP SCHEMA した結果も反映される。
func (s *Server) pluginOrphanSchemaLister(plugins []plugin.Definition) apiadmin.PluginOrphanSchemaLister {
	known := make([]string, 0, len(plugins))
	for _, d := range plugins {
		known = append(known, d.Name)
	}
	return func(ctx context.Context) ([]string, error) {
		// ListSchemas は呼び出しごとに接続を張って捨てる。モデレーター以上の
		// 管理画面からしか呼ばれない低頻度の診断なので、既存プールへの相乗りで
		// pluginstore の口を増やすより使い捨てで十分と判断している。
		present, err := pluginstore.ListSchemas(ctx, s.config.DSN())
		if err != nil {
			return nil, err
		}
		return pluginstore.OrphanSchemas(present, known), nil
	}
}

// warnOrphanPluginData reports plugin schemas with no matching built-in plugin.
//
// **プラグインを消してもデータは自動で消さない** (削除した行は復元できないので、
// 一時的に外しただけの運用でデータが飛ぶ方が損害が大きい)。代わりに残っている
// ことを起動時に知らせる。黙って残ると、運営者は自分の DB に何があるか把握
// できなくなる。
//
// 検査に失敗しても起動は止めない。これは診断であって、動作の前提ではない。
func (s *Server) warnOrphanPluginData(ctx context.Context, plugins []plugin.Definition) {
	present, err := pluginstore.ListSchemas(ctx, s.config.DSN())
	if err != nil {
		slog.Warn("プラグインの残存データを確認できませんでした", "err", err)
		return
	}
	known := make([]string, 0, len(plugins))
	for _, d := range plugins {
		known = append(known, d.Name)
	}
	for _, schema := range pluginstore.OrphanSchemas(present, known) {
		slog.Warn("使われていないプラグインのデータが残っています",
			"schema", schema,
			"対処", "不要なら DROP SCHEMA \""+schema+"\" CASCADE で削除してください (自動では消しません)")
	}
}

// requestHostFor extracts the Host header value to use for in-process API
// calls. 絶対 URL を組み立てる handler があるので、空のままにしない。
func requestHostFor(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "localhost"
	}
	return u.Host
}

// --- Context ---

type pluginContext struct {
	name       string
	logger     *slog.Logger
	api        plugin.API
	storage    plugin.Storage
	config     plugin.Config
	secrets    plugin.Secrets
	peer       plugin.Peer
	queue      plugin.Queue
	accounts   plugin.Accounts
	httpClient *http.Client
	goGate     *pluginGoGate
	goStart    func(func())
}

func (c *pluginContext) Name() string            { return c.name }
func (c *pluginContext) Logger() *slog.Logger    { return c.logger }
func (c *pluginContext) API() plugin.API         { return c.api }
func (c *pluginContext) Storage() plugin.Storage { return c.storage }
func (c *pluginContext) Config() plugin.Config   { return c.config }

// Accounts は **常に非 nil** (Peer / Queue と同じ理由)。テストなどで
// pluginContext を直接組んだときも、呼ぶとエラーを返す実装に倒す。
func (c *pluginContext) Accounts() plugin.Accounts {
	if c.accounts == nil {
		return newPluginAccounts(c.name, nil)
	}
	return c.accounts
}

// Secrets は **常に非 nil** を返す (Peer / Queue と同じ理由)。未配線なら
// 鍵の無い service の口を渡すので、呼ぶと ErrSecretsUnavailable になる。
func (c *pluginContext) Secrets() plugin.Secrets {
	if c.secrets == nil {
		return unconfiguredPluginSecrets().ForPlugin(c.name)
	}
	return c.secrets
}

// HTTP returns the outbound client shared with the rest of mk-go.
//
// **未配線でも nil を返さない (#3037)。** nil を返すと、プラグイン側の
// `ctx.HTTP().Do(...)` がそのまま nil 参照 panic になる。素の client は
// SSRF ガードも proxy 設定も持たないので、代わりに**必ず失敗する**
// transport を返す — 「黙って無防備に外へ出る」より「動かない」ほうが
// 気付ける。
func (c *pluginContext) HTTP() *http.Client {
	if c.httpClient == nil {
		return &http.Client{Transport: unwiredPluginTransport{}}
	}
	return c.httpClient
}

// unwiredPluginTransport fails every request with an explanatory error.
type unwiredPluginTransport struct{}

func (unwiredPluginTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("plugin: HTTP client が未配線です (mk-go の不具合です)")
}

// Peer は **常に非 nil** を返す。Peered を立てていないプラグインには、
// 呼ぶとエラーになる実装を渡す — nil を返すと、プラグイン側の nil チェック
// 漏れがそのまま panic になる。
func (c *pluginContext) Peer() plugin.Peer { return c.peer }

// Queue も **常に非 nil**。queue client が未配線のときは呼ぶとエラーになる
// 実装を渡す (Peer と同じ理由)。
func (c *pluginContext) Queue() plugin.Queue { return c.queue }

// pluginHTTPTimeout bounds one request made through ctx.HTTP().
//
// **プラグインの主な用途はリモートからの取り寄せ**なので、AP の配送
// (10 秒) より長めに取る。足りないものは
// `http.NewRequestWithContext` で per-request に伸ばせる。
const pluginHTTPTimeout = 30 * time.Second

// pluginRetryBackoffBase is the exponential backoff base applied whenever a
// plugin asks for retries.
//
// **プラグインの主な用途はリモートからの取り寄せ** (#2818 の動機) なので、
// 相手が落ちているときに連打しない間隔にする。deliver / inbox の
// httpRelatedBackoff (1 分起点、8 時間上限) ほど長くはしない — あちらは連合の
// 配送で、こちらは利用者の画面に出る取得。
const pluginRetryBackoffBase = 10 * time.Second

// pluginQueue implements plugin.Queue for one plugin.
type pluginQueue struct {
	name    string
	client  *queue.Client
	hasJobs bool
}

// Enqueue adds a job to this plugin's queue.
//
// **ロールを問わず使える。** worker を持たない web 専用プロセスからでも積める
// (処理するのは queue ロールのプロセス)。ハンドラの登録は Jobs callback 側で、
// そちらは RunsQueue() のときしか走らない。
func (q *pluginQueue) Enqueue(ctx context.Context, name string, payload any, opts ...plugin.EnqueueOption) error {
	if !q.hasJobs {
		return fmt.Errorf("plugin queue: Definition.Jobs を宣言していないため積めません")
	}
	if q.client == nil {
		return fmt.Errorf("plugin queue: queue client が未配線です")
	}
	if name == "" {
		return fmt.Errorf("plugin queue: ジョブ名が空です")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("plugin queue: payload を JSON 化できません: %w", err)
	}

	var o plugin.EnqueueOptions
	for _, fn := range opts {
		fn(&o)
	}
	var dopts []driver.EnqueueOption
	if o.Delay > 0 {
		dopts = append(dopts, driver.WithProcessIn(o.Delay))
	}
	if o.MaxAttempts > 1 {
		// driver の MaxRetry は「初回を除く回数」。
		dopts = append(dopts, driver.WithMaxRetry(o.MaxAttempts-1))
		// **backoff を必ず付ける。** 未設定の mkq は遅延 0 で再投入するので、
		// 落ちている取得先を連打したうえ delayed bucket にも滞在しない
		// (driver/option.go の BackoffType が名指ししている罠)。公開面に
		// ノブが無い以上、プラグイン側では回避できない。
		dopts = append(dopts, driver.WithBackoff(driver.BackoffExponential, pluginRetryBackoffBase))
	}
	if o.DedupTTL > 0 {
		dopts = append(dopts, driver.WithUnique(o.DedupTTL))
	}
	return q.client.EnqueuePlugin(ctx, q.name, name, body, dopts...)
}

type pluginGoGate struct {
	mu      sync.Mutex
	state   uint8
	pending []queuedPluginGo
}

type queuedPluginGo struct {
	ctx *pluginContext
	fn  func()
}

const (
	pluginGoPending uint8 = iota
	pluginGoOpen
	pluginGoDiscarded
)

func (g *pluginGoGate) enqueue(ctx *pluginContext, fn func()) {
	g.mu.Lock()
	switch g.state {
	case pluginGoPending:
		g.pending = append(g.pending, queuedPluginGo{ctx: ctx, fn: fn})
		g.mu.Unlock()
		return
	case pluginGoDiscarded:
		g.mu.Unlock()
		return
	default:
		g.mu.Unlock()
		ctx.startGo(fn)
	}
}

func (g *pluginGoGate) open() {
	g.mu.Lock()
	if g.state != pluginGoPending {
		g.mu.Unlock()
		return
	}
	g.state = pluginGoOpen
	pending := g.pending
	g.pending = nil
	g.mu.Unlock()

	for _, queued := range pending {
		queued.ctx.startGo(queued.fn)
	}
}

func (g *pluginGoGate) discard() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != pluginGoPending {
		return
	}
	g.state = pluginGoDiscarded
	g.pending = nil
}

// --- Config ---

// enabledKey is the one setting mk-go interprets itself.
const enabledKey = "enabled"

// pluginEnabled reports whether the plugin should be registered.
//
// **既定は有効。** ビルドに含めた時点で運営者は意図してそのプラグインを選んで
// いるので、動かすのに追加の設定を要求しない。`enabled: false` を書いたときだけ
// 止める。
//
// bool 以外が書かれていた場合も有効として扱う。設定の書き間違いで機能が黙って
// 消える方が、無効化が効かないより厄介なため。
func pluginEnabled(settings map[string]any) bool {
	v, ok := pluginSetting(settings, enabledKey)
	if !ok {
		return true
	}
	b, ok := v.(bool)
	if !ok {
		return true
	}
	return b
}

// pluginSetting looks a reserved key up without caring about case.
//
// **viper が小文字化する。** 設定ファイルに camelCase で書いた `peerMaxBody` は
// `peermaxbody` で届くので、完全一致で引くと予約キーが素通りする (実測)。
// `enabled` が動いていたのは元から小文字だったからで、規則ではない。
func pluginSetting(settings map[string]any, key string) (any, bool) {
	if v, ok := settings[key]; ok {
		return v, true
	}
	for k, v := range settings {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return nil, false
}

// isReservedPluginKey reports whether k is consumed by mk-go itself.
func isReservedPluginKey(k string) bool {
	return strings.EqualFold(k, enabledKey) || strings.EqualFold(k, peerMaxBodyKey)
}

// pluginConfig wraps the plugin's settings, minus the reserved keys.
func pluginConfig(settings map[string]any) plugin.Config {
	rest := make(map[string]any, len(settings))
	for k, v := range settings {
		if isReservedPluginKey(k) {
			continue
		}
		rest[k] = v
	}
	return &pluginConfigValue{settings: rest}
}

type pluginConfigValue struct {
	settings map[string]any
}

// Unmarshal decodes the settings into v.
//
// **JSON を経由する。** YAML から読んだ値は map[string]any になっており、
// 構造体へ入れるには変換が要る。encoding/json を使えば `json` タグという
// Go では馴染みのある方法で書けて、mapstructure のような追加の作法を
// プラグイン作者に要求しない。
func (c *pluginConfigValue) Unmarshal(v any) error {
	if len(c.settings) == 0 {
		// 何も書かれていなければ v を変更しない。呼び出し側が入れた既定値が
		// 残るようにする。
		return nil
	}
	raw, err := json.Marshal(c.settings)
	if err != nil {
		return fmt.Errorf("plugin: 設定を変換できません: %w", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("plugin: 設定を読み込めません: %w", err)
	}
	return nil
}

// --- Storage ---

// pluginStorageOpener creates the storage handle for one plugin.
type pluginStorageOpener func(name string) (plugin.Storage, error)

// dbBackedStorage returns the production opener. 開いた pool は Shutdown で
// 閉じられるよう hook に登録する。
func (s *Server) dbBackedStorage() pluginStorageOpener {
	return func(name string) (plugin.Storage, error) {
		store, err := pluginstore.Open(s.config.DSN(), name, 0)
		if err != nil {
			return nil, err
		}
		s.registerShutdownHook(func(context.Context) { _ = store.Close() })
		return &pluginStorage{store: store}, nil
	}
}

// pluginStorage adapts pluginstore.Store to the public interface.
//
// 公開面は `plugin.Migration` を受けるが内部は `pluginstore.Migration` を使う。
// 同じ形でも型を分けるのは、内部側に項目を足したくなったときに公開面を巻き込ま
// ないため。
type pluginStorage struct {
	store *pluginstore.Store
}

func (s *pluginStorage) DB() *sql.DB { return s.store.DB() }

func (s *pluginStorage) Migrate(ctx context.Context, migrations []plugin.Migration) error {
	conv := make([]pluginstore.Migration, len(migrations))
	for i, m := range migrations {
		conv[i] = pluginstore.Migration{Version: m.Version, SQL: m.SQL}
	}
	return s.store.Migrate(ctx, conv)
}

// Go runs fn with panic recovery.
//
// **プラグインが素の `go` を書くとプロセスごと落ちる。** Go は他 goroutine の
// panic を回収できず、echo の Recover middleware では止められない。
func (c *pluginContext) Go(fn func()) {
	if c.goGate != nil {
		c.goGate.enqueue(c, fn)
		return
	}
	c.startGo(fn)
}

func (c *pluginContext) startGo(fn func()) {
	run := func() {
		defer func() {
			if r := recover(); r != nil {
				c.logger.Error("plugin の goroutine が panic しました", "panic", r)
			}
		}()
		fn()
	}
	if c.goStart != nil {
		c.goStart(run)
		return
	}
	go run()
}

// --- Router ---

type pluginRouter struct {
	group *echo.Group
	roles middleware.RoleChecker
	// err holds the first registration failure (pluginJobs と同じ方針)。
	err error
}

func (r *pluginRouter) GET(path string, h plugin.Handler) {
	if reservedPluginPath(path) {
		r.err = fmt.Errorf("パス %q は mk-go の予約です (_ で始まるパスは使えません)", path)
		return
	}
	r.group.GET(path, wrapPluginHandler(h, r.roles))
}

func (r *pluginRouter) POST(path string, h plugin.Handler) {
	if reservedPluginPath(path) {
		r.err = fmt.Errorf("パス %q は mk-go の予約です (_ で始まるパスは使えません)", path)
		return
	}
	r.group.POST(path, wrapPluginHandler(h, r.roles))
}

// reservedPluginPath reports whether mk-go owns this path.
//
// **プラグインに受け口を奪わせない。** peer の受信 (#2537) のように本体が
// 張るエンドポイントと同名を登録できると、署名検証を通らない経路で同じ URL を
// 提供できてしまう。`_` 始まりをまとめて予約する。
func reservedPluginPath(path string) bool {
	return strings.HasPrefix(path, "/_")
}

// wrapPluginHandler adapts a plugin handler to echo.
//
// エラーはhost-ownedのStatusErrorまたはcoded status errorのときだけメッセージを
// 返す。素のerrorを返すとプラグインの内部事情 (DBのエラー文字列など) がそのまま
// 外に出るため、ログにだけ残して汎用メッセージを返す。
func wrapPluginHandler(h plugin.Handler, roles middleware.RoleChecker) echo.HandlerFunc {
	return func(c echo.Context) error {
		res, err := h(&pluginRequest{c: c, roles: roles})
		if err != nil {
			se, code := plugin.ExtractStatusError(err)
			if se != nil {
				errorBody := map[string]any{"message": se.Message}
				if code != "" {
					errorBody["code"] = code
				}
				return c.JSON(se.Status, map[string]any{"error": errorBody})
			}
			slog.Error("plugin handler がエラーを返しました", "path", c.Path(), "err", err)
			return c.JSON(http.StatusInternalServerError, map[string]any{
				"error": map[string]any{"message": "Internal error."},
			})
		}
		if res == nil {
			return c.NoContent(http.StatusNoContent)
		}
		if b, ok := res.(plugin.Blob); ok {
			return writePluginBlob(c, b)
		}
		return c.JSON(http.StatusOK, res)
	}
}

// writePluginBlob writes a raw plugin response.
//
// **`nosniff` だけでは足りない (#3037)。** あれはブラウザの MIME 推測を止める
// だけで、Content-Type が**実際に** `text/html` や `image/svg+xml` のときには
// 何も止めない。プラグインが取得元の Content-Type をそのまま流すと
// (`plugin.Blob` の doc が「主な用途は画像のプロキシ」と書いている用途その
// もの)、**同一オリジンの XSS** になる。Misskey のフロントは `account` を
// localStorage に置くので、これはアカウント乗っ取りと同じ。
//
// **mk-go は自分のファイル配信 (`filesHandler`) で同じ脅威を 3 重に塞いで
// いる**のに、こちらはそのどれも持っていなかった。同じ 3 つを付ける:
//
//   - `BrowserSafeContentType` で allowlist 外の型を
//     `application/octet-stream` に矯正する (upstream
//     `FileServerUtils.getSafeContentType` と同じ規則)
//   - `Content-Security-Policy` で script / object の実行を封じる
//     (値は `filesHandler` と同じ)
//   - `Content-Disposition: inline`
//
// **allowlist は image / audio / video だけ。** JSON を返したいプラグインは
// `Blob` ではなく素の値を返せばよい (本体が JSON 化する)。`text/plain` 等は
// octet-stream に落ちるので、ブラウザでは表示ではなくダウンロードになる。
// denylist にしないのは、危険な型を数え上げる形が必ず漏れるため。
func writePluginBlob(c echo.Context, b plugin.Blob) error {
	ct := b.ContentType
	if ct == "" {
		ct = echo.MIMEOctetStream
	}
	ct = coredrive.BrowserSafeContentType(ct)
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	c.Response().Header().Set("Content-Security-Policy",
		"default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'")
	c.Response().Header().Set("Content-Disposition", "inline")
	if b.CacheControl != "" {
		// /api 配下には既定の Cache-Control が付くので上書きする。
		c.Response().Header().Set("Cache-Control", b.CacheControl)
	}
	return c.Blob(http.StatusOK, ct, b.Body)
}

// --- Request ---

type pluginRequest struct {
	c echo.Context
	// roles は権限判定に使う。未配線なら常に false を返す (画面は出ても
	// API は通らない、という安全側に倒す)。
	roles middleware.RoleChecker
}

func (r *pluginRequest) Context() context.Context { return r.c.Request().Context() }
func (r *pluginRequest) Bind(v any) error         { return json.NewDecoder(r.c.Request().Body).Decode(v) }
func (r *pluginRequest) Param(name string) string { return r.c.Param(name) }
func (r *pluginRequest) Query(name string) string { return r.c.QueryParam(name) }

// UserID returns the authenticated user's ID, or "" when unauthenticated.
// auth.Authenticate() はグローバル middleware なので、プラグインのルートでも
// 解決済みになっている。
func (r *pluginRequest) UserID() string {
	if u := middleware.GetUser(r.c); u != nil {
		return u.ID
	}
	return ""
}

// IsModerator reports whether the caller is a moderator.
//
// **未配線なら false。** 判定できないときに true を返すと、権限の穴が
// 「動いているように見える」形で残る。
func (r *pluginRequest) IsModerator() bool {
	id := r.UserID()
	if id == "" || r.roles == nil {
		return false
	}
	return r.roles.IsModerator(id)
}

// IsAdministrator reports whether the caller is an administrator.
func (r *pluginRequest) IsAdministrator() bool {
	id := r.UserID()
	if id == "" || r.roles == nil {
		return false
	}
	return r.roles.IsAdministrator(id)
}

// --- API ---

type pluginAPI struct {
	echo     *echo.Echo
	userRepo repository.UserRepository
	host     string
}

func (a *pluginAPI) Anonymous() plugin.Caller {
	return &pluginCaller{api: a}
}

func (a *pluginAPI) AsUser(userID string) plugin.Caller {
	return &pluginCaller{api: a, userID: userID}
}

type pluginCaller struct {
	api    *pluginAPI
	userID string
}

// Call dispatches an mk-go REST endpoint in-process.
//
// **loopback HTTP ではなく echo に直接 ServeHTTP する。** ネットワークを介さない
// のでソケットも TLS も要らない一方、middleware chain は本物のリクエストと
// 完全に同じものを通る。認証・可視性・レート制限を別実装で再現する必要が無く、
// mk-go 側を変えても自動的に追従する。
//
// レート制限も同じように適用される。プラグインが高頻度で呼ぶと、その利用者の
// 制限に掛かる。**ただし `Anonymous()` (= 利用者を伴わない呼び出し) は別** —
// プロセス内呼び出しの印が付いているので IP バケットに落ちず、実質的に
// 無制限になる。プラグイン側で自前の上限を持つこと。
func (c *pluginCaller) Call(ctx context.Context, endpoint string, params any) (json.RawMessage, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	body := []byte("{}")
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("plugin: %s のパラメータを JSON 化できません: %w", endpoint, err)
		}
		// nil の map / pointer は `null` になる。body が object でないと
		// endpoint が INVALID_PARAM (#/type) で弾くようになったので (#3330)、
		// 「パラメータ無し」として {} に寄せ、以前通っていた呼び出しを壊さない。
		if string(b) != "null" {
			body = b
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/api/"+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("plugin: %s のリクエストを組み立てられません: %w", endpoint, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Host = c.api.host
	// RemoteAddr を空にすると、IP を見る middleware が解釈に困る。
	req.RemoteAddr = "127.0.0.1:0"
	// **ただしこの IP は実在しない。** そのままだと利用者の `user_ip` に
	// `127.0.0.1` が入って関連アカウント検索の材料が汚れ、レート制限の
	// IP バケットも全利用者で共有される。印を付けて両方から外す。
	req = middleware.MarkInternalCall(req)

	if c.userID != "" {
		token, err := c.nativeToken()
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}

	// httptest.NewRecorder は stdlib の ResponseWriter 実装。WriteHeader の
	// 暗黙呼び出しまで含めて正しく振る舞うので、自前で書き直さない。
	rec := httptest.NewRecorder()
	c.api.echo.ServeHTTP(rec, req)

	res := rec.Result()
	defer res.Body.Close() //nolint:errcheck // in-memory recorder
	raw := rec.Body.Bytes()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, &plugin.APIError{Endpoint: endpoint, Status: res.StatusCode, Body: json.RawMessage(raw)}
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return json.RawMessage(raw), nil
}

// nativeToken resolves the user's native login token.
//
// アプリ用のアクセストークンではなく native token を使う。AsUser は「その利用者
// として振る舞う」ものなので、scope で制限された third-party token では意味が
// 変わってしまう (secure endpoint も通らない)。
func (c *pluginCaller) nativeToken() (string, error) {
	if c.api.userRepo == nil {
		return "", fmt.Errorf("plugin: AsUser を使えません (user repository が未配線です)")
	}
	u, err := c.api.userRepo.FindByID(c.userID)
	if err != nil {
		return "", fmt.Errorf("plugin: AsUser(%s) の利用者を取得できません: %w", c.userID, err)
	}
	if u == nil {
		return "", fmt.Errorf("plugin: AsUser(%s) の利用者が存在しません", c.userID)
	}
	if u.Host != nil {
		return "", fmt.Errorf("plugin: AsUser(%s) はリモート利用者です (ローカル利用者のみ指定できます)", c.userID)
	}
	if u.Token == nil || *u.Token == "" {
		return "", fmt.Errorf("plugin: AsUser(%s) の利用者はトークンを持ちません", c.userID)
	}
	return *u.Token, nil
}

// validateEndpoint rejects anything that is not a plain endpoint name.
//
// プラグインが組み立てた文字列がそのまま path になるので、`../` で /api の外へ
// 出られないようにする。
func validateEndpoint(endpoint string) error {
	if endpoint == "" {
		return fmt.Errorf("plugin: endpoint が空です")
	}
	if strings.HasPrefix(endpoint, "/") || strings.Contains(endpoint, "..") ||
		strings.ContainsAny(endpoint, "?#") {
		return fmt.Errorf("plugin: endpoint %q が不正です (例: \"notes/show\")", endpoint)
	}
	return nil
}

// --- Jobs ---

// pluginJobError maps plugin.ErrNoRetry onto the driver's skip-retry sentinel.
//
// プラグインは driver を import できない (internal) ので、公開面の sentinel を
// ここで写す。写さないと、4xx のように何度やっても直らない失敗を、残りの回数
// だけ繰り返す。
func pluginJobError(err error) error {
	if err == nil || !errors.Is(err, plugin.ErrNoRetry) || errors.Is(err, driver.ErrSkipRetry) {
		return err
	}
	return fmt.Errorf("%w: %w", driver.ErrSkipRetry, err)
}

type pluginJobs struct {
	name      string
	server    *queue.Server
	scheduler *queue.Scheduler
	// err holds the first registration failure. プラグイン側の callback を
	// エラーで汚さないため、ここに溜めて呼び出し元がまとめて検査する。
	err error
}

// taskType namespaces a plugin job so it cannot collide with mk-go's own types.
//
// **enqueue 側と同じものを使う** (queue.PluginTaskType)。別々に組み立てると、
// 片方を変えたときにジョブが「処理者なし」で捨てられる。
func (j *pluginJobs) taskType(name string) string {
	return queue.PluginTaskType(j.name, name)
}

func (j *pluginJobs) Handle(name string, h plugin.JobHandler) {
	if j.err != nil {
		return
	}
	if name == "" {
		j.err = fmt.Errorf("ジョブ名が空です")
		return
	}
	if j.server == nil {
		j.err = fmt.Errorf("ジョブ %q を登録できません (queue server が未配線です)", name)
		return
	}
	j.server.Handle(j.taskType(name), func(ctx context.Context, t driver.Task) error {
		return pluginJobError(h(ctx, t.Payload()))
	})
}

func (j *pluginJobs) Schedule(cron string, name string, payload any) {
	if j.err != nil {
		return
	}
	if j.scheduler == nil {
		j.err = fmt.Errorf("ジョブ %q をスケジュールできません (scheduler が未配線です)", name)
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		j.err = fmt.Errorf("ジョブ %q のペイロードを JSON 化できません: %w", name, err)
		return
	}
	if err := j.scheduler.RegisterPluginJob(cron, j.name, name, body); err != nil {
		j.err = fmt.Errorf("ジョブ %q をスケジュールできません: %w", name, err)
	}
}
