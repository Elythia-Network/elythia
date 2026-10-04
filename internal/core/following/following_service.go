// Package following provides the UserFollowingService for managing follow
// relationships and follow requests.
package following

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// Errors returned by Service.
var (
	// ErrAlreadyFollowing is returned when the follower already follows the followee.
	ErrAlreadyFollowing = errors.New("already following")
	// ErrNotFollowing is returned when there is no following relationship to delete.
	ErrNotFollowing = errors.New("not following")
	// ErrSelfFollow is returned when a user attempts to follow themselves.
	ErrSelfFollow = errors.New("cannot follow yourself")
	// ErrFolloweeNotFound is returned when the target user does not exist.
	ErrFolloweeNotFound = errors.New("followee not found")
	// ErrRequestNotFound is returned when the follow request does not exist.
	ErrRequestNotFound = errors.New("follow request not found")
	// ErrAlreadyRequested is returned when a follow request already exists.
	ErrAlreadyRequested = errors.New("already requested")
	// ErrBlocked is returned when the followee has blocked the follower.
	ErrBlocked = errors.New("blocking relationship prevents this operation")
	// ErrBlocking is returned when the follower is blocking the followee.
	// upstream following/create は方向で BLOCKING / BLOCKED を区別する (#1562)。
	ErrBlocking = errors.New("you are blocking that user")
)

// BlockingChecker reports whether one user has blocked another. パッケージ間の
// 循環依存を避けるためinterfaceで受け取る (実装は core/blocking)。
type BlockingChecker interface {
	IsBlocked(blockerID, blockeeID string) (bool, error)
}

// FollowResult represents the outcome of a Follow call.
type FollowResult struct {
	// Following is non-nil when the relationship was created directly.
	Following *model.Following
	// Request is non-nil when a follow request was created instead of following directly.
	Request *model.FollowRequest
}

// FollowOptions controls behavior of the Follow operation.
// upstream Misskey TS UserFollowingService.follow() の options object と
// 同等の役割。今後 silent / requestId 等の field を追加する余地を残す。
type FollowOptions struct {
	// WithReplies sets the initial value of `Following.withReplies` (and the
	// bridging `FollowRequest.withReplies` for locked followees) for the
	// newly created row. fanout 層が follower の per-followee setting として
	// 参照する (#1056 / PR #1055)。
	WithReplies bool
	// Silent suppresses the follower's `follow` main stream event and user
	// webhook when the following is created directly.
	//
	// 本家 follow の silent と同じ (insertFollowingDoc の `!silent` が止めるのは
	// フォローした側の follow だけで、followed・通知・申請の経路は変えない)。
	// フォローの CSV インポート (core/transfer) と、silent の付いた
	// relationship キューの follow ジョブが使う。
	Silent bool
}

// NotificationHook is invoked after follow/follow-request events to create
// notification entries. パッケージ間の循環依存を避けるためinterfaceで受け取る。
type NotificationHook interface {
	OnFollowed(followerID, followeeID string)
	OnFollowRequested(followerID, followeeID string)
	OnFollowAccepted(followerID, followeeID string)
	// OnFollowRejected is called after a follow request is rejected so the
	// corresponding `receiveFollowRequest` notification can be purged.
	OnFollowRejected(followerID, followeeID string)
}

// FederationHook is invoked after follow/unfollow/accept events involving a
// remote user so that the ActivityPub layer can deliver Follow/Undo/Accept
// activities. パッケージ間の循環依存を避けるためinterfaceで受け取る。
type FederationHook interface {
	OnLocalFollowed(follower, followee *model.User)
	OnLocalUnfollowed(follower, followee *model.User)
	OnLocalFollowAccepted(follower, followee *model.User)
}

// ChartHook is invoked after follow / unfollow events so the chart
// subsystem can record per-user and per-instance follow deltas.
// パッケージ間の循環依存を避けるため interface で受け取る (実装は
// core/chart/charthook)。
type ChartHook interface {
	OnFollow(follower, followee *model.User)
	OnUnfollow(follower, followee *model.User)
}

// WebhookHook is invoked after a follow / unfollow / follow-accepted event so
// that user webhooks subscribed to `follow` / `unfollow` / `followed` can fire.
// 循環依存を避けるため interface で受け取る (実装は core/webhook)。
type WebhookHook interface {
	OnFollow(follower, followee *model.User)
	OnUnfollow(follower, followee *model.User)
	OnFollowed(follower, followee *model.User)
}

// MainStreamPublisher emits real-time events to a single target user's `main`
// WebSocket channel. Used here to deliver `follow`, `followed`, `unfollow`,
// and `receiveFollowRequest` events so the frontend can reflect follow state
// changes immediately. 循環依存を避けるため interface で受け取る (実装は
// internal/stream)。
type MainStreamPublisher interface {
	PublishMainEvent(userID, eventType string, body any)
}

// Service manages following relationships and follow requests.
type Service struct {
	userRepo            repository.UserRepository
	followingRepo       repository.FollowingRepository
	followRequestRepo   repository.FollowRequestRepository
	instanceRepo        repository.InstanceRepository // optional, for #596 incremental counters
	idGen               id.Generator
	notificationHook    NotificationHook
	blockingChecker     BlockingChecker
	federationHook      FederationHook
	chartHook           ChartHook
	webhookHook         WebhookHook
	mainStreamPublisher MainStreamPublisher
	// userPacker は main stream の follow 系イベントの利用者を Webhook と同じ形に組む (#3330)。
	userPacker UserPacker
	// meUpdatedPublisher はフォロー申請の作成・承認・取り消しで followee に
	// meUpdated を流す (#3330)。
	meUpdatedPublisher MeUpdatedPublisher
	// relationReload は follow 変更を streaming connection へ通知する (#2400)。
	relationReload RelationReloadPublisher
	// silencedChecker は meta.silencedHosts の判定。未配線なら承認要求を
	// 増やさない (= 従来どおり) が、production では必ず配線する。
	silencedChecker SilencedHostChecker
	// instanceStatsEnabled は meta.enableStatsForFederatedInstances を読む。
	// 未配線なら本家の既定値 (true) と同じく常に集計する。
	instanceStatsEnabled func() bool
}

// SilencedHostChecker reports whether a remote host is silenced.
type SilencedHostChecker interface {
	IsSilenced(host string) bool
}

