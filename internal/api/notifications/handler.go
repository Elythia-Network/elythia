// Package notifications provides /api/notifications/* endpoints.
package notifications

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/notehide"
	"github.com/elythia-network/elythia/internal/api/pagination"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// ModeratorChecker reports whether a user has moderation privileges.
// 実装は core/role.Service。
type ModeratorChecker interface {
	IsModerator(userID string) bool
	IsAdministrator(userID string) bool
	// HasRolePolicy は運営向け通知のうち、モデレーター権限ではなく**ロール
	// ポリシーで gate されているもの**の再確認に使う (#2987)。絵文字の申請は
	// `canManageCustomEmojis` で審査でき、`HasRolePolicy` が短絡するのは root と
	// 管理者だけ (モデレーターは短絡しない) ので、IsModerator では判定できない。
	HasRolePolicy(userID, policyKey string) bool
}

// Handler handles notifications-related API endpoints.
type Handler struct {
	svc           *notification.Service
	idGen         id.Generator
	userRepo      repository.UserRepository
	noteRepo      repository.NoteRepository
	queryService  *corenote.QueryService
	followReqRepo repository.FollowRequestRepository
	// mutingRepo は read 時の valid-notifier filter で viewer の muting set
	// (ListMuteeIDs) を引くための optional 依存 (#1775)。未配線なら muting filter
	// は素通り (suspended / mutedInstances filter は引き続き効く)。
	mutingRepo           repository.MutingRepository
	instanceRepo         repository.InstanceRepository
	emojiRepo            repository.EmojiRepository
	testNotifier         TestNotifier
	roleLookup           entity.RoleLookup
	chatInvitationLookup entity.ChatInvitationLookup
	// abuseReportStates は abuseReport 通知の現在の状態を read 時に batch で
	// 引く (#2868)。**未配線なら abuseReport を返さない** (roleAssigned と同じ
	// fail-closed)。状態を出せないまま「未対応」に見せると、対処済みの通報に
	// 別のモデレーターが二重で当たる。
	abuseReportStates       AbuseReportStateLookup
	abuseUnresolvedCount    func() (int64, error)
	emojiApplicationLookup  entity.EmojiApplicationLookup
	signupApplicationLookup entity.SignupApplicationLookup
	// moderatorChecker は read 時に abuseReport 通知の閲覧権限を再確認する
	// (#2868)。対象ユーザー ID が Extra に入り通報の存在自体が機微なので、権限を
	// 失った元モデレーターが admin/abuse-user-reports の 403 を迂回して
	// 通知欄から読み続けられないようにする。**未配線なら abuseReport を
	// 返さない (fail-closed)** — 判定できないまま出すより出さないほうが安全側。
	moderatorChecker ModeratorChecker
	// noteFieldResolver は通知に埋め込む note の後段 field (files / channel /
	// myReaction / poll の isVoted) を埋める (#2735)。packer は Files を空スライスで
	// 初期化するだけなので、未配線だと添付メディアが一切返らない。
	noteFieldResolver *entity.NoteFieldResolver
	// accessTokenRepo は notifications/create の 'app' 通知で、リクエストに使われた
	// access token を raw token から再解決し、header/icon の fallback (token.name /
	// token.iconUrl) + appAccessTokenId を埋めるために使う (#1557)。未配線なら
	// fallback 無し。
	accessTokenRepo repository.AccessTokenRepository
}

// SetAccessTokenRepo wires the access token repo used by notifications/create
// to resolve the request's app token for header/icon fallback (#1557)。
func (h *Handler) SetAccessTokenRepo(r repository.AccessTokenRepository) {
	h.accessTokenRepo = r
}

// SetRoleLookup wires the lookup used to pack roleAssigned notifications'
// embedded role (#1559)。
func (h *Handler) SetRoleLookup(fn entity.RoleLookup) { h.roleLookup = fn }

// SetModeratorChecker wires the moderation check used to re-verify who may
// read abuseReport notifications (#2868).
//
// **未配線なら abuseReport を返さない (fail-closed)。** 対象ユーザー ID が Extra に入り通報の存在自体が機微なので、判定できないまま出すより出さないほうが安全側。
func (h *Handler) SetModeratorChecker(c ModeratorChecker) { h.moderatorChecker = c }

// AbuseReportStateLookup resolves the resolution state of several reports at
// once (#2868)。実装は repository.AbuseReportRepository.FindStatesByIDs。
type AbuseReportStateLookup func(ids []string) (map[string]model.AbuseReportState, error)

// SetAbuseReportLookup wires the read-time state lookup for abuseReport
// notifications (#2868)。未配線なら abuseReport 通知を返さない (fail-closed)。
func (h *Handler) SetAbuseReportLookup(fn AbuseReportStateLookup) { h.abuseReportStates = fn }

// SetAbuseReportUnresolvedCounter wires the count of unresolved reports shown
// on abuseReport notifications (#3200)。実装は
// repository.AbuseReportRepository.CountUnresolved。未配線なら件数を出さない。
func (h *Handler) SetAbuseReportUnresolvedCounter(fn func() (int64, error)) {
	h.abuseUnresolvedCount = fn
}

// SetEmojiApplicationLookup wires the read-time state lookup for
// emojiApplicationProcessed notifications (#2934)。未配線なら返さない
// (fail-closed、abuseReport と同じ)。
//
// **batch にしていない。** abuseReport はモデレーター全員へ配るので 1 ページに
// 数百件並びうるが、申請の結果通知は申請者本人にしか飛ばず、同じページに何件も
// 並ぶ形にならない。N+1 を避けるための複雑さに見合わない。
func (h *Handler) SetEmojiApplicationLookup(fn entity.EmojiApplicationLookup) {
	h.emojiApplicationLookup = fn
}

// SetSignupApplicationLookup wires the read-time state lookup for
// signupApplicationReceived notifications (#2987)。未配線なら通知ごと返さない
// (fail-closed。状態を引けないまま出すと、処理済みの申請に二重で当たる)。
func (h *Handler) SetSignupApplicationLookup(fn entity.SignupApplicationLookup) {
	h.signupApplicationLookup = fn
}

