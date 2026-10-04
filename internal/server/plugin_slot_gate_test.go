package server

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// SlotName は union 型なので、**宣言だけして描画先を置き忘れても vue-tsc は
// 通る** (#2915)。症状は「プラグインがそのスロットに出ない」だけで、エラーも
// ログも出ない。プラグイン側は自分の不具合を疑うことになる。
//
// Go 側の `wiring-check` (#2762) が見ているのと同じ「宣言したのに配線して
// いない」の形なので、同じように検査する。見るのは次の 3 つで、**どれが欠けても
// 同じ「出ないが緑」になる** (ただし「どこに置いたか」は見ない。下の既知の
// 取りこぼしを参照):
//
//  1. `SlotName` に宣言があること
//  2. `<MkPluginSlot name="...">` がどこかにあること
//  3. **そのファイルが MkPluginSlot を import していること**
//
// **このゲートは `make check` でも、CI の required の `test` でも走る。** frontend/
// を本体で追跡するようになった (#3379) ので、以前のように「submodule を checkout
// する `frontend-check` だけが検査する」(#2892) 形ではない。
//
// 3 を見るのは、`MkPluginSlot.vue` が `components/global/` ではないので
// **グローバル登録されていない**ため。import を落とすと Vue は解決できない
// まま素の要素として描画し、production ビルドでは警告すら出ない。
// vue-tsc も eslint も緑のままになることを実測した (レビュー H2)。兄弟の
// `reaction_longpress_gate_test.go` が import と呼び出しの両方を見ているのと
// 同じ形。
//
// **形を決め打たない (レビュー H1)。** 当初は `'[a-z-]+:[a-z-]+'` だけを
// 宣言として拾っていたが、その形から外れた名前 (camelCase・数字・3 段・
// アンダースコア) と、`| ExtraSlot` のような別名 union のメンバーが**丸ごと
// 検査対象から外れて緑になる**ことを実測された。全滅しないので NotEmpty では
// 気付けない。宣言部分を切り出した後は中身が union のメンバーしか無いので、
// **リテラルを全部拾い、リテラル以外が残ったら落とす**。
//
// **既知の取りこぼし** (どれも意図的に行わないと踏めない):
//
//   - `v-if="false"` を付けた `<MkPluginSlot>` は「有る」と数える。Go 側の
//     `wiring-check` と同じ限界。
//   - `:ctx` を渡し忘れても落ちない。スロットごとに ctx が要るかは違う
//     (`settings:profile` / `admin:federation` は渡さないのが正しい) ので、
//     検査するなら「どのスロットが ctx を要求するか」という第 2 の一覧が要り、
//     それ自体が同期を要する (#2792 が allowlist で踏んだ型)。
//   - **置かれた場所は見ない (レビュー 2 周目 M2)。** `admin:user` の存在理由は
//     「見せる相手」なのに、mount を公開プロフィールへ移しても落ちない。
//     スロットごとに期待するファイルを持たせると第 2 の一覧になり、そちらの
//     同期が新しい負債になる (#2792 が allowlist で踏んだ型)。**omission
//     (配線し忘れ) は黙って起きるが、移設は誰かが意図して書く変更**なので、
//     検査ではなくレビューで捕まえる側に置く。
//   - **`<script>` 内の文字列リテラルも mount と数える (レビュー 2 周目 L1)。**
//     コメントは落とすが文字列は落とさない。意図的に書かないと踏めない。
//     import 側も同じで、テンプレートリテラルの中に import 文の形を置くと
//     「束縛されている」と数える (レビュー 3 周目 L4)。
//   - **タグ名は PascalCase 決め打ち。** Vue が許す `<mk-plugin-slot>` は
//     拾わない (レビュー 3 周目 L5)。落ちる側。
//   - **union のメンバーは単引用符のみ。** `| "admin:user"` は「別名 union?」と
//     出て落ちる (レビュー 3 周目 L2)。eslint の `@stylistic/quotes` は warn で
//     `--quiet` により止まらないので到達はしうるが、落ちる側なので放置する。
//   - **import を複数行に折り返すと拾えない** (パスと識別子が別の行になる)。
//     落ちる側。
//   - 共有の `stripComments` は `blockCommentRe` を `lineCommentRe` より先に
//     掛けるので、**行コメントの中の `/*` が次の `*/` まで食う**。宣言側で
//     これを踏むと union のメンバーが消え、残りが配線済みなら黙って通る
//     (レビュー L1 で実測)。mount 側では `(?m)//.*$` が `https://` を行末まで
//     食うが、そちらは「配線が無い」と誤って落ちる側。5 つの gate が共有する
//     ヘルパーなので、ここだけのために変えない。
var (
	slotNameRe = regexp.MustCompile(`'([^']*)'`)
	// **白リストで判定する (レビュー 2 周目 M3)。** 当初は「文字が残ったら
	// 落とす」(`[A-Za-z]`) にしていたが、TypeScript の識別子は Unicode を
	// 許すので `type 新スロット = ...` や `type _ = ...` の別名メンバーが
	// **残っているのに当たらず、そのスロットが declared から黙って消える**。
	// union の骨格 (`|` と空白と括弧) 以外が残ったら落とす形にする。
	unionNoiseRe = regexp.MustCompile(`^[|\s()]*$`)
	// **タグの終端を `>` で判定しない (レビュー M1)。** 属性値に `>` は普通に
	// 入る (`:ctx="{ n: xs.length > 0 }"`)。引用符の中は読み飛ばす。
	//
	// **`name=` の直前に空白を要求する (レビュー M2 の副作用を避ける)。**
	// 単引用符も受けるが、`:name="expr"` のような動的な渡し方は拾わない
	// (拾うと式そのものがスロット名として記録される)。
	slotMountRe  = regexp.MustCompile(`<MkPluginSlot(?:"[^"]*"|'[^']*'|[^>"'])*?\sname=["']([^"']*)["']`)
	docSlotRowRe = regexp.MustCompile("^\\|\\s*`([^`]+)`\\s*\\|")
	// **ATX 見出しは `#` の後に空白が要る (レビュー 3 周目 M2)。** 2 周目で
	// 「`#` で始まる行」を見出しとして打ち切ったところ、`#2915 で…` のような
	// issue 参照行 (tracked な md に 16 行ある) とコードフェンス内の `# コメント`
	// でも打ち切るようになり、**表は無傷なのに「1 行も拾えない」と落ちる**
	// ようになった。しかも診断が「書式が変わった?」を指すので調査が遠回りになる。
	docHeadingRe = regexp.MustCompile(`^\s{0,3}#{1,6}(?:\s|$)`)
	docFenceRe   = regexp.MustCompile("^\\s{0,3}(?:```|~~~)")
	// **import の「形」を列挙しない (レビュー 3 周目 M1)。** 2 周目で
	// `^\s*import MkPluginSlot from '...'` という形を要求したところ、この
	// frontend が実際に使っている `defineAsyncComponent(() => import('...'))`
	// (49 ファイル) と `import A, { b } from '...'` を**正しく書いているのに
	// 落とす**ようになった。形を 1 つずつ選択肢に足す直し方は、毎周隣に穴が
	// 開く型 (#2857 / #2930 が同じ結論に達している)。
	//
	// 代わりに **パスを含む行から引用符の中身を消して、識別子が残るか**だけを
	// 見る。書き方に依存しない:
	//
	//	import MkPluginSlot from '…/MkPluginSlot.vue'                → 識別子が残る ○
	//	const MkPluginSlot = defineAsyncComponent(() => import('…')) → ○
	//	import PluginSlot from '…/MkPluginSlot.vue'                  → 残らない ×
	//	const p = '…/MkPluginSlot.vue'                               → 残らない ×
	//
	// **`import type` だけは別に弾く** — 型だけの import は実行時の
	// コンポーネントを束縛しないので、識別子は残るが解決できない。
	slotPathRe     = regexp.MustCompile(`MkPluginSlot\.vue`)
	quotedRe       = regexp.MustCompile("\"[^\"]*\"|'[^']*'|`[^`]*`")
	slotIdentRe    = regexp.MustCompile(`\bMkPluginSlot\b`)
	typeOnlyImport = regexp.MustCompile(`\bimport\s+type\b`)
)

