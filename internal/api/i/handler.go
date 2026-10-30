package i

import (
	"context"
	"net/http"
	"strings"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/notehide"
	"github.com/elythia-network/elythia/internal/bgwork"
	corelocalfederation "github.com/elythia-network/elythia/internal/core/localfederation"
	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/core/passwordguard"
	"github.com/elythia-network/elythia/internal/core/twofactor"
	"github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/misc/password"
	miscsmtp "github.com/elythia-network/elythia/internal/misc/smtp"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// RoleProvider abstracts role queries for /api/i responses.
// 循環参照を避けるため interface で受け取る。
type RoleProvider interface {
	IsAdministrator(userID string) bool
	IsModerator(userID string) bool
	IsSilenced(userID string) bool
	GetUserRoles(userID string) ([]*model.Role, error)
	GetUserPolicies(userID string) map[string]any
	// HasRolePolicy gates per-policy field updates (canUpdateBioMedia 等)
	// 単発 lookup なので caller は GetUserPolicies を介さずに済む (#1024)。
	HasRolePolicy(userID, policyKey string) bool
}

// checkedRoleProvider is used by authorization decisions that must distinguish
// a resolved deny from failures in the native inputs or providers that declare
// the decision's policy keys.
type checkedRoleProvider interface {
	GetUserPoliciesCheckedForKeys(userID string, keys ...string) (map[string]any, error)
}

// EmailSender sends an email message (subject + text + optional HTML).
// SMTP 設定は実装側が Meta から読み取る。テストではスタブを注入する。
// HTML 同送が必要なら Message.HTML を設定する (#600 item 4)。
type EmailSender func(to string, msg miscsmtp.Message)

// AccountMover performs the i/move workflow (AP delivery + user row updates).
// 具体実装は core/move.Service。循環依存を避けるため handler 側には narrow
// interface として置く。
type AccountMover interface {
	Move(src *model.User, dstURI string) error
}

// MoveInValidator reports whether a user is the destination of a recently
// confirmed account move, used to relax the bulk-import size limit (#2415)。
// 実体は core/move.Service。
type MoveInValidator interface {
	HasRecentConfirmedMoveIn(dst *model.User) bool
}

// Handler handles account-related API endpoints.
type Handler struct {
	// background は応答の後の処理 (rel=me の検証とメール) を起動する箱 (#3661)。世代の停止で
	// 待つ。nil なら素の goroutine で起動する (テスト)。
	background       *bgwork.Group
	userService      *user.Service
	idGen            id.Generator
	roleProvider     RoleProvider
	registryRepo     repository.RegistryRepository
	favoriteRepo     repository.NoteFavoriteRepository
	transferEnqueuer TransferEnqueuer
	// #2230: i/delete-account の cascade 削除と連合 Delete 配信 (admin/delete-account と同経路)。
	deleteAccountEnqueuer   DeleteAccountEnqueuer
	accountDeletionFed      AccountDeletionFederationHook
	webauthnSvc             *twofactor.WebAuthnService
	securityKeyRepo         repository.UserSecurityKeyRepository
	metaRepo                repository.MetaRepository
	emailSender             EmailSender
	serverURL               string
	signinRepo              repository.SigninRepository
	accessTokenRepo         repository.AccessTokenRepository
	galleryRepo             GalleryRepository
	pageLikeRepo            repository.PageLikeRepository
	mover                   AccountMover
	localFederationObserver activitypub.LocalFederationObserver
	localFederationService  *corelocalfederation.Service
	notificationSvc         UnreadNotificationSource
	moveInValidator         MoveInValidator
	achievementNotifier     AchievementNotifier
	followRequestRepo       repository.FollowRequestRepository
	// followRequestAccepter は鍵を外したときに溜まったリクエストを承認する (#3466)。
	followRequestAccepter FollowRequestBulkAccepter
	announcementRepo      AnnouncementUnreadSource
	chatRepo              ChatUnreadSource
	antennaUnreadRepo     AntennaUnreadSource
	channelUnreadRepo     ChannelUnreadSource
	unreadFlags           repository.MeUnreadFlagsReader
	piningRepo            repository.UserNotePiningRepository
	noteRepo              repository.NoteRepository
	pageRepo              repository.PageRepository
	instanceRepo          repository.InstanceRepository
	emojiRepo             repository.EmojiRepository
	bufReader             entity.BufferedReactionsReader
	avatarDecorationRepo  repository.AvatarDecorationRepository
	mainStreamPublisher   MainStreamPublisher
	fieldRes              *entity.NoteFieldResolver
	// emailValidationClient は verifymail / truemail SaaS への outbound に
	// 使う SSRF-safe HTTP client (#638)。nil ならデフォルトクライアント。
	emailValidationClient *http.Client
	// verifyLinkClient は i/update の rel=me link 検証で profile field の URL を
	// fetch するための SSRF-safe HTTP client (#1786)。nil なら検証を skip。
	verifyLinkClient *http.Client
	// hardMutePublisher は i/update で hardMutedWords を変更したときに
	// streaming connection に reload signal を流す (#791)。未配線時は
	// publish skip = 旧挙動 (= reconnect で反映)。
	hardMutePublisher HardMutePublisher
	// relationReload は mutedInstances 変更を streaming connection へ通知する
	// (#2400)。未配線なら通知しない。
	relationReload RelationReloadPublisher
	// pinDeliveryHook は note pin/unpin 時に Add/Remove(featured) を followers へ
	// 配信する (#2024)。未配線なら連合配信 skip。
	pinDeliveryHook PinDeliveryHook
	// profileUpdateHook は profile 編集時に Update(Person) を followers へ
	// 配信する (#1560)。nil なら配信しない。
	profileUpdateHook ProfileUpdateHook
	// authInvalidator は i/regenerate-token で旧 token を auth middleware
	// の cache から即時削除するために使う (#884)。未配線時は cache TTL
	// (30 秒) 待ちで stale 旧 token が auth 通過する security regression が
	// 残るので production では必ず wire する。
	authInvalidator TokenInvalidator
	// streamRevoker は資格情報を失効させたときに、その資格情報で張られた
	// /streaming の接続を閉じる。tokenCache を落としても WebSocket は接続時に
	// 1 度しか認証しないので、失効後も通知・DM を受け取り続ける。
	streamRevoker StreamRevoker
	// totpReplayGuard は i/2fa/done / 2fa-gated 操作で同一 TOTP コードの
	// 二度使いを refuse する (RFC 6238 §5.2 / mk-go 独自 hardening、upstream
	// Misskey TS は持たない)。nil なら無保護 (= unit test / dev fallback)。
	// production では必ず wire する。
	totpReplayGuard twofactor.ReplayGuard
	// twoFAPending は i/2fa/register が発行した仮の秘密の期限と、i/2fa/done の
	// 失敗回数を数える。nil なら数えない (router は必ず配線する)。
	twoFAPending twofactor.PendingGuard
	// userRepo は movedTo / alsoKnownAs の URI→ローカルID 解決 (#1255) に使う。
	// ローカル actor URI は FindByID、それ以外は FindByURI で引き、remote fetch はしない。
	userRepo repository.UserRepository
	// driveFileRepo は i/import-* の fileId 所有権検証 (#1555) に使う。
	// upstream Misskey の各 import endpoint は driveFilesRepository.findOneBy
	// ({id, userId: me.id}) で「リクエスト元 user 所有の file」のみ許可し、
	// 他人 / 不在 / system file (userId NULL) は NO_SUCH_FILE で弾く。未配線時は
	// fail-closed (= 常に NO_SUCH_FILE) にして cross-user file read を防ぐ。
	driveFileRepo repository.DriveFileRepository
	// importDriveReader は i/import-antennas で file content を同期 download して
	// antenna 件数を数え TOO_MANY_ANTENNAS を enforce するために使う (#1667)。
	// 未配線時は limit check を skip して worker に委ねる (degrade)。
	importDriveReader ImportFileReader
	// antennaCounter は i/import-antennas の TOO_MANY_ANTENNAS 判定で現 antenna
	// 件数を数えるのに使う (#1667)。未配線時は limit check を skip する。
	antennaCounter AntennaCounter
	// passwordGuard は現在のパスワードの照合失敗をアカウント単位で数える。
	// nil なら数えない (router は必ず配線する。TestPasswordFailureGuardIsWired)。
	passwordGuard passwordguard.Guard
	// passwordHasher は i/change-passwordで新しいパスワードをハッシュする(#3780)。
	// 零値は既定のcost。
	passwordHasher password.Hasher
	// testModeは2FAのTOTPの検証を素通しにする(本家UserAuthService.validateOtpの
	// `NODE_ENV === 'test'`相当、#3780)。twofactor.ValidateWithReplayへ渡す。
	testMode bool
	// noteHider は閲覧者が見てはいけない投稿の中身を隠す (#3786)。無いと匿名の
	// 閲覧者への ugcVisibilityForVisitor が効かないので、構築子の必須の引数で受ける
	// (#3827)。
	noteHider *notehide.Hider
	// entityPacker は利用者や投稿を包むときの依存 (role 由来の値、メディアのプロキシ等) を
	// 持つ (#3790、#3792)。組み立てで渡す。
	entityPacker *entity.Packer
}

func (h *Handler) SetLocalFederationObserver(observer activitypub.LocalFederationObserver) {
	h.localFederationObserver = observer
}

func (h *Handler) SetLocalFederationService(service *corelocalfederation.Service) {
	h.localFederationService = service
}

// TokenInvalidator は i/regenerate-token / i/change-password 等の sensitive
// 操作後に auth cache の旧 token entry を即時削除するための interface
// (#884)。実装は internal/server/middleware/auth.go の AuthMiddleware が
// 担当する。circular import を避けるため interface 経由で受け取る。
//
// InvalidateTokensForUser は user 自身が複数 device から log-in している
// ケースで「自分の全 session を即時失効したい」操作 (i/delete-account 等)
// で使う (#965)。token 単独削除では他端末の cache が最大 30s stale 続け、
// 削除済 user 名義で操作可能な攻撃面が残るため (PR #966 で admin 経路
// から要求された same-shape gap)。
//
// InvalidateUserToken は token 1 つを捨てる。他のプロセスへは利用者 ID で伝わる
// (token は pub/sub に流さない、#3664)。
type TokenInvalidator interface {
	InvalidateUserToken(userID, token string)
	InvalidateTokensForUser(userID string)
}

// HardMutePublisher publishes a wordmute reload event for the given user.
// Implementation lives in internal/server/router_adapters.go (Redis pubsub
// adapter), the handler only depends on this minimal interface.
type HardMutePublisher interface {
	PublishHardMuteReload(userID string)
}

// RelationReloadPublisher publishes a relation reload event so streaming
// connections rebuild their mute/block snapshot (#2400). mutedInstances は
// MuteBlockSnapshot の MutedInstances に載るため、変更時に通知が要る。
type RelationReloadPublisher interface {
	PublishMuteBlockReload(userID string)
}

// ProfileUpdateHook delivers Update(Person) to remote followers on local
// profile edit (#1560)。実装は core/federation。
type ProfileUpdateHook interface {
	OnLocalProfileUpdated(userID string)
}

// PinDeliveryHook delivers Add/Remove(featured collection) to remote followers
// when a local user pins/unpins a note (#2024)。実装は core/federation。
type PinDeliveryHook interface {
	OnLocalPinChanged(userID, noteID string, isAddition bool)
}

// SetPinDeliveryHook wires the AP Add/Remove featured-collection delivery hook (#2024)。
func (h *Handler) SetPinDeliveryHook(hook PinDeliveryHook) {
	h.pinDeliveryHook = hook
}

// SetHardMutePublisher wires a publisher that emits wordmute reload events
// when i/update changes hardMutedWords (#791). Optional; nil disables the
// realtime reload path (the streaming connection still picks up the new
// rules at the next reconnect).
// SetRelationReloadPublisher wires the relation reload publisher (#2400)。
func (h *Handler) SetRelationReloadPublisher(p RelationReloadPublisher) {
	h.relationReload = p
}

func (h *Handler) SetHardMutePublisher(p HardMutePublisher) {
	h.hardMutePublisher = p
}

// SetProfileUpdateHook wires the AP Update(Person) delivery hook (#1560)。
func (h *Handler) SetProfileUpdateHook(hook ProfileUpdateHook) {
	h.profileUpdateHook = hook
}

// SetEmailValidationClient wires the outbound HTTP client used by
// verifymail / truemail siteverify calls (#638). production では SSRF-safe
// + forward proxy 経由の client を渡すこと。
func (h *Handler) SetEmailValidationClient(c *http.Client) {
	h.emailValidationClient = c
}

// SetVerifyLinkClient wires the SSRF-safe outbound HTTP client used by i/update
// to verify rel=me profile-field backlinks (#1786). nil disables verification.
func (h *Handler) SetVerifyLinkClient(c *http.Client) {
	h.verifyLinkClient = c
}

// SetNoteFieldResolver wires the shared resolver that fills Files /
// MyReaction / Channel on packed pinned notes (#426)。
func (h *Handler) SetNoteFieldResolver(r *entity.NoteFieldResolver) {
	h.fieldRes = r
}

// SetUserRepo wires a UserRepository used to resolve movedTo / alsoKnownAs
// actor URIs to local user IDs (#1255). Unwired (test) leaves those null.
func (h *Handler) SetUserRepo(r repository.UserRepository) {
	h.userRepo = r
}

// resolveUserIDByURI resolves an ActivityPub actor URI to a local user ID via
// a local DB lookup only (no remote fetch). Canonical local actor URIs are
// looked up by ID because local users have a NULL URI column; other URIs use
// FindByURI. Returns ("", false) when unwired or the URI is not known locally.
func (h *Handler) resolveUserIDByURI(uri string) (string, bool) {
	if h.userRepo == nil {
		return "", false
	}
	if id := activitypub.NewURLBuilder(h.serverURL).LocalUserIDFromURI(uri); id != "" {
		u, err := h.userRepo.FindByID(id)
		if err != nil || u == nil {
			return "", false
		}
		return u.ID, true
	}
	u, err := h.userRepo.FindByURI(uri)
	if err != nil || u == nil {
		return "", false
	}
	return u.ID, true
}

// resolveAlsoKnownAs converts the i/update alsoKnownAs lines (each an acct
// `@user@host` / `@user` or a canonical AP URI) into a deduplicated list of
// canonical actor URIs, mirroring upstream Misskey TS i/update.ts:445-475.
//
// 各 line につき:
//   - 空文字なら NO_SUCH_USER (upstream の `if (!line) throw noSuchUser`)
//   - acct/URI をローカル DB から解決 (i/move 同様 remote fetch はしない。
//     frontend は移行先 actor 解決後の acct/URI を渡す運用)
//   - 解決した user が自分自身なら FORBIDDEN_TO_SET_YOURSELF
//   - 解決した user の canonical URI が空なら URI_NULL
//
// 戻り値の URI は入力順を保ちつつ重複を除く (upstream の Set 相当)。空 line を
// 1 つも含まない要求が空配列ならそのまま空 slice を返す (= core 側で NULL
// クリア)。
func (h *Handler) resolveAlsoKnownAs(meID string, lines []string) ([]string, *avatarDecorationAPIError) {
	noSuchUser := &avatarDecorationAPIError{
		status: http.StatusBadRequest,
		body:   apierr.Error("NO_SUCH_USER", "No such user.", "fcd2eef9-a9b2-4c4f-8624-038099e90aa5"),
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			return nil, noSuchUser
		}
		u := h.lookupAlsoKnownAsTarget(line)
		if u == nil {
			return nil, noSuchUser
		}
		if u.IsLocal() && h.localFederationObserver != nil && !h.localFederationObserver.IsExternallyAvailable(u) {
			return nil, noSuchUser
		}
		if u.ID == meID {
			return nil, &avatarDecorationAPIError{
				status: http.StatusBadRequest,
				body:   apierr.Error("FORBIDDEN_TO_SET_YOURSELF", "You can't set yourself as your own alias.", "25c90186-4ab0-49c8-9bba-a1fa6c202ba4"),
			}
		}
		uri := h.canonicalUserURI(u)
		if uri == "" {
			return nil, &avatarDecorationAPIError{
				status: http.StatusBadRequest,
				body:   apierr.Error("URI_NULL", "User ActivityPup URI is null.", "bf326f31-d430-4f97-9933-5d61e4d48a23"),
			}
		}
		if _, dup := seen[uri]; dup {
			continue
		}
		seen[uri] = struct{}{}
		out = append(out, uri)
	}
	return out, nil
}

