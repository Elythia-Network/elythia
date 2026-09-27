package stream

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shiroha-a/mk/internal/misc/credkey"
)

// StreamRevokeTopic は資格情報の失効 (トークン再生成 / アクセストークン失効 /
// 凍結 / アカウント削除) を全プロセスの stream.Manager に伝える pubsub topic 名。
//
// WebSocket は接続時に一度だけ認証され、user / scope / role policy を
// Connection に固定する。HTTP 側の tokenCache を落としても既存の接続は
// 生き続けるので、漏洩したトークンで張られた接続が失効後も通知・DM・
// フォロワー限定投稿を受け取り続ける。接続は張られたプロセスにしか居ないため、
// 失効を処理したプロセスから直接閉じることはできず、pubsub で配る
// (#791 の wordmute reload と同型)。
//
// **upstream は現行版でもサーバー側から切っていない。** mk-go 独自の硬化
// (docs/divergence.md)。
const StreamRevokeTopic = "credential:revoke"

// StreamRevokePayload is the JSON wire format for StreamRevokeTopic.
//
// Credential が空なら UserID の全接続、空でなければ UserID の接続のうち
// その資格情報で張られたものだけを閉じる。UserID を必須にしているのは、
// 資格情報の鍵だけで照合すると、鍵の導出を誤ったときに他人の接続を閉じうるため。
type StreamRevokePayload struct {
	UserID     string `json:"userId"`
	Credential string `json:"credential,omitempty"`
}

// RevokeCloseCode is the WebSocket close code sent to revoked connections.
// 1008 (Policy Violation) は「方針に反するため閉じる」の汎用コードで、
// クライアントは再接続を試み、失効済みの資格情報なら upgrade で拒否される。
const RevokeCloseCode = websocket.ClosePolicyViolation

// revokeCloseReason is the close frame reason for revoked connections.
const revokeCloseReason = "credential revoked"

// defaultRevokeSettle は失効 event を受けてから接続を閉じるまでの猶予。
//
// **i/regenerate-token は myTokenRegenerated を main stream へ送ってから失効を
// publish する** が、両者は別 topic で、pubsub の実装は topic ごとに別の購読
// (= 別 goroutine) で配るので、受信側での到着順は保証されない。閉じた後の
// Send は拒否されるので、失効が先に着くと通知が落ちる。猶予を置いて、
// 先に publish された event が接続のキューに載るのを待つ。
// 猶予の間に流れうるのは「失効の直前まで正当に見えていたもの」と同じ範囲で、
// 閉じない場合 (無期限) と比べれば十分に短い。
const defaultRevokeSettle = time.Second

// SetRevokeSettleDelay overrides the delay between receiving a revoke event
// and closing the matching connections. d <= 0 restores the default. 主に
// テストで時計を縮めるための setter。
func (m *Manager) SetRevokeSettleDelay(d time.Duration) {
	m.revokeSettle = d
}

func (m *Manager) settleDelay() time.Duration {
	if m.revokeSettle > 0 {
		return m.revokeSettle
	}
	return defaultRevokeSettle
}

// defaultRevokeRecheck は 1 回目の閉じ処理から 2 回目 (安全網) までの間隔。
//
// **HTTP 側の tokenCache (30 秒) より長くする。** 失効の直前に DB を引いた
// リクエストが、無効化 event を受けた後で cache に積む競合は避けられない
// (引いた時点では旧 token が有効だった)。その entry は最長で TTL の間生きるので、
// 1 回目で閉じた直後の再接続がそれで認証されうる。TTL が切れた後にもう一度同じ
// 条件で閉じれば、その経路で張り直された接続も残らない。router は
// middleware の TTL から明示的に設定する (SetRevokeRecheckDelay)。
const defaultRevokeRecheck = 35 * time.Second

// SetRevokeRecheckDelay overrides the delay before the second (safety-net)
// close pass. d <= 0 restores the default.
func (m *Manager) SetRevokeRecheckDelay(d time.Duration) {
	m.revokeMu.Lock()
	m.revokeRecheck = d
	m.revokeMu.Unlock()
}

func (m *Manager) recheckDelay() time.Duration {
	m.revokeMu.RLock()
	defer m.revokeMu.RUnlock()
	if m.revokeRecheck > 0 {
		return m.revokeRecheck
	}
	return defaultRevokeRecheck
}

// OnStreamRevoke registers fn to be called with the user id as soon as a
// revoke event is handled on this process (received over pubsub or published
// locally), before the settle delay. nil is ignored.
//
// **HTTP 側の tokenCache の無効化を全プロセスへ配るためにある。** tokenCache は
// プロセス内の map なので、失効を処理したプロセスが自分の cache を落としても、
// Web ノードが複数ある構成では他のノードに旧 token の entry が残る。閉じた
// 接続がそのノードへ再接続すると古い entry で認証され、今度は閉じる契機が
// 無いので無期限に残る。失効 event は全プロセスへ届くので、それに相乗りして
// 各プロセスが自分の cache を落とす。落とす単位は利用者ごと (event は生 token を
// 持たない)。落としすぎても次のリクエストで DB を引き直すだけで済む。
func (m *Manager) OnStreamRevoke(fn func(userID string)) {
	if fn == nil {
		return
	}
	m.revokeMu.Lock()
	m.revokeObservers = append(m.revokeObservers, fn)
	m.revokeMu.Unlock()
}

