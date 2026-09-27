// Package stream implements the per-connection state for the WebSocket
// streaming endpoint and the Manager that distributes pubsub messages to
// connected clients.
package stream

import (
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shiroha-a/mk/internal/model"
)

// Conn is the subset of *websocket.Conn that Connection actually uses. テスト
// 時に net.Conn を完全にスタブ化するため interface 化している。
type Conn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	WriteControl(messageType int, data []byte, deadline time.Time) error
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
	// SetReadLimit caps a single inbound frame. gorilla の既定は 0 = 無制限で、
	// 未認証の接続 1 本で任意量のメモリを確保させられる。
	SetReadLimit(limit int64)
	SetPongHandler(h func(appData string) error)
	Close() error
}

// defaultPingInterval は ping/pong による keepalive のデフォルト間隔。
const defaultPingInterval = 30 * time.Second

// readDeadline は Pong 待ちの猶予 (= pingInterval + 余白)。
const readDeadline = 60 * time.Second

// sendQueueSize はクライアントごとの送信バッファ長。これを超えると古いメッセージ
// は破棄され接続は閉じられる。
const sendQueueSize = 64

// MessageHandler is the callback invoked for each parsed client → server
// message. msgType / body は Misskey 風のワイヤプロトコル (e.g. "connect" /
// "disconnect"). Connection 側は dispatch せず、上位の Channel framework に
// 委譲する。
type MessageHandler func(msgType string, body json.RawMessage)

// CloseHandler is invoked once when the connection is fully torn down so that
// the Manager can release per-connection resources (channels, subscriptions).
type CloseHandler func()

// Connection wraps a single WebSocket client and provides thread-safe send
// semantics with a bounded outbound queue. すべての送信は writer goroutine 経由
// で行われるため、複数 goroutine から Send を呼び出してもデッドロックしない。
type Connection struct {
	id           string
	user         *model.User
	permissions  []string
	conn         Conn
	send         chan []byte
	closeC       chan struct{}
	pingInterval time.Duration

	closeOnce sync.Once
	closed    bool
	closedMu  sync.Mutex

	// credential は接続の認証に使った資格情報の鍵 (credkey.Native /
	// credkey.AccessToken)。匿名接続は ""。失効時に「その資格情報で張られた
	// 接続だけ」を閉じるために持つ。
	credential string

	// drainC は CloseWithCode が writeLoop へ「送信キューを吐き切ってから close
	// frame を送って閉じろ」と伝えるための合図。drainOnce で 1 度だけ close する。
	drainC     chan struct{}
	drainOnce  sync.Once
	closeFrame []byte

	handler      MessageHandler
	closeHandler CloseHandler

	// hardMuteRules は viewer の user_profile.hardMutedWords をそのまま保持する
	// jsonb 由来 byte slice (#787)。streaming channel が per-note dispatch 時に
	// notesfilter.MatchOne でこの rules と照合し、match したら send しない。
	// 認証時に router が一度だけ fetch、また i/update 経由で reload 通知
	// (#791) が来たときに更新される。read (per-publish) と write (reload) が
	// 並行するので hardMuteMu で protect する。
	hardMuteMu    sync.RWMutex
	hardMuteRules []byte

	// followingSnapshot は viewer の followee 一覧と各 followee の withReplies
	// 設定を保持する map (#1063)。HomeTimelineChannel / HybridTimelineChannel /
	// LocalTimelineChannel が reply note の表示可否を決めるときに upstream の
	// `this.following[note.userId]?.withReplies` 相当の参照を行う。snapshot は
	// 接続確立時に 1 回 fetch する。Following 変更時に refresh する subscriber
	// は将来 #1063 follow-up で導入する想定だが、現状の接続単位の精度でも
	// "自分の reply が流れない" / "self-thread が流れない" / "reply-to-me が
	// 流れない" の 3 escape hatch には影響しないため drop-in 互換性回復は
	// 完了する。read (per-publish) と write (refresh) は followingMu で protect。
	followingMu       sync.RWMutex
	followingSnapshot map[string]bool

	// muteBlock は viewer の mute/block snapshot (#1711)。main / notifications
	// channel が notification / mention 配信時に instance-mute / block / mute を
	// gate するのに使う。接続確立時に 1 回 fetch する。followingSnapshot と同じく
	// read (per-publish) と write (set) が並行するので muteBlockMu で protect。
	muteBlockMu sync.RWMutex
	muteBlock   *MuteBlockSnapshot

	// policies は viewer の role policy snapshot (#1942)。timeline channel が
	// init() で ltlAvailable / gtlAvailable を gate するのに使う。接続確立時に
	// 1 回 fetch する (anon は base policy)。set-once だが Manager goroutine で
	// 書き接続後の read-loop goroutine で読むため mutex で happens-before を張る。
	// nil = provider 未配線 (test/旧挙動) → gate skip (fail-open)。
	policiesMu sync.RWMutex
	policies   map[string]any
}