// lookupAlsoKnownAsTarget resolves a single alsoKnownAs line to a local user
// row. http(s) URIs go through FindByURI; acct forms (`@user@host` / `@user`)
// go through FindByUsernameLower. Returns nil on any miss or when userRepo is
// unwired (fail-closed = NO_SUCH_USER at the caller).
func (h *Handler) lookupAlsoKnownAsTarget(line string) *model.User {
	if h.userRepo == nil {
		return nil
	}
	if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
		u, err := h.userRepo.FindByURI(line)
		if err != nil || u == nil {
			return nil
		}
		return u
	}
	username, host := splitAcct(line)
	if username == "" {
		return nil
	}
	u, err := h.userRepo.FindByUsernameLower(username, host)
	if err != nil || u == nil {
		return nil
	}
	return u
}

// canonicalUserURI returns the actor URI used as the alsoKnownAs alias for u:
// local users get the server-rooted /users/<id> form, remote users surface
// their stored uri. Returns "" when a remote user has no uri (= URI_NULL).
func (h *Handler) canonicalUserURI(u *model.User) string {
	if u.IsLocal() {
		if h.serverURL == "" {
			return ""
		}
		return strings.TrimRight(h.serverURL, "/") + "/users/" + u.ID
	}
	if u.URI != nil {
		return *u.URI
	}
	return ""
}

