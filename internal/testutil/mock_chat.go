package testutil

import (
	"sort"
	"strings"
	"sync"

	"github.com/elythia-network/elythia/internal/model"
)

// MockChatRepository is an in-memory implementation of repository.ChatRepository
// shared across api/chat, core/chat and stream/channels test packages (#709).
//
// 各 test ファイルで重複していた mockChatRepo / fakeChatRepo / chatFakeRepo
// を集約。ChatRepository に新メソッドを追加するたびに複数 stub に実装を生やす
// 苦痛を解消する。
//
// 設計の意図:
//   - 全 map 操作は内部 mutex 配下で行うため goroutine 並列 (例: ap_delivery
//     のジョブ送出) でも race にならない。
//   - メッセージ書き込み / 読み込みは clone して保持することで、呼び出し側が
//     返り値を mutate しても store 内が壊れないようにする。
//   - CreateErr / UpdateErr / DeleteErr で error path をテストできる。
//   - 各 List* / FindInvitation 等 filter 操作はテストで実際に必要だった
//     ものだけ実装している。残り (ListRoomsByOwner, ListMembersByRoom 等)
//     は記録された state を元に予測可能な順序で返す。
type MockChatRepository struct {
	mu sync.Mutex

	// Rooms keyed by room ID.
	Rooms map[string]*model.ChatRoom
	// Messages keyed by message ID. Stored as cloned copies.
	Messages map[string]*model.ChatMessage
	// Memberships keyed by "<userID>:<roomID>".
	Memberships map[string]*model.ChatRoomMembership
	// Invitations keyed by invitation ID.
	Invitations map[string]*model.ChatRoomInvitation
	// AddedReactions / RemovedReactions record the keys passed to
	// AddReaction / RemoveReaction so tests can assert canonicalisation (#2106 N4).
	AddedReactions   []string
	RemovedReactions []string

	// CreateErr forces Create* to return this error without persisting.
	CreateErr error
	// UpdateErr forces UpdateMessage to return this error without persisting.
	UpdateErr error
	// DeleteErr forces DeleteMessage to return this error without removing.
	DeleteErr error
	// ListMembershipsErr forces ListMembershipsByUser to return this error.
	ListMembershipsErr error
	// TransferOwnershipErr forces TransferRoomOwnership to return this error
	// without touching the store.
	TransferOwnershipErr error
	// FindRoomErr / FindMembershipErr / ListMembersErr force the corresponding
	// lookup to fail. Used to cover the "DB 障害を not-found に丸めない" branches
	// (#2792), which are otherwise unreachable against an in-memory store.
	FindRoomErr       error
	FindMembershipErr error
	ListMembersErr    error
	// FindInvitationErr は FindInvitation と FindInvitationByID の両方に効く
	// (どちらも同じ「招待を引く」経路で、#2792 の判断も同じであるべきなため)。
	FindInvitationErr error
}

// NewMockChatRepository constructs an empty MockChatRepository ready for use.
func NewMockChatRepository() *MockChatRepository {
	return &MockChatRepository{
		Rooms:       make(map[string]*model.ChatRoom),
		Messages:    make(map[string]*model.ChatMessage),
		Memberships: make(map[string]*model.ChatRoomMembership),
		Invitations: make(map[string]*model.ChatRoomInvitation),
	}
}

func membershipKey(userID, roomID string) string { return userID + ":" + roomID }

// cursorKeep reports whether id passes the (sinceID, untilID) id-cursor used by
// the paginated chat list mocks (#1747)。
func cursorKeep(id, sinceID, untilID string) bool {
	if untilID != "" && id >= untilID {
		return false
	}
	if sinceID != "" && id <= sinceID {
		return false
	}
	return true
}

// clampMockLimit resolves limit<=0 to def and caps at 100 (upstream maximum)。
func clampMockLimit(limit, def int) int {
	if limit <= 0 {
		return def
	}
	if limit > 100 {
		return 100
	}
	return limit
}

// --- Rooms ---

func (m *MockChatRepository) CreateRoom(room *model.ChatRoom) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.CreateErr != nil {
		return m.CreateErr
	}
	m.Rooms[room.ID] = room
	return nil
}

