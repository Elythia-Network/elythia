package chat

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	corechat "github.com/shiroha-a/mk/internal/core/chat"
	"github.com/shiroha-a/mk/internal/core/moderationlog"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// legacyNow is a tiny indirection so tests can override time in the handler's
// legacy (non-service) path if needed. 現状は time.Now を直接返す。
func legacyNow() time.Time { return time.Now() }

// chatPageParams holds the standard chat list pagination params (#1747)。
// 各 list endpoint の request struct に埋め込んで limit + 4 cursor を受ける。
type chatPageParams struct {
	Limit     int    `json:"limit"`
	SinceID   string `json:"sinceId"`
	UntilID   string `json:"untilId"`
	SinceDate *int64 `json:"sinceDate"`
	UntilDate *int64 `json:"untilDate"`
}

// cursor normalizes the 4 cursor params to (sinceID, untilID) via
// id.NormalizeCursor (sinceDate/untilDate を aidx prefix へ変換)。
//
// ok=false は列に入らないカーソルで、呼び出し側は 400 を返すこと (#3025)。
func (p chatPageParams) cursor() (string, string, bool) {
	return id.NormalizeCursor(p.SinceID, p.UntilID, p.SinceDate, p.UntilDate)
}

// clampedLimit resolves limit<=0 to def and caps at 100 (upstream maximum)。
func (p chatPageParams) clampedLimit(def int) int {
	if p.Limit <= 0 {
		return def
	}
	if p.Limit > 100 {
		return 100
	}
	return p.Limit
}

// Handler handles chat/* endpoints.
type Handler struct {
	repo  repository.ChatRepository
	idGen id.Generator
	svc   *corechat.Service
	// fileRepo / moderatorChecker は drive/files/attached-chat-messages 用
	// (#1218)。fileId の owner 検証 (moderator は任意 file 可) に使う。未配線
	// 時は当該 endpoint が空配列を返す。
	fileRepo         repository.DriveFileRepository
	moderatorChecker ModeratorChecker
	// userRepo は invitations/create のレスポンス (ChatRoomInvitation.user =
	// UserLite) で invitee user を解決するために使う (#... H-PR9a)。未配線時は
	// user field を省略する。
	userRepo repository.UserRepository
	// chatAvail は create-to-user で recipient の chatAvailability(write) を検査する
	// ため role policy を引く (#1796、upstream getChatAvailability(toUser).write)。
	// 未配線なら recipient policy check を skip。
	chatAvail middleware.ChatAvailabilityChecker
	// invitationNotifier は invitations/create で被招待ユーザーへ
	// 'chatRoomInvitationReceived' 通知を送る (#1559)。未配線なら通知しない。
	invitationNotifier ChatInvitationNotifier
	// modLog は moderator が他人の room を削除した際に監査ログを残すために使う
	// (#1541, upstream ChatService.deleteRoom の moderationLog.log)。未配線なら
	// 記録を skip する。
	modLog ModLogger
}

// maxRoomMembers mirrors upstream ChatService.MAX_ROOM_MEMBERS。join /
// invitations/create で room の membership 数がこの上限に達したら拒否する。
const maxRoomMembers = 50

// ModeratorChecker reports whether a user has moderator privileges. Implemented
// by core/role.Service. Used to widen attached-chat-messages from owner-only to
// any file for moderators (upstream `userId: isModerator ? undefined : me.id`).
type ModeratorChecker interface {
	IsModerator(userID string) bool
}

// SetDriveFileRepo wires the drive file repository for attached-chat-messages.
func (h *Handler) SetDriveFileRepo(r repository.DriveFileRepository) { h.fileRepo = r }

// SetModeratorChecker wires a moderator checker for attached-chat-messages.
func (h *Handler) SetModeratorChecker(m ModeratorChecker) { h.moderatorChecker = m }

// SetUserRepo wires the user repository for resolving invitee UserLite in the
// invitations/create response.
func (h *Handler) SetUserRepo(r repository.UserRepository) { h.userRepo = r }

// SetChatAvailabilityChecker wires the role-policy provider used to verify a
// recipient's chatAvailability(write) in create-to-user (#1796). nil skips it.
func (h *Handler) SetChatAvailabilityChecker(c middleware.ChatAvailabilityChecker) {
	h.chatAvail = c
}

// ChatInvitationNotifier records a 'chatRoomInvitationReceived' notification on
// the invitee's stream (#1559)。循環依存を避けるため interface で受け取る
// (実装は core/notification.Hook)。
type ChatInvitationNotifier interface {
	OnChatRoomInvitationReceived(inviteeID, inviterID, invitationID string)
}

// SetInvitationNotifier wires the notifier used by invitations/create to send a
// 'chatRoomInvitationReceived' notification (upstream ChatService の招待作成)。
func (h *Handler) SetInvitationNotifier(n ChatInvitationNotifier) { h.invitationNotifier = n }

// ModLogger records a moderation action. Satisfied by *core/moderationlog.Service.
// nil 配線時は記録を skip する (#1541)。
type ModLogger interface {
	Log(ctx context.Context, moderatorID string, t moderationlog.LogType, info map[string]any)
}

// SetModLog wires the moderation-log writer used when a moderator deletes
// another user's chat room (#1541)。
func (h *Handler) SetModLog(m ModLogger) { h.modLog = m }

// PackInvitationByID loads an invitation by ID (with room + invitee user) and
// packs it for the given viewer, returning false when it no longer exists. Used
// as the read-time lookup for chatRoomInvitationReceived notifications (#1559)。
func (h *Handler) PackInvitationByID(invitationID, viewerID string) (map[string]any, bool) {
	if h.repo == nil {
		return nil, false
	}
	inv, err := h.repo.FindInvitationByID(invitationID)
	if err != nil || inv == nil {
		return nil, false
	}
	// room は schema 上必須。load 失敗時は通知を drop する (= 不完全な
	// invitation を embed しない)。
	room, err := h.repo.FindRoomByID(inv.RoomID)
	if err != nil || room == nil {
		return nil, false
	}
	inv.Room = room
	// user (被招待者 UserLite) も schema 上必須。upstream packRoomInvitation は
	// user 解決失敗で throw → NotificationEntityService が null で drop する。
	// mk-go も解決不能なら通知を drop して shape 不完全な invitation を embed
	// しない (= 通知 packing 経路は degraded path を取らない)。
	if h.userRepo == nil {
		return nil, false
	}
	u, uerr := h.userRepo.FindByID(inv.UserID)
	if uerr != nil || u == nil {
		return nil, false
	}
	inv.User = u
	return h.packInvitationDetailed(inv, viewerID), true
}

// NewHandler creates a new chat handler.
func NewHandler(repo repository.ChatRepository, idGen id.Generator) *Handler {
	return &Handler{repo: repo, idGen: idGen}
}

// SetService wires a chat Service so that state-changing message endpoints
// (create/update/delete/read) route through it and publish streaming events.
// 未設定の場合は従来どおり repo 直操作にフォールバックする (既存テスト互換)。
func (h *Handler) SetService(svc *corechat.Service) {
	h.svc = svc
}

