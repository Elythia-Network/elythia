package channels

import (
	"encoding/json"
	"sync"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
)

// UserListMembershipLookup fetches a list's memberships so the channel can
// snapshot per-member withReplies at Init (#2020、upstream user-list.ts の
// membershipsMap)。
type UserListMembershipLookup interface {
	ListMembers(listID string) ([]*model.UserListMembership, error)
}

// UserListOwnerLookup resolves a list by id for the channel-side ownership
// gate. repository.UserListRepository satisfies it (FindByID).
type UserListOwnerLookup interface {
	FindByID(id string) (*model.UserList, error)
}

// UserListLookup bundles the owner and membership lookups a production userList
// channel needs. repository.UserListRepository satisfies it.
type UserListLookup interface {
	UserListOwnerLookup
	UserListMembershipLookup
}

// UserListChannel forwards notes from members of a user list.
//
// upstream `user-list.ts` は Init で `membershipsMap[userId] = {withReplies}` を
// snapshot し、reply を per-member の withReplies で gate する。mk-go も
// membershipLookup が配線されていれば同じく Init snapshot を取って per-member
// gate する (#2020)。lookup 未配線時は従来どおり reply gate を skip する
// (後方互換、test 経路)。
type UserListChannel struct {
	ctx         stream.ChannelContext
	topic       string
	streamTopic string
	filter      noteFilter
	owners      UserListOwnerLookup
	memberships UserListMembershipLookup
	// withRepliesByUser は member userID → withReplies の snapshot。Init で構築し、
	// userAdded/userRemoved event で live 更新する (#2051)。nil の間は per-member
	// reply gate を skip する。note 配信 (userListTimeline:) と membership event
	// (userListStream:) は別 topic で並行 fanout されうるため mu で保護する。
	mu                sync.RWMutex
	withRepliesByUser map[string]bool
}

// UserListFactory carries the owner lookup used by the Init ownership gate and
// the membership lookup used for per-member withReplies gating (#2020)。
type UserListFactory struct {
	owners      UserListOwnerLookup
	memberships UserListMembershipLookup
}

// NewUserListFactory constructs a UserListFactory with the given lookup. owners
// and memberships are both served by it; a nil lookup fails closed (every
// subscription rejected).
func NewUserListFactory(l UserListLookup) *UserListFactory {
	// nil interface を代入すると owners / memberships も nil interface になるので、
	// Init 側の nil check がそのまま fail-closed として効く。
	return &UserListFactory{owners: l, memberships: l}
}

// New builds a new UserListChannel. Usable as a stream.ChannelFactory.
func (f *UserListFactory) New(ctx stream.ChannelContext) stream.Channel {
	return &UserListChannel{ctx: ctx, owners: f.owners, memberships: f.memberships}
}

