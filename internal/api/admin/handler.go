package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/api/meself"
	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/core/captcha"
	"github.com/elythia-network/elythia/internal/core/iplookuplog"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/core/procstats"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/misc/password"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/repository"
	"gorm.io/gorm"
)

// RelayService is the subset of core/relay.Service needed by the
// admin/relays endpoints. Kept as an interface so tests can inject a
// fake without the rest of the federation pipeline.
type RelayService interface {
	Add(ctx context.Context, inbox string) (*model.Relay, error)
	Remove(ctx context.Context, id string) error
	List(ctx context.Context) ([]*model.Relay, error)
}

// AbuseForwarder encapsulates the full "render a Flag activity for the
// report and deliver it to the target user's origin inbox" flow used by
// admin/forward-abuse-user-report. Kept as an interface so the handler
// stays testable without dragging in the full activitypub + queue stack.
type AbuseForwarder interface {
	ForwardReport(reportID string) error
}

// DeleteAccountEnqueuer schedules the background cascade deletion of a
// user's related rows (notes / drive files / follow graph) after the
// admin/accounts/delete flag flip. narrow interface to keep admin handler
// tests decoupled from the full queue stack.
type DeleteAccountEnqueuer interface {
	EnqueueDeleteAccount(payload queue.DeleteAccountPayload) error
}

// InstanceMetadataFetcher refreshes remote instance metadata (nodeinfo +
// iconUrl/faviconUrl) for the given host. Narrow interface that matches
// coreinstance.FetchMetadataService.Fetch so admin handler tests don't
// have to pull in the full federation stack.
type InstanceMetadataFetcher interface {
	Fetch(host string) error
}

// SystemAccountFetcher returns (and lazily creates) the built-in proxy /
// relay / instance actor users. Narrow interface matching
// coresystemaccount.Service.Fetch so admin handler tests stay decoupled.
type SystemAccountFetcher interface {
	Fetch(kind string) (*model.User, error)
}

// ObjectStorageEnqueuer schedules object storage housekeeping on the
// `objectStorage` queue (#2325). Implemented by queue.Client; admin handlers
// take the narrow interface so tests stay decoupled from the queue stack.
type ObjectStorageEnqueuer interface {
	// EnqueueDeleteObjectStorageFile queues the removal of one object.
	EnqueueDeleteObjectStorageFile(key string) error
	// EnqueueCleanRemoteFiles queues the bulk remote-cache sweep.
	EnqueueCleanRemoteFiles() error
}

// UnfollowEnqueuer schedules per-pair Unfollow background jobs, used by
// admin/federation/remove-all-following to detach all incoming follows from
// a host without blocking the HTTP request. The job is consumed by
// processors.UnfollowProcessor which calls core/following.Service.Unfollow
// (= Following row 削除 + Reject(Follow) 配送)。Misskey TS の
// queueService.createUnfollowJob 相当。
type UnfollowEnqueuer interface {
	EnqueueUnfollow(payload queue.UnfollowPayload) error
}