// SetSilencedHostChecker wires the silenced-host lookup.
func (s *Service) SetSilencedHostChecker(c SilencedHostChecker) {
	s.silencedChecker = c
}

// HasSilencedHostChecker reports whether the silenced-host lookup was wired.
//
// 未配線だとサイレンスしたホストからのフォローが承認なしで通り、followers
// 限定ノートがそのまま配送される。起動時検査に使う。
func (s *Service) HasSilencedHostChecker() bool { return s != nil && s.silencedChecker != nil }

// RelationReloadPublisher notifies streaming connections that a viewer's
// following snapshot changed (#2400)。実装は stream 側の adapter。
//
// followingSnapshot は接続確立時にしか取らないため、これが無いと follow /
// unfollow の直後は timeline の reply gate が古いままになる。
type RelationReloadPublisher interface {
	PublishFollowingReload(userID string)
}

// SetRelationReloadPublisher wires the streaming reload publisher. 未配線なら
// 通知しない (= 従来どおり再接続まで stale)。
func (s *Service) SetRelationReloadPublisher(p RelationReloadPublisher) {
	s.relationReload = p
}

// publishFollowingReload notifies the follower's streaming connections.
//
// 変わるのは **follower 側**の snapshot (自分が誰を follow しているか)。followee
// 側の followingSnapshot は変わらない。
func (s *Service) publishFollowingReload(followerID string) {
	if s.relationReload == nil || followerID == "" {
		return
	}
	s.relationReload.PublishFollowingReload(followerID)
}

// NewService creates a new following Service.
func NewService(
	userRepo repository.UserRepository,
	followingRepo repository.FollowingRepository,
	followRequestRepo repository.FollowRequestRepository,
	idGen id.Generator,
) *Service {
	return &Service{
		userRepo:          userRepo,
		followingRepo:     followingRepo,
		followRequestRepo: followRequestRepo,
		idGen:             idGen,
	}
}

// SetNotificationHook attaches a NotificationHook used by Follow/AcceptRequest.
func (s *Service) SetNotificationHook(h NotificationHook) {
	s.notificationHook = h
}

// SetBlockingChecker attaches a BlockingChecker used by Follow.
func (s *Service) SetBlockingChecker(c BlockingChecker) {
	s.blockingChecker = c
}

// SetFederationHook attaches a FederationHook used to dispatch Follow / Undo /
// Accept activities to remote inboxes.
func (s *Service) SetFederationHook(h FederationHook) {
	s.federationHook = h
}

// SetChartHook attaches a ChartHook invoked after follow / unfollow.
func (s *Service) SetChartHook(h ChartHook) {
	s.chartHook = h
}

// SetWebhookHook attaches a WebhookHook invoked after follow / unfollow /
// follow-accepted events so user webhooks subscribed to follow events fire.
func (s *Service) SetWebhookHook(h WebhookHook) {
	s.webhookHook = h
}

// SetMainStreamPublisher attaches a publisher used to emit `main` channel
// events (`follow`, `followed`, `unfollow`, `receiveFollowRequest`). Optional —
// nil disables emit.
func (s *Service) SetMainStreamPublisher(p MainStreamPublisher) {
	s.mainStreamPublisher = p
}

// UserPacker packs the user carried by the follow-related main stream events.
// *userpack.Packer satisfies it; the user webhooks use the same packer.
type UserPacker interface {
	Lite(u *model.User) entity.UserLite
	DetailedNotMe(ctx context.Context, target, viewer *model.User) (entity.UserDetailed, bool)
}

// SetUserPacker wires the packer for the main stream follow / unfollow /
// followed / receiveFollowRequest bodies. Unwired, the events carry the
// legacy shape built from the user row alone (test fixtures).
func (s *Service) SetUserPacker(p UserPacker) {
	s.userPacker = p
}

// HasUserPacker reports whether the user packer was wired.
//
// 未配線だと main stream の follow / unfollow が profile を読まない従来の形に
// 落ち、unfollow の後もフォロワー限定のカウントが見える。起動時検査に使う。
func (s *Service) HasUserPacker() bool { return s.userPacker != nil }

// MeUpdatedPublisher emits `meUpdated` (the user's own MeDetailed) to a local
// user's main stream. The `i` API handler satisfies it.
type MeUpdatedPublisher interface {
	PublishMeUpdated(userID string)
}

// SetMeUpdatedPublisher wires the meUpdated publisher used when a follow request
// is created, accepted or cancelled.
func (s *Service) SetMeUpdatedPublisher(p MeUpdatedPublisher) {
	s.meUpdatedPublisher = p
}

// HasMeUpdatedPublisher reports whether the meUpdated publisher was wired.
//
// 未配線だとフォロー申請の作成・承認・取り消しで followee の
// hasPendingReceivedFollowRequest がリロードまで変わらない。起動時検査に使う。
func (s *Service) HasMeUpdatedPublisher() bool { return s.meUpdatedPublisher != nil }

// publishMeUpdated emits `meUpdated` to u when u is local.
//
// 本家は createFollowRequest / acceptFollowRequest / cancelFollowRequest で
// followee を MeDetailed で pack して流す (受け取った申請の印
// hasPendingReceivedFollowRequest が変わるため)。リモートの利用者には
// 購読する接続が無い。
func (s *Service) publishMeUpdated(u *model.User) {
	if s.meUpdatedPublisher == nil || u == nil || !u.IsLocal() {
		return
	}
	s.meUpdatedPublisher.PublishMeUpdated(u.ID)
}

// PackedFollowWebhookHook is implemented by a WebhookHook that accepts the
// followee already packed for the main stream, so one follow / unfollow packs
// the user once (upstream publishes the same packed value to both).
type PackedFollowWebhookHook interface {
	OnFollowPacked(follower, followee *model.User, packed func() (entity.UserDetailed, bool))
	OnUnfollowPacked(follower, followee *model.User, packed func() (entity.UserDetailed, bool))
}

// followeePacker returns a memoized builder of followee packed as
// UserDetailedNotMe seen by follower, or nil when the packer is unwired.
//
// main stream と Webhook で共有し、組み立てを 1 回にする (本家は 1 回 pack した
// 値を両方へ流す)。どちらも要らなければ一度も組まない。
func (s *Service) followeePacker(follower, followee *model.User) func() (entity.UserDetailed, bool) {
	if s.userPacker == nil {
		return nil
	}
	return sync.OnceValues(func() (entity.UserDetailed, bool) {
		return s.userPacker.DetailedNotMe(context.Background(), followee, follower)
	})
}

