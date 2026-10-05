package repository

import (
	"strings"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// ChatRepository handles chat-related persistence.
type ChatRepository interface {
	// Room operations
	CreateRoom(room *model.ChatRoom) error
	FindRoomByID(id string) (*model.ChatRoom, error)
	// FindRoomByURI looks up a room by its canonical AP URI. リモート room の
	// 身元は URI であって room id ではない (#2994) ので、AP 経路の引き当ては
	// すべてこちらを通す。
	FindRoomByURI(uri string) (*model.ChatRoom, error)
	UpdateRoom(room *model.ChatRoom) error
	DeleteRoom(id string) error
	ListRoomsByOwner(ownerID, sinceID, untilID string, limit int) ([]*model.ChatRoom, error)
	ListJoinedRooms(userID, sinceID, untilID string, limit int) ([]*model.ChatRoom, error)
	// TransferRoomOwnership hands a room over to newOwnerID while keeping the
	// "owner has no membership row" invariant intact. See the implementation
	// for why both sides have to move together.
	TransferRoomOwnership(roomID, oldOwnerID, newOwnerID, oldOwnerMembershipID string) error

	// Message operations
	CreateMessage(msg *model.ChatMessage) error
	FindMessageByID(id string) (*model.ChatMessage, error)
	FindMessageByURI(uri string) (*model.ChatMessage, error)
	UpdateMessage(msg *model.ChatMessage) error
	DeleteMessage(id string) error
	ListMessagesByRoom(roomID, sinceID, untilID string, limit int) ([]*model.ChatMessage, error)
	ListMessagesByUser(userID, otherUserID, sinceID, untilID string, limit int) ([]*model.ChatMessage, error)
	// ListMessagesByFileID returns chat messages that attached the given drive
	// file with id-cursor pagination. Ordering follows paginationOrder, so a
	// sinceID-only page comes back oldest-first. Used by
	// drive/files/attached-chat-messages.
	ListMessagesByFileID(fileID, untilID, sinceID string, limit int) ([]*model.ChatMessage, error)
	SearchMessages(meID, query string, limit int, userID, roomID string) ([]*model.ChatMessage, error)

	// Membership operations
	CreateMembership(m *model.ChatRoomMembership) error
	FindMembership(userID, roomID string) (*model.ChatRoomMembership, error)
	UpdateMembership(m *model.ChatRoomMembership) error
	DeleteMembership(userID, roomID string) error
	// ListMembersByRoom returns ALL memberships of a room. Used by federation
	// fan-out (deliver to every member) and the room-full count, so it must not
	// be paginated. The paginated room/members endpoint uses ListMembersByRoomPaged.
	ListMembersByRoom(roomID string) ([]*model.ChatRoomMembership, error)
	// ListMembersByRoomPaged is the id-cursor paginated variant for the
	// chat/rooms/members endpoint (#1747)。
	ListMembersByRoomPaged(roomID, sinceID, untilID string, limit int) ([]*model.ChatRoomMembership, error)
	ListMembershipsByUser(userID, sinceID, untilID string, limit int) ([]*model.ChatRoomMembership, error)

	// Invitation operations
	CreateInvitation(inv *model.ChatRoomInvitation) error
	DeleteInvitation(id string) error
	FindInvitation(userID, roomID string) (*model.ChatRoomInvitation, error)
	// FindInvitationByID looks up an invitation by its primary key. Used to
	// pack chatRoomInvitationReceived notifications at read time (#1559)。
	FindInvitationByID(id string) (*model.ChatRoomInvitation, error)
	UpdateInvitation(inv *model.ChatRoomInvitation) error

	// Unread count
	CountUnread(userID string) (int64, error)

	// HasUnreadFromUser reports whether readerID has any unread 1-on-1 message
	// from otherID. Used by chat/history to populate isRead (#1749)。
	HasUnreadFromUser(readerID, otherID string) (bool, error)
	// HasUnreadInRoom reports whether readerID has any unread message in roomID
	// (excluding their own). Used by chat/history to populate isRead (#1749)。
	HasUnreadInRoom(readerID, roomID string) (bool, error)

	// Mark read
	MarkRead(userID, messageID string) error

	// Invitation listing
	ListInvitationsByUser(userID string, ignored bool, sinceID, untilID string, limit int) ([]*model.ChatRoomInvitation, error)
	ListInvitationsByRoom(roomID, sinceID, untilID string, limit int) ([]*model.ChatRoomInvitation, error)

	// UpdateDeliveryStatus sets the AP delivery flags on a chat message.
	UpdateDeliveryStatus(messageID string, delivering, failed bool) error

	// ListHistory returns the most recent message per conversation (DM + room)
	// involving userID, ordered by recency.
	ListHistory(userID string, limit int) ([]*model.ChatMessage, error)

	// ListUserHistory is the DM-only variant: 1-on-1 conversations only,
	// rooms excluded. Mirrors upstream ChatService.userHistory (#692)。
	ListUserHistory(userID string, limit int) ([]*model.ChatMessage, error)

	// ListRoomHistory is the room-only variant: per-room latest message,
	// for rooms the user owns or has membership in. Mirrors upstream
	// ChatService.roomHistory (#692)。
	ListRoomHistory(userID string, limit int) ([]*model.ChatMessage, error)

	// Mark all as read
	MarkAllRead(userID string) error

	// MarkAllReadFromUser bulk-marks all DM messages from `fromUserID` to
	// `readerID` as read (appends readerID to `reads`). Used by user-timeline
	// to clear the unread badge on chat open (#692, upstream
	// ChatService.readUserChatMessage 相当)。
	MarkAllReadFromUser(readerID, fromUserID string) error

	// MarkAllReadInRoom bulk-marks all room messages in `roomID` (excluding
	// reader's own) as read. Used by room-timeline (#692, upstream
	// ChatService.readRoomChatMessage 相当)。
	MarkAllReadInRoom(readerID, roomID string) error

	// Reactions
	AddReaction(messageID, reaction string) error
	// RemoveReaction removes one reaction and reports whether a row changed.
	//
	// **消えたかどうかを返すのが要点 (#3037)。** 呼び出し側はこれを見て
	// stream への publish を決める。無条件に publish すると、**会話の参加者で
	// ない利用者**が任意の messageId に対して `unreact` を投げるだけで、
	// その部屋 / DM のストリームにイベントを注入できる。
	RemoveReaction(messageID, reaction string) (bool, error)
}

type chatRepository struct {
	db *gorm.DB
}

// NewChatRepository creates a new ChatRepository.
func NewChatRepository(db *gorm.DB) ChatRepository {
	return &chatRepository{db: db}
}

// maxChatPageLimit caps the page size of the chat list endpoints (upstream
// paramDef maximum:100)。
const maxChatPageLimit = 100

// applyIDCursor appends id-cursor (sinceID / untilID) + paginationOrder
// ordering + limit to a chat list query, mirroring upstream
// makePaginationQuery: a sinceID-only page is ASC, everything else is DESC.
// idCol must be the
// (optionally table-qualified) primary-key column to paginate on. limit<=0 falls
// back to defaultLimit and is clamped to maxChatPageLimit (#1747)。
func applyIDCursor(q *gorm.DB, idCol, sinceID, untilID string, limit, defaultLimit int) *gorm.DB {
	if untilID != "" {
		q = q.Where(idCol+" < ?", untilID)
	}
	if sinceID != "" {
		q = q.Where(idCol+" > ?", sinceID)
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxChatPageLimit {
		limit = maxChatPageLimit
	}
	// upstream ChatService.ts で makePaginationQuery を使うのは 7 メソッド
	// (:392 / :416 / :701 / :711 / :721 / :791 / :972) で、いずれも後段で
	// order を上書きしない (#2713)。cursor を取らない userHistory /
	// roomHistory / searchMessages (:438 / :491 / :853) だけは直接 id DESC で、
	// mk-go でも対応する ListHistory / ListUserHistory / ListRoomHistory /
	// SearchMessages はこの helper を通していない。
	//
	// この helper を通る repository メソッドは 8 つ (grep applyIDCursor の
	// 呼び出し site 数)。うち ListJoinedRooms だけは upstream に対応 endpoint が
	// 無い mk-go 独自のもので、近縁の joining.ts が membership.id で cursor を
	// 切るのに対しこちらは room.id で切る。追従先が無いので同じ規則に揃えた。
	return q.Order(paginationOrder(sinceID, untilID, idCol)).Limit(limit)
}

func (r *chatRepository) CreateRoom(room *model.ChatRoom) error {
	return r.db.Create(room).Error
}

func (r *chatRepository) FindRoomByID(id string) (*model.ChatRoom, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var room model.ChatRoom
	if err := r.db.Preload("Owner").Where(`"id" = ?`, id).First(&room).Error; err != nil {
		return nil, err
	}
	return &room, nil
}

// FindRoomByURI looks up a remote room copy by its canonical AP URI.
//
// **ローカル room は `uri` が NULL。** SQL の `=` は NULL に一致しないので、
// 空文字で引いてもローカル room は返らない (自前のガードは置かない)。
func (r *chatRepository) FindRoomByURI(uri string) (*model.ChatRoom, error) {
	if !storable(uri) {
		return nil, ErrNotFound
	}
	var room model.ChatRoom
	if err := r.db.Preload("Owner").Where(`"uri" = ?`, uri).First(&room).Error; err != nil {
		return nil, err
	}
	return &room, nil
}

func (r *chatRepository) UpdateRoom(room *model.ChatRoom) error {
	return r.db.Save(room).Error
}

// TransferRoomOwnership hands roomID over from oldOwnerID to newOwnerID in one
// transaction, keeping the "owner has no membership row" invariant.
//
// upstream ChatService.ts が room のメンバー集合を
// `memberships.concat({userId: room.ownerId, isMuted: false})` で作り、
// 「ownerはmembershipレコードを作らないため」とコメントしているとおり、owner は
// membership 行を持たない。mk-go の読み取り側もこれに揃っている
// (`packRoomDetailed` は owner に `isMuted: false` を固定で返し、
// `isRoomMember` は owner を暗黙のメンバーとして扱う)。
//
// **ownerId を書き換えるだけだと両側が壊れる。**
//   - 新 owner は membership 行を抱えたまま owner になる。mute していた場合、
//     API もフロントも「ミュートしていない」と言うのに行だけが残り、UI から
//     解除する手段が無い (フロントは owner に mute スイッチを出さない)。
//     joined / joining 一覧にも自分の room が出る。
//     **通知は落ちない** — fan-out は owner を mute 判定より前に never-muted と
//     して seed する (core/chat/service.go の emitRoomNewChatMessage)。
//   - 旧 owner は行を持たないまま非 owner になる。`isRoomMember` は
//     `ownerId == userID || membership` なので**どちらも false** になり、
//     譲渡した本人が room から締め出される。
//
// oldOwnerMembershipID は旧 owner に作る行の主キー。repository は ID 生成器を
// 持たないので呼び出し元が採番する。
//
// **連合面は対象外。** 譲渡そのものを AP で伝える仕組みは無く、local 所有の
// room を remote user へ渡すと roomURI や attributedTo が相手ホストに存在しない
// 値に化ける (core/chat/service.go の roomURI 組み立てと renderer.go の
// AttributedTo)。これは transfer-ownership が元から持つ性質で、ここでは直さない。
//
// UPDATE は `"ownerId" = oldOwnerID` を条件に持つ。同時に走った 2 つの譲渡の
// うち後から来たほうは 0 行更新になり ErrNotFound を返すので、**消える側の
// membership だけが適用された中途半端な状態**にはならない。
func (r *chatRepository) TransferRoomOwnership(roomID, oldOwnerID, newOwnerID, oldOwnerMembershipID string) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.ChatRoom{}).
			Where(`"id" = ? AND "ownerId" = ?`, roomID, oldOwnerID).
			Update("ownerId", newOwnerID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		if oldOwnerID == newOwnerID {
			// owner が変わらないなら membership も動かさない。**ここで返さないと
			// 下の入れ替えが自分の行を消して作り直す**ので、この関数自身が
			// 「owner は行を持たない」契約を破る。呼び出し元のガードには任せない。
			//
			// owner 検査の**後**に置くこと。先頭で返すと、その room の owner で
			// ない者による自己譲渡が黙って成功する。
			return nil
		}
		if err := tx.Where(`"roomId" = ? AND "userId" = ?`, roomID, newOwnerID).
			Delete(&model.ChatRoomMembership{}).Error; err != nil {
			return err
		}
		// **保留中の招待も消す。** 残すと新 owner が自分でそれを accept でき、
		// 今消したばかりの行が同じ room に作り直される
		// (`InvitationsAccept` / `RoomsJoin` / `AddMemberViaAP` はいずれも
		// 「その user が room の owner か」を見ない)。upstream は owner 宛の
		// 招待を作れない (createRoomInvitation が self-invite と既存メンバーを
		// 弾く) ので、この形の行は譲渡でしか生まれない。
		if err := tx.Where(`"roomId" = ? AND "userId" = ?`, roomID, newOwnerID).
			Delete(&model.ChatRoomInvitation{}).Error; err != nil {
			return err
		}
		// 旧 owner を明示メンバーとして残す。譲渡は「owner の座を渡す」操作で
		// あって「room を抜ける」操作ではない。
		//
		// **既にある行は触らない。** 不整合データ (この修正より前の譲渡で
		// 残った行) では旧 owner が行を持っていることがあり、そこで
		// `isMuted` を false に戻すと利用者が設定した mute を勝手に解除する。
		var existing model.ChatRoomMembership
		err := tx.Where(`"roomId" = ? AND "userId" = ?`, roomID, oldOwnerID).First(&existing).Error
		if err == nil {
			return nil
		}
		if !IsNotFound(err) {
			return err
		}
		return tx.Create(&model.ChatRoomMembership{
			ID:     oldOwnerMembershipID,
			UserID: oldOwnerID,
			RoomID: roomID,
		}).Error
	})
}