// splitAcct parses an acct form (`@user@host` / `user@host` / `@user`) into a
// lowercased username and optional host pointer (nil for local). Empty / `@`
// only input yields an empty username so the caller can reject it.
func splitAcct(acct string) (string, *string) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(acct), "@")
	if trimmed == "" {
		return "", nil
	}
	username := trimmed
	var host string
	if at := strings.IndexByte(trimmed, '@'); at >= 0 {
		username = trimmed[:at]
		host = strings.ToLower(strings.TrimSpace(trimmed[at+1:]))
	}
	username = strings.ToLower(username)
	if host == "" {
		return username, nil
	}
	return username, &host
}

// MainStreamPublisher emits events to a single user's `main` WebSocket
// channel. Used here to publish `myTokenRegenerated` so other sessions of
// the same user learn that their API token was invalidated.
// 循環依存を避けるためinterfaceで受け取る(実装はinternal/stream)。
type MainStreamPublisher interface {
	PublishMainEvent(userID, eventType string, body any)
}

// StreamRevoker closes live /streaming connections whose credential was just
// invalidated. Implemented by stream.StreamRevokePublisher (pubsub fan-out so
// connections held by other processes close too).
type StreamRevoker interface {
	RevokeUserStreams(userID string)
	RevokeNativeTokenStreams(userID, token string)
	RevokeAccessTokenStreams(userID, tokenID string)
}

