package channels

import (
	"encoding/json"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
)

// NotificationsChannel forwards notifications addressed to the authenticated
// user. Anonymous connections subscribe to nothing (the channel exists but
// stays silent).
type NotificationsChannel struct {
	ctx   stream.ChannelContext
	topic string
}

// NewNotifications returns a channel factory for "notifications".
func NewNotifications(ctx stream.ChannelContext) stream.Channel {
	return &NotificationsChannel{ctx: ctx}
}

func (c *NotificationsChannel) Init(_ json.RawMessage) error {
	user, ok := c.ctx.User().(*model.User)
	if !ok || user == nil {
		return stream.ErrInvalidParams
	}
	c.topic = "notifications:" + user.ID
	c.ctx.Subscribe(c.topic)
	return nil
}

func (c *NotificationsChannel) OnRedisEvent(payload []byte) {
	// muted instance 由来の通知は drop する (#1711, upstream main.ts の
	// isUserFromMutedInstance)。user-mute は通知作成時に適用済みなのでここでは
	// instance-mute のみ評価する。
	if notificationFromMutedInstance(payload, c.ctx.MuteBlockSnapshot()) {
		return
	}
	payload = hideNotificationNote(c.ctx, payload)
	_ = c.ctx.Send("notification", json.RawMessage(payload))
}

func (c *NotificationsChannel) OnClientMessage(string, json.RawMessage) {}

// ShouldShare implements stream.ShareableChannel.
func (c *NotificationsChannel) ShouldShare() bool { return true }

// RequiredPermission implements stream.PermittedChannel.
func (c *NotificationsChannel) RequiredPermission() string { return "read:account" }

func (c *NotificationsChannel) Dispose() {
	if c.topic != "" {
		c.ctx.Unsubscribe(c.topic)
	}
}

// MainChannel is the catch-all per-user channel. It currently forwards both
// notifications and follow events that target the authenticated user. Each
// inbound payload arrives with a hint type so the client can route it.
//
// Misskey 本家の main は notification / mention / unreadMessagingMessage /
// follow / unfollow / followed / receiveFollowRequest / readAllNotifications
// など多種のイベントを受信する。Phase 4.1 では notification と follow event
// に絞る。
type MainChannel struct {
	ctx     stream.ChannelContext
	notif   string
	mainTop string
}

// NewMain returns a channel factory for "main".
func NewMain(ctx stream.ChannelContext) stream.Channel {
	return &MainChannel{ctx: ctx}
}

func (c *MainChannel) Init(_ json.RawMessage) error {
	// TS本家のmain.ts: if (!this.user) return false;
	user, ok := c.ctx.User().(*model.User)
	if !ok || user == nil {
		return stream.ErrInvalidParams
	}
	c.notif = "notifications:" + user.ID
	c.mainTop = "main:" + user.ID
	c.ctx.Subscribe(c.notif)
	c.ctx.Subscribe(c.mainTop)
	return nil
}