// handleRevoke runs the observers immediately, then closes the matching
// connections after the settle delay and once more after the recheck delay.
// 同じ payload で 2 度呼ばれても害は無い (observer は cache を落とすだけ、
// RevokeStreams は閉じた接続を登録から外すので 2 度目は何もしない)。
func (m *Manager) handleRevoke(payload StreamRevokePayload) {
	m.revokeMu.RLock()
	observers := slices.Clone(m.revokeObservers)
	m.revokeMu.RUnlock()
	for _, fn := range observers {
		fn(payload.UserID)
	}
	// 走査も猶予の後に行う。失効の直前に認証を通り、まだ register されて
	// いない接続も拾える。
	time.AfterFunc(m.settleDelay(), func() {
		m.RevokeStreams(payload.UserID, payload.Credential)
	})
	time.AfterFunc(m.recheckDelay(), func() {
		m.RevokeStreams(payload.UserID, payload.Credential)
	})
}

// RevokeStreams closes every live connection owned by userID, or only those
// authenticated with credential when it is non-empty. Returns the number of
// connections closed. userID が空なら何もしない。
//
// 閉じるときは送信キューを吐き切ってから close frame (RevokeCloseCode) を
// 送る (Connection.CloseWithCode)。
func (m *Manager) RevokeStreams(userID, credential string) int {
	if userID == "" {
		return 0
	}
	m.mu.RLock()
	targets := make([]*Connection, 0)
	for _, c := range m.conns {
		u := c.User()
		if u == nil || u.ID != userID {
			continue
		}
		if credential != "" && c.Credential() != credential {
			continue
		}
		targets = append(targets, c)
	}
	m.mu.RUnlock()
	// close handler が unregister で m.mu を取るので、lock の外で閉じる。
	for _, c := range targets {
		c.CloseWithCode(RevokeCloseCode, revokeCloseReason)
	}
	return len(targets)
}

// SubscribeStreamRevoke starts listening on StreamRevokeTopic and closes the
// matching local connections after the settle delay.
//
// **同一 Manager に対して 1 度だけ呼ぶこと** (SubscribeWordMuteReload と同じ
// 制約)。bus が nil なら subscribe しない。
func (m *Manager) SubscribeStreamRevoke() {
	if m.bus == nil {
		return
	}
	m.subscribeManaged(StreamRevokeTopic, func(raw []byte) {
		var payload StreamRevokePayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			slog.Warn("stream: revoke payload unmarshal failed", "err", err)
			return
		}
		if payload.UserID == "" {
			slog.Warn("stream: revoke payload missing userId")
			return
		}
		m.handleRevoke(payload)
	})
}

// UnsubscribeStreamRevoke stops listening on StreamRevokeTopic.
func (m *Manager) UnsubscribeStreamRevoke() {
	if m.bus == nil {
		return
	}
	m.unsubscribeManaged(StreamRevokeTopic)
}

// StreamRevokePublisher publishes revoke events to StreamRevokeTopic. Handlers
// that invalidate a credential call it so connections on every process close.
type StreamRevokePublisher struct {
	pub   PubSubPublisher
	local *Manager
}

// NewStreamRevokePublisher constructs a StreamRevokePublisher. local is the
// Manager of this process (may be nil); it handles the event directly so the
// connections held here close even when publishing fails.
//
// **publish の成否に自分のプロセスを依存させない。** Redis が落ちていると
// publish は失敗し、pubsub 経由では自分自身にも届かない。失効を処理した
// プロセスが持つ接続 (失効操作をした本人の端末が繋いでいることが多い) まで
// 閉じなくなるので、local は pubsub を経由せず直接処理する。publish が成功すると
// 同じ event が pubsub 経由でもう一度届くが、handleRevoke は冪等。
func NewStreamRevokePublisher(pub PubSubPublisher, local *Manager) *StreamRevokePublisher {
	return &StreamRevokePublisher{pub: pub, local: local}
}

// RevokeUserStreams closes every streaming connection of userID (suspension,
// account deletion).
func (p *StreamRevokePublisher) RevokeUserStreams(userID string) {
	p.publish(StreamRevokePayload{UserID: userID})
}

// RevokeNativeTokenStreams closes the connections of userID that were
// authenticated with the given native login token (i/regenerate-token).
// token が空なら何もしない — 空のまま送ると全接続を閉じる意味になるため。
func (p *StreamRevokePublisher) RevokeNativeTokenStreams(userID, token string) {
	key := credkey.Native(token)
	if key == "" {
		return
	}
	p.publish(StreamRevokePayload{UserID: userID, Credential: key})
}

// RevokeAccessTokenStreams closes the connections of userID that were
// authenticated with the app access token tokenID (i/revoke-token, OAuth code
// replay). tokenID が空なら何もしない。
func (p *StreamRevokePublisher) RevokeAccessTokenStreams(userID, tokenID string) {
	key := credkey.AccessToken(tokenID)
	if key == "" {
		return
	}
	p.publish(StreamRevokePayload{UserID: userID, Credential: key})
}

func (p *StreamRevokePublisher) publish(payload StreamRevokePayload) {
	if p == nil || payload.UserID == "" {
		return
	}
	if p.local != nil {
		p.local.handleRevoke(payload)
	}
	if p.pub == nil {
		return
	}
	// 文字列だけの struct なので Marshal は失敗しない。
	raw, _ := json.Marshal(payload)
	if err := p.pub.Publish(context.Background(), StreamRevokeTopic, json.RawMessage(raw)); err != nil {
		slog.Warn("stream revoke: publish failed", "userId", payload.UserID, "err", err)
	}
}
