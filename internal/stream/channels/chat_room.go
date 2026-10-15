package channels

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"

	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
)

// ChatRoomChannel forwards chat room events (message / edited / deleted /
// read) to any connected client that is a member of the target room.
// Init に失敗 (未参加、room 不明) した場合は topic を subscribe せず沈黙する
// — 本家 Misskey ChatRoomChannel と同じ挙動。
type ChatRoomChannel struct {
	ctx    stream.ChannelContext
	svc    *corechat.Service
	roomID string
	topic  string
	// leftは、部屋を見られなくなったと分かった後の印。以降のイベントを送らない。
	left atomic.Bool
	// viewableUntilは、見られると確かめた結果を使い回してよい期限(UnixNano)。
	viewableUntil atomic.Int64
	// nowは時刻の取得元。nilならtime.Now(テストが差し替える)。
	now func() time.Time
}

// chatRoomRecheckInterval is how long a positive access check of a chatRoom
// channel is reused before the next event checks again.
//
// 発言のたびにDBを引かないよう、見られるという判定だけを短く覚える。部屋を
// 抜けた後も、この間はイベントが届きうる。見られないという判定はすぐに購読を
// 外すので覚えない。
const chatRoomRecheckInterval = 5 * time.Second

// ChatRoomFactory holds the shared chat service so the registry can build
// channel instances per connection.
type ChatRoomFactory struct {
	svc *corechat.Service
}

// NewChatRoomFactory constructs a ChatRoomFactory.
func NewChatRoomFactory(svc *corechat.Service) *ChatRoomFactory {
	return &ChatRoomFactory{svc: svc}
}

// New builds a new channel bound to ctx, usable directly as stream.ChannelFactory.
func (f *ChatRoomFactory) New(ctx stream.ChannelContext) stream.Channel {
	return &ChatRoomChannel{ctx: ctx, svc: f.svc}
}

// Init parses `roomId` from params, verifies the connected user is a member
// of the room, then subscribes to the shared Redis topic.
func (c *ChatRoomChannel) Init(params json.RawMessage) error {
	var p struct {
		RoomID string `json:"roomId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.RoomID == "" {
		return stream.ErrInvalidParams
	}
	user, ok := c.ctx.User().(*model.User)
	if !ok || user == nil {
		return stream.ErrInvalidParams
	}
	if c.svc != nil {
		// #2106 L57: room メンバーに加え moderator も購読を許可する
		// (upstream hasPermissionToViewRoomTimeline)。
		ok, err := c.svc.CanViewRoomTimeline(user.ID, p.RoomID)
		if err != nil || !ok {
			return stream.ErrInvalidParams
		}
	}
	c.roomID = p.RoomID
	c.topic = "chatRoomStream:" + p.RoomID
	c.markViewable()
	c.ctx.Subscribe(c.topic)
	return nil
}

func (c *ChatRoomChannel) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// markViewable records a positive access check for chatRoomRecheckInterval.
func (c *ChatRoomChannel) markViewable() {
	c.viewableUntil.Store(c.clock().Add(chatRoomRecheckInterval).UnixNano())
}

// OnRedisEvent decodes a {type, body} envelope and forwards the body to the
// client with the matching event type, as long as the viewer may still see
// the room.
func (c *ChatRoomChannel) OnRedisEvent(payload []byte) {
	if c.left.Load() {
		return
	}
	var env struct {
		Type string          `json:"type"`
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(payload, &env); err != nil || env.Type == "" {
		return
	}
	if !c.stillViewable() {
		return
	}
	_ = c.ctx.Send(env.Type, env.Body)
}

// stillViewable re-checks the viewer's access to the room before an event is
// forwarded and stops the channel once the access is gone.
//
// 部屋を見られなくなったら(自分で抜ける、オーナーに外される、部屋が消される、
// moderatorでなくなる)、開いたままのチャンネルにもイベントを送らない。外れる
// 経路はAPIのhandler・service・連合の受信に散っているので、退室のイベントを
// 全経路から出すより、配信の側で見直すほうが確実。
// 判定がDBの障害で取れないときは、そのイベントだけ送らずに購読は残す。
// 見られるという判定はchatRoomRecheckIntervalのあいだ使い回す。
func (c *ChatRoomChannel) stillViewable() bool {
	if c.svc == nil {
		return true
	}
	user, ok := c.ctx.User().(*model.User)
	if !ok || user == nil {
		return false
	}
	if c.clock().UnixNano() < c.viewableUntil.Load() {
		return true
	}
	ok, err := c.svc.CanViewRoomTimeline(user.ID, c.roomID)
	if err != nil {
		slog.Warn("chat room channel: membership re-check failed",
			"user", user.ID, "room", c.roomID, "err", err)
		return false
	}
	if !ok {
		if c.left.CompareAndSwap(false, true) {
			c.ctx.Unsubscribe(c.topic)
		}
		return false
	}
	c.markViewable()
	return true
}

// OnClientMessage handles the single supported client-side action: "read".
// Clients send `{type: "read", body: {id: messageID}}` to mark a message as
// read, which propagates to the service layer. Other message types are
// silently ignored.
func (c *ChatRoomChannel) OnClientMessage(msgType string, body json.RawMessage) {
	if c.svc == nil || c.roomID == "" {
		return
	}
	user, ok := c.ctx.User().(*model.User)
	if !ok || user == nil {
		return
	}
	switch msgType {
	case "read":
		// chat-userと同じく、既読にはwrite:chatを要求する。
		if !c.ctx.HasPermission(chatWriteKind) {
			return
		}
		// upstream chat-room.ts onMessage('read') は body を見ず部屋全体を既読化
		// する (#1549)。
		if err := c.svc.ReadRoomChat(context.Background(), user.ID, c.roomID); err != nil {
			slog.Info("chat room channel: read failed",
				"user", user.ID, "room", c.roomID, "err", err)
		}
	}
}

// RequiredPermission implements stream.PermittedChannel.
func (c *ChatRoomChannel) RequiredPermission() string { return "read:chat" }

// Dispose unsubscribes from the room topic.
func (c *ChatRoomChannel) Dispose() {
	if c.topic != "" {
		c.ctx.Unsubscribe(c.topic)
	}
}

// HasService reports whether the chat service was wired into the factory.
//
// 未配線だと `chatRoom` channel の購読でメンバーシップ検査 (moderator を含む
// `CanViewRoomTimeline`) が丸ごと skip され、**誰でも任意の room の timeline を
// 購読できる**。起動時検査に使う (#2683)。
func (f *ChatRoomFactory) HasService() bool { return f.svc != nil }