// invalidateUserTokenCache removes every cached entry that resolves to the
// given userID, across all of the user's tokens (native login + access
// tokens / multi-device sessions). Self-mutation that affects the entire
// session set (e.g. i/delete-account 論理削除) calls this so other devices
// of the same user are also locked out within the cache TTL window
// (#967, sibling of admin-side #966 user-token invalidate).
//
// i/update と i/move もこれを使う (#3664。本家の localUserUpdated と同じく
// 利用者単位で、他のプロセスへも伝わる)。**DB への書き込みを commit した後に
// 呼ぶこと** (AuthMiddleware.InvalidateTokensForUser の前提)。
//
// noop when invalidator is not wired or userID is empty.
func (h *Handler) invalidateUserTokenCache(userID string) {
	if h.authInvalidator == nil || userID == "" {
		return
	}
	h.authInvalidator.InvalidateTokensForUser(userID)
}

// SetMainStreamPublisher attaches a publisher used to emit events on
// /api/i/* endpoints (currently `myTokenRegenerated`). Optional — nil
// disables emit.
func (h *Handler) SetMainStreamPublisher(p MainStreamPublisher) {
	h.mainStreamPublisher = p
}

// SetTestMode enables the upstream-compatible TOTP bypass of test runs (see
// twofactor.ValidateWithReplay). Production leaves it false.
func (h *Handler) SetTestMode(v bool) { h.testMode = v }

