package stream

import (
	"container/list"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/elythia-network/elythia/internal/core/ugcvisibility"
	"github.com/elythia-network/elythia/internal/model"
)

// PubSubBus is the minimal pubsub interface that Dispatcher needs. core/event.
// PubSubService がこれを満たす。reference counting は Dispatcher 側で行う。
type PubSubBus interface {
	// Subscribe returns the function that removes this handler again.
	// **トピック名で解除しない** — 同じトピックを複数の接続が購読するので、
	// 名前で閉じると他人の購読まで止まる (#H-4)。
	Subscribe(topic string, handler func([]byte)) func()
}

// channelEntry holds an active per-connection channel subscription with its
// list of pubsub topics.
type channelEntry struct {
	id      string
	name    string // channel name from connect envelope (e.g. "homeTimeline")
	channel Channel
	topics  map[string]struct{}
}

// NotificationReader marks all notifications as read for a user. Implemented
// by core/notification or similar service; nil disables readNotification.
type NotificationReader interface {
	ReadAll(userID string) error
}

// NoteVisibilityChecker gates subNote subscriptions by checking whether the
// connection's viewer is allowed to see the target note (#1460 IDOR fix)。
// 旧実装は noteID を持っていれば誰でも noteStream:<noteID> を subscribe
// でき、followers / specified note の reacted / unreacted / pollVoted /
// deleted event が漏れていた (i/notifications の #1444 IDOR の WS 版)。
//
// *core/note.QueryService が既存 RequireVisible(viewer, noteID) method で
// 本 interface を自動 satisfy する (戻り値の (*model.Note, error) と
// ErrNoteNotFound セマンティクスは存在隠蔽用)。dispatcher は note 本体は
// 使わず err 有無だけで gate するので、interface に追加メソッドは不要。
type NoteVisibilityChecker interface {
	RequireVisible(viewer *model.User, noteID string) (*model.Note, error)
}

// Dispatcher is the per-Connection state holding registered channels and the
// reverse map "topic → channel id" used to route inbound pubsub events.
//
// maxChannelsPerConnection は 1 WebSocket 接続が保持できる channel 数の上限
// (upstream Connection.ts MAX_CHANNELS_PER_CONNECTION、#1943)。超過した connect
// 要求は silent に無視する。各 channel は pubsub subscribe を伴うため resource
// exhaustion を防ぐ。
const maxChannelsPerConnection = 32

// maxTopicsPerConnection は 1 WebSocket 接続が保持できる distinct な pubsub
// トピック数の上限。
//
// **チャンネル単位の上限だけでは足りない。** ハッシュタグのチャンネルは
// 1 つで最大 32 トピックを購読でき (`maxHashtagTopics`)、チャンネルは
// 1 接続に 32 個まで作れるので、**1 本の接続から distinct な Redis 購読を
// 1024 個**開ける。`PubSubService` は distinct トピックごとに Redis へ
// 接続を張るので、`/streaming` を数本開くだけで valkey の `maxclients` に
// 届き、インスタンス全体が新規接続を拒否する。**`/streaming` は未認証で
// 張れて接続数の上限も無い。**
//
// 128 は「普通の利用者が同時に開く数」より十分大きい — upstream の
// フロントエンドが張るのは HTL / LTL / STL / GTL / 通知 / メイン +
// 開いているタブ分で、実測でも 20 を超えない。
const maxTopicsPerConnection = 128

// maxNoteSubsPerConnection は 1 接続が保持できる subNote (ノート単位の購読)
// の上限。超えたら**最も古く購読したものから外す** (LRU)。
//
// subNote も noteStream:<id> を 1 トピックとして購読するので、上限が無いと
// maxTopicsPerConnection と同じ理由で Redis の接続を無制限に増やせる。
// 公開ノートは未認証でも購読できるので、可視性の確認は歯止めにならない。
// upstream も 1 接続あたりの件数に上限を設けて古いものから外す。値が upstream
// (1536) より小さいのは、mk-go は購読 1 件ごとに Redis の接続を張るため。
// フロントエンドは画面に出したノートを購読するので、流れて見えなくなった
// ものから外れるだけで、表示中のノートの更新は届き続ける。
const maxNoteSubsPerConnection = 128