// notifyFolloweeEvent emits `follow` / `unfollow` to the follower's webhooks
// (when withWebhook) and main stream, packing followee at most once.
func (s *Service) notifyFolloweeEvent(event string, follower, followee *model.User, isFollowing, withWebhook bool) {
	packed := s.followeePacker(follower, followee)
	if withWebhook && s.webhookHook != nil {
		s.dispatchFolloweeWebhook(event, follower, followee, packed)
	}
	s.publishFolloweeEvent(event, follower, followee, isFollowing, packed)
}

func (s *Service) dispatchFolloweeWebhook(event string, follower, followee *model.User, packed func() (entity.UserDetailed, bool)) {
	if p, ok := s.webhookHook.(PackedFollowWebhookHook); ok && packed != nil {
		if event == "follow" {
			p.OnFollowPacked(follower, followee, packed)
		} else {
			p.OnUnfollowPacked(follower, followee, packed)
		}
		return
	}
	if event == "follow" {
		s.webhookHook.OnFollow(follower, followee)
	} else {
		s.webhookHook.OnUnfollow(follower, followee)
	}
}

// publishFolloweeEvent publishes `follow` / `unfollow` to the follower's main
// stream, carrying followee packed as UserDetailedNotMe seen by the follower.
//
// 本家 UserFollowingService は Webhook と同じ
// `pack(followee, follower, {schema: 'UserDetailedNotMe'})` の値を流し、
// フォローした側がローカルのときだけ publish する。関係の行は呼び出し時点の
// ものを読むので、follow では isFollowing=true とフォロワー限定のカウントが、
// unfollow では isFollowing=false と伏せたカウントが乗る。profile を読めない
// ときは本家と同じく流さない。
func (s *Service) publishFolloweeEvent(event string, follower, followee *model.User, isFollowing bool, packed func() (entity.UserDetailed, bool)) {
	if s.mainStreamPublisher == nil || follower == nil || followee == nil || !follower.IsLocal() {
		return
	}
	if packed == nil {
		s.mainStreamPublisher.PublishMainEvent(follower.ID, event, entity.PackUserForFollowStreamEvent(followee, isFollowing, false, s.idGen))
		return
	}
	d, ok := packed()
	if !ok {
		return
	}
	s.mainStreamPublisher.PublishMainEvent(follower.ID, event, d)
}

// publishFollowerLite publishes `followed` / `receiveFollowRequest` to the
// followee's main stream, carrying follower packed as UserLite. 本家と同じく
// followee がローカルのときだけ流す。
func (s *Service) publishFollowerLite(event string, follower, followee *model.User) {
	if s.mainStreamPublisher == nil || follower == nil || followee == nil || !followee.IsLocal() {
		return
	}
	body := entity.PackUserLite(follower)
	if s.userPacker != nil {
		body = s.userPacker.Lite(follower)
	}
	s.mainStreamPublisher.PublishMainEvent(followee.ID, event, body)
}

// SetInstanceRepo wires an InstanceRepository so Follow / Unfollow /
// AcceptRequest 経路で remote host の followersCount / followingCount を
// incremental に保つ (#596)。未配線でも core 機能には影響しないが、admin
// dashboard の federation pie chart が起動直後以外で実値を反映しなくなる。
func (s *Service) SetInstanceRepo(r repository.InstanceRepository) {
	s.instanceRepo = r
}

// SetInstanceStatsGate wires the meta.enableStatsForFederatedInstances lookup.
// When the function returns false, follow / unfollow no longer touch the
// instance followersCount / followingCount columns. Unwired means enabled,
// which is the upstream default.
func (s *Service) SetInstanceStatsGate(enabled func() bool) {
	s.instanceStatsEnabled = enabled
}

// MetaInstanceStatsGate returns a gate for SetInstanceStatsGate that reads
// meta.enableStatsForFederatedInstances on every call. A nil repository or a
// failed fetch reports enabled (the upstream default).
func MetaInstanceStatsGate(metaRepo repository.MetaRepository) func() bool {
	return func() bool {
		if metaRepo == nil {
			return true
		}
		m, err := metaRepo.Fetch()
		if err != nil || m == nil {
			// 読めないときは既定値 (true) に倒す。集計列は best-effort で、
			// 起動時の RecomputeFollowCounts が整合を取り直す。
			return true
		}
		return m.EnableStatsForFederatedInstances
	}
}

// adjustInstanceCountsForFollowing は Following 行の create / delete 時に
// 該当 remote instance の followersCount / followingCount を delta 分動かす。
// delta は +1 (create) / -1 (delete)。
// best-effort: 失敗しても呼び出し元には伝えない (起動時 RecomputeFollowCounts
// で eventually 復旧)。block による解除も UnfollowForBlock 経由でここを通る。
//
// 列の向きは本家 UserFollowingService.insertFollowingDoc / decrementFollowing に
// 揃える (#3330)。instance の列は「その host の側から見た」数なので:
//   - remote → local のフォローは、その host の利用者が**フォローしている**数
//     なので followingCount を動かす
//   - local → remote のフォローは、その host の利用者が**フォローされている**数
//     なので followersCount を動かす
//
// 以前は逆に足していたので、federation/instances の subscribing / publishing と
// federation/stats の上位が入れ替わっていた。片側がローカルの組だけを数えるのも
// 本家の if / else if と同じ。
func (s *Service) adjustInstanceCountsForFollowing(f *model.Following, delta int) {
	if s.instanceRepo == nil || delta == 0 {
		return
	}
	// 本家は meta.enableStatsForFederatedInstances が false なら instance の
	// 集計列を一切動かさない。
	if s.instanceStatsEnabled != nil && !s.instanceStatsEnabled() {
		return
	}
	followerRemote := f.FollowerHost != nil && *f.FollowerHost != ""
	followeeRemote := f.FolloweeHost != nil && *f.FolloweeHost != ""
	switch {
	case followerRemote && !followeeRemote:
		if err := s.instanceRepo.IncrementFollowingCount(*f.FollowerHost, delta); err != nil {
			slog.Warn("instance counter: followingCount adjust failed",
				"host", *f.FollowerHost, "delta", delta, "err", err)
		}
	case !followerRemote && followeeRemote:
		if err := s.instanceRepo.IncrementFollowersCount(*f.FolloweeHost, delta); err != nil {
			slog.Warn("instance counter: followersCount adjust failed",
				"host", *f.FolloweeHost, "delta", delta, "err", err)
		}
	}
}