// SetPasswordHasher sets the Hasher that i/change-password hashes the new
// password with. Unset, the default cost is used.
func (h *Handler) SetPasswordHasher(hasher password.Hasher) { h.passwordHasher = hasher }

// SetInstanceRepo attaches an InstanceRepository so favorites/notifications
// note embeds populate UserLite.Instance for remote users (#277).
func (h *Handler) SetInstanceRepo(r repository.InstanceRepository) {
	h.instanceRepo = r
}

func (h *Handler) instanceLookup() entity.InstanceLookup {
	if h.instanceRepo == nil {
		return nil
	}
	return h.instanceRepo
}

// SetEmojiRepo attaches an EmojiRepository so custom emoji shortcodes in
// note text and user displayNames get resolved to URLs.
func (h *Handler) SetEmojiRepo(r repository.EmojiRepository) {
	h.emojiRepo = r
}

// SetReactionReader wires a BufferedReactionsReader so PackNote / PackNotes
// can merge in-flight buffered reaction deltas (#647)。
func (h *Handler) SetReactionReader(r entity.BufferedReactionsReader) {
	h.bufReader = r
}

func (h *Handler) reactionReader() entity.BufferedReactionsReader {
	return h.bufReader
}

func (h *Handler) emojiLookup() entity.EmojiLookup {
	if h.emojiRepo == nil {
		return nil
	}
	return h.emojiRepo
}