// 1 つの Connection に対して 1 つの Dispatcher がぶら下がり、複数の Channel
// を保持する。pubsub の global subscription 管理は Manager の Router (K-4)
// に集約するため、Dispatcher は per-channel の subscribe/unsubscribe API を
// 公開して Manager 側がそれを listen する形にする。
type Dispatcher struct {
	conn     *Connection
	registry *Registry
	bus      PubSubBus

	mu       sync.RWMutex
	channels map[string]*channelEntry   // channel id → entry
	topics   map[string]map[string]bool // topic → set of channel ids
	// **bus の解除は名前ではなくハンドルで行う。** 同じトピックを別の接続も
	// 購読しているので、名前で閉じると他人の購読まで止まる (#H-4)。
	busCancels map[string]*pendingCancel // topic → この Dispatcher の購読を外す枠

	// subNote の refcount 管理 (noteID → count)
	noteSubMu      sync.Mutex
	noteSubs       map[string]int
	noteCancels    map[string]*pendingCancel // noteID → 購読解除の枠
	noteOrder      *list.List                // 購読した順 (先頭が最も古い)。値は noteID
	noteElems      map[string]*list.Element  // noteID → noteOrder の要素
	notifReader    NotificationReader
	noteVisibility NoteVisibilityChecker
	ugcVisibility  UGCVisibilityLookup
}

// NewDispatcher constructs a Dispatcher for the given connection. registry /
// bus が nil の場合、対応する操作は no-op になる (テスト用)。
func NewDispatcher(conn *Connection, registry *Registry, bus PubSubBus) *Dispatcher {
	return &Dispatcher{
		conn:        conn,
		registry:    registry,
		bus:         bus,
		channels:    make(map[string]*channelEntry),
		topics:      make(map[string]map[string]bool),
		busCancels:  make(map[string]*pendingCancel),
		noteSubs:    make(map[string]int),
		noteCancels: make(map[string]*pendingCancel),
		noteOrder:   list.New(),
		noteElems:   make(map[string]*list.Element),
	}
}

// SetNotificationReader wires a NotificationReader for readNotification.
func (d *Dispatcher) SetNotificationReader(nr NotificationReader) {
	d.notifReader = nr
}

// SetNoteVisibilityChecker wires a NoteVisibilityChecker so handleSubNote
// can refuse subscribing to non-visible notes (#1460 IDOR fix)。未配線時
// は fail-closed で全 subNote が subscribe しない (= production の
// router.go は必ず wire する、test の partial setup を漏れに繋げない、
// notifications #1444 と同 doctrine)。
func (d *Dispatcher) SetNoteVisibilityChecker(c NoteVisibilityChecker) {
	d.noteVisibility = c
}

// SetUGCVisibilityLookup wires the live meta.ugcVisibilityForVisitor lookup
// used by anonymous connections (channel note gate and noteUpdated gate).
// nil disables both gates (fail-open).
func (d *Dispatcher) SetUGCVisibilityLookup(l UGCVisibilityLookup) {
	d.ugcVisibility = l
}

// ugcVisibilityForVisitor returns the current policy, or "" when unwired.
func (d *Dispatcher) ugcVisibilityForVisitor() string {
	if d.ugcVisibility == nil {
		return ""
	}
	return d.ugcVisibility()
}

// HandleClientMessage parses a Misskey-style envelope and forwards it to the
// appropriate handler. 想定する type は connect / disconnect / ch / その他。
func (d *Dispatcher) HandleClientMessage(msgType string, body json.RawMessage) {
	switch msgType {
	case "connect":
		d.handleConnect(body)
	case "disconnect":
		d.handleDisconnect(body)
	case "ch", "channel":
		// upstream Connection.ts は 'channel' と alias 'ch' を同じ
		// onChannelMessageRequested に dispatch する。official misskey-js は 'ch' を
		// 送るが、非標準クライアントの 'channel' も受ける (#1780)。
		d.handleChannelMessage(body)
	case "readNotification":
		d.handleReadNotification()
	case "subNote", "s", "sr":
		// upstream Connection.ts:152 では 'sr' は 'onSubscribeNote' (= subNote/s)
		// の単純な alias。全通知既読 (readAllNotification) は別 type 'readNotification'
		// のみが行う。'sr' で handleReadNotification を呼ぶと、frontend が note を
		// capture する度 (use-note-capture が 'sr' を送る) に未読通知が全消去される
		// バグになる (#2106 H6)。
		d.handleSubNote(body)
	case "unsubNote", "un":
		d.handleUnsubNote(body)
	}
}

