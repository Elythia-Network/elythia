package plugin

import (
	"context"
	"encoding/json"
	"errors"
)

/*
 * 同じプラグインを入れている mk-go 同士だけで通信する経路。
 *
 * **ActivityPub には出さない。** AP に載せると、こちらの不具合の症状が
 * 他人のサーバー側に出るうえ、一度公開した形は後から塞げない (#2476 で
 * 「AP 関連は公開しない」と決めている)。ここは mk-go 専用の HTTP 経路に
 * 閉じるので、壊れても他実装には届かない。代わりに、相手も同じプラグインを
 * 持っていることが前提になる。
 *
 * 宛先・署名・ブロック・SSRF・大きさ・受け口のレート制限は mk-go 側で担保する
 * ([Router] と違い、peer の受け口には専用の throttle がある)。**送信側には
 * 無い**ので、送る量はプラグインが決めること。プラグインが面倒を見るのは
 * payload の中身と、送る間隔。
 *
 * wire の仕様は docs/plugin-peer-protocol.md。
 */

// Peer is a private channel to the same plugin on other mk-go instances.
//
// 使うには [Definition.Peered] を立てる。立てると nodeinfo にプラグイン名が
// 出て、相手から「このインスタンスは同じプラグインを持っている」と分かる。
// 立てていないプラグインでこれを呼ぶとエラーになる。
//
// **[Handle] と [OnReply] は [Definition.Peer] の中で登録する。** Routes の中で
// 登録すると、ロールを分割した構成 (MK_ONLY_SERVER / MK_ONLY_QUEUE) で片方に
// しか登録されない — 送信の POST は queue ロールで走るので、OnReply が
// server ロールにしか無いと応答が届かない (#2819)。
type Peer interface {
	// Send posts a payload to the same plugin on host and returns the id
	// that identifies this exchange.
	//
	// **POST はキューが行う**ので、相手が遅くてもこちらのリクエストは詰まらない。
	// 応答は [Peer.OnReply] に届く。ただし**即座に返るとは限らない** — 相手の
	// nodeinfo がキャッシュに無いと、ここで取得を待つ (最大 10 秒)。
	//
	// **再起動をまたぐ (#2819)。** 送信中のものはデプロイで消えない。
	//
	// 相手が同じプラグインを持たない / ブロックしている / 宛先が不正な場合は
	// ここでエラーになり、送信そのものが起きない。
	Send(ctx context.Context, host string, payload any) (id string, err error)

	// Handle registers the receiver for payloads from other instances.
	//
	// from は HTTP Signature で確定したホスト。**名乗りではない**ので、
	// 送信元としてそのまま信頼してよい。戻り値は相手の [Peer.OnReply] に
	// 渡る。nil を返せば応答なしとして扱われる。
	//
	// **中身は信用しない。** 相手は同じプラグインを持っているだけで、
	// 善良とは限らない。値域の検査はプラグインの責任。
	Handle(fn PeerHandler)

	// OnReply registers the receiver for replies to [Peer.Send].
	//
	// **届かないことがある。** 相手が落ちている / 応答しない / リトライの
	// 上限に達した / 相手がプラグインを外した場合は呼ばれない。mk-go は
	// 「いつか必ず届く」を約束しないので、取り直しはプラグイン側が期限を
	// 持って行うこと。
	//
	// **複数回呼ばれることがある。** 送信はキューに載るので、worker が途中で
	// 落ちれば同じ交換が積み直される。加算や追記はそのままでは二重になる。
	OnReply(fn PeerReplyHandler)

	// Has reports whether host runs this plugin.
	//
	// nodeinfo 由来のキャッシュを見る。相手が宣言していなければ false。
	Has(ctx context.Context, host string) (bool, error)
}

// PeerHandler processes one payload from another instance and returns the
// reply. Returning an error makes mk-go respond with an error status; the
// sender's [Peer.OnReply] is not called.
//
// Wrap [ErrBadPeerPayload] when the payload itself is wrong: mk-go then
// answers 400, which the sender does not retry, and logs it at DEBUG. Any
// other error, including a JSON decoding error returned as is, is treated as
// a fault on this side: 500 (retried by the sender) and an ERROR log. A
// [*StatusError] from [Errorf] is treated like any other error (500, retried);
// wrap the sentinel instead. For a payload that does not decode:
//
//	if err := json.Unmarshal(payload, &req); err != nil {
//		return nil, fmt.Errorf("%w: %w", plugin.ErrBadPeerPayload, err)
//	}
type PeerHandler func(ctx context.Context, from string, payload json.RawMessage) (any, error)

// ErrBadPeerPayload marks a [PeerHandler] error as the sender's fault (a
// payload that fails validation). Wrap it: fmt.Errorf("%w: user is empty",
// plugin.ErrBadPeerPayload).
var ErrBadPeerPayload = errors.New("plugin: bad peer payload")

// PeerReplyHandler processes the reply to a [Peer.Send]. id is what Send
// returned.
type PeerReplyHandler func(ctx context.Context, from, id string, reply json.RawMessage) error