func (r *chatRepository) DeleteRoom(id string) error {
	return r.db.Where(`"id" = ?`, id).Delete(&model.ChatRoom{}).Error
}

func (r *chatRepository) ListRoomsByOwner(ownerID, sinceID, untilID string, limit int) ([]*model.ChatRoom, error) {
	var rooms []*model.ChatRoom
	q := applyIDCursor(r.db.Where(`"ownerId" = ?`, ownerID), `"id"`, sinceID, untilID, limit, 30)
	if err := q.Find(&rooms).Error; err != nil {
		return nil, err
	}
	return rooms, nil
}

func (r *chatRepository) ListJoinedRooms(userID, sinceID, untilID string, limit int) ([]*model.ChatRoom, error) {
	var rooms []*model.ChatRoom
	q := r.db.Joins(`JOIN "chat_room_membership" ON "chat_room_membership"."roomId" = "chat_room"."id"`).
		Where(`"chat_room_membership"."userId" = ?`, userID)
	q = applyIDCursor(q, `"chat_room"."id"`, sinceID, untilID, limit, 30)
	if err := q.Find(&rooms).Error; err != nil {
		return nil, err
	}
	return rooms, nil
}

func (r *chatRepository) CreateMessage(msg *model.ChatMessage) error {
	return r.db.Create(msg).Error
}

