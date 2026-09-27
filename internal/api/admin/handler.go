package admin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/pagination"
	"github.com/shiroha-a/mk/internal/config"
	"github.com/shiroha-a/mk/internal/core/captcha"
	coredrive "github.com/shiroha-a/mk/internal/core/drive"
	"github.com/shiroha-a/mk/internal/core/emojiapplication"
	"github.com/shiroha-a/mk/internal/core/iplookuplog"
	"github.com/shiroha-a/mk/internal/core/moderationlog"
	"github.com/shiroha-a/mk/internal/core/procstats"
	"github.com/shiroha-a/mk/internal/core/role"
	"github.com/shiroha-a/mk/internal/core/signup"
	"github.com/shiroha-a/mk/internal/core/signupapplication"
	corewebhook "github.com/shiroha-a/mk/internal/core/webhook"
	"github.com/shiroha-a/mk/internal/core/webpush"
	"github.com/shiroha-a/mk/internal/effectivepolicy"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/colfit"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/safehttp"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"gorm.io/datatypes"
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
	// procStats は admin/server-metrics が返すプロセス統計の provider 束 (#2395)。
	// zero value でも Collect は成功する (取れない section が省かれる)。
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
}

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

// SetUserTokenInvalidator wires the AuthMiddleware so admin/suspend-user
// / admin/unsuspend-user / admin/accounts/delete drop the target user's
// cached auth entries immediately. production では必ず wire すること
// (未配線時は最大 30 秒 stale window が残る)。
func (h *Handler) SetUserTokenInvalidator(inv UserTokenInvalidator) {
	h.userTokenInvalidator = inv
}

// UserStreamRevoker closes every live /streaming connection of a user.
// Implemented by stream.StreamRevokePublisher (pubsub fan-out so connections
// held by other processes close too).
type UserStreamRevoker interface {
	RevokeUserStreams(userID string)
}

// SetUserStreamRevoker wires the revoker used by admin/suspend-user and the
// account deletion endpoints.
func (h *Handler) SetUserStreamRevoker(r UserStreamRevoker) {
	h.userStreamRevoker = r
}

// HasUserStreamRevoker reports whether the stream revoker is wired.
//
// 未配線だと、凍結・削除した利用者の WebSocket が**再接続するまで通知・DM・
// フォロワー限定投稿を受け取り続ける** (HasUserTokenInvalidator とは独立)。
func (h *Handler) HasUserStreamRevoker() bool { return h.userStreamRevoker != nil }

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

// SetSuspensionOriginRepo wires the suspension-origin store (#2973).
//
// **未配線だとモデレーターの解除が次の actor refresh で戻る。** 由来が無い行を
// resolver が local 扱いにするのは**解除方向だけ**で、凍結方向は
// 「まだ誰も判断していない」として発信元に従うため。`criticalWiring` で見る。
func (h *Handler) SetSuspensionOriginRepo(r repository.UserSuspensionOriginRepository) {
	h.suspensionOriginRepo = r
}

// HasSuspensionOriginRepo reports whether the store is wired.
func (h *Handler) HasSuspensionOriginRepo() bool { return h.suspensionOriginRepo != nil }

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
// catalog cache used by entity.PackUserLite. Implemented by
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

// SetEmojiImageFetcher attaches an EmojiImageFetcher used by admin/emoji/copy.
// nil のままだと EmojiCopy は src の URL をそのまま継承する legacy 挙動を
// 維持する (テスト容易性 + 未配線環境での graceful degradation 用)。
func (h *Handler) SetEmojiImageFetcher(f EmojiImageFetcher) {
	h.emojiImageFetcher = f
}

// HasEmojiImageFetcher reports whether emoji images can be taken into drive.
//
// **未配線だと 3 つのバグがそのまま残る (#2966 / #670 / #2999)。** 自作画像の申請を
// 承認すると絵文字が申請者所有のファイルを参照し続け (申請者が消すと壊れる)、
// リモート絵文字の複製は相手サーバーの URL を参照し続け (相手が消すと壊れる)、
// `admin/emoji/add` は操作者の drive ファイルを参照し続ける (操作者が消すと壊れる)。
// いずれも「動いているように見えて後から壊れる」ので、起動時に気付けるようにする。
func (h *Handler) HasEmojiImageFetcher() bool { return h.emojiImageFetcher != nil }

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