func TestEveryPluginSlotHasAMountPoint(t *testing.T) {
	fe := filepath.Join(repoRootDir(t), "frontend", "packages", "frontend")
	api := filepath.Join(fe, "src", "plugin-api.ts")
	if _, err := os.Stat(api); err != nil {
		require.NoError(t, err, "frontend/ は追跡しているので、%s を読めないのはパスの誤り", api)
	}

	declared := declaredSlotNames(t, api)
	mounted, mountedWithoutImport := mountedSlotNames(t, filepath.Join(fe, "src"))

	var missing, noImport, wired []string
	for name := range declared {
		if where, ok := mounted[name]; ok {
			wired = append(wired, name+" ("+where+")")
			continue
		}
		if where, ok := mountedWithoutImport[name]; ok {
			noImport = append(noImport, name+" ("+where+")")
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(wired)
	// **他所で正しく配線されていても、壊れた設置は報告する (レビュー 3 周目 M3)。**
	// declared を軸に短絡すると、スロット名ごとに**最初の 1 ファイルしか
	// import を守らない**。
	for name, where := range mountedWithoutImport {
		if _, okWired := mounted[name]; okWired {
			noImport = append(noImport, name+" ("+where+")")
		}
	}
	sort.Strings(missing)
	sort.Strings(noImport)

	require.Emptyf(t, missing,
		"SlotName に宣言があるのに <MkPluginSlot name=\"...\"> が 1 つも無い: %v\n"+
			"プラグインはそのスロットに描画されないが、型検査もテストも通る (#2915)\n"+
			"配線済み: %v", missing, wired)
	require.Emptyf(t, noImport,
		"<MkPluginSlot> は置かれているが、そのファイルが MkPluginSlot を default import していない: %v\n"+
			"MkPluginSlot は components/global/ ではないのでグローバル登録されておらず、\n"+
			"import が無いと解決できないまま素の要素として描画される (production では無言)",
		noImport)

	// **doc との突き合わせもここで行う (レビュー M3)。** スロット一覧は
	// `docs/plugins/authoring.md` にもあり、片側更新が起きる (#2892 / #2898 の型)。
	// ゲートを 2 本に分けず、同じ declared を使い回す。
	doc := filepath.Join(repoRootDir(t), "docs", "plugins", "authoring.md")
	documented := documentedSlotNames(t, doc)
	require.Equalf(t, sortedKeys(declared), sortedKeys(documented),
		"SlotName と %s のスロット一覧が食い違っている (#2915)", doc)
}

// declaredSlotNames extracts the members of the SlotName union.
func declaredSlotNames(t *testing.T, api string) map[string]bool {
	t.Helper()
	src := stripComments(readFileString(t, api))
	const marker = "export type SlotName ="
	i := strings.Index(src, marker)
	require.GreaterOrEqualf(t, i, 0, "plugin-api.ts から SlotName の宣言を見つけられない (書式が変わった?)")
	decl := src[i+len(marker):]
	if end := strings.Index(decl, ";"); end >= 0 {
		decl = decl[:end]
	}

	out := map[string]bool{}
	for _, m := range slotNameRe.FindAllStringSubmatch(decl, -1) {
		out[m[1]] = true
	}
	// 拾えなかったら落とす。書式が変わって空振りすると、検査していないのに
	// 緑になる (#2874 / #2828 と同じ判断)。
	require.NotEmptyf(t, out, "SlotName を 1 つも拾えない (書式が変わった?): %s", api)

	// リテラル以外のメンバー (別名 union の展開など) は、この gate が追えない。
	// 黙って検査対象から外すのではなく落とす。
	rest := slotNameRe.ReplaceAllString(decl, "")
	require.Truef(t, unionNoiseRe.MatchString(rest),
		"SlotName に文字列リテラル以外のメンバーがある (別名 union?): %q\n"+
			"そのメンバーは検査されないので、展開してリテラルで書くこと", strings.TrimSpace(rest))
	return out
}

// mountedSlotNames scans the frontend for <MkPluginSlot name="..."> sites.
//
// 2 つ目の戻り値は「置かれているが import が無い」= 描画されないもの。
func mountedSlotNames(t *testing.T, root string) (map[string]string, map[string]string) {
	t.Helper()
	ok, broken := map[string]string{}, map[string]string{}
	require.NoError(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".vue" {
			return err
		}
		// **コメントを落としてから見る。** コメントアウトして残すのは配線を
		// 消すのと同じ (#2762 / #2856 が Go 側で踏んだのと同型)。
		src := stripComments(readFileString(t, path))
		hits := slotMountRe.FindAllStringSubmatch(src, -1)
		if len(hits) == 0 {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		require.NoError(t, relErr)
		dst := ok
		if !bindsSlotComponent(src) {
			dst = broken
		}
		for _, m := range hits {
			if _, seen := dst[m[1]]; !seen {
				dst[m[1]] = rel
			}
		}
		return nil
	}))
	return ok, broken
}

// bindsSlotComponent reports whether the file binds MkPluginSlot at runtime.
//
// 判定は「MkPluginSlot.vue を含む行」ごとに、引用符の中身を消してから識別子が
// 残るかを見る。import の書き方 (静的 / 動的 / 相対 / default+named / 同一行に
// 2 文) に依存しない。**既知の取りこぼし**: import を複数行に折り返すとパスと
// 識別子が別の行になるので拾えない (落ちる側)。
func bindsSlotComponent(src string) bool {
	for _, line := range strings.Split(src, "\n") {
		if !slotPathRe.MatchString(line) || typeOnlyImport.MatchString(line) {
			continue
		}
		if slotIdentRe.MatchString(quotedRe.ReplaceAllString(line, "")) {
			return true
		}
	}
	return false
}

// documentedSlotNames reads the slot table in docs/plugins/authoring.md.
func documentedSlotNames(t *testing.T, doc string) map[string]bool {
	t.Helper()
	lines := strings.Split(readFileString(t, doc), "\n")
	i := -1
	for n, l := range lines {
		if strings.TrimSpace(l) == "### スロット" {
			i = n
			break
		}
	}
	require.GreaterOrEqualf(t, i, 0, "%s に「### スロット」の節が無い (書式が変わった?)", doc)

	out := map[string]bool{}
	started, inFence := false, false
	for _, l := range lines[i+1:] {
		// **次の見出しで打ち切る (レビュー 2 周目 L4)。** 打ち切らないと、
		// スロット表が消えたときに**文書のずっと後ろの無関係な表**を読み、
		// 「admin: true」のような意味不明な差分を出す (落ちる側ではあるが、
		// 診断が「表が無い」ではなく別の場所を指すので調査が遠回りになる)。
		if docFenceRe.MatchString(l) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if docHeadingRe.MatchString(l) {
			break
		}
		if !strings.HasPrefix(strings.TrimSpace(l), "|") {
			if started {
				break // 表が終わった
			}
			continue // 表が始まる前の地の文
		}
		started = true
		if m := docSlotRowRe.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			out[m[1]] = true
		}
	}
	require.NotEmptyf(t, out, "%s のスロット一覧から 1 行も拾えない (書式が変わった?)", doc)
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