func (m *MockChatRepository) FindRoomByID(id string) (*model.ChatRoom, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FindRoomErr != nil {
		return nil, m.FindRoomErr
	}
	if r, ok := m.Rooms[id]; ok {
		return r, nil
	}
	return nil, ErrNotFound
}

// FindRoomByURI looks a room up by its canonical AP URI (#2994). ローカル room は
// `uri` が NULL なので一致しない (本物の SQL と同じ)。
func (m *MockChatRepository) FindRoomByURI(uri string) (*model.ChatRoom, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FindRoomErr != nil {
		return nil, m.FindRoomErr
	}
	for _, r := range m.Rooms {
		if r.URI != nil && *r.URI == uri {
			return r, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockChatRepository) UpdateRoom(room *model.ChatRoom) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Rooms[room.ID] = room
	return nil
}

// TransferRoomOwnership mirrors the real repository: it moves ownership only
// when the room is still owned by oldOwnerID, and swaps the membership rows so
// that the owner never holds one (see repository.TransferRoomOwnership).
//
// **意味論を実装に揃えておくこと。** ここが単なる ownerId 代入だと、
// membership 行の入れ替えを検証しているつもりのテストが素通りする。
func (m *MockChatRepository) TransferRoomOwnership(roomID, oldOwnerID, newOwnerID, oldOwnerMembershipID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.TransferOwnershipErr != nil {
		return m.TransferOwnershipErr
	}
	room, ok := m.Rooms[roomID]
	if !ok || room.OwnerID != oldOwnerID {
		return ErrNotFound
	}
	// **引数で渡されたオブジェクトを書き換えない。** 実 repository は DB 行を
	// 更新するだけなので、呼び出し元が握っている room は旧 owner のまま残る。
	// ここで同じポインタを書き換えると、その差が test では見えなくなる。
	updated := *room
	updated.OwnerID = newOwnerID
	m.Rooms[roomID] = &updated
	if oldOwnerID == newOwnerID {
		return nil
	}
	delete(m.Memberships, membershipKey(newOwnerID, roomID))
	// 譲渡は保留中の招待も消す (残ると新 owner が accept して行を作り直せる)。
	for id, inv := range m.Invitations {
		if inv.RoomID == roomID && inv.UserID == newOwnerID {
			delete(m.Invitations, id)
		}
	}
	if _, exists := m.Memberships[membershipKey(oldOwnerID, roomID)]; !exists {
		m.Memberships[membershipKey(oldOwnerID, roomID)] = &model.ChatRoomMembership{
			ID:     oldOwnerMembershipID,
			UserID: oldOwnerID,
			RoomID: roomID,
		}
	}
	return nil
}

func (m *MockChatRepository) DeleteRoom(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Rooms, id)
	return nil
}

func (m *MockChatRepository) ListRoomsByOwner(ownerID, sinceID, untilID string, limit int) ([]*model.ChatRoom, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*model.ChatRoom, 0)
	for _, r := range m.Rooms {
		if r.OwnerID == ownerID && cursorKeep(r.ID, sinceID, untilID) {
			out = append(out, r)
		}
	}
	SortMockPage(out, sinceID, untilID, func(r *model.ChatRoom) string { return r.ID })
	return capRooms(out, clampMockLimit(limit, 30)), nil
}

func (m *MockChatRepository) ListJoinedRooms(userID, sinceID, untilID string, limit int) ([]*model.ChatRoom, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*model.ChatRoom, 0)
	for _, mem := range m.Memberships {
		if mem.UserID == userID {
			if r, ok := m.Rooms[mem.RoomID]; ok && cursorKeep(r.ID, sinceID, untilID) {
				out = append(out, r)
			}
		}
	}
	SortMockPage(out, sinceID, untilID, func(r *model.ChatRoom) string { return r.ID })
	return capRooms(out, clampMockLimit(limit, 30)), nil
}

func capRooms(rooms []*model.ChatRoom, limit int) []*model.ChatRoom {
	if len(rooms) > limit {
		return rooms[:limit]
	}
	return rooms
}

// --- Messages ---

func (m *MockChatRepository) CreateMessage(msg *model.ChatMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.CreateErr != nil {
		return m.CreateErr
	}
	clone := *msg
	m.Messages[msg.ID] = &clone
	return nil
}

