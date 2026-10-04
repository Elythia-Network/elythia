package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 自動追い読み (`v-appear`) を持つコンポーネントは、レート制限の理由を出すこと (#2955)。
//
// **振る舞いの検証は vitest 側が持つ** (`test/unit/paginator-rate-limit.test.ts`)。
// 当初はこのゲートで `paginator.ts` の字面を見ていたが、敵対的レビューで
// **11 変異中 7 件を素通り**することを実測された — 「`canFetch*` を落とさず印
// だけ立てる」「向きを取り違える」という、修正の本体そのものを壊す変異が通って
// いた。**文字列照合では振る舞いを検証できない。**
//
// ここが見るのは vitest では届かない範囲、すなわち**新しい自動追い読みの場所が
// 増えたときに理由の表示を忘れる**ことだけ。実際 `i/notifications` は `MkPagination` を
// 経由せず独自のボタンを持っており、**そこだけ理由が出ていなかった**。
//
// **既知の取りこぼし**: `MkDrive` は Paginator を使わず独自の `fetchMoreFiles`
// で回すので対象外 (`drive/files` にレート制限も無い)。allowlist に理由付きで
// 持つ。
var noPaginatorAutoLoad = map[string]string{
	// **`drive/files` / `drive/folders` にレート制限が無い** (`DefaultEndpointLimits`
	// に無く、middleware は未登録 endpoint を素通しする) ので 429 が来ない。
	//
	// **当初「Paginator を使わない」と書いていたが事実と逆** (レビュー 2 周目 M-2)。
	// `MkDrive` は `new Paginator('drive/files', ...)` を持ち、`fetchMoreFiles` は
	// それを呼ぶだけ。**`drive/files` に制限を足したら、ここを外して
	// `:paginator` を渡すだけで済む** — 誤った理由を残すと「使えない」と
	// 誤解される。
	"MkDrive.vue": "drive/files / drive/folders にレート制限が無い",
}

// noPaginatorAutoLoad の除外の根拠になっている endpoint。制限が付いたら落とす。
var noPaginatorAutoLoadEndpoints = []string{"drive/files", "drive/folders"}

// bindsRateLimitedNotice reports whether the file binds the notice component.
//
// #2915 の `bindsSlotComponent` と同じ形 — パスを含む行から引用符の中身を
// 消して識別子が残るかを見る。import の書き方 (静的 / 動的 / 相対) に依存せず、
// 別名 import やパスを文字列として置いただけの形は落ちる。
func bindsRateLimitedNotice(src string) bool {
	for _, line := range strings.Split(src, "\n") {
		if !strings.Contains(line, "MkRateLimitedNotice.vue") || typeOnlyImport.MatchString(line) {
			continue
		}
		if strings.Contains(quotedRe.ReplaceAllString(line, ""), "MkRateLimitedNotice") {
			return true
		}
	}
	return false
}

func TestAutoLoadingComponentsShowRateLimit(t *testing.T) {
	fe := filepath.Join(repoRootDir(t), "frontend", "packages", "frontend", "src")
	components := filepath.Join(fe, "components")
	if _, err := os.Stat(components); err != nil {
		require.NoError(t, err, "frontend/ は追跡しているので、%s を読めないのはパスの誤り", components)
	}

	// **`src` 全体を歩く (レビュー 2 周目 M-3)。** 当初は `components/` 直下
	// だけを見ており、**サブディレクトリ / `pages` / `widgets` の 376 ファイルが
	// 範囲外**だった。今は取りこぼしゼロだが、範囲外に自動追い読みを足した
	// ときに**静かに検査から外れる**。
	var autoLoading, missing, noImport []string // autoLoading はフルパス
	require.NoError(t, filepath.WalkDir(fe, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".vue" {
			return err
		}
		src := stripComments(readFileString(t, path))
		// **`v-appear` だけでなく poll も見る。** streaming 系は
		// `useInterval` で 10〜22.5 秒ごとに撃つので、`v-appear` を持たない
		// コンポーネントでも 429 に当たる。
		if !strings.Contains(src, "v-appear") && !strings.Contains(src, "useInterval") {
			return nil
		}
		if !strings.Contains(src, "fetchOlder") && !strings.Contains(src, "fetchNewer") {
			return nil
		}
		name := filepath.Base(path)
		// **フルパスで持つ (レビュー 3 周目 M-1)。** basename だけを溜めて
		// 後で `components/` から読み直す形にしていたため、`components` の外に
		// 同名のファイルがあると**別のファイルを検査していた** (実測で
		// `pages/MkPagination.vue` に欠陥のあるものを置いても緑だった)。
		autoLoading = append(autoLoading, path)
		if _, skip := noPaginatorAutoLoad[name]; skip {
			return nil
		}
		// **template での使用を見る。** 識別子だけを探すと **import 行が
		// 残っているだけで緑になる** (実測で素通りした)。
		// **2 箇所とも要る。** 初回失敗用 (`v-else-if`、一覧が空のとき) と、
		// 読めている一覧の末尾に出す用。片方だけだと、もう片方の状況で
		// 何も出ない (実測で末尾だけ消しても素通りした)。
		if strings.Count(src, "<MkRateLimitedNotice") < 2 {
			missing = append(missing, name)
			return nil
		}
		// **import も見る (レビュー 2 周目 H-2)。** `MkRateLimitedNotice` は
		// `components/global/` ではないので**グローバル登録されていない**。
		// import を落とすと解決できないまま素の要素として描画され、
		// **production では警告すら出ない**。実測で vue-tsc も eslint も
		// このゲートも緑のままだった。#2915 の `plugin_slot_gate_test.go` が
		// 同じ条件で同じ検査を持っている。
		if !bindsRateLimitedNotice(src) {
			noImport = append(noImport, name)
		}
		return nil
	}))

	// 拾えなかったら落とす。書式が変わって空振りすると、検査していないのに
	// 緑になる (#2874 / #2828 と同じ判断)。
	require.NotEmptyf(t, autoLoading, "`v-appear` を持つコンポーネントを 1 つも拾えない (書式が変わった?)")
	require.Emptyf(t, missing,
		"自動追い読みを持つのにレート制限の理由を出していない: %v\n"+
			"429 で停止したとき、ボタンが黙って消えるだけになり「これで全部」に見える (#2955)", missing)
	// **allowlist の理由を実装と突き合わせる (レビュー 3 周目 M-2)。** 除外の
	// 根拠は「その endpoint にレート制限が無い」ことなので、制限を足した瞬間に
	// 落ちる必要がある。ファイル名で引くだけだと、理由が偽になっても静かに
	// 検査から外れたままになる。
	defs := readFileString(t, filepath.Join(repoRootDir(t), "internal", "server", "middleware", "ratelimit_defs.go"))
	for _, ep := range noPaginatorAutoLoadEndpoints {
		require.NotContainsf(t, stripComments(defs), `"`+ep+`":`,
			"%s にレート制限が付いた。除外の根拠が消えたので noPaginatorAutoLoad から外すこと", ep)
	}

	require.Emptyf(t, noImport,
		"<MkRateLimitedNotice> は置かれているが import していない: %v\n"+
			"components/global/ ではないのでグローバル登録されておらず、解決できないまま\n"+
			"素の要素として描画される (production では無言)", noImport)

	// **表示そのものが中身を持っていること。** 上は「置いてあるか」しか見て
	// いないので、`MkRateLimitedNotice` の中を空にすると全部素通りする
	// (実測で `v-if="false"` と再試行の削除が通った)。
	noticeSrc := stripComments(readFileString(t,
		filepath.Join(components, "MkRateLimitedNotice.vue")))
	// **MkError より前の枝に置くこと (レビュー 2 周目 H-1)。** v-if チェーンは
	// 排他なので、後ろに置くと初回取得の 429 (= `error` も立つ) で必ず
	// `MkError` (「何かがおかしいようです」) が選ばれ、**理由が画面に出ない**。
	for _, path := range autoLoading {
		name := filepath.Base(path)
		if _, skip := noPaginatorAutoLoad[name]; skip {
			continue
		}
		src := stripComments(readFileString(t, path))
		notice := strings.Index(src, "<MkRateLimitedNotice v-else-if")
		mkError := strings.Index(src, "<MkError")
		require.GreaterOrEqualf(t, notice, 0, "%s: MkError と同じ枝に notice が無い", name)
		if mkError >= 0 {
			require.Lessf(t, notice, mkError,
				"%s: notice が MkError より後ろにある。初回の 429 で理由が出ない", name)
		}
		// **読めている分があるときはこの枝を選ばない。** 条件を落とすと
		// 429 で**一覧ごと消える** (H-1 の修正が一度作った回帰)。
		// **条件は `error`。** items の件数で見ると、streaming が 1 件 prepend した
		// 瞬間に枝から外れて MkError に戻る (realtimeMode は既定 true)。
		require.Containsf(t, src, `v-else-if="paginator.rateLimited.value && paginator.error.value"`,
			"%s: 初回失敗用の枝の条件が error でない。streaming で 1 件届くと MkError に戻る", name)
	}

	for _, want := range []struct{ frag, why string }{
		{`v-if="paginator.rateLimited.value"`, "レート制限のときだけ出す条件が無い"},
		{"i18n.ts.rateLimitExceeded", "理由の文言が出ない"},
		{`@click="retry"`, "再試行の導線が無い"},
		{"props.paginator.retryAfterRateLimit()", "再試行が Paginator を呼んでいない"},
		{`:disabled="!paginator.canRetryAfterRateLimit.value"`, "再試行の冷却が Paginator 側でない。枝の移動でアンマウントされて消える"},
	} {
		require.Containsf(t, noticeSrc, want.frag, "MkRateLimitedNotice: %s", want.why)
	}
}