// Follow creates a following relationship from follower to followee.
// If the followee is locked, a FollowRequest is created instead and the
// FollowResult contains Request set instead of Following.
//
// Phase 2 Step B implementation only handles local follows. Federation
// (HTTP signatures, AP delivery), block checks, and notifications are
// added in later phases.
// shouldAutoAcceptFollow reports whether a follow toward a locked followee should be
// auto-accepted (create Following directly) instead of a follow request, mirroring
// upstream UserFollowingService.follow の autoAccept 分岐 (#2106 N21): followee が
// local user で profile.autoAcceptFollowed=true、かつ followee→follower の follow が既に
// 存在する (相互フォロー) とき。既に follow 済のケースは呼び元の ErrAlreadyFollowing guard で
// 別途処理される。
func (s *Service) shouldAutoAcceptFollow(followee *model.User, followerID string) bool {
	if followee.Host != nil { // remote followee は相手側のサーバーで承認処理される
		return false
	}
	profile, err := s.userRepo.FindProfileByUserID(followee.ID)
	if err != nil || profile == nil || !profile.AutoAcceptFollowed {
		return false
	}
	mutual, err := s.followingRepo.Exists(followee.ID, followerID)
	return err == nil && mutual
}

func (s *Service) Follow(followerID, followeeID string, opts FollowOptions) (*FollowResult, error) {
	if followerID == followeeID {
		return nil, ErrSelfFollow
	}

	followee, err := s.userRepo.FindByID(followeeID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrFolloweeNotFound
	}
	follower, err := s.userRepo.FindByID(followerID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrFolloweeNotFound
	}

	// 既存のフォロー関係をチェック
	if exists, err := s.followingRepo.Exists(followerID, followeeID); err != nil {
		return nil, err
	} else if exists {
		return nil, ErrAlreadyFollowing
	}

	// ブロック関係があるとフォロー不可 (双方向で確認)。upstream
	// UserFollowingService.follow は blocking (follower→followee) を先に
	// 検査して 'blocking'、次に blocked (followee→follower) を 'blocked' と
	// 方向別に投げ、create.ts が BLOCKING / BLOCKED に map する (#1562)。
	// 相互ブロック時に BLOCKING が優先される順序も upstream に合わせる。
	if s.blockingChecker != nil {
		if blocking, err := s.blockingChecker.IsBlocked(followerID, followeeID); err != nil {
			return nil, err
		} else if blocking {
			return nil, ErrBlocking
		}
		if blocked, err := s.blockingChecker.IsBlocked(followeeID, followerID); err != nil {
			return nil, err
		} else if blocked {
			return nil, ErrBlocked
		}
	}

	// follow request 化の判定 (upstream UserFollowingService.follow の OR 条件)。
	// #2106 N22: locked に加え、bot follower かつ local followee の profile.carefulBot=true でも
	// follow request 化する (「Bot からのフォローに慎重」設定)。
	needsApproval := followee.IsLocked
	if !needsApproval && follower.IsBot && followee.Host == nil {
		if profile, perr := s.userRepo.FindProfileByUserID(followeeID); perr == nil && profile != nil && profile.CarefulBot {
			needsApproval = true
		}
	}
	// **サイレンスしたホストからのフォローは必ず承認制にする。**
	//
	// upstream `UserFollowingService.follow` の 4 つ目の OR 条件
	// (`isLocalUser(followee) && isRemoteUser(follower) &&
	// isSilencedHost(meta.silencedHosts, follower.host)`)。これが無いと、
	// サイレンス指定したインスタンスの利用者が未施錠のローカルアカウントを
	// 承認なしで即フォローでき、以後 followers 限定ノートが配送される。
	// mk-go は `IsSilenced` の機構自体は持っていて、リモートノートの取り込み
	// では使っているのに、この経路だけ繋がっていなかった。
	if !needsApproval && s.silencedChecker != nil &&
		followee.Host == nil && follower.Host != nil && *follower.Host != "" &&
		s.silencedChecker.IsSilenced(*follower.Host) {
		needsApproval = true
	}
	// #2106 N21: followee が local + profile.autoAcceptFollowed=true + 相互フォロー
	// (followee→follower) のときは follow request を作らず即 Following を成立させる
	// (下の通常 Following 作成経路に fall through)。remote follower の場合は handleFollow が
	// result.Following を見て AP Accept を返送する。
	if needsApproval && !s.shouldAutoAcceptFollow(followee, followerID) {
		// 既存リクエストがあればエラー
		if exists, err := s.followRequestRepo.Exists(followerID, followeeID); err != nil {
			return nil, err
		} else if exists {
			return nil, ErrAlreadyRequested
		}
		req := &model.FollowRequest{
			ID:           s.idGen.Generate(time.Now()),
			FollowerID:   followerID,
			FolloweeID:   followeeID,
			FollowerHost: follower.Host,
			FolloweeHost: followee.Host,
			WithReplies:  opts.WithReplies,
		}
		if err := s.followRequestRepo.Create(req); err != nil {
			return nil, err
		}
		if s.notificationHook != nil {
			s.notificationHook.OnFollowRequested(followerID, followeeID)
		}
		// リモートの承認制followeeにはAP Follow activityを送る必要がある
		// (相手側の inbox で FollowRequest が作られ、承認時に Accept が
		// 返ってくる)。federationHook.OnLocalFollowed は shouldDeliverFollow
		// でローカル→リモート条件を確認してから配信する。
		if s.federationHook != nil {
			s.federationHook.OnLocalFollowed(follower, followee)
		}
		// TS本家: UserFollowingService は `receiveFollowRequest` を
		// followee の main stream に publish する。body は follower の UserLite
		// (`pack(follower, followee)`)。
		s.publishFollowerLite("receiveFollowRequest", follower, followee)
		s.publishMeUpdated(followee)
		return &FollowResult{Request: req}, nil
	}

	f := &model.Following{
		ID:           s.idGen.Generate(time.Now()),
		FollowerID:   followerID,
		FolloweeID:   followeeID,
		FollowerHost: follower.Host,
		FolloweeHost: followee.Host,
		WithReplies:  opts.WithReplies,
	}
	if err := s.followingRepo.Create(f); err != nil {
		return nil, err
	}

	// どちらかが移行済みなら、カウント・instance の集計列・chart を動かさない。
	// 本家 insertFollowingDoc の `if (!followeeUser.movedToUri &&
	// !followerUser.movedToUri)` と同じガードで、unfollow 側
	// (decrementFollowing) と対になる (#3330)。片側だけ守ると、移行済みの
	// アカウントへのフォロー (リモートからの Follow は拒否されない) で増えた
	// カウントが、解除のときには減らずに残る。
	moved := isMoved(follower) || isMoved(followee)
	if !moved {
		// counterの更新は失敗しても致命ではないが、現状はerror伝播
		if err := s.userRepo.IncrementFollowingCount(followerID, 1); err != nil {
			return nil, err
		}
		if err := s.userRepo.IncrementFollowersCount(followeeID, 1); err != nil {
			return nil, err
		}
		// remote instance の集計列を incremental 更新 (#596)。failure は warn-log
		// のみで握り潰し、起動時 RecomputeFollowCounts が安全網。
		s.adjustInstanceCountsForFollowing(f, 1)
	}

	if s.notificationHook != nil {
		s.notificationHook.OnFollowed(followerID, followeeID)
	}
	if s.federationHook != nil {
		// アウトバウンド: local→remote follow の場合は Follow activity を送る。
		// インバウンド (remote→local) の Accept 返送は processor 側で original
		// Follow の ID を使って直接発行する (ここで OnLocalFollowAccepted を
		// 呼ぶと original ID が失われ、相手が Accept をマッチングできない)。
		s.federationHook.OnLocalFollowed(follower, followee)
	}
	if s.chartHook != nil && !moved {
		s.chartHook.OnFollow(follower, followee)
	}
	// Webhookはfollower側で `follow`、followee側で `followed` を発火する。
	if s.webhookHook != nil {
		s.webhookHook.OnFollowed(follower, followee)
	}
	// TS本家 UserFollowingService.follow() は follower の main に `follow`
	// (相手側の UserDetailedNotMe)、followee の main に `followed` (自分を
	// follow した UserLite) を publish する。frontend MkFollowButton.onFollowChange
	// は body.isFollowing / body.hasPendingFollowRequestFromYou を読む。
	if !opts.Silent {
		s.notifyFolloweeEvent("follow", follower, followee, true, true)
	}
	s.publishFollowerLite("followed", follower, followee)

	s.publishFollowingReload(followerID)
	return &FollowResult{Following: f}, nil
}