// Handler handles admin API endpoints.
type Handler struct {
	// background は応答の後の処理 (テストメールの送信) を起動する箱 (#3661)。世代の停止で
	// 待つ。nil なら素の goroutine で起動する (テスト)。
	background *bgwork.Group
	// detailExtras は UserDetailed のピン留め・移行先を users/show と同じ規則で埋める (#3330)。
	detailExtras userpack.DetailExtras
	// listPacker は一覧の利用者を本家 packMany と同じ形に組む (#3330)。
	listPacker    userpack.ListPacker
	signupService *signup.Service
	roleService   *role.Service
	metaRepo      repository.MetaRepository
	userRepo      repository.UserRepository
	// suspensionOriginRepo は凍結の由来の記録先 (#2973)。モデレーターの判断を
	// local として刻み、リモートの `toot:suspended` に上書きされないようにする。
	suspensionOriginRepo repository.UserSuspensionOriginRepository
	abuseRepo            repository.AbuseReportRepository
	modLogService        *moderationlog.Service
	emojiRepo            repository.EmojiRepository
	// #2934 の申請。nil なら endpoint は 500 を返す (未配線の構成)。
	emojiApplicationRepo     repository.EmojiApplicationRepository
	emojiApplicationReviewer emojiApplicationReviewer
	// broadcastPub は emoji の add/update/delete を broadcast stream へ流し、全
	// connection の emoji picker を live-refresh するために使う (#2046)。未配線なら
	// 通知しない。
	broadcastPub  BroadcastPublisher
	driveFileRepo repository.DriveFileRepository
	// driveBulkDeleter は federation/delete-all-files で host のファイルを物理
	// ストレージ込みで削除し drive 使用量を減算する (#1772)。未配線時は
	// driveFileRepo.DeleteByHost に degrade (row のみ削除)。
	driveBulkDeleter DriveBulkDeleter
	adminDB          *gorm.DB
	userIPRepo       repository.UserIPRepository
	securityKeyRepo  repository.UserSecurityKeyRepository
	queueInspector   QueueInspector
	// queueRuntime は mk-go 独自の worker 実行時情報 (#2277)。
	// 未配線なら admin/queue 応答から runtime block を省く。
	queueRuntime QueueRuntimeProvider
	queueRedis   QueueRedisInfoProvider
	// procStats は admin/server-metrics の起動時刻と時計 (#2395)。zero value でも
	// Collect は成功する (uptimeMs が 0 になるだけ)。
	procStats         procstats.Deps
	emojiEnqueuer     EmojiImportEnqueuer
	emojiImageFetcher EmojiImageFetcher
	// remoteEmojiMetaFetcher は admin/emoji/fetch-remote-meta が使う (#2698)。
	remoteEmojiMetaFetcher RemoteEmojiMetaFetcher
	relayService           RelayService
	abuseForwarder         AbuseForwarder
	deleteAccountEnqueuer  DeleteAccountEnqueuer
	systemWebhookRepo      repository.SystemWebhookRepository
	recipientRepo          repository.AbuseReportNotificationRecipientRepository
	// systemWebhookDispatcher は resolve-abuse-user-report 時に
	// abuseReportResolved system webhook を発火するための dispatcher
	// (*core/webhook.Service)。nil なら発火しない (#1723)。
	systemWebhookDispatcher SystemWebhookDispatcher
	adRepo                  repository.AdRepository
	metaResponseInvalidator MetaResponseCacheInvalidator
	avatarDecoRepo          repository.AvatarDecorationRepository
	avatarDecoInvalidator   AvatarDecorationCacheInvalidator
	emojiDecoInvalidator    AvatarDecorationCacheInvalidator
	inviteRepo              repository.RegistrationTicketRepository
	promoNoteRepo           repository.PromoNoteRepository
	noteFinder              NoteFinder
	smtpProxyURL            string
	serverURL               string
	idGen                   id.Generator
	configSetupPassword     string
	instanceMetadataFetcher InstanceMetadataFetcher
	systemAccountFetcher    SystemAccountFetcher
	followingRepo           repository.FollowingRepository
	unfollowEnqueuer        UnfollowEnqueuer
	// followReqRepo は suspend 時に対象 user の双方向 followRequest を削除する
	// ために使う (#1759, upstream UserSuspendService.postSuspend)。nil なら skip。
	followReqRepo repository.FollowRequestRepository
	// storageDeleter は admin の bulk drive cleanup で物理オブジェクトを消す
	// object storage backend (nil なら DB 行のみ削除)。
	storageDeleter StorageDeleter
	// localStorageDeleter は storedInternal=true な行 (object storage 有効化より
	// 前に保存されたファイル) の実体を消すためのローカル backend (#2315)。
	localStorageDeleter StorageDeleter
	// objectStorageEnqueuer は bulk drive cleanup の実体削除を queue へ
	// 逃がす (#2325)。nil なら従来どおり handler 内で同期削除する。
	objectStorageEnqueuer ObjectStorageEnqueuer
	// captchaVerifierFactory builds a one-off captcha.Verifier for
	// admin/captcha/save verification. nil uses the real providers; tests
	// inject a stub to avoid external HTTP calls.
	captchaVerifierFactory CaptchaVerifierFactory
	// serverPlugins は組み込み済みプラグインのスナップショット (#2497)。
	// 起動時の配線で確定する情報なので、リクエストごとに再計算しない。
	serverPlugins []ServerPluginInfo
	// pluginOrphanSchemas は残存データの検査。未配線なら応答で null になる。
	pluginOrphanSchemas PluginOrphanSchemaLister
	// instanceRepo は admin/federation/* の instance lookup / update を
	// inject 可能にするための DI 口 (#676)。FederationUpdateInstance の
	// log type 分岐をテストするため、`adminDB` 直叩きから repository 経由
	// に剥がしている。未配線時は `adminDB` フォールバックは持たず no-op。
	instanceRepo repository.InstanceRepository
	// deliveryHealth は admin/federation/delivery-health の集計元 (#2461)。
	// 未配線なら空の結果を返す (機能が無効なだけで、エラーではない)。
	deliveryHealth DeliveryHealthProvider
	// inboxHealth は admin/federation/inbox-health の集計元 (#2471)。
	inboxHealth DeliveryHealthProvider
	// deliveryBreaker は落ちた配送先へのブレーカー (#3048)。nil なら一覧は空で、
	// 閉じる操作は何もしない。
	deliveryBreaker DeliveryBreakerAdmin
	// ipSearchRepo は admin/ip/* の検索元 (#3104)。**未配線なら 500 を返す** —
	// 空の結果は「その IP を使ったアカウントは無い」という誤った事実になる。
	ipSearchRepo repository.UserIPSearchRepository
	// ipLookupAudit は admin/ip/* の照会を監査に残す (#3106)。
	ipLookupAudit *iplookuplog.Service
	// ipLookupLogRepo は admin/ip/lookup-log の読み出し元 (#3106)。
	ipLookupLogRepo repository.IPLookupLogRepository
	// driveUsage は admin/drive/usage の集計元 (#3053)。**未配線なら 500 を
	// 返す** — 0 バイトを返すと「使っていない」という誤った事実を出すため。
	driveUsage DriveUsageProvider
	// signupApplications は承認制の登録の審査面 (#2555)。未配線なら該当
	// endpoint は 503 を返す。
	signupApplications SignupApplicationReviewer
	// selfCheck は admin/self-check の実行元 (#2463)。未配線なら空の結果を返す。
	selfCheck SelfCheckRunner
	// remoteCheck は admin/federation/check-host の実行元 (#3055)。
	// remoteCheckSelfHost は自ホスト (断る)。
	remoteCheck         RemoteChecker
	remoteCheckSelfHost string
	// goneCleaner は消えたインスタンスとのフォロー関係の片付け (#3067)。
	goneCleaner GoneInstanceCleaner
	// fedRules は連合のルール (#3090)。
	fedRules FederationRuleManager
	// dbHealth は DB の健全性 (#3095)。
	dbHealth DatabaseHealthReader
	// clock は時刻の取得 (nil なら time.Now)。テストで経過時間に依存する計算
	// (関連アカウントのスコアの減衰など) を固定するため。
	clock func() time.Time
	// userTokenInvalidator は admin が他 user を suspend / unsuspend /
	// 論理削除した直後に target user の全 tokenCache entry を即時失効する
	// ために使う (#965)。i/regenerate-token (#884) や i/update (#960) と
	// 同じ AuthMiddleware が「token 単独削除」「user 全 token 削除」の 2
	// 種類の API を提供しており、admin 経由は後者を使う。production では
	// router で必ず wire する (未配線時は 30s cache TTL 待ちで stale 旧 user
	// が auth 通過する security regression が残る)。
	userTokenInvalidator UserTokenInvalidator
	// userStreamRevoker は凍結・削除した user の /streaming 接続を閉じる。
	// WebSocket は接続時に 1 度しか認証しないので、tokenCache を落としても
	// 既存の接続は通知・DM を受け取り続ける。
	userStreamRevoker UserStreamRevoker
	// signinRepo は admin/show-user の `signins` field を実データで埋める
	// ために使う (#1198)。未配線時は `[]` fallback で shape compat を保つ。
	signinRepo repository.SigninRepository
	// instanceSuspendInvalidator は admin が remote instance を suspend /
	// unsuspend した直後に deliver hot path の suspend 判定 cache を即時失効
	// するために使う (#1407 review)。未配線時は最大 cacheTTL (5min) 待ちで
	// stale な配送可否判定が残るが security regression ではないため optional。
	instanceSuspendInvalidator InstanceSuspendCacheInvalidator
	// userModerationFed は suspend / delete-account / unsuspend 時に対象 local
	// user の AP Delete / Undo(Delete) を remote instances へ配信する (#1759)。
	// nil なら連合副作用は no-op (security guard / DB 更新には影響しない)。
	userModerationFed UserModerationFederationHook
	// passwordHasher はadmin/reset-passwordで作ったパスワードをハッシュする(#3780)。
	// 零値は既定のcost。
	passwordHasher password.Hasher
	// selfPacker は閲覧者自身を MeDetailed へ昇格させ、/api/i の handler で未読・
	// role・ピン留めを埋める (#3784)。未配線なら補完せずに昇格だけする。
	selfPacker *meself.Packer
	// entityPacker は利用者や投稿を包むときの依存 (role 由来の値、メディアのプロキシ等) を
	// 持つ (#3792)。組み立てで渡す。
	entityPacker *entity.Packer
}