// OnRedisEvent forwards a payload to the appropriate frontend event:
//
//   - main:<userID> topic carries a `{type, body}` envelope from
//     MainStreamPublisher (e.g. unreadNotification / readAllNotifications /
//     follow / meUpdated etc). Forward as the wrapped type.
//   - notifications:<userID> topic carries a bare packed Notification body
//     ({id, type:"mention"|"follow"|..., createdAt, ...}). Forward as
//     "notification" so the frontend's `main.on('notification', ...)`
//     listeners (timeline live update) fire.
//
// 旧実装は payload 全体に対して `type` だけを見て分岐していたため、bare
// 通知 body 内の `type:"mention"` を envelope の type として誤って扱い、
// 存在しない `mention` イベントを送出して通知 timeline のライブ更新を
// 落としていた (#420 follow-up)。判定は mainStreamEnvelope に集約してある。
func (c *MainChannel) OnRedisEvent(payload []byte) {
	if envType, envBody, ok := mainStreamEnvelope(payload); ok {
		// maybeRefreshFollowing は元 body (follow/unfollow の user) を見るので
		// note-hide ゲートより前に元の body で実行する。
		c.maybeRefreshFollowing(envType, envBody)
		var body json.RawMessage
		// mention envelope は upstream main.ts と同じく isNoteVisibleForMe +
		// isNoteMutedOrBlocked (instance-mute / user-mute / block / renote-mute /
		// channel-mute) を適用してから送る (#1711)。reply / renote は upstream
		// main.ts の switch でも gate されない (notification / mention のみ) ので
		// そのまま転送する。可視性は publish 段の CanSeeNote でも gate 済だが、
		// mention recipient は mentions/visibleUserIds に含まれるため再評価しても
		// 誤 drop しない (defense-in-depth)。
		if envType == "mention" {
			if !streamNoteVisibleForViewer(envBody, viewerIDFromCtx(c.ctx), c.ctx.FollowingSnapshot()) {
				return
			}
			if noteMutedOrBlocked(envBody, c.ctx.MuteBlockSnapshot()) {
				return
			}
		}
		// reply/renote/mention envelope の body は packed note。viewer 可視性で
		// top-level 著者設定 + depth-2 embed を hide する (#1568)。
		if isNoteEnvelope(envType) {
			body = json.RawMessage(hideEmbedsForViewer(envBody, viewerUserFromCtx(c.ctx), c.ctx.FollowingSnapshot(), time.Now().UnixMilli()))
		} else {
			// note を内包する envelope にも bare notification と同じ **hide gate** を
			// 通す。unreadNotification は packed notification を body にそのまま
			// 載せるので、これが無いと非可視 embed の内容が「未読バッジ用」イベント
			// 経由でそのまま届く。
			//
			// bare 経路がその前に掛けている instance-mute drop
			// (notificationFromMutedInstance) はここでは掛けない。upstream main.ts も
			// gate するのは notification / mention だけで unreadNotification は素通し
			// なので、そちらは parity 側に合わせてある。
			//
			// **type を列挙しないのは、列挙こそが漏れた原因だから。** #1568 は
			// bare notification topic と reply/renote/mention envelope だけを塞ぎ、
			// 同じ packed body を運ぶ unreadNotification を見落としていた。
			// hideNotificationNote は `note` を持たない body (readAllNotifications の
			// null、meUpdated、driveFileCreated 等) を verbatim で返すので、通知と
			// 無関係な envelope には作用しない。**ただしコストは掛かる** — probe struct
			// が 1 field でも json.Unmarshal は body 全体を走査するので、任意サイズの
			// body を運ぶ型 (registryUpdated / pageEvent) にも 1 回分乗る。probe struct は
			// 1 field なので追加コストは Unmarshal 1 回分に留まる (FollowingSnapshot は
			// note を検出してからしか触らない)。
			body = json.RawMessage(hideNotificationNote(c.ctx, envBody))
		}
		_ = c.ctx.Send(envType, body)
		return
	}
	// bare notification (notifications: topic) も muted instance 由来なら drop
	// する (#1711)。NotificationsChannel と同 doctrine。
	if notificationFromMutedInstance(payload, c.ctx.MuteBlockSnapshot()) {
		return
	}
	payload = hideNotificationNote(c.ctx, payload)
	_ = c.ctx.Send("notification", json.RawMessage(payload))
}