func (r *chatRepository) FindMessageByID(id string) (*model.ChatMessage, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var msg model.ChatMessage
	if err := r.db.Preload("FromUser").Preload("File").Where(`"id" = ?`, id).First(&msg).Error; err != nil {
		return nil, err
	}
	return &msg, nil
}

func (r *chatRepository) FindMessageByURI(uri string) (*model.ChatMessage, error) {
	if !storable(uri) {
		return nil, ErrNotFound
	}
	var msg model.ChatMessage
	if err := r.db.Preload("FromUser").Preload("File").Where(`"uri" = ?`, uri).First(&msg).Error; err != nil {
		return nil, err
	}
	return &msg, nil
}

func (r *chatRepository) UpdateMessage(msg *model.ChatMessage) error {
	return r.db.Save(msg).Error
}

func (r *chatRepository) DeleteMessage(id string) error {
	return r.db.Where(`"id" = ?`, id).Delete(&model.ChatMessage{}).Error
}

func (r *chatRepository) ListMessagesByRoom(roomID, sinceID, untilID string, limit int) ([]*model.ChatMessage, error) {
	var msgs []*model.ChatMessage
	q := applyIDCursor(r.db.Preload("FromUser").Preload("File").Where(`"toRoomId" = ?`, roomID),
		`"id"`, sinceID, untilID, limit, 20)
	if err := q.Find(&msgs).Error; err != nil {
		return nil, err
	}
	return msgs, nil
}