// UnreadNotificationSource is the subset of notification.Service used by /api/i
// to compute hasUnreadNotification / unreadNotificationsCount /
// hasUnreadMentions / hasUnreadSpecifiedNotes. Keeping this as a local
// interface avoids pulling core/notification into the handler test harness
// and lets the tests inject a stub.
//
// 3 フラグを 1 度の Redis scan で集約する UnreadSummary を採用済み (#321)。
type UnreadNotificationSource interface {
	UnreadSummary(ctx context.Context, userID string, mentionTypes []notification.Type) (notification.UnreadSummary, error)
}

// UnreadStreamSource is implemented by notification.Service. It lets /api/i
// read hasUnreadSpecifiedNotes from note_unread together with the other
// unread flags in one statement instead of inside UnreadSummary (#3608).
type UnreadStreamSource interface {
	// SpecifiedNoteFromDB reports whether UnreadSummary reads note_unread.
	SpecifiedNoteFromDB() bool
	// UnreadStreamSummary is UnreadSummary without the note_unread read.
	UnreadStreamSummary(ctx context.Context, userID string, mentionTypes []notification.Type) (notification.UnreadSummary, error)
}

// 名前を変えると型アサーションが黙って外れて 5 本に戻るので、コンパイルで縛る。
var _ UnreadStreamSource = (*notification.Service)(nil)

// AchievementNotifier creates the achievementEarned notification when a user
// unlocks a new achievement (claim-achievement)。未配線なら通知は作らず実績の
// 記録だけ行う (degrade)。実装は *notification.Service。
type AchievementNotifier interface {
	Create(ctx context.Context, in notification.CreateInput) (*notification.Notification, error)
}

// AntennaUnreadSource reports whether a user has any unread antenna notes.
// Satisfied by repository.AntennaNoteUnreadRepository.
type AntennaUnreadSource interface {
	HasAnyByUser(userID string) (bool, error)
}

// ChannelUnreadSource reports whether a user has any unread channel notes.
// Satisfied by repository.ChannelNoteUnreadRepository.
type ChannelUnreadSource interface {
	HasAnyByUser(userID string) (bool, error)
}

// AnnouncementUnreadSource lists active announcements the user has not read.
// Mirrors the subset of AnnouncementRepository needed here.
type AnnouncementUnreadSource interface {
	UnreadForUser(userID string) ([]*model.Announcement, error)
}

// ChatUnreadSource returns the number of unread chat messages for a user.
type ChatUnreadSource interface {
	CountUnread(userID string) (int64, error)
}

// GalleryRepository is the subset of repository.GalleryRepository used by
// the i/gallery/* endpoints. Kept as a handler-local alias so we can
// swap implementations in tests without pulling the full repository.
type GalleryRepository interface {
	ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.GalleryPost, error)
	ListLikesByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.GalleryLike, error)
	FindPostsByIDs(ids []string) ([]*model.GalleryPost, error)
	// ExistsLike は GalleryPost.isLiked 解決用 (#1555)。
	ExistsLike(userID, postID string) (bool, error)
}

// SetGalleryRepo wires the gallery repo for i/gallery/*.
func (h *Handler) SetGalleryRepo(r GalleryRepository) { h.galleryRepo = r }

// SetPageLikeRepo wires the page_like repo for i/page-likes.
func (h *Handler) SetPageLikeRepo(r repository.PageLikeRepository) {
	h.pageLikeRepo = r
}

