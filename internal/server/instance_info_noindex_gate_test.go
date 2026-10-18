package server

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// instanceInfoRouteRe matches the /instance-info/:host registration and
// captures the handler it points at.
var instanceInfoRouteRe = regexp.MustCompile(`s\.echo\.GET\("/instance-info/:host",\s*([\w.]+)\)`)

// TestInstanceInfoRouteIsNoindex asserts `/instance-info/:host` stays on the
// noindex handler (#3030).
//
// **外すと誰も気付かない。** ルートを消しても SPA catchall が**同じ shell を
// noindex 無しで**返すだけなので、応答は 200 のまま見た目も変わらない。気付くのは
// 検索結果に連合先の情報ページが並び始めたときで、そこから原因に辿り着くのは難しい。
//
// ハンドラ名まで見る — 別のハンドラに差し替えると noindex が消えるのに、
// 「登録されているか」だけの検査では通ってしまう。
//
// **コメントを剥がしてから探す。** そうしないと、登録行そのものを `//` や
// `/* */` で囲んだだけで緑のまま通る (#2856 が `wiring-check` で踏んだのと
// 同じ形)。コメントアウトは「消す」のと同じ。
func TestInstanceInfoRouteIsNoindex(t *testing.T) {
	// パッケージ全体を読む (#3672)。登録を別のファイルへ移しても同じ登録を見て、
	// 2 つのファイルに分かれた再登録も数える。
	const router = "internal/server"
	ms := instanceInfoRouteRe.FindAllStringSubmatch(packageSourceWithoutComments(t), -1)
	// **ちょうど 1 件であることまで要求する。** Echo は同じ method + path の
	// 再登録を黙って後勝ちで上書きするので、先頭一致だけを見る形だと、後段で
	// 別のハンドラに登録し直されても緑のまま noindex が消える
	// (#2969 の「pin 行はちょうど 1 件であることも要求する」と同型)。
	require.LessOrEqualf(t, len(ms), 1,
		"%s が /instance-info/:host を %d 回登録している。Echo は後勝ちで上書きするので、"+
			"どれが効いているか読み取れない", router, len(ms))
	var m []string
	if len(ms) == 1 {
		m = ms[0]
	}
	require.NotEmptyf(t, m,
		"%s が /instance-info/:host を登録していない (#3030)。\n"+
			"**登録の書式を変えただけでもここに来る** — 引数を複数行に折り返した、"+
			"パスを定数に括り出した、param 名を変えた、のいずれかなら "+
			"instanceInfoRouteRe も一緒に直すこと (fail-closed にしてあるので、"+
			"検査が空振りしたまま緑になることは無い)", router)
	require.Equalf(t, "ssrMeta.NoIndexPage", m[1],
		"/instance-info/:host が noindex を出さないハンドラに向いている")
}