// SetChatInvitationLookup wires the lookup used to pack
// chatRoomInvitationReceived notifications' embedded invitation (#1559)。
func (h *Handler) SetChatInvitationLookup(fn entity.ChatInvitationLookup) {
	h.chatInvitationLookup = fn
}

// SetNoteFieldResolver wires the resolver that populates the embedded note's
// `files` (and channel / myReaction / poll isVoted) on notification payloads
// (#2735)。未配線なら従来どおり files は空配列のまま。
func (h *Handler) SetNoteFieldResolver(r *entity.NoteFieldResolver) {
	h.noteFieldResolver = r
}

// notificationOptions returns the per-call packing options (role / chat
// invitation lookups, viewer, note field resolver) for the given viewer
// (= notifiee)。
//
// viewer は note の後段 field 解決にも使われる。upstream NotificationEntityService は
// note を `noteEntityService.pack(noteId, { id: meId }, { detail: true })` で
// pack するので、myReaction / poll の isVoted も notifiee 視点で埋まる。
func (h *Handler) notificationOptions(viewerID string, rows []entity.NotificationItem) ([]entity.NotificationOption, error) {
	lookup, err := h.batchAbuseReportLookup(rows)
	if err != nil {
		return nil, err
	}
	return []entity.NotificationOption{
		entity.WithRoleLookup(h.roleLookup),
		entity.WithChatInvitationLookup(h.chatInvitationLookup),
		entity.WithAbuseReportLookup(lookup),
		entity.WithEmojiApplicationLookup(h.emojiApplicationLookup),
		entity.WithSignupApplicationLookup(h.signupApplicationLookup),
		entity.WithViewer(viewerID),
		entity.WithNoteFieldResolver(h.noteFieldResolver),
	}, nil
}

// batchAbuseReportLookup resolves every abuseReport state in one query (#2868).
//
// **1 件ずつ引かない。** 通知一覧はページあたり最大 100 件で、1 件ずつ引くと
// リクエストごとに数百 SELECT が直列に走る。noteFieldResolver が「ページで
// lookup 1 回」に畳んでいるのと同じ形にする。
//
// **DB 障害を「削除済み」に丸めない (#2792)。** 丸めると一時的な接続断で通報の
// 通知だけが消え、しかも既読位置は進むので未読の合図が失われる。エラーは
// 呼び出し元へ返して 500 にする。
//
// abuseReport が 1 件も無いページでは 1 回も引かない。
func (h *Handler) batchAbuseReportLookup(rows []entity.NotificationItem) (entity.AbuseReportLookup, error) {
	ids := make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)
	for _, item := range rows {
		n := item.N
		if n == nil || n.Type != notification.TypeAbuseReport {
			continue
		}
		id, _ := n.Extra["reportId"].(string)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		// 未配線と区別が付かない nil ではなく、常に false を返す lookup を渡す。
		// この経路には abuseReport が無いので実際には呼ばれない。
		return func(string) (entity.AbuseReportStatus, bool) {
			return entity.AbuseReportStatus{}, false
		}, nil
	}
	if h.abuseReportStates == nil {
		// 未配線: fail-closed (通知を drop する)。
		return nil, nil
	}
	states, err := h.abuseReportStates(ids)
	if err != nil {
		return nil, err
	}
	// 件数はページで 1 回だけ引く (インスタンス全体の値なので通知ごとに変わらない)。
	// エラーは状態の lookup と同じく 500 にする (#2792)。
	var unresolved *int64
	if h.abuseUnresolvedCount != nil {
		n, err := h.abuseUnresolvedCount()
		if err != nil {
			return nil, err
		}
		unresolved = &n
	}
	return func(reportID string) (entity.AbuseReportStatus, bool) {
		st, ok := states[reportID]
		if !ok {
			return entity.AbuseReportStatus{}, false
		}
		out := entity.AbuseReportStatus{Resolved: st.Resolved, UnresolvedCount: unresolved}
		if st.ResolvedAs != nil {
			out.ResolvedAs = *st.ResolvedAs
		}
		if st.AssigneeID != nil {
			out.AssigneeID = *st.AssigneeID
		}
		return out, true
	}, nil
}

// NewHandler creates a new notifications Handler.
func NewHandler(svc *notification.Service, idGen id.Generator) *Handler {
	return &Handler{svc: svc, idGen: idGen}
}

// TestNotifier creates a 'test' notification on the caller's own stream so the
// test-notification endpoint can verify web push / streaming delivery (#1559)。
// 循環依存を避けるため interface で受け取る (実装は core/notification.Hook)。
type TestNotifier interface {
	OnTest(userID string)
}

// SetTestNotifier attaches a TestNotifier used by TestNotification so the test
// notification also fires web push (upstream notifications/test-notification)。
func (h *Handler) SetTestNotifier(n TestNotifier) { h.testNotifier = n }

// SetRepos attaches repositories for resolving user/note objects in notifications.
func (h *Handler) SetRepos(userRepo repository.UserRepository, noteRepo repository.NoteRepository) {
	h.userRepo = userRepo
	h.noteRepo = noteRepo
}

// SetInstanceRepo attaches an InstanceRepository so user.instance (used by
// frontend InstanceTicker for theme color / software name) is populated on
// notification payloads (#415 follow-up).
func (h *Handler) SetInstanceRepo(r repository.InstanceRepository) {
	h.instanceRepo = r
}

// SetEmojiRepo attaches an EmojiRepository so custom emoji shortcodes in
// user.name / note.text resolve to URLs in notification payloads.
func (h *Handler) SetEmojiRepo(r repository.EmojiRepository) {
	h.emojiRepo = r
}

// instanceLookup returns an InstanceLookup adapter (nil-safe when the
// repository hasn't been wired).
func (h *Handler) instanceLookup() entity.InstanceLookup {
	if h.instanceRepo == nil {
		return nil
	}
	return h.instanceRepo
}

// emojiLookup returns an EmojiLookup adapter (nil-safe when the repository
// hasn't been wired).
func (h *Handler) emojiLookup() entity.EmojiLookup {
	if h.emojiRepo == nil {
		return nil
	}
	return h.emojiRepo
}