// Unfollow removes a following relationship from follower to followee.
func (s *Service) Unfollow(followerID, followeeID string) error {
	return s.unfollow(followerID, followeeID, unfollowOpts{deliver: true, notify: true})
}

// UnfollowWithoutNotify removes the following and delivers Undo(Follow) /
// Reject like Unfollow, but emits no main-stream `unfollow` event or user
// webhook.
//
// 本家 UserFollowingService.unfollow の silent=true と同じ。relationship キューの
// silent な unfollow (admin/federation/remove-all-following、凍結時の
// unFollowAll) が使う。
func (s *Service) UnfollowWithoutNotify(followerID, followeeID string) error {
	return s.unfollow(followerID, followeeID, unfollowOpts{deliver: true})
}

// UnfollowQuiet removes the following without any AP delivery, main-stream
// event or user webhook. Counts and charts are still updated.
//
// 消えたインスタンスとのフォロー関係の片付け (#3067) 用。管理者の操作で何百件も
// 解除するので、利用者の webhook へ「フォロー解除」を 1 件ずつ飛ばさない
// (upstream の UserFollowingService.unfollow の silent=true も webhook と main
// stream の publish を止める)。**配送も止める点は upstream の silent と違う** —
// あちらは silent でも Undo / Reject を配送するが、片付けの相手はもう存在しない
// ので送っても届かない。
func (s *Service) UnfollowQuiet(followerID, followeeID string) error {
	return s.unfollow(followerID, followeeID, unfollowOpts{})
}

// unfollowOpts selects the side effects of unfollow.
type unfollowOpts struct {
	// deliver は相手へ Undo(Follow) / Reject を配送するか。
	deliver bool
	// notify は main stream の unfollow と利用者の webhook を出すか。
	notify bool
}

func (s *Service) unfollow(followerID, followeeID string, opts unfollowOpts) error {
	if followerID == followeeID {
		return ErrSelfFollow
	}

	f, err := s.followingRepo.FindByPair(followerID, followeeID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrNotFollowing
	}
	if err := s.followingRepo.Delete(f); err != nil {
		return err
	}
	// どちらかが移行済みならカウントを触らない。upstream
	// UserFollowingService.decrementFollowing の
	// `if (!follower.movedToUri && !followee.movedToUri)` と同じガード。
	//
	// アカウント移行時に PostMoveProcess が「unfollow せずカウントだけ落とす」
	// 調整を先に済ませている (#2418)。ここで無条件に減らすと、後から同じ関係を
	// unfollow したときに二重に減る。
	moved := hasMoved(s.userRepo, followerID) || hasMoved(s.userRepo, followeeID)
	if !moved {
		if err := s.userRepo.IncrementFollowingCount(followerID, -1); err != nil {
			return err
		}
		if err := s.userRepo.IncrementFollowersCount(followeeID, -1); err != nil {
			return err
		}
		// remote instance counter -1 (#596)
		s.adjustInstanceCountsForFollowing(f, -1)
	}
	// hook 呼び出しに必要なユーザー情報を一度だけロードして使い回す。
	// 失敗してもベストエフォートで continue する。
	if s.federationHook != nil || s.chartHook != nil || s.webhookHook != nil || s.mainStreamPublisher != nil {
		follower, ferr := s.userRepo.FindByID(followerID)
		followee, eerr := s.userRepo.FindByID(followeeID)
		if ferr == nil && eerr == nil {
			// #2106 N11: deliver=false (inbound Reject 処理) では Undo(Follow) を
			// rejecter へ逆配送しない。chart / webhook / main stream は upstream
			// remoteReject も publishUnfollow するので維持する。
			if opts.deliver && s.federationHook != nil {
				s.federationHook.OnLocalUnfollowed(follower, followee)
			}
			// チャートもカウントと同じく、どちらかが移行済みなら触らない。本家
			// decrementFollowing は perUserFollowingChart / instanceChart の更新を
			// `!movedToUri` の分岐の中でだけ行う (移行済みの側は "TODO: adjust
			// charts" のまま何もしない)。
			if s.chartHook != nil && !moved {
				s.chartHook.OnUnfollow(follower, followee)
			}
			// TS本家は自分が unfollow した相手を main に publish する
			// (フォローボタン等の即時反映)。本家 unfollow の
			// `!silent && isLocalUser(follower)` と同じく、follower がローカルの
			// ときだけ main stream と Webhook に出す。
			if opts.notify {
				s.publishUnfollow(follower, followee)
			}
		}
	}
	s.publishFollowingReload(followerID)
	return nil
}