// NewConnection wraps an upgraded WebSocket. id は呼び出し側 (Manager) が一意
// に割り当てる。MessageHandler が nil の場合、受信メッセージは破棄される。
func NewConnection(id string, user *model.User, conn Conn) *Connection {
	return &Connection{
		id:           id,
		user:         user,
		conn:         conn,
		send:         make(chan []byte, sendQueueSize),
		closeC:       make(chan struct{}),
		drainC:       make(chan struct{}),
		pingInterval: defaultPingInterval,
	}
}

// SetPingInterval overrides the keepalive ping cadence. 主にテストで時計を
// 縮めるための setter。0 以下は無視される。Start を呼ぶ前に設定すること。
func (c *Connection) SetPingInterval(d time.Duration) {
	if d > 0 {
		c.pingInterval = d
	}
}

// ID returns the connection identifier assigned by the Manager.
func (c *Connection) ID() string { return c.id }

// SetCredential records the key of the credential this connection was
// authenticated with (see internal/misc/credkey). Must be called before the
// connection is registered with the Manager.
func (c *Connection) SetCredential(key string) { c.credential = key }

// Credential returns the key set by SetCredential ("" for anonymous).
func (c *Connection) Credential() string { return c.credential }

// User returns the authenticated user, or nil for anonymous connections.
func (c *Connection) User() *model.User { return c.user }

// SetHardMuteRules attaches the viewer's persisted hardMutedWords (raw jsonb
// from user_profile) so timeline channels can drop matching notes before
// sending them to the client (#787). Called once after authentication and
// then again whenever a wordmute reload event for this user arrives via
// pubsub (#791) so changes propagate without forcing the client to reconnect.
func (c *Connection) SetHardMuteRules(rules []byte) {
	c.hardMuteMu.Lock()
	c.hardMuteRules = rules
	c.hardMuteMu.Unlock()
}

// HardMuteRules returns the currently active hardMutedWords rules. Safe for
// concurrent read while SetHardMuteRules updates the value (#791).
// Returns nil for anonymous connections / when the lookup failed / when the
// user has no rule set.
//
// 戻り値は internal slice の参照 — caller は **絶対に mutate しないこと**。
// per-publish hot path で defensive copy を避けるための約束で、現状の唯一の
// caller (notesfilter.MatchOne) は read-only で扱う。
func (c *Connection) HardMuteRules() []byte {
	c.hardMuteMu.RLock()
	defer c.hardMuteMu.RUnlock()
	return c.hardMuteRules
}

// SetFollowingSnapshot attaches the viewer's followee map (followeeID →
// withReplies) so timeline channels can apply upstream Misskey の
// `this.following[note.userId]?.withReplies` 相当の reply gate (#1063)。
// 接続確立時に router の lookup が呼ばれ、その結果をここで保持する。nil を
// 渡すと "follow 情報なし" 扱いになり escape hatch (= 自分の reply / reply
// to me / self-thread) のみで reply が通る upstream default 挙動に degrade
// する。
//
// 並行安全: 本 method は内部 map を直接 mutate せず、**新しい map を全置換**
// する設計。FollowingSnapshot() で返した古い map は channel.OnRedisEvent が
// 読み込み中でも安全に GC される (Go の map read は同じ map に対する
// concurrent write が無ければ thread-safe)。**将来 refresh subscriber
// (#791 と同等) を実装するときも必ず「新しい map を作って Set する」パターン
// を守ること** — 既存 map を mutate すると in-flight な reader と race する。
func (c *Connection) SetFollowingSnapshot(snap map[string]bool) {
	c.followingMu.Lock()
	c.followingSnapshot = snap
	c.followingMu.Unlock()
}