// SetSelfPacker wires the Packer that promotes the viewer's own entry into
// MeDetailed. Unset promotes without the handler-only fields.
func (h *Handler) SetSelfPacker(p *meself.Packer) { h.selfPacker = p }

// UserModerationFederationHook delivers AP Delete / Undo(Delete) for a local
// user's actor to remote instances when the account is suspended, deleted, or
// unsuspended (#1759). Implemented by core/federation.UserModerationDeliveryHook.
// nil disables the federation side-effect.
type UserModerationFederationHook interface {
	// OnUserDeleted は suspend / delete-account で Delete(actor) を配信する。
	OnUserDeleted(user *model.User)
	// OnUserRestored は unsuspend で Undo(Delete) を配信する。
	OnUserRestored(user *model.User)
}

// SetUserModerationFederationHook wires the AP delivery hook used on
// suspend / delete-account / unsuspend (#1759). nil disables federation.
func (h *Handler) SetUserModerationFederationHook(hook UserModerationFederationHook) {
	h.userModerationFed = hook
}

// SetFollowRequestRepo wires the follow-request repository so SuspendUser can
// delete the target's pending follow requests (both directions, #1759).
func (h *Handler) SetFollowRequestRepo(r repository.FollowRequestRepository) {
	h.followReqRepo = r
}

// InstanceSuspendCacheInvalidator drops the cached delivery-suspend decision
// for a host so the next ShouldSkipDelivery re-reads it from the DB. Implemented
// by core/instance.Service. Used by admin/federation/update-instance which
// writes suspensionState through instanceRepo directly (bypassing
// instance.Service.Suspend) — see #1407 review.
type InstanceSuspendCacheInvalidator interface {
	InvalidateSuspendCache(host string)
}

// SetInstanceSuspendCacheInvalidator wires the instance Service so
// admin/federation/update-instance flushes the deliver suspend cache for the
// affected host immediately. production では wire 推奨 (未配線時は最大 5min
// stale window が残るが、issue #1407 が許容する範囲)。
func (h *Handler) SetInstanceSuspendCacheInvalidator(inv InstanceSuspendCacheInvalidator) {
	h.instanceSuspendInvalidator = inv
}

// invalidateInstanceSuspendCache は host の deliver suspend 判定 cache を即時
// 失効させる helper。invalidator 未配線 / host 空のときは noop。
func (h *Handler) invalidateInstanceSuspendCache(host string) {
	if h.instanceSuspendInvalidator == nil || host == "" {
		return
	}
	h.instanceSuspendInvalidator.InvalidateSuspendCache(host)
}

