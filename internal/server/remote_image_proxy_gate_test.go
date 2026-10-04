package server

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// gridImageColRe matches the `type: 'image'` of a grid column setting.
//
// **引用符と空白を緩く見る。** 書式が変わって 1 つも拾えなくなると、
// 「検査していないのに緑」になるのがこの gate の最悪の壊れ方なので、
// 取りこぼしにくい形にしておく (件数は下で完全一致させるため、広く拾って
// 誤検出したときも**落ちる**側に倒れる)。
// **キーの引用符も受ける (レビュー M1)。** `'type': 'image'` は TS として
// 正当で、`type\s*:` だけだと素通りする。
var gridImageColRe = regexp.MustCompile(`['"]?type['"]?\s*:\s*['"]image['"]`)

// srcBindingRe matches a bound `src` attribute (`:src` / `v-bind:src`).
var srcBindingRe = regexp.MustCompile(`(?::|v-bind:)src\s*=\s*("[^"]*"|'[^']*')`)

// proxyLeaves is the set of leaf expressions that count as already proxied,
// in addition to direct media-proxy calls.
//
// **名指しした中間参照を葉として許すため。** `:src="previewUrls.get(app.id)!"` も
// `return ... ? getStaticImageUrl(proxied) : proxied;` も、値そのものは呼び出し
// ではないが**別の pin で proxy 済みだと固定してある**。ここに載せない限り葉は
// 生の値として扱われる。
type proxyLeaves []string

// ok reports whether expr can only ever yield a media-proxy URL.
//
// **「式全体がちょうど 1 つの呼び出し」だけでは狭すぎる (1 周目レビュー M2)。**
// `(getProxiedImageUrl(x))` も `cond ? getProxiedImageUrl(a) : getProxiedImageUrl(b)`
// も**正しい書き方**なのに、厳密な形だけを許すと偽陽性で落ちる — #2957 の 2 周目が
// 取り下げになったのがそれ。
//
// **かといって「呼び出しが含まれるか」では緩すぎる (#2957 の 1 周目)。**
// `cond ? getProxiedImageUrl(a) : app.url` は生の枝が残っているのに免除される。
// 2 周目の回帰もこの形で、`props.emoji.url ?? getStaticImageUrl(...)` が
// 部分一致の検査を素通りした。
//
// **見るのは「proxy を通らない葉があるか」。** 結果になりうる枝 (三項の両辺と
// `??` / `||` の各項) をすべて取り出し、**全部**が proxy 呼び出しか既知の葉で
// あることを求める。**三項の条件部は結果にならないので枝に数えない** — 数えると
// `a > b ? getProxiedImageUrl(a) : getProxiedImageUrl(b)` という正しい式が
// 条件部 `a > b` のせいで落ちる (実測)。
func (l proxyLeaves) ok(expr string) bool {
	branches := resultBranches(expr)
	if len(branches) == 0 {
		return false
	}
	for _, branch := range branches {
		if !l.leafOK(branch) {
			return false
		}
	}
	return true
}

// leafOK reports whether a single branch is proxied, allowing surrounding
// parentheses and outer wrappers like getStaticImageUrl().
func (l proxyLeaves) leafOK(expr string) bool {
	expr = strings.TrimSpace(expr)
	for strings.HasPrefix(expr, "(") && matchingParen(expr, 0) == len(expr)-1 {
		expr = strings.TrimSpace(expr[1 : len(expr)-1])
	}
	if slices.Contains(l, expr) {
		return true
	}
	for _, wrapper := range []string{"getProxiedImageUrl(", "getStaticImageUrl("} {
		if !strings.HasPrefix(expr, wrapper) {
			continue
		}
		if matchingParen(expr, len(wrapper)-1) != len(expr)-1 {
			continue
		}
		if wrapper == "getProxiedImageUrl(" {
			return true
		}
		// `getStaticImageUrl(...)` は中身が proxy でなければ意味がない。
		return l.ok(expr[len(wrapper) : len(expr)-1])
	}
	return false
}

// isProxiedExpr reports whether expr is proxied with no named indirection.
func isProxiedExpr(expr string) bool { return proxyLeaves(nil).ok(expr) }

// resultBranches returns every sub-expression that can become the value of expr.
func resultBranches(expr string) []string {
	expr = strings.TrimSpace(expr)
	for strings.HasPrefix(expr, "(") && matchingParen(expr, 0) == len(expr)-1 {
		expr = strings.TrimSpace(expr[1 : len(expr)-1])
	}
	if q, colon := topLevelTernary(expr); q >= 0 {
		return append(resultBranches(expr[q+1:colon]), resultBranches(expr[colon+1:])...)
	}
	if parts := splitFallbacks(expr); len(parts) > 1 {
		var out []string
		for _, part := range parts {
			out = append(out, resultBranches(part)...)
		}
		return out
	}
	return []string{expr}
}

// topLevelTernary locates the `?` and its matching `:` of a top-level ternary,
// or returns -1, -1.
func topLevelTernary(expr string) (int, int) {
	depth, q, nested := 0, -1, 0
	for i := 0; i < len(expr); i++ {
		switch expr[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case '?':
			if depth > 0 {
				break
			}
			// `?.` (optional chaining) と `??` は三項ではない。
			if i+1 < len(expr) && (expr[i+1] == '.' || expr[i+1] == '?') {
				i++
				break
			}
			if q < 0 {
				q = i
			} else {
				nested++
			}
		case ':':
			if depth > 0 || q < 0 {
				break
			}
			if nested > 0 {
				nested--
				break
			}
			return q, i
		}
	}
	return -1, -1
}