// FollowingSnapshot returns the viewer's followee map. Safe for concurrent
// read while SetFollowingSnapshot updates the value. Returns nil for
// anonymous connections / when the lookup is unwired or failed.
//
// 戻り値は internal map の参照。caller は **mutate しないこと** — read-only
// で扱う前提で defensive copy を省略している。
func (c *Connection) FollowingSnapshot() map[string]bool {
	c.followingMu.RLock()
	defer c.followingMu.RUnlock()
	return c.followingSnapshot
}

// SetPolicies attaches the viewer's role-policy snapshot (#1942). Called once
// at connect time by the Manager. nil leaves timeline policy gating disabled
// (fail-open).
func (c *Connection) SetPolicies(p map[string]any) {
	c.policiesMu.Lock()
	c.policies = p
	c.policiesMu.Unlock()
}

// Policies returns the viewer's role-policy snapshot, or nil when the policy
// provider is unwired. Read-only; callers must not mutate.
func (c *Connection) Policies() map[string]any {
	c.policiesMu.RLock()
	defer c.policiesMu.RUnlock()
	return c.policies
}

// UpdateFollowingSnapshot mutates the live followee snapshot after a
// follow/unfollow so reply gating stays fresh without a reconnect (#1211).
// It is copy-on-write: a new map is built and swapped under the lock, leaving
// any map already handed out via FollowingSnapshot() immutable for concurrent
// readers (timeline channels iterate it outside the lock). A nil snapshot
// (anonymous / lookup unwired) is left untouched — those connections gate on
// the escape hatches only and never consult the snapshot.
func (c *Connection) UpdateFollowingSnapshot(followeeID string, following bool) {
	if followeeID == "" {
		return
	}
	c.followingMu.Lock()
	defer c.followingMu.Unlock()
	if c.followingSnapshot == nil {
		return
	}
	next := make(map[string]bool, len(c.followingSnapshot)+1)
	for k, v := range c.followingSnapshot {
		next[k] = v
	}
	if following {
		// 新規 follow は withReplies=false (reply 表示は後から opt-in)。再 follow で
		// 既存エントリがある場合は withReplies 設定を維持する。
		if _, exists := next[followeeID]; !exists {
			next[followeeID] = false
		}
	} else {
		delete(next, followeeID)
	}
	c.followingSnapshot = next
}

// SetMuteBlockSnapshot attaches the viewer's mute/block snapshot so the main /
// notifications channels (#1711) and the timeline channels (#1812) can drop
// notes / notifications / mentions involving muted instances, muted users,
// users blocking the viewer, renote-muted users, or muted channels. Called once
// at connection setup. nil leaves the gate disabled (fail-open: nothing dropped),
// matching upstream の「Set が空 = 全通過」default。並行安全: 内部 pointer を
// 全置換するだけで snapshot 内 map は mutate しない (reader は古い snapshot を
// そのまま読み続けて GC される)。
func (c *Connection) SetMuteBlockSnapshot(snap *MuteBlockSnapshot) {
	c.muteBlockMu.Lock()
	c.muteBlock = snap
	c.muteBlockMu.Unlock()
}

// MuteBlockSnapshot returns the viewer's mute/block snapshot. Safe for
// concurrent read while SetMuteBlockSnapshot updates the value. Returns nil for
// anonymous connections / when the lookup is unwired or failed — callers must
// treat nil as "no filtering" (fail-open).
//
// 戻り値は internal pointer / map の参照。caller は **mutate しないこと**。
func (c *Connection) MuteBlockSnapshot() *MuteBlockSnapshot {
	c.muteBlockMu.RLock()
	defer c.muteBlockMu.RUnlock()
	return c.muteBlock
}