// handleConnect creates a new Channel via the registry and registers it.
func (d *Dispatcher) handleConnect(body json.RawMessage) {
	var req struct {
		ID      string          `json:"id"`
		Channel string          `json:"channel"`
		Params  json.RawMessage `json:"params"`
		Pong    bool            `json:"pong"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.ID == "" || req.Channel == "" {
		return
	}
	if d.registry == nil {
		return
	}
	factory := d.registry.Lookup(req.Channel)
	if factory == nil {
		return
	}
	// requireCredential channel (hybrid/home/main/notifications/antenna/drive/
	// admin/reversi/chat 等) は anon (user==nil) 接続を弾く (upstream connectChannel
	// の requireCredential gate、silent return で connected ack も送らない、#1944)。
	if d.registry.RequiresCredential(req.Channel) && (d.conn == nil || d.conn.User() == nil) {
		return
	}
	d.mu.Lock()
	if _, exists := d.channels[req.ID]; exists {
		d.mu.Unlock()
		return
	}
	// shouldShare付きチャンネルで既に同名が接続済みなら新規作成しない
	if d.hasShareableChannelLocked(req.Channel) {
		d.mu.Unlock()
		// #2106 L58: upstream Connection.ts は既接続 shareable channel への重複 connect を
		// pong 有無に関わらず silent に return する (connected ack を送らない)。
		return
	}
	// 1 接続あたりの channel 数上限 (upstream MAX_CHANNELS_PER_CONNECTION=32、#1943)。
	// 超過した connect 要求は silent に無視する (upstream connectChannel と同じ)。
	// 既存 id / shareable hit は上で return 済なので、新規 channel 追加のみを制限する。
	if len(d.channels) >= maxChannelsPerConnection {
		d.mu.Unlock()
		return
	}
	entry := &channelEntry{id: req.ID, name: req.Channel, topics: map[string]struct{}{}}
	d.channels[req.ID] = entry
	d.mu.Unlock()

	ctx := &channelContext{dispatcher: d, id: req.ID}
	ch := factory(ctx)

	// OAuth2 スコープチェック。ch が PermittedChannel を実装していれば
	// Connection のトークン permission 配列と照合する。
	if !d.checkPermission(ch) {
		d.mu.Lock()
		delete(d.channels, req.ID)
		d.mu.Unlock()
		return
	}

	entry.channel = ch
	if err := ch.Init(req.Params); err != nil {
		// TS Misskey の init() が false を返したときと同じ挙動: ロールバックし
		// pong ack も送らず、クライアントには通知しない (silent drop)。
		// Init 中に Subscribe 済みの可能性があるため removeChannel で topic の
		// refcount を整理する。
		d.removeChannel(req.ID)
		return
	}

	d.sendConnectedIfRequested(req.ID, req.Pong)
}

// checkPermission verifies that the connection's token carries the scope
// required by ch. Returns true when ch does not require any permission or
// when the scope is present.
func (d *Dispatcher) checkPermission(ch Channel) bool {
	pc, ok := ch.(PermittedChannel)
	if !ok {
		return true
	}
	kind := pc.RequiredPermission()
	if kind == "" {
		return true
	}
	if d.conn == nil {
		return false
	}
	return d.conn.HasPermission(kind)
}

// hasShareableChannelLocked checks if a shareable channel with the same name
// already exists. mu must be held by the caller.
func (d *Dispatcher) hasShareableChannelLocked(name string) bool {
	for _, e := range d.channels {
		if e.name != name || e.channel == nil {
			continue
		}
		if sc, ok := e.channel.(ShareableChannel); ok && sc.ShouldShare() {
			return true
		}
	}
	return false
}

// sendConnectedIfRequested emits a `connected` envelope when the client
// requested confirmation via pong=true in connect.
func (d *Dispatcher) sendConnectedIfRequested(id string, pong bool) {
	if !pong || d.conn == nil {
		return
	}
	_ = d.conn.Send(map[string]any{
		"type": "connected",
		"body": map[string]any{"id": id},
	})
}

// handleDisconnect tears down a previously-connected channel.
func (d *Dispatcher) handleDisconnect(body json.RawMessage) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.ID == "" {
		return
	}
	d.removeChannel(req.ID)
}

// handleChannelMessage forwards a `ch` envelope to the matching Channel.
func (d *Dispatcher) handleChannelMessage(body json.RawMessage) {
	var req struct {
		ID   string          `json:"id"`
		Type string          `json:"type"`
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.ID == "" {
		return
	}
	d.mu.RLock()
	entry, ok := d.channels[req.ID]
	d.mu.RUnlock()
	if !ok {
		return
	}
	entry.channel.OnClientMessage(req.Type, req.Body)
}

// removeChannel disposes of a channel entry and unsubscribes from any topics
// it owned (with reference counting).
func (d *Dispatcher) removeChannel(id string) {
	d.mu.Lock()
	entry, ok := d.channels[id]
	if !ok {
		d.mu.Unlock()
		return
	}
	delete(d.channels, id)
	topics := make([]string, 0, len(entry.topics))
	for t := range entry.topics {
		topics = append(topics, t)
		set := d.topics[t]
		delete(set, id)
		if len(set) == 0 {
			delete(d.topics, t)
		}
	}
	d.mu.Unlock()

	for _, t := range topics {
		// このトピックが他の channel からも参照されていなければ pubsub
		// 解除を実行する。
		d.mu.RLock()
		stillUsed := len(d.topics[t]) > 0
		d.mu.RUnlock()
		if !stillUsed {
			d.cancelTopic(t)
		}
	}
	if entry.channel != nil {
		entry.channel.Dispose()
	}
}

// CloseAll tears down every registered channel and noteStream subscription.
// Manager が connection close 時に呼ぶ。
func (d *Dispatcher) CloseAll() {
	d.mu.Lock()
	ids := make([]string, 0, len(d.channels))
	for id := range d.channels {
		ids = append(ids, id)
	}
	d.mu.Unlock()
	for _, id := range ids {
		d.removeChannel(id)
	}
	// noteStream の Redis subscription もクリーンアップ
	d.noteSubMu.Lock()
	cancels := make([]func(), 0, len(d.noteCancels))
	// **全ての枠に印を残す。** `noteSubs` に無い枠 (購読の確立中) も含めて
	// 回らないと、直後に埋まったハンドルが残る。
	for _, slot := range d.noteCancels {
		if c := slot.take(); c != nil {
			cancels = append(cancels, c)
		}
	}
	d.noteSubs = make(map[string]int)
	d.noteCancels = make(map[string]*pendingCancel)
	d.noteOrder = list.New()
	d.noteElems = make(map[string]*list.Element)
	d.noteSubMu.Unlock()
	for _, c := range cancels {
		c()
	}
}

// pendingCancel is the slot that holds the unsubscribe handle for one topic
// while the subscription is being established.
//
// **枠をロック内で先に置く。** `bus.Subscribe` は Redis へ dial するので
// ロックの外で呼ぶしかないが、その隙間に解除 (`CloseAll` / `unsubscribe`) が
// 走ると、**まだ空のマップを見て「解除するものは無い」と判断し、直後に
// 書き込まれたハンドルが誰にも呼ばれなくなる** — ハンドラと Redis 購読が
// プロセス寿命まで残る。`CloseAll` は読み取りループ以外の goroutine
// (送信キュー満杯・書き込み失敗・`Manager.Shutdown`) からも呼ばれるので、
// 読み取り側だけを見ても防げない。
type pendingCancel struct {
	cancel   func()
	canceled bool
}

// resolve stores the handle, or invokes it immediately when the slot was
// already canceled while the subscription was being established.
// 呼び出しはロックの外で行うこと (cancel は pubsub 側のロックを取る)。
func (p *pendingCancel) resolve(cancel func(), mu sync.Locker) {
	mu.Lock()
	if p.canceled {
		mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	}
	p.cancel = cancel
	mu.Unlock()
}

// take marks the slot canceled and returns the handle to invoke, if it has
// already arrived. 呼び出し側はロックを保持していること。
func (p *pendingCancel) take() func() {
	if p == nil {
		return nil
	}
	p.canceled = true
	c := p.cancel
	p.cancel = nil
	return c
}

// subscribe registers a topic for a channel id and ensures the bus is listening.
func (d *Dispatcher) subscribe(channelID, topic string) {
	d.mu.Lock()
	entry, ok := d.channels[channelID]
	if !ok {
		d.mu.Unlock()
		return
	}
	if _, dup := entry.topics[topic]; dup {
		d.mu.Unlock()
		return
	}
	entry.topics[topic] = struct{}{}
	set, exists := d.topics[topic]
	if !exists {
		set = map[string]bool{}
		d.topics[topic] = set
	}
	// **接続あたりの上限。** チャンネル単位の上限だけでは、ハッシュタグの
	// ように 1 チャンネルで多数のトピックを購読するものを積まれると
	// `maxChannelsPerConnection` 倍まで増える。
	if !exists && len(d.topics) > maxTopicsPerConnection {
		delete(d.topics, topic)
		delete(entry.topics, topic)
		d.mu.Unlock()
		return
	}
	set[channelID] = true
	first := !exists
	var slot *pendingCancel
	if first && d.bus != nil {
		slot = &pendingCancel{}
		d.busCancels[topic] = slot
	}
	d.mu.Unlock()

	if slot != nil {
		cancel := d.bus.Subscribe(topic, func(payload []byte) { d.fanout(topic, payload) })
		slot.resolve(cancel, &d.mu)
	}
}

// unsubscribe undoes subscribe for a single channel id.
func (d *Dispatcher) unsubscribe(channelID, topic string) {
	d.mu.Lock()
	entry, ok := d.channels[channelID]
	if !ok {
		d.mu.Unlock()
		return
	}
	if _, present := entry.topics[topic]; !present {
		d.mu.Unlock()
		return
	}
	delete(entry.topics, topic)
	set := d.topics[topic]
	delete(set, channelID)
	last := len(set) == 0
	if last {
		delete(d.topics, topic)
	}
	d.mu.Unlock()

	if last {
		d.cancelTopic(topic)
	}
}

// cancelTopic removes this dispatcher's bus handler for topic, if any.
//
// **解除はロックの外で呼ぶ。** cancel は pubsub 側のロックを取るので、
// Dispatcher のロックを持ったまま呼ぶとロック順序が交差する。
func (d *Dispatcher) cancelTopic(topic string) {
	d.mu.Lock()
	cancel := d.busCancels[topic].take()
	delete(d.busCancels, topic)
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// fanout dispatches a pubsub payload to every channel subscribed to topic.
func (d *Dispatcher) fanout(topic string, payload []byte) {
	d.mu.RLock()
	ids := make([]string, 0, len(d.topics[topic]))
	for id := range d.topics[topic] {
		ids = append(ids, id)
	}
	channels := make([]Channel, 0, len(ids))
	for _, id := range ids {
		if entry, ok := d.channels[id]; ok {
			channels = append(channels, entry.channel)
		}
	}
	d.mu.RUnlock()

	for _, ch := range channels {
		if ch != nil {
			ch.OnRedisEvent(payload)
		}
	}
}

// channelContext is the per-channel facade implementing ChannelContext. それ
// 自身は薄く、実体は Dispatcher 経由で Connection に橋渡しする。
type channelContext struct {
	dispatcher *Dispatcher
	id         string
}

func (c *channelContext) ID() string { return c.id }
func (c *channelContext) User() any {
	if c.dispatcher.conn == nil {
		return nil
	}
	return c.dispatcher.conn.User()
}

// Send wraps the payload in a Misskey "channel" envelope and queues it on the
// underlying Connection.
func (c *channelContext) Send(msgType string, body any) error {
	if c.dispatcher.conn == nil {
		return nil
	}
	return c.dispatcher.conn.Send(map[string]any{
		"type": "channel",
		"body": map[string]any{
			"id":   c.id,
			"type": msgType,
			"body": body,
		},
	})
}

func (c *channelContext) Subscribe(topic string)   { c.dispatcher.subscribe(c.id, topic) }
func (c *channelContext) Unsubscribe(topic string) { c.dispatcher.unsubscribe(c.id, topic) }

// HardMuteRules forwards the connection-level hardMutedWords rule set so
// timeline channels can drop matching notes before sending (#787).
func (c *channelContext) HardMuteRules() []byte {
	if c.dispatcher.conn == nil {
		return nil
	}
	return c.dispatcher.conn.HardMuteRules()
}

// FollowingSnapshot forwards the connection-level followee map so timeline
// channels can gate reply notes per upstream Misskey の
// `this.following[note.userId]?.withReplies` semantics (#1063).
func (c *channelContext) FollowingSnapshot() map[string]bool {
	if c.dispatcher.conn == nil {
		return nil
	}
	return c.dispatcher.conn.FollowingSnapshot()
}

// UpdateFollowingSnapshot forwards a live follow/unfollow snapshot update to the
// connection so reply gating stays fresh without a reconnect (#1211).
func (c *channelContext) UpdateFollowingSnapshot(followeeID string, following bool) {
	if c.dispatcher.conn == nil {
		return
	}
	c.dispatcher.conn.UpdateFollowingSnapshot(followeeID, following)
}

// MuteBlockSnapshot forwards the connection-level mute/block snapshot so the
// main / notifications channels (#1711) and every timeline channel (#1812) can
// gate delivery of muted / blocked / muted-instance / renote-muted /
// channel-muted notes.
func (c *channelContext) MuteBlockSnapshot() *MuteBlockSnapshot {
	if c.dispatcher.conn == nil {
		return nil
	}
	return c.dispatcher.conn.MuteBlockSnapshot()
}

func (c *channelContext) UserPolicies() map[string]any {
	if c.dispatcher.conn == nil {
		return nil
	}
	return c.dispatcher.conn.Policies()
}

// UGCVisibilityForVisitor forwards the live meta.ugcVisibilityForVisitor so
// note channels can apply the visitor policy to anonymous viewers.
func (c *channelContext) UGCVisibilityForVisitor() string {
	return c.dispatcher.ugcVisibilityForVisitor()
}

// --- readNotification / subNote / unsubNote ---

// handleReadNotification marks all notifications as read for the connected user.
func (d *Dispatcher) handleReadNotification() {
	if d.notifReader == nil || d.conn == nil || d.conn.User() == nil {
		return
	}
	if err := d.notifReader.ReadAll(d.conn.User().ID); err != nil {
		slog.Warn("stream: readNotification failed", "err", err)
	}
}

// handleSubNote subscribes to note-level events (reactions, deletions, etc.)
// for a specific note. TS互換: refcount で同一noteIDの重複subscribeを管理。
func (d *Dispatcher) handleSubNote(body json.RawMessage) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.ID == "" {
		return
	}
	// Visibility gate (#1460 IDOR fix): 任意の noteID で subscribe させると
	// followers / specified note の reacted / unreacted / pollVoted / deleted
	// event を非対象 viewer に漏らす。HTTP 側 #1444 と同じ shape で、
	// RequireVisible が ErrNoteNotFound (= 不存在 / 非可視を hide) を返したら
	// subscribe を silently skip する。refcount inc の前で gate するので、
	// 拒否されたあとに unsubNote で state を leak する経路も発生しない。
	// 匿名 conn (User()==nil) は CanSeeNote の nil-viewer 判定で followers /
	// specified が自動的に弾かれる。checker 未配線時は fail-closed (= 同じく
	// silently skip)。
	if d.noteVisibility == nil {
		return
	}
	var viewer *model.User
	if d.conn != nil {
		viewer = d.conn.User()
	}
	if _, err := d.noteVisibility.RequireVisible(viewer, req.ID); err != nil {
		return
	}
	topic := "noteStream:" + req.ID
	d.noteSubMu.Lock()
	// 新しいノートで上限に達していたら、最も古く購読したものを外す。
	var evicted func()
	if _, ok := d.noteSubs[req.ID]; !ok && len(d.noteSubs) >= maxNoteSubsPerConnection {
		evicted = d.evictOldestNoteSubLocked()
	}
	d.noteSubs[req.ID]++
	if e, ok := d.noteElems[req.ID]; ok {
		d.noteOrder.MoveToBack(e)
	} else {
		d.noteElems[req.ID] = d.noteOrder.PushBack(req.ID)
	}
	var slot *pendingCancel
	if d.noteSubs[req.ID] == 1 && d.bus != nil {
		slot = &pendingCancel{}
		d.noteCancels[req.ID] = slot
	}
	d.noteSubMu.Unlock()
	if evicted != nil {
		evicted()
	}

	if slot != nil {
		cancel := d.bus.Subscribe(topic, func(payload []byte) {
			d.forwardNoteEvent(req.ID, payload)
		})
		slot.resolve(cancel, &d.noteSubMu)
	}
}

// evictOldestNoteSubLocked drops the least recently subscribed note and
// returns its bus cancel (nil if none), to be called after unlocking.
// Caller must hold noteSubMu.
func (d *Dispatcher) evictOldestNoteSubLocked() func() {
	front := d.noteOrder.Front()
	if front == nil {
		return nil
	}
	id := front.Value.(string)
	d.noteOrder.Remove(front)
	delete(d.noteElems, id)
	delete(d.noteSubs, id)
	// 購読の確立中 (resolve 前) の枠でも take が印を残すので、後から埋まった
	// ハンドルはその場で外れる (CloseAll と同じ)。
	cancel := d.noteCancels[id].take()
	delete(d.noteCancels, id)
	return cancel
}

// handleUnsubNote decrements the refcount for a note subscription and
// unsubscribes when it reaches zero.
func (d *Dispatcher) handleUnsubNote(body json.RawMessage) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.ID == "" {
		return
	}
	d.noteSubMu.Lock()
	count, ok := d.noteSubs[req.ID]
	if !ok {
		d.noteSubMu.Unlock()
		return
	}
	count--
	if count <= 0 {
		delete(d.noteSubs, req.ID)
		if e, ok := d.noteElems[req.ID]; ok {
			d.noteOrder.Remove(e)
			delete(d.noteElems, req.ID)
		}
		cancel := d.noteCancels[req.ID].take()
		delete(d.noteCancels, req.ID)
		d.noteSubMu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	}
	d.noteSubs[req.ID] = count
	d.noteSubMu.Unlock()
}

// forwardNoteEvent sends a noteStream event directly to the connection
// (not wrapped in a channel envelope). TS互換のトップレベル noteUpdated 形式。
// Redis payload は {type, body} envelope で届く (reacted, deleted, pollVoted 等)。
func (d *Dispatcher) forwardNoteEvent(noteID string, payload []byte) {
	if d.conn == nil {
		return
	}
	var env struct {
		Type string          `json:"type"`
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(payload, &env); err != nil || env.Type == "" {
		return
	}
	// upstream Connection.onNoteStreamMessage と同じく、未ログインの接続には
	// ugcVisibilityForVisitor が none のとき noteUpdated を一切送らない。
	// local のときの扱いは upstream も TODO のまま (none だけ見る) なので揃える。
	if d.conn.User() == nil && ugcvisibility.HidesAll(d.ugcVisibilityForVisitor()) {
		return
	}
	_ = d.conn.Send(map[string]any{
		"type": "noteUpdated",
		"body": map[string]any{
			"id":   noteID,
			"type": env.Type,
			"body": env.Body,
		},
	})
}