// AcceptRequest accepts a pending follow request, deleting the request and
// creating a Following relationship.
func (s *Service) AcceptRequest(followeeID, followerID string) error {
	// ブロック関係があると承認不可 (双方向で確認)。upstream の
	// acceptFollowRequest 自体にはこの検査は無く、block 時に cancelRequest で
	// 申請を消すことだけで守っている。本実装は Block の申請取り消しを
	// best-effort にしているため (取り消し失敗時も block 自体は成立させる)、
	// 取り消しが失敗して申請が残った場合の多層防御としてここでも検査する。
	// Follow() の blockingChecker 検査と対称 (self=followeeID, target=followerID)。
	if s.blockingChecker != nil {
		if blocking, err := s.blockingChecker.IsBlocked(followeeID, followerID); err != nil {
			return err
		} else if blocking {
			return ErrBlocking
		}
		if blocked, err := s.blockingChecker.IsBlocked(followerID, followeeID); err != nil {
			return err
		} else if blocked {
			return ErrBlocked
		}
	}

	req, err := s.followRequestRepo.FindByPair(followerID, followeeID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrRequestNotFound
	}
	if err := s.followRequestRepo.Delete(req); err != nil {
		return err
	}
	f := &model.Following{
		ID:           s.idGen.Generate(time.Now()),
		FollowerID:   req.FollowerID,
		FolloweeID:   req.FolloweeID,
		FollowerHost: req.FollowerHost,
		FolloweeHost: req.FolloweeHost,
		WithReplies:  req.WithReplies,
	}
	if err := s.followingRepo.Create(f); err != nil {
		return err
	}
	// 承認でも本家は insertFollowingDoc を通るので、Follow と同じ移行済みの
	// ガードを掛ける (#3330)。
	moved := hasMoved(s.userRepo, req.FollowerID) || hasMoved(s.userRepo, req.FolloweeID)
	if !moved {
		if err := s.userRepo.IncrementFollowingCount(req.FollowerID, 1); err != nil {
			return err
		}
		if err := s.userRepo.IncrementFollowersCount(req.FolloweeID, 1); err != nil {
			return err
		}
		// remote instance counter +1 (#596)
		s.adjustInstanceCountsForFollowing(f, 1)
	}
	if s.notificationHook != nil {
		// follower側には「follow request accepted」、followee側には通常の
		// follow と同じく「follow」通知を作る (TS本家 insertFollowingDoc が
		// acceptFollowRequest 経由でも同じ通知を発火するため、本家互換)。
		// これを呼ばないとfollowee の通知欄に「リクエストが来ました」しか
		// 残らず「フォローされました」が出ない (#349 コメント)。
		s.notificationHook.OnFollowAccepted(req.FollowerID, req.FolloweeID)
		s.notificationHook.OnFollowed(req.FollowerID, req.FolloweeID)
	}
	if s.federationHook != nil || s.chartHook != nil || s.webhookHook != nil || s.mainStreamPublisher != nil || s.meUpdatedPublisher != nil {
		follower, ferr := s.userRepo.FindByID(req.FollowerID)
		followee, eerr := s.userRepo.FindByID(req.FolloweeID)
		if ferr == nil && eerr == nil {
			if s.federationHook != nil {
				s.federationHook.OnLocalFollowAccepted(follower, followee)
			}
			if s.chartHook != nil && !moved {
				s.chartHook.OnFollow(follower, followee)
			}
			if s.webhookHook != nil {
				s.webhookHook.OnFollowed(follower, followee)
			}
			// Accept によって Following が成立するので、Follow() と同じく
			// follower の main に `follow`、followee の main に `followed`
			// を publish する (TS本家 UserFollowingService.acceptFollow
			// と同等)。follow event body は UserDetailed + isFollowing=true。
			s.notifyFolloweeEvent("follow", follower, followee, true, true)
			s.publishFollowerLite("followed", follower, followee)
			// 本家 acceptFollowRequest は最後に followee へ meUpdated を流す
			// (受け取った申請が 1 件減る)。
			s.publishMeUpdated(followee)
		}
	}
	// Accept で Following が成立するので follower の snapshot が変わる (#2400)。
	// Follow() だけに通知を置くと、承認制アカウントへの follow が反映されない。
	s.publishFollowingReload(req.FollowerID)
	return nil
}

// requestEvents selects the main stream / webhook events emitted when a
// pending follow request is removed.
//
// 本家は申請を消す経路ごとに流すものが違う。following/requests/cancel と
// AP の Undo(Follow) (cancelFollowRequest) は followee に meUpdated だけ、
// following/requests/reject (rejectFollowRequest) は follower に unfollow と
// Webhook だけ、ブロックに伴う取り消し (UserBlockingService.cancelRequest) は
// その両方を流す。
type requestEvents struct {
	// meUpdated は followee がローカルなら followee の main に meUpdated を流す。
	meUpdated bool
	// unfollow は follower がローカルなら follower の main に unfollow を流し、
	// Webhook の unfollow も出す。
	unfollow bool
}

// RejectRequest rejects a pending follow request received by the followee.
//
// 本家 rejectFollowRequest と同じく、follower がローカルなら unfollow を
// main stream と Webhook に出す (publishUnfollow)。meUpdated は流さない。
func (s *Service) RejectRequest(followeeID, followerID string) error {
	return s.rejectRequest(followeeID, followerID, requestEvents{unfollow: true})
}