// UserTokenInvalidator drops every cached auth entry for the given userID
// (= all sessions / API tokens belonging to that user) so the next
// authenticated request from any of those tokens re-resolves through the
// DB. Implemented by middleware.AuthMiddleware. Used by admin actions
// that change a user's middleware-relevant fields (isSuspended /
// isDeleted) — see #965.
type UserTokenInvalidator interface {
	InvalidateTokensForUser(userID string)
}

// UserStreamRevoker closes every live /streaming connection of a user.
// Implemented by stream.StreamRevokePublisher (pubsub fan-out so connections
// held by other processes close too).
type UserStreamRevoker interface {
	RevokeUserStreams(userID string)
}

// revokeUserStreams closes the target user's streaming connections. 凍結解除
// では呼ばない (凍結中の利用者の token では /streaming へ接続できない — upgrade が 403 — ので閉じる対象が無い)。
func (h *Handler) revokeUserStreams(userID string) {
	if h.userStreamRevoker == nil || userID == "" {
		return
	}
	h.userStreamRevoker.RevokeUserStreams(userID)
}

// invalidateUserTokenCache は target user の全 token cache entry を即時
// 失効させる helper。invalidator 未配線 / userID 空のときは noop。本
// helper を経由することで suspend / unsuspend / delete 等の admin 操作
// から共通の defensive guard を保てる (#965)。
func (h *Handler) invalidateUserTokenCache(userID string) {
	if h.userTokenInvalidator == nil || userID == "" {
		return
	}
	h.userTokenInvalidator.InvalidateTokensForUser(userID)
}

// NoteFinder is the minimal subset of repository.NoteRepository that admin
// handlers need to validate a noteId. Kept narrow so tests can supply a tiny
// fake without implementing the full NoteRepository surface.
type NoteFinder interface {
	FindByID(id string) (*model.Note, error)
}

// SetConfigSetupPassword sets the initial setup password from config. TS互換:
// 初回セットアップ時にクライアントが送るsetupPasswordとconfig値を照合する。
func (h *Handler) SetConfigSetupPassword(pw string) {
	h.configSetupPassword = pw
}

// SetSystemWebhookRepo attaches a SystemWebhookRepository for admin/system-webhook/*.
func (h *Handler) SetSystemWebhookRepo(r repository.SystemWebhookRepository) {
	h.systemWebhookRepo = r
}

// SetInstanceMetadataFetcher attaches the fetcher used by
// admin/federation/refresh-remote-instance-metadata to re-fetch nodeinfo +
// icon for a specific host on demand.
func (h *Handler) SetInstanceMetadataFetcher(f InstanceMetadataFetcher) {
	h.instanceMetadataFetcher = f
}

// SetSystemAccountFetcher attaches the system account service used by
// admin/meta and admin/update-proxy-account to materialize the built-in
// proxy account.
func (h *Handler) SetSystemAccountFetcher(f SystemAccountFetcher) {
	h.systemAccountFetcher = f
}

// SetInstanceRepo wires an InstanceRepository for admin/federation handlers
// to use when reading/updating instance rows. Without it,
// FederationUpdateInstance early-returns 204 (#676)。
func (h *Handler) SetInstanceRepo(r repository.InstanceRepository) {
	h.instanceRepo = r
}

// SetSigninRepo wires a SigninRepository so admin/show-user can populate the
// target user's signin history. Without it the `signins` field falls back to
// an empty array (#1198).
func (h *Handler) SetSigninRepo(r repository.SigninRepository) { h.signinRepo = r }

// recordLocalSuspensionOrigin marks the user's suspension state as decided by
// our own moderator (#2973).
//
// **失敗しても呼び出し元は続行する。** 凍結・削除そのものは成立させたいため。
// ただし**残るのは「記録が無い」ではなく「古い由来」**なので、`remote` の行が
// 残っていると発信元に解除されうる。そこは warn で拾う (解除側は
// `UnsuspendUser` が 500 で返す)。
func (h *Handler) recordLocalSuspensionOrigin(userID string) {
	if h.suspensionOriginRepo == nil {
		return
	}
	if err := h.suspensionOriginRepo.Set(userID, model.SuspensionOriginLocal); err != nil {
		slog.Warn("admin: failed to record suspension origin", "userId", userID, "err", err)
	}
}

// SetFollowingRepo attaches a FollowingRepository for admin endpoints that
// need to enumerate Following rows by host (e.g.
// admin/federation/remove-all-following).
func (h *Handler) SetFollowingRepo(r repository.FollowingRepository) {
	h.followingRepo = r
}

// SetUnfollowEnqueuer attaches the queue client used by
// admin/federation/remove-all-following to schedule per-pair Unfollow
// background jobs. Typically wired with the project-wide queue client.
func (h *Handler) SetUnfollowEnqueuer(e UnfollowEnqueuer) {
	h.unfollowEnqueuer = e
}

// SetRecipientRepo attaches an AbuseReportNotificationRecipientRepository for
// admin/abuse-report/notification-recipient/*.
func (h *Handler) SetRecipientRepo(r repository.AbuseReportNotificationRecipientRepository) {
	h.recipientRepo = r
}