// splitFallbacks splits expr on top-level `??` and `||`.
func splitFallbacks(expr string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(expr); i++ {
		switch expr[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case '?', '|':
			if depth == 0 && i+1 < len(expr) && expr[i+1] == expr[i] {
				out = append(out, expr[start:i])
				i++
				start = i + 1
			}
		}
	}
	return append(out, expr[start:])
}

// matchingParen returns the index of the `)` that closes the `(` at or after i,
// or -1.
func matchingParen(expr string, open int) int {
	depth := 0
	for i := open; i < len(expr); i++ {
		switch expr[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// feRoot returns the fork frontend's `packages` directory.
func feRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRootDir(t), "frontend", "packages")
}

// readFrontendSource reads a file under `packages/` with comments removed.
//
// **コメントアウトして残すのは消すのと同じ。** `//` / `/* */` / `<!-- -->` の
// 3 種を落としてから探す (#2762 / #2932 の gate と同じ判断)。
func readFrontendSource(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(feRoot(t), rel))
	require.NoErrorf(t, err, "%s を読めない", rel)
	return stripComments(string(raw))
}

// readFrontendRaw is readFrontendSource without comment stripping.
//
// **`:src` を走査する検査はこちらを使う (1 周目レビュー M3)。** `//.*$` は
// 文字列リテラルの `//` も落とすので、`:alt="'//'"` のような属性が同じ行にあると
// `:src` ごと消えて検査から静かに外れる (偽陰性)。
func readFrontendRaw(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(feRoot(t), filepath.FromSlash(rel)))
	require.NoErrorf(t, err, "%s を読めない (#2964 のゲートが想定するファイルが無い)", rel)
	return string(raw)
}

// blockAfter returns the source between marker and the first terminator after it.
//
// **関数の中身をまとめて見るために要る。** 「ファイルのどこかに
// `getProxiedImageUrl` がある」で判定すると、**別の用途で 1 回呼んでいるだけ**の
// ファイルで免除が成立する (#2957 の 1 周目がこれで失敗した)。marker が
// 見つからないこと自体も検出したい変異 (= 定義を 1 行の式に戻す形) なので、
// 呼び出し側で落とす。
func blockAfter(src, marker, terminator string) (string, bool) {
	start := strings.Index(src, marker)
	if start < 0 {
		return "", false
	}
	rest := src[start+len(marker):]
	end := strings.Index(rest, terminator)
	if end < 0 {
		return "", false
	}
	return src[start : start+len(marker)+end], true
}

// enclosingObject returns the brace-balanced object literal containing idx.
//
// **同じ行にあるかで判定しない。** グリッドの列定義は 1 行で書かれることも
// 複数行に割られることもある (`custom-emojis-manager.local.list.vue` が後者)。
// 行で見ると、整形で割られた瞬間にその列が検査対象から静かに消える。
func enclosingObject(src string, idx int) string {
	depth, start := 0, -1
	for i := idx - 1; i >= 0; i-- {
		switch src[i] {
		case '}':
			depth++
		case '{':
			if depth == 0 {
				start = i
			} else {
				depth--
			}
		}
		if start >= 0 {
			break
		}
	}
	if start < 0 {
		return ""
	}
	depth = 0
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	return src[start:]
}