func (r *chatRepository) ListMessagesByFileID(fileID, untilID, sinceID string, limit int) ([]*model.ChatMessage, error) {
	if limit <= 0 {
		limit = 10
	}
	q := r.db.Preload("FromUser").Preload("ToUser").Preload("ToRoom").Preload("File").
		Where(`"fileId" = ?`, fileID)
	if untilID != "" {
		q = q.Where(`"id" < ?`, untilID)
	}
	if sinceID != "" {
		q = q.Where(`"id" > ?`, sinceID)
	}
	var msgs []*model.ChatMessage
	// upstream drive/files/attached-chat-messages.ts は makePaginationQuery を
	// 使う (#2713)。cursor ページングする chat 一覧のうち、applyIDCursor を
	// 通らない唯一のもの (SearchMessages / List*History は cursor を取らない)。
	if err := q.Order(paginationOrder(sinceID, untilID, `"id"`)).Limit(limit).Find(&msgs).Error; err != nil {
		return nil, err
	}
	return msgs, nil
}

func (r *chatRepository) ListMessagesByUser(userID, otherUserID, sinceID, untilID string, limit int) ([]*model.ChatMessage, error) {
	var msgs []*model.ChatMessage
	q := r.db.Preload("FromUser").Preload("File").
		Where(`("fromUserId" = ? AND "toUserId" = ?) OR ("fromUserId" = ? AND "toUserId" = ?)`,
			userID, otherUserID, otherUserID, userID)
	q = applyIDCursor(q, `"id"`, sinceID, untilID, limit, 20)
	if err := q.Find(&msgs).Error; err != nil {
		return nil, err
	}
	return msgs, nil
}