func (s *Service) rejectRequest(followeeID, followerID string, ev requestEvents) error {
	req, err := s.followRequestRepo.FindByPair(followerID, followeeID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrRequestNotFound
	}
	if err := s.followRequestRepo.Delete(req); err != nil {
		return err
	}
	// followee 側に残る `receiveFollowRequest` 通知を掃除する (#349 コメント)。
	// そうしないと、同じユーザーから後で再リクエストが来たときに過去の解決済
	// 通知まで一覧に復活する。
	if s.notificationHook != nil {
		s.notificationHook.OnFollowRejected(req.FollowerID, req.FolloweeID)
	}
	// 配送も通知も未配線なら DB lookup を省く。
	if !s.needsRequestUsers() {
		return nil
	}
	followee, eerr := s.userRepo.FindByID(followeeID)
	if eerr != nil {
		return nil
	}
	follower, ferr := s.userRepo.FindByID(followerID)
	// リモート follower からの pending request を reject したときに Reject(Follow)
	// を配信して相手側の following を解消する (本家
	// UserFollowingService.rejectFollowRequest 相当)。OnLocalUnfollowed は
	// follower=remote / followee=local 時に Reject を送る分岐を持っている。
	if s.federationHook != nil && ferr == nil {
		s.federationHook.OnLocalUnfollowed(follower, followee)
	}
	if ferr != nil {
		follower = nil
	}
	s.publishRequestRemoved(follower, followee, ev)
	return nil
}

// CancelRequest cancels an outgoing follow request created by the follower.
// フォロー申請の送信元 (local follower) がキャンセル操作をしたときに呼ばれる。
// リモート locked followee に対する申請の場合は Undo Follow activity を相手に
// 送って pending FollowRequest を取り消してもらう必要がある。送らないと相手
// 側に stale なリクエストが残り、後で承認されても local には Following が
// 存在しない矛盾状態になる。
//
// 本家 cancelFollowRequest と同じく、流すのは followee の meUpdated だけ。
// follower へ unfollow は流さない (フォローボタンは API の応答で戻す)。
func (s *Service) CancelRequest(followerID, followeeID string) error {
	return s.cancelRequest(followerID, followeeID, requestEvents{meUpdated: true})
}

func (s *Service) cancelRequest(followerID, followeeID string, ev requestEvents) error {
	req, err := s.followRequestRepo.FindByPair(followerID, followeeID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrRequestNotFound
	}
	if err := s.followRequestRepo.Delete(req); err != nil {
		return err
	}
	// hooks / publisher のいずれもが未配線なら DB lookup 自体を省く。テスト時に
	// userRepo が minimal でも影響しない。
	if !s.needsRequestUsers() {
		return nil
	}
	followee, eerr := s.userRepo.FindByID(followeeID)
	if eerr != nil {
		return nil
	}
	// federationHook.OnLocalUnfollowed は shouldDeliverFollow でローカル →
	// リモート条件を判定するので、ローカル followee の場合は自動的に no-op。
	// follower の lookup に失敗したときは配送と follower 宛ての通知だけ飛ばし、
	// followee の meUpdated は続ける。
	follower, ferr := s.userRepo.FindByID(followerID)
	if s.federationHook != nil && ferr == nil {
		s.federationHook.OnLocalUnfollowed(follower, followee)
	}
	if ferr != nil {
		follower = nil
	}
	s.publishRequestRemoved(follower, followee, ev)
	return nil
}

// needsRequestUsers reports whether removing a follow request has any side
// effect that needs the user rows.
func (s *Service) needsRequestUsers() bool {
	return s.federationHook != nil || s.mainStreamPublisher != nil || s.webhookHook != nil || s.meUpdatedPublisher != nil
}

// publishRequestRemoved emits the events selected by ev after a follow request
// was removed. follower may be nil when it could not be loaded.
func (s *Service) publishRequestRemoved(follower, followee *model.User, ev requestEvents) {
	if ev.meUpdated {
		s.publishMeUpdated(followee)
	}
	if ev.unfollow {
		s.publishUnfollow(follower, followee)
	}
}

// publishUnfollow emits `unfollow` to a local follower's main stream and user
// webhooks, carrying followee packed as UserDetailedNotMe seen by follower.
//
// 本家 UserFollowingService.publishUnfollow (と、ブロック・unfollow の同じ形の
// 処理) は follower がローカルのときだけ出す。
func (s *Service) publishUnfollow(follower, followee *model.User) {
	if follower == nil || followee == nil || !follower.IsLocal() {
		return
	}
	s.notifyFolloweeEvent("unfollow", follower, followee, false, true)
}

// CancelFollowRequestsBetween cancels any pending follow requests between a and
// b, in both directions. blocking.Service が block 作成時に呼ぶ。upstream
// UserBlockingService.block の cancelRequest 双方向呼び出し相当。
//
// **申請を出した側の locality で後始末が変わる。**
//   - follower が local: CancelRequest 経路。followee が remote なら
//     Undo(Follow) を配送する。
//   - follower が remote: RejectRequest 経路。followee 側に残る
//     receiveFollowRequest 通知を掃除し、remote follower に Reject を配送する
//     (相手側の pending request も解消させる)。
//
// 流すものはどちらの経路でも本家 UserBlockingService.cancelRequest と同じで、
// followee がローカルなら meUpdated、follower がローカルで silent でなければ
// unfollow (main stream と Webhook)。silent はインポートしたブロック用。
//
// 該当する request が無い direction は no-op。2 方向のうち先に失敗したものを
// 返すが、呼び出し元 (block) は warn に留めて block を成立させる。
func (s *Service) CancelFollowRequestsBetween(a, b string, silent bool) error {
	var firstErr error
	for _, dir := range [2][2]string{{a, b}, {b, a}} {
		if err := s.cancelOneFollowRequest(dir[0], dir[1], silent); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// cancelOneFollowRequest cancels the (followerID → followeeID) request if it
// exists, choosing the path that performs the right cleanup for the follower's
// locality. Request が無い場合は no-op (ErrRequestNotFound を返さない)。
func (s *Service) cancelOneFollowRequest(followerID, followeeID string, silent bool) error {
	if _, err := s.followRequestRepo.FindByPair(followerID, followeeID); err != nil {
		// **DB 障害を not-found に丸めない (#2799)。** 呼び出し元が warn に
		// 出せるよう種別は残す。
		if repository.IsNotFound(err) {
			return nil
		}
		return err
	}
	ev := requestEvents{meUpdated: true, unfollow: !silent}
	// local からの申請取り消しは remote followee への Undo(Follow) を伴う
	// cancelRequest 経路に寄せる。
	if follower, err := s.userRepo.FindByID(followerID); err == nil && follower != nil && follower.IsLocal() {
		if err := s.cancelRequest(followerID, followeeID, ev); err != nil && !errors.Is(err, ErrRequestNotFound) {
			return err
		}
		return nil
	}
	// remote からの申請、または follower を引けないときは followee 側の通知を
	// 掃除して Reject を送る経路にする。userRepo の一時障害でも行の削除は進む。
	// 事前確認後に並行して消えた request は no-op にする (accept との競合)。
	if err := s.rejectRequest(followeeID, followerID, ev); err != nil && !errors.Is(err, ErrRequestNotFound) {
		return err
	}
	return nil
}

// RemoteReject handles an inbound Reject(Follow) from a remote followee: it
// removes the pending follow request and the following (if any) without any AP
// delivery, then emits `unfollow` to the local follower's main stream and user
// webhooks.
//
// 本家 UserFollowingService.remoteReject と同じ。申請も関係の行も消すだけで
// 何も配送せず (#2106 N11)、publishUnfollow は行が無くても 1 回だけ出す。
// 以前は UnfollowSilent と CancelRequest を続けて呼んでいたので、申請を
// 拒否されたときに Webhook が出ず、rejecter へ Undo(Follow) を送り返していた。
func (s *Service) RemoteReject(followerID, followeeID string) error {
	if req, err := s.followRequestRepo.FindByPair(followerID, followeeID); err == nil {
		if err := s.followRequestRepo.Delete(req); err != nil {
			return err
		}
	} else if !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2799)。
		return err
	}
	if err := s.unfollow(followerID, followeeID, unfollowOpts{}); err != nil && !errors.Is(err, ErrNotFollowing) {
		return err
	}
	if s.mainStreamPublisher == nil && s.webhookHook == nil {
		return nil
	}
	follower, ferr := s.userRepo.FindByID(followerID)
	followee, eerr := s.userRepo.FindByID(followeeID)
	if ferr != nil || eerr != nil {
		return nil
	}
	s.publishUnfollow(follower, followee)
	return nil
}