// gridImageColumnFiles walks the frontend sources and returns the files that
// declare a grid column of `type: 'image'`.
//
// **`bindTo:` の同居で選り分ける。** `GridColumnSetting` は `bindTo` を必須で
// 持つので、同じオブジェクトリテラルに `bindTo:` があるものだけがグリッドの
// 画像セル。透かし (`MkWatermarkEditorDialog`) や Page ブロックの
// `type: 'image'` は別物で、上流の都合で増減するため巻き込むと追従のたびに
// 落ちる。
func gridImageColumnFiles(t *testing.T) (files []string, scanned int) {
	t.Helper()
	root := feRoot(t)
	for _, pkg := range []string{"frontend", "frontend-embed"} {
		base := filepath.Join(root, pkg, "src")
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || (filepath.Ext(path) != ".vue" && filepath.Ext(path) != ".ts") {
				return nil
			}
			scanned++
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			// **列を数えるときは `//` を落とさない (レビュー M3)。**
			// `lineCommentRe` は文字列リテラルの `//` も落とすので、同じ行に
			// URL があると `type: 'image'` ごと消えて**列を数え落とす**
			// (= 閉世界の主張が崩れる、偽陰性)。コメントアウトされた列を
			// 数えてしまう側 (落ちる側) に倒すほうが安全。
			src := htmlCommentRe.ReplaceAllString(string(raw), "")
			src = blockCommentRe.ReplaceAllString(src, "")
			// **1 ファイル 1 件に丸めない。** 既に名指ししているファイルの
			// 中に 2 本目の画像列が生えると、ファイル名の集合は変わらないので
			// 素通りする (変異検証で実測した唯一の見逃し)。列 1 本につき
			// 1 エントリを積んで、件数まで突き合わせる。
			for _, loc := range gridImageColRe.FindAllStringIndex(src, -1) {
				if !strings.Contains(enclosingObject(src, loc[0]), "bindTo") {
					continue
				}
				rel, _ := filepath.Rel(root, path)
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
		require.NoErrorf(t, err, "%s を走査できない", base)
	}
	sort.Strings(files)
	return files, scanned
}

// filesContaining returns the frontend files whose source contains needle.
func filesContaining(t *testing.T, needle string) []string {
	t.Helper()
	root := feRoot(t)
	var out []string
	base := filepath.Join(root, "frontend", "src")
	err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || (filepath.Ext(path) != ".vue" && filepath.Ext(path) != ".ts") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(stripComments(string(raw)), needle) {
			rel, _ := filepath.Rel(root, path)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	require.NoErrorf(t, err, "%s を走査できない", base)
	sort.Strings(out)
	return out
}

// 正しい直し方。**画面によって違うので 2 つある。**
//
// `noFallback` (第 4 引数) は media proxy が失敗したときに元の URL へ落とすのを
// 止める指定で、**`@error` の受け皿がある画面でだけ妥当**。受け皿が無いところで
// 渡すと、proxy が 403/404/500 を返した瞬間に壊れ画像アイコンと長大な proxy URL
// (alt) がそのまま出る。#2957 の失敗メッセージはこれを区別せず `noFallback` を
// 勧めており、**指示どおり直すと落ち続ける**状態だった。
const (
	// **「こう直せ」を形まで含めて書く。** #2957 の失敗メッセージは
	// `getProxiedImageUrl(...)` を勧めるだけで、ゲートが**どの形**を期待して
	// いるかを言わなかったので、指示どおり直しても落ち続けた。ゲートは
	// リテラルで形を固定しているので、その形をそのまま書く。
	fixWithFallback = "media proxy を通すこと: `getProxiedImageUrl(<url>, 'emoji')` " +
		"(静止画設定を見るなら `getStaticImageUrl()` で包む)。" +
		"**`noFallback` (第 4 引数) は渡さないこと** — この画面には `@error` の受け皿が無いので、" +
		"proxy が失敗したときに壊れ画像アイコンが出るだけになる。" +
		"**書き方を変えたいときはこのゲートの needle も更新すること** (形をリテラルで固定している)"
	fixSummary = "リモート由来の URL は media proxy を通すこと (`getProxiedImageUrl`)。" +
		"`noFallback` (第 4 引数) を渡してよいのは `<img @error>` の受け皿がある画面だけ"
	fixNoFallback = "media proxy を通すこと: `getProxiedImageUrl(<url>, 'emoji', false, true)`。" +
		"この画面は `<img @error>` で代替表示に落とすので `noFallback` を渡してよい。" +
		"**書き方を変えたいときはこのゲートの needle も更新すること** (形をリテラルで固定している)"
)

// localConstRe matches a single-expression `const` binding inside a function body.
var localConstRe = regexp.MustCompile(`(?m)^\s*const\s+([A-Za-z_$][\w$]*)\s*=\s*([^;]+);`)

// returnExprRe matches a `return <expr>;` statement.
var returnExprRe = regexp.MustCompile(`(?m)^\s*(?:.*\breturn\s+)([^;]+);`)

// returnExprs returns the expressions an arrow-function body can yield.
//
// **簡潔形も受ける (1 周目レビュー H3)。** `() => getStaticImageUrl(getProxiedImageUrl(...))`
// は**正しい書き方**なのに、`return` 文だけを探すと 1 つも読めずに落ちる。
// #2957 の 2 周目が取り下げになったのがこの型の偽陽性。
func returnExprs(body string) []string {
	body = strings.TrimSpace(body)
	if i := strings.Index(body, "=>"); i >= 0 {
		if rhs := strings.TrimSpace(body[i+2:]); !strings.HasPrefix(rhs, "{") {
			return []string{strings.TrimSuffix(rhs, ";")}
		}
	}
	var out []string
	for _, m := range returnExprRe.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// srcBindings returns every bound `src` expression in a template.
//
// **`<img>` に絞らない。** タグの範囲を正規表現で切ろうとすると、属性値の中の
// `>` (`:src="a > b ? x : y"` は実在する形) で途中から見えなくなる。`:src` は
// 画像系のコンポーネントにも付くので、全部拾って判定側で振り分けるほうが広く、
// 実測でも対象 4 ファイルの `:src` は 8 件すべて `<img>` のものだった。
func srcBindings(src string) []string {
	var out []string
	for _, m := range srcBindingRe.FindAllStringSubmatch(src, -1) {
		out = append(out, strings.TrimSpace(m[1][1:len(m[1])-1]))
	}
	return out
}

// requireProxiedSrcBindings asserts that every bound `src` in a remote-facing
// screen is either a known indirection or an expression that can only yield a
// media-proxy URL.
//
// **完全一致の集合にはしない。** `:src="getProxiedImageUrl(e.publicUrl, 'emoji')"`
// は**正しい書き方**で、#2957 の 2 周目はこれを偽陽性で落として取り下げになった。
// 許すのは「名指しした間接参照」か「全ての枝が proxy 呼び出し」のどちらかで、
// `:src="app.url"` (#2935 の修正前の形) はどちらでもないので落ちる。
//
// **`@error` の有無は見ない (1 周目レビュー H4)。** 受け皿の有無と `noFallback` の
// 要否を同じ検査で強制すると、「受け皿を外せ」と「第 4 引数を渡せ」が同時に立って
// **どちらに従っても落ちる**状態になった。別の関心事なので混ぜない。
//
// **コメント除去を通していないソースを渡すこと (1 周目レビュー M3)。** `//.*$` は
// 文字列リテラルの `//` も落とすので、`:alt="'//'"` を持つ `<img>` が丸ごと消えて
// **検査から静かに外れる** (偽陰性)。
func requireProxiedSrcBindings(t *testing.T, rel, raw string, approved ...string) {
	t.Helper()
	// HTML コメントだけは落とす (コメントアウトした `<img>` は消したのと同じ)。
	src := htmlCommentRe.ReplaceAllString(raw, "")
	bindings := srcBindings(src)
	// **1 つも拾えなかったら落とす。** 書式が変わって空振りすると、
	// 検査していないのに緑になる。
	require.NotEmptyf(t, bindings, "%s から :src の束縛を 1 つも読めなかった (書式が変わった?)", rel)
	leaves := proxyLeaves(approved)
	for _, expr := range bindings {
		require.Truef(t, leaves.ok(expr),
			"%s が :src=%q を画像として表示している。名指ししていない値をそのまま画像に"+
				"出すと、それがリモート由来だったとき CSP (#2425) で黙って消える "+
				"(#2935 の修正前の形は `:src=\"app.url\"` そのもの)。\n%s\n"+
				"proxy 済みの値を作る computed/helper を経由するなら、**その名前をこの gate の"+
				"approved へ足すこと** (このファイルの %s) (#2964)",
			rel, expr, fixSummary, t.Name())
	}
}

// gridImageColumns is the closed world of grid `type: 'image'` columns.
//
// **1 列 1 エントリで、いまはどのファイルも 1 本ずつ。** 同じファイルに 2 本目が
// 生えたら重複して落ちる (ファイル名だけの集合では素通りする)。
//
// **4 つしかなく、すべてカスタム絵文字管理。** グリッドの画像セルは
// `MkDataCell.vue` が `<img :src="cell.value">` を出すだけで **`@error` を
// 持たない**ので、ここへ渡す URL は「proxy 済み」か「自オリジン」でなければ
// ならない。増えたらこの一覧を更新し、その供給元を下の検査に足すこと。
var gridImageColumns = []string{
	"frontend/src/pages/admin/custom-emojis-manager.local.list.vue",
	"frontend/src/pages/admin/custom-emojis-manager.logs.vue",
	"frontend/src/pages/admin/custom-emojis-manager.register.vue",
	"frontend/src/pages/admin/custom-emojis-manager.remote.vue",
}

// TestRemoteImagesGoThroughMediaProxy asserts that every screen which renders a
// remote-origin URL as an image routes it through the media proxy (#2964).
//
// **mk-go の CSP は upstream に無い (#2425)。** `img-src 'self' data: blob:` を
// enforce しているので、相手サーバーのオリジンの画像は**1 件も表示されない**。
// エラーもログも出ず、開発者の手元 (CSP 無効) では再現しないので、本番で
// 初めて気付く。同じ型を 3 回踏んでいる:
//
//   - #2903 `const imgUrl = computed(() => props.emoji.url)` (インポートのモーダル)
//   - #2935 `<img :src="app.url">` (絵文字申請の審査画面)
//   - #2957 `url: it.publicUrl` (リモート絵文字の一覧グリッド)
//
// **名前で探す形は機能しない (#2957 の取り下げで実証済み)。** 上の 3 件で
// `publicUrl` / `originalUrl` という名前が出るのは 1 件だけで、他は
// `props.emoji.url` / `app.url` という別名で受けている。
//
// そこで **供給元を名指しする wiring 型**にしてある
// (`TestReactableRemoteReactionIsWired` と同じ形)。ヘルパー関数に切り出しても
// 逃げられないよう、**ヘルパーの中身まで辿る**のが要点 — #2957 は
// `emojiImageUrl()` に切り出した結果、gate が自力で評価する行が 0 行になった。
//
// **描画側 (`:src`) も見る。** 供給元の pin だけだと、`previewUrls` を正しく
// 保ったまま `<img :src="app.url">` を**足す / 戻す**形 (= #2935 の修正前その
// もの) が素通りする。ただし `:src` の判定は 2 周とも失敗している — 部分一致は
// 同じ式の無関係な呼び出しで免除され (#2957 の 1 周目、2 周目のこのゲートの
// 回帰も同型)、「式全体がちょうど 1 つの呼び出しか」は括弧や三項という正しい
// 書き方を偽陽性で落とした (#2957 の 2 周目)。いまは `proxyLeaves.ok` が
// **結果になりうる枝をすべて取り出して、生の葉が 1 つでもあれば落とす**。
//
// **一般に「リモート由来か」を静的に判定することはできない。** `publicUrl` は
// ローカル絵文字なら自オリジン、リモートなら相手のオリジンで、型も名前も同じ。
// だから「どの画面がリモートを出すか」を名指しするしかない。逆に、ローカル由来
// しか出さない画面 (`register.vue` は自分の drive、`local.list.vue` は
// ローカル絵文字) を proxy 必須にするのは**誤り**なので、そちらは「ローカルの
// ままであること」を固定する。
func TestRemoteImagesGoThroughMediaProxy(t *testing.T) {
	probe := filepath.Join(feRoot(t), "frontend", "src", "pages", "admin", "custom-emojis-manager.remote.vue")
	if _, err := os.Stat(probe); err != nil {
		require.NoError(t, err, "frontend/ は追跡しているので、%s を読めないのはパスの誤り", probe)
	}

	t.Run("grid image columns are a closed world", func(t *testing.T) {
		// **一覧が増えたことに気付けないと、この gate の前提が崩れる。**
		// 名指し型は「名指しした先しか見ない」ので、5 つ目の画像列が生えた
		// 瞬間に無検査の経路ができる。**空振りの検出もここが担う** — 書式が
		// 変わって正規表現が 1 つも拾わなくなれば集合が空になり、落ちる。
		found, scanned := gridImageColumnFiles(t)
		require.Greaterf(t, scanned, 500, "走査したファイルが %d 件しかない。走査範囲が壊れている", scanned)
		require.Equalf(t, gridImageColumns, found,
			"グリッドの image 列の一覧が変わった。\n"+
				"新しい列の供給元がリモート由来なら %s。\n"+
				"ローカル由来 (自分の drive / ローカル絵文字) なら proxy は不要。\n"+
				"どちらにせよ %s の gridImageColumns と供給元の検査を更新すること (#2964)",
			fixWithFallback, t.Name())
	})

	t.Run("remote emoji grid goes through the proxy", func(t *testing.T) {
		const rel = "frontend/src/pages/admin/custom-emojis-manager.remote.vue"
		src := readFrontendSource(t, rel)

		// #2957 の修正前の形そのもの。**ここが本体。**
		const wantGridURL = "url: emojiImageUrl(it.publicUrl),"
		require.Containsf(t, src, wantGridURL,
			"%s のグリッドが publicUrl を proxy に通していない (%q が無い)。リモート絵文字の"+
				"publicUrl は相手サーバーのオリジンなので、CSP (#2425) を enforce している"+
				"構成では**1 件も表示されない**。%s (#2957)", rel, wantGridURL, fixWithFallback)
		require.NotContainsf(t, src, "url: it.publicUrl,",
			"%s が publicUrl を生のままグリッドへ渡している (#2957 の修正前の形)。%s", rel, fixWithFallback)

		// **ヘルパーの中身まで見る。** 任意の名前の関数で包めば見えなくなる、
		// というのが #2957 の取り下げ理由だった。中身が proxy を通さなく
		// なったらここで落ちる。
		body, ok := blockAfter(src, "function emojiImageUrl(url: string): string {", "\n}")
		require.Truef(t, ok,
			"%s の emojiImageUrl の定義を読めない。グリッドへ渡す URL を作るヘルパーなので、"+
				"消した / 1 行の式に戻したなら %s", rel, fixWithFallback)
		const wantProxyCall = "getProxiedImageUrl(url, 'emoji')"
		require.Containsf(t, body, wantProxyCall,
			"%s の emojiImageUrl が proxy を通していない (%q が無い)。%s",
			rel, wantProxyCall, fixWithFallback)
		const wantStaticCall = "getStaticImageUrl(proxied)"
		require.Containsf(t, body, wantStaticCall,
			"%s の emojiImageUrl が disableShowingAnimatedImages を無視している (%q が無い)。"+
				"`getProxiedImageUrl` の第 4 引数は noFallback であって静止画とは無関係なので、"+
				"`getStaticImageUrl()` で包むこと", rel, wantStaticCall)

		// ログのグリッドへはグリッドの行 (= proxy 済み) をそのまま流す。
		// 生の publicUrl を積み直すとログ側だけ CSP で消える。
		require.Containsf(t, src, "url: it.item.url,",
			"%s がログへ渡す URL がグリッドの行 (proxy 済み) から外れている。"+
				"生の publicUrl を積み直すとログの画像だけが消える (#2964)", rel)
	})

	t.Run("local-only grids stay local", func(t *testing.T) {
		// **ローカル由来だけを出す画面を proxy 必須にしない。** 自オリジンの
		// URL は CSP の `'self'` で通るので proxy は不要で、通すと無駄な
		// ラウンドトリップと allowlist 依存を増やすだけ。ここで固定するのは
		// 「供給元がローカルのままであること」で、リモートに化けたら落ちる。
		for _, c := range []struct {
			rel  string
			want []string
			why  string
		}{
			{
				rel: "frontend/src/pages/admin/custom-emojis-manager.register.vue",
				want: []string{
					"function fromDriveFile(it: Misskey.entities.DriveFile): GridItem {",
					"url: it.url,",
				},
				why: "自分の drive のファイルなので自オリジン",
			},
			{
				rel: "frontend/src/pages/admin/custom-emojis-manager.local.list.vue",
				want: []string{
					"hostType: 'local',",
					"url: it.publicUrl,",
				},
				why: "ローカル絵文字だけを引くクエリなので publicUrl は自オリジン",
			},
		} {
			src := readFrontendSource(t, c.rel)
			for _, want := range c.want {
				require.Containsf(t, src, want,
					"%s に %q が無い。この画面の供給元は %s。"+
						"リモート由来に変わったなら %s、変わっていないならこの gate の想定を更新すること (#2964)",
					c.rel, want, c.why, fixWithFallback)
			}
		}
	})

	t.Run("registration log grid has only the known suppliers", func(t *testing.T) {
		// ログのグリッドは 3 画面で共有している。**供給元が増えたら気付きたい** —
		// 新しい画面がリモートの生 URL を積むと、ログの画像だけが黙って消える。
		require.Equalf(t,
			[]string{
				"frontend/src/pages/admin/custom-emojis-manager.local.list.logs.vue",
				"frontend/src/pages/admin/custom-emojis-manager.register.vue",
				"frontend/src/pages/admin/custom-emojis-manager.remote.vue",
			},
			filesContaining(t, "custom-emojis-manager.logs.vue"),
			"登録ログのグリッド (image 列を持つ) の利用側が変わった。"+
				"新しい供給元がリモート由来の URL を積むなら %s。"+
				"%s の一覧を更新すること (#2964)", fixWithFallback, t.Name())
	})

	t.Run("application review screens go through the proxy", func(t *testing.T) {
		// #2935 の修正前の形 (`<img :src="app.url">`) を固定する。
		// **供給元 (`resolvePreviewUrl` / `previewUrls`) と描画側 (`:src`) の
		// 両方を見る**ので、どちらを生の値へ戻しても、生の値を出す `<img>` を
		// **足して**も落ちる。
		const rel = "frontend/src/pages/admin/custom-emojis-manager.applications.vue"
		src := readFrontendSource(t, rel)

		body, ok := blockAfter(src, "function resolvePreviewUrl(app: Application): string | null {", "\n}")
		require.Truef(t, ok, "%s の resolvePreviewUrl の定義を読めない (#2935)", rel)
		require.Containsf(t, body, "if (!app.remoteHost) return app.url;",
			"%s がローカル / リモートを出し分けていない。自分の drive のファイルまで proxy へ回すと"+
				"無駄なラウンドトリップになる (#2935)", rel)
		require.Containsf(t, body, "getProxiedImageUrl(app.url, 'emoji', false, true)",
			"%s がリモートの URL を proxy に通していない。CSP (#2425) で審査画面の画像が"+
				"1 件も出なくなる。%s (#2935)", rel, fixNoFallback)

		// **`previewUrls` を埋める側も見る (レビュー H3)。** `:src` が参照する
		// map が `resolvePreviewUrl` で埋まっていることを固定しないと、
		// `map.set(app.id, app.url)` に戻すだけで **#2935 の修正前の挙動が
		// ゲート緑のまま復元できる** (`resolvePreviewUrl` は定義だけ残って
		// 未使用になる)。`relatedPreviewUrl` 側には同じ pin があり、ここだけ
		// 非対称だった。
		producer, ok := blockAfter(src, "const previewUrls = computed(() => {", "\n});")
		require.Truef(t, ok, "%s の previewUrls の定義を読めない (#2935)", rel)
		require.Containsf(t, producer, "resolvePreviewUrl(app)",
			"%s の previewUrls が resolvePreviewUrl を通していない。`:src` が参照する map に"+
				"生の URL を入れると、proxy を通す関数は定義だけ残って未使用になり、"+
				"審査画面の画像が CSP (#2425) で 1 件も出なくなる (#2935)", rel)

		// **描画側も見る。** producer を残したまま `<img>` の `:src` を生の値へ
		// 戻す形が #2935 の修正前そのもので、producer の pin だけでは通ってしまう。
		requireProxiedSrcBindings(t, rel, readFrontendRaw(t, rel), "previewUrls.get(app.id)!")
	})

	t.Run("related application thumbnails go through the proxy", func(t *testing.T) {
		const rel = "frontend/src/utility/emoji-application-related.ts"
		src := readFrontendSource(t, rel)

		body, ok := blockAfter(src, "export function relatedPreviewUrl(item: RelatedItem, broken: ReadonlySet<string>): string | null {", "\n}")
		require.Truef(t, ok, "%s の relatedPreviewUrl の定義を読めない (#2960)", rel)
		require.Containsf(t, body, "if (!item.remoteHost) return item.url;",
			"%s がローカル / リモートを出し分けていない (#2960)", rel)
		require.Containsf(t, body, "getProxiedImageUrl(item.url, 'emoji', false, true)",
			"%s がリモートの URL を proxy に通していない。%s (#2960)", rel, fixNoFallback)

		// **利用側の閉世界。** ヘルパーが正しくても、利用側が生の `item.url` を
		// 出せば同じことになる。新しい利用側が増えたらここで落ちる。
		consumers := []string{
			"frontend/src/pages/admin-user.emoji-applications.vue",
			"frontend/src/pages/admin/custom-emojis-manager.application-related.vue",
		}
		// 定義しているファイル自身も needle を含むので一覧に入れる。
		// 突き合わせは sort 済みの集合どうしで行う。
		expected := append([]string{rel}, consumers...)
		sort.Strings(expected)
		require.Equalf(t, expected,
			filesContaining(t, "relatedPreviewUrl"),
			"relatedPreviewUrl の利用側が変わった。新しい画面もサムネイルを"+
				"このヘルパー経由で解決させ、%s の一覧を更新すること (#2964)", t.Name())

		for _, crel := range consumers {
			csrc := readFrontendSource(t, crel)
			require.Containsf(t, csrc, "map.set(item.id, relatedPreviewUrl(item, brokenPreviews.value))",
				"%s がサムネイルを relatedPreviewUrl で解決していない。`:src` が参照する map に"+
					"生の URL を入れると、proxy を通す関数は定義だけ残って未使用になる。%s", crel, fixNoFallback)
			requireProxiedSrcBindings(t, crel, readFrontendRaw(t, crel), "previewUrls.get(item.id)!")
		}
	})

	t.Run("remote emoji dialog goes through the proxy", func(t *testing.T) {
		// #2903 の修正前の形 (`const imgUrl = computed(() => props.emoji.url)`)。
		// **定義が 1 行の式に戻ると marker ごと消える**ので、そこで落ちる。
		const rel = "frontend/src/components/MkRemoteEmojiEditDialog.vue"
		src := readFrontendSource(t, rel)

		// **式全体を取る (2 周目レビュー H2)。** 「1 行だけ読む」形にすると、
		// 同じ行に proxy 呼び出しが 1 つあるだけで免除される — `props.emoji.url ??
		// getStaticImageUrl(...)` のような**ありがちな取り違え**が素通りした。
		// これは #2957 の 1 周目が踏んだ「同じ行の無関係な呼び出しで免除される」
		// そのもの。括弧の対応で `computed(...)` の中身を丸ごと切り出す。
		//
		// **ブロック形は強制しない (1 周目レビュー H3)。** 強制すると
		// `computed(() => getStaticImageUrl(getProxiedImageUrl(...)))` という
		// **正しい書き方**を「#2903 の修正前の形だ」と誤診断する。
		const marker = "const imgUrl = computed("
		at := strings.Index(src, marker)
		require.GreaterOrEqualf(t, at, 0,
			"%s に `%s` が無い。名前を変えたならこのゲートの needle も更新すること (#2903)",
			rel, marker)
		openIdx := at + len(marker) - 1
		endIdx := matchingParen(src, openIdx)
		require.Greaterf(t, endIdx, openIdx,
			"%s の imgUrl の括弧が閉じていない (#2903)", rel)
		body := src[openIdx+1 : endIdx]
		// **部分一致で見ない (2 周目レビュー H2)。** 「proxy 呼び出しが body に
		// 含まれるか」だと、`props.emoji.url ?? getStaticImageUrl(getProxiedImageUrl(...))`
		// のような**ありがちな取り違え**が素通りする (実測で見逃した)。
		// `return` する式ごとに「proxy を通らない葉があるか」を見る。
		// ローカルの `const` は初期化式が proxy 済みなら葉として許す
		// (実コードは `const proxied = getProxiedImageUrl(...)` を経由する)。
		leaves := proxyLeaves{"null"}
		for _, m := range localConstRe.FindAllStringSubmatch(body, -1) {
			if leaves.ok(m[2]) {
				leaves = append(leaves, m[1])
			}
		}
		returns := returnExprs(body)
		require.NotEmptyf(t, returns, "%s の imgUrl から返り値の式を 1 つも読めない (#2903)", rel)
		for _, expr := range returns {
			require.Truef(t, leaves.ok(expr),
				"%s の imgUrl が `%s` を返している (proxy を通らない葉がある)。"+
					"リモート絵文字の URL は相手サーバーのオリジンなので、CSP (#2425) を"+
					"enforce している構成では 4 タイルすべてが表示されない。%s (#2903)",
				rel, strings.TrimSpace(expr), fixWithFallback)
		}
		require.Containsf(t, body, "getProxiedImageUrl(props.emoji.url, 'emoji')",
			"%s の imgUrl が props.emoji.url を proxy に通していない。%s (#2903)",
			rel, fixWithFallback)
		require.Containsf(t, body, "getStaticImageUrl(",
			"%s の imgUrl が disableShowingAnimatedImages を無視している "+
				"(`getStaticImageUrl()` で包むこと)", rel)

		requireProxiedSrcBindings(t, rel, readFrontendRaw(t, rel), "imgUrl")
	})
}

// 取りこぼす形 (このゲートでは捕まらないもの):
//
//   - **名指ししていない画面**でリモートの生 URL を `<img>` に出す形。一般の
//     「リモート由来か」判定は静的にはできない (`publicUrl` はローカルなら
//     自オリジン) ので、新しい画面は人が判断してこの一覧へ足すしかない。
//     グリッドの画像列だけは閉世界で数えているので、そちらは足し忘れても落ちる。
//   - **`:src` の値を変数で組み立てる形。** 走査は式を静的に見て「proxy を
//     通らない葉があるか」だけを判定するので、`const u = app.url` を経由して
//     `:src="u"` と書かれると**変数の中身**までは追えない。名指しした中間参照
//     (`previewUrls` / `imgUrl`) はその中身を別の pin で押さえているが、
//     approved へ名前を足すときは同じように供給元も pin すること。
//   - **`:src` を持たない画像** (静的な `src=`)。自オリジンの資産なので proxy は
//     不要で、走査対象にも入らない。
//   - `:src` 以外の経路 (`background-image` などの CSS、`fetch` して blob 化する
//     形、`:style` に URL を埋める形)。
//   - 供給元のさらに上流 (API のレスポンスや、別ファイルの computed が
//     `previewUrls` に混ぜ込む形)。
//   - グリッド列を `{ ...base, type: 'image' }` の形で書く形 (spread の先を
//     追えないので `bindTo` の同居を判定できない)。
//   - `packages/sw` / `packages/frontend-shared` など、走査範囲の外。
//
// **rename は偽陽性ではない。** `previewUrls` を `previews` に変えると approved に
// 無い名前になって落ちるが、それは**設計どおり** — 中身が proxy 済みだという保証は
// 名前に紐付いた別の pin が持っているので、名前を変えたら両方を更新する。失敗
// メッセージがそのとおりに指示する。

// TestRemoteImageProxyGateClassifiesSources pins the classification logic with
// synthetic sources (#2964).
//
// **実コードだけを見ていると、検出する側の枝が一度も実行されない。** いまの
// frontend/ は全て正しい書き方なので、`enclosingObject` の「`bindTo` を持たない
// `type: 'image'` を除外する」枝も、`isProxiedExpr` の「proxy を通らない葉を
// 見つける」枝も一度も通らない。その状態だと**判定を常に真 / 常に偽へ書き換える
// 変異が素通りする**。#2792 の `secretfield-check` が「検出ロジックは人工の
// ソースで固定する」と結論したのと同じ理由。
func TestRemoteImageProxyGateClassifiesSources(t *testing.T) {
	t.Run("grid columns are told apart from other type: 'image'", func(t *testing.T) {
		for name, c := range map[string]struct {
			src  string
			grid bool
		}{
			"grid column (one line)": {
				src: `cols: [{ bindTo: 'url', icon: 'ti-icons', type: 'image', editable: false, width: 'auto' }],", `, grid: true},
			"grid column (split across lines)": {
				src: "cols: [\n{\nbindTo: 'url',\nicon: 'ti-icons',\ntype: 'image',\nwidth: 'auto',\n},\n]", grid: true},
			// 透かしや Page ブロックの `type: 'image'`。上流の都合で増減するので
			// 巻き込むと追従のたびに落ちる。
			"watermark layer":  {src: `const layer = { id: genId(), type: 'image', opacity: 1 };`, grid: false},
			"type declaration": {src: `block: Extract<Misskey.entities.PageBlock, { type: 'image' }>,`, grid: false},
			// **同じファイルの別の場所に bindTo があっても巻き込まない (レビュー L1)。**
			// 境界を見ずに全文から探す形に退行すると、グリッドを持つファイルの
			// 無関係な `type: 'image'` まで列として数える。
			"bindTo elsewhere in the file": {
				src: "cols: [{ bindTo: 'name', type: 'text' }],\nconst layer = { type: 'image' };", grid: false},
			// **キーを quote した書き方も列として数える。** 正規表現は
			// `['"]?type['"]?` で受けているが、実コードに 1 件も無いので
			// ここで固定しないと**その分岐を外しても緑のまま**になる。
			"quoted key": {
				src: `cols: [{ bindTo: 'url', 'type': 'image' }],`, grid: true},
			"double-quoted value": {
				src: `cols: [{ bindTo: "url", type: "image" }],`, grid: true},
		} {
			loc := gridImageColRe.FindStringIndex(c.src)
			require.NotNilf(t, loc, "%s: type: 'image' を拾えない", name)
			got := strings.Contains(enclosingObject(c.src, loc[0]), "bindTo")
			require.Equalf(t, c.grid, got, "%s の判定", name)
		}
	})

	t.Run("src expressions are told apart", func(t *testing.T) {
		// **偽陽性と偽陰性の両側を固定する。** #2957 は 1 周目が緩すぎて
		// (同じ式に proxy 呼び出しが 1 つあれば免除)、2 周目が厳しすぎて
		// (式全体がちょうど 1 つの呼び出しでなければ落とす) 取り下げになった。
		// いまの判定は「三項と `??` / `||` で枝に割り、**すべての枝**が proxy
		// 呼び出しか」を見る。
		for name, c := range map[string]struct {
			expr    string
			proxied bool
		}{
			"bare proxy call":       {expr: `getProxiedImageUrl(app.url, 'emoji')`, proxied: true},
			"wrapped in parens":     {expr: `(getProxiedImageUrl(app.url, 'emoji'))`, proxied: true},
			"static wraps proxy":    {expr: `getStaticImageUrl(getProxiedImageUrl(u, 'emoji'))`, proxied: true},
			"both ternary branches": {expr: `x ? getProxiedImageUrl(a) : getProxiedImageUrl(b)`, proxied: true},
			// 折り返した正しい式で落ちないこと (実コードは 1 行だが、
			// 属性が増えれば折り返す)。
			"wrapped across lines": {
				expr: "x\n? getProxiedImageUrl(a)\n: getProxiedImageUrl(b)", proxied: true},
			// 属性値の中の `>` で切らないこと。
			"comparison inside": {expr: `a > b ? getProxiedImageUrl(a) : getProxiedImageUrl(b)`, proxied: true},

			// `?.` を三項の `?` と取り違えないこと。
			"optional chaining": {expr: `getProxiedImageUrl(app?.url, 'emoji')`, proxied: true},
			"nested ternary": {
				expr: `a ? b ? getProxiedImageUrl(x) : getProxiedImageUrl(y) : getProxiedImageUrl(z)`, proxied: true},

			"raw url": {expr: `app.url`, proxied: false},
			"nested ternary with raw branch": {
				expr: `a ? b ? getProxiedImageUrl(x) : app.url : getProxiedImageUrl(z)`, proxied: false},
			"named indirection":   {expr: `previewUrls.get(app.id)!`, proxied: false},
			"one raw branch":      {expr: `x ? getProxiedImageUrl(a) : app.url`, proxied: false},
			"raw fallback":        {expr: `app.url ?? getProxiedImageUrl(a)`, proxied: false},
			"raw or":              {expr: `app.url || getProxiedImageUrl(a)`, proxied: false},
			"static wraps raw":    {expr: `getStaticImageUrl(app.url)`, proxied: false},
			"proxy call as arg":   {expr: `pick(getProxiedImageUrl(a), app.url)`, proxied: false},
			"proxy call in parts": {expr: `getProxiedImageUrl(a) + app.url`, proxied: false},
		} {
			require.Equalf(t, c.proxied, isProxiedExpr(c.expr), "%s の判定", name)
		}
	})

	t.Run("bound src attributes are collected", func(t *testing.T) {
		// **属性値の中の `>` と改行で切れないこと。** `<img>` のタグ範囲を
		// 正規表現で切る実装はここで壊れる (偽陰性)。
		const tpl = `
<img v-if="ok" :src="a > b ? getProxiedImageUrl(a) : getProxiedImageUrl(b)" :alt="n"/>
<img
	v-bind:src='imgUrl'
	:alt="n"
/>
<img src="/static-assets/x.png"/>
`
		require.Equal(t, []string{
			`a > b ? getProxiedImageUrl(a) : getProxiedImageUrl(b)`,
			`imgUrl`,
		}, srcBindings(tpl))
	})
}