// SystemWebhookDispatcher fans a system-level event out to subscribed system
// webhooks, optionally excluding specific webhook IDs. Implemented by
// *core/webhook.Service. Used to fire abuseReportResolved on report resolution
// (#1723).
type SystemWebhookDispatcher interface {
	DispatchSystemExcluding(eventType string, body any, excludes []string)
	// DispatchSystemTest enqueues a single test delivery to a system webhook
	// (admin/system-webhook/test, #1542). overrideURL/Secret が非空ならそちらへ。
	DispatchSystemTest(webhookID, eventType string, body any, overrideURL, overrideSecret string)
}

// SetSystemWebhookDispatcher wires the system webhook dispatcher used to emit
// abuseReportResolved. nil disables firing (DB update still happens).
func (h *Handler) SetSystemWebhookDispatcher(d SystemWebhookDispatcher) {
	h.systemWebhookDispatcher = d
}

// SetAdRepo attaches an AdRepository for admin/ad/*.
func (h *Handler) SetAdRepo(r repository.AdRepository) { h.adRepo = r }

// MetaResponseCacheInvalidator drops the cached /api/meta response bodies.
// Implemented by api/meta.Handler (#2649). `ads` は /api/meta にそのまま載るので、
// admin 側で ad を作り替えたら落とす必要がある。
//
// **これは process-local。** meta 行には internal:metaUpdated の cross-worker
// 経路がある (#1740) が、ads には無いので、複数プロセス構成では mutation を
// 処理した worker 以外は最大 TTL 分だけ古い ads を返す。TTL が数秒なので
// そこで頭打ちになる。
type MetaResponseCacheInvalidator interface {
	InvalidateResponseCache()
}

// SetMetaResponseInvalidator wires the /api/meta response cache invalidator so
// ad mutations are reflected immediately in the worker that handled them.
func (h *Handler) SetMetaResponseInvalidator(inv MetaResponseCacheInvalidator) {
	h.metaResponseInvalidator = inv
}

// invalidateMetaResponseCache is a nil-safe helper for the ad mutation
// handlers. noop when the invalidator is not wired (unit tests).
func (h *Handler) invalidateMetaResponseCache() {
	if h.metaResponseInvalidator == nil {
		return
	}
	h.metaResponseInvalidator.InvalidateResponseCache()
}

// AvatarDecorationCacheInvalidator drops the process-wide avatar decoration
// catalog cache used by (*entity.Packer).PackUserLite. Implemented by
// core/avatardecoration.Resolver.
type AvatarDecorationCacheInvalidator interface {
	Invalidate()
}

// SetAvatarDecorationInvalidator wires the catalog cache invalidator so that
// admin-side catalog mutations are visible to user packing immediately
// instead of after the resolver TTL (#2258).
func (h *Handler) SetAvatarDecorationInvalidator(inv AvatarDecorationCacheInvalidator) {
	h.avatarDecoInvalidator = inv
}

// invalidateAvatarDecorationCache is a nil-safe helper for the mutation
// handlers. noop when the invalidator is not wired (unit tests).
func (h *Handler) invalidateAvatarDecorationCache() {
	if h.avatarDecoInvalidator == nil {
		return
	}
	h.avatarDecoInvalidator.Invalidate()
}

// SetEmojiDecorationInvalidator wires the cache used to resolve emoji-backed
// avatar decorations (#2975). 絵文字を作った直後にそれを装着すると、キャッシュが
// 古い間は**保存されているのに API が `avatarDecorations: []` を返す** —
// #2258 が catalog 側で踏んだのと同じ形なので、同じように mutation で捨てる。
func (h *Handler) SetEmojiDecorationInvalidator(inv AvatarDecorationCacheInvalidator) {
	h.emojiDecoInvalidator = inv
}

// invalidateEmojiDecorationCache is a nil-safe helper. noop when unwired.
//
// **呼ぶのは publishEmoji* の 4 つで、いずれも body の先頭**。どの helper も
// `broadcastPub == nil` で早期 return するので、後ろに置くと stream を
// 使わない構成で効かなくなる。位置は
// `TestEveryEmojiPublishHelperCallsInvalidate` が AST で固定する。
//
// **helper に置くだけでは足りない。** 絵文字 row を変えて publishEmoji* を
// 通らない経路があると素通りするので、`TestEmojiMutationsDropDecorationCache`
// が「絵文字を変える関数はキャッシュを捨てる」を関数単位で見る (zip
// インポートは実際にこちら側で、`internal/core/emojiimport` が自前の
// invalidator を持つ)。
func (h *Handler) invalidateEmojiDecorationCache() {
	if h.emojiDecoInvalidator == nil {
		return
	}
	h.emojiDecoInvalidator.Invalidate()
}

// SetAvatarDecorationRepo attaches an AvatarDecorationRepository for
// admin/avatar-decorations/*.
func (h *Handler) SetAvatarDecorationRepo(r repository.AvatarDecorationRepository) {
	h.avatarDecoRepo = r
}

// SetInviteRepo attaches a RegistrationTicketRepository for admin/invite/*.
func (h *Handler) SetInviteRepo(r repository.RegistrationTicketRepository) {
	h.inviteRepo = r
}

// SetPromoNoteRepo attaches a PromoNoteRepository for admin/promo/*.
func (h *Handler) SetPromoNoteRepo(r repository.PromoNoteRepository) { h.promoNoteRepo = r }