// UnfollowForBlock removes the follower→followee following for a block, with
// the same side effects as Unfollow. A missing following is not an error.
// silent suppresses only the `unfollow` main stream event and user webhook.
//
// 本家 UserBlockingService.block は UserFollowingService.unfollow(blocker,
// blockee, silent) を双方向で呼ぶ。silent が止めるのは unfollow の publish と
// Webhook だけで、カウント・チャート・Undo(Follow) / Reject(Follow) の配送は
// silent でも行う (ブロックのインポートでも相手のサーバーへ解除を伝える)。
func (s *Service) UnfollowForBlock(followerID, followeeID string, silent bool) error {
	err := s.unfollow(followerID, followeeID, unfollowOpts{deliver: true, notify: !silent})
	if errors.Is(err, ErrNotFollowing) {
		return nil
	}
	return err
}

// ListReceivedRequests returns follow requests received by userID. sinceID /
// untilID はMisskey本家の pagination cursor (空文字で無指定扱い)。
func (s *Service) ListReceivedRequests(userID string, limit int, sinceID, untilID string) ([]*model.FollowRequest, error) {
	return s.followRequestRepo.ListReceived(userID, limit, sinceID, untilID)
}

// ListSentRequests returns follow requests sent by userID.
func (s *Service) ListSentRequests(userID string, limit int, sinceID, untilID string) ([]*model.FollowRequest, error) {
	return s.followRequestRepo.ListSent(userID, limit, sinceID, untilID)
}

// ListReceivedFollowing returns followings where userID is the followee.
// 「userIDをフォローしているユーザー」(=followers) を返す。
func (s *Service) ListReceivedFollowing(userID, sinceID, untilID string, limit, offset int) ([]*model.Following, error) {
	return s.followingRepo.ListFollowersWithCursor(userID, sinceID, untilID, limit, offset)
}

// ListSentFollowing returns followings where userID is the follower.
// 「userIDがフォローしているユーザー」を返す。
func (s *Service) ListSentFollowing(userID, sinceID, untilID string, limit, offset int) ([]*model.Following, error) {
	return s.followingRepo.ListFollowingWithCursor(userID, sinceID, untilID, limit, offset)
}

// ListFollowingForList wraps repository.ListFollowingForList for /api/following/list
// (upstream 2026.5.2 #17385 + #17416)。cursor + notify-only filter 版。
func (s *Service) ListFollowingForList(followerID, sinceID, untilID string, notification bool, limit int) ([]*model.Following, error) {
	return s.followingRepo.ListFollowingForList(followerID, sinceID, untilID, notification, limit)
}

// UpdateRelation applies partial updates to a single follower/followee pair.
// Used by following/update to toggle per-relation flags (notify, withReplies).
// Returns nil if the relation does not exist (no-op).
func (s *Service) UpdateRelation(followerID, followeeID string, fields map[string]any) error {
	return s.followingRepo.UpdateRelation(followerID, followeeID, fields)
}

// IsFollowing reports whether follower currently follows followee. Used by
// following/update to emit NOT_FOLLOWING before applying a no-op update
// (#1562、upstream update.ts は Following row 不在で notFollowing を返す)。
func (s *Service) IsFollowing(followerID, followeeID string) (bool, error) {
	return s.followingRepo.Exists(followerID, followeeID)
}

// UpdateAllByFollower applies partial updates to every Following row whose
// followerId matches. Used by following/update-all.
func (s *Service) UpdateAllByFollower(followerID string, fields map[string]any) error {
	return s.followingRepo.UpdateAllByFollower(followerID, fields)
}

// hasMoved reports whether the user has a non-empty movedToUri. 参照に失敗した
// 場合は false を返し、カウント調整を通常どおり行う (取得できないことを理由に
// カウントを放置すると、移行していない普通の unfollow でカウントがずれる)。
func hasMoved(userRepo repository.UserRepository, userID string) bool {
	if userRepo == nil {
		return false
	}
	u, err := userRepo.FindByID(userID)
	if err != nil {
		return false
	}
	return isMoved(u)
}

// isMoved reports whether u has a non-empty movedToUri.
func isMoved(u *model.User) bool {
	return u != nil && u.MovedToURI != nil && *u.MovedToURI != ""
}

// HasBlockingChecker reports whether the blocking checker was wired.
//
// 未配線だとブロック関係を無視してフォローが成立する。gate は
// `IsBlocked(followerID, followeeID)` と `IsBlocked(followeeID, followerID)` の
// **双方向**で、後者は inbound の AP Follow も通す。起動時検査に使う (#2708)。
func (s *Service) HasBlockingChecker() bool { return s.blockingChecker != nil }