// SetQueryService attaches a note QueryService used to gate embedded notes by
// viewer visibility. #1444 で「i/notifications の note embed が CanSeeNote
// check 無しで followers / specified note を漏らす」IDOR を塞ぐ。未配線の
// 場合は fail-closed として note embed 自体を丸ごと skip する (production の
// router.go は必ず wire する)。
func (h *Handler) SetQueryService(qs *corenote.QueryService) {
	h.queryService = qs
}

// SetFollowRequestRepo attaches a FollowRequestRepository. 受信済み
// receiveFollowRequest 通知について対応する follow_request 行が無ければ
// (承認/拒否により消えた) 通知一覧から除外する用途 (本家
// NotificationEntityService と同じ挙動)。
func (h *Handler) SetFollowRequestRepo(r repository.FollowRequestRepository) {
	h.followReqRepo = r
}

// ListRequest is the request body for notifications.
type ListRequest struct {
	Limit        *int     `json:"limit"`
	IncludeTypes []string `json:"includeTypes"`
	ExcludeTypes []string `json:"excludeTypes"`
	SinceID      string   `json:"sinceId"`
	UntilID      string   `json:"untilId"`
	SinceDate    *int64   `json:"sinceDate"`
	UntilDate    *int64   `json:"untilDate"`
	// MarkAsRead controls whether fetching the notification list implicitly
	// marks all notifications as read. nil / true means "mark as read"
	// (default), explicit false leaves the read state untouched.
	// 本家 i/notifications の暗黙引数。Misskey フロントが
	// `notifications/mark-all-as-read` を明示呼び出しせず、通知一覧 fetch の
	// 副作用で badge が消えることを期待しているため (#420)。
	MarkAsRead *bool `json:"markAsRead"`
}