func (m *MockChatRepository) FindMessageByID(id string) (*model.ChatMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if msg, ok := m.Messages[id]; ok {
		clone := *msg
		return &clone, nil
	}
	return nil, ErrNotFound
}

func (m *MockChatRepository) FindMessageByURI(uri string) (*model.ChatMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range m.Messages {
		if msg.URI != nil && *msg.URI == uri {
			clone := *msg
			return &clone, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockChatRepository) UpdateMessage(msg *model.ChatMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.UpdateErr != nil {
		return m.UpdateErr
	}
	clone := *msg
	m.Messages[msg.ID] = &clone
	return nil
}

func (m *MockChatRepository) DeleteMessage(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DeleteErr != nil {
		return m.DeleteErr
	}
	delete(m.Messages, id)
	return nil
}

func (m *MockChatRepository) ListMessagesByRoom(roomID, sinceID, untilID string, limit int) ([]*model.ChatMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*model.ChatMessage
	for _, msg := range m.Messages {
		if msg.ToRoomID != nil && *msg.ToRoomID == roomID && cursorKeep(msg.ID, sinceID, untilID) {
			cp := *msg
			out = append(out, &cp)
		}
	}
	SortMockPage(out, sinceID, untilID, func(x *model.ChatMessage) string { return x.ID })
	return capMessages(out, clampMockLimit(limit, 20)), nil
}

func (m *MockChatRepository) ListMessagesByUser(userID, otherUserID, sinceID, untilID string, limit int) ([]*model.ChatMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*model.ChatMessage
	for _, msg := range m.Messages {
		dm := (msg.FromUserID == userID && msg.ToUserID != nil && *msg.ToUserID == otherUserID) ||
			(msg.FromUserID == otherUserID && msg.ToUserID != nil && *msg.ToUserID == userID)
		if dm && cursorKeep(msg.ID, sinceID, untilID) {
			cp := *msg
			out = append(out, &cp)
		}
	}
	SortMockPage(out, sinceID, untilID, func(x *model.ChatMessage) string { return x.ID })
	return capMessages(out, clampMockLimit(limit, 20)), nil
}

// capMessages truncates to limit. Callers must sort with SortMockPage first:
// the cursor decides the direction, so sorting here would hide it.
func capMessages(msgs []*model.ChatMessage, limit int) []*model.ChatMessage {
	if len(msgs) > limit {
		return msgs[:limit]
	}
	return msgs
}

func (m *MockChatRepository) ListMessagesByFileID(fileID, untilID, sinceID string, limit int) ([]*model.ChatMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 10
	}
	var out []*model.ChatMessage
	for _, msg := range m.Messages {
		if msg.FileID == nil || *msg.FileID != fileID {
			continue
		}
		if untilID != "" && msg.ID >= untilID {
			continue
		}
		if sinceID != "" && msg.ID <= sinceID {
			continue
		}
		out = append(out, msg)
	}
	SortMockPage(out, sinceID, untilID, func(x *model.ChatMessage) string { return x.ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MockChatRepository) SearchMessages(meID, query string, limit int, userID, roomID string) ([]*model.ChatMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 10
	}
	q := strings.ToLower(query)
	var out []*model.ChatMessage
	for _, msg := range m.Messages {
		if msg.Text == nil || !strings.Contains(strings.ToLower(*msg.Text), q) {
			continue
		}
		match := false
		switch {
		case userID != "":
			match = (msg.FromUserID == meID && msg.ToUserID != nil && *msg.ToUserID == userID) ||
				(msg.FromUserID == userID && msg.ToUserID != nil && *msg.ToUserID == meID)
		case roomID != "":
			match = msg.ToRoomID != nil && *msg.ToRoomID == roomID
		default:
			switch {
			case msg.FromUserID == meID:
				match = true
			case msg.ToUserID != nil && *msg.ToUserID == meID:
				match = true
			case msg.ToRoomID != nil:
				if _, ok := m.Memberships[membershipKey(meID, *msg.ToRoomID)]; ok {
					match = true
				} else if r, ok := m.Rooms[*msg.ToRoomID]; ok && r.OwnerID == meID {
					match = true
				}
			}
		}
		if match {
			cp := *msg
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// --- Memberships ---

func (m *MockChatRepository) CreateMembership(mem *model.ChatRoomMembership) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Memberships[membershipKey(mem.UserID, mem.RoomID)] = mem
	return nil
}

func (m *MockChatRepository) FindMembership(userID, roomID string) (*model.ChatRoomMembership, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FindMembershipErr != nil {
		return nil, m.FindMembershipErr
	}
	if mem, ok := m.Memberships[membershipKey(userID, roomID)]; ok {
		return mem, nil
	}
	return nil, ErrNotFound
}

func (m *MockChatRepository) UpdateMembership(mem *model.ChatRoomMembership) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Memberships[membershipKey(mem.UserID, mem.RoomID)] = mem
	return nil
}

func (m *MockChatRepository) DeleteMembership(userID, roomID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Memberships, membershipKey(userID, roomID))
	return nil
}

func (m *MockChatRepository) ListMembersByRoom(roomID string) ([]*model.ChatRoomMembership, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ListMembersErr != nil {
		return nil, m.ListMembersErr
	}
	out := make([]*model.ChatRoomMembership, 0)
	for _, mem := range m.Memberships {
		if mem.RoomID == roomID {
			out = append(out, mem)
		}
	}
	return out, nil
}

func (m *MockChatRepository) ListMembersByRoomPaged(roomID, sinceID, untilID string, limit int) ([]*model.ChatRoomMembership, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*model.ChatRoomMembership, 0)
	for _, mem := range m.Memberships {
		if mem.RoomID == roomID && cursorKeep(mem.ID, sinceID, untilID) {
			out = append(out, mem)
		}
	}
	SortMockPage(out, sinceID, untilID, func(x *model.ChatRoomMembership) string { return x.ID })
	return capMemberships(out, clampMockLimit(limit, 30)), nil
}

func (m *MockChatRepository) ListMembershipsByUser(userID, sinceID, untilID string, limit int) ([]*model.ChatRoomMembership, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ListMembershipsErr != nil {
		return nil, m.ListMembershipsErr
	}
	out := make([]*model.ChatRoomMembership, 0)
	for _, mem := range m.Memberships {
		if mem.UserID == userID && cursorKeep(mem.ID, sinceID, untilID) {
			cp := *mem
			if r, ok := m.Rooms[mem.RoomID]; ok {
				cp.Room = r
			}
			out = append(out, &cp)
		}
	}
	// upstream getMyMemberships は makePaginationQuery を通すので、
	// sinceId 単独指定のときだけ membership id 昇順になる。
	SortMockPage(out, sinceID, untilID, func(x *model.ChatRoomMembership) string { return x.ID })
	return capMemberships(out, clampMockLimit(limit, 30)), nil
}

func capMemberships(rows []*model.ChatRoomMembership, limit int) []*model.ChatRoomMembership {
	if len(rows) > limit {
		return rows[:limit]
	}
	return rows
}

// --- Invitations ---

func (m *MockChatRepository) CreateInvitation(inv *model.ChatRoomInvitation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.CreateErr != nil {
		return m.CreateErr
	}
	m.Invitations[inv.ID] = inv
	return nil
}

func (m *MockChatRepository) DeleteInvitation(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Invitations, id)
	return nil
}