// SetNoteFinder attaches a NoteFinder used to validate noteId inputs.
func (h *Handler) SetNoteFinder(r NoteFinder) { h.noteFinder = r }

// SetSMTPProxyURL forwards admin/send-email TCP connections through the
// configured proxy (cfg.ProxySMTP). Empty string disables the proxy and
// falls back to direct dial. See internal/misc/smtp.SendWithOptions.
func (h *Handler) SetSMTPProxyURL(u string) { h.smtpProxyURL = u }

// SetServerURL sets the base URL used inside password-reset email bodies.
func (h *Handler) SetServerURL(u string) { h.serverURL = u }

// SetRelayService wires the relay service used by admin/relays endpoints.
// nil を渡せば Admin API が DB fallback (create/update/delete のみ) に戻る。
func (h *Handler) SetRelayService(s RelayService) {
	h.relayService = s
}

// SetAbuseForwarder wires a forwarder for admin/forward-abuse-user-report.
// When nil the handler falls back to just flipping the DB forwarded flag
// (pre-P4-5 behaviour).
func (h *Handler) SetAbuseForwarder(f AbuseForwarder) {
	h.abuseForwarder = f
}

// CaptchaVerifierFactory builds a captcha.Verifier for the given provider and
// the request-supplied secret / sitekey / instanceUrl. Used by
// admin/captcha/save to verify the captchaResult before persisting settings.
type CaptchaVerifierFactory func(provider, secret, sitekey, instanceURL string) captcha.Verifier

// SetCaptchaVerifierFactory overrides the captcha verifier construction used by
// admin/captcha/save (tests inject a stub to avoid external HTTP).
func (h *Handler) SetCaptchaVerifierFactory(f CaptchaVerifierFactory) {
	h.captchaVerifierFactory = f
}

// SetDeleteAccountEnqueuer wires the enqueuer that schedules cascade
// deletion of related rows for admin/accounts/delete and admin/delete-account.
// When nil the handlers still flip the soft-delete flags but no background
// cleanup runs — useful in tests.
func (h *Handler) SetDeleteAccountEnqueuer(e DeleteAccountEnqueuer) {
	h.deleteAccountEnqueuer = e
}

// EmojiImportEnqueuer is the subset of queue.Enqueuer needed to schedule
// admin/emoji/import-zip jobs. 小さいインターフェースにすることで handler の
// テストが容易になる。
type EmojiImportEnqueuer interface {
	EnqueueImportCustomEmojis(payload queue.ImportCustomEmojisPayload) error
}

// SetEmojiImportEnqueuer attaches an EmojiImportEnqueuer for the admin/emoji/
// import-zip endpoint.
func (h *Handler) SetEmojiImportEnqueuer(e EmojiImportEnqueuer) {
	h.emojiEnqueuer = e
}

// EmojiImageFetcher downloads a remote image and stores it in the local
// drive, returning the resulting drive file. Used by admin/emoji/copy to
// detach a copied emoji from its source server (#670). 小さい interface に
// 切り出すことで http client / drive.Service への直接依存を持たずに済み、
// handler 単体テストで fake を差し込める。
type EmojiImageFetcher interface {
	FetchAndStore(ctx context.Context, url string, user *model.User, name string) (*model.DriveFile, error)
	// CopyToSystemFile duplicates an existing drive file as a system-owned one
	// (#2966。`admin/emoji/add` も同じ複製を通す、#2999)。
	//
	// **HTTP を経由しない。** 自分の公開 URL を叩くと SSRF ガードと衝突し、
	// 非公開 URL の構成では取れず、DB 上の行と実体の対応も確かめられない。
	// ストレージから直に読む。
	//
	// **元のファイルは触らない** — 申請者はノートの添付やプロフィールで
	// 使っている可能性があり、所有権を移すと drive から突然消える。
	CopyToSystemFile(ctx context.Context, src *model.DriveFile, name string, sensitive bool) (*model.DriveFile, error)
	// DeleteSystemFile removes a system-owned file created by CopyToSystemFile
	// when the approval then failed (#2966).
	DeleteSystemFile(ctx context.Context, fileID string) error
}

// QueueInspector abstracts the driver inspector for queue management
// endpoints.
type QueueInspector interface {
	Queues() ([]string, error)
	GetQueueInfo(qname string) (*QueueInfoResult, error)
	DeleteTask(qname, taskID string) error
	DeleteAllPendingTasks(qname string) (int, error)
	RunTask(qname, taskID string) error
	// PauseQueue / UnpauseQueue pause and resume a queue (#17436)。
	PauseQueue(qname string) error
	UnpauseQueue(qname string) error
	// Task listing APIs. page is 1-indexed.
	ListPendingTasks(qname string, page, pageSize int) ([]*QueueTaskSummary, error)
	ListActiveTasks(qname string, page, pageSize int) ([]*QueueTaskSummary, error)
	ListScheduledTasks(qname string, page, pageSize int) ([]*QueueTaskSummary, error)
	ListRetryTasks(qname string, page, pageSize int) ([]*QueueTaskSummary, error)
	// ListDelayedTasks は delayed バケット全体 (scheduled + retry) を新しい順に返す (#3167)。
	ListDelayedTasks(qname string, page, pageSize int) ([]*QueueTaskSummary, error)
	ListCompletedTasks(qname string, page, pageSize int) ([]*QueueTaskSummary, error)
	ListFailedTasks(qname string, page, pageSize int) ([]*QueueTaskSummary, error)
	GetTaskInfo(qname, taskID string) (*QueueTaskSummary, error)
	// GetTaskLogs returns the job's log lines (oldest first) and the total
	// number stored. upstream の show-job-logs は queue.getJobLogs(jobId).logs
	// をそのまま返す (#2689)。
	GetTaskLogs(qname, taskID string, start, end int64) ([]string, int64, error)
	QueueMetrics(qname, kind string) (*QueueMetricsResult, error)
}