// mapChatErr maps a core/chat service error to an Echo response.
func (h *Handler) mapChatErr(c echo.Context, err error) error {
	switch {
	case errors.Is(err, corechat.ErrNotFound):
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_MESSAGE", "No such message.", "006d73c9-dada-5b5d-a0f5-00b01f70bc3c"))
	case errors.Is(err, corechat.ErrForbidden):
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
	case errors.Is(err, corechat.ErrInvalidTarget):
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "toUserId or toRoomId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	case errors.Is(err, corechat.ErrChatBlocked):
		// recipient が sender を block している (upstream YOU_HAVE_BEEN_BLOCKED)。
		return c.JSON(http.StatusBadRequest, apierr.Error("YOU_HAVE_BEEN_BLOCKED", "You cannot send a message because you have been blocked by this user.", "c15a5199-7422-4968-941a-2a462c478f7d"))
	case errors.Is(err, corechat.ErrChatScopeViolation):
		// CherryPick の `recipient is cannot chat (...)` 相当 (#692)。
		// upstream は ApiError を持たず単なる Error を投げるので mk-go 固有
		// code (RECIPIENT_CANNOT_CHAT) で返す。frontend の対応は別 issue。
		return c.JSON(http.StatusForbidden, apierr.Error("RECIPIENT_CANNOT_CHAT", "Recipient does not allow chat from you.", "5e1fa6e8-1d2f-49a3-9c20-7a09be7b4c43"))
	case errors.Is(err, corechat.ErrChatScopeUnconfigured):
		// followingRepo 未配線。production では起きてはいけないので 500 と
		// log で運用者に気付かせる (silent allow するより安全側)。
		slog.Error("chat: followingRepo missing; chatScope granular check failed closed", "err", err)
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
}

// packRoom returns the bare ChatRoom shape used by packRoomWithCreatedAt.
// createdAt は wrapper 側で ID 派生で付与するため、本関数は packRoom*
// helper の内部実装として packRoomWithCreatedAt 経由でのみ呼ぶこと。
// 直接 caller として使うと createdAt が抜けて upstream 互換 contract を
// 破る (= packedChatRoomSchema は createdAt 必須)。
//
// upstream Misskey TS の packedChatRoomSchema は `isArchived` を返さない。
// mk-go も互換のため field を出力しない (#851)。archived state は別経路で
// 取得する設計を踏襲する。
func packRoom(r *model.ChatRoom) map[string]any {
	result := map[string]any{
		"id": r.ID, "name": r.Name, "ownerId": r.OwnerID,
		"description": r.Description,
	}
	if r.Owner != nil {
		result["owner"] = packUser(r.Owner)
	}
	return result
}

// packMessage builds the ChatMessage response shape used across chat
// endpoints. nil pointer field (toUserId / toRoomId / text / fileId) は
// upstream の TypeORM 由来 undefined 挙動に揃え、JSON response から omit する
// (= map に key を入れない、#851)。
//
// 注: m.URI は remote chat message の AP canonical URI (federation 内部用途)
// だが、upstream の packedChatMessageSchema には `uri` field が無いため
// 外部 response には含めない。
//
// 注: m.Reads (chat_message.reads) は mk-go 独自の DB column で、upstream の
// packedChatMessageSchema には対応 field が無い。response から omit して
// upstream 互換に揃える (#855)。既読判定 (isRead) は別途 hint 経由で
// computed する設計に移行予定。
func packMessage(m *model.ChatMessage) map[string]any {
	result := map[string]any{
		"id":         m.ID,
		"fromUserId": m.FromUserID,
		"reactions":  packChatReactions(m.Reactions),
	}
	if m.ToUserID != nil {
		result["toUserId"] = *m.ToUserID
	}
	if m.ToRoomID != nil {
		result["toRoomId"] = *m.ToRoomID
	}
	if m.Text != nil {
		result["text"] = *m.Text
	}
	if m.FileID != nil {
		result["fileId"] = *m.FileID
	}
	if m.FromUser != nil {
		result["fromUser"] = packUser(m.FromUser)
	}
	return result
}

// packChatReactions converts the stored `"<userId>/<reaction>"` reaction
// strings (CherryPick chat reaction 形式) into the object array shape that
// misskey_dart の ChatMessage.fromJson expects (`reactions: [{reaction}]`)。
// raw 文字列配列のまま返すと `String is not Map<String, dynamic>` で落ちる
// (#1246)。user は upstream の packMessageLite と同じく省略 (schema 上 nullable)。
func packChatReactions(reactions []string) []map[string]any {
	out := make([]map[string]any, 0, len(reactions))
	for _, r := range reactions {
		reaction := r
		// 保存形式は "<userId>/<reaction>"。最初の "/" 以降を reaction とする。
		if i := strings.Index(r, "/"); i >= 0 {
			reaction = r[i+1:]
		}
		out = append(out, map[string]any{"reaction": reaction})
	}
	return out
}

// packMessageWithCreatedAt augments packMessage with createdAt parsed from the
// message ID and the eager-loaded file (if any). createdAt 欠落だと FE 側の
// MkTime が `Invalid Date` を表示し、一覧では NaN/NaN になる (#692)。
//
// file field は upstream packedChatMessageSchema の `file: optional/nullable
// (DriveFile)` に対応し、m.File が Preload で eager load されている場合のみ
// pack する (#855 PR-C)。chat repository の主要 list/find query で
// Preload("File") を行う前提。create-to-user 直後の response は CreateMessage
// が file を eager load しないため、file field 不在となる現状制約あり (= TS
// upstream とは差分、follow-up issue で create response も file を含めるよう
// 修正予定)。
func (h *Handler) packMessageWithCreatedAt(m *model.ChatMessage) map[string]any {
	result := packMessage(m)
	if h.idGen != nil {
		if t, err := h.idGen.ParseTime(m.ID); err == nil {
			result["createdAt"] = t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		if m.File != nil {
			result["file"] = entity.PackDriveFile(m.File, h.idGen)
		}
	}
	return result
}

// packRoomWithCreatedAt augments packRoom with createdAt parsed from the
// room ID. upstream の packedChatRoomSchema は createdAt を必須 field と
// するため、room を返す全 endpoint は本 method 経由で createdAt を含める
// (packMessageWithCreatedAt と同 pattern, #855)。
func (h *Handler) packRoomWithCreatedAt(r *model.ChatRoom) map[string]any {
	result := packRoom(r)
	if h.idGen != nil {
		if t, err := h.idGen.ParseTime(r.ID); err == nil {
			result["createdAt"] = t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
	return result
}

// packRoomDetailed augments packRoomWithCreatedAt with viewer (= meID) -
// specific optional field `isMuted` / `invitationExists`。upstream Misskey
// TS の ChatEntityService.packRoom と同じ semantics:
//   - meID が空、または me === room.OwnerID の場合は lookup を skip
//     (= owner は自身の room なので mute も invitation も無関係、両 false)
//   - me !== owner の場合のみ membership / invitation を lookup する
//
// upstream packedChatRoomSchema は両 field を `optional: true` で定義する
// が、mk-go は handler 経由 response で常に boolean を返す (= 利用側が
// undefined / boolean のどちらか分岐を考えずに済むよう一貫させる、#855)。
//
// caller が meID を渡せない場面 (例: federation 経由の eager pack や
// 内部用途) では packRoomWithCreatedAt をそのまま使う。
//
// **owner の特別扱いは維持する** (#2858 で検討した結果)。これは upstream
// ChatEntityService.ts:251-252 の `me.id !== room.ownerId` ガードの写しであって、
// transfer-ownership が membership 行を残していたことへの回避策ではない。
//
// **membership を見る形に統一してはいけない。** owner の行が残っている DB
// (000082 適用前) では `isMuted: true` を返すことになるが、フロントは owner に
// ミュートのスイッチを出さない (`room.info.vue` の `v-if="!isOwner"`) ため、
// 利用者からは**解除できない設定**として見える。owner never-muted で固定して
// おけば、行が残っていても API の見え方は壊れない。
func (h *Handler) packRoomDetailed(r *model.ChatRoom, meID string) map[string]any {
	result := h.packRoomWithCreatedAt(r)
	isMuted := false
	invitationExists := false
	if meID != "" && meID != r.OwnerID {
		if mem, err := h.repo.FindMembership(meID, r.ID); err == nil && mem != nil {
			isMuted = mem.IsMuted
		}
		if _, err := h.repo.FindInvitation(meID, r.ID); err == nil {
			invitationExists = true
		}
	}
	result["isMuted"] = isMuted
	result["invitationExists"] = invitationExists
	return result
}

// packInvitationDetailed builds the upstream ChatRoomInvitation response shape
// {id, createdAt, userId, user(UserLite), roomId, room(ChatRoom)} used by
// invitations/create (upstream ChatEntityService.packRoomInvitation)。
// inv.User / inv.Room が eager load 済みならそれを pack する (caller が事前に
// 解決して詰める)。meID は room pack の viewer (= inviter)。
func (h *Handler) packInvitationDetailed(inv *model.ChatRoomInvitation, meID string) map[string]any {
	result := map[string]any{
		"id": inv.ID, "userId": inv.UserID, "roomId": inv.RoomID,
	}
	if h.idGen != nil {
		if t, err := h.idGen.ParseTime(inv.ID); err == nil {
			result["createdAt"] = t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
	if inv.User != nil {
		result["user"] = packUser(inv.User)
	}
	if inv.Room != nil {
		result["room"] = h.packRoomDetailed(inv.Room, meID)
	}
	return result
}

// isRoomMember reports whether userID is the owner of, or a member of, the room
// (upstream ChatService.isRoomMember: room.ownerId===userId || membership exists)。
func (h *Handler) isRoomMember(room *model.ChatRoom, userID string) bool {
	if room.OwnerID == userID {
		return true
	}
	_, err := h.repo.FindMembership(userID, room.ID)
	return err == nil
}

// isModerator reports whether userID has moderator privileges (nil-safe).
func (h *Handler) isModerator(userID string) bool {
	return h.moderatorChecker != nil && h.moderatorChecker.IsModerator(userID)
}

// packMembershipDetailed builds the upstream ChatRoomMembership response shape
// {id, createdAt, userId, roomId, room(ChatRoom)} used by rooms/joining
// (upstream ChatEntityService.packRoomMembership with populateRoom:true,
// populateUser:false)。m.Room が eager load 済みなら room を pack する。
func (h *Handler) packMembershipDetailed(m *model.ChatRoomMembership, meID string) map[string]any {
	result := map[string]any{
		"id": m.ID, "userId": m.UserID, "roomId": m.RoomID,
	}
	if h.idGen != nil {
		if t, err := h.idGen.ParseTime(m.ID); err == nil {
			result["createdAt"] = t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
	if m.Room != nil {
		result["room"] = h.packRoomDetailed(m.Room, meID)
	}
	return result
}

// packMessageDetailed builds the upstream-compatible ChatMessage payload
// expected by MkChatHistories.vue: createdAt (ID から派生)、fromUser、
// toUser (DM の時)、toRoom (room の時)。upstream の
// ChatEntityService.packMessageDetailed 相当 (#692)。
//
// FE は createdAt を `new Date(m.createdAt).getTime()` で sort key に使う
// ので、欠損していると `NaN` になりリストが空になる。room メッセージは
// `'room' in m` で判定して "other" のかわりに room を表示する。
//
// meID は viewer (request 主体) の user ID。toRoom の `isMuted` /
// `invitationExists` を viewer 視点で計算する (#860 PR-D)。空文字列を渡した
// 場合は packRoomDetailed が lookup を skip して両 false 固定。
//
// 注: upstream の packMessageDetailed は `isRead` field を返さない (json-schema
// 上 `optional: true` だが実装側で emit していない、PR #861 で再調査済)。
// mk-go 側で先に追加すると upstream と逆方向の shape drift を生むため
// **意図的に未対応** とする。upstream が isRead を実装した場合に追従する。
func (h *Handler) packMessageDetailed(m *model.ChatMessage, meID string) map[string]any {
	result := h.packMessageWithCreatedAt(m)
	if m.ToUser != nil {
		result["toUser"] = packUser(m.ToUser)
	}
	if m.ToRoom != nil {
		result["toRoom"] = h.packRoomDetailed(m.ToRoom, meID)
		// upstream の `'room' in m` 判定に合わせて `room` alias も入れる。
		// 旧 chat 実装ではこの key で判定していた残存 client がある。
		result["room"] = h.packRoomDetailed(m.ToRoom, meID)
	}
	// room message の reactions は full/room schema で user (UserLite) を含む
	// (upstream packMessageForRoom、#1556)。1on1 lite は user を持たないので
	// base の {reaction} のまま。room 判定は ToRoomID 列で行う (ToRoom の eager
	// load 有無に依らない)。
	//
	// #2106 L18: upstream は messages/show・search・history で full schema
	// (packedChatMessageSchema) を使い、1on1 message でも reactions[].user を必須にする
	// (frontend XMessage.vue が record.user.id を参照する)。packRoomReactions は ToRoomID
	// 非依存に "userID/reaction" を解決できるので、room/1on1 双方で user を埋める。
	result["reactions"] = h.packRoomReactions(m)
	return result
}

// packRoomReactions builds room-message reactions as {user, reaction} by
// resolving the stored "<userId>/<reaction>" form to a UserLite (#1556)。
// full/room schema は reactions[].user を required non-null とするため、user を
// 解決できた reaction だけ emit する (= 削除済 user の reaction は drop して、
// user 欠落で client が crash しないよう保証)。userRepo 未配線 (test / early
// boot) は base の {reaction} に degrade する。
func (h *Handler) packRoomReactions(m *model.ChatMessage) []map[string]any {
	if h.userRepo == nil {
		return packChatReactions(m.Reactions)
	}
	type parsed struct{ userID, reaction string }
	items := make([]parsed, 0, len(m.Reactions))
	idSet := make(map[string]struct{}, len(m.Reactions))
	for _, r := range m.Reactions {
		uid, reaction := "", r
		if i := strings.Index(r, "/"); i >= 0 {
			uid, reaction = r[:i], r[i+1:]
		}
		items = append(items, parsed{uid, reaction})
		if uid != "" {
			idSet[uid] = struct{}{}
		}
	}
	userByID := make(map[string]*model.User, len(idSet))
	if len(idSet) > 0 {
		ids := make([]string, 0, len(idSet))
		for id := range idSet {
			ids = append(ids, id)
		}
		if users, err := h.userRepo.FindManyByIDs(ids); err == nil {
			for _, u := range users {
				userByID[u.ID] = u
			}
		}
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		u, ok := userByID[it.userID]
		if !ok {
			continue // user 未解決 (削除済等) の reaction は drop
		}
		out = append(out, map[string]any{"user": packUser(u), "reaction": it.reaction})
	}
	return out
}

// packUser produces the UserLite shape consumed by FE chat components.
// FE の MkAvatar / MkUserName が `avatarUrl` / `avatarBlurhash` を読むため、
// 含めないとアイコンが空のまま表示される (#692)。avatarUrl 未設定時は
// `entity.IdenticonURL` 経由で identicon URL に fallback する (TS upstream
// `getIdenticonUrl` 相当)。
func packUser(u *model.User) map[string]any {
	return map[string]any{
		"id":             u.ID,
		"username":       u.Username,
		"name":           u.Name,
		"host":           u.Host,
		"avatarUrl":      entity.IdenticonURL(u),
		"avatarBlurhash": u.AvatarBlurhash,
		"isBot":          u.IsBot,
		"isCat":          u.IsCat,
	}
}

// AttachedChatMessages handles POST /api/drive/files/attached-chat-messages.
// 指定 drive file を添付した chat message を id cursor で返す。並び順は
// repository の paginationOrder に従うので、sinceId 単独指定のページだけは
// 古い順になる (#2713)。
// upstream (read:drive) と同じく file は owner-scope で引く
// (moderator は任意 file 可)。
// 連合互換: res は ChatMessage の配列 (packMessageDetailed)。
func (h *Handler) AttachedChatMessages(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		FileID    string `json:"fileId"`
		Limit     int    `json:"limit"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
	}
	if err := c.Bind(&req); err != nil || req.FileID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "fileId is required.", "6b7f9d2c-1e4a-4b8c-9d3e-7a5f0c2b1e88"))
	}
	// upstream paramDef は limit を minimum:1 / maximum:100 / default:10 で
	// clamp する。> 100 は default に落とさず max へ clamp して挙動を揃える。
	if req.Limit <= 0 {
		req.Limit = 10
	} else if req.Limit > 100 {
		req.Limit = 100
	}
	if h.fileRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	file, err := h.fileRepo.FindByID(req.FileID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil || file == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "Some files are not found.", "485ce26d-f5d2-4313-9783-e689d131eafb"))
	}
	// owner-scope: moderator 以外は自分の file のみ参照可 (他人の private chat
	// の添付を覗けないようにする)。
	isMod := h.moderatorChecker != nil && h.moderatorChecker.IsModerator(user.ID)
	if !isMod && (file.UserID == nil || *file.UserID != user.ID) {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "Some files are not found.", "485ce26d-f5d2-4313-9783-e689d131eafb"))
	}
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	msgs, err := h.repo.ListMessagesByFileID(req.FileID, untilID, sinceID, req.Limit)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, h.packMessageDetailed(m, user.ID))
	}
	return c.JSON(http.StatusOK, out)
}

// --- Rooms ---

// RoomsCreate handles POST /api/chat/rooms/create.
func (h *Handler) RoomsCreate(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := c.Bind(&req); err != nil || req.Name == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "name is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	// upstream paramDef は name maxLength:256 / description maxLength:1024
	// (chat/rooms/create.ts)。超過は schema validation で弾かれるため INVALID_PARAM。
	if utf8.RuneCountInString(req.Name) > 256 || utf8.RuneCountInString(req.Description) > 1024 {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "name or description is too long.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	room := &model.ChatRoom{
		ID: h.idGen.Generate(time.Now()), Name: req.Name,
		OwnerID: user.ID, Description: req.Description,
	}
	if err := h.repo.CreateRoom(room); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// golden ChatRoom は owner (UserLite) を必須とする。upstream は ownerId から
	// owner を解決して必ず含めるため、作成者 (= 認証 user) を attach してから
	// pack する (#1284)。未 attach だと packRoom が owner を omit していた。
	room.Owner = user
	return c.JSON(http.StatusOK, h.packRoomDetailed(room, user.ID))
}

// RoomsShow handles POST /api/chat/rooms/show.
//
// upstream Misskey TS 2026.5.4 (`hasPermissionToViewRoomInfo`) と互換するため、
// owner / member / 招待受信者 / moderator 以外は noSuchRoom (404) を返す
// (#1164 Phase C)。旧 mk-go は room id を知っていれば誰でも room メタ情報を
// 取得できる drop-in regression 状態だったため本 gate で塞ぐ。
func (h *Handler) RoomsShow(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		RoomID string `json:"roomId"`
	}
	if err := c.Bind(&req); err != nil || req.RoomID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roomId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	room, err := h.repo.FindRoomByID(req.RoomID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		// upstream chat/rooms/show.ts:30 の noSuchRoom id に揃える (#1771)。
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "857ae02f-8759-4d20-9adb-6e95fffe4fd7"))
	}
	if h.svc != nil {
		ok, err := h.svc.HasPermissionToViewRoomInfo(user.ID, room)
		if err != nil || !ok {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "857ae02f-8759-4d20-9adb-6e95fffe4fd7"))
		}
	}
	return c.JSON(http.StatusOK, h.packRoomDetailed(room, user.ID))
}

// RoomsUpdate handles POST /api/chat/rooms/update.
func (h *Handler) RoomsUpdate(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		RoomID      string `json:"roomId"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := c.Bind(&req); err != nil || req.RoomID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roomId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	// upstream paramDef は name maxLength:256 / description maxLength:1024
	// (chat/rooms/update.ts)。
	if utf8.RuneCountInString(req.Name) > 256 || utf8.RuneCountInString(req.Description) > 1024 {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "name or description is too long.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	room, err := h.repo.FindRoomByID(req.RoomID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil || room.OwnerID != user.ID {
		// upstream chat/rooms/update.ts:30 の noSuchRoom id に揃える (#1771)。
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "fcdb0f92-bda6-47f9-bd05-343e0e020932"))
	}
	if req.Name != "" {
		room.Name = req.Name
	}
	if req.Description != "" {
		room.Description = req.Description
	}
	if err := h.repo.UpdateRoom(room); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	return c.JSON(http.StatusOK, h.packRoomDetailed(room, user.ID))
}

// RoomsDelete handles POST /api/chat/rooms/delete.
func (h *Handler) RoomsDelete(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		RoomID string `json:"roomId"`
	}
	if err := c.Bind(&req); err != nil || req.RoomID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roomId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	room, err := h.repo.FindRoomByID(req.RoomID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "d4e3753d-97bf-4a19-ab8e-21080fbc0f4b"))
	}
	// upstream hasPermissionToDeleteRoom は owner OR moderator を許可する。
	// 旧 mk-go は owner-only だったため moderator が他人の room を削除できなかった。
	isOwner := room.OwnerID == user.ID
	isMod := h.isModerator(user.ID)
	if !isOwner && !isMod {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "d4e3753d-97bf-4a19-ab8e-21080fbc0f4b"))
	}
	_ = h.repo.DeleteRoom(req.RoomID)
	// upstream deleteRoom は deleter が moderator なら deleteChatRoom を監査ログに残す
	// (owner かどうかに依らず moderator なら記録する)。
	if isMod && h.modLog != nil {
		h.modLog.Log(c.Request().Context(), user.ID, moderationlog.LogDeleteChatRoom, map[string]any{
			"roomId": room.ID,
			"room": map[string]any{
				"id": room.ID, "name": room.Name, "ownerId": room.OwnerID, "description": room.Description,
			},
		})
	}
	return c.NoContent(http.StatusNoContent)
}

// RoomsOwned handles POST /api/chat/rooms/owned.
func (h *Handler) RoomsOwned(c echo.Context) error {
	user := middleware.GetUser(c)
	var req chatPageParams
	_ = c.Bind(&req)
	sinceID, untilID, cursorOK := req.cursor()
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	rooms, _ := h.repo.ListRoomsByOwner(user.ID, sinceID, untilID, req.clampedLimit(30))
	result := make([]map[string]any, len(rooms))
	for i, r := range rooms {
		// 全 room の owner = user.ID (= 一覧の filter 条件)。packRoomDetailed は
		// 内部で me === owner check で lookup を skip し isMuted/invitationExists
		// を false に固定する。
		result[i] = h.packRoomDetailed(r, user.ID)
	}
	return c.JSON(http.StatusOK, result)
}

// RoomsJoined handles POST /api/chat/rooms/joined.
func (h *Handler) RoomsJoined(c echo.Context) error {
	user := middleware.GetUser(c)
	var req chatPageParams
	_ = c.Bind(&req)
	sinceID, untilID, cursorOK := req.cursor()
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	rooms, _ := h.repo.ListJoinedRooms(user.ID, sinceID, untilID, req.clampedLimit(30))
	result := make([]map[string]any, len(rooms))
	for i, r := range rooms {
		// joined 一覧は本人が member の room なので room ごとに membership
		// lookup が走る (= isMuted を返すため)。upstream は packRooms hint で
		// bulk fetch するが、本実装は per-room lookup (= 件数 N の clean fetch)。
		// hint cache 化は別 issue で扱う。
		result[i] = h.packRoomDetailed(r, user.ID)
	}
	return c.JSON(http.StatusOK, result)
}

// RoomsLeave handles POST /api/chat/rooms/leave.
func (h *Handler) RoomsLeave(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		RoomID string `json:"roomId"`
	}
	if err := c.Bind(&req); err != nil || req.RoomID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roomId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	// service 経由で membership 削除 + remote member/owner への Remove 連合配信
	// (#1364)。service 未配線時 (federation disabled の旧構成) は repo 直叩きで
	// membership 削除のみ。
	if h.svc != nil {
		_ = h.svc.LeaveRoom(c.Request().Context(), user.ID, req.RoomID)
	} else {
		_ = h.repo.DeleteMembership(user.ID, req.RoomID)
	}
	return c.NoContent(http.StatusNoContent)
}