// SearchMessages mirrors upstream ChatService.searchMessages: when userID is
// set it scopes to the 1-on-1 conversation with that user, when roomID is set
// it scopes to that room, and otherwise it covers every message the viewer can
// see (own 1-on-1 messages + rooms they are a member of or own). The membership
// + NO_SUCH_ROOM gate for roomID is enforced by the handler before this is
// reached (upstream search.ts).
func (r *chatRepository) SearchMessages(meID, query string, limit int, userID, roomID string) ([]*model.ChatMessage, error) {
	// 列に入らない文字は保存された値に現れないので、一致しえない (#3025)。
	// **引く前に弾く** — LIKE のパターンに載せるとクエリごと落ちて 500 になる。
	if !storable(query) || !storable(meID) || !storable(userID) || !storable(roomID) {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	q := r.db.Preload("FromUser").Preload("ToUser").Preload("ToRoom").Preload("ToRoom.Owner").Preload("File")
	switch {
	case userID != "":
		q = q.Where(`("fromUserId" = ? AND "toUserId" = ?) OR ("fromUserId" = ? AND "toUserId" = ?)`,
			meID, userID, userID, meID)
	case roomID != "":
		q = q.Where(`"toRoomId" = ?`, roomID)
	default:
		// 自分の 1-on-1 + 所属 / 所有 room のメッセージを対象にする
		// (upstream の membership / owned-room サブクエリ相当)。
		memberSub := r.db.Model(&model.ChatRoomMembership{}).Select(`"roomId"`).Where(`"userId" = ?`, meID)
		ownedSub := r.db.Model(&model.ChatRoom{}).Select(`"id"`).Where(`"ownerId" = ?`, meID)
		q = q.Where(`"fromUserId" = ? OR "toUserId" = ? OR "toRoomId" IN (?) OR "toRoomId" IN (?)`,
			meID, meID, memberSub, ownedSub)
	}
	// LIKE metacharacter を escape して literal 一致にする (upstream sqlLikeEscape)。
	q = q.Where(`LOWER("text") LIKE ?`, "%"+escapeLike(strings.ToLower(query))+"%")
	var msgs []*model.ChatMessage
	if err := q.Order(`"id" DESC`).Limit(limit).Find(&msgs).Error; err != nil {
		return nil, err
	}
	return msgs, nil
}

func (r *chatRepository) CreateMembership(m *model.ChatRoomMembership) error {
	return r.db.Create(m).Error
}

func (r *chatRepository) FindMembership(userID, roomID string) (*model.ChatRoomMembership, error) {
	if !storable(userID) || !storable(roomID) {
		return nil, ErrNotFound
	}
	var m model.ChatRoomMembership
	if err := r.db.Where(`"userId" = ? AND "roomId" = ?`, userID, roomID).First(&m).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *chatRepository) UpdateMembership(m *model.ChatRoomMembership) error {
	return r.db.Save(m).Error
}

func (r *chatRepository) DeleteMembership(userID, roomID string) error {
	return r.db.Where(`"userId" = ? AND "roomId" = ?`, userID, roomID).Delete(&model.ChatRoomMembership{}).Error
}

func (r *chatRepository) ListMembersByRoom(roomID string) ([]*model.ChatRoomMembership, error) {
	var members []*model.ChatRoomMembership
	if err := r.db.Preload("User").Where(`"roomId" = ?`, roomID).Find(&members).Error; err != nil {
		return nil, err
	}
	return members, nil
}

func (r *chatRepository) ListMembersByRoomPaged(roomID, sinceID, untilID string, limit int) ([]*model.ChatRoomMembership, error) {
	var members []*model.ChatRoomMembership
	q := applyIDCursor(r.db.Preload("User").Where(`"roomId" = ?`, roomID), `"id"`, sinceID, untilID, limit, 30)
	if err := q.Find(&members).Error; err != nil {
		return nil, err
	}
	return members, nil
}

func (r *chatRepository) ListMembershipsByUser(userID, sinceID, untilID string, limit int) ([]*model.ChatRoomMembership, error) {
	var rows []*model.ChatRoomMembership
	q := applyIDCursor(r.db.Preload("Room").Preload("Room.Owner").Where(`"userId" = ?`, userID),
		`"id"`, sinceID, untilID, limit, 30)
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *chatRepository) CreateInvitation(inv *model.ChatRoomInvitation) error {
	return r.db.Create(inv).Error
}

func (r *chatRepository) DeleteInvitation(id string) error {
	return r.db.Where(`"id" = ?`, id).Delete(&model.ChatRoomInvitation{}).Error
}

func (r *chatRepository) UpdateInvitation(inv *model.ChatRoomInvitation) error {
	return r.db.Save(inv).Error
}

func (r *chatRepository) FindInvitation(userID, roomID string) (*model.ChatRoomInvitation, error) {
	if !storable(userID) || !storable(roomID) {
		return nil, ErrNotFound
	}
	var inv model.ChatRoomInvitation
	if err := r.db.Where(`"userId" = ? AND "roomId" = ?`, userID, roomID).First(&inv).Error; err != nil {
		return nil, err
	}
	return &inv, nil
}

func (r *chatRepository) FindInvitationByID(id string) (*model.ChatRoomInvitation, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var inv model.ChatRoomInvitation
	if err := r.db.Where(`"id" = ?`, id).First(&inv).Error; err != nil {
		return nil, err
	}
	return &inv, nil
}

func (r *chatRepository) CountUnread(userID string) (int64, error) {
	var count int64
	if err := r.db.Model(&model.ChatMessage{}).
		Where(`"toUserId" = ? AND NOT ("reads" @> ARRAY[?]::varchar[])`, userID, userID).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// HasUnreadFromUser mirrors MarkAllReadFromUser's WHERE clause: a 1-on-1
// message from otherID to readerID that readerID has not read yet.
func (r *chatRepository) HasUnreadFromUser(readerID, otherID string) (bool, error) {
	var count int64
	if err := r.db.Model(&model.ChatMessage{}).
		Where(`"fromUserId" = ? AND "toUserId" = ? AND NOT ("reads" @> ARRAY[?]::varchar[])`, otherID, readerID, readerID).
		Limit(1).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// HasUnreadInRoom mirrors MarkAllReadInRoom's WHERE clause: a room message
// authored by someone other than readerID that readerID has not read yet.
func (r *chatRepository) HasUnreadInRoom(readerID, roomID string) (bool, error) {
	var count int64
	if err := r.db.Model(&model.ChatMessage{}).
		Where(`"toRoomId" = ? AND "fromUserId" <> ? AND NOT ("reads" @> ARRAY[?]::varchar[])`, roomID, readerID, readerID).
		Limit(1).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func (r *chatRepository) MarkRead(userID, messageID string) error {
	return r.db.Exec(`UPDATE "chat_message" SET "reads" = array_append("reads", ?) WHERE "id" = ? AND NOT ("reads" @> ARRAY[?]::varchar[])`,
		userID, messageID, userID).Error
}

func (r *chatRepository) ListInvitationsByUser(userID string, ignored bool, sinceID, untilID string, limit int) ([]*model.ChatRoomInvitation, error) {
	var rows []*model.ChatRoomInvitation
	q := applyIDCursor(r.db.Preload("Room").Where(`"userId" = ? AND "ignored" = ?`, userID, ignored),
		`"id"`, sinceID, untilID, limit, 30)
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *chatRepository) ListInvitationsByRoom(roomID, sinceID, untilID string, limit int) ([]*model.ChatRoomInvitation, error) {
	var rows []*model.ChatRoomInvitation
	q := applyIDCursor(r.db.Preload("User").Where(`"roomId" = ?`, roomID), `"id"`, sinceID, untilID, limit, 30)
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *chatRepository) UpdateDeliveryStatus(messageID string, delivering, failed bool) error {
	return r.db.Model(&model.ChatMessage{}).Where("id = ?", messageID).
		Updates(map[string]any{"isDelivering": delivering, "isDeliverFailed": failed}).Error
}

// ListHistory returns the most recent message per conversation (1-on-1 or
// room) involving userID, using DISTINCT ON to pick one row per conversation.
func (r *chatRepository) ListHistory(userID string, limit int) ([]*model.ChatMessage, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	// 1対1 (toUserId/fromUserId) とルーム (toRoomId経由のmembership) をUNION
	// し、会話keyごとの最新メッセージを取る。
	// GORMの.Limit()は.Raw()の後では無視されるため、SQL内にLIMITを埋め込む。
	// DISTINCT ONはconversation_keyの昇順を強制するため、そのままLIMITを
	// 掛けるとアルファベット順で先頭N件が返ってしまう。外側のサブクエリで
	// id DESCに並べ直してからLIMITすることで最新のN会話を取得する。
	// Raw SQLではPreloadが使えないため、2段階で取得する:
	// 1) Raw SQLで対象メッセージIDを取得
	// 2) 通常のGORMクエリでPreload("FromUser")付きで再取得
	// ルーム所有者はmembershipレコードなしでも暗黙メンバーとして扱う
	// (IsRoomMemberと同じセマンティクス)。EXISTS句でmembership OR owner を判定。
	var ids []string
	err := r.db.Raw(`
		SELECT id FROM (
			SELECT DISTINCT ON (conversation_key) *
			FROM (
				SELECT *, LEAST("fromUserId","toUserId") || '-' || GREATEST("fromUserId","toUserId") AS conversation_key
				FROM "chat_message"
				WHERE ("fromUserId" = ? OR "toUserId" = ?) AND "toRoomId" IS NULL
				UNION ALL
				SELECT cm.*, 'room:' || cm."toRoomId" AS conversation_key
				FROM "chat_message" cm
				WHERE cm."toRoomId" IS NOT NULL
				AND (
					EXISTS (SELECT 1 FROM "chat_room_membership" m WHERE m."roomId" = cm."toRoomId" AND m."userId" = ?)
					OR EXISTS (SELECT 1 FROM "chat_room" cr WHERE cr."id" = cm."toRoomId" AND cr."ownerId" = ?)
				)
			) sub
			ORDER BY conversation_key, id DESC
		) conversations
		ORDER BY id DESC
		LIMIT ?
	`, userID, userID, userID, userID, limit).Pluck("id", &ids).Error
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var msgs []*model.ChatMessage
	if err := r.db.Preload("FromUser").Preload("ToUser").Preload("ToRoom").Preload("File").Where("id IN ?", ids).Order("id DESC").Find(&msgs).Error; err != nil {
		return nil, err
	}
	return msgs, nil
}

// ListUserHistory returns the latest DM (1-on-1) message per peer, ordered
// by recency. Mirrors upstream ChatService.userHistory (#692)。room messages
// (toRoomId IS NOT NULL) は対象外。
func (r *chatRepository) ListUserHistory(userID string, limit int) ([]*model.ChatMessage, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	var ids []string
	err := r.db.Raw(`
		SELECT id FROM (
			SELECT DISTINCT ON (conversation_key) *
			FROM (
				SELECT *, LEAST("fromUserId","toUserId") || '-' || GREATEST("fromUserId","toUserId") AS conversation_key
				FROM "chat_message"
				WHERE ("fromUserId" = ? OR "toUserId" = ?) AND "toRoomId" IS NULL
			) sub
			ORDER BY conversation_key, id DESC
		) conversations
		ORDER BY id DESC
		LIMIT ?
	`, userID, userID, limit).Pluck("id", &ids).Error
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var msgs []*model.ChatMessage
	if err := r.db.Preload("FromUser").Preload("ToUser").Preload("File").Where("id IN ?", ids).Order("id DESC").Find(&msgs).Error; err != nil {
		return nil, err
	}
	return msgs, nil
}

// ListRoomHistory returns the latest message per room that userID owns or
// is a member of. Mirrors upstream ChatService.roomHistory (#692)。
func (r *chatRepository) ListRoomHistory(userID string, limit int) ([]*model.ChatMessage, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	var ids []string
	err := r.db.Raw(`
		SELECT id FROM (
			SELECT DISTINCT ON ("toRoomId") cm.*
			FROM "chat_message" cm
			WHERE cm."toRoomId" IS NOT NULL
			AND (
				EXISTS (SELECT 1 FROM "chat_room_membership" m WHERE m."roomId" = cm."toRoomId" AND m."userId" = ?)
				OR EXISTS (SELECT 1 FROM "chat_room" cr WHERE cr."id" = cm."toRoomId" AND cr."ownerId" = ?)
			)
			ORDER BY "toRoomId", cm.id DESC
		) conversations
		ORDER BY id DESC
		LIMIT ?
	`, userID, userID, limit).Pluck("id", &ids).Error
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var msgs []*model.ChatMessage
	if err := r.db.Preload("FromUser").Preload("ToRoom").Preload("File").Where("id IN ?", ids).Order("id DESC").Find(&msgs).Error; err != nil {
		return nil, err
	}
	return msgs, nil
}

func (r *chatRepository) MarkAllRead(userID string) error {
	return r.db.Exec(`UPDATE "chat_message" SET "reads" = array_append("reads", ?) WHERE "toUserId" = ? AND NOT ("reads" @> ARRAY[?]::varchar[])`,
		userID, userID, userID).Error
}

// MarkAllReadFromUser appends readerID to `reads` for every unread DM
// where fromUserID -> readerID. Idempotent via NOT (reads @> ...) guard.
func (r *chatRepository) MarkAllReadFromUser(readerID, fromUserID string) error {
	return r.db.Exec(
		`UPDATE "chat_message" SET "reads" = array_append("reads", ?) WHERE "fromUserId" = ? AND "toUserId" = ? AND NOT ("reads" @> ARRAY[?]::varchar[])`,
		readerID, fromUserID, readerID, readerID,
	).Error
}

// MarkAllReadInRoom appends readerID to `reads` for every unread room
// message in roomID, except messages authored by readerID itself.
func (r *chatRepository) MarkAllReadInRoom(readerID, roomID string) error {
	return r.db.Exec(
		`UPDATE "chat_message" SET "reads" = array_append("reads", ?) WHERE "toRoomId" = ? AND "fromUserId" <> ? AND NOT ("reads" @> ARRAY[?]::varchar[])`,
		readerID, roomID, readerID, readerID,
	).Error
}

func (r *chatRepository) AddReaction(messageID, reaction string) error {
	// MarkReadと同じく重複ガード付き。同一reactionの二重追加を防ぐ。
	return r.db.Exec(`UPDATE "chat_message" SET "reactions" = array_append("reactions", ?) WHERE "id" = ? AND NOT ("reactions" @> ARRAY[?]::varchar[])`,
		reaction, messageID, reaction).Error
}

func (r *chatRepository) RemoveReaction(messageID, reaction string) (bool, error) {
	// **実際に持っているときだけ UPDATE する。** `array_remove` は無い要素を
	// 渡しても成功するので、条件を付けないと `RowsAffected` が常に 1 になり
	// 「消えたか」を判定できない (`AddReaction` の重複ガードと対称)。
	res := r.db.Exec(`UPDATE "chat_message" SET "reactions" = array_remove("reactions", ?) WHERE "id" = ? AND "reactions" @> ARRAY[?]::varchar[]`,
		reaction, messageID, reaction)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}