// QueueMetricsResult is the per-queue completed / failed time-series
// snapshot returned by QueueInspector.QueueMetrics. Mirrors
// driver.MetricsResult — duplicated here to keep the admin package
// free of internal/queue/driver imports.
type QueueMetricsResult struct {
	Count int64
	Data  []int64
}

// QueueInfoResult holds basic queue statistics.
type QueueInfoResult struct {
	Queue     string
	Size      int
	Active    int
	Pending   int
	Completed int
	Failed    int
	Scheduled int
	Retry     int
	IsPaused  bool
	// QualifiedName is BullMQ's `<keyPrefix>:<name>`。空なら Queue で代替する。
	QualifiedName string
}

// QueueTaskSummary mirrors queue.TaskSummary for handler responses without
// taking a compile-time dependency on the queue package.
type QueueTaskSummary struct {
	ID            string
	Queue         string
	Type          string
	State         string
	Payload       []byte
	Retried       int
	MaxRetry      int
	LastErr       string
	LastFailedAt  time.Time
	NextProcessAt time.Time
	EnqueuedAt    time.Time
	ProcessedAt   time.Time
	CompletedAt   time.Time
	// ProcessedBy is the worker name that last dequeued the job (BullMQ
	// job.processedBy). Output as the upstream optional QueueJob.processedBy.
	ProcessedBy string

	// 以下は BullMQ の job HASH をそのまま運ぶ (#2689)。upstream の
	// packJobData と同じく**保存されている値をそのまま出す**。持たない
	// driver は空のまま。
	Opts        json.RawMessage
	Delay       int64
	Stacktrace  []string
	ReturnValue json.RawMessage
	Progress    json.RawMessage
	// AttemptsAt は試行ごとの開始時刻 (unix ミリ秒、古い順)。mkq の拡張で、
	// upstream には対応物が無い (#2692)。
	AttemptsAt []int64
}

// SetDriveFileRepo attaches a DriveFileRepository for admin drive operations.
func (h *Handler) SetDriveFileRepo(r repository.DriveFileRepository) {
	h.driveFileRepo = r
}

// StorageDeleter removes a stored object by its access key (drive.Storage の
// Delete に対応)。admin/drive/clean-remote-files と delete-all-files-of-a-user で
// DB 行削除前に object storage の物理オブジェクトを消すために使う。
type StorageDeleter interface {
	Delete(accessKey string) error
}

// SetLocalStorageDeleter wires the always-local backend used to delete rows
// whose `storedInternal` is true while object storage is the active backend
// (#2315). Optional — nil falls back to the primary deleter.
func (h *Handler) SetLocalStorageDeleter(s StorageDeleter) {
	h.localStorageDeleter = s
}

// SetObjectStorageEnqueuer wires the queue client used to offload drive
// cleanup work to the `objectStorage` queue (#2325). Optional — nil keeps the
// synchronous in-request deletion.
func (h *Handler) SetObjectStorageEnqueuer(e ObjectStorageEnqueuer) {
	h.objectStorageEnqueuer = e
}

// SetStorageDeleter wires the object-storage backend so bulk drive cleanups can
// remove physical objects (not just DB rows). nil keeps the DB-only behaviour.
func (h *Handler) SetStorageDeleter(s StorageDeleter) {
	h.storageDeleter = s
}

// SetAdminDB attaches a DB connection for ad/invite/relay operations.
func (h *Handler) SetAdminDB(db *gorm.DB) {
	h.adminDB = db
}

// SetUserIPRepo attaches a UserIPRepository for admin/get-user-ips.
func (h *Handler) SetUserIPRepo(r repository.UserIPRepository) {
	h.userIPRepo = r
}

// SetSecurityKeyRepo attaches a UserSecurityKeyRepository for admin/unset-mfa
// (passkey bulk deletion). 未配線のまま unset-mfa を呼ぶと 500 になる
// (パスキーを消さずに成功を返すと危険な中間状態が残るため)。
func (h *Handler) SetSecurityKeyRepo(r repository.UserSecurityKeyRepository) {
	h.securityKeyRepo = r
}

// SetQueueInspector attaches a queue inspector for admin queue endpoints.
func (h *Handler) SetQueueInspector(qi QueueInspector) {
	h.queueInspector = qi
}