// RoomsMute handles POST /api/chat/rooms/mute.
func (h *Handler) RoomsMute(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		RoomID string `json:"roomId"`
		// upstream mute.ts:28-34 は mute:boolean を required で受け、isMuted=mute に
		// する (mute:false で unmute)。以前は param を無視して常に mute していた (#1771)。
		Mute bool `json:"mute"`
	}
	if err := c.Bind(&req); err != nil || req.RoomID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roomId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	m, err := h.repo.FindMembership(user.ID, req.RoomID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		// upstream mute.ts は membership 不在で noSuchRoom を返す (以前は 204)。
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "c2cde4eb-8d0f-42f1-8f2f-c4d6bfc8e5df"))
	}
	m.IsMuted = req.Mute
	_ = h.repo.UpdateMembership(m)
	return c.NoContent(http.StatusNoContent)
}

// RoomsUnmute handles POST /api/chat/rooms/unmute.
func (h *Handler) RoomsUnmute(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		RoomID string `json:"roomId"`
	}
	if err := c.Bind(&req); err != nil || req.RoomID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roomId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	m, err := h.repo.FindMembership(user.ID, req.RoomID)
	if err != nil {
		return c.NoContent(http.StatusNoContent)
	}
	m.IsMuted = false
	_ = h.repo.UpdateMembership(m)
	return c.NoContent(http.StatusNoContent)
}

