package stream

import (
	"encoding/json"
	"errors"
	"sync"
)

// ErrInvalidParams indicates that a Channel.Init call received invalid or
// missing parameters and cannot proceed. Dispatcher は Init がこのエラー
// (または任意の non-nil error) を返した場合、channel 登録をロールバックし
// pong ack も送らない。TS Misskey の init() が false を返したときの挙動に
// 相当する。
var ErrInvalidParams = errors.New("stream: invalid channel params")

// Channel is the per-subscription state for one streaming channel attached to
// a single Connection. Implementations are constructed by a ChannelFactory
// when the client sends a `connect` envelope and torn down on `disconnect` or
// connection close.
type Channel interface {
	// Init is called once after the channel is registered. params は client が
	// connect で渡した任意 JSON で、channel 実装ごとに解釈する。非 nil の
	// error を返すと Dispatcher は channel 登録をロールバックし、クライアント
	// には (TS 互換の silent drop 方針に従い) 何も送らない。必須パラメータ
	// 欠如などの典型的な拒否には ErrInvalidParams を返す。
	Init(params json.RawMessage) error

	// OnRedisEvent is called by the Manager when a subscribed Redis pub/sub
	// channel produces a message. payload は Redis から受信した raw bytes。
	OnRedisEvent(payload []byte)

	// OnClientMessage forwards a `ch` envelope sent by the client to this
	// specific channel id. msgType / body は client envelope の `type` /
	// `body`。
	OnClientMessage(msgType string, body json.RawMessage)

	// Dispose is called once when the channel is unregistered. 実装は購読
	// 解除や goroutine 停止をここで行う。
	Dispose()
}

// ChannelContext is what a Channel implementation receives so it can send
// messages back to its owning Connection without holding a direct pointer.
type ChannelContext interface {
	// ID returns the per-channel id supplied by the client.
	ID() string
	// User returns the authenticated user, or nil for anonymous connections.
	User() any
	// Send marshals payload as JSON and queues it as a `channel` envelope to
	// the client.
	Send(msgType string, body any) error
	// Subscribe registers an additional Redis topic. payload はこの channel の
	// OnRedisEvent に届く。
	Subscribe(topic string)
	// Unsubscribe stops listening on a topic.
	Unsubscribe(topic string)
	// HardMuteRules returns the viewer's persisted hardMutedWords (raw jsonb).
	// Timeline channels use it to drop matching notes per-publish (#787).
	// Returns nil when the connection is anonymous / has no rule set.
	HardMuteRules() []byte
	// FollowingSnapshot returns the viewer's followee map (followeeID →
	// withReplies). Timeline channels use it to gate reply notes per-publish
	// (#1063) the same way upstream Misskey checks
	// `this.following[note.userId]?.withReplies`. Returns nil when the
	// connection is anonymous or the lookup is unwired — in that case channels
	// should fall back to the 3 escape hatches only (= drop reply unless
	// reply-to-me / isMe / self-thread).
	FollowingSnapshot() map[string]bool
	// MuteBlockSnapshot returns the viewer's mute/block snapshot (muting /
	// blocking-me / renote-muting / muted-instances / muting-channels). The
	// main / notifications channels use it to drop notifications / mentions
	// involving muted instances, muted users, or users blocking the viewer
	// (#1711). Returns nil when the connection is anonymous or the lookup is
	// unwired — callers must treat nil as "no filtering" (fail-open).
	MuteBlockSnapshot() *MuteBlockSnapshot
	// UserPolicies returns the viewer's effective role policies, fetched once at
	// connect time. Timeline channels gate ltlAvailable / gtlAvailable on it
	// (#1942). Returns nil when the policy provider is unwired — callers treat
	// nil as "no gating" (fail-open).
	UserPolicies() map[string]any
	// UGCVisibilityForVisitor returns the live meta.ugcVisibilityForVisitor
	// ("all" / "local" / "none"). Channels consult it only for anonymous
	// viewers, mirroring upstream NoteStreamingHidingService.filter. Returns ""
	// when the lookup is unwired — callers treat "" as "all" (fail-open, the
	// startup wiring check guards production).
	UGCVisibilityForVisitor() string
}

// PermittedChannel is an optional interface a Channel can implement to
// declare the OAuth2 permission scope required for subscription.
// Dispatcher は connect 時にトークンの permission 配列と照合し、kind が
// 含まれなければチャンネル作成を拒否する。
type PermittedChannel interface {
	Channel
	// RequiredPermission returns the OAuth2 scope string (e.g. "read:account").
	// An empty string means no permission is required.
	RequiredPermission() string
}

// ShareableChannel is an optional interface a Channel can implement to
// signal that a single Connection should not hold multiple instances of the
// same channel name simultaneously. 例: serverStats/queueStats のような
// broadcast チャンネルは 1 接続につき 1 購読で十分。Dispatcher は connect
// 時に既存の同名チャンネルを検出し、2 回目の接続を no-op にする。
type ShareableChannel interface {
	Channel
	// ShouldShare reports whether this channel instance should be shared.
	// true を返すと、同じ Connection 内の同名チャンネルへの追加 connect は
	// 新しいエントリを作らずに無視される。
	ShouldShare() bool
}

// ChannelFactory builds a new Channel for the given context. Channel name は
// Misskey クライアントが connect.body.channel に指定する文字列 (e.g.
// "homeTimeline").
type ChannelFactory func(ctx ChannelContext) Channel

// Registry maps channel name → ChannelFactory. router.go で各 Channel 実装を
// register することで Manager が参照できる。
type Registry struct {
	mu           sync.RWMutex
	factories    map[string]ChannelFactory
	credentialed map[string]bool
}

// NewRegistry constructs an empty Registry.
func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]ChannelFactory), credentialed: make(map[string]bool)}
}

// Register binds name → factory. 同名で複数回 register した場合は最後の登録が
// 有効になる。
func (r *Registry) Register(name string, factory ChannelFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = factory
}

// RegisterCredentialed binds name → factory and marks the channel as requiring
// an authenticated connection (upstream requireCredential=true). Anonymous
// (user==nil) connect requests to such a channel are silently rejected by the
// Dispatcher (#1944)。requireCredential は channel ごとの登録時プロパティなので、
// scope (PermittedChannel) と分けて registry 側で一括宣言する。
func (r *Registry) RegisterCredentialed(name string, factory ChannelFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = factory
	r.credentialed[name] = true
}

// RequiresCredential reports whether the named channel requires an authenticated
// connection (#1944). Unknown names return false.
func (r *Registry) RequiresCredential(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.credentialed[name]
}

// Lookup returns the factory for name, or nil when name is unknown.
func (r *Registry) Lookup(name string) ChannelFactory {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.factories[name]
}