// mainStreamEnvelope reports whether payload is a `{type, body}` envelope from
// MainStreamPublisher (the `main:<userID>` topic) and returns its type and raw
// body. Payloads that are not envelopes come from the `notifications:<userID>`
// topic, which the same channel also subscribes to, and carry a bare packed
// Notification.
//
// 判定は「`type` と `body` が揃っていて、**かつ packed notification の署名
// (`id` と `createdAt`) を持たない**」。entity の packNotificationCore は
// この 3 つを必ず出力し、Extra の merge は core key と衝突するキーを skip する
// ので id / createdAt / type は潰せない。envelope 側 (PublishMainEvent が出す
// `map[string]any{"type":..,"body":..}`) は top-level に id / createdAt を
// 持たない。
//
// **`type` と `body` の存在だけでは足りない** (#2738)。`app` 通知は Extra の
// `body` / `header` / `icon` が top-level に merge されるので
// `{"id":..,"type":"app","createdAt":..,"body":"hello",..}` になり、存在だけを
// 見る判定では envelope と誤検出されて、イベント名 `app` / body は文字列
// `"hello"` として送られていた。期待は `notification` イベント + 通知オブジェクト
// 全体。
//
// **body が JSON object かどうかで切るのは不可。** envelope 側にも
// `{"type":"readAllNotifications","body":null}` のように object でない body が
// あり、bare 側の `app` body は文字列にも object にもなりうるので、どちら向きにも
// 誤る。
//
// **「top-level のキーがちょうど 2 つ」でも切れるが、採らない。** 現状の
// PublishMainEvent は 2 キーしか出さないので判定としては成立するが、envelope に
// キーが 1 つ増えた瞬間に **20 種ある main イベント全部**が bare 側へ落ちる。
// packed notification の署名を見る側は、誤ると落ちるのが notification 経路だけで
// 済むうえ、署名は packer の契約として固定されている。
//
// #420 の回帰も同時に防ぐ。bare 通知の `type:"mention"` は id / createdAt を
// 伴うのでここに来ない。
func mainStreamEnvelope(payload []byte) (string, json.RawMessage, bool) {
	var probe struct {
		Type      string          `json:"type"`
		Body      json.RawMessage `json:"body"`
		ID        json.RawMessage `json:"id"`
		CreatedAt json.RawMessage `json:"createdAt"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return "", nil, false
	}
	if probe.Type == "" || len(probe.Body) == 0 {
		return "", nil, false
	}
	if len(probe.ID) > 0 || len(probe.CreatedAt) > 0 {
		return "", nil, false
	}
	return probe.Type, probe.Body, true
}

// hideNotificationNote applies the per-viewer hideNote gate to the note embedded
// in a bare notification payload (notifications: topic body): the embedded note
// gets the top-level author-pref gate + its depth-2 renote/reply embed gate, the
// rest of the notification envelope is preserved. Returns the original payload
// verbatim when there is no embedded note or nothing is hideable (#1568).
func hideNotificationNote(ctx stream.ChannelContext, payload []byte) []byte {
	var probe struct {
		Note json.RawMessage `json:"note"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return payload
	}
	if len(probe.Note) == 0 || string(probe.Note) == "null" {
		return payload
	}
	gated, changed := hideNoteRawForViewer(probe.Note, viewerUserFromCtx(ctx), ctx.FollowingSnapshot(), time.Now().UnixMilli())
	if !changed {
		return payload
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return payload
	}
	m["note"] = json.RawMessage(gated)
	out, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return out
}

// isNoteEnvelope reports whether a main-stream {type,body} envelope carries a
// packed note as its body. publishNoteMainEvents emits exactly reply / renote /
// mention with body = packed NoteEntity; quote-type notifications flow via the
// bare notifications: topic instead (covered by hideNotificationNote).
func isNoteEnvelope(eventType string) bool {
	switch eventType {
	case "reply", "renote", "mention":
		return true
	default:
		return false
	}
}

// followingSnapshotUpdater is the optional capability a ChannelContext may
// expose to mutate the connection's followee snapshot live. The main channel
// uses it to keep timeline reply gating fresh after the viewer follows or
// unfollows someone, without waiting for a reconnect (#1211).
type followingSnapshotUpdater interface {
	UpdateFollowingSnapshot(followeeID string, following bool)
}

// maybeRefreshFollowing updates the connection's followee snapshot when the
// viewer's own follow list changes. The `follow` / `unfollow` events are
// emitted to the actor's own `main` stream by the following service, so the
// body is the followee user object; only its id is needed. `followed` (someone
// followed me) does not change my follow list and is intentionally ignored.
func (c *MainChannel) maybeRefreshFollowing(eventType string, body json.RawMessage) {
	if eventType != "follow" && eventType != "unfollow" {
		return
	}
	updater, ok := c.ctx.(followingSnapshotUpdater)
	if !ok {
		return
	}
	var u struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &u) != nil || u.ID == "" {
		return
	}
	updater.UpdateFollowingSnapshot(u.ID, eventType == "follow")
}

func (c *MainChannel) OnClientMessage(string, json.RawMessage) {}

// ShouldShare implements stream.ShareableChannel.
func (c *MainChannel) ShouldShare() bool { return true }

// RequiredPermission implements stream.PermittedChannel.
func (c *MainChannel) RequiredPermission() string { return "read:account" }

func (c *MainChannel) Dispose() {
	if c.notif != "" {
		c.ctx.Unsubscribe(c.notif)
	}
	if c.mainTop != "" {
		c.ctx.Unsubscribe(c.mainTop)
	}
}