// SetPermissions attaches OAuth2 permission scopes for this connection.
// トークン経由で接続された場合に、AccessToken の permission 配列を渡す想定。
// cookie/session 認証の場合は呼び出さない（空の permissions は権限ありとも無しとも区別できないため、
// チャンネル側の要件チェックに委ねる）。
func (c *Connection) SetPermissions(perms []string) {
	c.permissions = perms
}

// Permissions returns the OAuth2 permission scopes attached to this connection.
func (c *Connection) Permissions() []string {
	return c.permissions
}

// HasPermission reports whether the connection is allowed to access the
// given scope. session/cookie 認証ではトークン permission が無いため、
// nil permissions は「制限なし (フルアクセス)」として扱う。
// 空スライス ([]) は明示的にスコープなしのトークンを意味する。
func (c *Connection) HasPermission(kind string) bool {
	if kind == "" {
		return true
	}
	if c.permissions == nil {
		return true
	}
	return slices.Contains(c.permissions, kind)
}

// SetMessageHandler installs the callback invoked for each inbound client
// message. Must be called before Start.
func (c *Connection) SetMessageHandler(h MessageHandler) {
	c.handler = h
}

// SetCloseHandler installs a callback invoked once when the connection is
// fully closed.
func (c *Connection) SetCloseHandler(h CloseHandler) {
	c.closeHandler = h
}

// Start launches the read / write goroutines. Start blocks until the
// connection is closed; callers typically run it in a dedicated goroutine.
func (c *Connection) Start() {
	go c.writeLoop()
	c.readLoop()
}

// Send queues a JSON-encoded payload for delivery to the client. Returns an
// error if the connection is already closed or the queue is full.
func (c *Connection) Send(payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	c.closedMu.Lock()
	if c.closed {
		c.closedMu.Unlock()
		return errors.New("connection closed")
	}
	select {
	case c.send <- body:
		c.closedMu.Unlock()
		return nil
	default:
		// バッファが溢れたら接続を切断する。クライアントが追いついていない or
		// ネットワークが詰まっているケースで、これ以上保持しても遅延が悪化する
		// だけなので。closedMu は closeInternal が内部で取り直すので先に release。
		c.closedMu.Unlock()
		c.closeInternal()
		return errors.New("send queue full")
	}
}

// Close terminates the connection. 二重 close は安全。
func (c *Connection) Close() {
	c.closeInternal()
}

// closeFlushTimeout bounds how long CloseWithCode waits for the writer to
// flush before tearing the socket down unconditionally.
var closeFlushTimeout = 5 * time.Second

// CloseWithCode stops accepting new payloads, flushes the ones already queued,
// sends a WebSocket close frame with code / reason and then tears the
// connection down. 二重呼び出しや Close 後の呼び出しは no-op。
//
// **キュー済みのものは送り切る。** 失効の直前に publish された event
// (i/regenerate-token の myTokenRegenerated) が、閉じる判断より先に届いて
// いればクライアントまで届けるため。逆に閉じると決めた後の Send は拒否する
// ので、失効後に新しい event が流れることは無い。
func (c *Connection) CloseWithCode(code int, reason string) {
	c.closedMu.Lock()
	if c.closed {
		c.closedMu.Unlock()
		return
	}
	// closed を立てるのは Send と同じ lock の中。以後 Send は enqueue しないので、
	// writeLoop が吐き切るべき要素数は有限に確定する。
	c.closed = true
	c.closeFrame = websocket.FormatCloseMessage(code, reason)
	c.closedMu.Unlock()
	c.drainOnce.Do(func() { close(c.drainC) })
	// writeLoop が動いていない (Start 前) / 書き込みで詰まっている場合でも
	// 必ず閉じる。closeInternal は冪等なので、先に閉じ終わっていれば no-op。
	time.AfterFunc(closeFlushTimeout, c.closeInternal)
}