// QueueRuntimeProvider exposes the mk-go-only worker runtime view backing the
// `runtime` block on admin/queue responses (#2277): how many workers a queue
// currently has, the configured auto-scale bounds, recent latency quantiles
// and the recent scale events.
//
// Workers は「仕事を取れる worker 数」であって goroutine 数ではない。mkq
// driver では handler が閾値を超えて戻ってこない worker が除かれるので、
// job を処理している最中でも minWorkers を下回ることがある (#2657)。
// 放棄した handler が予算を食って roster ごと縮んでいることもある (#2658)。
// どちらなのかは `mk_job_workers_quarantined` と
// `mk_job_handlers_abandoned` を見比べて判断する
// (`enableMetrics: true` のときだけ expose される)。
//
// upstream Misskey は worker 数を静的な config でしか持たないので該当情報が
// 無い。additive block なので純正 frontend は無視する。
type QueueRuntimeProvider interface {
	// QueueRuntime returns the runtime view for qname. ok=false when the
	// queue is unknown to the provider (= no block emitted).
	QueueRuntime(qname string) (QueueRuntime, bool)
}

// QueueRuntime is the per-queue runtime view. Durations are milliseconds so
// the JSON stays language-neutral.
type QueueRuntime struct {
	Workers        int               `json:"workers"`
	MinWorkers     int               `json:"minWorkers"`
	MaxWorkers     int               `json:"maxWorkers"`
	AutoScale      bool              `json:"autoScale"`
	DispatchWaitMs Quantiles         `json:"dispatchWaitMs"`
	ProcessingMs   Quantiles         `json:"processingMs"`
	RecentFailures int               `json:"recentFailures"`
	ScaleEvents    []QueueScaleEvent `json:"scaleEvents"`
}

// Quantiles summarises a latency window. Count is the number of samples the
// window currently holds (bounded ring buffer), not a lifetime total.
type Quantiles struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	Max   float64 `json:"max"`
}

// QueueScaleEvent is one committed auto-scale decision, newest first.
type QueueScaleEvent struct {
	At        string `json:"at"`
	Direction string `json:"direction"`
	From      int    `json:"from"`
	To        int    `json:"to"`
}

// SetQueueRuntimeProvider wires the runtime view (#2277).
func (h *Handler) SetQueueRuntimeProvider(p QueueRuntimeProvider) {
	h.queueRuntime = p
}

// SetPasswordHasher sets the Hasher that admin/reset-password hashes the
// generated password with. Unset, the default cost is used.
func (h *Handler) SetPasswordHasher(hasher password.Hasher) { h.passwordHasher = hasher }

// NewHandler creates a new admin Handler.
//
// The remaining arguments are the dependencies whose absence would degrade
// behaviour silently (#3823):
//   - background is the group the work after a response (the test mail)
//     runs in, so stopping the server generation waits for it (#3661)
//   - entityPacker is the Packer this handler packs entities with (#3792)
//   - detailExtras fills pinnedNotes / pinnedPage / movedTo / alsoKnownAs for
//     admin/accounts/find-by-email and admin/update-proxy-account (#3330)
//   - listPacker packs the users embedded in admin/show-users,
//     admin/roles/users, admin/abuse-user-reports and
//     admin/show-moderation-logs, and the instance / emojis of
//     admin/accounts/find-by-email (#3330)
//   - suspensionOriginRepo records that a moderator decided the suspension
//     state, so the next actor refresh does not undo it (#2973)
//   - userTokenInvalidator drops the target user's cached auth entries on
//     admin/suspend-user / admin/unsuspend-user / admin/accounts/delete (#965)
//   - userStreamRevoker closes the /streaming connections of suspended and
//     deleted users
//   - ipLookupAudit records the admin/ip/* lookups (#3106)
//   - emojiImageFetcher takes emoji images into system-owned drive files for
//     admin/emoji/copy, admin/emoji/add, admin/emoji/update and emoji
//     application approval (#2966 / #670 / #2999)
func NewHandler(
	signupService *signup.Service,
	roleService *role.Service,
	metaRepo repository.MetaRepository,
	userRepo repository.UserRepository,
	idGen id.Generator,
	background *bgwork.Group,
	entityPacker *entity.Packer,
	detailExtras userpack.DetailExtras,
	listPacker userpack.ListPacker,
	suspensionOriginRepo repository.UserSuspensionOriginRepository,
	userTokenInvalidator UserTokenInvalidator,
	userStreamRevoker UserStreamRevoker,
	ipLookupAudit *iplookuplog.Service,
	emojiImageFetcher EmojiImageFetcher,
) *Handler {
	return &Handler{
		signupService:        signupService,
		roleService:          roleService,
		metaRepo:             metaRepo,
		userRepo:             userRepo,
		idGen:                idGen,
		background:           background,
		entityPacker:         entityPacker,
		detailExtras:         detailExtras,
		listPacker:           listPacker,
		suspensionOriginRepo: suspensionOriginRepo,
		userTokenInvalidator: userTokenInvalidator,
		userStreamRevoker:    userStreamRevoker,
		ipLookupAudit:        ipLookupAudit,
		emojiImageFetcher:    emojiImageFetcher,
	}
}

// SetAbuseRepo attaches the abuse report repository.
func (h *Handler) SetAbuseRepo(r repository.AbuseReportRepository) { h.abuseRepo = r }

// SetModLogService attaches the moderation log service. Both the read
// endpoint (admin/show-moderation-logs → Service.List) and the write
// path (admin handlers → Service.Log) go through this single object so
// wiring cannot drift out of sync.
func (h *Handler) SetModLogService(s *moderationlog.Service) {
	h.modLogService = s
}

// now returns the current time, or the fixed one set in tests (SetClockForTest)。
func (h *Handler) now() time.Time {
	if h.clock != nil {
		return h.clock()
	}
	return time.Now()
}