// RoomsTransferOwnership handles POST /api/chat/rooms/transfer-ownership.
func (h *Handler) RoomsTransferOwnership(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		RoomID string `json:"roomId"`
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.RoomID == "" || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roomId and userId are required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	room, err := h.repo.FindRoomByID(req.RoomID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil || room.OwnerID != user.ID {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "6ab4d7df-5043-57b9-bd5d-ff9908288473"))
	}
	if req.UserID == user.ID {
		// 自分への譲渡は何も変えない。repository 側にも同じガードがあるが、
		// ここで返すことで下の room-full 検査も含めて丸ごと省ける。
		return c.NoContent(http.StatusNoContent)
	}
	// **譲渡先は room の既存メンバーに限る。**
	//
	// 非メンバーへ渡せると、旧 owner に membership 行を作るこの実装では
	// **同意していない相手の room に投稿し続けられる**。相手は owner なので
	// fan-out で never-muted 扱いになり (core/chat/service.go)、membership 行を
	// 持たないため `rooms/mute` も 400 になる。つまり止める手段が room ごと
	// 消すことしかない一方通行の経路になる。譲渡前の実装では旧 owner が
	// 締め出されるため、この形は成立しなかった。
	//
	// メンバーに限ると副次的に 2 つ消える。membership 行数が動かないので
	// room-full の上限がずれず、行がある以上 user も実在するので NO_SUCH_USER の
	// 検査も要らない (`chat_room."ownerId"` の FK 違反による 500 も起きない)。
	//
	// upstream Misskey にこの endpoint は無いので parity 上の制約は無く、
	// fork frontend と misskey-js のどちらにも caller が無い。
	if _, err := h.repo.FindMembership(req.UserID, room.ID); err != nil {
		if !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return apierr.JSONInternalError(c)
		}
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_MEMBER", "Not a member.", "05af3d8f-a5d9-4282-8cf0-4fcc5f4e1734"))
	}
	// **譲渡先はローカル利用者に限る (#2994)。** リモート利用者は
	// `AddMemberViaAP` でこちらの room のメンバーになれるので、この endpoint は
	// **ローカルの room の owner をリモート利用者にできてしまう**。そうなると
	// 「owner がリモート = 取り込んだ copy」という前提が崩れ、
	// `chat_room.host` / `uri` の backfill がこちらの room に偽のリモート URI を
	// 刻む。以後その room 宛の Accept / Reject / group message は
	// `resolveRoomByAPURI` に弾かれて恒久的に drop され、配送側は存在しない
	// リモート URI を名乗る。
	//
	// 譲る意味も無い — リモート利用者はこちらの API を叩けないので、owner に
	// なっても room を管理できない。upstream にこの endpoint は無いので parity 上の
	// 制約も無い。
	if h.userRepo == nil {
		// 未配線では譲渡先がローカルか確かめられない。素通しにしない。
		return apierr.JSONInternalError(c)
	}
	target, terr := h.userRepo.FindByID(req.UserID)
	if terr != nil {
		if !repository.IsNotFound(terr) {
			return apierr.JSONInternalError(c)
		}
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_MEMBER", "Not a member.", "05af3d8f-a5d9-4282-8cf0-4fcc5f4e1734"))
	}
	if !target.IsLocal() {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_MEMBER", "Not a member.", "05af3d8f-a5d9-4282-8cf0-4fcc5f4e1734"))
	}
	// **owner の membership 行を入れ替える。** ownerId を書き換えるだけだと
	// 新 owner は解除できない mute を抱え、旧 owner は room から締め出される
	// (repository.TransferRoomOwnership のコメントに詳しい)。
	if err := h.repo.TransferRoomOwnership(room.ID, user.ID, req.UserID, h.idGen.Generate(time.Now())); err != nil {
		if repository.IsNotFound(err) {
			// 同時に別の譲渡が確定した場合。もう自分の room ではないので、
			// 上の owner 検査と同じ応答に揃える。
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "6ab4d7df-5043-57b9-bd5d-ff9908288473"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// --- Messages ---

// MessagesCreate handles POST /api/chat/messages/create.
// service が wire されていれば streaming 配信も同時に行う。
func (h *Handler) MessagesCreate(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Text     *string `json:"text"`
		ToUserID *string `json:"toUserId"`
		ToRoomID *string `json:"toRoomId"`
		FileID   *string `json:"fileId"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	if h.svc == nil {
		// 未wire時のフォールバック (テスト互換)
		return h.messagesCreateLegacy(c, user, req.Text, req.ToUserID, req.ToRoomID, req.FileID)
	}
	// upstream は create-to-user / create-to-room の 2 endpoint に分かれ、検証順と
	// golden error id が異なる。mk-go は単一 handler で分岐するため、先に宛先種別を
	// 確定し以降の error id を切り替える (#1541)。
	toRoom := req.ToRoomID != nil && *req.ToRoomID != ""
	toUser := req.ToUserID != nil && *req.ToUserID != ""
	if !toRoom && !toUser {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "toUserId or toRoomId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	// upstream create-to-room.ts:77-91 は file/content 検証より先に room 存在を
	// 確認し、無ければ noSuchRoom を返す。mk-go は単一 handler なので room path で
	// 先に room を解決する (以前は service の ErrNotFound が NO_SUCH_MESSAGE に
	// マップされ、検証順も逆だった、#1771)。
	if toRoom && h.repo != nil {
		if _, err := h.repo.FindRoomByID(*req.ToRoomID); err != nil {
			if !repository.IsNotFound(err) {
				// **DB 障害を not-found に丸めない** (#2792)。
				return apierr.JSONInternalError(c)
			}
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "8098520d-2da5-4e8f-8ee1-df78b55a4ec6"))
		}
	}
	// create-to-user / create-to-room で CONTENT_REQUIRED / NO_SUCH_FILE の id が
	// 異なる (golden_error_ids.json)。
	contentRequiredID := "25587321-b0e6-449c-9239-f8925092942c"
	noSuchFileID := "4372b8e2-185d-4146-8749-2f68864a3e5f"
	if toRoom {
		contentRequiredID = "340517b7-6d04-42c0-bac1-37ee804e3594"
		noSuchFileID = "b6accbd3-1d7b-4d9f-bdb7-eb185bac06db"
	}

	text := ""
	if req.Text != nil {
		text = *req.Text
	}
	// upstream paramDef は text maxLength:2000 (create-to-user.ts/create-to-room.ts)。
	// 超過は schema validation 段階で弾かれるため INVALID_PARAM 相当で reject する。
	if utf8.RuneCountInString(text) > 2000 {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "text is too long.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	fileID := ""
	if req.FileID != nil {
		fileID = *req.FileID
	}
	// NO_SUCH_FILE: upstream は driveFilesRepository.findOneBy({id:fileId,userId:me})
	// が null なら noSuchFile を投げる。所有者でない / 存在しない fileId を弾く。
	// fileRepo 未配線 (legacy test) は素通しする。
	if fileID != "" && h.fileRepo != nil {
		f, ferr := h.fileRepo.FindByID(fileID)
		if ferr != nil && !repository.IsNotFound(ferr) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return apierr.JSONInternalError(c)
		}
		if ferr != nil || f.UserID == nil || *f.UserID != user.ID {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "No such file.", noSuchFileID))
		}
	}
	// CONTENT_REQUIRED: upstream は text==null && file==null を弾く (text=="" でも
	// null でなければ許容する挙動に合わせ、req.Text が nil の場合のみ無 text とみなす)。
	if req.Text == nil && fileID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("CONTENT_REQUIRED", "Content required. You need to set text or fileId.", contentRequiredID))
	}

	var (
		msg *model.ChatMessage
		err error
	)
	if toRoom {
		msg, err = h.svc.CreateMessageToRoom(c.Request().Context(), user.ID, *req.ToRoomID, text, fileID)
	} else {
		// RECIPIENT_IS_YOURSELF: 自分宛 DM は upstream で recipientIsYourself。
		if *req.ToUserID == user.ID {
			return c.JSON(http.StatusBadRequest, apierr.Error("RECIPIENT_IS_YOURSELF", "Cannot send a message to yourself.", "17e2ba79-e22a-4cbc-bf91-d327643f4a7e"))
		}
		// upstream create-to-user.ts:112-115 は getUser(toUserId) で recipient を
		// 解決し、不在なら NO_SUCH_USER を返す (以前は不在 user にも message を作って
		// いた、#1771)。user-timeline と同じ status/code/id に揃える。
		if h.userRepo != nil {
			if _, ferr := h.userRepo.FindByID(*req.ToUserID); ferr != nil {
				if !repository.IsNotFound(ferr) {
					// **DB 障害を not-found に丸めない** (#2792)。
					return apierr.JSONInternalError(c)
				}
				return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "11795c64-40ea-4198-b06e-3c873ed9039d"))
			}
		}
		// upstream ChatService.createMessageToUser:177-179 は recipient の
		// chatAvailability(write) が false なら送信を拒否する (#1796)。recipient が
		// chat 不可なので 403 ACCESS_DENIED を返す。
		// #2106 L19 (意図的 divergence): upstream は generic Error で 500 INTERNAL_ERROR に
		// なるが、mk-go は semantics 上適切な 403 ACCESS_DENIED を維持する (worse な 500 回避)。
		if h.chatAvail != nil {
			policy, _ := h.chatAvail.GetUserPolicies(*req.ToUserID)["chatAvailability"].(string)
			if !middleware.ChatAvailabilityAllows(policy, "write") {
				return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "The recipient cannot receive chat messages.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
			}
		}
		msg, err = h.svc.CreateMessageToUser(c.Request().Context(), user.ID, *req.ToUserID, text, fileID)
	}
	if err != nil {
		return h.mapChatErr(c, err)
	}
	return c.JSON(http.StatusOK, h.packMessageDetailed(withSender(h.reloadIfFilePending(msg), user), user.ID))
}

// messagesCreateLegacy preserves pre-Phase-9.8 behaviour for callers that
// construct a Handler without wiring the service. 既存の chat handler テスト
// はこちらを通る。
func (h *Handler) messagesCreateLegacy(c echo.Context, user *model.User, text, toUserID, toRoomID, fileID *string) error {
	msg := &model.ChatMessage{
		ID: h.idGen.Generate(legacyNow()), FromUserID: user.ID,
		ToUserID: toUserID, ToRoomID: toRoomID,
		Text: text, FileID: fileID,
	}
	if err := h.repo.CreateMessage(msg); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	return c.JSON(http.StatusOK, h.packMessageDetailed(withSender(h.reloadIfFilePending(msg), user), user.ID))
}

// withSender attaches the authenticated sender as FromUser when the row has
// no eager-loaded relation, so create responses include the golden-required
// `fromUser` (UserLite). 作成直後の message は CreateMessage が FromUser を
// Preload しないため fromUser が欠落していた (#1288。ChatRoom owner #1285 と
// 同種の embed 漏れ)。reload で DB から FromUser が載った場合は上書きしない。
func withSender(m *model.ChatMessage, sender *model.User) *model.ChatMessage {
	if m != nil && m.FromUser == nil {
		m.FromUser = sender
	}
	return m
}

// reloadIfFilePending re-fetches the message via FindMessageByID (which
// preloads File) when the message has a fileId but no eager-loaded File
// yet. CreateMessage 経路は File を Preload しないので、create response の
// `file` field を upstream packMessageDetailed と同 shape にするための
// post-fetch helper (#860 PR-D)。reload に失敗した場合は元の msg をその
// まま返す (= file 不在で fall-through)。
func (h *Handler) reloadIfFilePending(msg *model.ChatMessage) *model.ChatMessage {
	if msg == nil || msg.FileID == nil || msg.File != nil {
		return msg
	}
	if reloaded, err := h.repo.FindMessageByID(msg.ID); err == nil {
		return reloaded
	}
	return msg
}

// MessagesShow handles POST /api/chat/messages/show.
func (h *Handler) MessagesShow(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		MessageID string `json:"messageId"`
	}
	if err := c.Bind(&req); err != nil || req.MessageID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "messageId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	msg, err := h.repo.FindMessageByID(req.MessageID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_MESSAGE", "No such message.", "006d73c9-dada-5b5d-a0f5-00b01f70bc3c"))
	}
	// **当事者か moderator でなければ返さない。** upstream show.ts と同じ判定
	// (`message.fromUserId !== me.id && message.toUserId !== me.id && !isModerator`)。
	// room 宛のメッセージは `toUserId` が null なので、投稿者本人以外はここで
	// 落ちる — room の発言を読む経路は messages/room-timeline のほうで、
	// そちらは member かどうかを見ている。
	//
	// 応答は not-found と同じにする。区別すると messageId を総当たりして
	// 「その id が存在するか」を引き出せる。
	if msg.FromUserID != user.ID && (msg.ToUserID == nil || *msg.ToUserID != user.ID) && !h.isModerator(user.ID) {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_MESSAGE", "No such message.", "006d73c9-dada-5b5d-a0f5-00b01f70bc3c"))
	}
	return c.JSON(http.StatusOK, h.packMessageDetailed(msg, user.ID))
}

// MessagesUpdate handles POST /api/chat/messages/update.
func (h *Handler) MessagesUpdate(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		MessageID string  `json:"messageId"`
		Text      *string `json:"text"`
	}
	if err := c.Bind(&req); err != nil || req.MessageID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "messageId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	if h.svc == nil {
		// legacy fallback
		msg, err := h.repo.FindMessageByID(req.MessageID)
		if err != nil && !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return apierr.JSONInternalError(c)
		}
		if err != nil || msg.FromUserID != user.ID {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_MESSAGE", "No such message.", "006d73c9-dada-5b5d-a0f5-00b01f70bc3c"))
		}
		if req.Text != nil {
			msg.Text = req.Text
			_ = h.repo.UpdateMessage(msg)
		}
		return c.NoContent(http.StatusNoContent)
	}
	text := ""
	if req.Text != nil {
		text = *req.Text
	}
	if _, err := h.svc.UpdateMessage(c.Request().Context(), user.ID, req.MessageID, text); err != nil {
		return h.mapChatErr(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// MessagesDelete handles POST /api/chat/messages/delete.
func (h *Handler) MessagesDelete(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		MessageID string `json:"messageId"`
	}
	if err := c.Bind(&req); err != nil || req.MessageID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "messageId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	if h.svc == nil {
		msg, err := h.repo.FindMessageByID(req.MessageID)
		if err != nil && !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return apierr.JSONInternalError(c)
		}
		if err != nil || msg.FromUserID != user.ID {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_MESSAGE", "No such message.", "006d73c9-dada-5b5d-a0f5-00b01f70bc3c"))
		}
		_ = h.repo.DeleteMessage(req.MessageID)
		return c.NoContent(http.StatusNoContent)
	}
	if err := h.svc.DeleteMessage(c.Request().Context(), user.ID, req.MessageID); err != nil {
		return h.mapChatErr(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// MessagesRead handles POST /api/chat/messages/read.
func (h *Handler) MessagesRead(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		MessageID string `json:"messageId"`
	}
	if err := c.Bind(&req); err != nil || req.MessageID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "messageId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	if h.svc != nil {
		if err := h.svc.MarkReadByMessageID(c.Request().Context(), user.ID, req.MessageID); err != nil {
			return h.mapChatErr(c, err)
		}
		return c.NoContent(http.StatusNoContent)
	}
	_ = h.repo.MarkRead(user.ID, req.MessageID)
	return c.NoContent(http.StatusNoContent)
}

// Messages handles POST /api/chat/messages (list).
func (h *Handler) Messages(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		RoomID string `json:"roomId"`
		UserID string `json:"userId"`
		Limit  int    `json:"limit"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	var msgs []*model.ChatMessage
	if req.RoomID != "" {
		// room 発言の閲覧は room-timeline と同じ permission gate を適用する
		// (member OR moderator)。これが無いと room-timeline を塞いでも本経路から
		// 任意 room の発言を読めてしまう。
		room, err := h.repo.FindRoomByID(req.RoomID)
		if err != nil && !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return apierr.JSONInternalError(c)
		}
		if err != nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "c4d9f88c-9270-4632-b032-6ed8cee36f7f"))
		}
		if !h.isRoomMember(room, user.ID) && !h.isModerator(user.ID) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "c4d9f88c-9270-4632-b032-6ed8cee36f7f"))
		}
		msgs, _ = h.repo.ListMessagesByRoom(req.RoomID, "", "", req.Limit)
	} else if req.UserID != "" {
		msgs, _ = h.repo.ListMessagesByUser(user.ID, req.UserID, "", "", req.Limit)
	}
	result := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		result[i] = h.packMessageDetailed(m, user.ID)
	}
	return c.JSON(http.StatusOK, result)
}