func (m *MockChatRepository) UpdateInvitation(inv *model.ChatRoomInvitation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Invitations[inv.ID] = inv
	return nil
}

func (m *MockChatRepository) FindInvitationByID(id string) (*model.ChatRoomInvitation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FindInvitationErr != nil {
		return nil, m.FindInvitationErr
	}
	if inv, ok := m.Invitations[id]; ok {
		return inv, nil
	}
	return nil, ErrNotFound
}

func (m *MockChatRepository) FindInvitation(userID, roomID string) (*model.ChatRoomInvitation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FindInvitationErr != nil {
		return nil, m.FindInvitationErr
	}
	for _, inv := range m.Invitations {
		if inv.UserID == userID && inv.RoomID == roomID {
			return inv, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockChatRepository) ListInvitationsByUser(_ string, _ bool, _, _ string, _ int) ([]*model.ChatRoomInvitation, error) {
	return nil, nil
}

func (m *MockChatRepository) ListInvitationsByRoom(roomID, sinceID, untilID string, limit int) ([]*model.ChatRoomInvitation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var rows []*model.ChatRoomInvitation
	for _, inv := range m.Invitations {
		if inv.RoomID == roomID && cursorKeep(inv.ID, sinceID, untilID) {
			rows = append(rows, inv)
		}
	}
	SortMockPage(rows, sinceID, untilID, func(x *model.ChatRoomInvitation) string { return x.ID })
	if l := clampMockLimit(limit, 30); len(rows) > l {
		rows = rows[:l]
	}
	return rows, nil
}

// --- Read tracking / unread count ---

func (m *MockChatRepository) CountUnread(_ string) (int64, error) { return 0, nil }

// HasUnreadFromUser reports whether readerID has an unread 1-on-1 message from
// otherID (readerID not in reads). Mirrors the production WHERE clause.
func (m *MockChatRepository) HasUnreadFromUser(readerID, otherID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range m.Messages {
		if msg.FromUserID == otherID && msg.ToUserID != nil && *msg.ToUserID == readerID && !readsContains(msg.Reads, readerID) {
			return true, nil
		}
	}
	return false, nil
}

// HasUnreadInRoom reports whether readerID has an unread room message (authored
// by someone else) in roomID.
func (m *MockChatRepository) HasUnreadInRoom(readerID, roomID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range m.Messages {
		if msg.ToRoomID != nil && *msg.ToRoomID == roomID && msg.FromUserID != readerID && !readsContains(msg.Reads, readerID) {
			return true, nil
		}
	}
	return false, nil
}

func readsContains(reads []string, id string) bool {
	for _, r := range reads {
		if r == id {
			return true
		}
	}
	return false
}

// MarkRead appends userID to the message's reads slice (idempotency は real
// impl 側のクエリで担保される。テスト用なので重複防止はしない)。
func (m *MockChatRepository) MarkRead(userID, messageID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if msg, ok := m.Messages[messageID]; ok {
		msg.Reads = append(msg.Reads, userID)
	}
	return nil
}

func (m *MockChatRepository) MarkAllRead(_ string) error            { return nil }
func (m *MockChatRepository) MarkAllReadFromUser(_, _ string) error { return nil }
func (m *MockChatRepository) MarkAllReadInRoom(_, _ string) error   { return nil }

// --- Delivery status / history ---

func (m *MockChatRepository) UpdateDeliveryStatus(_ string, _, _ bool) error { return nil }

func (m *MockChatRepository) ListHistory(_ string, _ int) ([]*model.ChatMessage, error) {
	return nil, nil
}

func (m *MockChatRepository) ListUserHistory(_ string, _ int) ([]*model.ChatMessage, error) {
	return nil, nil
}

func (m *MockChatRepository) ListRoomHistory(_ string, _ int) ([]*model.ChatMessage, error) {
	return nil, nil
}

// --- Reactions ---

// AddReaction records the attempt and updates the stored message.
//
// **状態を更新するのが要点 (#3037 レビュー)。** 更新しないと、`RemoveReaction`
// が「消えたか」を返すようになったこと (#3037) と噛み合わず、
// `React` → `Unreact` を通すテストで unreact が一度も publish されなくなる。
// それでもテストが緑になる形があり (最後のイベントが react なので同じ
// アサーションを満たす)、**有効だった検査が黙って空虚になっていた**。
func (m *MockChatRepository) AddReaction(messageID, key string) error {
	m.AddedReactions = append(m.AddedReactions, key)
	msg := m.Messages[messageID]
	if msg == nil {
		return nil
	}
	for _, r := range msg.Reactions {
		if r == key {
			return nil // 重複は足さない (本番の重複ガードと同じ)
		}
	}
	msg.Reactions = append(msg.Reactions, key)
	return nil
}

func (m *MockChatRepository) RemoveReaction(messageID, key string) (bool, error) {
	m.RemovedReactions = append(m.RemovedReactions, key)
	// **実データに合わせて「消えたか」を返す。** 常に true を返すと、
	// publish の条件 (#3037) を検査するテストが空虚になる。
	msg := m.Messages[messageID]
	if msg == nil {
		return false, nil
	}
	for i, r := range msg.Reactions {
		if r == key {
			msg.Reactions = append(append([]string{}, msg.Reactions[:i]...), msg.Reactions[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}