// isSilenced returns whether the given user has any role whose merged
// policies deny canPublicNote. Wraps roleProvider so a nil provider
// (early boot / tests that skip role wiring) yields false.
func (h *Handler) isSilenced(userID string) bool {
	if h.roleProvider == nil {
		return false
	}
	return h.roleProvider.IsSilenced(userID)
}

// SetServerURL sets the canonical instance URL used for email verification
// links and local ActivityPub actor URI resolution.
func (h *Handler) SetServerURL(u string) {
	h.serverURL = u
}

// SetEmailSender attaches an EmailSender for update-email verification.
func (h *Handler) SetEmailSender(s EmailSender) {
	h.emailSender = s
}

// SetMetaRepo attaches a MetaRepository. When set, i/update enforces
// meta.prohibitedWordsForNameOfUser against the display name. Tests that
// don't need this validation can leave it unset.
func (h *Handler) SetMetaRepo(r repository.MetaRepository) {
	h.metaRepo = r
}

// SetWebAuthn attaches the WebAuthn service + security key repository.
// Both are required to enable WebAuthn endpoints; if either is nil the
// register/done/remove/update/passwordless handlers return a no-op 204 (so
// existing test fixtures that don't wire the dependency keep passing).
func (h *Handler) SetWebAuthn(svc *twofactor.WebAuthnService, repo repository.UserSecurityKeyRepository) {
	h.webauthnSvc = svc
	h.securityKeyRepo = repo
}

// SetSigninRepo attaches a SigninRepository for i/signin-history.
func (h *Handler) SetSigninRepo(r repository.SigninRepository) {
	h.signinRepo = r
}

// SetFavoriteRepo attaches a NoteFavoriteRepository for i/favorites.
func (h *Handler) SetFavoriteRepo(r repository.NoteFavoriteRepository) {
	h.favoriteRepo = r
}

// SetAccountMover attaches an AccountMover used by i/move. If unset, i/move
// returns 501 so the endpoint fails loudly instead of silently no-oping.
func (h *Handler) SetAccountMover(m AccountMover) {
	h.mover = m
}

// SetMoveInValidator wires the check used to relax the import size limit right
// after an account move. 未設定なら常に通常上限。
func (h *Handler) SetMoveInValidator(v MoveInValidator) {
	h.moveInValidator = v
}

// SetNotificationService wires the notification service used to compute
// hasUnreadNotification / unreadNotificationsCount / hasUnreadMentions on
// /api/i. When unset those fields fall back to default (false/0).
func (h *Handler) SetNotificationService(s UnreadNotificationSource) {
	h.notificationSvc = s
}

// SetAchievementNotifier wires the notification service used to emit the
// achievementEarned notification on claim-achievement.
func (h *Handler) SetAchievementNotifier(s AchievementNotifier) {
	h.achievementNotifier = s
}

// SetFollowRequestRepo wires the follow_request repository used to compute
// hasPendingReceivedFollowRequest on /api/i.
func (h *Handler) SetFollowRequestRepo(r repository.FollowRequestRepository) {
	h.followRequestRepo = r
}

// FollowRequestBulkAccepter accepts the pending follow requests toward a
// user. *following.Service satisfies it.
type FollowRequestBulkAccepter interface {
	AcceptAllRequests(followeeID string) error
}

// SetAnnouncementRepo wires the announcement repository used to compute
// hasUnreadAnnouncement / unreadAnnouncements on /api/i.
func (h *Handler) SetAnnouncementRepo(r AnnouncementUnreadSource) {
	h.announcementRepo = r
}

// SetChatRepo wires the chat repository used to compute hasUnreadChatMessages
// on /api/i.
func (h *Handler) SetChatRepo(r ChatUnreadSource) {
	h.chatRepo = r
}

// SetAntennaUnreadRepo wires the antenna_note_unread repository used to
// populate hasUnreadAntenna on /api/i. Optional — nil leaves the field
// false (current behaviour).
func (h *Handler) SetAntennaUnreadRepo(r AntennaUnreadSource) {
	h.antennaUnreadRepo = r
}