func (c *UserListChannel) Init(params json.RawMessage) error {
	// TS Misskey: 認証欠如もlistId欠如もinitでfalseを返す
	user, ok := c.ctx.User().(*model.User)
	if !ok || user == nil {
		return stream.ErrInvalidParams
	}
	var p struct {
		ListID      string `json:"listId"`
		WithReplies *bool  `json:"withReplies"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	if p.ListID == "" {
		return stream.ErrInvalidParams
	}
	// Ownership gate: 接続中の user が所有する list でなければ購読を拒否する。
	// 本家 user-list.ts の `userListsRepository.exists({id, userId})` に相当し、
	// REST notes/user-list-timeline の owner check とも揃えている。isPublic な
	// list も owner 以外は購読させない (本家 / REST と同じ)。
	// owners 未配線時は fail-closed で購読拒否 (antenna #1569 と同方針)。
	if c.owners == nil {
		return stream.ErrInvalidParams
	}
	list, err := c.owners.FindByID(p.ListID)
	if err != nil || list == nil || list.UserID != user.ID {
		return stream.ErrInvalidParams
	}
	c.filter = parseNoteFilter(params)
	// withRepliesパラメータがtrueなら上書き
	if p.WithReplies != nil && *p.WithReplies {
		c.filter.WithReplies = true
	}
	// per-member withReplies を Init で snapshot する (upstream membershipsMap、#2020)。
	// lookup 未配線時は nil のまま = reply gate skip (後方互換)。
	if c.memberships != nil {
		c.withRepliesByUser = map[string]bool{}
		if members, err := c.memberships.ListMembers(p.ListID); err == nil {
			for _, m := range members {
				c.withRepliesByUser[m.UserID] = m.WithReplies
			}
		}
	}
	c.topic = "userListTimeline:" + p.ListID
	c.ctx.Subscribe(c.topic)
	// membership event (userAdded/userRemoved) は別 topic で届く (#1549, upstream
	// userListStream:${listId})。note 配信 (userListTimeline:) とは分離されている。
	c.streamTopic = "userListStream:" + p.ListID
	c.ctx.Subscribe(c.streamTopic)
	return nil
}

func (c *UserListChannel) OnRedisEvent(payload []byte) {
	// userListStream: topic からの membership event は {type, body} envelope で
	// 届く (#1549)。note 配信 (userListTimeline:) は raw note payload なので
	// top-level type を持たず、ここで分岐できる。
	var env struct {
		Type string          `json:"type"`
		Body json.RawMessage `json:"body"`
	}
	if json.Unmarshal(payload, &env) == nil && (env.Type == "userAdded" || env.Type == "userRemoved" || env.Type == "userUpdated") {
		c.applyMembershipEvent(env.Type, env.Body)
		// userUpdated は snapshot 更新専用の internal event。upstream に無く frontend も
		// 解さないので client へは流さない (#2051)。
		if env.Type != "userUpdated" {
			_ = c.ctx.Send(env.Type, env.Body)
		}
		return
	}

	viewerID := viewerIDFromCtx(c.ctx)
	// 未ログインの viewer には meta.ugcVisibilityForVisitor を適用する
	// (upstream NoteStreamingHidingService.filter)。
	if anonUGCVisibilityDrop(c.ctx, payload, viewerID) {
		return
	}
	if !c.filter.shouldEmit(payload, c.ctx.HardMuteRules(), viewerID) {
		return
	}
	if noteMutedOrBlocked(payload, c.ctx.MuteBlockSnapshot()) {
		return
	}
	// followers visibility note の defense-in-depth filter (#1465)。fanout 段
	// で list owner の follow 関係を check してから push する設計だが、過去に
	// stream へ滞留した entry や fanout の設定ミスに対して WS 側でも 1 段
	// gate する。本人 (`viewerID == note.userId`) は CanSeeNote の semantics
	// と同じく無条件 pass、それ以外は FollowingSnapshot で `note.userId` を
	// follow しているか確認する。snapshot が nil (= anonymous は Init で
	// 弾く設計なので来ない / lookup 未配線) の場合は fail-closed で drop する。
	// public / home / specified は fanout 段で適切に gate 済 (specified は
	// shouldFanoutToFollowers が除外) のためここでは何もしない。
	if !userListVisibilityShouldEmit(payload, viewerID, c.ctx.FollowingSnapshot()) {
		return
	}
	// per-member withReplies gate (#2020)。snapshot 配線時のみ適用。投稿者の
	// withReplies を mu 下で 1 件 lookup してから gate に渡す (#2051)。
	if c.withRepliesByUser != nil {
		var author struct {
			UserID string `json:"userId"`
		}
		_ = json.Unmarshal(payload, &author)
		c.mu.RLock()
		authorWithReplies := c.withRepliesByUser[author.UserID]
		c.mu.RUnlock()
		if !userListReplyShouldEmit(payload, viewerID, authorWithReplies, c.ctx.FollowingSnapshot()) {
			return
		}
	}
	payload = hideEmbeds(c.ctx, payload)
	// pure renote の renote.myReaction を viewer 毎に inject (#2058)。
	payload = injectRenoteMyReaction(payload, viewerID)
	_ = c.ctx.Send("note", json.RawMessage(payload))
}

// applyMembershipEvent live-updates the withReplies snapshot on membership
// events (#2051):
//   - userAdded: body は UserLite (id のみ、withReplies を持たない) なので新規 member は
//     withReplies=false default で追加 (upstream の membership 作成時 default と一致)。
//   - userUpdated: update-membership による withReplies 変更を body の値で反映。
//   - userRemoved: snapshot から除去。
//
// これにより upstream user-list.ts の 5s poll refresh と等価な add/remove/toggle 反映を
// event 駆動で実現する。
func (c *UserListChannel) applyMembershipEvent(eventType string, body json.RawMessage) {
	if c.withRepliesByUser == nil {
		return // gate 未配線時は snapshot を保持しない
	}
	var u struct {
		ID          string `json:"id"`
		WithReplies bool   `json:"withReplies"`
	}
	if json.Unmarshal(body, &u) != nil || u.ID == "" {
		return
	}
	c.mu.Lock()
	switch eventType {
	case "userAdded":
		c.withRepliesByUser[u.ID] = false // UserLite body は withReplies を持たない (新規 default false)
	case "userUpdated":
		c.withRepliesByUser[u.ID] = u.WithReplies // body の withReplies を反映 (#2051)
	case "userRemoved":
		delete(c.withRepliesByUser, u.ID)
	}
	c.mu.Unlock()
}

func (c *UserListChannel) OnClientMessage(string, json.RawMessage) {}

func (c *UserListChannel) Dispose() {
	if c.topic != "" {
		c.ctx.Unsubscribe(c.topic)
	}
	if c.streamTopic != "" {
		c.ctx.Unsubscribe(c.streamTopic)
	}
}

// userListVisibilityPayload は userList channel の visibility gate が
// 参照する最小 fields。`visibility` と `userId` (author) があれば判定できる。
type userListVisibilityPayload struct {
	UserID     string `json:"userId"`
	Visibility string `json:"visibility"`
}

// userListReplyPayload は reply gate が参照する最小 fields。
type userListReplyPayload struct {
	UserID string `json:"userId"`
	Reply  *struct {
		UserID     string `json:"userId"`
		Visibility string `json:"visibility"`
	} `json:"reply"`
}

// userListReplyShouldEmit applies upstream user-list.ts の per-member withReplies
// reply gate (#2020):
//   - 投稿者 (note.userId) の withReplies が true: reply 先が followers visibility で
//     viewer が reply.userId を follow していなければ drop。
//   - withReplies が false: 「接続主 (viewer) への reply」「接続主自身の投稿」「投稿者の
//     自己 reply」のいずれでもなければ drop。
//
// reply でない note / parse 失敗は pass (visibility gate 側で fail-closed 済)。
// authorWithReplies は投稿者 (note.userId) の membership withReplies (呼び出し側で
// lookup 済)。
func userListReplyShouldEmit(payload []byte, viewerID string, authorWithReplies bool, following map[string]bool) bool {
	var note userListReplyPayload
	if err := json.Unmarshal(payload, &note); err != nil || note.Reply == nil {
		return true
	}
	isMe := viewerID != "" && viewerID == note.UserID
	if authorWithReplies {
		if note.Reply.Visibility == "followers" {
			if following == nil {
				return false
			}
			_, follows := following[note.Reply.UserID]
			return follows
		}
		return true
	}
	// withReplies=false: 接続主への reply / 接続主の投稿 / 自己 reply 以外は drop。
	if note.Reply.UserID != viewerID && !isMe && note.Reply.UserID != note.UserID {
		return false
	}
	return true
}

// userListVisibilityShouldEmit returns false when a `followers` visibility note
// is being delivered to a viewer who does not follow the author (and is not the
// author themselves). For `public` / `home` / `specified` payloads it returns
// true unconditionally — fanout already filters those (specified is excluded by
// shouldFanoutToFollowers, public / home reach every list member by design).
//
// Defensive behaviour:
//   - payload parse 失敗 → conservative に drop (= 既存 reply gate と逆の方針
//     だが、visibility 不明 note は IDOR fail-closed が安全)。
//   - snapshot=nil + viewer ≠ author → drop (anonymous は Init で弾くが、
//     followingSnapshot lookup が未配線 / 取得失敗で nil 返却の場合に備える)。
func userListVisibilityShouldEmit(payload []byte, viewerID string, snap map[string]bool) bool {
	var note userListVisibilityPayload
	if err := json.Unmarshal(payload, &note); err != nil {
		return false
	}
	if note.Visibility != "followers" {
		return true
	}
	if viewerID == "" {
		return false
	}
	if note.UserID == viewerID {
		return true
	}
	if snap == nil {
		return false
	}
	_, follows := snap[note.UserID]
	return follows
}