// flushAndClose drains the queued payloads, writes the close frame and closes
// the connection. writeLoop 上でだけ呼ぶ (書き込みは writer goroutine に限る)。
func (c *Connection) flushAndClose() {
	defer c.closeInternal()
	// 吐き切る間の書き込みに期限を置く。相手が読まないと WriteMessage は
	// 送信バッファが空くまで戻らないので、CloseWithCode の保険 (closeFlushTimeout
	// 後の closeInternal) に頼らず、writer 自身がそこで諦めて閉じる。
	_ = c.conn.SetWriteDeadline(time.Now().Add(closeFlushTimeout))
	for {
		select {
		case body := <-c.send:
			if err := c.conn.WriteMessage(websocket.TextMessage, body); err != nil {
				return
			}
		default:
			c.closedMu.Lock()
			frame := c.closeFrame
			c.closedMu.Unlock()
			_ = c.conn.WriteControl(websocket.CloseMessage, frame, time.Now().Add(time.Second))
			return
		}
	}
}

// isClosing reports whether the connection has been marked closed (by
// CloseWithCode or closeInternal).
func (c *Connection) isClosing() bool {
	c.closedMu.Lock()
	defer c.closedMu.Unlock()
	return c.closed
}

// closeInternal performs the actual close + cleanup logic. closeOnce で保護
// されているため繰り返し呼ばれても 1 回しか実行されない。
func (c *Connection) closeInternal() {
	c.closeOnce.Do(func() {
		c.closedMu.Lock()
		c.closed = true
		c.closedMu.Unlock()
		close(c.closeC)
		_ = c.conn.Close()
		if c.closeHandler != nil {
			c.closeHandler()
		}
	})
}

// maxIncomingFrameBytes caps a single inbound WebSocket frame.
// API の body limit (1MiB) と同値。
const maxIncomingFrameBytes = 1 << 20

// readLoop drives ReadMessage in a loop, dispatching parsed payloads to the
// installed MessageHandler. ループ終了時には writer も巻き込んで全停止する。
func (c *Connection) readLoop() {
	defer c.closeInternal()
	// **受信フレームに上限を置く。**
	//
	// gorilla の既定 `readLimit` は 0 = 無制限で、`ReadMessage` はフレーム全体を
	// 成長するバッファへ載せる。Echo の body limit は upgrade 後のフレームには
	// 効かないので、未認証の接続 1 本で任意量のメモリを確保させられる。
	// upstream (`ws`) は既定 `maxPayload` 100MiB で 1009 を返して閉じる。
	// ストリーミングのメッセージは実際には数 KB なので、こちらは API の
	// body limit と同じ 1MiB にする (upstream より厳しい = 安全側)。
	c.conn.SetReadLimit(maxIncomingFrameBytes)
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(readDeadline))
	})
	_ = c.conn.SetReadDeadline(time.Now().Add(readDeadline))
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		// **閉じると決めた後はクライアントのメッセージを処理しない。**
		// CloseWithCode から実際に閉じるまで (送信キューを吐き切る間、最長
		// closeFlushTimeout) も読み続けるが、その間に connect / subNote 等を
		// 受け付けると、失効した資格情報で新しい購読を張れてしまう。読むこと自体は
		// やめない — ここで return すると defer の closeInternal が吐き切りを
		// 待たずに接続を切る。
		if c.isClosing() {
			continue
		}
		var env struct {
			Type string          `json:"type"`
			Body json.RawMessage `json:"body"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			slog.Debug("stream: invalid client message", "err", err)
			continue
		}
		if c.handler != nil {
			c.handler(env.Type, env.Body)
		}
	}
}

// writeLoop drains the send queue and emits ping frames at pingInterval.
func (c *Connection) writeLoop() {
	ping := time.NewTicker(c.pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-c.closeC:
			return
		case <-c.drainC:
			c.flushAndClose()
			return
		case body := <-c.send:
			if err := c.conn.WriteMessage(websocket.TextMessage, body); err != nil {
				c.closeInternal()
				return
			}
		case <-ping.C:
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)); err != nil {
				c.closeInternal()
				return
			}
		}
	}
}