// NewHandler creates a new admin Handler.
func NewHandler(
	signupService *signup.Service,
	roleService *role.Service,
	metaRepo repository.MetaRepository,
	userRepo repository.UserRepository,
	idGen id.Generator,
) *Handler {
	return &Handler{
		signupService: signupService,
		roleService:   roleService,
		metaRepo:      metaRepo,
		userRepo:      userRepo,
		idGen:         idGen,
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

// AccountsCreate handles POST /api/admin/accounts/create.
// 初回セットアップ (rootUserId未設定) の場合は認証不要。
// それ以外はadmin権限が必要。
func (h *Handler) AccountsCreate(c echo.Context) error {
	var req struct {
		Username      string  `json:"username"`
		Password      string  `json:"password"`
		SetupPassword *string `json:"setupPassword"`
	}
	if err := c.Bind(&req); err != nil || req.Username == "" || req.Password == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	meta, err := h.metaRepo.Fetch()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	user := middleware.GetUser(c)
	isInitialSetup := meta.RootUserID == nil && user == nil

	// **利用者が既に居るなら初回セットアップではない。**
	//
	// `meta.rootUserId` を書くのはコードベース全体で 1 箇所 (signup service の
	// `isInitialSetup` 分岐) しか無く、公開 `/api/signup` は本番でそこを通らない
	// (`internal/api/signup/handler.go` が `h.testMode && ...` で判定している。
	// 開放した瞬間に誰でも root を取れてしまうのを避けるための、それ自体は
	// 妥当な判断)。その結果、**運営者が登録を開放して通常の signup で最初の
	// アカウントを作ると `rootUserId` は永久に NULL のまま**になり、この
	// endpoint が未認証のまま開き続ける。`update-meta` は `rootUserId` を
	// protected として落とすので、管理者が手で閉じることもできない。
	//
	// upstream は `SignupService` が経路を問わず「`rootUserId` が NULL なら
	// 作ったアカウントを root にする」ので最初の 1 人で窓が閉じる。mk-go は
	// そちらを採らない代わりに、ここで閉じる。
	//
	// **数えられなければ初回セットアップとして扱わない (fail-closed)。**
	// 判定できないことを理由に未認証の窓を開けるわけにいかない。正当な初回
	// セットアップは再試行すれば通るし、原因が分かるよう 500 で返す
	// (ACCESS_DENIED に潰すと「権限が足りない」と誤読される)。
	if isInitialSetup {
		if h.userRepo == nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
		n, cerr := h.userRepo.CountLocalUsers()
		if cerr != nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
		if n > 0 {
			isInitialSetup = false
		}
	}

	if isInitialSetup {
		// TS互換: setupPassword検証。configにsetupPasswordが設定されている場合は
		// クライアントの値と一致させる。未設定なのにクライアントが非空値を送った場合も拒否。
		clientPW := ""
		if req.SetupPassword != nil {
			clientPW = strings.TrimSpace(*req.SetupPassword)
		}
		if h.configSetupPassword != "" {
			// **定数時間で比較する。** ここは未認証で到達できる唯一の
			// credential 検査で、素の != は一致した先頭バイト数だけ時間が延びる。
			if subtle.ConstantTimeCompare([]byte(clientPW), []byte(h.configSetupPassword)) != 1 {
				return c.JSON(http.StatusBadRequest, apierr.Error("INCORRECT_INITIAL_PASSWORD", "Initial password is incorrect.", "97147c55-1ae1-4f6f-91d6-e1c3e0e76d62"))
			}
		} else if clientPW != "" {
			return c.JSON(http.StatusBadRequest, apierr.Error("INCORRECT_INITIAL_PASSWORD", "Initial password is incorrect.", "97147c55-1ae1-4f6f-91d6-e1c3e0e76d62"))
		}
	} else {
		// 初回セットアップ以外はadministrator権限必須。upstream 2026.7.0 (#17783)
		// で「rootUserId 本人のみ」から「任意の administrator」に緩和された
		// (初期設定で作った root 以外の admin がこの API を使えなかった修正)。
		// roleService 未配線時は旧仕様の root 本人比較に fallback (fail-closed)。
		if user == nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
		}
		if h.roleService != nil {
			if !h.roleService.IsAdministrator(user.ID) {
				return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
			}
		} else if meta.RootUserID == nil || *meta.RootUserID != user.ID {
			return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
		}
		// #2106 S2: upstream create.ts の inline `token !== null` gate を再現する。
		// この endpoint は route middleware を持たず paramDef に kind も無いため、
		// app/OAuth access token (read:account 等の狭い scope) でも root に到達して
		// アカウント作成できる scope 越境になる。native login token のみ許可する。
		if sc := middleware.GetAuthScope(c); sc != nil && sc.IsApp {
			return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
		}
	}

	// **admin は最小文字数の制限を受けない (#3015)。** 運営が公式アカウントに
	// 短い ID を配れるようにするため。`preservedUsernames` は引き続き効くので、
	// 「予約は効くが最小長は効かない」という非対称になる。
	result, err := h.signupService.Signup(req.Username, req.Password, isInitialSetup, signup.UsernamePolicyOperator)
	if err != nil {
		if err == signup.ErrUsernameAlreadyExists {
			return c.JSON(http.StatusConflict, apierr.Error("USERNAME_ALREADY_EXISTS", "Username already exists.", "0a504947-b888-4a99-9f62-8c4a0f3a3dab"))
		}
		if err == signup.ErrInvalidUsername {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid username.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
		if err == signup.ErrUsernameReserved {
			return c.JSON(http.StatusBadRequest, apierr.Error("USED_USERNAME", "That username is reserved.", "4b54bee6-2c25-42c3-a10f-7d0d1fbd91f9"))
		}
		slog.Error("admin/accounts/create: signup failed", "username", req.Username, "err", err)
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	u := result.User
	detailed := entity.PackUserDetailed(u, nil)

	isAdmin := false
	isMod := false
	userPolicies := role.DefaultPolicies()
	if h.roleService != nil {
		isAdmin = h.roleService.IsAdministrator(u.ID)
		isMod = h.roleService.IsModerator(u.ID)
		userPolicies = h.roleService.GetUserPolicies(u.ID)
	}

	out := map[string]any{
		// UserLite
		"id":                u.ID,
		"name":              detailed.Name,
		"username":          detailed.Username,
		"host":              detailed.Host,
		"avatarUrl":         detailed.AvatarURL,
		"avatarBlurhash":    detailed.AvatarBlurhash,
		"avatarDecorations": detailed.AvatarDecorations,
		"isBot":             detailed.IsBot,
		"isCat":             detailed.IsCat,
		"emojis":            detailed.Emojis,
		"onlineStatus":      detailed.OnlineStatus,
		"badgeRoles":        badgeRolesForMap(detailed.BadgeRoles),
		// UserDetailed
		"bannerUrl":      detailed.BannerURL,
		"bannerBlurhash": detailed.BannerBlurhash,
		"isLocked":       detailed.IsLocked,
		"isSilenced":     false,
		"isSuspended":    detailed.IsSuspended,
		"description":    detailed.Description,
		"location":       detailed.Location,
		"birthday":       detailed.Birthday,
		"lang":           detailed.Lang,
		"fields":         detailed.Fields,
		"verifiedLinks":  detailed.VerifiedLinks,
		"followersCount": detailed.FollowersCount,
		"followingCount": detailed.FollowingCount,
		"notesCount":     detailed.NotesCount,
		"uri":            detailed.URI,
		"url":            detailed.URL,
		"movedTo":        u.MovedToURI,
		"alsoKnownAs":    u.AlsoKnownAs,
		"updatedAt":      detailed.UpdatedAt,
		"lastFetchedAt":  nil,
		// MeDetailed (新規ユーザーなのでゼロ値が多い)
		"avatarId":                        u.AvatarID,
		"bannerId":                        u.BannerID,
		"followersVisibility":             detailed.FollowersVisibility,
		"followingVisibility":             detailed.FollowingVisibility,
		"chatScope":                       u.ChatScope,
		"canChat":                         true,
		"followedMessage":                 nil,
		"memo":                            nil,
		"moderationNote":                  nil,
		"hideOnlineStatus":                u.HideOnlineStatus,
		"isAdmin":                         isAdmin,
		"isModerator":                     isMod,
		"isDeleted":                       u.IsDeleted,
		"isExplorable":                    u.IsExplorable,
		"hasUnreadNotification":           false,
		"hasPendingReceivedFollowRequest": false,
		"hasUnreadAnnouncement":           false,
		"hasUnreadAntenna":                false,
		"hasUnreadChannel":                false,
		"hasUnreadMentions":               false,
		"hasUnreadSpecifiedNotes":         false,
		"hasUnreadChatMessages":           false,
		"unreadNotificationsCount":        0,
		"unreadAnnouncements":             []any{},
		"pinnedNoteIds":                   []string{},
		"pinnedNotes":                     []any{},
		"pinnedPageId":                    nil,
		"pinnedPage":                      nil,
		"policies":                        userPolicies,
		"roles":                           []any{},
		"securityKeysList":                []any{},
		"mutingNotificationTypes":         []any{},
		"notificationRecieveConfig":       map[string]any{},
		"emailNotificationTypes":          []string{"follow", "receiveFollowRequest"},
		"twoFactorEnabled":                false,
		"usePasswordLessLogin":            false,
		"securityKeys":                    false,
		"twoFactorBackupCodesStock":       "none",
		"autoAcceptFollowed":              true,
		"noCrawle":                        false,
		"preventAiLearning":               true,
		"alwaysMarkNsfw":                  false,
		"autoSensitive":                   false,
		"carefulBot":                      false,
		"injectFeaturedNote":              true,
		"receiveAnnouncementEmail":        true,
		"publicReactions":                 true,
		"loggedInDays":                    0,
		"achievements":                    []any{},
		// token (MeDetailed にない追加フィールド)
		"token": result.Token,
	}

	// createdAt は ID から復元
	if s, err := aidxCreatedAtString(h.idGen, u.ID); err == nil {
		out["createdAt"] = s
	}

	return c.JSON(http.StatusOK, out)
}

// ShowUser handles POST /api/admin/show-user.
func (h *Handler) ShowUser(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "userId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	user, err := h.userRepo.FindByID(req.UserID)
	if err != nil {
		// **DB 障害を「そんな利用者は居ない」にしない** (#2792)。障害を 404 で
		// 返すと「対象が消えた」と読めてしまい、監視でも 5xx が立たない。
		if !repository.IsNotFound(err) {
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		return c.JSON(http.StatusNotFound, apierr.Error("NO_SUCH_USER", "No such user.", "2b730f78-1179-461b-88ad-d24c9af1a5ce"))
	}

	// 非 administrator (= moderator) は他 administrator の情報を閲覧できない
	// (upstream show-user.ts:223-226 の 'cannot show info of admin' guard)。
	//
	// **対象側は判定できないときに通さない (#3037 レビュー 2 周目)。**
	// `IsAdministrator` は判定できないときに false を返すので、素で使うと
	// `ListByUser` が一時的に失敗する窓で**管理者の email / 2FA / サインイン
	// 履歴が見える** (#2792)。実行者側は false = 「管理者ではない」に倒れる
	// ので、そちらは元から安全側。
	me := middleware.GetUser(c)
	if h.roleService != nil {
		if me != nil && !h.roleService.IsAdministrator(me.ID) {
			// **root も同じ理由で `targetIsRoot` を通す (レビュー 3 周目)。**
			switch isRoot, undet := h.targetIsRoot(user); {
			case undet:
				return apierr.JSONInternalError(c)
			case isRoot:
				return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Cannot show info of admin.", "0d4e3a3e-2c1f-4d8b-9d2a-7a0c1c1b2f3a"))
			}
			targetAdmin, _, err := h.roleService.RolePrivileges(user.ID)
			if err != nil {
				slog.Error("admin/show-user: cannot determine the target's privileges", "userId", user.ID, "err", err)
				return apierr.JSONInternalError(c)
			}
			if targetAdmin {
				return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Cannot show info of admin.", "0d4e3a3e-2c1f-4d8b-9d2a-7a0c1c1b2f3a"))
			}
		}
	}

	profile, _ := h.userRepo.FindProfileByUserID(user.ID)

	// **IP を出してよいのは `canSearchIpHistory` を持つ相手だけ** (#3114)。
	// `HasRolePolicy` は管理者を短絡するので、既定 (policy false) の構成では
	// 管理者だけが通る = `admin/ip/*` と同じ条件になる。
	//
	// **roleService 未配線なら伏せる側へ倒す** (fail-closed。この packer の
	// 他の field と同じ方針)。
	showIPs := h.roleService != nil && me != nil &&
		h.roleService.HasRolePolicy(me.ID, role.PolicyCanSearchIPHistory)
	if h.roleService == nil {
		// **黙って伏せない。** 配線が落ちると IP が永久に空になるだけで
		// 誰も気付けない (`middleware/role_policy.go` も同じ状況で鳴らす)。
		slog.Error("admin/show-user: roleService is not wired; signin IPs are withheld")
	}

	resp := h.packAdminUser(user, profile, showIPs)
	// **内部連絡用のキーは wire に出さない。** `signins` を引けたかどうかは
	// 監査の判断にだけ使う。
	signinsOK, _ := resp[signinsLoadedKey].(bool)
	delete(resp, signinsLoadedKey)
	if showIPs {
		// **実際に IP を返したときだけ記録する** (#3114 / #3106)。伏せた応答も、
		// **引けずに空になった応答も**開示が起きていないので残さない — 記録すると
		// 「本当に 0 件だった」と「DB が落ちていて何も返していない」が
		// `ip_lookup_log` 上で区別できなくなる (`admin/get-user-ips` と同じ判断)。
		if signinsOK && me != nil && h.ipLookupAudit != nil {
			signins, _ := resp["signins"].([]map[string]any)
			h.ipLookupAudit.Record(iplookuplog.Entry{
				UserID: me.ID, Kind: model.IPLookupKindSignins,
				TargetUserID: user.ID, ResultCount: len(signins),
			})
		}
		noStoreIPLookup(c)
	}
	return c.JSON(http.StatusOK, resp)
}

// ShowUsers handles POST /api/admin/show-users.
func (h *Handler) ShowUsers(c echo.Context) error {
	var req struct {
		State    string `json:"state"`
		Origin   string `json:"origin"`
		Hostname string `json:"hostname"`
		Username string `json:"username"`
		Sort     string `json:"sort"`
		Limit    *int   `json:"limit"`
		Offset   int    `json:"offset"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	users, err := h.userRepo.ListUsers(model.UserListFilter{
		State:    req.State,
		Origin:   req.Origin,
		Hostname: req.Hostname,
		Username: req.Username,
		Sort:     req.Sort,
		Limit:    limit,
		Offset:   req.Offset,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// Profile を per-row FindProfileByUserID で引いていた N+1 (#300 1-4) を
	// FindProfilesByUserIDs (#503 で追加済) の 1 batch query に置換。
	ids := make([]string, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.ID)
	}
	profiles, err := h.userRepo.FindProfilesByUserIDs(ids)
	if err != nil {
		// Profile fetch 失敗は ShowUsers 全体の致命扱いではないので、空 map で
		// fallback する (各 user は profile=nil で pack される)。
		profiles = nil
	}
	profileByUser := make(map[string]*model.UserProfile, len(profiles))
	for _, p := range profiles {
		profileByUser[p.UserID] = p
	}

	// upstream show-users.ts:117 は packMany(users, me, {schema:'UserDetailed'})
	// で UserDetailed を返す。admin detail packer (packAdminUser) を使うと
	// email / signins / roleAssigns / notificationRecieveConfig といった admin
	// 専用 field が一覧経由で漏れ、show-user の admin guard を迂回できてしまう
	// (moderator が admin の連絡先や signin IP を取得可能)。一覧は UserDetailed
	// に揃えて過剰露出を防ぐ。
	result := make([]entity.UserDetailed, 0, len(users))
	for _, u := range users {
		d := entity.PackUserDetailed(u, profileByUser[u.ID], h.idGen)
		// **モデレーターにはカウントを見せる** (upstream
		// `UserEntityService.pack` は `isMe || iAmModerator` に実数を返す)。
		// packer は既定で伏せるので、ここでゲートを通さないと 0 になる。
		entity.GateCountVisibility(&d, false, true, false)
		result = append(result, d)
	}
	return c.JSON(http.StatusOK, result)
}

// badgeRolesForMap returns badgeRoles for admin map-based responses. The packer
// now returns *[]any (nil = absent for remote users with
// showRoleBadgesOfRemoteUsers off, #1653), but admin endpoints serialise via
// map[string]any which ignores omitempty — a nil pointer would emit
// schema-invalid `null` (UserLite.badgeRoles is nullable:false). Coerce nil to
// `[]` so the admin response keeps its previous `[]` shape.
func badgeRolesForMap(br *[]any) []any {
	if br == nil {
		return []any{}
	}
	return *br
}

// signinsLoadedKey は packAdminUser が「signins を引けたか」を呼び出し元へ
// 渡すための内部キー。**wire へ出す前に必ず delete する。**
const signinsLoadedKey = "__signinsLoaded"

// packAdminUser returns a MeDetailed-equivalent response for admin endpoints.
func (h *Handler) packAdminUser(u *model.User, profile *model.UserProfile, showIPs bool) map[string]any {
	// upstream admin/show-user (show-user.ts:233-261) が返すのはこの 24 key
	// だけで、UserLite / UserDetailed / MeDetailed は含まない。旧実装は
	// PackUserDetailed をベースに 70 key 近くを返しており、id をはじめ
	// upstream に無い field を多数含んでいた (#2287)。frontend の
	// admin-user.vue は users/show の結果を別途持っていて、こちらからは
	// email / lastActiveDate / policies / roleAssigns / roles / isModerator /
	// isSilenced / isSuspended / moderationNote しか読まないので、upstream の
	// key 集合に絞っても UI は壊れない。
	resp := map[string]any{
		"isSuspended":  u.IsSuspended,
		"isHibernated": u.IsHibernated,
		// roleService 未配線時は false / 既定 policy に倒す (fail-closed)。
		"isModerator": false,
		"isSilenced":  false,
		"policies":    role.DefaultPolicies(),
		"roles":       []any{},
		// profile 未取得時でも key は必ず生やす (upstream は必ず返す)。
		"email":                     nil,
		"emailVerified":             false,
		"followedMessage":           nil,
		"autoAcceptFollowed":        false,
		"noCrawle":                  false,
		"preventAiLearning":         false,
		"alwaysMarkNsfw":            false,
		"autoSensitive":             false,
		"carefulBot":                false,
		"injectFeaturedNote":        false,
		"receiveAnnouncementEmail":  false,
		"mutedWords":                []any{},
		"mutedInstances":            []any{},
		"notificationRecieveConfig": map[string]any{},
		"moderationNote":            "",
	}
	if profile != nil {
		resp["email"] = profile.Email
		resp["emailVerified"] = profile.EmailVerified
		resp["autoAcceptFollowed"] = profile.AutoAcceptFollowed
		resp["noCrawle"] = profile.NoCrawle
		resp["preventAiLearning"] = profile.PreventAiLearning
		resp["alwaysMarkNsfw"] = profile.AlwaysMarkNsfw
		resp["autoSensitive"] = profile.AutoSensitive
		resp["carefulBot"] = profile.CarefulBot
		resp["injectFeaturedNote"] = profile.InjectFeaturedNote
		resp["receiveAnnouncementEmail"] = profile.ReceiveAnnouncementEmail
		resp["mutedWords"] = profile.MutedWords
		resp["mutedInstances"] = profile.MutedInstances
		resp["followedMessage"] = profile.FollowedMessage
		if profile.ModerationNote != nil {
			resp["moderationNote"] = *profile.ModerationNote
		}
		resp["notificationRecieveConfig"] = metaJSONValue(profile.NotificationRecieveConfig, map[string]any{})
	}
	if h.roleService != nil {
		resp["isModerator"] = h.roleService.IsModerator(u.ID)
		// isSilenced は role policy 由来 (canPublicNote を否定する role を持つか)。
		resp["isSilenced"] = h.roleService.IsSilenced(u.ID)
		// GetUserRoles は expired 除外済みの active role list。err 時は
		// frontend の `.map(...)` が例外を吐かないよう空配列に倒して観測する。
		if userRoles, rerr := h.roleService.GetUserRoles(u.ID); rerr == nil {
			resp["roles"] = userRoles
		} else {
			slog.Warn("admin/show-user: failed to load roles", "userId", u.ID, "err", rerr)
			resp["roles"] = []any{}
		}
		resp["policies"] = h.roleService.GetUserPolicies(u.ID)
	}
	// signins / roleAssigns は repo / service 未配線や lookup 失敗時も
	// 空配列に fallback する (roles と同じ扱い、#888 / #1198)。
	signins, signinsOK := h.packUserSignins(u.ID, showIPs)
	resp["signins"] = signins
	resp[signinsLoadedKey] = signinsOK
	resp["roleAssigns"] = h.packUserRoleAssigns(u.ID)
	if u.LastActiveDate != nil {
		resp["lastActiveDate"] = u.LastActiveDate.UTC().Format("2006-01-02T15:04:05.000Z")
	} else {
		resp["lastActiveDate"] = nil
	}
	return resp
}

// packUserSignins returns the user's signin history in the admin/show-user
// `signins` shape. Upstream returns every row (signinsRepository.findBy);
// we pass limit=-1 so GORM omits the LIMIT clause and matches that. Rows come
// back newest-first (ListByUserID's default order); upstream leaves the order
// unspecified so this is a benign deviation. Returns an empty slice (never
// nil) so the JSON field is always `[]` for callers without a wired signin
// repository or on lookup failure.
//
// **showIPs が false なら `ip` を伏せる (#3114)。** ここは `signin` テーブルの
// ログイン IP をそのまま返す口で、**`admin/ip/*` が要求する `canSearchIpHistory`
// を通らない**。#3104 が「IP とアカウントの対応は既定でモデレーターに開かない」
// と決めた以上、同じ種類の情報をモデレーター権限だけで全件返すのは、mk-go が
// 自分で作った権限境界と食い違う。**upstream からの意図的な逸脱**
// (docs/divergence.md §7)。
//
// **空文字にする。** `Signin` の json-schema は `ip` を
// `optional: false, nullable: false` と宣言しているので、key を消すことも
// null にすることもできない。
func (h *Handler) packUserSignins(userID string, showIPs bool) ([]map[string]any, bool) {
	out := []map[string]any{}
	if h.signinRepo == nil {
		return out, false
	}
	signins, err := h.signinRepo.ListByUserID(userID, -1, "", "")
	if err != nil {
		slog.Warn("admin/show-user: failed to load signins", "userId", userID, "err", err)
		return out, false
	}
	for _, s := range signins {
		packed := entity.PackSignin(s, h.idGen)
		if packed == nil {
			continue
		}
		if !showIPs {
			// **`PackSignin` 側は触らない。** あれは本人向けの main stream の
			// `signin` イベントでも使う (`api/signin/handler.go`)。自分の IP を
			// 自分が見るのは正当なので、伏せるのは admin の経路だけ。
			packed["ip"] = ""
			// **`headers` も伏せる。** ここには保存時の HTTP header がそのまま
			// 入っており、**本番構成の nginx が必ず付ける `X-Real-IP` /
			// `X-Forwarded-For`** が含まれる (`deploy/uds/nginx/mkgo.conf`)。
			// `ip` だけ潰しても**キー 1 つ隣で同じ IP が読める**ので、伏せる
			// 意味が無くなる (敵対的レビューで実測)。`sanitizeHeaders` が落とす
			// のは `Authorization` / `Cookie` / `Set-Cookie` の 3 つだけ。
			//
			// golden schema は `headers` を `type: other` / 非 null / 必須と
			// 宣言しているので、`ip` と同じく**空の object** に置き換える。
			packed["headers"] = map[string]any{}
		}
		out = append(out, packed)
	}
	return out, true
}

// packUserRoleAssigns returns the user's active role assignments in the
// admin/show-user `roleAssigns` shape ({createdAt, expiresAt, roleId}).
// createdAt is derived from the aidx-encoded assignment ID. Mirrors upstream
// which maps roleService.getUserAssigns (expired assignments excluded).
func (h *Handler) packUserRoleAssigns(userID string) []map[string]any {
	out := []map[string]any{}
	if h.roleService == nil {
		return out
	}
	assigns, err := h.roleService.GetUserAssigns(userID)
	if err != nil {
		slog.Warn("admin/show-user: failed to load role assignments", "userId", userID, "err", err)
		return out
	}
	const tsFormat = "2006-01-02T15:04:05.000Z"
	for _, a := range assigns {
		entry := map[string]any{
			"roleId":    a.RoleID,
			"expiresAt": nil,
		}
		if t, perr := h.idGen.ParseTime(a.ID); perr == nil {
			entry["createdAt"] = t.UTC().Format(tsFormat)
		}
		if a.ExpiresAt != nil {
			entry["expiresAt"] = a.ExpiresAt.UTC().Format(tsFormat)
		}
		out = append(out, entry)
	}
	return out
}

// SuspendUser handles POST /api/admin/suspend-user.
func (h *Handler) SuspendUser(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "userId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	user, err := h.userRepo.FindByID(req.UserID)
	if err != nil {
		// **DB 障害を「そんな利用者は居ない」にしない** (#2792)。障害を 404 で
		// 返すと「対象が消えた」と読めてしまい、監視でも 5xx が立たない。
		if !repository.IsNotFound(err) {
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		return c.JSON(http.StatusNotFound, apierr.Error("NO_SUCH_USER", "No such user.", "2b730f78-1179-461b-88ad-d24c9af1a5ce"))
	}

	// upstream suspend-user.ts: モデレーター/管理者 (root を含む) アカウントは
	// 凍結できない。moderator が他の moderator/admin を凍結する権限昇格を防ぐ。
	//
	// **判定できないときは通さない (#3037 レビュー 2 周目)。** `IsModerator` は
	// 判定できないときに false を返すので、素で使うと `ListByUser` が一時的に
	// 失敗する窓で**他のモデレーターを凍結できる** (#2792)。
	//
	// **root は `targetIsRoot` で見る (#3037 レビュー 3 周目)。** `user.IsRoot`
	// だけでは足りない — `isRoot` 列が入った migration より前に作られた root は
	// `false` のままで、**本番の root がまさにそれ**。`RolePrivileges` が内部で
	// 呼ぶ `isRootUser` は meta の失敗を握り潰して `isRoot` へ落ちるので、
	// meta を読めない窓では `(false, false, nil)` = 「特権なし」を返し、
	// **モデレーターが root を凍結できた**。資格情報のリセット側と同じ判定へ揃える。
	switch isRoot, undet := h.targetIsRoot(user); {
	case undet:
		return apierr.JSONInternalError(c)
	case isRoot:
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Cannot suspend a moderator account.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
	}
	if h.roleService != nil {
		_, targetMod, err := h.roleService.RolePrivileges(user.ID)
		if err != nil {
			slog.Error("admin/suspend-user: cannot determine the target's privileges", "userId", user.ID, "err", err)
			return apierr.JSONInternalError(c)
		}
		if targetMod {
			return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Cannot suspend a moderator account.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
		}
	}
	// **system アカウントは凍結させない。**
	//
	// `instance.actor` / `relay.actor` / `proxy.actor` を凍結すると、下の
	// `OnUserDeleted` が**既知の全リモート inbox へ `Delete(actor)` を配る**。
	// system アカウントはロールを持たないので `RolePrivileges` は
	// `(false, false, nil)` を返し、上の判定を素通りしていた。
	//
	// 削除経路 (`isProtectedAccount`) と資格情報リセット経路
	// (`isSystemAccountUser`) は既に塞いであり、**凍結経路だけが残っていた**。
	if isSystemAccountUser(user) {
		return c.JSON(http.StatusBadRequest,
			apierr.Error("ACCESS_DENIED", "Cannot suspend a system account.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
	}

	if err := h.userRepo.UpdateUser(req.UserID, map[string]any{"isSuspended": true}); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// **モデレーターの判断を local として刻む** (#2973)。これが無いと、リモート
	// actor が `toot:suspended` を下ろした時点で凍結が解除されてしまう。
	h.recordLocalSuspensionOrigin(req.UserID)
	// 凍結直後の auth bypass 防止 (#965)。target の全 token cache entry を
	// 即時削除し、middleware 通過後の P2 gate (#964) に依存せず確実に弾く。
	h.invalidateUserTokenCache(req.UserID)
	// 既に張られている WebSocket も閉じる (mk-go 独自、docs/divergence.md)。
	h.revokeUserStreams(req.UserID)
	// upstream UserSuspendService.suspend: local user なら全 sharedInbox へ
	// Delete(actor) を配信する (#1759)。best-effort (queue 経由)。
	if h.userModerationFed != nil {
		h.userModerationFed.OnUserDeleted(user)
	}
	// upstream postSuspend + unFollowAll: 双方向 followRequest 削除 + outgoing
	// follow の全 unfollow (#1759)。best-effort。
	h.cleanupSuspendedUserRelations(req.UserID)
	h.logUserActionWithUser(c, moderationlog.LogSuspend, user)
	return c.NoContent(http.StatusNoContent)
}

// UnsuspendUser handles POST /api/admin/unsuspend-user.
func (h *Handler) UnsuspendUser(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "userId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	user, err := h.userRepo.FindByID(req.UserID)
	if err != nil {
		// **DB 障害を「そんな利用者は居ない」にしない** (#2792)。障害を 404 で
		// 返すと「対象が消えた」と読めてしまい、監視でも 5xx が立たない。
		if !repository.IsNotFound(err) {
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		return c.JSON(http.StatusNotFound, apierr.Error("NO_SUCH_USER", "No such user.", "2b730f78-1179-461b-88ad-d24c9af1a5ce"))
	}

	if err := h.userRepo.UpdateUser(req.UserID, map[string]any{"isSuspended": false}); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// **解除も local として刻む** (#2973)。これが無いと、発信元が
	// `toot:suspended` を立て続けている限り次の actor refresh で無言で戻る。
	//
	// **ここは失敗を 500 で返す。** 凍結側と違い、記録できないまま解除すると
	// 「解除したはずが戻っている」という、この issue が塞ぎに来た状態そのものに
	// なる。resolver 側が「記録できないなら触らない」に倒しているのと揃える。
	if h.suspensionOriginRepo != nil {
		if err := h.suspensionOriginRepo.Set(req.UserID, model.SuspensionOriginLocal); err != nil {
			slog.Warn("admin: failed to record unsuspension origin", "userId", req.UserID, "err", err)
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
	}
	// 凍結解除直後に target の全 token cache entry を invalidate する (#965)。
	// cache 内に isSuspended=true な stale user が残っていると middleware の
	// P2 gate (#964) が cache hit 経路でも fire してしまい、解除済 user が
	// 認証通らない逆方向の bug になる。
	h.invalidateUserTokenCache(req.UserID)
	// upstream UserSuspendService.unsuspend: local user なら全 sharedInbox へ
	// Undo(Delete) を配信する (#1759)。
	if h.userModerationFed != nil {
		h.userModerationFed.OnUserRestored(user)
	}
	h.logUserActionWithUser(c, moderationlog.LogUnsuspend, user)
	return c.NoContent(http.StatusNoContent)
}

// fetchProxyAccountID returns the proxy system user ID for inclusion in the
// admin/meta response. fetcher 未配線 / 取得失敗時は nil を返すが、その場合
// frontend settings.vue の `users/show` 呼び出しが400で落ちて画面が真っ白
// になるため production では必ず wire しておくこと。
func (h *Handler) fetchProxyAccountID() any {
	if h.systemAccountFetcher == nil {
		return nil
	}
	user, err := h.systemAccountFetcher.Fetch("proxy")
	if err != nil || user == nil {
		return nil
	}
	return user.ID
}

// metaJSONValue decodes a jsonb meta column (clientOptions /
// deliverSuspendedSoftware) into a generic value for the admin/meta response.
// Empty or invalid JSON falls back to the provided default so the frontend
// always sees the expected object/array shape (upstream returns the raw column
// which defaults to '{}' / '[]').
func metaJSONValue(raw []byte, fallback any) any {
	if len(raw) == 0 {
		return fallback
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return fallback
	}
	return out
}

// AdminMeta handles POST /api/admin/meta.
func (h *Handler) AdminMeta(c echo.Context) error {
	m, err := h.metaRepo.Fetch()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	resp := map[string]any{
		// Basic
		"maintainerName": m.MaintainerName, "maintainerEmail": m.MaintainerEmail,
		"version": config.MisskeyVersion, "uri": h.serverURL,
		"name": m.Name, "shortName": m.ShortName, "description": m.Description,
		"langs": m.Langs, "pinnedUsers": m.PinnedUsers,
		"hiddenTags": m.HiddenTags, "blockedHosts": m.BlockedHosts,
		"silencedHosts": m.SilencedHosts, "sensitiveWords": m.SensitiveWords,
		"prohibitedWords": m.ProhibitedWords,
		"themeColor":      m.ThemeColor, "bannerUrl": m.BannerURL,
		"backgroundImageUrl": m.BackgroundImageURL, "logoImageUrl": m.LogoImageURL,
		"iconUrl":       m.IconURL,
		"app192IconUrl": m.App192IconURL, "app512IconUrl": m.App512IconURL,
		"defaultLightTheme": m.DefaultLightTheme, "defaultDarkTheme": m.DefaultDarkTheme,
		"disableRegistration":       m.DisableRegistration,
		"emailRequiredForSignup":    m.EmailRequiredForSignup,
		"approvalRequiredForSignup": m.ApprovalRequiredForSignup,
		"signupApplicationForm":     m.SignupApplicationForm,
		// Cache
		"cacheRemoteFiles":          m.CacheRemoteFiles,
		"cacheRemoteSensitiveFiles": m.CacheRemoteSensitiveFiles,
		// Captcha
		"enableHcaptcha": m.EnableHcaptcha, "hcaptchaSiteKey": m.HcaptchaSiteKey, "hcaptchaSecretKey": m.HcaptchaSecretKey,
		"enableRecaptcha": m.EnableRecaptcha, "recaptchaSiteKey": m.RecaptchaSiteKey, "recaptchaSecretKey": m.RecaptchaSecretKey,
		"enableTurnstile": m.EnableTurnstile, "turnstileSiteKey": m.TurnstileSiteKey, "turnstileSecretKey": m.TurnstileSecretKey,
		"enableMcaptcha": m.EnableMcaptcha, "mcaptchaSiteKey": m.McaptchaSiteKey, "mcaptchaSecretKey": m.McaptchaSecretKey, "mcaptchaInstanceUrl": m.McaptchaInstanceURL,
		"enableTestcaptcha": m.EnableTestcaptcha,
		// Email
		"enableEmail": m.EnableEmail, "email": m.Email,
		"smtpHost": m.SMTPHost, "smtpPort": m.SMTPPort,
		"smtpUser": m.SMTPUser, "smtpPass": m.SMTPPass, "smtpSecure": m.SMTPSecure,
		// Service Worker
		"enableServiceWorker": m.EnableServiceWorker,
		"swPublickey":         m.SwPublicKey, "swPrivateKey": m.SwPrivateKey,
		// Object Storage
		"useObjectStorage":              m.UseObjectStorage,
		"objectStorageBucket":           m.ObjectStorageBucket,
		"objectStoragePrefix":           m.ObjectStoragePrefix,
		"objectStorageBaseUrl":          m.ObjectStorageBaseURL,
		"objectStorageEndpoint":         m.ObjectStorageEndpoint,
		"objectStorageRegion":           m.ObjectStorageRegion,
		"objectStoragePort":             m.ObjectStoragePort,
		"objectStorageUseSSL":           m.ObjectStorageUseSSL,
		"objectStorageUseProxy":         m.ObjectStorageUseProxy,
		"objectStorageSetPublicRead":    m.ObjectStorageSetPublicRead,
		"objectStorageS3ForcePathStyle": m.ObjectStorageS3ForcePathStyle,
		"objectStorageAccessKey":        m.ObjectStorageAccessKey,
		"objectStorageSecretKey":        m.ObjectStorageSecretKey,
		// Chunked upload (#2313)。mk-go 独自。オブジェクトストレージ設定画面に
		// 出すので object storage 群と同じブロックに置く。
		"chunkedUploadEnabled":             m.ChunkedUploadEnabled,
		"chunkedUploadChunkSizeMb":         m.ChunkedUploadChunkSizeMb,
		"chunkedUploadSessionTtlMinutes":   m.ChunkedUploadSessionTTLMinutes,
		"chunkedUploadMaxSessionsPerUser":  m.ChunkedUploadMaxSessionsPerUser,
		"chunkedUploadMaxPendingMbPerUser": m.ChunkedUploadMaxPendingMbPerUser,
		// リレー経由投稿の揮発化 (#2332)。mk-go 独自。コントロールパネル →
		// リレー に出すので relay 設定と同じ画面で扱う (#2335)。
		"enableRelayOrphanUserCleanup": m.EnableRelayOrphanUserCleanup,
		"relayOrphanUserGraceDays":     m.RelayOrphanUserGraceDays,
		"enableEphemeralRelayNotes":    m.EnableEphemeralRelayNotes,
		"ephemeralRelayNoteTtlMinutes": m.EphemeralRelayNoteTTLMinutes,
		// URLs
		"tosUrl": m.TermsOfServiceURL, "repositoryUrl": m.RepositoryURL,
		"feedbackUrl": m.FeedbackURL, "impressumUrl": m.ImpressumURL,
		"privacyPolicyUrl": m.PrivacyPolicyURL, "inquiryUrl": m.InquiryURL,
		// Federation
		"federation": m.Federation, "federationHosts": m.FederationHosts,
		"enableFanoutTimeline":           m.EnableFanoutTimeline,
		"enableFanoutTimelineDbFallback": m.EnableFanoutTimelineDBFallback,
		"proxyRemoteFiles":               m.ProxyRemoteFiles,
		"signToActivityPubGet":           m.SignToActivityPubGet,
		// Policies (upstream は { ...DEFAULT_POLICIES, ...instance.policies })
		"policies": role.MergeMetaPolicies(m.Policies),
		// Moderation
		"sensitiveMediaDetection":                m.SensitiveMediaDetection,
		"sensitiveMediaDetectionSensitivity":     m.SensitiveMediaDetectionSensitivity,
		"setSensitiveFlagAutomatically":          m.SetSensitiveFlagAutomatically,
		"enableSensitiveMediaDetectionForVideos": m.EnableSensitiveMediaDetectionForVideos,
		// 公式 sensitive-detector 接続設定 (upstream 2026.7.0 #17570)。
		"sensitiveMediaDetectionApiUrl":              m.SensitiveMediaDetectionAPIURL,
		"sensitiveMediaDetectionApiKey":              m.SensitiveMediaDetectionAPIKey,
		"sensitiveMediaDetectionTimeout":             m.SensitiveMediaDetectionTimeout,
		"sensitiveMediaDetectionMaxImagesPerRequest": m.SensitiveMediaDetectionMaxImagesPerRequest,
		"enableIpLogging":                            m.EnableIPLogging,
		"enableActiveEmailValidation":                m.EnableActiveEmailValidation,
		// Feature flags
		"enableChartsForRemoteUser":         m.EnableChartsForRemoteUser,
		"enableChartsForFederatedInstances": m.EnableChartsForFederatedInstances,
		"enableStatsForFederatedInstances":  m.EnableStatsForFederatedInstances,
		"enableServerMachineStats":          m.EnableServerMachineStats,
		"enableIdenticonGeneration":         m.EnableIdenticonGeneration,
		"enableReactionsBuffering":          m.EnableReactionsBuffering,
		"enableRemoteNotesCleaning":         m.EnableRemoteNotesCleaning,
		"enableVerifymailApi":               m.EnableVerifymailAPI,
		"enableTruemailApi":                 m.EnableTruemailAPI,
		"showRoleBadgesOfRemoteUsers":       m.ShowRoleBadgesOfRemoteUsers,
		"singleUserMode":                    m.SingleUserMode,
		"allowExternalApRedirect":           m.AllowExternalApRedirect,
		// Images
		"serverErrorImageUrl": m.ServerErrorImageURL, "notFoundImageUrl": m.NotFoundImageURL,
		"infoImageUrl": m.InfoImageURL, "mascotImageUrl": m.MascotImageURL,
		// Misc
		"translatorAvailable": m.DeeplAuthKey != nil,
		"notesPerOneAd":       m.NotesPerOneAd,
		"clientOptions":       metaJSONValue(m.ClientOptions, map[string]any{}),
		"deeplAuthKey":        m.DeeplAuthKey, "deeplIsPro": m.DeeplIsPro,
		"googleAnalyticsMeasurementId": m.GoogleAnalyticsMeasurementID,
		"manifestJsonOverride":         m.ManifestJSONOverride,
		"bannedEmailDomains":           m.BannedEmailDomains,
		"mediaSilencedHosts":           m.MediaSilencedHosts,
		"preservedUsernames":           m.PreservedUsernames,
		"minimumUsernameLength":        m.MinimumUsernameLength,
		"prohibitedWordsForNameOfUser": m.ProhibitedWordsForNameOfUser,
		"deliverSuspendedSoftware":     metaJSONValue(m.DeliverSuspendedSoftware, []any{}),
		"verifymailAuthKey":            m.VerifymailAuthKey, "truemailAuthKey": m.TruemailAuthKey, "truemailInstance": m.TruemailInstance,
		// proxyAccountId は frontend admin/settings 画面が読み込み時に
		// users/show でこの ID を引くため、必ず非空でなければ画面が
		// 真っ白になる (#348)。本家 SystemAccountService.fetch('proxy')
		// と同じく lazy 作成して埋める。
		"proxyAccountId": h.fetchProxyAccountID(),
		// URL Preview
		"urlPreviewEnabled":              m.URLPreviewEnabled,
		"urlPreviewTimeout":              m.URLPreviewTimeout,
		"urlPreviewMaximumContentLength": m.URLPreviewMaximumContentLength,
		"urlPreviewRequireContentLength": m.URLPreviewRequireContentLength,
		"urlPreviewUserAgent":            m.URLPreviewUserAgent,
		"urlPreviewSummaryProxyUrl":      m.URLPreviewSummaryProxyURL,
		"urlPreviewAllowRedirect":        m.URLPreviewAllowRedirect,
		"urlPreviewSensitiveList":        m.URLPreviewSensitiveList,
		// upstream: summalyProxy は urlPreviewSummaryProxyUrl の別名で同値を返す
		"summalyProxy": m.URLPreviewSummaryProxyURL,
		// Timeline cache
		"perLocalUserUserTimelineCacheMax":  m.PerLocalUserUserTimelineCacheMax,
		"perRemoteUserUserTimelineCacheMax": m.PerRemoteUserUserTimelineCacheMax,
		"perUserHomeTimelineCacheMax":       m.PerUserHomeTimelineCacheMax,
		"perUserListTimelineCacheMax":       m.PerUserListTimelineCacheMax,
		// Remote notes cleaning
		"remoteNotesCleaningExpiryDaysForEachNotes":         m.RemoteNotesCleaningExpiryDaysForEachNotes,
		"remoteNotesCleaningMaxProcessingDurationInMinutes": m.RemoteNotesCleaningMaxProcessingDurationInMinutes,
		// Visitor
		"ugcVisibilityForVisitor": m.UgcVisibilityForVisitor,
	}
	return c.JSON(http.StatusOK, resp)
}

// UpdateMeta handles POST /api/admin/update-meta.
func (h *Handler) UpdateMeta(c echo.Context) error {
	var fields map[string]any
	if err := c.Bind(&fields); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// "i" フィールドを除外 (auth token)
	delete(fields, "i")
	// #2106 S1: identity / 専用 endpoint 管轄の column を generic map 経路から除外する。
	// update-meta は admin が任意 meta column を書ける generic passthrough 設計 (model
	// コメント参照) だが、rootUserId を書けると root 権限を別 user に付け替えられ
	// (admin→root 昇格)、id は singleton PK を壊す。proxyAccountId は
	// admin/update-proxy-account が管轄。upstream の paramDef もこれらを accept しない。
	// **alias 経由の書き込みもここで止まる。** `renameUpdateMetaFields` より
	// 前なので alias 名では消せないが、`dropUnknownMetaFields` が
	// `updateMetaProtectedColumns` を除いた集合で絞るため、rename の後で必ず
	// 落ちる。
	for _, protected := range updateMetaProtectedColumns {
		delete(fields, protected)
	}
	// upstream update-meta.ts の enum 制約を持つ field を pre-validate (#1108
	// sweep)。silent fallback で不正値が DB に書かれる drift を防ぐ:
	// sensitiveMediaDetection が "purple" のような不正値で保存されると
	// downstream consumer (media detection service) が default に倒れて
	// admin の意図と乖離する。federation / ugcVisibilityForVisitor は
	// drastic な network 遮断や匿名 view 不可化を引き起こすので、こちらは
	// silent corruption 防止としても重要。
	if err := validateUpdateMetaEnums(fields); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", err.Error(), "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if err := validateUpdateMetaNumericMinimums(fields); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", err.Error(), "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if err := validateUpdateMetaNumericRanges(fields); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", err.Error(), "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// 登録ゲートの型を揃える (#2803)。**normalizeSignupConditions より前に置く** —
	// 正規化は bool しか見ないのに、後段の UPDATE は bool 以外でも列を書き換える
	// ので、順番が逆だと「ゲートは外れたのに補正は走らない」組み合わせが残る。
	if err := normalizeSignupGateBools(fields); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", err.Error(), "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// 登録可否の組み合わせ検証 (#2565)。**更新後の状態**で判定するので、
	// 既存 meta とマージしてから見る。meta が引けないときは検証を諦めて
	// 素通しする (ここで 500 にすると、meta が壊れているときに設定を直す
	// 手段まで塞いでしまう)。
	currentMeta, _ := h.metaRepo.Fetch()
	normalizeSignupConditions(fields, currentMeta)
	// 申請フォームの定義を検証する (#2570)。**上限を置かないと管理者が自分で
	// 壊せる** — 項目を無制限に足せば申請ページが使い物にならなくなる。
	if err := validateSignupApplicationForm(fields); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", err.Error(), "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// frontend が送る API 名 → DB カラム名の差異を吸収する (#348)。API は
	// 本家互換の camelCase alias (tosUrl 等) を使うが、DB 側は
	// packages/backend/src/models/Meta.ts と同じ正規名で保持している。
	// alias が frontend から来たら DB カラム名に translate して渡す。
	renameUpdateMetaFields(fields)

	// **既定 policy の型をここでも見る (#3037 レビュー 2 周目)。**
	// `policies` は `roles/update-default-policies` と **同じ `meta.policies`
	// 列**を書く 4 本目の経路で (`metaJSONBColumns` のコメント参照)、
	// generic passthrough なので保護列でもなく `dropUnknownMetaFields` も
	// 通す。ここを素通しにすると、数値の policy に文字列を入れるだけで
	// **全利用者でその上限が消える** — consumer は
	// `if limit, ok := role.PolicyNumber(v); ok { ...gate... }` の形で読むので、
	// 上限違反で弾かれるのではなく上限そのものが無くなる (#2611 / #3037)。
	// rename の後に置くのは、alias で来た場合も正規名で見るため。
	if raw, ok := fields["policies"]; ok && raw != nil {
		// **`null` は従来どおり受ける (#3037 レビュー 3 周目)。**
		// `coerceMetaJSONBFields` が nil を `{}` に倒して列をリセットする。
		// 他の jsonb 列 (`clientOptions` / `deliverSuspendedSoftware`) も
		// null を受けるので、ここだけ 400 にすると非対称になる。
		policies, isMap := raw.(map[string]any)
		if !isMap {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM",
				"policies must be an object.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
		if key := invalidDefaultPolicyKey(policies); key != "" {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM",
				"policy "+key+" has a value of the wrong type.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
	}

	// upstream update-meta.ts の値正規化 (host lowercase/sort/dedup、空文字→null、
	// URL 検証、trim) を再現する。coerceMetaArrayFields より前に走らせ、host 系は
	// ここで model.StringArray 化まで済ませる (coerce 側は pass-through になる)。
	normalizeUpdateMetaFields(fields)

	// JSON Bind 後の []any{...} (中身は string) を model.StringArray に揃える
	// (#590)。GORM は map[string]any を Updates() に渡すと値の型をそのまま
	// driver に流すため、model.StringArray (= driver.Valuer 実装) に変換しない
	// と varchar[] 列の UPDATE が `expression is of type record` で失敗する。
	// これは federationHosts / blockedHosts / silencedHosts / langs 等
	// すべての varchar[] 列に影響していたバグで、admin が whitelist 連合や
	// host ブロックを保存しても永続化されない原因だった。
	coerceMetaArrayFields(fields)
	// clientOptions は upstream update-meta.ts と同じく既存値への shallow merge
	// (`{...serverSettings.clientOptions, ...ps.clientOptions}`) にする (#1851)。
	// replace だと partial update で未指定キーが消える。incoming が map なら既存
	// meta.clientOptions に上書きマージ、null (= map でない) なら incoming 無し
	// として既存を維持する (upstream の `{...existing, ...null}` と同じ)。coerce
	// 前に行い、merge 後の map を coerceMetaJSONBFields が datatypes.JSON 化する。
	if raw, ok := fields["clientOptions"]; ok {
		merged := map[string]any{}
		if m, err := h.metaRepo.Fetch(); err == nil && m != nil && len(m.ClientOptions) > 0 {
			_ = json.Unmarshal(m.ClientOptions, &merged)
		}
		if incoming, isMap := raw.(map[string]any); isMap {
			for k, v := range incoming {
				merged[k] = v
			}
		}
		fields["clientOptions"] = merged
	}
	// jsonb 列 (deliverSuspendedSoftware / policies / clientOptions) は decoded
	// []any / map を []byte に marshal しないと UPDATE が型不一致で失敗する
	// (#1732 / #1823 / #1846)。
	coerceMetaJSONBFields(fields)

	// VAPID 鍵 auto-generate (#492): Service Worker 有効化時に
	// swPublicKey / swPrivateKey が両方空のとき backend で生成して詰める。
	// 既存鍵 (片方だけ欠けの中途状態 含む) は触らないことで、運用者が
	// 外部生成した鍵を保持できるようにする。
	if err := h.maybeAutoGenerateVAPID(fields); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// before snapshot for moderation log。Misskey TS の updateServerSettings
	// info は full meta の before/after で SMTP secret 等もそのまま記録する
	// 仕様 (上流 update-meta.ts でも mask 無し)。互換性最優先で同じ挙動。
	//
	// 順序メモ: maybeAutoGenerateVAPID は fields map に鍵を inject するが
	// metaRepo の DB 状態は触らないので、ここで Fetch() しても VAPID 生成
	// 前後で値は変わらない。よって before lookup の位置は VAPID 生成の前
	// でも後でも結果は同じ。可読性のため Update 直前に置いている。
	// **列でないキーを落とす (#3037)。** キーはそのまま UPDATE の列識別子に
	// なるので、知らないキーが 1 つあると GORM が存在しない列を書こうとして
	// 500 になる。詳細は `dropUnknownMetaFields`。
	//
	// **変換が全部終わってから絞る。** alias の rename (`tosUrl`) も、
	// `summalyProxy` → `urlPreviewSummaryProxyUrl` の解決も、VAPID の注入も
	// 上で済んでいる。手前に置くと正当な入力まで落ちる (実測: `summalyProxy` の
	// alias テストが落ちた)。
	if dropped := dropUnknownMetaFields(fields); len(dropped) > 0 {
		slog.Warn("admin: update-meta ignored unknown fields", "fields", dropped)
	}

	beforeMeta, _ := h.metaRepo.Fetch()
	if err := h.metaRepo.Update(fields); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	if afterMeta, err := h.metaRepo.Fetch(); err == nil {
		h.logModeration(c, moderationlog.LogUpdateServerSettings, map[string]any{
			"before": beforeMeta,
			"after":  afterMeta,
		})
	}
	return c.NoContent(http.StatusNoContent)
}

// maybeAutoGenerateVAPID inspects the update-meta field map and, when
// Service Worker is being enabled (or is already enabled) and both the
// public and private VAPID keys would end up empty, generates a fresh
// key pair and injects it into fields. Skips key generation when:
//   - SW is being disabled or is already disabled (no-op)
//   - at least one key is already set (respects operator-supplied keys)
func (h *Handler) maybeAutoGenerateVAPID(fields map[string]any) error {
	current, err := h.metaRepo.Fetch()
	if err != nil {
		return err
	}
	enable := metaBoolAfterUpdate(fields, "enableServiceWorker", current.EnableServiceWorker)
	if !enable {
		return nil
	}
	pub := metaStringAfterUpdate(fields, "swPublicKey", strDeref(current.SwPublicKey))
	priv := metaStringAfterUpdate(fields, "swPrivateKey", strDeref(current.SwPrivateKey))
	if pub != "" || priv != "" {
		return nil
	}
	newPub, newPriv, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return err
	}
	fields["swPublicKey"] = newPub
	fields["swPrivateKey"] = newPriv
	return nil
}

// metaBoolAfterUpdate returns the effective bool value of key after the
// update would be applied — incoming value if present and bool-typed,
// otherwise the current DB value.
func metaBoolAfterUpdate(fields map[string]any, key string, current bool) bool {
	if v, ok := fields[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return current
}

// metaBoolExplicit reports whether the update carries a usable bool for key.
//
// bool 以外は「明示した」と扱わない。update-meta の経路では
// normalizeSignupGateBools が先に null を落とし残りを 400 で弾くので、ここへ来るのは
// bool だけだが、
// **presence だけの判定にすると、その検査を緩めたときに壊れた値を明示扱いして
// 補正を飛ばす** (= 登録が開いたまま残る) 側に倒れる。閉じる側の既定へ倒す。
func metaBoolExplicit(fields map[string]any, key string) bool {
	v, ok := fields[key]
	if !ok {
		return false
	}
	_, isBool := v.(bool)
	return isBool
}

// metaStringAfterUpdate returns the effective string value of key after the
// update would be applied. Empty string, JSON null, and absence are all
// treated as "empty" so that an admin clearing a key with null still
// triggers the auto-generate path.
func metaStringAfterUpdate(fields map[string]any, key, current string) string {
	if v, ok := fields[key]; ok {
		switch s := v.(type) {
		case string:
			return s
		case nil:
			// JSON null は明示的に空にしたい意図と解釈する
			return ""
		}
	}
	return current
}

func strDeref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// updateMetaFieldAliases maps frontend API names to DB column names for
// update-meta. 本家 Misskey の admin/meta.ts / update-meta.ts は一部
// field で alias を使っており、mk-go の AdminMeta も同じ alias で公開
// しているため、save path にも逆向きの translate を入れる必要がある。
var updateMetaFieldAliases = map[string]string{
	"tosUrl":      "termsOfServiceUrl", // 本家: update-meta.ts の termsOfServiceUrl が admin/meta では tosUrl で出る
	"swPublickey": "swPublicKey",
	// API param は mcaptchaSiteKey (capital K) だが DB 列は mcaptchaSitekey
	// (lower k)。alias しないと存在しない列への UPDATE で保存できない (#1174)。
	"mcaptchaSiteKey": "mcaptchaSitekey",
}

func renameUpdateMetaFields(fields map[string]any) {
	for alias, canonical := range updateMetaFieldAliases {
		if v, ok := fields[alias]; ok {
			fields[canonical] = v
			delete(fields, alias)
		}
	}
}

// updateMetaEnumAllowed enumerates upstream update-meta.ts の `enum: [...]`
// 制約付き field とその許容値集合 (#1108 sweep)。silent fallback を
// 防ぐため UpdateMeta の入口で reject する。upstream 同様 nil / 未送出
// は no-op (= field 自体が fields map に無いので validate 対象外)。
//
// clientOptions.entrancePageStyle は nested object なので別途 dict 構造
// で扱う必要がある。本 helper は top-level enum のみ対象。entrancePage
// Style は admin UI 改修が他系列より少ない前提 + nested handler 不在の
// ため本 sweep の scope 外 (= 別 PR で nested validator を追加するなら
// follow-up)。
var updateMetaEnumAllowed = map[string]map[string]struct{}{
	"sensitiveMediaDetection":            {"none": {}, "all": {}, "local": {}, "remote": {}},
	"sensitiveMediaDetectionSensitivity": {"medium": {}, "low": {}, "high": {}, "veryLow": {}, "veryHigh": {}},
	"federation":                         {"all": {}, "none": {}, "specified": {}},
	"ugcVisibilityForVisitor":            {"all": {}, "local": {}, "none": {}},
}

// validateUpdateMetaEnums returns an error when fields map contains an
// enum-constrained key whose value isn't in the allowed set. nil / non-string
// values are reject (= upstream の json schema validator が string + enum で
// 制約する semantics に合わせる)。
// updateMetaNumericMinimums は update-meta で `minimum` 制約を持つ integer
// カラム (upstream ajv paramDef の minimum 相当)。JSON decode 後の数値は
// float64 で届く。
var updateMetaNumericMinimums = map[string]float64{
	// upstream 2026.7.0 sensitiveMediaDetection 外部サービス設定 (minimum: 1)。
	"sensitiveMediaDetectionTimeout":             1,
	"sensitiveMediaDetectionMaxImagesPerRequest": 1,
	// 分割アップロード (#2313)。mk-go 独自。
	"chunkedUploadMaxSessionsPerUser":  1,
	"chunkedUploadMaxPendingMbPerUser": 1,
}

// updateMetaNumericRanges は上下限を両方持つ integer カラム。分割アップロード
// (#2313) の設定は S3 側の制約 (最小パートサイズ) とリバースプロキシの上限に
// 挟まれるので、片側だけでは足りない。
//
// core/drive 側でも同じ範囲に clamp しているが、admin UI から入れた値が黙って
// 別の値になるのは分かりにくいので、入口では明示的に弾く。
var updateMetaNumericRanges = map[string]struct{ min, max float64 }{
	"chunkedUploadChunkSizeMb":       {coredrive.MinChunkSizeMb, coredrive.MaxChunkSizeMb},
	"chunkedUploadSessionTtlMinutes": {coredrive.MinSessionTTLMinutes, coredrive.MaxSessionTTLMinutes},
	// 最小文字数 (#3015)。**上限は `localUsernamePattern` の 20 と揃える** —
	// 21 以上を書けると、どの username も format 検証で先に落ちるので
	// 登録が全滅する。0 以下も列の既定 (1) と食い違うので弾く。
	// service 側にも clamp があるが、**壊れた値を DB に書かせない**のが
	// ここの役割 (silent fallback で admin の意図と乖離するのを防ぐ、#1108)。
	"minimumUsernameLength": {float64(signup.MinUsernameLength), float64(signup.MaxUsernameLength)},
}

// validateUpdateMetaNumericRanges rejects integer columns outside their
// supported range.
func validateUpdateMetaNumericRanges(fields map[string]any) error {
	for key, r := range updateMetaNumericRanges {
		v, ok := fields[key]
		if !ok {
			continue
		}
		if v == nil {
			return errors.New(key + " must be an integer")
		}
		n, ok := v.(float64)
		if !ok || n != math.Trunc(n) {
			return errors.New(key + " must be an integer")
		}
		if n < r.min || n > r.max {
			return fmt.Errorf("%s must be between %d and %d", key, int(r.min), int(r.max))
		}
	}
	return nil
}

// validateUpdateMetaNumericMinimums rejects non-numeric or below-minimum
// values for integer meta columns (ajv `type: integer, minimum: N` 相当)。
func validateUpdateMetaNumericMinimums(fields map[string]any) error {
	for key, minimum := range updateMetaNumericMinimums {
		v, ok := fields[key]
		if !ok {
			continue
		}
		// JSON null は upstream の ajv (nullable 無しの integer) が弾く。
		// mk-go で素通しすると NOT NULL 列に NULL を書いて 500 になる。
		if v == nil {
			return errors.New(key + " must be an integer")
		}
		n, ok := v.(float64)
		if !ok || n != math.Trunc(n) {
			return errors.New(key + " must be an integer")
		}
		if n < minimum {
			return fmt.Errorf("%s must be >= %d", key, int(minimum))
		}
	}
	return nil
}

func validateUpdateMetaEnums(fields map[string]any) error {
	for key, allowed := range updateMetaEnumAllowed {
		v, ok := fields[key]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return errors.New(key + " must be a string")
		}
		if _, ok := allowed[s]; !ok {
			// 許容値は alphabetical sort で error 文に並べる (= deterministic
			// error message、test の assert.Contains が安定する)。
			keys := make([]string, 0, len(allowed))
			for k := range allowed {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return errors.New(key + " must be one of: " + strings.Join(keys, ", "))
		}
	}
	return nil
}

// metaArrayColumns enumerates every varchar[] column on `meta` that admin
// can touch through /api/admin/update-meta. Each column is declared
// `NOT NULL DEFAULT '{}'` (see migration 000001_initial.up.sql), so any
// JSON null or untyped slice the frontend sends has to be coerced before
// it reaches lib/pq — otherwise the entire UPDATE rolls back. The set is
// kept narrow to avoid touching unrelated fields. Add new entries here
// when a varchar[] meta column is introduced.
var metaArrayColumns = map[string]struct{}{
	"langs":                        {},
	"pinnedUsers":                  {},
	"hiddenTags":                   {},
	"blockedHosts":                 {},
	"silencedHosts":                {},
	"mediaSilencedHosts":           {},
	"sensitiveWords":               {},
	"prohibitedWords":              {},
	"prohibitedWordsForNameOfUser": {},
	"serverRules":                  {},
	"federationHosts":              {},
	"bannedEmailDomains":           {},
	"preservedUsernames":           {},
	"urlPreviewSensitiveList":      {},
}

// coerceMetaArrayFields normalises array-shaped values bound from JSON
// into model.StringArray for known varchar[] columns on the meta row. It is
// the admin/update-meta only helper — every meta varchar[] column listed
// in metaArrayColumns is NOT NULL with DEFAULT '{}'.
//
// 必要な理由 (#590): JSON decoder は array を []any に decode するが、
// lib/pq の Value driver は []any を varchar[] に変換できず
// "expression is of type record" で PostgreSQL 側エラーになり、UPDATE 全体
// が rollback する。さらに JSON null を送られると nil が model.StringArray
// 列に流れて NOT NULL 制約違反でも rollback する。両方を空配列もしくは
// model.StringArray に揃えることで、admin の whitelist / blocklist 設定が
// 期待どおり永続化される。
//
// 関連列以外 (例: rootUserId のような nullable string) は触らない。
// metaArrayColumns に列挙された key のみが coerce 対象。
func coerceMetaArrayFields(fields map[string]any) {
	for k, v := range fields {
		if _, ok := metaArrayColumns[k]; !ok {
			continue
		}
		switch arr := v.(type) {
		case nil:
			// JSON null は admin の「リスト解除」意図と解釈し、空配列に
			// 揃えて NOT NULL 制約違反を避ける (Misskey TS は配列型で
			// declared、null 自体を弾くので mk-go 側で寄せる)。
			fields[k] = model.StringArray{}
		case []any:
			// 全要素 string なら model.StringArray、不正型混入 (string 以外)
			// は実 repo に流して error にさせる (型エラーを silently 飲み
			// 込まないため)。
			strs := make([]string, 0, len(arr))
			allStrings := true
			for _, e := range arr {
				s, isStr := e.(string)
				if !isStr {
					allStrings = false
					break
				}
				strs = append(strs, s)
			}
			if allStrings {
				fields[k] = model.StringArray(strs)
			}
		}
		// model.StringArray / []string が来ているケースは driver.Valuer 互換
		// なのでそのまま real repo に流す。
	}
}

// metaJSONBColumns lists meta jsonb columns whose update-meta payload arrives as
// a decoded []any / map[string]any and must be json.Marshal された []byte に
// 変換しないと jsonb 列の UPDATE が型不一致で失敗する (#1732 deliverSuspendedSoftware)。
// policies は object 形 jsonb で update-default-policies / update-meta の両経路から
// 来る (#1823)。clientOptions も object 形 jsonb で update-meta が whitelist 無しで
// 受理するため、未登録だと同じ型不一致 500 になる (#1846)。
//
// model.Meta に datatypes.JSON 列を追加したらここにも登録すること。漏れは
// TestMetaJSONBColumns_CoversAllJSONColumns (reflection guard) が検出する。
var metaJSONBColumns = map[string]struct{}{
	"deliverSuspendedSoftware": {},
	"policies":                 {},
	"clientOptions":            {},
	"signupApplicationForm":    {},
}

// metaJSONBObjectColumns は metaJSONBColumns のうち object ({}) 形のもの。nil の
// とき array 形は `[]`、object 形は `{}` を default にするための区別 (#1823 / #1846)。
var metaJSONBObjectColumns = map[string]struct{}{
	"policies":      {},
	"clientOptions": {},
}

// coerceMetaJSONBFields marshals decoded JSON values for known jsonb meta
// columns into datatypes.JSON ([]byte) so GORM/pgx can write them. nil は
// 空 default (array→`[]` / object→`{}`)、既に []byte/string のものはそのまま流す。
func coerceMetaJSONBFields(fields map[string]any) {
	for k, v := range fields {
		if _, ok := metaJSONBColumns[k]; !ok {
			continue
		}
		switch v.(type) {
		case nil:
			if _, isObj := metaJSONBObjectColumns[k]; isObj {
				fields[k] = datatypes.JSON([]byte("{}"))
			} else {
				fields[k] = datatypes.JSON([]byte("[]"))
			}
		case []byte, string, datatypes.JSON:
			// 既に raw JSON。そのまま流す。
		default:
			if raw, err := json.Marshal(v); err == nil {
				fields[k] = datatypes.JSON(raw)
			}
		}
	}
}

// normalizeUpdateMetaFields reproduces upstream update-meta.ts の値正規化:
// host リストの filter(Boolean)/lowercase/sort/dedup/blocked 除外、空文字→null、
// repositoryUrl の URL 検証、urlPreviewUserAgent/summalyProxy の trim。fields は
// rename 済 (canonical column name) を前提とする。array 系は model.StringArray に
// 変換するので、後続の coerceMetaArrayFields は pass-through となる。
func normalizeUpdateMetaFields(fields map[string]any) {
	// filter(Boolean): 空文字列要素を除去する array columns。
	for _, key := range []string{"langs", "pinnedUsers", "hiddenTags", "sensitiveWords", "prohibitedWords", "prohibitedWordsForNameOfUser", "urlPreviewSensitiveList"} {
		if arr, ok := metaStringSlice(fields[key]); ok {
			fields[key] = model.StringArray(filterNonEmptyHosts(arr, false))
		}
	}
	// blockedHosts / federationHosts: filter(Boolean) + lowercase。
	var blocked []string
	for _, key := range []string{"blockedHosts", "federationHosts"} {
		if arr, ok := metaStringSlice(fields[key]); ok {
			norm := filterNonEmptyHosts(arr, true)
			fields[key] = model.StringArray(norm)
			if key == "blockedHosts" {
				blocked = norm
			}
		}
	}
	// silencedHosts / mediaSilencedHosts: sort + dedup + 空除外 + blockedHosts 除外。
	// blocked は同一リクエストで送られた (正規化済) blockedHosts のみを参照する
	// (upstream は set.blockedHosts?.includes、= リクエストに無ければ除外しない)。
	for _, key := range []string{"silencedHosts", "mediaSilencedHosts"} {
		if arr, ok := metaStringSlice(fields[key]); ok {
			fields[key] = model.StringArray(sortDedupExcludeHosts(arr, blocked))
		}
	}
	// 空文字→null の string columns (deeplAuthKey 等)。
	for _, key := range []string{"deeplAuthKey", "verifymailAuthKey", "truemailInstance", "truemailAuthKey", "googleAnalyticsMeasurementId"} {
		if v, ok := fields[key]; ok {
			if s, ok := v.(string); ok && s == "" {
				fields[key] = nil
			}
		}
	}
	// repositoryUrl: 妥当な絶対 URL でなければ null (upstream URL.canParse)。
	if v, ok := fields["repositoryUrl"]; ok {
		if s, ok := v.(string); ok && !isParsableURL(s) {
			fields["repositoryUrl"] = nil
		}
	}
	// urlPreviewUserAgent: trim して空なら null、非空なら原文 (untrimmed) を保存
	// (upstream は trim 結果で null 判定しつつ ps.urlPreviewUserAgent を格納)。
	if v, ok := fields["urlPreviewUserAgent"]; ok {
		if s, ok := v.(string); ok {
			if strings.TrimSpace(s) == "" {
				fields["urlPreviewUserAgent"] = nil
			}
		}
	}
	// summalyProxy は urlPreviewSummaryProxyUrl の別名。両者のうち
	// urlPreviewSummaryProxyUrl を優先し、trim して空なら null、非空なら trim 済を
	// 保存する (upstream)。
	//
	// upstream は `(urlPreviewSummaryProxyUrl ?? summalyProxy)` の null 合体だが、
	// ここでは key 存在で分岐する。urlPreviewSummaryProxyUrl が明示的 JSON null で
	// 送られた場合は summalyProxy へフォールバックせず null になる差分があるが、
	// frontend が両者を同時送出するケースは無いため許容する。
	if _, hasA := fields["urlPreviewSummaryProxyUrl"]; hasA {
		fields["urlPreviewSummaryProxyUrl"] = trimToNil(fields["urlPreviewSummaryProxyUrl"])
		delete(fields, "summalyProxy")
	} else if v, hasB := fields["summalyProxy"]; hasB {
		fields["urlPreviewSummaryProxyUrl"] = trimToNil(v)
		delete(fields, "summalyProxy")
	}
	// upstream 2026.7.0 update-meta.ts は sensitive-detector の URL / key を
	// `ps.X === '' ? null : ps.X` で null 化する (コントロールパネルで欄を空に
	// したら未設定に戻る)。
	for _, key := range []string{"sensitiveMediaDetectionApiUrl", "sensitiveMediaDetectionApiKey"} {
		if v, ok := fields[key]; ok {
			fields[key] = trimToNil(v)
		}
	}
}

// metaStringSlice extracts a []string from a JSON-bound array value. Returns
// ok=false when v is not an array of all-strings (nil / non-array / mixed types
// are left untouched so coerceMetaArrayFields / the real repo handle them).
func metaStringSlice(v any) ([]string, bool) {
	switch arr := v.(type) {
	case []string:
		return arr, true
	case model.StringArray:
		return []string(arr), true
	case []any:
		out := make([]string, 0, len(arr))
		for _, e := range arr {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// filterNonEmptyHosts drops empty-string entries and, when lower is true,
// lowercases each entry (upstream blockedHosts/federationHosts handling).
func filterNonEmptyHosts(in []string, lower bool) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if lower {
			s = strings.ToLower(s)
		}
		out = append(out, s)
	}
	return out
}

// sortDedupExcludeHosts sorts, removes empties + duplicates, and excludes any
// host present in blocked (upstream silencedHosts/mediaSilencedHosts handling).
func sortDedupExcludeHosts(in, blocked []string) []string {
	sorted := append([]string(nil), in...)
	sort.Strings(sorted)
	blockedSet := make(map[string]struct{}, len(blocked))
	for _, b := range blocked {
		blockedSet[b] = struct{}{}
	}
	out := make([]string, 0, len(sorted))
	last := ""
	first := true
	for _, h := range sorted {
		if h == "" {
			continue
		}
		if !first && h == last {
			continue
		}
		first = false
		last = h
		if _, blockedHost := blockedSet[h]; blockedHost {
			continue
		}
		out = append(out, h)
	}
	return out
}

// isParsableURL reports whether s is a parseable absolute URL (scheme + host),
// approximating upstream `URL.canParse(ps.repositoryUrl)`.
//
// 厳密には URL.canParse より厳格で、host を要求する。これにより
// `mailto:a@b.com` / `javascript:...` のような hostless scheme を弾く
// (canParse は true)。repositoryUrl の現実的入力は http(s) のみで、
// scheme だけの危険 URL を排除できる安全側の差分なので許容する。
func isParsableURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme != "" && u.Host != ""
}

// trimToNil trims a string value and returns nil when the result is empty,
// otherwise the trimmed string. Non-string values pass through unchanged.
func trimToNil(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	t := strings.TrimSpace(s)
	if t == "" {
		return nil
	}
	return t
}

// --- Role endpoints ---

// RolesCreate handles POST /api/admin/roles/create.
//
// upstream Misskey TS の paramDef は 14 field を required (一部 nullable) で
// 受け取る (name / description / color / iconUrl / target / condFormula /
// isPublic / isModerator / isAdministrator / isExplorable / asBadge /
// canEditMembersByModerator / displayOrder / policies)。mk-go も drop-in
// 互換のため同 field を accept する (#889)。
//
// model.Role の全 column を実際に persist する (PR #1102 で配線完了)。
// 旧版は policies / condFormula / color / iconUrl / target / canEdit
// MembersByModerator を request で受け取りつつ /dev/null に流していた
// ため、admin UI から policy / 色 / アイコン等を設定しても DB に書かれず
// 「ロール設定が反映されない」現象になっていた。
func (h *Handler) RolesCreate(c echo.Context) error {
	var req struct {
		Name                            *string         `json:"name"`
		Description                     *string         `json:"description"`
		Color                           *string         `json:"color"`
		IconURL                         *string         `json:"iconUrl"`
		Target                          *string         `json:"target"`
		CondFormula                     *map[string]any `json:"condFormula"`
		IsPublic                        *bool           `json:"isPublic"`
		IsModerator                     *bool           `json:"isModerator"`
		IsAdministrator                 *bool           `json:"isAdministrator"`
		IsExplorable                    *bool           `json:"isExplorable"`
		AsBadge                         *bool           `json:"asBadge"`
		CanEditMembersByModerator       *bool           `json:"canEditMembersByModerator"`
		PreserveAssignmentOnMoveAccount *bool           `json:"preserveAssignmentOnMoveAccount"`
		DisplayOrder                    *int            `json:"displayOrder"`
		Policies                        *map[string]any `json:"policies"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// upstream paramDef の required field を非 nil で要求する (= partial
	// payload を 400 で reject、TS 互換、#889)。
	// color / iconUrl は upstream paramDef で nullable: true。JSON で null
	// 送出時に Go の `*string` が nil になり「field 不在」と区別できない
	// ため required check から除外する (= TS は `null` 可で accept、mk-go
	// では実 require は省略するが TS frontend の payload は通る)。
	if req.Name == nil || *req.Name == "" ||
		req.Description == nil ||
		req.Target == nil ||
		req.CondFormula == nil ||
		req.IsPublic == nil ||
		req.IsModerator == nil ||
		req.IsAdministrator == nil ||
		req.AsBadge == nil ||
		req.CanEditMembersByModerator == nil ||
		req.DisplayOrder == nil ||
		req.Policies == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Required parameters missing.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	opts := role.CreateOptions{
		Color:                     req.Color,
		IconURL:                   req.IconURL,
		IsPublic:                  *req.IsPublic,
		IsModerator:               *req.IsModerator,
		IsAdministrator:           *req.IsAdministrator,
		AsBadge:                   *req.AsBadge,
		IsExplorable:              req.IsExplorable != nil && *req.IsExplorable,
		DisplayOrder:              *req.DisplayOrder,
		CanEditMembersByModerator: *req.CanEditMembersByModerator,
	}
	if req.PreserveAssignmentOnMoveAccount != nil {
		opts.PreserveAssignmentOnMoveAccount = *req.PreserveAssignmentOnMoveAccount
	}
	// Target は upstream paramDef で `enum: ['manual', 'conditional']`。
	// nest.js framework が unknown を 400 で reject するので、mk-go も
	// silent fallback せず 400 を返す (新規 role なので Update 経路ほど
	// 破壊的ではないが、shape を upstream に揃える)。
	switch *req.Target {
	case string(model.RoleTargetManual):
		opts.Target = model.RoleTargetManual
	case string(model.RoleTargetConditional):
		opts.Target = model.RoleTargetConditional
	default:
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "target must be 'manual' or 'conditional'.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// CondFormula / Policies は JSON object → datatypes.JSON (= []byte) に Marshal。
	// upstream は object 全体をそのまま column に書くだけで内部構造は consumer 任せ。
	if cf, err := json.Marshal(*req.CondFormula); err == nil {
		opts.CondFormula = cf
	} else {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "condFormula must be a JSON object.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if key := invalidRolePolicyKey(*req.Policies); key != "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM",
			"policy "+key+" has a value of the wrong type.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if pol, err := json.Marshal(*req.Policies); err == nil {
		opts.Policies = pol
	} else {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "policies must be a JSON object.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	r, err := h.roleService.Create(*req.Name, *req.Description, opts)
	if err != nil {
		// **クライアント起因の検証失敗を 5xx に潰さない (#3037)。** 管理者に
		// "Internal error." としか出ないと何が悪いか分からないうえ、監視にも
		// 5xx として乗る (#2792 の逆向き)。update 側と同じ応答にすること。
		if errors.Is(err, role.ErrSelfGrantablePrivilege) {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM",
				selfGrantableRoleMessage, "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	h.logModeration(c, moderationlog.LogCreateRole, map[string]any{
		"roleId": r.ID,
		"role":   r,
	})
	return c.JSON(http.StatusOK, h.packRole(r))
}

// selfGrantableRoleMessage is the 400 body shared by roles/create and
// roles/update.
//
// **2 箇所で同じ文言にする。** 片方だけ直すと、管理者が create と update で
// 違う説明を読むことになる。
//
// **判定の実態に合わせること。** #3037 の 2 周目で `isLocal` /
// `createdLessThan` / 短い `createdMoreThan` / 恒真式 / 未知の型が拒否側に
// 加わったのに、この文面は 1 周目の一覧のままだった。#3045 でも同じことが
// 起きている (年齢の閾値が消えて `createdMoreThan` が長さによらず拒否側に
// なった)。管理者は**自分が使っていない条件の一覧**を読まされ、何が悪いのか
// 分からない。2 度あったので `TestSelfGrantableRoleMessageExplainsEveryLeafCondition`
// で止める。
const selfGrantableRoleMessage = "登録するだけで満たせる条件 (isLocal / createdLessThan)、待てば満たせる条件 (createdMoreThan と not(createdLessThan))、本人が切り替えられる条件 (isBot / isCat / isLocked / isExplorable / フォロワー数 / フォロー数 / 投稿数)、全員に一致する式 (空の and、空の or の否定、中身の無い not の否定)、判定できない未知の条件を使った条件つきロールには、管理者・モデレーターや権限を配る policy を持たせられません。用意できない条件 (isRemote / isSuspended / roleAssignedTo) も、not で包むと新規アカウントがそのまま満たすので同じく使えません。条件で配りたいときは手動ロールを作って roleAssignedTo で参照してください。既存のロールは名前や色だけの更新でもこの検査を通るので、その場合は権限を下ろすか条件を差し替えるか手動ロールに変えてください。"

// invalidRolePolicyKey returns the first policy entry whose value has the wrong
// type, or "" when every entry is acceptable.
//
// **型を見ないと gate が消える (#3037)。** policy の consumer は
// `if limit, ok := role.PolicyNumber(v); ok { ...gate... }` の形なので、
// 数値の policy に文字列が入ると **上限違反で弾かれるのではなく上限そのものが
// 消える** (#2611 と同じ壊れ方)。管理画面は型どおりの値しか送らないが、
// この endpoint を直に 1 回叩けばその状態を作れた。
//
// 期待する形は `{ "<key>": {"useDefault":…, "priority":…, "value":…} }`。
// `useDefault` が真の entry は値を読まないので見ない。**形そのものが違う
// entry は読み飛ばす** — `parseRolePolicies` も同じ扱いで、ここで弾くと
// upstream が entry の形を拡張したときに mk-go だけが設定を拒否する。
func invalidRolePolicyKey(policies map[string]any) string {
	for key, raw := range policies {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if useDefault, ok := entry["useDefault"].(bool); ok && useDefault {
			continue
		}
		value, ok := entry["value"]
		if !ok {
			continue
		}
		if !effectivepolicy.ValidatePolicyValue(key, value) {
			return key
		}
	}
	return ""
}

// invalidDefaultPolicyKey is invalidRolePolicyKey for the flat
// `{ "<key>": <value> }` shape that `roles/update-default-policies` takes.
func invalidDefaultPolicyKey(policies map[string]any) string {
	for key, value := range policies {
		if !effectivepolicy.ValidatePolicyValue(key, value) {
			return key
		}
	}
	return ""
}

// packRole renders a role in the upstream-compatible shape (usersCount /
// createdAt / default-filled policies 等)。raw model.Role を返すとこれらが欠落
// する。usersCount は active assignment 数 (per-role count、role は少数なので
// N+1 でも許容、upstream pack も per-role count する)。
func (h *Handler) packRole(r *model.Role) map[string]any {
	return entity.PackRole(r, h.roleService.CountAssignedUsers(r.ID), h.idGen, role.DefaultPolicies())
}

// RolesShow handles POST /api/admin/roles/show.
func (h *Handler) RolesShow(c echo.Context) error {
	var req struct {
		RoleID string `json:"roleId"`
	}
	if err := c.Bind(&req); err != nil || req.RoleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roleId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	r, err := h.roleService.Show(req.RoleID)
	if err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "07dc7d34-c0d8-49b7-96c6-db3ce64ee0b3"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.packRole(r))
}

// RolesList handles POST /api/admin/roles/list.
func (h *Handler) RolesList(c echo.Context) error {
	// upstream admin/roles/list.ts は lastUsedAt DESC で返す (#2061)。
	roles, err := h.roleService.ListByLastUsed()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	out := make([]map[string]any, 0, len(roles))
	for _, r := range roles {
		out = append(out, h.packRole(r))
	}
	return c.JSON(http.StatusOK, out)
}

// RolesUpdate handles POST /api/admin/roles/update.
//
// upstream Misskey TS の paramDef は 15 field を accept (roleId 以外はすべて
// optional partial update)。旧 mk-go は 5 field しか accept しておらず、
// admin UI で **policies / color / iconUrl / target / condFormula / asBadge /
// isExplorable / displayOrder / canEditMembersByModerator / preserveAssignment
// OnMoveAccount** を変えても DB に書かれず「ロール設定が反映されない」現象
// になっていた (PR #1102)。特に policies field の欠落は「canPublicNote 等の
// ポリシーが UI 上で trueに見えても実際は default 値のまま」という drop-in
// regression 重大度高めの bug。本 fix で upstream 15 field 全部を pipe する。
func (h *Handler) RolesUpdate(c echo.Context) error {
	var req struct {
		RoleID                          string          `json:"roleId"`
		Name                            *string         `json:"name"`
		Description                     *string         `json:"description"`
		Color                           *string         `json:"color"`
		IconURL                         *string         `json:"iconUrl"`
		Target                          *string         `json:"target"`
		CondFormula                     *map[string]any `json:"condFormula"`
		IsModerator                     *bool           `json:"isModerator"`
		IsAdministrator                 *bool           `json:"isAdministrator"`
		IsPublic                        *bool           `json:"isPublic"`
		IsExplorable                    *bool           `json:"isExplorable"`
		AsBadge                         *bool           `json:"asBadge"`
		CanEditMembersByModerator       *bool           `json:"canEditMembersByModerator"`
		PreserveAssignmentOnMoveAccount *bool           `json:"preserveAssignmentOnMoveAccount"`
		DisplayOrder                    *int            `json:"displayOrder"`
		Policies                        *map[string]any `json:"policies"`
	}
	if err := c.Bind(&req); err != nil || req.RoleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roleId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	before, err := h.roleService.Show(req.RoleID)
	if err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "cd23ef55-09ad-428a-ac61-95a45e124b32"))
		}
		return apierr.JSONInternalError(c)
	}
	fields := map[string]any{}
	if req.Name != nil {
		fields["name"] = *req.Name
	}
	if req.Description != nil {
		fields["description"] = *req.Description
	}
	if req.Color != nil {
		// mk-go specific drift: upstream は `null` 送出時に column を null
		// クリアするが、Go の `*string` JSON binding では「field 未送出」と
		// 「null 明示」を区別できず両者とも nil pointer になる。frontend が
		// クリア意図で `null` を送ったケースを救済するため、空文字 ("") も
		// null クリアとして扱う運用にする。upstream は "" を空文字として
		// 保存するので、theoretically 「色を文字 0 個に設定」したい場合の
		// 挙動は乖離するが、実用上「色 = empty string」は存在しない (#1102)。
		if *req.Color == "" {
			fields["color"] = nil
		} else {
			fields["color"] = *req.Color
		}
	}
	if req.IconURL != nil {
		// 上記 Color と同じ理由で空文字を null クリア扱いする。
		if *req.IconURL == "" {
			fields["iconUrl"] = nil
		} else {
			fields["iconUrl"] = *req.IconURL
		}
	}
	if req.Target != nil {
		// upstream paramDef は `enum: ['manual', 'conditional']` で nest.js
		// framework が unknown 値を 400 で reject する。mk-go も silent
		// fallback (旧版は unknown → manual) せずに 400 を返す方が、frontend
		// の typo 等で **既存 conditional role が意図せず manual に書き換わる
		// silent corruption** を防げる。
		switch *req.Target {
		case string(model.RoleTargetManual):
			fields["target"] = model.RoleTargetManual
		case string(model.RoleTargetConditional):
			fields["target"] = model.RoleTargetConditional
		default:
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "target must be 'manual' or 'conditional'.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
	}
	if req.CondFormula != nil {
		cf, err := json.Marshal(*req.CondFormula)
		if err != nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "condFormula must be a JSON object.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
		fields["condFormula"] = cf
	}
	if req.IsModerator != nil {
		fields["isModerator"] = *req.IsModerator
	}
	if req.IsAdministrator != nil {
		fields["isAdministrator"] = *req.IsAdministrator
	}
	if req.IsPublic != nil {
		fields["isPublic"] = *req.IsPublic
	}
	if req.IsExplorable != nil {
		fields["isExplorable"] = *req.IsExplorable
	}
	if req.AsBadge != nil {
		fields["asBadge"] = *req.AsBadge
	}
	if req.CanEditMembersByModerator != nil {
		fields["canEditMembersByModerator"] = *req.CanEditMembersByModerator
	}
	if req.PreserveAssignmentOnMoveAccount != nil {
		fields["preserveAssignmentOnMoveAccount"] = *req.PreserveAssignmentOnMoveAccount
	}
	if req.DisplayOrder != nil {
		fields["displayOrder"] = *req.DisplayOrder
	}
	if req.Policies != nil {
		if key := invalidRolePolicyKey(*req.Policies); key != "" {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM",
				"policy "+key+" has a value of the wrong type.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
		pol, err := json.Marshal(*req.Policies)
		if err != nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "policies must be a JSON object.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
		fields["policies"] = pol
	}
	if len(fields) == 0 {
		// 全フィールドが nil = 何も変更しないリクエスト。before == after の
		// 無意味な log を書かずに早期 return する。
		return c.NoContent(http.StatusNoContent)
	}
	after, err := h.roleService.UpdateFields(req.RoleID, fields)
	if err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "cd23ef55-09ad-428a-ac61-95a45e124b32"))
		}
		// create と同じ理由 (#3037)。**更新後の姿で判定する**ので、
		// 「条件つきに変える」「管理者を立てる」「条件を差し替える」の
		// どれ 1 つでもここへ来る。
		if errors.Is(err, role.ErrSelfGrantablePrivilege) {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM",
				selfGrantableRoleMessage, "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
		return apierr.JSONInternalError(c)
	}
	h.logModeration(c, moderationlog.LogUpdateRole, map[string]any{
		"roleId": req.RoleID,
		"before": before,
		"after":  after,
	})
	return c.NoContent(http.StatusNoContent)
}

// RolesDelete handles POST /api/admin/roles/delete.
func (h *Handler) RolesDelete(c echo.Context) error {
	var req struct {
		RoleID string `json:"roleId"`
	}
	if err := c.Bind(&req); err != nil || req.RoleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roleId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// 削除直後だと role 情報を取れないので事前に snapshot を取って log info に含める
	snapshot, _ := h.roleService.Show(req.RoleID)
	if err := h.roleService.Delete(req.RoleID); err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "de0d6ecd-8e0a-4253-88ff-74bc89ae3d45"))
		}
		return apierr.JSONInternalError(c)
	}
	h.logModeration(c, moderationlog.LogDeleteRole, map[string]any{
		"roleId": req.RoleID,
		"role":   snapshot,
	})
	return c.NoContent(http.StatusNoContent)
}

// RolesAssign handles POST /api/admin/roles/assign.
func (h *Handler) RolesAssign(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
		RoleID string `json:"roleId"`
		// upstream assign.ts paramDef は expiresAt: { type:'integer', nullable }
		// = epoch ms。frontend も epoch ms (number) を送るため *int64 で受ける。
		// 以前は *string + RFC3339 parse だったが、JSON number を *string に
		// bind すると unmarshal error → 400 となり期限付き role 付与が常に
		// 失敗していた (#1542)。
		ExpiresAt *int64 `json:"expiresAt"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" || req.RoleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "userId and roleId are required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// canEditMembersByModerator gate (upstream assign.ts:69-76)。role lookup →
	// gate の順で、moderator (非 administrator) は canEditMembersByModerator が
	// 立つ role しか付け外しできない。NO_SUCH_ROLE / ACCESS_DENIED の UUID は
	// assign 固有のもの。
	if handled, err := h.requireCanEditRoleMembers(c, req.RoleID, "6503c040-6af4-4ed9-bf07-f2dd16678eab", "25b5bc31-dc79-4ebd-9bd2-c84978fd052c"); handled {
		return err
	}
	// upstream assign.ts:77-81: 対象 user 不在なら NO_SUCH_USER (#1542)。
	if h.userRepo != nil {
		if _, err := h.userRepo.FindByID(req.UserID); err != nil {
			if !repository.IsNotFound(err) {
				// **DB 障害を not-found に丸めない** (#2792)。
				return apierr.JSONInternalError(c)
			}
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "558ea170-f653-4700-94d0-5a818371d0df"))
		}
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		t := time.UnixMilli(*req.ExpiresAt)
		// upstream assign.ts:83-85: 過去 (現在以下) の expiresAt は no-op で
		// return する (過去日時付与の無効化)。
		if !t.After(time.Now()) {
			return c.NoContent(http.StatusNoContent)
		}
		expiresAt = &t
	}
	if err := h.roleService.Assign(req.UserID, req.RoleID, expiresAt); err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "6503c040-6af4-4ed9-bf07-f2dd16678eab"))
		}
		if err == role.ErrAlreadyAssigned {
			return c.JSON(http.StatusConflict, apierr.Error("ALREADY_ASSIGNED", "Role already assigned.", "67d8689c-25c6-435f-8eed-6ea68e5e53e9"))
		}
		return apierr.JSONInternalError(c)
	}
	h.logRoleAssignment(c, moderationlog.LogAssignRole, req.UserID, req.RoleID)
	return c.NoContent(http.StatusNoContent)
}

// requireCanEditRoleMembers enforces the upstream canEditMembersByModerator
// gate shared by admin/roles/assign and /unassign. It looks up the role first
// (responding NO_SUCH_ROLE with the per-endpoint error id when missing) and then
// denies moderators that may not edit the role's members, while administrators
// (and root) are always allowed.
//
// The returned handled flag reports whether a response was already written: when
// true the caller must return the accompanying error immediately; when false the
// caller may proceed. echo の c.JSON は成功時 nil を返すため bool で「応答を書い
// たか」を伝える (err だけでは deny を short-circuit できない)。
//
// 認可判定 (viewer-based) なので service 層ではなく handler 層に置く。viewer が
// 取得できない場合は administrator とみなさず ACCESS_DENIED に倒す (fail-closed)。
// NO_SUCH_ROLE / ACCESS_DENIED の UUID は assign と unassign で異なるため呼び
// 出し側から渡す。
func (h *Handler) requireCanEditRoleMembers(c echo.Context, roleID, noSuchRoleID, accessDeniedID string) (handled bool, err error) {
	r, err := h.roleService.Show(roleID)
	if err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return true, c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", noSuchRoleID))
		}
		return true, apierr.JSONInternalError(c)
	}
	isAdmin := false
	if me := middleware.GetUser(c); me != nil {
		isAdmin = h.roleService.IsAdministrator(me.ID)
	}
	// **管理者権限を配るロールは管理者しか付け外しできない (#3037)。**
	//
	// `canEditMembersByModerator` は「モデレーターがメンバーを編集してよいか」
	// しか見ない。**管理者ロールにそれを立てるのは管理画面のチェックボックス
	// 1 つ**で、立った瞬間からモデレーターは自分自身にそのロールを付けられる
	// = 管理者へ昇格できる。ロールの作成・更新は管理者専用 (`RequireAdmin`)
	// なのに、付け外しだけがこの穴で抜けていた。
	//
	// **unassign も塞ぐ。** 管理者ロールを外せるなら、モデレーターが管理者を
	// 降格させて (あるいは全管理者を降ろして) 実質の最上位になれる。
	//
	// **upstream も同じ状態だが、そちらに合わせない。** `assign.ts:69-76` は
	// `canEditMembersByModerator` だけを見る。ここは mk-go を厳しい側に倒して
	// `docs/divergence.md` に記録する (プロジェクトの既定方針)。
	if r.IsAdministrator && !isAdmin {
		return true, c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Only administrators can edit members of an administrator role.", accessDeniedID))
	}
	// **間接的に特権を配るロールも管理者しか付け外しできない
	// (#3037 レビュー 2 周目)。**
	//
	// 上の判定は「そのロール自身が管理者ロールか」しか見ない。条件つきロールが
	// `roleAssignedTo` でこのロールを参照していると、**権限を持たない素の
	// ロールを配るだけで管理者になれる**。`checkConditionalPrivilege` の側も
	// 「利用者本人が条件を満たせるか」しか見ないので、2 つの判定の隙間を通る。
	if !isAdmin {
		indirect, err := h.roleService.RoleGrantsPrivilegeIndirectly(roleID)
		if err != nil {
			// **判定できないなら通さない (#2792)。** ここは「特権を配らない
			// ことを確かめてから触る」判定なので、fail-open にすると
			// ロールを列挙できない窓で昇格が成立する。
			slog.Error("admin: cannot determine whether the role grants privileges indirectly", "roleId", roleID, "err", err)
			return true, apierr.JSONInternalError(c)
		}
		if indirect {
			return true, c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED",
				"Only administrators can edit members of a role that grants privileges through a conditional role.", accessDeniedID))
		}
	}
	if r.CanEditMembersByModerator {
		return false, nil
	}
	if isAdmin {
		return false, nil
	}
	return true, c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Only administrators can edit members of the role.", accessDeniedID))
}

// RolesUnassign handles POST /api/admin/roles/unassign.
func (h *Handler) RolesUnassign(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
		RoleID string `json:"roleId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" || req.RoleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "userId and roleId are required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// canEditMembersByModerator gate (upstream unassign.ts:71-78)。role 不在は
	// assign 同様 NO_SUCH_ROLE を先に返す (TS と同順)。NO_SUCH_ROLE /
	// ACCESS_DENIED の UUID は unassign 固有のもの。
	if handled, err := h.requireCanEditRoleMembers(c, req.RoleID, "6e519036-a70d-4c76-b679-bc8fb18194e2", "24636eee-e8c1-493e-94b2-e16ad401e262"); handled {
		return err
	}
	// upstream unassign.ts:80-84: 対象 user 不在なら NO_SUCH_USER (#1542)。
	if h.userRepo != nil {
		if _, err := h.userRepo.FindByID(req.UserID); err != nil {
			if !repository.IsNotFound(err) {
				// **DB 障害を not-found に丸めない** (#2792)。
				return apierr.JSONInternalError(c)
			}
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "2b730f78-1179-461b-88ad-d24c9af1a5ce"))
		}
	}
	if err := h.roleService.Unassign(req.UserID, req.RoleID); err != nil {
		if err == role.ErrNotAssigned {
			return c.JSON(http.StatusBadRequest, apierr.Error("NOT_ASSIGNED", "Role not assigned.", "b9060ac7-5c94-4da4-9f55-2047c953df44"))
		}
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	h.logRoleAssignment(c, moderationlog.LogUnassignRole, req.UserID, req.RoleID)
	return c.NoContent(http.StatusNoContent)
}

// RolesUsers handles POST /api/admin/roles/users.
func (h *Handler) RolesUsers(c echo.Context) error {
	var req struct {
		RoleID    string `json:"roleId"`
		Limit     *int   `json:"limit"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
	}
	if err := c.Bind(&req); err != nil || req.RoleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roleId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}

	assignments, err := h.roleService.ListByRole(req.RoleID, untilID, sinceID, limit)
	if err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "224eff5e-2488-4b18-b3e7-f50d94421648"))
		}
		return apierr.JSONInternalError(c)
	}

	// Profile を per-row 引いていた N+1 を 1 batch query に集約 (#503 と同じ動機)。
	ids := make([]string, 0, len(assignments))
	for _, a := range assignments {
		if a.User != nil {
			ids = append(ids, a.User.ID)
		}
	}
	// Profile fetch 失敗は handler 全体の致命扱いではないので、空 map で
	// fallback する (各 user は profile=nil で pack される)。ShowUsers と同方針。
	profiles, _ := h.userRepo.FindProfilesByUserIDs(ids)
	profileByUser := make(map[string]*model.UserProfile, len(profiles))
	for _, p := range profiles {
		profileByUser[p.UserID] = p
	}

	// Misskey TS は admin/roles/users の返却を { id, createdAt, user } の配列で
	// 包むため互換のため同じ envelope に揃える (UI の Roles 詳細ページが
	// assignment.user を直接参照している)。createdAt は assignment.id (ULID) から
	// 復元 (User と同じく ID 由来 timestamp)。
	result := make([]map[string]any, 0, len(assignments))
	for _, a := range assignments {
		if a.User == nil {
			// role_assignment は残っているのに user 行が消えているデータ不整合。
			// 結果件数だけ silent に減らすと debug が困難なので警告ログを出して
			// admin tooling 側で清掃の signal にする (#598 review item 1)。
			slog.Warn("admin/roles/users: dangling role assignment",
				"assignmentId", a.ID, "userId", a.UserID, "roleId", a.RoleID)
			continue
		}
		createdAt := ""
		if t, err := h.idGen.ParseTime(a.ID); err == nil {
			createdAt = t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		// upstream users.ts:101 は expiresAt: assign.expiresAt?.toISOString() ?? null
		// を含める (#1542)。nil は JSON null。
		var expiresAt any
		if a.ExpiresAt != nil {
			expiresAt = a.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		result = append(result, map[string]any{
			"id":        a.ID,
			"createdAt": createdAt,
			// upstream users.ts:95 は packMany(users, me, {schema:'UserDetailed'})。
			// packAdminUser を使うと email / signins / roleAssigns 等の admin 専用
			// field が read:admin:roles scope に漏れる (show-user の admin guard 迂回)。
			// ShowUsers と同様 UserDetailed に揃えて過剰露出を防ぐ (#1822)。
			"user":      h.packModeratorVisibleUser(a.User, profileByUser[a.User.ID]),
			"expiresAt": expiresAt,
		})
	}
	return c.JSON(http.StatusOK, result)
}

// RolesUpdateDefaultPolicies handles POST /api/admin/roles/update-default-policies.
func (h *Handler) RolesUpdateDefaultPolicies(c echo.Context) error {
	var req struct {
		Policies map[string]any `json:"policies"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// **値の型を見る (#3037)。** ここは既定 policy そのものなので、数値の
	// policy に文字列が入ると**全利用者でその上限が消える**。
	if key := invalidDefaultPolicyKey(req.Policies); key != "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM",
			"policy "+key+" has a value of the wrong type.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// upstream update-default-policies.ts は更新前後の policies を取得して
	// moderationLogService.log('updateServerSettings', {before, after}) を記録する
	// (#1542)。before snapshot を Update 前に取得する。
	beforeMeta, _ := h.metaRepo.Fetch()
	// Meta の policies フィールドを更新。policies は jsonb 列なので decoded map を
	// そのまま渡すと UPDATE が型不一致で失敗する (#1823)。coerceMetaJSONBFields で
	// datatypes.JSON ([]byte) に変換してから渡す (RolesCreate/Update の Marshal と
	// 同じ動機、UpdateMeta 経路と共通化)。
	policyFields := map[string]any{"policies": req.Policies}
	coerceMetaJSONBFields(policyFields)
	if err := h.metaRepo.Update(policyFields); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// moderation log (before/after policies)。TS は policies のみを記録する。
	if afterMeta, err := h.metaRepo.Fetch(); err == nil {
		var beforePolicies any
		if beforeMeta != nil {
			beforePolicies = beforeMeta.Policies
		}
		h.logModeration(c, moderationlog.LogUpdateServerSettings, map[string]any{
			"before": beforePolicies,
			"after":  afterMeta.Policies,
		})
	}
	return c.NoContent(http.StatusNoContent)
}

// --- Emoji Admin endpoints ---

// SetEmojiRepo attaches the emoji repository.
func (h *Handler) SetEmojiRepo(r repository.EmojiRepository) { h.emojiRepo = r }

// BroadcastPublisher emits an instance-wide stream event to every connected
// client. Implemented by *stream.BroadcastPublisher (#2046)。
type BroadcastPublisher interface {
	PublishBroadcast(eventType string, body any)
}

// SetBroadcastPublisher wires the broadcast stream publisher used to emit
// emojiAdded / emojiUpdated / emojiDeleted (#2046)。nil disables emission.
func (h *Handler) SetBroadcastPublisher(p BroadcastPublisher) { h.broadcastPub = p }

// publishEmojiAdded emits emojiAdded `{emoji}` (single packed emoji).
func (h *Handler) publishEmojiAdded(e *model.Emoji) {
	h.invalidateEmojiDecorationCache()
	if h.broadcastPub == nil {
		return
	}
	h.broadcastPub.PublishBroadcast("emojiAdded", map[string]any{"emoji": entity.PackEmojiDetailed(e)})
}

// publishEmojiUpdated emits emojiUpdated `{emojis:[...]}` (packed array).
func (h *Handler) publishEmojiUpdated(emojis ...*model.Emoji) {
	h.invalidateEmojiDecorationCache()
	if h.broadcastPub == nil || len(emojis) == 0 {
		return
	}
	packed := make([]any, len(emojis))
	for i, e := range emojis {
		packed[i] = entity.PackEmojiDetailed(e)
	}
	h.broadcastPub.PublishBroadcast("emojiUpdated", map[string]any{"emojis": packed})
}

// publishEmojiDeleted emits emojiDeleted `{emojis:[...]}` (packed array).
func (h *Handler) publishEmojiDeleted(emojis ...*model.Emoji) {
	h.invalidateEmojiDecorationCache()
	if h.broadcastPub == nil || len(emojis) == 0 {
		return
	}
	packed := make([]any, len(emojis))
	for i, e := range emojis {
		packed[i] = entity.PackEmojiDetailed(e)
	}
	h.broadcastPub.PublishBroadcast("emojiDeleted", map[string]any{"emojis": packed})
}

// publishEmojiUpdatedByIDs re-fetches the given emoji ids and emits a single
// emojiUpdated event (upstream bulk ops の packDetailedMany(ids) 相当、#2046)。
func (h *Handler) publishEmojiUpdatedByIDs(ids []string) {
	h.invalidateEmojiDecorationCache()
	if h.broadcastPub == nil || h.emojiRepo == nil || len(ids) == 0 {
		return
	}
	rows, err := h.emojiRepo.FindManyByIDs(ids)
	if err != nil || len(rows) == 0 {
		return
	}
	h.publishEmojiUpdated(rows...)
}

// EmojiAdd handles POST /api/admin/emoji/add.
//
// upstream Misskey TS は `fileId` を必須として drive 経由でしか emoji を
// 登録できない設計だが、mk-go は legacy で `url` 直接受けも維持する。両 path
// をサポートして drop-in 互換を保つ:
//   - `fileId` 指定時: drive_file から URL を resolve して保存 (= upstream 互換)
//   - `url` 直接指定時: そのまま保存 (legacy)
//   - 両方なし: 400 INVALID_PARAM
//
// `fileId` 経路は**画像を system 所有の drive ファイルへ複製してから**参照する
// (#2999)。upstream は操作者のファイルをそのまま指すので意図的な乖離
// (docs/divergence.md §7)。
//
// **`url` 経路は取り込まない。** あちらの意味は「この URL を指す」で、drive に行の
// 無い外部 URL も指せる escape hatch (upstream には無い mk-go 独自の経路)。
// 取り込むには HTTP が要り、自分の URL を叩くのは SSRF ガードと衝突する。
// 直そうとしている失敗形は「**利用者が自分の drive ファイルを消す**」で、
// `url` 経路には所有者の概念そのものが無い。fork frontend もこの経路は使わない
// (`emoji-edit-dialog.vue` / `custom-emojis-manager.register.vue` はどちらも
// `fileId` を送る)。
func (h *Handler) EmojiAdd(c echo.Context) error {
	var req struct {
		Name        string   `json:"name"`
		URL         string   `json:"url"`
		FileID      string   `json:"fileId"`
		Category    *string  `json:"category"`
		Aliases     []string `json:"aliases"`
		License     *string  `json:"license"`
		IsSensitive bool     `json:"isSensitive"`
		LocalOnly   bool     `json:"localOnly"`
		RoleIDs     []string `json:"roleIdsThatCanBeUsedThisEmojiAsReaction"`
	}
	if err := c.Bind(&req); err != nil || req.Name == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "name is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// upstream paramDef は name に `^[a-zA-Z0-9_]+$` を強制する。
	//
	// **長さも見る (#2998)。** upstream の paramDef に `maxLength` は無いので、
	// 128 文字を超える名前は `emoji.name` varchar(128) に入らず SQLSTATE 22001 が
	// 生のまま 500 になる。**利用者の入力で 5xx を立てない**のは申請経路
	// (`emojiapplication.service`) が既に採っている判断で、`admin/emoji/copy` も
	// #2998 で同じ形にした。ここだけ残すと同じ入力が endpoint 次第で 400 と 500 に
	// 分かれる。
	if !emojiNamePattern.MatchString(req.Name) || utf8.RuneCountInString(req.Name) > emojiNameMaxRunes {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid emoji name.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// **`idGen` もここで見る (#2999)。** `Generate` は複製を作った**後**に呼ぶので、
	// nil のまま到達すると panic し、誰からも参照されない複製が残る。
	// `CreateFromApplication` が同じ理由で冒頭に置いている。
	if h.emojiRepo == nil || h.idGen == nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// fileId 経路は drive_file を resolve する (NO_SUCH_FILE)。filetype 検証は
	// upstream の error 順序 (noSuchFile → duplicate → unsupportedFileType) に
	// 合わせ、duplicate チェックの後で行う。
	var driveFile *model.DriveFile
	url := req.URL
	if url == "" && req.FileID != "" && h.driveFileRepo != nil {
		f, err := h.driveFileRepo.FindByID(req.FileID)
		if err != nil && !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return apierr.JSONInternalError(c)
		}
		if err != nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "No such file.", "fc46b5a4-6b92-4c33-ac66-b806659bb5cf"))
		}
		driveFile = f
	}
	if url == "" && driveFile == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "url or fileId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// 同名のローカル emoji が既に存在する場合は DUPLICATE_NAME (upstream 互換)。
	// **重複チェックを DB 障害で skip しない** (#2792)。
	existing, dupErr := h.emojiRepo.FindByNameAndHost(req.Name, nil)
	if dupErr != nil && !repository.IsNotFound(dupErr) {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if dupErr == nil && existing != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("DUPLICATE_NAME", "Duplicate name.", "f7a3462c-4e6e-4069-8421-b9bd4f4c3975"))
	}
	// **name 以外の列も見る (#3018)。** `category` varchar(128) /
	// `license` varchar(1024) を超える値はここを抜けると `Create` で
	// SQLSTATE 22001 になり、**利用者の入力で 5xx が立つ**。名前だけ長さを見て
	// いたのは #2998 の片手落ちで、`admin/emoji/copy` は 4 列すべて見ている。
	//
	// **upstream が宣言する error より後ろに置く。** `add.ts` の
	// `noSuchFile` / `duplicateName` を先に返さないと、2 つ問題があるリクエストで
	// drop-in クライアントの分岐が変わる (`EmojiUpdate` も同じ理由で絵文字の解決より
	// 後ろに置いてある)。
	//
	// **`unsupportedFileType` より前になるのは、置ける場所が無いから。** MIME の検査と
	// 複製は同じ `if driveFile != nil` ブロックにあり、**ブロックの中へ入れると `url`
	// 経路が検査から外れ、ブロックの外 (後ろ) へ出すと複製の後ろになる** — 後者は
	// 400 のたびに誰からも参照されない system 所有の複製が残る (#2999 / #3014)。
	// どちらも 400 の client error なので、この 2 つの順序が入れ替わってもクライアント
	// から見た意味は変わらない。
	if !emojiBodyFits(req.Category, emojiCategoryMaxRunes) {
		return emojiValueTooLong(c, "category")
	}
	if !emojiBodyFits(req.License, emojiLicenseMaxRunes) {
		return emojiValueTooLong(c, "license")
	}
	// **`url` 直接指定も見る (#3018)。** `emoji.originalUrl` / `publicUrl` は
	// varchar(512) で、この経路は利用者の文字列をそのまま両方へ入れる。
	// **`fileId` 経路は複製を作った後に見る** (#3023)。あちらに入るのは drive が
	// 作った URL で、保存先が決めるまで長さが分からないため。
	// **切らない** — 途中で切った URL は別物で、取りに行っても無駄なうえ壊れた
	// 参照を保存することになる (`colfit` の doc と同じ判断)。
	if url != "" && !colfit.Fits(url, emojiURLMaxRunes) {
		return emojiValueTooLong(c, "url")
	}
	// 配列も**複製より前**に確定させる。全部落ちたら弾く (#3018)。
	aliases, ok := fitEmojiAliases(req.Aliases)
	if !ok {
		return emojiValueTooLong(c, "aliases")
	}
	roleIDs, ok := fitEmojiRoleIDs(req.RoleIDs)
	if !ok {
		return emojiValueTooLong(c, "roleIdsThatCanBeUsedThisEmojiAsReaction")
	}
	// drive 画像なら MIME を allowlist で検証し、webpublic variant を優先して
	// originalUrl/publicUrl/type を導出する (upstream CustomEmojiService.add)。
	publicURL := url
	var fileType *string
	systemFileID := ""
	if driveFile != nil {
		if !isAllowedEmojiImageType(driveFile.Type) {
			return c.JSON(http.StatusBadRequest, apierr.Error("UNSUPPORTED_FILE_TYPE", "Unsupported file type.", "f7599d96-8750-af68-1633-9575d625c1a7"))
		}
		// **操作者の drive ファイルを参照し続けない (#2999)。** `fileId` で渡るのは
		// ふつうモデレーター自身が直前に上げたファイルで、本人が drive から消せば
		// 絵文字の画像が壊れる (ロールの変更や退会でも同じ)。申請の承認は #2966、
		// リモートの複製と zip の取り込みは #670 で既に system 所有にしてある。
		// **`admin/emoji/update` の `fileId` も #3014 で同じ形にした** — 差し替えた
		// 先も複製してから参照するので、REST の経路はこれで揃っている。
		//
		// **元のファイルは触らない** — ノートの添付やプロフィールで使っている
		// 可能性があり、所有権を移すと利用者の drive から突然消える。
		//
		// **HTTP は経由しない** (`CopyToSystemFile`)。自分の公開 URL を叩くのは
		// SSRF ガードと衝突し、非公開 URL の構成では取れない (#2966 と同じ判断)。
		//
		// **複製は検証をすべて通した後。** 先に複製すると、重複名や MIME で弾く
		// 分まで実体を作って捨てることになる (upstream の `copy.ts` がその形で、
		// `DUPLICATE_NAME` のたびに孤児を残す)。
		src := driveFile
		if h.emojiImageFetcher != nil && !coredrive.IsSystemOwned(driveFile) {
			// **読む前に行の `size` で断る (レビュー M3/L7)。** 複製は実体を丸ごと
			// メモリへ読むので、上限超過を読み切ってから 400 にするのは無駄。申請経路も
			// `emojiapplication.checkFile` が同じ規則を申請の時点で掛けている。
			//
			// **発火するのは role policy の `maxFileSizeMb` を 32 MiB より上へ
			// 設定した (ことがある) 構成だけ** — 既定は 30 MB なので upload できた
			// 時点で必ず上限内だが、一度上げて入れた行は上限を戻しても残る。
			// **一括登録の同時実行 (同梱 frontend は最大 100 件を一度に投げる) は
			// これでは軽くならない** — 1 件あたりのピークは上限のままで、rate limit か
			// queue 化が要る (別 issue)。
			//
			// **`size` は行に書いてあるだけで実体とずれうる** (TS 由来の行や手で直した
			// 行)。**過小申告は読み出し側の `safehttp.ErrResponseTooLarge` が捕まえる**
			// が、**過大申告はここで確定する** (実体を読まずに 400 になる)。
			if int64(driveFile.Size) > MaxEmojiCopyBytes {
				return c.JSON(http.StatusBadRequest, apierr.Error(
					"EMOJI_IMAGE_TOO_LARGE", "The image is too large to register as an emoji.",
					"6b1d5f0a-3c9e-4f27-9a4d-7e2b8c1f0d64"))
			}
			ctx := c.Request().Context()
			copied, cerr := h.emojiImageFetcher.CopyToSystemFile(ctx, driveFile, req.Name, req.IsSensitive)
			if cerr == nil && copied == nil {
				// 実装の契約違反。ここで気付かないと下の `src` 参照で panic する。
				cerr = errors.New("copy returned no file")
			}
			if cerr != nil {
				slog.WarnContext(ctx, "emoji add: drive copy failed",
					"fileId", req.FileID, "name", req.Name, "err", cerr)
				// **絵文字を作らない。** 元ファイルを参照して作ると、直そうと
				// している依存をそのまま残すことになる。
				return emojiCopyFailureResponse(c, cerr, "fc46b5a4-6b92-4c33-ac66-b806659bb5cf")
			}
			// **複製した実体の MIME を見る (#2966 と同じ理由)。** `Upload` は
			// `AnalyseFile` でバイト列から型を引き直すので、行の宣言と実体が
			// ずれていると allowlist 外の型が絵文字として登録される。
			// **弾いたら片付ける** — その時点で誰からも参照されない。
			if !isAllowedEmojiImageType(copied.Type) {
				h.deleteSystemEmojiFile(ctx, copied.ID)
				return c.JSON(http.StatusBadRequest, apierr.Error("UNSUPPORTED_FILE_TYPE", "Unsupported file type.", "f7599d96-8750-af68-1633-9575d625c1a7"))
			}
			src = copied
			systemFileID = copied.ID
		}
		// **URL が列に入るかを見る (#3023)。** 複製の URL は保存先が決めるので、
		// 作ってからでないと分からない。**弾いたら片付ける** — その時点で誰からも
		// 参照されない (MIME の再検査と同じ形)。
		if !emojiFileURLFits(src) {
			h.deleteSystemEmojiFile(c.Request().Context(), systemFileID)
			return emojiFileURLTooLong(c)
		}
		// 不変条件 (#722): `emoji.originalUrl` は必ず `drive_file.url` と一致
		// させる。`DriveFileRepository.DeleteOrphans` の guard が
		// `NOT EXISTS (emoji.originalUrl = drive_file.url ...)` で system 所有の
		// 絵文字画像を保護しているので、webpublic を入れると guard が外れる。
		url = src.URL
		publicURL = preferWebpublicURL(src)
		fileType = preferWebpublicType(src)
	}
	now := time.Now()
	e := &model.Emoji{
		ID:          h.idGen.Generate(now),
		UpdatedAt:   &now,
		Name:        req.Name,
		OriginalURL: url,
		PublicURL:   publicURL,
		Type:        fileType,
		Category:    req.Category,
		// 列に入らない alias は要素ごと落とす (#3018)。
		Aliases:     model.StringArray(aliases),
		License:     req.License,
		IsSensitive: req.IsSensitive,
		LocalOnly:   req.LocalOnly,
		// 列に入らない role id は落とす (#3018)。`aliases` と同じ varchar(128)[]。
		RoleIDsThatCanBeUsedThisEmojiAsReaction: model.StringArray(roleIDs),
	}
	if err := h.emojiRepo.Create(e); err != nil {
		// **取り込んだものを片付ける (#2999)。** ここまで来た複製は誰からも
		// 参照されないので、残すと孤児になる。`DeleteOrphans` は
		// `admin/drive/cleanup` からしか走らないので、掃除するまで実体を食う。
		//
		// **ただし載ったかを読み直してから (#3019)。** INSERT が commit 済みで
		// ack だけ失われた場合に消すと、**名前が使用中のまま画像だけ無い**状態に
		// なり、同じ名前で登録し直しても `DUPLICATE_NAME` で弾かれる。
		h.cleanupUnreferencedEmojiCopy(c.Request().Context(), e.ID, systemFileID, e.OriginalURL)
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	h.logModeration(c, moderationlog.LogAddCustomEmoji, map[string]any{
		"emojiId": e.ID,
		"emoji":   e,
	})
	// local emoji の追加を broadcast stream へ流し emoji picker を live-refresh (#2046)。
	h.publishEmojiAdded(e)
	// upstream は EmojiDetailed (packDetailed) を返す。raw model.Emoji を返すと
	// `url` が欠落し originalUrl/publicUrl/uri/type 等の内部 field が漏れる。
	return c.JSON(http.StatusOK, entity.PackEmojiDetailed(e))
}

// emojiCopyFailureResponse maps a CopyToSystemFile failure onto the wire.
//
// **`EMOJI_IMAGE_TOO_LARGE` と 500 は承認経路 (`emojiApplicationReviewError`) と
// 同じ error id を返す。** 同じ複製が同じ理由で失敗したのに endpoint ごとに id が
// 違うと、クライアントは経路ごとに分岐を持つことになる。**`NO_SUCH_FILE` だけは
// 例外**で、upstream が endpoint ごとに別の id を宣言しているため呼び出し元から
// 渡す (下記)。
//
// **種別を潰さない (#2792)。** 全部 500 にすると、選び直せば済むもの
// (実体がもう無い / 大きすぎる) が運用側に直しようのない 5xx になる。逆に全部
// 400 にすると、ストレージや DB の障害が client error に化けて監視でも 5xx が
// 立たない。
//
// **`NO_SUCH_FILE` の id だけは呼び出し元から渡す。** upstream は endpoint ごとに
// 別の id を宣言しており (`add.ts` は `fc46b5a4…`、`update.ts` は `14fb9fd9…`)、
// 共有すると**同じ endpoint が同じ code に 2 つの id を返す**ことになる
// (`update` は drive 行が無いときに自分の id を返すので)。`error.id` で分岐する
// クライアントは片方を取りこぼす。
func emojiCopyFailureResponse(c echo.Context, err error, noSuchFileID string) error {
	switch {
	case errors.Is(err, coredrive.ErrObjectNotFound):
		// 行はあるのに実体が無い (`isLink` / `accessKey` 無し / ストレージから
		// 消えた)。別のファイルを選べば済むので 400。
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"NO_SUCH_FILE", "No such file.", noSuchFileID))
	case errors.Is(err, safehttp.ErrResponseTooLarge):
		// 複製の上限 (32 MiB) を超えている。drive が受け取る上限 (role policy の
		// `maxFileSizeMb`) はこれより上へ設定できるので、**upload はできたのに
		// 絵文字にはできない**帯がありうる。
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"EMOJI_IMAGE_TOO_LARGE", "The image is too large to register as an emoji.",
			"6b1d5f0a-3c9e-4f27-9a4d-7e2b8c1f0d64"))
	}
	return c.JSON(http.StatusInternalServerError, apierr.Error(
		"INTERNAL_ERROR", "Failed to copy emoji image.",
		"c2f7a3d1-58be-4e09-bb26-0d4a9e7f3c15"))
}

// emojiNamePattern mirrors upstream admin/emoji/add の paramDef name pattern。
var emojiNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// allowedEmojiImageTypes mirrors upstream FILE_TYPE_IMAGE (const.ts)。
// image/svg+xml は XSS 理由で意図的に除外する。prefix 判定 ("image/") では
// svg や任意 subtype を通してしまうため、明示 allowlist で完全一致判定する。
// **集合は core/emojiapplication が持つ。** 申請側 (checkFile) と承認側が別々に
// 持つと、片方が prefix 判定になった瞬間に「申請は通るのに承認で落ちる」形に
// なる (レビュー R3)。
func isAllowedEmojiImageTypeShared(mime string) bool {
	return emojiapplication.IsAllowedImageType(mime)
}

// isAllowedEmojiImageType reports whether a drive file MIME may back a custom
// emoji. Empty type is rejected (upstream FILE_TYPE_IMAGE.includes("")===false)。
func isAllowedEmojiImageType(mime string) bool {
	return isAllowedEmojiImageTypeShared(mime)
}

// preferWebpublicURL returns the drive file's webpublic URL when present,
// else its canonical URL (upstream `webpublicUrl ?? url`).
//
// **実体は core/drive が持つ。** 既存データを直すバッチ (#2990) が同じ導出を
// するので、api 層に閉じたままだと再実装になる。
func preferWebpublicURL(f *model.DriveFile) string {
	return coredrive.PreferWebpublicURL(f)
}

// preferWebpublicType returns the drive file's webpublic MIME when present,
// else its canonical type (upstream `webpublicType ?? type`). Returned as a
// pointer so a non-empty value lands in emoji.type (NULL when both empty).
func preferWebpublicType(f *model.DriveFile) *string {
	return coredrive.PreferWebpublicType(f)
}

// EmojiUpdate handles POST /api/admin/emoji/update.
//
// Misskey TS の admin/emoji/update は name/category/aliases に加え license/
// isSensitive/localOnly も受け付ける。フロントの編集ダイアログがこれら全部
// 送信するため Request struct を Misskey 互換に拡張する (#650 問題 2)。
//
// `fileId` 経路は**画像を system 所有の drive ファイルへ複製してから**参照する
// (#3014)。upstream は渡された drive ファイルの URL をそのまま入れるので、意図的な
// 乖離 (docs/divergence.md §7)。`admin/emoji/add` の #2999 と同じ形。
//
// Aliases は []string なので nil (省略) と [] (空配列) が型で区別できない。
// Misskey TS 側も optional `?` で undefined と空配列を区別しないため、
// nil != 空配列でない場合に "aliases を全削除" として扱う。実装上は
// `req.Aliases != nil` で「フロントが aliases フィールドを明示送信した」
// と判定しており、空配列送信なら全削除、フィールド欠落なら現状維持。
func (h *Handler) EmojiUpdate(c echo.Context) error {
	var req struct {
		ID          string   `json:"id"`
		Name        *string  `json:"name"`
		FileID      *string  `json:"fileId"`
		Category    *string  `json:"category"`
		Aliases     []string `json:"aliases"`
		License     *string  `json:"license"`
		IsSensitive *bool    `json:"isSensitive"`
		LocalOnly   *bool    `json:"localOnly"`
		RoleIDs     []string `json:"roleIdsThatCanBeUsedThisEmojiAsReaction"`
	}
	// upstream update.ts paramDef は allOf+anyOf で {id} または {name} の
	// どちらか必須。id 無し name 指定時は local emoji を name で解決する (#1543)。
	if err := c.Bind(&req); err != nil || (req.ID == "" && (req.Name == nil || *req.Name == "")) {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "id or name is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.emojiRepo == nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// upstream update.ts:91-95 は emoji 解決より先に fileId の driveFile を検証し、
	// null なら NO_SUCH_FILE を返す (#1772。emoji 不在 AND fileId 不正が同時のとき
	// upstream は NO_SUCH_FILE を優先する)。ここで解決した file を後段の URL 差し替え
	// でも再利用する。
	var emojiFile *model.DriveFile
	if req.FileID != nil && *req.FileID != "" {
		if h.driveFileRepo == nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
		f, ferr := h.driveFileRepo.FindByID(*req.FileID)
		if ferr != nil && !repository.IsNotFound(ferr) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return apierr.JSONInternalError(c)
		}
		if ferr != nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "No such file.", "14fb9fd9-0731-4e2f-aeb9-f09e4740333d"))
		}
		emojiFile = f
	}
	// before snapshot for moderation log (also used to gate NO_SUCH_EMOJI when
	// the row is missing — UpdateFields の RowsAffected==0 経路は repo 側で
	// ErrRecordNotFound に昇格済み (#650 問題 2)).
	// id 指定があれば id 解決、無ければ name で local emoji を解決する。upstream
	// CustomEmojiService.update は `data.id ? getEmojiById : getEmojiByName` (#1543)。
	var before *model.Emoji
	var err error
	if req.ID != "" {
		before, err = h.emojiRepo.FindByID(req.ID)
	} else {
		before, err = h.emojiRepo.FindByNameAndHost(*req.Name, nil)
	}
	if err != nil || before == nil {
		// #729: 再現報告のため request 側 id と原因 (find vs update path) を
		// log に残す。frontend 側で stale id を握っているのか、repo 側で
		// 何か起きているのか切り分けに使う。
		slog.WarnContext(c.Request().Context(), "EmojiUpdate: NO_SUCH_EMOJI on lookup",
			"id", req.ID, "err", err)
		return c.JSON(http.StatusNotFound, apierr.NoSuchEmoji())
	}
	// 以降の UpdateFields / moderation log は解決済み id を使う。
	req.ID = before.ID
	// name 変更判定用に旧 name を UpdateFields 前に控える (#2046 broadcast 用。
	// GORM では before は別 struct だが、in-place 更新する実装に依らず安全側)。
	oldName := before.Name
	fields := map[string]any{}
	if req.Name != nil {
		// リネーム時は同名 local emoji の重複を弾く (SAME_NAME_EMOJI_EXISTS)。
		if *req.Name != before.Name {
			// **新しい名前に pattern を掛ける。これは upstream より厳しい。**
			// upstream の paramDef は `anyOf[{id 必須}, {name 必須 + pattern}]`
			// なので、**`id` を渡すと name の pattern は検査されない** —
			// frontend の通常の呼び方がまさにそれ。結果として `:foo bar:` の
			// ような名前が保存でき、AP で broadcast される
			// (`docs/divergence.md` に意図的乖離として記載)。
			//
			// **名前が変わらないときは掛けない。** frontend は名前を編集して
			// いなくても `name` を必ず送るので、掛けると**既に非準拠な名前で
			// 保存されている絵文字のカテゴリやライセンスが編集できなくなる**。
			// そういう行は **TS から引き継いだ DB に残りうる** — upstream の
			// `admin/emoji/update` は `id` を渡す経路で pattern を掛けないので、
			// 非準拠な名前のローカル絵文字が作れてしまう。**AP 経路は根拠にならない**
			// (作るのは remote 行だけで、ここが守るのはローカル)。`EmojiCopy` は
			// #2998 で塞いだので、mk-go が新しく作る経路はもう無い。塞ぎたいのは
			// 「不正な名前を新しく入れる」ことで、既に保存済み・broadcast 済みの
			// 名前を保存し直すのを拒む価値は無い。
			//
			// **長さも見る (#3018)。** pattern は文字種しか縛らないので、129 文字の
			// ASCII 名は素通りして `UpdateFields` が SQLSTATE 22001 で落ちる。
			// `admin/emoji/add` と `copy` は既に同じ値 (`emojiNameMaxRunes`) を見ており、
			// **名前は切らずに弾く** — 切ると別の絵文字になる。
			if !emojiNamePattern.MatchString(*req.Name) || utf8.RuneCountInString(*req.Name) > emojiNameMaxRunes {
				return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid emoji name.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
			}
			// **重複チェックを DB 障害で skip しない** (#2792)。
			dup, derr := h.emojiRepo.FindByNameAndHost(*req.Name, nil)
			if derr != nil && !repository.IsNotFound(derr) {
				return c.JSON(http.StatusInternalServerError, apierr.InternalError())
			}
			if derr == nil && dup != nil && dup.ID != req.ID {
				return c.JSON(http.StatusBadRequest, apierr.Error("SAME_NAME_EMOJI_EXISTS", "Emoji with the same name already exists.", "7180fe9d-1ee3-bff9-647d-fe9896d2ffb8"))
			}
		}
		fields["name"] = *req.Name
	}
	// **列に入らない値を複製より前に弾く (#3018)。** `category` varchar(128) /
	// `license` varchar(1024) を超える値はここを抜けると `UpdateFields` が
	// SQLSTATE 22001 で落ち、**利用者の入力で 5xx が立つ**。#3014 より前は
	// `UpdateFields` のエラーを種別を問わず 404 に丸めていたので「そんな絵文字は
	// 無い」に化けていた。
	//
	// **複製より前**でなければならない — 後ろに置くと、400 のたびに誰からも
	// 参照されない system 所有の複製が残る (#2999 / #3014 と同じ理由)。
	// upstream の error 順序 (NO_SUCH_FILE → NO_SUCH_EMOJI) は変えないので、
	// 絵文字の解決より後に置いてある。
	if !emojiBodyFits(req.Category, emojiCategoryMaxRunes) {
		return emojiValueTooLong(c, "category")
	}
	if !emojiBodyFits(req.License, emojiLicenseMaxRunes) {
		return emojiValueTooLong(c, "license")
	}
	// 配列も**複製より前**に確定させる。全部落ちたら弾く (#3018) — 空配列を書くのは
	// 「全消去」なので、落ちた結果を 204 で返すと既存の値を黙って消すことになる。
	aliases, ok := fitEmojiAliases(req.Aliases)
	if !ok {
		return emojiValueTooLong(c, "aliases")
	}
	roleIDs, ok := fitEmojiRoleIDs(req.RoleIDs)
	if !ok {
		return emojiValueTooLong(c, "roleIdsThatCanBeUsedThisEmojiAsReaction")
	}
	// fileId 指定時は drive の画像で URL を差し替える (upstream 互換)。file は
	// emoji 解決前に検証済み (emojiFile)、NO_SUCH_FILE はそこで返している (#1772)。
	systemFileID := ""
	systemFileURL := ""
	if emojiFile != nil {
		f := emojiFile
		if !isAllowedEmojiImageType(f.Type) {
			return c.JSON(http.StatusBadRequest, apierr.Error("UNSUPPORTED_FILE_TYPE", "Unsupported file type.", "f7599d96-8750-af68-1633-9575d625c1a7"))
		}
		// **差し替えた先も system 所有にする (#3014)。** ここに来る `fileId` は
		// ふつうモデレーター自身が直前に上げたファイルで、本人が drive から消せば
		// 絵文字の画像が壊れる (ロールの変更や退会でも同じ)。`admin/emoji/add` は
		// #2999、申請の承認は #2966、リモートの複製と zip の取り込みは #670 で
		// 既に system 所有にしてあり、**REST の経路ではここが最後の 1 つ**だった。
		// 回避策も無い — 画像を選ぶ画面が叩く `drive/files` は呼び出し元自身の
		// ファイルしか返さないので、「system 所有のファイルを選ぶ」ことはできない。
		//
		// **差し替える前に指していたファイルは消さない。** 元が既に system 所有なら
		// 複製しない規則 (#2999) がある以上、同じファイルを別の絵文字が指している
		// 可能性があり、`EmojiRepository` に `originalUrl` の完全一致引きは無く
		// (`ListV2` が持つのは `publicUrl` の部分一致だけ)、足しても「引いた後・
		// 消す前」のレースは残る。`admin/emoji/delete` も絵文字の行だけ消して
		// drive ファイルは残す (`admin/drive/cleanup` が回収する) ので、ここだけ
		// 消すと経路ごとに後始末の意味が変わる。孤児 guard (`orphanWhere`) は
		// `emoji.originalUrl = drive_file.url` または `publicUrl = url` で参照を
		// 判定するので、**参照が外れたものだけ**が回収対象になり、共有中のものは
		// 守られる。元が利用者
		// 所有ならそもそも触ってはいけない (ノートの添付やプロフィールで使っている
		// 可能性がある)。代償は「cleanup を回すまで実体を食う」ことだけで、これは
		// `admin/emoji/delete` が既に持っている性質と同じ。
		//
		// 検証をすべて通した後に複製する / 複製した実体の MIME をもう一度見る /
		// 行の `size` で先に断る、はどれも `EmojiAdd` と同じ理由 (#2999)。
		src := f
		if h.emojiImageFetcher != nil && !coredrive.IsSystemOwned(f) {
			// **読む前に行の `size` で断る。** 複製は実体を丸ごとメモリへ読むので、
			// 上限超過を読み切ってから 400 にするのは無駄。過小申告は読み出し側の
			// `safehttp.ErrResponseTooLarge` が捕まえる。
			if int64(f.Size) > MaxEmojiCopyBytes {
				return c.JSON(http.StatusBadRequest, apierr.Error(
					"EMOJI_IMAGE_TOO_LARGE", "The image is too large to register as an emoji.",
					"6b1d5f0a-3c9e-4f27-9a4d-7e2b8c1f0d64"))
			}
			// **複製に渡すのは更新後の値。** 同じリクエストで `name` /
			// `isSensitive` を変えていればそちらが載るので、省略されたときだけ
			// 現在の絵文字の値を使う。
			copyName := before.Name
			if req.Name != nil {
				copyName = *req.Name
			}
			copySensitive := before.IsSensitive
			if req.IsSensitive != nil {
				copySensitive = *req.IsSensitive
			}
			ctx := c.Request().Context()
			copied, cerr := h.emojiImageFetcher.CopyToSystemFile(ctx, f, copyName, copySensitive)
			if cerr == nil && copied == nil {
				// 実装の契約違反。ここで気付かないと下の `src` 参照で panic する。
				cerr = errors.New("copy returned no file")
			}
			if cerr != nil {
				slog.WarnContext(ctx, "emoji update: drive copy failed",
					"emojiId", req.ID, "fileId", *req.FileID, "err", cerr)
				// **絵文字を書き換えない。** 元ファイルを指して更新すると、直そうと
				// している依存をそのまま作ることになる。差し替え前の画像は生きて
				// いるので、失敗しても表示は壊れない。
				// **`NO_SUCH_FILE` は `update.ts` の id で返す** — 上の drive 行が
				// 無いときと同じ endpoint・同じ code なので、id を分けると
				// クライアントが片方を取りこぼす。
				return emojiCopyFailureResponse(c, cerr, "14fb9fd9-0731-4e2f-aeb9-f09e4740333d")
			}
			// **複製した実体の MIME を見る (#2966 と同じ理由)。** `Upload` は
			// `AnalyseFile` でバイト列から型を引き直すので、行の宣言と実体が
			// ずれていると allowlist 外の型が絵文字として登録される。
			// **弾いたら片付ける** — その時点で誰からも参照されない。
			if !isAllowedEmojiImageType(copied.Type) {
				h.deleteSystemEmojiFile(ctx, copied.ID)
				return c.JSON(http.StatusBadRequest, apierr.Error("UNSUPPORTED_FILE_TYPE", "Unsupported file type.", "f7599d96-8750-af68-1633-9575d625c1a7"))
			}
			src = copied
			systemFileID = copied.ID
			systemFileURL = copied.URL
		}
		// **URL が列に入るかを見る (#3023)。** 弾いたら複製を片付ける。
		if !emojiFileURLFits(src) {
			h.deleteSystemEmojiFile(c.Request().Context(), systemFileID)
			return emojiFileURLTooLong(c)
		}
		// upstream update.ts: originalUrl=url, publicUrl=webpublicUrl??url,
		// fileType=webpublicType??type。EmojiAdd / EmojiCopy と同ロジック。
		//
		// 不変条件 (#722): `originalUrl` は必ず `drive_file.url` と一致させる。
		// 孤児 cleanup の guard が `NOT EXISTS (emoji.originalUrl = drive_file.url
		// ...)` で system 所有の絵文字画像を保護しているので、webpublic を入れると
		// guard が外れる。
		fields["originalUrl"] = src.URL
		fields["publicUrl"] = preferWebpublicURL(src)
		if t := preferWebpublicType(src); t != nil {
			fields["type"] = *t
		}
	}
	if req.Category != nil {
		fields["category"] = *req.Category
	}
	if req.Aliases != nil {
		// `[]string` を直接 GORM に渡すと PostgreSQL の varchar[] 列に対して
		// NULL として serialize される (#729)。`model.StringArray` で wrap する
		// と空 slice 含めて `'{}'` PostgreSQL array リテラルに正しく変換さ
		// れる。Aliases 列は NOT NULL DEFAULT '{}' なので NULL 書き込みは
		// 即制約違反でエラーになっていた。
		// 正規化は上で済ませてある。**nil にはしない** — `req.Aliases != nil` が
		// 「明示送信した」の判定なので、nil に戻すと「省略」に化ける。
		fields["aliases"] = model.StringArray(aliases)
	}
	if req.License != nil {
		fields["license"] = *req.License
	}
	if req.IsSensitive != nil {
		fields["isSensitive"] = *req.IsSensitive
	}
	if req.LocalOnly != nil {
		fields["localOnly"] = *req.LocalOnly
	}
	// リアクション利用可能ロールの更新 (upstream の roleIdsThatCanBeUsedThisEmojiAsReaction)。
	if req.RoleIDs != nil {
		fields["roleIdsThatCanBeUsedThisEmojiAsReaction"] = model.StringArray(roleIDs)
	}
	if len(fields) == 0 {
		// 何も変更しないリクエストは log を書かずに 204 で返す。
		return c.NoContent(http.StatusNoContent)
	}
	if err := h.emojiRepo.UpdateFields(req.ID, fields); err != nil {
		if !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない (#2792)。** 接続断が「そんな絵文字は
			// 無い」に化けると、クライアントからは区別できず監視でも 5xx が立たない。
			slog.ErrorContext(c.Request().Context(), "EmojiUpdate: UpdateFields failed",
				"id", req.ID, "fields", fieldKeys(fields), "systemFileId", systemFileID, "err", err)
			h.cleanupUnreferencedEmojiCopy(c.Request().Context(), req.ID, systemFileID, systemFileURL)
			return apierr.JSONInternalError(c)
		}
		// #729: FindByID 直後の RowsAffected==0 はほぼ起こらない (concurrent
		// delete 等のレース) ので診断 log で気付けるようにする。
		slog.WarnContext(c.Request().Context(), "EmojiUpdate: NO_SUCH_EMOJI on UpdateFields",
			"id", req.ID, "fields", fieldKeys(fields), "err", err)
		// **載らなかった複製を片付ける (#3014)。** 行がもう無い (= `RowsAffected==0`
		// の昇格を含む) ので更新は載っておらず、絵文字は差し替え前の URL を指した
		// まま。作った複製は誰からも参照されない孤児として残る。
		h.deleteSystemEmojiFile(c.Request().Context(), systemFileID)
		return c.JSON(http.StatusNotFound, apierr.NoSuchEmoji())
	}
	after, err := h.emojiRepo.FindByID(req.ID)
	if err != nil {
		// ここに到達するのは UpdateFields 直後の row 消失 (race) のみ。
		// 操作自体は成功しているので 204 を返す。
		return c.NoContent(http.StatusNoContent)
	}
	h.logModeration(c, moderationlog.LogUpdateCustomEmoji, map[string]any{
		"emojiId": req.ID,
		"before":  before,
		"after":   after,
	})
	// broadcast stream へ流す (#2046)。upstream は name 変更時のみ
	// emojiDeleted(old)+emojiAdded(new)、それ以外は emojiUpdated(after)。
	if req.Name != nil && *req.Name != oldName {
		h.publishEmojiDeleted(before)
		h.publishEmojiAdded(after)
	} else {
		h.publishEmojiUpdated(after)
	}
	return c.NoContent(http.StatusNoContent)
}

// cleanupUnreferencedEmojiCopy deletes a system-owned copy unless the emoji row
// ended up referencing it (#3014 / #3019).
//
// **エラーが返っても書き込みが載っていることがある。** PostgreSQL は COMMIT を送った
// 後・ack が届く前に接続が切れると、サーバー側は commit 済みなのにクライアントは
// エラーを受け取る。載っているのに複製を消すと**絵文字が存在しないファイルを指す** —
// 差し替え (`update`) なら差し替え前の画像はもう参照されておらず戻らないし、
// 登録 (`add` / 承認) なら**名前が使用中のまま画像だけ無い**状態になり、同じ名前で
// 登録し直しても `DUPLICATE_NAME` で弾かれる (承認では申請が pending のまま残り、
// もう一度承認を押しても同じ理由で通らない)。
//
// **逆に、確実に載っていない失敗では孤児になる。** 列幅超過 (SQLSTATE 22001) や
// 一意制約違反はそれで、`admin/drive/cleanup` は手動でしか走らないので掃除するまで
// 実体を食う。
//
// **読み直して分ける。** 後始末バッチ (#2990) の `finishFailedUpdate` と同じ
// 3 分岐で、**読み直せないときは残す** — 参照されている複製を消すほうが、
// 参照されない複製を残すより悪い (後者は孤児 cleanup が回収する)。
//
// **承認の `approvalLanded` (#2966) は逆に倒している (読めなければ消す)。**
// 理由は「復旧できるか」ではなく**消せる範囲が違う**こと。あちらの後始末
// (`DeleteCreatedEmoji`) は**絵文字の行ごと**消せるので、消せば承認を押し直して
// 作り直せる (`TestApproveCleansUpWhenReadBackFails` がその判断を固定している)。
// こちらが決められるのは**複製の行方だけ**で、絵文字の行には触らない — 選べるのは
// 「画像が生きた絵文字」か「画像だけ 404 の絵文字」で、後者を選ぶ理由が無い。
//
// **「残せば復旧できる」ではない。** 承認経路では `Create` が失敗した時点で
// `Approve` が return するので申請は pending のまま残り、**そのまま押し直すと
// 複製を残しても消しても `DUPLICATE_NAME`**。差がつくのは画像が生きているか
// どうかだけ。
//
// **復旧はできる。** 審査画面は衝突している絵文字の id を出す
// (`packEmojiApplicationForModerator` の `nameConflict`) ので、それを
// `admin/emoji/delete` で消せば承認を押し直せる。**却下に倒さないこと** —
// 申請者の枠を消費したまま閉じることになる。
//
// **`copiedURL` で照合する。** 登録 (`add` / 承認) は id が新しいので「行があるか」
// だけでも判定できるが、差し替えは行が元からあるので URL を見ないと分からない。
// どちらの経路も `emoji.originalUrl` には複製の `url` をそのまま入れる (#722) ので、
// 同じ述語で足りる。
func (h *Handler) cleanupUnreferencedEmojiCopy(ctx context.Context, emojiID, fileID, copiedURL string) {
	// **`copiedURL` が空なら照合にならない** ので触らない (空同士が「一致」に見える)。
	// **現状の storage 実装では到達しない** — `Put` は成功すれば必ず非空の URL を
	// 返す。防御的に置いてあるだけで、倒す向きは「消さない」。後始末バッチの
	// `unrepairableFromRow` が同じ曖昧さを理由に複製対象から外している。
	if fileID == "" || copiedURL == "" {
		return
	}
	after, err := h.emojiRepo.FindByID(emojiID)
	switch {
	case err != nil && !repository.IsNotFound(err):
		// 読み直せないので載ったか分からない。残す。
		slog.WarnContext(ctx, "emoji: cannot tell whether the write landed; keeping the system copy",
			"emojiId", emojiID, "systemFileId", fileID, "err", err)
		return
	case err == nil && after != nil && after.OriginalURL == copiedURL:
		// **エラーは返ったが書き込みは載っていた。** 複製は参照されているので残す。
		//
		// **ログに残す。** 呼び出し元は 5xx を返すのに絵文字は実在する、という
		// 食い違いが起きている。運用側はこれを見ないと「失敗したはずなのに
		// `DUPLICATE_NAME` になる」の原因に辿り着けない。
		slog.WarnContext(ctx, "emoji: the write landed despite the error; keeping the system copy",
			"emojiId", emojiID, "systemFileId", fileID)
		return
	}
	// 行が無い / 別の URL を指している = 載っていない。複製は誰からも参照されない。
	h.deleteSystemEmojiFile(ctx, fileID)
}

// fieldKeys は map のキー一覧を sort 済み slice で返す診断 log 用 helper。
// log 行に value を直接含めると long string が混じって読みにくいので key
// のみ列挙する。
func fieldKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// EmojiDelete handles POST /api/admin/emoji/delete.
func (h *Handler) EmojiDelete(c echo.Context) error {
	var req struct {
		ID string `json:"id"`
	}
	if err := c.Bind(&req); err != nil || req.ID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "id is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.emojiRepo == nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// log info に snapshot を含めるため削除前に取得。取得失敗は NO_SUCH_EMOJI。
	snapshot, err := h.emojiRepo.FindByID(req.ID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_EMOJI", "No such emoji.", "be83669b-773a-44b7-b1f8-e5e5170ac3c2"))
	}
	if err := h.emojiRepo.Delete(req.ID); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_EMOJI", "No such emoji.", "be83669b-773a-44b7-b1f8-e5e5170ac3c2"))
	}
	h.logModeration(c, moderationlog.LogDeleteCustomEmoji, map[string]any{
		"emojiId": req.ID,
		"emoji":   snapshot,
	})
	// 削除を broadcast stream へ流し emoji picker から消す (#2046)。
	h.publishEmojiDeleted(snapshot)
	return c.NoContent(http.StatusNoContent)
}

// EmojiList handles POST /api/admin/emoji/list.
func (h *Handler) EmojiList(c echo.Context) error {
	var req struct {
		Query     string `json:"query"`
		Category  string `json:"category"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
		Limit     *int   `json:"limit"`
		Offset    int    `json:"offset"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.emojiRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	emojis, err := h.emojiRepo.ListWithFilter(req.Query, req.Category, true, sinceID, untilID, limit, req.Offset)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	return c.JSON(http.StatusOK, entity.PackEmojiDetailedList(emojis))
}

// EmojiListV2 handles POST /api/v2/admin/emoji/list.
// Returns an object with pagination info (allCount, allPages) instead of a plain array.
func (h *Handler) EmojiListV2(c echo.Context) error {
	var req struct {
		Query     *emojiV2QueryReq `json:"query"`
		SinceID   string           `json:"sinceId"`
		UntilID   string           `json:"untilId"`
		SinceDate *int64           `json:"sinceDate"`
		UntilDate *int64           `json:"untilDate"`
		Limit     *int             `json:"limit"`
		Page      int              `json:"page"`
		SortKeys  []string         `json:"sortKeys"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.emojiRepo == nil {
		return c.JSON(http.StatusOK, emojiListV2Response{
			Emojis:   []map[string]any{},
			Count:    0,
			AllCount: 0,
			AllPages: 0,
		})
	}

	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}

	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	filter := model.EmojiV2Filter{
		SinceID:  sinceID,
		UntilID:  untilID,
		Limit:    limit,
		Page:     req.Page,
		SortKeys: req.SortKeys,
	}
	if req.Query != nil {
		filter.Query = &model.EmojiV2Query{
			Name:          req.Query.Name,
			Host:          req.Query.Host,
			HostType:      req.Query.HostType,
			Category:      req.Query.Category,
			Type:          req.Query.Type,
			Aliases:       req.Query.Aliases,
			License:       req.Query.License,
			IsSensitive:   req.Query.IsSensitive,
			LocalOnly:     req.Query.LocalOnly,
			UpdatedAtFrom: req.Query.UpdatedAtFrom,
			UpdatedAtTo:   req.Query.UpdatedAtTo,
			RoleIDs:       req.Query.RoleIDs,
			URI:           req.Query.URI,
			PublicURL:     req.Query.PublicURL,
			OriginalURL:   req.Query.OriginalURL,
		}
	}

	emojis, err := h.emojiRepo.ListV2(filter)
	if err != nil {
		slog.Error("EmojiListV2: ListV2 failed", "error", err)
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	allCount, err := h.emojiRepo.CountV2(filter)
	if err != nil {
		slog.Error("EmojiListV2: CountV2 failed", "error", err)
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	allPages := 0
	if limit > 0 {
		allPages = int((allCount + int64(limit) - 1) / int64(limit))
	}

	roleByID := h.emojiRoleByID()
	packed := make([]map[string]any, 0, len(emojis))
	for _, e := range emojis {
		packed = append(packed, packEmojiDetailedAdmin(e, roleByID))
	}

	return c.JSON(http.StatusOK, emojiListV2Response{
		Emojis:   packed,
		Count:    len(packed),
		AllCount: allCount,
		AllPages: allPages,
	})
}

type emojiV2QueryReq struct {
	Name          string   `json:"name"`
	Host          string   `json:"host"`
	HostType      string   `json:"hostType"`
	Category      string   `json:"category"`
	Type          string   `json:"type"`
	Aliases       string   `json:"aliases"`
	License       string   `json:"license"`
	IsSensitive   *bool    `json:"isSensitive"`
	LocalOnly     *bool    `json:"localOnly"`
	UpdatedAtFrom string   `json:"updatedAtFrom"`
	UpdatedAtTo   string   `json:"updatedAtTo"`
	RoleIDs       []string `json:"roleIds"`
	// URI / PublicURL / OriginalURL は upstream Misskey の v2 検索 schema
	// で受け付けている filter (#466)。frontend (custom-emojis-manager.remote
	// .vue) の検索ボックスから送られてくるため、handler 側で受けないと
	// silently 無視される。受信したら repo の WHERE 句に展開する。
	URI         string `json:"uri"`
	PublicURL   string `json:"publicUrl"`
	OriginalURL string `json:"originalUrl"`
}

type emojiListV2Response struct {
	Emojis   []map[string]any `json:"emojis"`
	Count    int              `json:"count"`
	AllCount int64            `json:"allCount"`
	AllPages int              `json:"allPages"`
}

// emojiRoleByID resolves all roles by id once, so the v2 emoji packer can embed
// {id, name} for roleIdsThatCanBeUsedThisEmojiAsReaction without an N+1 lookup
// per emoji (#1300), and sort them by displayOrder/id like upstream (#1948-13)。
// roleService 未配線 / 取得失敗時は空 map を返し、未知 role 同様 roleIds から省かれる。
func (h *Handler) emojiRoleByID() map[string]*model.Role {
	out := map[string]*model.Role{}
	if h.roleService == nil {
		return out
	}
	roles, err := h.roleService.List()
	if err != nil {
		return out
	}
	for _, r := range roles {
		out[r.ID] = r
	}
	return out
}

// packEmojiDetailedAdmin builds the golden EmojiDetailedAdmin shape for the v2
// admin emoji list. upstream packDetailedAdmin と同じく roleIds を {id, name}
// に解決する (golden は object array、生の string[] ではない)。未知 role は
// 省く (#1300)。scalar field は model.Emoji の json tag と一致する。
func packEmojiDetailedAdmin(e *model.Emoji, roleByID map[string]*model.Role) map[string]any {
	// upstream packDetailedAdmin は解決した role を displayOrder DESC, id ASC で
	// sort してから {id, name} に map する (EmojiEntityService、#1948-13)。
	resolved := make([]*model.Role, 0, len(e.RoleIDsThatCanBeUsedThisEmojiAsReaction))
	for _, rid := range e.RoleIDsThatCanBeUsedThisEmojiAsReaction {
		if r, ok := roleByID[rid]; ok {
			resolved = append(resolved, r)
		}
	}
	sort.SliceStable(resolved, func(i, j int) bool {
		if resolved[i].DisplayOrder != resolved[j].DisplayOrder {
			return resolved[i].DisplayOrder > resolved[j].DisplayOrder // displayOrder DESC
		}
		return resolved[i].ID < resolved[j].ID // id ASC
	})
	roles := make([]map[string]any, 0, len(resolved))
	for _, r := range resolved {
		roles = append(roles, map[string]any{"id": r.ID, "name": r.Name})
	}
	// golden aliases は string[] 必須 (non-null)。nil model.StringArray は null に
	// なるため [] へ coalesce する。
	aliases := []string(e.Aliases)
	if aliases == nil {
		aliases = []string{}
	}
	return map[string]any{
		"id": e.ID,
		// upstream EmojiEntityService.ts:112 は updatedAt?.toISOString() ?? null。
		// raw *time.Time だと RFC3339Nano になるため .000Z/null に揃える (#1948-10)。
		"updatedAt":   entity.ISOMillisPtr(e.UpdatedAt),
		"name":        e.Name,
		"host":        e.Host,
		"publicUrl":   e.PublicURL,
		"originalUrl": e.OriginalURL,
		"uri":         e.URI,
		"type":        e.Type,
		"aliases":     aliases,
		"category":    e.Category,
		"license":     e.License,
		"localOnly":   e.LocalOnly,
		"isSensitive": e.IsSensitive,
		"roleIdsThatCanBeUsedThisEmojiAsReaction": roles,
	}
}

// --- Abuse Report endpoints ---

// AbuseReports handles POST /api/admin/abuse-user-reports.
//
// upstream paramDef (= packages/backend/src/server/api/endpoints/admin/
// abuse-user-reports.ts) で受ける主要 field を frontend (= MkPagination
// + pages/admin/abuses.vue) と互換にする:
//
//   - sinceId / untilId — cursor pagination (#1114 root cause)
//   - state             — 'all' | 'unresolved' | 'resolved'
//   - reporterOrigin    — 'combined' | 'local' | 'remote'
//   - targetUserOrigin  — 'combined' | 'local' | 'remote'
//   - limit             — default 10 / max 100
//
// 旧 mk-go は `resolved` boolean / `limit` / `offset` しか受けず、frontend が
// 送る untilId を無視して同じ first page を返し続けて MkPagination が末尾
// 検知できず無限ロードになっていた (= 過去 #487 / #488 / #491 / #493 / #520
// / #712 と完全に同 pattern)。
//
// 後方互換: 旧 `resolved` boolean field は引き続き受け付ける (= 旧 client /
// 既存 e2e 互換)。`state` と `resolved` の両方が来た場合は `state` を優先
// する (= upstream paramDef での enum 優先 pattern と同じ)。
func (h *Handler) AbuseReports(c echo.Context) error {
	var req struct {
		Resolved         *bool  `json:"resolved"`
		State            string `json:"state"`
		ReporterOrigin   string `json:"reporterOrigin"`
		TargetUserOrigin string `json:"targetUserOrigin"`
		SinceID          string `json:"sinceId"`
		UntilID          string `json:"untilId"`
		SinceDate        *int64 `json:"sinceDate"`
		UntilDate        *int64 `json:"untilDate"`
		Limit            *int   `json:"limit"`
		// ReportID は mk-go 独自の additive パラメータ (#2868)。通報の通知から
		// 該当の 1 件へ飛ぶために使う。
		ReportID string `json:"reportId"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.abuseRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// state enum 優先、無ければ legacy boolean fallback。'all' / 空文字列は
	// 「フィルタなし (= resolved 列で絞らない)」を意味する。
	resolved := req.Resolved
	switch req.State {
	case "unresolved":
		v := false
		resolved = &v
	case "resolved":
		v := true
		resolved = &v
	case "all", "":
		// 何もしない (= legacy boolean fallback または「全件」)
	default:
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "state must be 'all', 'unresolved', or 'resolved'.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// origin enum 検証: 空文字列 / 'combined' / 'local' / 'remote' 以外は reject
	// (= silent fallback で全件返すと「local だけ見たかった」が裏切られて
	// 静かに drop-in regression になるため)。
	if !isValidOrigin(req.ReporterOrigin) {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "reporterOrigin must be 'combined', 'local', or 'remote'.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if !isValidOrigin(req.TargetUserOrigin) {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "targetUserOrigin must be 'combined', 'local', or 'remote'.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// reportId 指定時はその 1 件だけを返す (#2868)。
	//
	// **state / origin / cursor の絞りを無視する。** 通知のリンクから開いた
	// ときに「他のモデレーターが先に解決済みにしたので一覧に出ない」が起きると、
	// リンクが役に立たない。1 件を名指しで引く以上、絞り込みは意味を持たない。
	if req.ReportID != "" {
		r, err := h.abuseRepo.FindByID(req.ReportID)
		if err != nil {
			// **DB 障害を not-found に丸めない** (#2792)。
			if !repository.IsNotFound(err) {
				return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
			}
			return c.JSON(http.StatusOK, []packedAbuseReport{})
		}
		if r == nil {
			return c.JSON(http.StatusOK, []packedAbuseReport{})
		}
		profByID := h.abuseUserProfiles([]*model.AbuseUserReport{r})
		return c.JSON(http.StatusOK, []packedAbuseReport{h.packAbuseReport(c.Request().Context(), r, profByID)})
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	reports, err := h.abuseRepo.List(resolved, req.ReporterOrigin, req.TargetUserOrigin, sinceID, untilID, limit)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// model.AbuseUserReport は createdAt 列を持たず aidx ID 先頭 8 文字に
	// timestamp を埋め込んでいる。frontend (MkAbuseReport.vue) は
	// `<MkTime :time="report.createdAt"/>` を直接読むため、aidx から派生
	// した createdAt 文字列を response に注入する。ShowModerationLogs と
	// 同パターン (#1116)。embedded struct で既存 field を JSON inline、
	// CreatedAt だけ上乗せする。
	// reporter / targetUser / assignee に紐づく profile を 1 batch で解決して
	// UserDetailed を pack する (N+1 回避)。
	profByID := h.abuseUserProfiles(reports)
	out := make([]packedAbuseReport, 0, len(reports))
	for _, r := range reports {
		out = append(out, h.packAbuseReport(c.Request().Context(), r, profByID))
	}
	return c.JSON(http.StatusOK, out)
}

// packAbuseReport converts an abuse report into the wire shape shared by
// admin/abuse-user-reports (list) and the abuseReportResolved system webhook
// body (#1723). profByID は abuseUserProfiles で batch 解決した profile map。
func (h *Handler) packAbuseReport(ctx context.Context, r *model.AbuseUserReport, profByID map[string]*model.UserProfile) packedAbuseReport {
	p := packedAbuseReport{
		ID:             r.ID,
		Comment:        r.Comment,
		Resolved:       r.Resolved,
		ReporterID:     r.ReporterID,
		TargetUserID:   r.TargetUserID,
		AssigneeID:     r.AssigneeID,
		Forwarded:      r.Forwarded,
		ResolvedAs:     r.ResolvedAs,
		ModerationNote: r.ModerationNote,
		// 生 model.User を inline すると usernameLower / inbox 等の内部
		// field が漏れ、UserDetailedNotMe 固有 field も欠ける。UserDetailed
		// で pack する。
		Reporter:   h.packAbuseUser(r.Reporter, profByID),
		TargetUser: h.packAbuseUser(r.TargetUser, profByID),
		Assignee:   h.packAbuseUser(r.Assignee, profByID),
	}
	if s, err := aidxCreatedAtString(h.idGen, r.ID); err == nil {
		p.CreatedAt = s
	} else if !errors.Is(err, ErrIDGenMissing) {
		// idGen は wired されているのに parse 失敗した場合のみログに残す。
		// 非 aidx 形式の legacy ID 等で createdAt が出せない時に frontend
		// 側で「日時の解析が失敗しました。」が出る原因を後追いできるように
		// する (ShowModerationLogs と同 pattern)。
		slog.DebugContext(ctx, "abuseReport: createdAt derive failed",
			"reportId", r.ID, "err", err)
	}
	return p
}

// abuseUserProfiles batch-fetches the profiles of every reporter / targetUser /
// assignee referenced by the reports, keyed by user ID (N+1 回避)。
func (h *Handler) abuseUserProfiles(reports []*model.AbuseUserReport) map[string]*model.UserProfile {
	if h.userRepo == nil {
		return nil
	}
	seen := map[string]struct{}{}
	ids := make([]string, 0, len(reports)*3)
	add := func(u *model.User) {
		if u == nil {
			return
		}
		if _, ok := seen[u.ID]; ok {
			return
		}
		seen[u.ID] = struct{}{}
		ids = append(ids, u.ID)
	}
	for _, r := range reports {
		add(r.Reporter)
		add(r.TargetUser)
		add(r.Assignee)
	}
	if len(ids) == 0 {
		return nil
	}
	profiles, err := h.userRepo.FindProfilesByUserIDs(ids)
	if err != nil {
		return nil
	}
	byID := make(map[string]*model.UserProfile, len(profiles))
	for _, p := range profiles {
		byID[p.UserID] = p
	}
	return byID
}

// packAbuseUser packs a related user as UserDetailed (= UserDetailedNotMe shape).
// Returns nil (-> JSON null) when u is nil so the assignee key stays present.
func (h *Handler) packAbuseUser(u *model.User, profByID map[string]*model.UserProfile) *entity.UserDetailed {
	if u == nil {
		return nil
	}
	d := h.packModeratorVisibleUser(u, profByID[u.ID])
	return &d
}

// packModeratorVisibleUser packs a user for a moderator-only response.
//
// **カウントのゲートを通す。** packer は `followersVisibility` /
// `followingVisibility` が public でないカウントを既定で伏せるので、通さないと
// モデレーター向けの画面で 0 になる。upstream `UserEntityService.pack` は
// `isMe || iAmModerator` に実数を返す。
func (h *Handler) packModeratorVisibleUser(u *model.User, profile *model.UserProfile) entity.UserDetailed {
	d := entity.PackUserDetailed(u, profile, h.idGen)
	entity.GateCountVisibility(&d, false, true, false)
	return d
}

// packedAbuseReport mirrors the upstream abuse-user-reports res schema
// (abuse-user-reports.ts:26-89) field-for-field. 旧実装は model.AbuseUserReport
// を embed して生 model.User の relation や targetUserHost/reporterHost 等の
// 内部 field を漏らしていた。明示 struct にして TS res と 1:1 に揃える。
// assignee/assigneeId/resolvedAs は nullable+optional:false なので key 常在
// (null when unset)。createdAt は aidx 由来で legacy 非 aidx ID では omit。
type packedAbuseReport struct {
	ID             string               `json:"id"`
	CreatedAt      string               `json:"createdAt,omitempty"`
	Comment        string               `json:"comment"`
	Resolved       bool                 `json:"resolved"`
	ReporterID     string               `json:"reporterId"`
	TargetUserID   string               `json:"targetUserId"`
	AssigneeID     *string              `json:"assigneeId"`
	Reporter       *entity.UserDetailed `json:"reporter"`
	TargetUser     *entity.UserDetailed `json:"targetUser"`
	Assignee       *entity.UserDetailed `json:"assignee"`
	Forwarded      bool                 `json:"forwarded"`
	ResolvedAs     *string              `json:"resolvedAs"`
	ModerationNote string               `json:"moderationNote"`
}

// isValidOrigin returns true if s is one of the valid origin filter values
// accepted by admin endpoints ('combined' / 'local' / 'remote') or empty.
// 'combined' と 空文字列は「フィルタなし」として等価扱い。
func isValidOrigin(s string) bool {
	switch s {
	case "", "combined", "local", "remote":
		return true
	default:
		return false
	}
}

// ResolveAbuseReport handles POST /api/admin/resolve-abuse-user-report.
//
// upstream paramDef:
//
//	reportId:    string (required)
//	resolvedAs:  enum ['accept', 'reject', null], nullable
//
// 旧 mk-go は `resolvedAs` を `"accept"` 固定にしていたため、admin が
// reject (= 「報告は不適切」判定) を選んでも accept として記録され、
// abuse handling の判定結果が反映されない drop-in regression があった
// (PR #1108 sweep)。本 fix で upstream paramDef 通り `resolvedAs` を
// accept し enum 検証する。
func (h *Handler) ResolveAbuseReport(c echo.Context) error {
	var req struct {
		ReportID   string  `json:"reportId"`
		ResolvedAs *string `json:"resolvedAs"`
	}
	if err := c.Bind(&req); err != nil || req.ReportID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "reportId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.abuseRepo == nil {
		return c.JSON(http.StatusNotFound, apierr.ErrorWithKind("NO_SUCH_ABUSE_REPORT", "No such abuse report.", "ac3794dd-2ce4-d878-e546-73c60c06b398", apierr.KindServer))
	}
	// upstream enum: ['accept', 'reject', null]。null は「未送出」+ JSON null
	// 両方を「resolve したが判定保留」として扱う (= column も null)。
	// 不正値は silent fallback せず 400 で reject (PR #1102 の RolesUpdate.target
	// fix と同方針: 既存 row の resolvedAs が意図せず書き換わる silent
	// corruption を防ぐ)。
	// upstream は resolve 時に assigneeId=moderator.id を記録する。
	fields := map[string]any{"resolved": true}
	if me := middleware.GetUser(c); me != nil {
		fields["assigneeId"] = me.ID
	}
	switch {
	case req.ResolvedAs == nil:
		fields["resolvedAs"] = nil
	case *req.ResolvedAs == "accept":
		fields["resolvedAs"] = "accept"
	case *req.ResolvedAs == "reject":
		fields["resolvedAs"] = "reject"
	case *req.ResolvedAs == "":
		// 明示的空文字も null 扱い (frontend が `null` の代わりに `""` を送る
		// 互換ケース、cf. PR #1102 color clear 経路と同じ workaround)。
		fields["resolvedAs"] = nil
	default:
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "resolvedAs must be 'accept', 'reject', or null.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if err := h.abuseRepo.UpdateFields(req.ReportID, fields); err != nil {
		return c.JSON(http.StatusNotFound, apierr.ErrorWithKind("NO_SUCH_ABUSE_REPORT", "No such abuse report.", "ac3794dd-2ce4-d878-e546-73c60c06b398", apierr.KindServer))
	}
	// upstream AbuseReportService.resolve は notifySystemWebhook('abuseReportResolved')
	// を呼ぶ。best-effort: DB 更新は済んでいるので発火失敗は握り潰す (#1723)。
	h.notifyAbuseReportResolved(c.Request().Context(), req.ReportID)
	return c.NoContent(http.StatusNoContent)
}

// notifyAbuseReportResolved fires the abuseReportResolved system webhook for a
// resolved report. 本家 AbuseReportNotificationService.notifySystemWebhook 相当:
// inactive な notification recipient (method=webhook) が指す systemWebhookId を
// excludes に渡し、残りの active system webhook へ packed report を配送する。
// dispatcher / abuseRepo 未配線時は no-op (#1723)。
func (h *Handler) notifyAbuseReportResolved(ctx context.Context, reportID string) {
	if h.systemWebhookDispatcher == nil || h.abuseRepo == nil {
		return
	}
	report, err := h.abuseRepo.FindByID(reportID)
	if err != nil {
		slog.WarnContext(ctx, "abuseReportResolved: load report failed", "reportId", reportID, "err", err)
		return
	}
	profByID := h.abuseUserProfiles([]*model.AbuseUserReport{report})
	body := h.packAbuseReport(ctx, report, profByID)
	h.systemWebhookDispatcher.DispatchSystemExcluding(
		corewebhook.SystemEventAbuseReportResolved, body, h.inactiveAbuseWebhookIDs())
}

// inactiveAbuseWebhookIDs returns the systemWebhookId values of inactive
// abuse-report-notification recipients (method=webhook). 本家 notifySystemWebhook
// の withoutWebhookIds 相当 (= 無効化された recipient 宛は配送しない)。recipientRepo
// 未配線 / 取得失敗時は nil (= excludes 無し)。
func (h *Handler) inactiveAbuseWebhookIDs() []string {
	if h.recipientRepo == nil {
		return nil
	}
	recipients, err := h.recipientRepo.List()
	if err != nil {
		return nil
	}
	var excludes []string
	for _, r := range recipients {
		if r.Method == "webhook" && !r.IsActive && r.SystemWebhookID != nil {
			excludes = append(excludes, *r.SystemWebhookID)
		}
	}
	return excludes
}

// ShowModerationLogs handles POST /api/admin/show-moderation-logs.
//
// model.ModerationLog は createdAt 列を持たず aidx ID 先頭 8 文字に
// timestamp を埋め込んでいる。frontend (modlog.ModLog.vue) は
// `<MkTime :time="log.createdAt"/>` を直接読むため、handler 側で aidx
// から派生した createdAt 文字列を response に注入する。invite/users 系
// と同じく "2006-01-02T15:04:05.000Z" 形式 (Misskey の標準)。
func (h *Handler) ShowModerationLogs(c echo.Context) error {
	var req struct {
		Limit     *int   `json:"limit"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
		Type      string `json:"type"`
		UserID    string `json:"userId"`
		Search    string `json:"search"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// sinceDate/untilDate を aidx prefix に正規化し、type/userId/search で絞る
	// (upstream show-moderation-logs.ts, #1539)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	logs, err := h.modLogService.List(model.ModerationLogFilter{
		Limit:   limit,
		SinceID: sinceID,
		UntilID: untilID,
		Type:    req.Type,
		UserID:  req.UserID,
		Search:  req.Search,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// upstream は user を UserDetailedNotMe (optional:false) で pack する。生 model.User
	// を埋めると usernameLower/inbox 等の内部列が漏れ avatarUrl 等 packed field も欠ける。
	// actor (moderator) の profile は重複が多いので distinct user で 1 度だけ fetch して
	// pack する (#1539)。
	profileByUser := map[string]*model.UserProfile{}
	out := make([]map[string]any, 0, len(logs))
	for _, l := range logs {
		m := map[string]any{
			"id":     l.ID,
			"userId": l.UserID,
			"type":   l.Type,
			"info":   l.Info,
		}
		if s, err := aidxCreatedAtString(h.idGen, l.ID); err == nil {
			m["createdAt"] = s
		} else if !errors.Is(err, ErrIDGenMissing) {
			// idGen は wired されているのに parse 失敗した場合のみログに残す。
			// 非 aidx 形式の legacy ID 等で createdAt が出せない時に frontend
			// 側で「Invalid Date」が出る原因を後追いできるようにする。
			slog.DebugContext(c.Request().Context(), "modlog: createdAt derive failed",
				"logId", l.ID, "err", err)
		}
		if l.User != nil {
			prof, ok := profileByUser[l.UserID]
			if !ok {
				if h.userRepo != nil {
					prof, _ = h.userRepo.FindProfileByUserID(l.UserID)
				}
				profileByUser[l.UserID] = prof
			}
			m["user"] = h.packModeratorVisibleUser(l.User, prof)
		}
		out = append(out, m)
	}
	return c.JSON(http.StatusOK, out)
}

// Signup condition constraints (#2565).
//
// 承認制は「登録を開放したうえで申請と承認を挟む」もの。招待制と重ねると
// **登録手段がゼロになる** — `approvalOpen` は `disableRegistration` が有効なら 503 を
// 返し、`/api/signup` は承認制が有効なら招待コードを見る前に 403 を返す。二重の
// ゲートにはならず、招待コードを持っていても入れない。
//
// メール必須との組み合わせは #2565 当時「承認フローが signupService を直接呼んで
// メール確認の経路を通らない」ことが理由だったが、**#2571 でその前提は無くなった**
// (下記のとおり排他は撤去済み)。
//
// どちらも管理画面で普通に作れてしまい、作った側は矛盾に気づけない。
// メール必須との排他は撤去した (#2571)。承認済みからの登録も
// emailRequiredForSignup が有効なら確認メールの経路を通るようになり、設定と実態が
// 食い違わなくなったため。**クレームコードは常に必須のまま**で、本人性の担保は
// コードが持つ。メールは独立した任意設定。

// signupGateBoolFields are the registration gates that must arrive as booleans.
var signupGateBoolFields = []string{"approvalRequiredForSignup", "disableRegistration"}

// normalizeSignupGateBools drops JSON null and rejects other non-bool values
// for the registration gates.
//
// **型が違うと「列は変わるのに正規化は走らない」状態が作れる (#2803)。** GORM は
// map の値をそのまま driver へ渡すので、`"false"` のような文字列でも PostgreSQL の
// boolean 入力構文に当たれば列は更新される。一方 normalizeSignupConditions は bool
// しか見ないので、値を string にして送るクライアント (Echo の Bind は
// form-urlencoded を string にする) は**承認制を外しつつ登録を全開のまま残せる**。
// 逆向きも同じで、`disableRegistration` を string で送ると #2565 が防ぐはずの
// 「承認制 + 招待制」が作れる。upstream は ajv の `type: 'boolean'` で string を
// 400 にするので、弾くほうが互換でもある。
//
// **null だけは弾かずに落とす。** upstream の paramDef は `nullable: true` で、
// 実装も `typeof ps.disableRegistration === 'boolean'` でしか読まない (= null は
// 無指定と同じ)。misskey-js の生成型も `boolean | null` なので、型どおりに送る
// クライアントを 400 にすると互換が壊れる。落とさないと NOT NULL 制約違反で
// 500 になる (この分岐を入れる前の mk-go の挙動)。
//
// **対象はこの 2 つに絞る** — 他の bool 列は型を間違えても正規化の判断を
// すり抜けさせる働きが無く、update-meta の全 bool 列を一括で弾くと既存クライアント
// への影響範囲が読めない。
func normalizeSignupGateBools(fields map[string]any) error {
	for _, key := range signupGateBoolFields {
		v, ok := fields[key]
		if !ok {
			continue
		}
		if v == nil {
			delete(fields, key)
			continue
		}
		if _, isBool := v.(bool); !isBool {
			return fmt.Errorf("%s must be a boolean", key)
		}
	}
	return nil
}

// normalizeSignupConditions keeps registration in step with the approval gate.
//
// **拒否ではなく同じ更新で開けるのが要点。** 「先に開放してから承認制を入れる」
// 手順を強制すると、開放してから承認制が入るまでの間に素通しで登録される窓が
// できる。承認制それ自体が `/api/signup` を 403 で塞ぐので、この開放は安全性
// ではなく表示上の整合のため (訪問者に「招待制」と出さない)。
//
// **承認制を外す更新では逆に閉じる (#2803)。** 開放は「承認制がゲートとして立って
// いる」ことが前提の整合なので、ゲートが消える更新でそれを維持する理由が無い。
// 維持すると、招待制 → 承認制 ON → 承認制 OFF の 3 操作でゲートが 1 つも無い全開
// 状態が残り、しかも `enableRegistration` の ON にある確認ダイアログを通らないので
// 無警告で起きる。**倒す先は閉じる側にする** — 元が開放だったサーバーが承認制を
// やめると招待制になるが、それは次に管理画面を開けばトグルに出るので気づける。
// 逆 (全開のまま残る) は開いていることが正常に見えるので気づけない。
//
// 承認制の状態が変わらない更新では触らない。**無条件に開け閉めすると、管理者が
// 登録を開いた / 閉じた操作を黙って巻き戻すことになる。**
//
// **meta が引けない (current == nil) ときは閉じる補正も走らない。** 呼び出し側が
// Fetch の error を握り潰して素通しする判断 (#2565) の帰結で、遷移かどうかを
// 判定できないため。ここで「承認制 OFF と言っている以上は閉じる」と決め打つと、
// 承認制が元から無効なサーバーへの冗長な更新が登録を閉じてしまう。
//
// meta が引けない状態が続いていれば列は書かれない。後段の maybeAutoGenerateVAPID が
// 自分の Fetch の error をそのまま返し、UpdateMeta は Update を呼ばずに 500 で終わる
// (TestUpdateMeta_MetaUnavailable が固定している)。**ただし Fetch は 2 回別々に
// 呼ばれる**ので、1 回目だけが一過性に失敗すると 2 回目は通り、補正の走らない更新が
// 書かれる。VAPID 側を meta 欠損に寛容にすると、この窓が窓でなくなる。
//
// 明示指定の扱いは向きで非対称。**閉じる側は明示を尊重し、開ける側は上書きする。**
// 開ける側の上書きは #2565 の整合の強制そのもの (重ねると登録手段がゼロになる)
// で、今回はそこには触らない。閉じる側は逆に、既定を
// 補うだけで運用者の意思を上書きする理由が無い (両方送るクライアントを壊さない)。
//
// 送っていない列を書き換える形なので「検証のみにして両方を送らせる」案もあるが、
// 残す (#2571 で判断)。**結合はサーバー側の都合** — 承認制それ自体はゲートとして
// 完結していて、開放は訪問者への表示を実態に合わせるためだけにある。それを知らない
// クライアントが「承認制を入れる」だけの自然な 1 リクエストで 400 を食うのは筋が
// 悪い。モデレーション画面は既に両方を送るので、ここは二重の保険として効く。
func normalizeSignupConditions(fields map[string]any, current *model.Meta) {
	next, ok := fields["approvalRequiredForSignup"].(bool)
	if !ok {
		return
	}
	was := current != nil && current.ApprovalRequiredForSignup
	if next == was {
		return // 今回の更新で切り替えたわけではない。
	}
	if next {
		if disabled := metaBoolAfterUpdate(fields, "disableRegistration", current != nil && current.DisableRegistration); disabled {
			fields["disableRegistration"] = false
		}
		return
	}
	if metaBoolExplicit(fields, "disableRegistration") {
		return // 明示指定がある。既定を補う必要は無い。
	}
	fields["disableRegistration"] = true
}

// validateSignupApplicationForm checks an incoming application form definition.
//
// 送られてこなければ何もしない (部分更新)。**保存してから壊れているのに気づく**
// 形にしないよう、ここで弾く。
func validateSignupApplicationForm(fields map[string]any) error {
	v, ok := fields["signupApplicationForm"]
	if !ok {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%w: signupApplicationForm is not encodable", signupapplication.ErrInvalidForm)
	}
	var parsed []signupapplication.FormField
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("%w: signupApplicationForm must be an array of fields", signupapplication.ErrInvalidForm)
	}
	if err := signupapplication.ValidateForm(parsed); err != nil {
		return err
	}
	// 検証済みの形に正規化して渡す。**生の any をそのまま入れると、未知のキーが
	// jsonb に残って後で読み手を混乱させる。**
	//
	// nil を空配列に寄せるのは、`{"signupApplicationForm": null}` を素直に
	// marshal すると jsonb の `null` が入るため。読み手は全て耐えるが、他の
	// jsonb 列は coerceMetaJSONBFields が `[]` に正規化しており、ここだけ
	// datatypes.JSON なのでその分岐に入らない。列ごとに中身が違う状態にしない。
	if parsed == nil {
		parsed = []signupapplication.FormField{}
	}
	normalized, err := json.Marshal(parsed)
	if err != nil {
		return fmt.Errorf("%w: signupApplicationForm is not encodable", signupapplication.ErrInvalidForm)
	}
	fields["signupApplicationForm"] = datatypes.JSON(normalized)
	return nil
}

// HasUserTokenInvalidator reports whether the auth cache invalidator is wired.
//
// 未配線だと admin/suspend-user や admin/accounts/delete の後も、対象の
// session が auth cache の TTL のあいだ生き残る。router 側のコメントも
// これを security regression と呼んでいる (#2682)。
func (h *Handler) HasUserTokenInvalidator() bool { return h.userTokenInvalidator != nil }
