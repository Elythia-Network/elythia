package plugin

import (
	"context"
	"encoding/json"
	"errors"
)

// NoteDeliveryRegistration narrows the remote hosts that a local note is
// delivered to and that may fetch it (#4199).
//
// **絞ることしかできない。** 本体は、配送先とAPの照会の可否を自分で決めたうえで、
// ここで返された条件に**合わない**ホストを外すだけに使う。プラグインが配送先を
// 増やしたり、本体が見せない投稿を見せたりする口は無い。不具合の症状が
// 「届かない」側にだけ出るようにするため ([compatibility.md] の「ActivityPub
// 関連」を参照)。
//
// 指定は、利用者がnotes/createに付ける`pluginOptions.<プラグイン名>`で受け取る。
// 本体はその値を読まずに [NoteDeliveryRegistration.Prepare] へ渡し、
// プラグインは条件を自分のstorageに保存する。配送と照会のたびに、本体は
// [NoteDeliveryRegistration.Targets] で条件を読み直す。
//
// [compatibility.md]: https://github.com/elythia-network/elythia/blob/develop/docs/plugins/compatibility.md
type NoteDeliveryRegistration struct {
	// Prepare stores the delivery targets the author asked for, before the note
	// is stored. Required.
	//
	// **ノートを保存する前に呼ぶ。** 保存の後に呼ぶと、条件が書かれる前に配送が
	// 走りうる (配送はノートの保存の直後に別のgoroutineで始まる)。ノートの
	// 保存に失敗したときは、ここで書いた行が残る。IDは他のノートに使われない
	// ので、残った行が他のノートに効くことは無い。
	//
	// [ErrBadNoteOption]を包んだエラーは、利用者の指定の誤りとして400
	// (`INVALID_PARAM`)になる。それ以外のエラーは500になる。どちらでもノートは
	// 作られない。
	Prepare func(ctx context.Context, req NoteDeliveryPrepareRequest) error

	// Targets reports the delivery targets of local notes. Required.
	//
	// 返す map には、条件のあるノートだけを入れる。入っていないIDは
	// 「このプラグインは絞らない」として扱う。
	//
	// **エラーは「どこにも配らない・誰にも見せない」になる。** 渡したノートは、
	// その回の配送ではリモートのどのホストにも届かず、APの照会は404になる。
	// 条件を読めないときに配ると、外したはずのホストへ届きうるため。配送の
	// 再試行は無いので、障害のあいだに作られたノートは連合しないまま残る。
	//
	// 1回で渡すIDは、APのoutboxの1ページ分(20件)までになる。
	Targets func(ctx context.Context, ids []string) (map[string]DeliveryTargets, error)
}

// NoteDeliveryPrepareRequest is what [NoteDeliveryRegistration.Prepare]
// receives.
type NoteDeliveryPrepareRequest struct {
	// NoteID is the ID the note will be stored under.
	NoteID string
	// UserID is the local author.
	UserID string
	// Option is this plugin's entry of `pluginOptions` in notes/create, as the
	// client sent it. 本体は中身を読まない。nullは渡さない (送られなかったのと
	// 同じに扱う)。
	Option json.RawMessage
}

// DeliveryTargetsMode tells how [DeliveryTargets.Hosts] is read.
type DeliveryTargetsMode string

const (
	// DeliveryTargetsInclude delivers only to the listed hosts.
	DeliveryTargetsInclude DeliveryTargetsMode = "include"
	// DeliveryTargetsExclude delivers to every host except the listed ones.
	DeliveryTargetsExclude DeliveryTargetsMode = "exclude"
)

// DeliveryTargets lists the remote hosts a note may reach.
//
// ホストは、本体がリモートの利用者の`host`として持つ形 (小文字、IDNは
// punycode、ポートがあれば`:`の後ろに付く) と完全一致で比べる。サブドメインは
// 含めない。比べるホストは、配送ではフォロワー・宛先の利用者の`host`、照会では
// 署名した利用者の`host`になる (inboxのURLのホストではない)。
//
// 照会では、署名で確かめられなければUser-Agentに書かれたホストを使い、
// それでも分からない相手は空文字で渡す。空文字はどのHostsとも一致しないので、
// includeでは見せず、excludeでは見せる。User-Agentは相手が自由に書けるので、
// 照会の判定は配送の絞り込みの補助と考えること。
//
// Modeが上の2つのどちらでもないときは、Hostsに関係なく、どのホストにも
// 配らず誰にも見せない。
type DeliveryTargets struct {
	Mode  DeliveryTargetsMode
	Hosts []string
}

// Allows reports whether host may receive or fetch the note.
func (t DeliveryTargets) Allows(host string) bool {
	listed := false
	for _, h := range t.Hosts {
		if h == host {
			listed = true
			break
		}
	}
	switch t.Mode {
	case DeliveryTargetsInclude:
		return listed
	case DeliveryTargetsExclude:
		return !listed
	default:
		return false
	}
}

// ErrBadNoteOption marks a [NoteDeliveryRegistration.Prepare] error as the
// client's fault. Wrap it: fmt.Errorf("%w: hosts is empty", plugin.ErrBadNoteOption).
var ErrBadNoteOption = errors.New("plugin: bad note option")