// MessagesSearch handles POST /api/chat/messages/search.
func (h *Handler) MessagesSearch(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Query  string `json:"query"`
		Limit  int    `json:"limit"`
		UserID string `json:"userId"`
		RoomID string `json:"roomId"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	// roomId 指定時は upstream search.ts と同じく room が存在し、かつ自分が member
	// (owner 含む) でなければ NO_SUCH_ROOM を返す (存在しない room も非 member も
	// 同じ code)。
	if req.RoomID != "" {
		room, err := h.repo.FindRoomByID(req.RoomID)
		if err != nil && !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return apierr.JSONInternalError(c)
		}
		if err != nil || !h.isRoomMember(room, user.ID) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "460b3669-81b0-4dc9-a997-44442141bf83"))
		}
	}
	msgs, _ := h.repo.SearchMessages(user.ID, req.Query, req.Limit, req.UserID, req.RoomID)
	result := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		result[i] = h.packMessageDetailed(m, user.ID)
	}
	return c.JSON(http.StatusOK, result)
}

// ReactionsCreate handles POST /api/chat/messages/reactions/create and
// POST /api/chat/messages/react. Reaction format: "userId/emoji".
func (h *Handler) ReactionsCreate(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		MessageID string `json:"messageId"`
		Reaction  string `json:"reaction"`
	}
	if err := c.Bind(&req); err != nil || req.MessageID == "" || req.Reaction == "" {
		return apierr.JSONInvalidParam(c)
	}
	// service 経由で AddReaction + react stream event を発火する (#1549)。
	if err := h.svc.React(c.Request().Context(), req.MessageID, user, req.Reaction); err != nil {
		return reactionErr(c, err, chatReactNoSuchMessageID)
	}
	return c.NoContent(http.StatusNoContent)
}

// ReactionsDelete handles POST /api/chat/messages/reactions/delete and
// POST /api/chat/messages/unreact.
func (h *Handler) ReactionsDelete(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		MessageID string `json:"messageId"`
		Reaction  string `json:"reaction"`
	}
	if err := c.Bind(&req); err != nil || req.MessageID == "" || req.Reaction == "" {
		return apierr.JSONInvalidParam(c)
	}
	// service 経由で RemoveReaction + unreact stream event を発火する (#1549)。
	if err := h.svc.Unreact(c.Request().Context(), req.MessageID, user, req.Reaction); err != nil {
		return reactionErr(c, err, chatUnreactNoSuchMessageID)
	}
	return c.NoContent(http.StatusNoContent)
}

// Error ids of NO_SUCH_MESSAGE on chat/messages/react and chat/messages/unreact
// (upstream 2026.10.0). The reactions/create and reactions/delete aliases share
// the same handlers and therefore the same ids.
const (
	chatReactNoSuchMessageID   = "9b5839b9-0ba0-4351-8c35-37082093d200"
	chatUnreactNoSuchMessageID = "c39ea42f-e3ca-428a-ad57-390e0a711595"
)

// reactionErr maps a React / Unreact service error to a response. Access
// errors (missing message, own message, not a participant) all become the
// same 400 NO_SUCH_MESSAGE; every other error stays a generic 500 as upstream.
func reactionErr(c echo.Context, err error, noSuchMessageID string) error {
	// 存在しない場合と参加していない場合を同じ応答にし、メッセージの
	// 存在をエラーから判別できないようにする (upstream と同じ)。
	if errors.Is(err, corechat.ErrMessageAccess) {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_MESSAGE", "No such message.", noSuchMessageID))
	}
	return apierr.JSONInternalError(c)
}

// --- Invitations ---

// InvitationsCreate handles POST /api/chat/rooms/invitations/create.
func (h *Handler) InvitationsCreate(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		RoomID string `json:"roomId"`
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.RoomID == "" || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roomId and userId are required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	// owner だけが招待を作成・連合できる。これが無いと任意の認証ユーザーが
	// owner の鍵で署名された Invite を remote へなりすまし送信できてしまう
	// (#1201 review)。upstream create.ts は findMyRoomById==null で先に noSuchRoom
	// (404) を投げるため、yourself より前に owner-check を行う (error 種別を揃える)。
	room, err := h.repo.FindRoomByID(req.RoomID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil || room.OwnerID != user.ID {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "916f9507-49ba-4e90-b57f-1fd4deaa47a5"))
	}
	// upstream createRoomInvitation は room 確認後に各 soft-reject を plain Error
	// (= generic 500) で投げる: yourself / already-member / already-invited /
	// room-full。owner が inviter のため invitee==owner のケースは yourself に
	// 収束する (upstream isRoomMember(ownerId===inviteeId) 相当は到達不能)。
	if user.ID == req.UserID {
		return apierr.JSONInternalError(c) // 'yourself'
	}
	if _, err := h.repo.FindMembership(req.UserID, req.RoomID); err == nil {
		return apierr.JSONInternalError(c) // 'already member'
	}
	if _, err := h.repo.FindInvitation(req.UserID, req.RoomID); err == nil {
		return apierr.JSONInternalError(c) // 'already invited'
	}
	// room-full 判定。count 取得失敗は fail-closed で 500 にする (upstream countBy
	// 失敗は request を abort する)。owner は membership 行を持たないため count は
	// 非 owner member 数 = TS countBy({roomId}) と一致 (上限 owner + 50)。
	members, err := h.repo.ListMembersByRoom(req.RoomID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	if len(members) >= maxRoomMembers {
		return apierr.JSONInternalError(c) // 'room is full'
	}
	// upstream packRoomInvitation は invitee を必須 UserLite として pack し、解決
	// 失敗時は throw (= 500)。invitee を永続化前に解決し、存在しない user への
	// invitation 行を作らない。userRepo 未配線 (test / early boot) は degraded path
	// として user field 省略で続行する (production は router で配線済み)。
	var inviteeUser *model.User
	if h.userRepo != nil {
		u, uerr := h.userRepo.FindByID(req.UserID)
		if uerr != nil {
			return apierr.JSONInternalError(c)
		}
		inviteeUser = u
	}
	inv := &model.ChatRoomInvitation{
		ID: h.idGen.Generate(time.Now()), UserID: req.UserID, RoomID: req.RoomID,
	}
	if err := h.repo.CreateInvitation(inv); err != nil {
		return apierr.JSONInternalError(c)
	}
	// 被招待ユーザー (local) へ chatRoomInvitationReceived 通知を送る (#1559)。
	// notifier は招待者 (= room owner = request user)。リモート invitee は
	// notifyLocalUser の host guard で除外される。
	if h.invitationNotifier != nil {
		h.invitationNotifier.OnChatRoomInvitationReceived(req.UserID, user.ID, inv.ID)
	}
	// invitee が remote user なら Invite activity を配送する (CherryPick group
	// chat federation, #1201)。local invitee なら service 側で no-op。
	if h.svc != nil {
		h.svc.FederateInvitation(req.RoomID, req.UserID)
	}
	// upstream res は packed ChatRoomInvitation を返す (旧 mk-go は 204)。
	inv.Room = room
	inv.User = inviteeUser
	return c.JSON(http.StatusOK, h.packInvitationDetailed(inv, user.ID))
}

// InvitationsDelete handles POST /api/chat/rooms/invitations/delete.
// ルームオーナーが自分の送った招待を取り消す。
//
// 招待された側は reject / ignore を使う。こちらは連合の Reject を送らないので、
// 被招待者に開放すると remote room の招待を黙って消せてしまう。
func (h *Handler) InvitationsDelete(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		InvitationID string `json:"invitationId"`
	}
	if err := c.Bind(&req); err != nil || req.InvitationID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "invitationId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	// **招待の所在と権限の有無を応答で区別しない。** 区別すると invitationId を
	// 総当たりして「その id が存在するか」だけを引き出せる。
	inv, err := h.repo.FindInvitationByID(req.InvitationID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err == nil {
		var room *model.ChatRoom
		room, err = h.repo.FindRoomByID(inv.RoomID)
		if err != nil && !repository.IsNotFound(err) {
			return apierr.JSONInternalError(c)
		}
		if err == nil && room.OwnerID != user.ID {
			err = repository.ErrNotFound
		}
	}
	if err != nil {
		return c.JSON(http.StatusNotFound, apierr.Error("NO_SUCH_INVITATION", "No such invitation.", "9db2b511-acf0-46f0-9856-ce089fa63b23"))
	}
	_ = h.repo.DeleteInvitation(req.InvitationID)
	return c.NoContent(http.StatusNoContent)
}

// InvitationsAccept handles POST /api/chat/rooms/invitations/accept.
func (h *Handler) InvitationsAccept(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		InvitationID string `json:"invitationId"`
		RoomID       string `json:"roomId"`
	}
	if err := c.Bind(&req); err != nil || req.RoomID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roomId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	// pending invitation がある場合のみ accept できる。これが無いと招待無しに
	// membership が作られ、任意 room owner へ未承諾 Accept が飛ぶ (#1206 review)。
	// 招待なしの参加は別途 rooms/join を使う。
	inv, err := h.repo.FindInvitation(user.ID, req.RoomID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusNotFound, apierr.Error("NO_SUCH_INVITATION", "No such invitation.", "8c8f38f0-3b7e-4f5e-9c6d-2e3f4a5b6c7d"))
	}
	// **owner の招待は行を作らずに消費する** (#2858)。owner は暗黙のメンバーで
	// membership 行を持たない。譲渡で owner 宛の招待が残っていた場合にここが
	// 行を作ると、その不変条件が壊れる (rooms/joined に自分の room が出る、
	// room-full の数が 1 ずれる、解除できない mute が生まれる)。
	room, rerr := h.repo.FindRoomByID(req.RoomID)
	if rerr != nil && !repository.IsNotFound(rerr) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if rerr == nil && room.OwnerID == user.ID {
		_ = h.repo.DeleteInvitation(inv.ID)
		return c.NoContent(http.StatusNoContent)
	}
	// メンバーシップを作成して招待を消費する。
	m := &model.ChatRoomMembership{
		ID: h.idGen.Generate(time.Now()), UserID: user.ID, RoomID: req.RoomID,
	}
	_ = h.repo.CreateMembership(m)
	_ = h.repo.DeleteInvitation(inv.ID)
	// remote room の招待なら Accept を owner へ配送する (#1205)。local room は
	// service 側で no-op。
	if h.svc != nil {
		h.svc.FederateInvitationResponse(req.RoomID, user.ID, true)
	}
	return c.NoContent(http.StatusNoContent)
}

// InvitationsReject handles POST /api/chat/rooms/invitations/reject.
func (h *Handler) InvitationsReject(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		RoomID string `json:"roomId"`
	}
	if err := c.Bind(&req); err != nil || req.RoomID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roomId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	// pending invitation がある場合のみ reject できる (任意 room owner への
	// 未承諾 Reject 送信を防ぐ、#1206 review)。
	inv, err := h.repo.FindInvitation(user.ID, req.RoomID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusNotFound, apierr.Error("NO_SUCH_INVITATION", "No such invitation.", "8c8f38f0-3b7e-4f5e-9c6d-2e3f4a5b6c7d"))
	}
	_ = h.repo.DeleteInvitation(inv.ID)
	// remote room の招待なら Reject を owner へ配送する (#1205)。
	if h.svc != nil {
		h.svc.FederateInvitationResponse(req.RoomID, user.ID, false)
	}
	return c.NoContent(http.StatusNoContent)
}

// MembersBan handles POST /api/chat/rooms/members/ban.
// ルームオーナーのみが他メンバーを退出させられる。
func (h *Handler) MembersBan(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		RoomID string `json:"roomId"`
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.RoomID == "" || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roomId and userId are required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	// **owner だけが実行できる。** 隣の members/update-membership と同じ形。
	// room の所在と権限の有無を応答で区別しない (どちらも NO_SUCH_ROOM)。
	room, err := h.repo.FindRoomByID(req.RoomID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil || room.OwnerID != user.ID {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "1ebd1536-1c92-4e6b-9ee0-bf4d14998bdb"))
	}
	_ = h.repo.DeleteMembership(req.UserID, req.RoomID)
	return c.NoContent(http.StatusNoContent)
}

// MembersUpdateMembership handles POST /api/chat/rooms/members/update-membership.
// ルームオーナーのみが他メンバーの設定を変更できる。
func (h *Handler) MembersUpdateMembership(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		RoomID  string `json:"roomId"`
		UserID  string `json:"userId"`
		IsMuted *bool  `json:"isMuted"`
	}
	if err := c.Bind(&req); err != nil || req.RoomID == "" || req.UserID == "" {
		return apierr.JSONInvalidParam(c)
	}
	room, err := h.repo.FindRoomByID(req.RoomID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil || room.OwnerID != user.ID {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "b3926861-29ef-4df6-98b5-a7c640ad2b5a"))
	}
	mem, err := h.repo.FindMembership(req.UserID, req.RoomID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusNotFound, apierr.Error("NO_SUCH_MEMBER", "Not a member.", "e3285385-56ee-4909-9ef4-4a0e6e2e614a"))
	}
	if req.IsMuted != nil {
		mem.IsMuted = *req.IsMuted
	}
	if err := h.repo.UpdateMembership(mem); err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// --- TS-compatible aliases ---

// MessagesCreateToUser handles POST /api/chat/messages/create-to-user.
func (h *Handler) MessagesCreateToUser(c echo.Context) error {
	return h.MessagesCreate(c)
}

// MessagesCreateToRoom handles POST /api/chat/messages/create-to-room.
func (h *Handler) MessagesCreateToRoom(c echo.Context) error {
	return h.MessagesCreate(c)
}

// UserTimeline handles POST /api/chat/messages/user-timeline.
//
// upstream の readUserChatMessage 相当: チャット画面を開いた瞬間に「相手 →
// 自分」の DM をすべて既読としてマークする。FE は新着メッセージにしか
// 'read' イベントを送らないので、initial load 時点の既読化はサーバ側で
// やらないと unread badge がリロードのたびに復活する (#692)。
func (h *Handler) UserTimeline(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		UserID string `json:"userId"`
		chatPageParams
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// upstream user-timeline.ts は getUser(userId) で相手を解決し、不在なら
	// NO_SUCH_USER を返す。userRepo 未配線 (legacy test) は検証 skip。
	if h.userRepo != nil {
		if _, err := h.userRepo.FindByID(req.UserID); err != nil {
			if !repository.IsNotFound(err) {
				// **DB 障害を not-found に丸めない** (#2792)。
				return apierr.JSONInternalError(c)
			}
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "11795c64-40ea-4198-b06e-3c873ed9039d"))
		}
	}
	sinceID, untilID, cursorOK := req.cursor()
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	msgs, err := h.repo.ListMessagesByUser(user.ID, req.UserID, sinceID, untilID, req.clampedLimit(10))
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	// 既読マーク (best effort; 失敗しても timeline 表示は継続)。
	if err := h.repo.MarkAllReadFromUser(user.ID, req.UserID); err != nil {
		slog.Warn("chat: MarkAllReadFromUser failed", "readerId", user.ID, "fromUserId", req.UserID, "err", err)
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, h.packMessageDetailed(m, user.ID))
	}
	return c.JSON(http.StatusOK, out)
}

// RoomTimeline handles POST /api/chat/messages/room-timeline.
//
// upstream の readRoomChatMessage 相当 (#692)。
func (h *Handler) RoomTimeline(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		RoomID string `json:"roomId"`
		chatPageParams
	}
	if err := c.Bind(&req); err != nil || req.RoomID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// upstream room-timeline.ts: room 不在 / 閲覧権限なし (member でも moderator でも
	// ない) は NO_SUCH_ROOM。これが無いと任意の認証ユーザーが任意 room の発言を
	// 読めてしまう (security)。owner は isRoomMember で member 扱い。
	room, err := h.repo.FindRoomByID(req.RoomID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "c4d9f88c-9270-4632-b032-6ed8cee36f7f"))
	}
	if !h.isRoomMember(room, user.ID) && !h.isModerator(user.ID) {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "c4d9f88c-9270-4632-b032-6ed8cee36f7f"))
	}
	sinceID, untilID, cursorOK := req.cursor()
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	msgs, err := h.repo.ListMessagesByRoom(req.RoomID, sinceID, untilID, req.clampedLimit(10))
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	if err := h.repo.MarkAllReadInRoom(user.ID, req.RoomID); err != nil {
		slog.Warn("chat: MarkAllReadInRoom failed", "readerId", user.ID, "roomId", req.RoomID, "err", err)
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, h.packMessageDetailed(m, user.ID))
	}
	return c.JSON(http.StatusOK, out)
}

// ReadAll handles POST /api/chat/read-all.
func (h *Handler) ReadAll(c echo.Context) error {
	user := middleware.GetUser(c)
	if err := h.repo.MarkAllRead(user.ID); err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// InvitationsIgnore handles POST /api/chat/rooms/invitations/ignore.
func (h *Handler) InvitationsIgnore(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		RoomID string `json:"roomId"`
	}
	if err := c.Bind(&req); err != nil || req.RoomID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if inv, err := h.repo.FindInvitation(user.ID, req.RoomID); err == nil {
		inv.Ignored = true
		_ = h.repo.UpdateInvitation(inv)
	}
	return c.NoContent(http.StatusNoContent)
}

// InvitationsInbox handles POST /api/chat/rooms/invitations/inbox.
func (h *Handler) InvitationsInbox(c echo.Context) error {
	user := middleware.GetUser(c)
	var req chatPageParams
	_ = c.Bind(&req)
	sinceID, untilID, cursorOK := req.cursor()
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	rows, err := h.repo.ListInvitationsByUser(user.ID, false, sinceID, untilID, req.clampedLimit(30))
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	out := make([]map[string]any, 0, len(rows))
	for _, inv := range rows {
		entry := map[string]any{"id": inv.ID, "roomId": inv.RoomID, "userId": inv.UserID}
		if inv.Room != nil {
			// inbox は本人が invitee (= owner ではない)。lookup で
			// invitationExists = true、isMuted = false (まだ非 member) になる。
			entry["room"] = h.packRoomDetailed(inv.Room, user.ID)
		}
		out = append(out, entry)
	}
	return c.JSON(http.StatusOK, out)
}

// InvitationsOutbox handles POST /api/chat/rooms/invitations/outbox.
func (h *Handler) InvitationsOutbox(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		RoomID string `json:"roomId"`
		chatPageParams
	}
	if err := c.Bind(&req); err != nil || req.RoomID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// 自分が所有するルームの招待一覧を返す
	room, err := h.repo.FindRoomByID(req.RoomID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil || room.OwnerID != user.ID {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "a3c6b309-9717-4316-ae94-a69b53437237"))
	}
	sinceID, untilID, cursorOK := req.cursor()
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	rows, err := h.repo.ListInvitationsByRoom(req.RoomID, sinceID, untilID, req.clampedLimit(30))
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	// upstream packRoomInvitation は {id, createdAt, roomId, room, userId, user} を
	// 返す。旧 mk-go は createdAt と room を欠いていた (#1541)。ListInvitationsByRoom は
	// User を Preload 済み、room は上で取得済みなので各 inv に詰めて
	// packInvitationDetailed (createdAt + room + user) で pack する。
	out := make([]map[string]any, 0, len(rows))
	for _, inv := range rows {
		inv.Room = room
		out = append(out, h.packInvitationDetailed(inv, user.ID))
	}
	return c.JSON(http.StatusOK, out)
}

// RoomsJoin handles POST /api/chat/rooms/join.
func (h *Handler) RoomsJoin(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		RoomID string `json:"roomId"`
	}
	if err := c.Bind(&req); err != nil || req.RoomID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// upstream joinToRoom は pending invitation を必須とする (findOneByOrFail)。
	// これが無いと任意の認証ユーザーが任意の room に勝手に join できる (security)。
	// invitation 不在は upstream の findOneByOrFail と同じく generic 500。
	inv, err := h.repo.FindInvitation(user.ID, req.RoomID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	// **owner の招待は行を作らずに消費する** (#2858、InvitationsAccept と同じ)。
	// owner は暗黙のメンバーなので membership 行を持たない。
	joinRoom, rerr := h.repo.FindRoomByID(req.RoomID)
	if rerr != nil && !repository.IsNotFound(rerr) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if rerr == nil && joinRoom.OwnerID == user.ID {
		_ = h.repo.DeleteInvitation(inv.ID)
		return c.NoContent(http.StatusNoContent)
	}
	// UNIQUE 制約違反を避けるため既存メンバーは冪等に扱う。新規 join 時のみ
	// MAX_ROOM_MEMBERS=50 を超える room を拒否する (upstream 'room is full')。
	if _, merr := h.repo.FindMembership(user.ID, req.RoomID); merr != nil {
		// count 取得失敗は fail-closed で 500 (upstream countBy 失敗は abort)。
		members, lerr := h.repo.ListMembersByRoom(req.RoomID)
		if lerr != nil {
			return apierr.JSONInternalError(c)
		}
		if len(members) >= maxRoomMembers {
			return apierr.JSONInternalError(c)
		}
		mem := &model.ChatRoomMembership{
			ID: h.idGen.Generate(time.Now()), UserID: user.ID, RoomID: req.RoomID,
		}
		if cerr := h.repo.CreateMembership(mem); cerr != nil {
			return apierr.JSONInternalError(c)
		}
	}
	// upstream は join 成功時に invitation を消費する。
	_ = h.repo.DeleteInvitation(inv.ID)
	return c.NoContent(http.StatusNoContent)
}

// RoomsJoining handles POST /api/chat/rooms/joining.
//
// upstream joining.ts は ChatRoomMembership[] (populateRoom:true,
// populateUser:false) を返す。旧 mk-go は RoomsJoined (= ChatRoom[]) の alias
// だったが、shape が異なるため membership を返すよう独立実装する。
func (h *Handler) RoomsJoining(c echo.Context) error {
	user := middleware.GetUser(c)
	var req chatPageParams
	_ = c.Bind(&req)
	sinceID, untilID, cursorOK := req.cursor()
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	rows, err := h.repo.ListMembershipsByUser(user.ID, sinceID, untilID, req.clampedLimit(30))
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		out = append(out, h.packMembershipDetailed(m, user.ID))
	}
	return c.JSON(http.StatusOK, out)
}

// RoomsMembers handles POST /api/chat/rooms/members.
func (h *Handler) RoomsMembers(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		RoomID string `json:"roomId"`
		chatPageParams
	}
	if err := c.Bind(&req); err != nil || req.RoomID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// upstream members.ts: room 不在 / 非 member は NO_SUCH_ROOM (member-only、
	// room-timeline と違い moderator は許可しない)。member でない第三者に member
	// 一覧を晒さない。
	room, err := h.repo.FindRoomByID(req.RoomID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "7b9fe84c-eafc-4d21-bf89-485458ed2c18"))
	}
	if !h.isRoomMember(room, user.ID) {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROOM", "No such room.", "7b9fe84c-eafc-4d21-bf89-485458ed2c18"))
	}
	sinceID, untilID, cursorOK := req.cursor()
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	members, err := h.repo.ListMembersByRoomPaged(req.RoomID, sinceID, untilID, req.clampedLimit(30))
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	out := make([]map[string]any, 0, len(members))
	for _, m := range members {
		// upstream packRoomMembership は {id, createdAt, userId, roomId, user}。
		// createdAt は required (id=aidx 由来)、isMuted は schema に無いので出さない (#1771)。
		entry := map[string]any{"id": m.ID, "userId": m.UserID, "roomId": m.RoomID}
		if h.idGen != nil {
			if t, err := h.idGen.ParseTime(m.ID); err == nil {
				entry["createdAt"] = t.UTC().Format("2006-01-02T15:04:05.000Z")
			}
		}
		if m.User != nil {
			entry["user"] = packUser(m.User)
		}
		out = append(out, entry)
	}
	return c.JSON(http.StatusOK, out)
}

// --- Other ---

// History handles POST /api/chat/history.
//
// upstream `chat/history` は `room: bool` parameter を取り、true の時は
// chat ルーム内の最新メッセージ、false (デフォルト) の時は 1-on-1 DM の
// 最新メッセージを返す。FE の MkChatHistories.vue は両方を並行に呼んで
// マージするため、room パラメータを正しくハンドリングしないと chat home
// で何も描画されない (#692)。
func (h *Handler) History(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Limit int  `json:"limit"`
		Room  bool `json:"room"`
	}
	_ = c.Bind(&req)
	if req.Limit <= 0 {
		req.Limit = 10
	}
	if req.Limit > 100 {
		req.Limit = 100
	}
	var (
		msgs []*model.ChatMessage
		err  error
	)
	if req.Room {
		msgs, err = h.repo.ListRoomHistory(user.ID, req.Limit)
	} else {
		msgs, err = h.repo.ListUserHistory(user.ID, req.Limit)
	}
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	// upstream history.ts は packMessagesDetailed の後に会話ごとの read 状態を
	// 計算して各 message に isRead を載せる (#1749)。packMessageDetailed 自体は
	// isRead を出さない (#861) ため、history endpoint でのみ上乗せする。
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		packed := h.packMessageDetailed(m, user.ID)
		packed["isRead"] = h.historyItemIsRead(user.ID, m, req.Room)
		out = append(out, packed)
	}
	return c.JSON(http.StatusOK, out)
}

// historyItemIsRead reports whether the conversation represented by a history
// item has no unread messages for meID. Mirrors upstream getRoomReadStateMap /
// getUserReadStateMap (mk-go は reads 配列から会話単位で導出する)。クエリ失敗時は
// upstream の `?? false` と同じく未読 (false) 扱いにする。
func (h *Handler) historyItemIsRead(meID string, m *model.ChatMessage, room bool) bool {
	if room {
		if m.ToRoomID == nil {
			return true
		}
		unread, err := h.repo.HasUnreadInRoom(meID, *m.ToRoomID)
		return err == nil && !unread
	}
	// 1-on-1: 相手 = 自分が sender なら toUser、そうでなければ fromUser。
	otherID := m.FromUserID
	if otherID == meID && m.ToUserID != nil {
		otherID = *m.ToUserID
	}
	unread, err := h.repo.HasUnreadFromUser(meID, otherID)
	return err == nil && !unread
}

// UnreadCount handles POST /api/chat/unread-count.
func (h *Handler) UnreadCount(c echo.Context) error {
	user := middleware.GetUser(c)
	count, _ := h.repo.CountUnread(user.ID)
	return c.JSON(http.StatusOK, map[string]any{"count": count})
}

// HasDriveFileRepo reports whether the drive file repository was wired.
//
// 未配線だと NO_SUCH_FILE の所有者チェックが素通しになり、他人の drive ファイルを
// チャットに添付できる (IDOR)。起動時検査に使う (#2708)。
func (h *Handler) HasDriveFileRepo() bool { return h.fileRepo != nil }