// Show handles POST /api/i/notifications - returns the authenticated user's
// notification timeline. 既定は降順だが、sinceId 単独のページだけは昇順になる
// (upstream と同じ。Service.List の ascending)。
func (h *Handler) Show(c echo.Context) error {
	user := middleware.GetUser(c)
	req, ok := h.bindListRequest(c)
	if !ok {
		return apierr.JSONInvalidParam(c)
	}

	// upstream i/notifications.ts が getNotifications の前に持つ 2 つの早期
	// return (#2835)。既読化もここで止まる。
	if emptyByTypeFilter(req) {
		return c.JSON(http.StatusOK, []any{})
	}
	// 早期 return の**後**に obsolete を除去する (#2837)。順序が結果を分ける。
	req.IncludeTypes = stripObsoleteTypes(req.IncludeTypes)
	req.ExcludeTypes = stripObsoleteTypes(req.ExcludeTypes)

	filtered, notifierByID, noteByID, err := h.collectNotifications(c, user, req)
	if err != nil {
		return apierr.JSONInternalError(c)
	}

	items := make([]entity.NotificationItem, 0, len(filtered))
	for _, n := range filtered {
		items = append(items, entity.NotificationItem{
			N:    n,
			User: notifierByID[n.NotifierID],
			Note: noteByID[n.NoteID],
		})
	}
	opts, err := h.notificationOptions(user.ID, items)
	if err != nil {
		slog.Error("notifications: resolve abuse report states failed", "err", err)
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	out := entity.PackNotifications(items, h.idGen, h.instanceLookup(), h.emojiLookup(), opts...)
	// depth-2 embed hide (#1570): collectNotifications の #1444 CanSeeNote gate は
	// 見えない note を丸ごと落とすが embed (renote/reply) には再帰しない。通知 note の
	// embed と著者設定ゲートを viewer 可視性で適用する。これを欠くと #1570 で塞いだ
	// i/notifications の depth-2 embed IDOR が再オープンする。
	notehide.HideNotificationNotes(user, out)
	h.maybeMarkAsRead(c, user, req)
	return c.JSON(http.StatusOK, out)
}

// notificationTypeList is the set counted by the excludeTypes "covers
// everything" check (emptyByTypeFilter)。**upstream 由来の型のみ。**
//
// **core の registry から導出する (#2898)。** 以前はここに upstream の一覧を
// リテラルで持っていたが、core 側の `Type` 定数との間で片側更新が起きていた
// (`importCompleted` が core にだけあった)。同じ一覧を 2 箇所に置くと、
// 固有型を足すたびにその穴を踏む。
//
// **obsolete も mk-go 固有も含まない。** 固有型を入れると、upstream の 20 種
// しか送らないクライアント (misskey-js の notificationTypes を使うもの) の
// 「すべて無効」が被覆判定を外れ、emptyByTypeFilter の早期 return を抜けて
// 既読化まで進む。1 件も返していないのに既読位置が飛ぶ (#2833 / #2835 が塞いだ
// 害の再オープン)。固有型は enum には入るので filter 値としては指定できる。
var notificationTypeList = notification.UpstreamTypeNames()

// obsoleteNotificationTypeList mirrors upstream `obsoleteNotificationTypes`。
// paramDef の enum には含まれるが、excludeTypes の全指定判定には数えない。
var obsoleteNotificationTypeList = notification.ObsoleteTypeNames()

// notificationTypeEnum mirrors upstream `[...notificationTypes,
// ...obsoleteNotificationTypes]` (types.ts)。includeTypes/excludeTypes の
// 要素検証に使う (#2062)。mk-go が produce しない type も upstream paramDef の
// enum に含まれるため許容する (filter で何も match しないだけ)。
//
// **2 つの list から組み立てる。** 手で二重に持つと、片方に type を足したときに
// もう片方が古いまま残る。enum にだけ足すと emptyByTypeFilter の全指定判定が
// 足りない type を無視して**早く空を返しすぎる**方向に壊れる。
var notificationTypeEnum = buildNotificationTypeEnum()

func buildNotificationTypeEnum() map[string]bool {
	// mk-go 固有の型も filter 値としては受け付ける。全指定判定
	// (notificationTypeList) には入れない — 理由は notificationTypeList の
	// コメントを参照。
	mkgo := notification.MkGoTypeNames()
	m := make(map[string]bool, len(notificationTypeList)+len(obsoleteNotificationTypeList)+len(mkgo))
	for _, t := range notificationTypeList {
		m[t] = true
	}
	for _, t := range obsoleteNotificationTypeList {
		m[t] = true
	}
	for _, t := range mkgo {
		m[t] = true
	}
	return m
}

// obsoleteNotificationTypeSet is the lookup form of obsoleteNotificationTypeList.
var obsoleteNotificationTypeSet = func() map[string]bool {
	m := make(map[string]bool, len(obsoleteNotificationTypeList))
	for _, t := range obsoleteNotificationTypeList {
		m[t] = true
	}
	return m
}()

// stripObsoleteTypes removes obsolete types from a type filter, mirroring
// upstream's `.filter(type => !obsoleteNotificationTypes.includes(type))` (#2837).
//
// **emptyByTypeFilter の後に掛ける。** upstream もその順序で、入れ替えると結果が
// 逆になる — `includeTypes:[]` は「何も含めない」で空配列を返すが、
// `includeTypes:["pollVote"]` は除去後に空になり、svc.List の
// `len(includeSet) > 0` が false になって**filter 無し = 全件**になる。
//
// **全部落ちたら nil を返す。** 長さ 0 の slice を返すと「明示的な
// `includeTypes:[]`」と区別が付かなくなり、collectNotificationsWithDropped の
// fail-closed guard がそれを「何も含めない」と読んで全件落とす。upstream の
// `filter()` は `[]` を返すが、あちらは nil / 空の区別を持たない言語なので
// 形ではなく**意味**に合わせる — 除去後に空になったものの意味は「filter 無し」。
func stripObsoleteTypes(types []string) []string {
	if len(types) == 0 {
		return types
	}
	out := make([]string, 0, len(types))
	for _, t := range types {
		if !obsoleteNotificationTypeSet[t] {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// emptyByTypeFilter reports whether the type filters alone make the query
// pointless, mirroring the two early returns upstream i/notifications.ts and
// i/notifications-grouped.ts have before `getNotifications` (#2835).
//
//   - includeTypes が明示的に空 = 何も含めない
//   - excludeTypes が notificationTypes を全て覆う
//
// **既読化より前に判定するのが要点。** MarkAllAsRead が進める先は fetch が返した
// 行ではなくストリームの最新エントリなので、ここで抜けずに既読化すると、
// 1 件も返していないのにユーザーが受け取っていない通知まで既読位置が飛ぶ
// (#2833 と同じ害の別経路)。
//
// excludeTypes が nil / 空のときは false。upstream の
// `notificationTypes.every(type => ps.excludeTypes?.includes(type))` も、
// undefined なら `?.` で undefined になり every が false、空配列なら
// `[].includes` が false で every が false になる。
func emptyByTypeFilter(req ListRequest) bool {
	if req.IncludeTypes != nil && len(req.IncludeTypes) == 0 {
		return true
	}
	if len(req.ExcludeTypes) == 0 {
		return false
	}
	excluded := make(map[string]struct{}, len(req.ExcludeTypes))
	for _, t := range req.ExcludeTypes {
		excluded[t] = struct{}{}
	}
	for _, t := range notificationTypeList {
		if _, ok := excluded[t]; !ok {
			return false
		}
	}
	return true
}

// validNotificationTypes reports whether every element of types is a known
// notification type enum value (nil / empty は valid、#2062)。
func validNotificationTypes(types []string) bool {
	for _, t := range types {
		if !notificationTypeEnum[t] {
			return false
		}
	}
	return true
}

// bindListRequest binds the shared i/notifications(-grouped) request body and
// normalizes the cursor / limit. Returns ok=false on a bind error so callers
// can emit INVALID_PARAM.
func (h *Handler) bindListRequest(c echo.Context) (ListRequest, bool) {
	var req ListRequest
	if err := c.Bind(&req); err != nil {
		return req, false
	}
	// upstream notifications(-grouped).ts は includeTypes/excludeTypes を
	// [...notificationTypes, ...obsoleteNotificationTypes] の enum で検証する。
	// enum 外を ajv 同様 400 で弾く (#2062)。
	if !validNotificationTypes(req.IncludeTypes) || !validNotificationTypes(req.ExcludeTypes) {
		return req, false
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1174)。Notification.ID
	// は notification_service.go で `idGen.Generate(now)` で aidx 形式で発番
	// されるため、aidx 比較 (= 後段の post-fetch filter `n.ID <= sinceID` /
	// `n.ID >= untilID`) で sinceDate / untilDate も正しく効く。Redis Stream
	// native ID と aidx ID は別物だが、本 endpoint の cursor は notification.ID
	// (= aidx) で判定する設計なので adapter pattern で完結する。
	// **列に入らないカーソルもここで ok=false にする (#3025)。** そのまま渡すと
	// `id < ?` の bind parameter で落ちて 500 になる。
	cursorSince, cursorUntil, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return req, false
	}
	req.SinceID, req.UntilID = cursorSince, cursorUntil
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return req, false
	}
	req.Limit = &limit
	return req, true
}

// collectNotifications runs the shared fetch + filter + batched user/note
// resolution path used by both Show and Grouped. It returns the post-filter
// notification slice, in Service.List's own order, plus the resolved notifier-user and
// viewer-visible note maps. The note map is gated by CanSeeNote (#1444 IDOR).
//
// #1953 以降、note-required 通知でその note が map に無いものは**行ごと**落ちる
// (noteId だけの行を返さない)。#1444 当初の「行は残して detail だけ落とす」形では
// ないので、他経路をここに揃えにいくときは注意すること。
func (h *Handler) collectNotifications(c echo.Context, user *model.User, req ListRequest) ([]*notification.Notification, map[string]*model.User, map[string]*model.Note, error) {
	all, dropped, notifierByID, noteByID, err := h.collectNotificationsWithDropped(c, user, req, false)
	if err != nil {
		return nil, nil, nil, err
	}
	return survivors(all, dropped), notifierByID, noteByID, nil
}

// survivors returns the rows of all that are not in dropped, preserving order.
//
// **id で引くので、位置ベースだった旧実装とは縮退の仕方が違う。** 同じ id の行が
// 2 つあって片方だけ drop されると両方消える (fail-closed 側)。逆に id が空の行を
// drop すると、id が空の行がすべて消える。通知 id は aidx で採番されるので実データ
// では起きないが、id を持たない通知を作れるようにするなら位置ベースに戻すこと。
func survivors(all []*notification.Notification, dropped map[string]struct{}) []*notification.Notification {
	out := make([]*notification.Notification, 0, len(all))
	for _, n := range all {
		if _, ok := dropped[n.ID]; ok {
			continue
		}
		out = append(out, n)
	}
	return out
}

// collectNotificationsWithDropped is collectNotifications without applying the
// drops: it returns **every** row Service.List produced plus the id set of the
// ones that must not be rendered.
//
// **grouped は drop 済みの列では正しく畳めない** (#2739)。upstream は raw 列で
// grouping してから pack で落とすので、grouping 条件に当たらない通知 (mute した
// 相手の follow など) を挟んだ 2 件は畳まれない。drop 済みの列を渡すと挟まった
// 通知が区切りとして働かず、両隣が 1 グループになる。
//
// **id 集合だけでは足りない。** 区切りになるかは両隣との pair 述語 (type /
// noteId / renote の target) で決まるので、落とした通知の中身そのものが要る。
// resolveAllNotes は drop 済み行の note まで引くかどうか。grouped だけが true。
func (h *Handler) collectNotificationsWithDropped(c echo.Context, user *model.User, req ListRequest, resolveAllNotes bool) ([]*notification.Notification, map[string]struct{}, map[string]*model.User, map[string]*model.Note, error) {
	// 明示的な includeTypes:[] は空配列を返す (= 何も含めない)。omit (nil) は全
	// type を通す。Go の []string は absent→nil / []→non-nil(len 0) で区別できる
	// (#1546)。
	//
	// **現状ここには到達しない** — Show / Grouped はどちらも emptyByTypeFilter で
	// 先に抜ける (#2835)。直接呼ぶ経路が増えたときのための fail-closed な二重防御。
	// stripObsoleteTypes が全除去で nil を返すのは、その結果 (意味は「filter 無し」)
	// をここへ「何も含めない」として渡さないため (#2837)。
	if req.IncludeTypes != nil && len(req.IncludeTypes) == 0 {
		return nil, map[string]struct{}{}, map[string]*model.User{}, map[string]*model.Note{}, nil
	}

	// svc.List が upstream NotificationService.getNotifications 相当に cursor
	// (sinceId/untilId)・向き (sinceId-only は昇順)・type filter・limit を適用して
	// 返す (#1953)。ここではその後段の drop (解決済み follow request / invalid
	// notifier / 削除済 note) を適用する。**不可視 note の drop は mk-go 独自**で、
	// upstream は hideNote で blank するだけで行を残す (下の #1953 ブロック参照)。
	rows, err := h.svc.List(c.Request().Context(), user.ID, req.SinceID, req.UntilID, (*req.Limit), req.IncludeTypes, req.ExcludeTypes)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	// **drop は列から抜かずに id 集合へ記録する** (#2739)。grouped は全列を
	// 走査して畳んでから、この集合でグループの中身を引く。
	dropped := make(map[string]struct{})

	// 2-pass で組み立てる:
	//   pass 1: followReq filter を通して残った rows と notifierID を集約
	//   pass 2: userRepo.FindManyByIDs / noteRepo.FindManyByIDsWithUser で
	//           まとめて引き、map で O(1) 解決して PackNotifications に渡す
	// (旧実装は 1 通知ごとに FindByID + FindByIDWithRelations を呼んで最大
	// 600 round-trip の N+1 を出していた。#515)。
	filtered := make([]*notification.Notification, 0, len(rows))
	notifierIDSet := make(map[string]struct{})
	for _, n := range rows {
		// 既に解決された follow request (accept / reject で消えた) に対応する
		// receiveFollowRequest 通知はユーザー視点で既に古く「残り続ける」ので
		// 除外する (#349 コメント対応、本家 NotificationEntityService 互換)。
		if n.Type == notification.TypeReceiveFollowReq && h.followReqRepo != nil && n.NotifierID != "" {
			if exists, err := h.followReqRepo.Exists(n.NotifierID, user.ID); err == nil && !exists {
				dropped[n.ID] = struct{}{}
				continue
			}
		}
		filtered = append(filtered, n)
		if n.NotifierID != "" {
			notifierIDSet[n.NotifierID] = struct{}{}
		}
	}

	notifierByID := make(map[string]*model.User, len(notifierIDSet))
	// #2106 N6: notifier fetch が成功したかを記録する。成功時のみ unresolved notifier の
	// notifier-required 通知を drop する (transient な FindManyByIDs 失敗で全件 mass-drop
	// しないため)。userRepo 未配線 / notifier 不要のときは false のままで drop しない。
	notifierFetchOK := false
	if h.userRepo != nil && len(notifierIDSet) > 0 {
		ids := make([]string, 0, len(notifierIDSet))
		for id := range notifierIDSet {
			ids = append(ids, id)
		}
		if users, err := h.userRepo.FindManyByIDs(ids); err == nil {
			notifierFetchOK = true
			for _, u := range users {
				notifierByID[u.ID] = u
			}
		}
	}

	// #1775: read-time valid-notifier filter。create 時 mute だけでなく read 時に
	// も notifier が (a) いま viewer に mute されている / (b) viewer の mutedInstances
	// host / (c) suspended のいずれかなら通知を落とす (upstream
	// NotificationEntityService #filterValidNotifier)。
	filtered = markDropped(dropped, filtered, h.filterValidNotifiers(user.ID, filtered, notifierByID))

	// #2106 N6: notifier user が解決できなかった notifier-required 通知を drop する
	// (upstream NotificationEntityService #packInternal の `needsUser && !userIfNeed →
	// return null`)。misskey-js / misskey_dart は Notification.user を non-null 必須で
	// decode するため、hard-delete された notifier の shape 不整合通知を返すと strict
	// client が当該ページ decode で crash する。transient な fetch 失敗では mass-drop
	// しないよう、fetch 成功時のみ適用する。
	if notifierFetchOK {
		kept := make([]*notification.Notification, 0, len(filtered))
		for _, n := range filtered {
			if n.NotifierID != "" {
				if _, ok := notifierByID[n.NotifierID]; !ok {
					dropped[n.ID] = struct{}{}
					continue
				}
			}
			kept = append(kept, n)
		}
		filtered = kept
	}

	// **grouped は survivor ではなく全列から集める** (#2739)。drop された通知も
	// 含めて畳むが、renote grouping の判定 (target note = renote note の RenoteID)
	// は noteByID からしか導けない。survivor だけを引くと、drop された renote の
	// target が解決できず `renoteTargetID` が "" を返し、**grouping 条件に当たる
	// のに区切りとして働いてしまう** (reaction は通知が持つ NoteID だけで判定
	// するので影響を受けず、renote だけが非対称に壊れる)。
	//
	// upstream は通知自体が `targetNoteId` を持つので note を引かずに判定できる
	// (models/Notification.ts:44)。mk-go は持たないため、この余分な fetch で
	// 代替する。件数は 1 ページぶん (最大 100) に収まる。
	//
	// **Show では引かない。** あちらは drop 済み行の note を一切使わないうえ、
	// FilterVisible が followers note ごとに follow 判定を 1 クエリ出すので、
	// grouped のためのコストを hot path に載せない。
	//
	// **これは性能のための分岐で、応答は変わらない** (Show に true を渡しても
	// テストは落ちない)。逆向き — grouped に false を渡す — は挙動が変わる。
	noteSource := filtered
	if resolveAllNotes {
		noteSource = rows
	}
	noteIDSet := make(map[string]struct{})
	for _, n := range noteSource {
		if n.NoteID != "" {
			noteIDSet[n.NoteID] = struct{}{}
		}
	}
	// noteByID 構築には CanSeeNote ゲートを挟む。followers / specified note は
	// 通知 recipient が author の follower でない / visibleUserIds に含まれない
	// 場合に不可視になる。これが無いと B の followers note への reply 通知経由で
	// 「A が B を follow していなくても」B の full note + author が漏れる
	// (#1444 IDOR)。queryService 未配線時は fail-closed で note 解決を完全 skip
	// する (= production router は必ず wire、test の partial setup を漏れに
	// 繋げない)。
	noteByID := make(map[string]*model.Note, len(noteIDSet))
	if h.noteRepo != nil && h.queryService != nil && len(noteIDSet) > 0 {
		ids := make([]string, 0, len(noteIDSet))
		for id := range noteIDSet {
			ids = append(ids, id)
		}
		if notes, err := h.noteRepo.FindManyByIDsWithUser(ids); err == nil {
			visible := h.queryService.FilterVisible(user, notes)
			for _, nn := range visible {
				noteByID[nn.ID] = nn
			}
		}
	}

	// upstream packMany は note-required 通知 (noteId を持つ type) の note が**削除済**
	// で pack できない場合、通知行ごと drop する
	// (`validNotifications.filter(x => !('noteId' in x) || packedNotes.has(x.noteId))`、
	// #1953)。以前は note embed だけ nil 化して行を残していたが、upstream は noteId
	// だけの通知を返さない。
	//
	// **不可視 note の扱いは upstream と違う。** 上の packMany は
	// NotificationEntityService のほうで、note を引くのは NoteEntityService.packMany。
	// そちらは入力と 1:1 で null を返さず、可視性は hideNote が blank するだけなので
	// 行は残る。mk-go は noteByID が FilterVisible 済で、不可視 note の行もここで
	// 落ちる。行の存在自体を伏せる分こちらが安全側なので維持する (#1444 の方針)。
	//
	// noteByID に無い NoteID を持つ行を落とす。repo / queryService 配線済のときだけ適用する
	// (未配線の partial test では note 解決自体が走らず、全 note-required 通知を
	// 誤って落とさないため)。
	// **列は絞らない。** この関数は drop を適用せず、全行と drop 集合の両方を
	// 返す契約 (grouped が raw 列を要るため)。ここでやるのは dropped の登録だけで、
	// 実際に落とすのは呼び出し側の `survivors`。
	if h.noteRepo != nil && h.queryService != nil {
		for _, n := range filtered {
			if n.NoteID == "" {
				continue
			}
			if _, ok := noteByID[n.NoteID]; !ok {
				dropped[n.ID] = struct{}{}
			}
		}
	}
	return rows, dropped, notifierByID, noteByID, nil
}

// markDropped records the rows of before that are missing from after into
// dropped, and returns after. filterValidNotifiers は survivor 列を返すので、
// 差分を取って drop 集合へ反映する。
func markDropped(dropped map[string]struct{}, before, after []*notification.Notification) []*notification.Notification {
	kept := make(map[string]struct{}, len(after))
	for _, n := range after {
		kept[n.ID] = struct{}{}
	}
	for _, n := range before {
		if _, ok := kept[n.ID]; !ok {
			dropped[n.ID] = struct{}{}
		}
	}
	return after
}

// SetMutingRepo wires the muting repository used by the read-time
// valid-notifier filter to load the viewer's muting set (#1775)。
func (h *Handler) SetMutingRepo(r repository.MutingRepository) {
	h.mutingRepo = r
}

// filterValidNotifiers drops notifications whose notifier is no longer a valid
// source for the viewer at READ time, mirroring upstream
// NotificationEntityService #validateNotifier (#1775):
//   - notifierId in the viewer's muting set
//   - notifier.host in the viewer's mutedInstances
//   - notifier.isSuspended
//
// Notifications without a notifierId (system notifications) always pass.
//
// **この関数は notifier を解決できなかった通知を落とさない。** 落とすのは
// 呼び出し元で、notifier の fetch に成功したときだけ (notifierFetchOK、#2106 N6)。
//
// **fail-open が 3 箇所ある。** notifier の FindManyByIDs (呼び出し元)、mute 一覧の
// ListMuteeIDs、instance-mute の FindProfileByUserID。どれも失敗を握り潰して
// 「その filter は無かったこと」にするので、**3 つすべてが成功したページでだけ**
// endpoint として upstream と同じになる。upstream は該当 fetch を await して
// そのまま throw する (= 500 で行を返さない) 側。一覧を全滅させるより残すほうが
// 害が小さいと判断している。notifier fetch の成否を知っているのは呼び出し元だけ
// なので、その判定だけそちらに置く。
func (h *Handler) filterValidNotifiers(viewerID string, rows []*notification.Notification, notifierByID map[string]*model.User) []*notification.Notification {
	// abuseReport はモデレーション用なので、read 時に権限を再確認する (#2868)。
	// 対象ユーザー ID が Extra に入り通報の存在自体が機微なので、権限を失った元
	// モデレーターが admin/abuse-user-reports の 403 を迂回して読み続けられない
	// ようにする。
	rows = h.filterModerationNotifications(viewerID, rows)

	muted := map[string]bool{}
	if h.mutingRepo != nil {
		if ids, err := h.mutingRepo.ListMuteeIDs(viewerID); err == nil {
			for _, id := range ids {
				muted[id] = true
			}
		}
	}
	mutedInstances := map[string]bool{}
	if h.userRepo != nil {
		if profile, err := h.userRepo.FindProfileByUserID(viewerID); err == nil && profile != nil && len(profile.MutedInstances) > 0 {
			var hosts []string
			if json.Unmarshal(profile.MutedInstances, &hosts) == nil {
				for _, host := range hosts {
					mutedInstances[host] = true
				}
			}
		}
	}
	if len(muted) == 0 && len(mutedInstances) == 0 && !h.anyNotifierSuspended(rows, notifierByID) {
		return rows
	}
	out := make([]*notification.Notification, 0, len(rows))
	for _, n := range rows {
		// **abuseReport は notifier のミュート等で落とさない (#2868)。**
		// notifier は通報者なので、モデレーターがミュートしている相手からの
		// 通報が通知欄に一切現れなくなる。凍結された利用者からの通報も同じ
		// (凍結の理由がその通報の対象であることもある)。
		if isStaffNotification(n.Type) {
			out = append(out, n)
			continue
		}
		if n.NotifierID != "" {
			if muted[n.NotifierID] {
				continue
			}
			if notifier, ok := notifierByID[n.NotifierID]; ok {
				if notifier.IsSuspended {
					continue
				}
				if notifier.Host != nil && mutedInstances[*notifier.Host] {
					continue
				}
			}
		}
		out = append(out, n)
	}
	return out
}

// anyNotifierSuspended reports whether at least one resolved notifier is
// suspended, so filterValidNotifiers can skip the rebuild loop entirely when
// there is nothing to drop (no mutes, no muted instances, no suspended).
func (h *Handler) anyNotifierSuspended(rows []*notification.Notification, notifierByID map[string]*model.User) bool {
	for _, n := range rows {
		if n.NotifierID == "" {
			continue
		}
		if u, ok := notifierByID[n.NotifierID]; ok && u.IsSuspended {
			return true
		}
	}
	return false
}

// maybeMarkAsRead honours the implicit markAsRead side effect: 本家
// i/notifications と互換で markAsRead 未指定または true なら通知一覧取得の
// 副作用で全通知を既読化し、main stream に readAllNotifications を publish
// する。これが無いとフロントエンドが /my/notifications を開いてもバッジ
// カウントが残り続ける (#420)。
func (h *Handler) maybeMarkAsRead(c echo.Context, user *model.User, req ListRequest) {
	if req.MarkAsRead != nil && !*req.MarkAsRead {
		return
	}
	// 暗黙既読は upstream i/notifications 同様 force を立てない。毎 fetch で
	// readAllNotifications を再送すると、保留中の unreadNotification を潰して
	// 逆にバッジが点かなくなる (#420 follow-up)。
	if err := h.svc.MarkAllAsRead(c.Request().Context(), user.ID, false); err != nil {
		// 既読化失敗は通知一覧の取得結果には影響しないので 200 のまま
		// 返し、ログだけ残す。
		slog.Warn("notifications: implicit mark-all-as-read failed",
			"userId", user.ID, "err", err)
	}
}

// MarkAllAsRead handles POST /api/notifications/mark-all-as-read.
//
// **明示的な既読操作なので force で呼ぶ** (upstream mark-all-as-read.ts と同じ、
// #2831)。バッジのカウンタはクライアント側にしか無く、readAllNotifications を
// 取りこぼすと読み取り位置だけが進んで guard に阻まれる。次の通知が来るまで
// バッジが残るので、その場で復帰させる手段としてこのボタンを force にする。
func (h *Handler) MarkAllAsRead(c echo.Context) error {
	user := middleware.GetUser(c)
	if err := h.svc.MarkAllAsRead(c.Request().Context(), user.ID, true); err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// Create handles POST /api/notifications/create.
//
// upstream Misskey TS notifications/create と同 semantics: 認証中の app
// (access token) が自分自身に type 'app' の任意通知を作る。body は必須、
// header / icon は任意。body / header / icon は Extra に格納し、entity の
// Extra spread で notification 出力に surface される (#1217)。
//
// header / icon は upstream 同様 `ps.header ?? token?.name ?? null` /
// `ps.icon ?? token?.iconUrl ?? null` で、リクエスト値が無ければ access token の
// name / iconUrl にフォールバックする (#1557)。token は raw token から
// 再解決する (native session token は access_token 行が無いので nil = fallback
// 無し)。'app' 通知の header/icon は schema 上 required + nullable なので、
// 値が無くても null で常に present にする。appAccessTokenId も記録する
// (内部メタデータ、レスポンスには出さない)。
func (h *Handler) Create(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Body   *string `json:"body"`
		Header *string `json:"header"`
		Icon   *string `json:"icon"`
	}
	// upstream paramDef は body を required にするのみ (ajv の required は
	// presence チェックなので空文字 "" も受理される)。よって不在 (nil) のみを
	// INVALID_PARAM とし、空文字は upstream 同様に通す。
	if err := c.Bind(&req); err != nil || req.Body == nil {
		return apierr.JSONInvalidParam(c)
	}
	if h.svc == nil || user == nil {
		return c.NoContent(http.StatusNoContent)
	}

	// リクエストの access token を再解決する (header/icon fallback + appAccessTokenId)。
	token := h.resolveRequestToken(c)

	// header = ps.header ?? token?.name ?? null。icon も同様。nil のままにすると
	// Extra spread で `null` として出力され、required + nullable な schema に揃う。
	var header any
	if req.Header != nil {
		header = *req.Header
	} else if token != nil && token.Name != nil {
		header = *token.Name
	}
	var icon any
	if req.Icon != nil {
		icon = *req.Icon
	} else if token != nil && token.IconURL != nil {
		icon = *token.IconURL
	}
	extra := map[string]any{"body": *req.Body, "header": header, "icon": icon}

	appAccessTokenID := ""
	if token != nil {
		appAccessTokenID = token.ID
	}

	// app 通知は notifier を持たない。NotifierID を空にすることで
	// service.Create の self-notification ガード (NotifierID == NotifieeID) も
	// 回避する。upstream 同様 fire-and-forget なので結果は 204 固定。
	if _, err := h.svc.Create(c.Request().Context(), notification.CreateInput{
		NotifieeID:       user.ID,
		Type:             notification.TypeApp,
		Extra:            extra,
		AppAccessTokenID: appAccessTokenID,
	}); err != nil {
		slog.Warn("notifications/create failed", "user", user.ID, "err", err)
	}
	return c.NoContent(http.StatusNoContent)
}

// resolveRequestToken re-resolves the request's raw token to its AccessToken
// row (#1557)。native session login token は access_token 行を持たないので nil
// (= header/icon fallback 無し)。repo 未配線 / token 無し / 解決失敗も nil。
func (h *Handler) resolveRequestToken(c echo.Context) *model.AccessToken {
	if h.accessTokenRepo == nil {
		return nil
	}
	raw := middleware.GetToken(c)
	if raw == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(raw))
	at, err := h.accessTokenRepo.FindByHashOrToken(hex.EncodeToString(sum[:]), raw)
	if err != nil {
		return nil
	}
	return at
}

// Flush handles POST /api/notifications/flush.
func (h *Handler) Flush(c echo.Context) error {
	user := middleware.GetUser(c)
	if h.svc != nil {
		if err := h.svc.Flush(c.Request().Context(), user.ID); err != nil {
			return apierr.JSONInternalError(c)
		}
	}
	return c.NoContent(http.StatusNoContent)
}

// TestNotification handles POST /api/notifications/test-notification.
// 本人へ 'test' 通知を送り、web push / streaming の疎通を確認できるようにする
// (upstream notifications/test-notification、#1559)。
func (h *Handler) TestNotification(c echo.Context) error {
	user := middleware.GetUser(c)
	if h.testNotifier != nil && user != nil {
		h.testNotifier.OnTest(user.ID)
	}
	return c.NoContent(http.StatusNoContent)
}

// filterModerationNotifications drops moderation-only notifications when the
// viewer no longer holds moderation privileges (#2868).
//
// **fail-closed。** checker 未配線なら abuseReport を落とす。対象ユーザー ID が
// Extra に入り、通報の存在自体が機微なので、判定できないまま出すより出さない
// ほうが安全側。administrator も通す (upstream の iAmModerator と同じ扱い)。
//
// **該当する通知が無ければ何もしない。** 権限判定は role 解決を伴うので、
// 通知一覧の大半を占める通常の通知のために毎回引かない。
func (h *Handler) filterModerationNotifications(viewerID string, rows []*notification.Notification) []*notification.Notification {
	hasModerationRow := false
	for _, n := range rows {
		if isStaffNotification(n.Type) {
			hasModerationRow = true
			break
		}
	}
	if !hasModerationRow {
		return rows
	}
	// **型ごとに判定が違う (#2987)。** 通報とアカウントの登録申請は
	// `RequireModerator` で審査するが、絵文字の申請は
	// `RequireRolePolicy(canManageCustomEmojis)` で、`HasRolePolicy` が短絡
	// するのは root と管理者だけ。モデレーターかどうかでは判定できない。
	//
	// 1 度ずつしか引かないよう、型ごとの結果を memo する。
	allowed := make(map[notification.Type]bool, 2)
	permitted := func(t notification.Type) bool {
		if v, ok := allowed[t]; ok {
			return v
		}
		v := h.staffNotificationVisible(viewerID, t)
		allowed[t] = v
		return v
	}
	out := make([]*notification.Notification, 0, len(rows))
	for _, n := range rows {
		if isStaffNotification(n.Type) && !permitted(n.Type) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// staffNotificationVisible reports whether viewerID may still see a staff
// notification of type t (#2868 / #2987).
//
// **未配線 / 未ログインでは見せない (fail-closed)。** 判定できないまま出すより
// 出さないほうが安全側 — 申請や通報の存在自体が機微で、Extra には対象の id が
// 入る。
func (h *Handler) staffNotificationVisible(viewerID string, t notification.Type) bool {
	if h.moderatorChecker == nil || viewerID == "" {
		return false
	}
	if t == notification.TypeEmojiApplicationReceived {
		// `HasRolePolicy` は root / 管理者を短絡し、そうでなければ policy を見る。
		return h.moderatorChecker.HasRolePolicy(viewerID, role.PolicyCanManageCustomEmojis)
	}
	// abuseReport / signupApplicationReceived。administrator も通す
	// (upstream の iAmModerator と同じ扱い)。
	return h.moderatorChecker.IsModerator(viewerID) || h.moderatorChecker.IsAdministrator(viewerID)
}

// isStaffNotification reports whether the type is delivered to the moderation
// team rather than to the user it is about (#2987).
//
// **registry を truth にする。** ここで型を並べ直すと、新しい運営向け通知を
// 足したときに「ミュートで消える」「権限を失っても見える」が静かに起きる。
func isStaffNotification(t notification.Type) bool {
	return notification.IsStaffType(t)
}
