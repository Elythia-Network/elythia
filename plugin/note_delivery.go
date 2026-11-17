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
	// 保存に失敗したときは、本体が [NoteDeliveryRegistration.ForgetNotes] で
	// 知らせる。保存したノートは、連合しない (localOnly) ものでも条件を使う
	// (OGPを隠す) ので、知らせない。
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
	// その回の配送ではリモートのどのホストにも届かない。条件を読めないときに
	// 配ると、外したはずのホストへ届きうるため。配送の再試行は無いので、障害の
	// あいだに作られたノートは連合しないまま残る。APの照会 (`/notes/:id`・
	// `/notes/:id/activity`・outbox・featured) は、障害のあいだ503になる
	// (404にすると、それを「消えた」と扱う相手で、障害が直っても戻らない)。
	//
	// 1回で渡すIDは、APのoutboxの1ページ分(20件)までになる。
	Targets func(ctx context.Context, ids []string) (map[string]DeliveryTargets, error)

	// ForgetNotes drops what Prepare stored for notes that no longer need it.
	// Optional: nil means the plugin keeps the rows.
	//
	// 本体が呼ぶのは、Prepareの後にノートの保存に失敗したときと、ローカルの
	// ノートを削除したとき (Deleteサービスを通るもの)。どちらも、作成や削除の
	// 応答を待たせないよう別のgoroutineから呼ぶ。同時に走る呼び出しは8件までで、
	// 超えた分とサーバーを止めている途中の分は呼ばずに捨てる (ログに残る)。**全部の削除を知らせるわけではない。** 返信先や
	// リノート元の削除に伴ってDBの外部キーで消えるノートと、[NoteDeliveryRegistration.ForgetUser]
	// で知らせる利用者の削除は、ここには来ない。IDは他のノートに使われないので、
	// 残った行が他のノートに効くことは無い。
	//
	// エラーは本体のログに残すだけで、ノートの作成や削除は止めない。
	ForgetNotes func(ctx context.Context, ids []string) error

	// ForgetUser drops what Prepare stored for every note of a deleted local
	// user. Optional: nil means the plugin keeps the rows.
	//
	// 利用者の削除のジョブが、その利用者のノートを消し終えた後に呼ぶ。エラーは
	// 本体のログに残すだけで、削除は止めない。
	ForgetUser func(ctx context.Context, userID string) error
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
// 署名した利用者の`host`になる (inboxのURLのホストではない)。比べる前に、本体が
// 大文字・末尾の`.`・既定のポート (`:443`と`:80`)・国際化ドメインの綴りを
// そろえる。
//
// **ホスト名として読めない値は、条件ごと読めないものとして扱う。** URL
// (`https://bad.example`)・パスの付いた値 (`bad.example/`)・ワイルドカード
// (`*.bad.example`)・空文字を1つでも含むと、そのノートはModeに関係なく、どの
// ホストにも配らず誰にも見せない。そのまま比べると一致せず、excludeでは
// 外したはずのホストへ届くため。
//
// **このサーバー自身のホストも書ける (#4203)。** ログインしていない閲覧者には、
// 自分のホストからの照会として同じ条件を当てる。includeに入れたら見せ、入れなければ
// 見せない。excludeに入れたら見せず、入れなければ見せる。効くのは埋め込み
// (`/embed/notes/:id`)・フィード (`/@user.rss`など)・未ログインの`notes/show`
// (本文を伏せる)、署名もUser-Agentのホストも無いActivityPubの照会で、ログイン
// しているローカルの利用者と、タイムライン・検索には効かない。
//
// 照会では、署名で確かめられなければUser-Agentに書かれたホストを使い、
// それでも分からない相手は、ログインしていない閲覧者と同じく自分のホストとして
// 判定する (#4203)。自分のホストを入れていなければ、includeでは見せず、excludeでは
// 見せる。User-Agentは相手が自由に書けるので、照会の判定は配送の絞り込みの補助と
// 考えること。
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