// SetChannelUnreadRepo wires the channel_note_unread repository used to
// populate hasUnreadChannel on /api/i. Optional — nil leaves the field
// false (current behaviour).
func (h *Handler) SetChannelUnreadRepo(r ChannelUnreadSource) {
	h.channelUnreadRepo = r
}

// SetMeUnreadFlagsReader wires the reader that fetches
// hasPendingReceivedFollowRequest / hasUnreadChatMessages / hasUnreadAntenna /
// hasUnreadChannel, and hasUnreadSpecifiedNotes when the notification service
// takes it from note_unread, in one statement (#3608). Each field is still
// written only when its source above is wired, so the response is the same
// with or without it; without it each field is one query.
//
// The values do not go through the repositories above: a repository wrapped
// with extra filtering would be bypassed. Keep them plain, or change
// repository.meUnreadFlagsSQL with them.
func (h *Handler) SetMeUnreadFlagsReader(r repository.MeUnreadFlagsReader) {
	h.unreadFlags = r
}

// SetPiningRepo wires the user_note_pining repository used to fill
// pinnedNoteIds / pinnedNotes on /api/i.
func (h *Handler) SetPiningRepo(r repository.UserNotePiningRepository) {
	h.piningRepo = r
}

// SetNoteRepo wires the note repository used to pack pinnedNotes entities.
func (h *Handler) SetNoteRepo(r repository.NoteRepository) {
	h.noteRepo = r
}

// SetPageRepo wires the page repository used to fill pinnedPage on /api/i.
func (h *Handler) SetPageRepo(r repository.PageRepository) {
	h.pageRepo = r
}

// SetAvatarDecorationRepo wires the avatar_decoration repository used to
// validate i/update avatarDecorations entries (existence + role gating).
// Optional — nil disables validation and lets i/update accept arbitrary
// decoration ids without checking the catalog.
func (h *Handler) SetAvatarDecorationRepo(r repository.AvatarDecorationRepository) {
	h.avatarDecorationRepo = r
}

// NewHandler creates a new account Handler.
//
// The remaining arguments are the dependencies whose absence degrades the
// handler silently:
//
//   - noteHider applies the per-viewer hideNote gate to the notes this
//     handler packs (#3786); entityPacker packs entities and rewrites media
//     URLs (#3790)
//   - roleProvider resolves roles and policies (alwaysMarkNsfw,
//     wordMuteLimit, canUpdateBioMedia, antennaLimit, avatarDecorationLimit)
//   - totpReplayGuard refuses a TOTP code that was already consumed within its
//     acceptance window (RFC 6238 §5.2) and backs the single-use reservation
//     of backup codes (#2852); twoFAPending bounds the lifetime and the
//     wrong-code budget of the temp secret issued by i/2fa/register
//   - authInvalidator drops tokens from the auth cache right away (#884);
//     accessTokenRepo backs i/revoke-token, i/apps and i/authorized-apps;
//     streamRevoker closes the /streaming connections of a revoked credential
//   - followRequestAccepter accepts the pending follow requests when the
//     account stops being locked (#3466)
//   - background is the work group the post-response work (rel=me
//     verification and mail) runs in, so stopping a generation waits for it
//     (#3661)
//
// 以前は setter で後から入れ、入れ忘れを起動時の検査 (criticalWiring) が見ていた。
// 渡し忘れをコンパイルで落とすため、構築子の必須の引数で受ける (#3827)。
func NewHandler(
	userService *user.Service,
	idGen id.Generator,
	noteHider *notehide.Hider,
	entityPacker *entity.Packer,
	roleProvider RoleProvider,
	totpReplayGuard twofactor.ReplayGuard,
	twoFAPending twofactor.PendingGuard,
	authInvalidator TokenInvalidator,
	accessTokenRepo repository.AccessTokenRepository,
	streamRevoker StreamRevoker,
	followRequestAccepter FollowRequestBulkAccepter,
	background *bgwork.Group,
) *Handler {
	return &Handler{
		userService:           userService,
		idGen:                 idGen,
		noteHider:             noteHider,
		entityPacker:          entityPacker,
		roleProvider:          roleProvider,
		totpReplayGuard:       totpReplayGuard,
		twoFAPending:          twoFAPending,
		authInvalidator:       authInvalidator,
		accessTokenRepo:       accessTokenRepo,
		streamRevoker:         streamRevoker,
		followRequestAccepter: followRequestAccepter,
		background:            background,
	}
}
