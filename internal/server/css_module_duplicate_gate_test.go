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

// 1 つの CSS module に同じクラス名を二度書かせない。
//
// **後から来た定義が勝つので、先に書いた側が黙って壊れる。** #2989 で実際に
// 踏んだ — `emoji-request.vue` には申請タブ用の `.preview` (`grid-template-columns:
// 96px 1fr`) が既にあり、そこへ「自分の申請」用の `.preview` を足したところ、
// 新しいほうの枠が申請タブの grid になって**画像が左カラムへ寄った**。
// 症状は「プレビューが少しずれている」だけで、**型検査も eslint も何も言わない**。
//
// **fork が触ったファイルだけを見る。** upstream の SFC には重複が普通に
// あり (意図的な上書きを含む)、全件に広げると他人の負債で落ち続ける。
// 対象は `git diff` ではなく固定の一覧 — 差分で決めると、直したあとに
// 検査対象から外れて再発しても気付けない。
var cssModuleDuplicateTargets = []string{
	"pages/emoji-request.vue",
	"pages/admin/custom-emojis-manager.applications.vue",
	"components/MkNotification.vue",
}

// cssModuleClassRe matches a top-level `.foo {` rule in a <style module> block.
//
// **行頭のものだけを見る。** 入れ子や擬似クラス (`&:hover`, `.a .b`) まで拾うと
// 正当な書き方が重複に見える。
var cssModuleClassRe = regexp.MustCompile(`(?m)^\.([A-Za-z_][A-Za-z0-9_-]*) \{`)

// cssPropertyRe matches a declaration name inside a rule body.
var cssPropertyRe = regexp.MustCompile(`(?m)^\s*([a-z-]+)\s*:`)

func TestCSSModulesHaveNoDuplicateClasses(t *testing.T) {
	fe := filepath.Join(repoRootDir(t), "frontend", "packages", "frontend", "src")
	if _, err := os.Stat(fe); err != nil {
		require.NoError(t, err, "frontend/ は追跡しているので、%s を読めないのはパスの誤り", fe)
	}

	for _, rel := range cssModuleDuplicateTargets {
		raw, err := os.ReadFile(filepath.Join(fe, rel))
		require.NoErrorf(t, err, "%s を読めない", rel)

		// `<style ... module>` の中だけを見る。scoped style は対象外
		// (同じクラス名でも DOM が違えば衝突しない、という話ではないが、
		// module の `$style.foo` は**名前が一意である前提**で参照される)。
		style := styleModuleBlock(string(raw))
		require.NotEmptyf(t, style, "%s から <style module> を読めない (書式が変わった?)", rel)

		// **同じプロパティが 2 度書かれている場合だけ落とす。**
		//
		// 同じクラスを 2 ブロックに分けて書くこと自体は upstream に普通にあり
		// (`.icon_renoteGroup` は形を定義するブロックと背景色だけのブロックに
		// 分かれている)、プロパティが被っていなければ何も壊れない。**衝突して
		// 初めて「後から来た側が勝つ」が害になる** — #2989 で踏んだのは
		// `display` と `grid-template-columns` が両方にあった形。
		blocks := cssRuleBodies(style)
		require.NotEmptyf(t, blocks, "%s からクラスを 1 つも読めない (正規表現が空振り?)", rel)

		conflicts := []string{}
		for name, bodies := range blocks {
			if len(bodies) < 2 {
				continue
			}
			count := map[string]int{}
			for _, body := range bodies {
				// 1 ブロック内の重複は数えない (同じ宣言を 2 度書くのは別の話)。
				inBlock := map[string]bool{}
				for _, m := range cssPropertyRe.FindAllStringSubmatch(body, -1) {
					inBlock[m[1]] = true
				}
				for prop := range inBlock {
					count[prop]++
				}
			}
			for prop, n := range count {
				if n > 1 {
					conflicts = append(conflicts, "."+name+" { "+prop+" }")
				}
			}
		}
		sort.Strings(conflicts)
		require.Emptyf(t, conflicts,
			"%s の CSS module で、同じクラスの同じプロパティが複数のブロックに\n"+
				"書かれている: %v\n"+
				"**後から来た定義が勝つので、先に書いた側が黙って壊れる。**\n"+
				"型検査も eslint も何も言わないので、目で見るまで気付けない (#2989 で\n"+
				"プレビューの枠が別用途の grid になり、画像が中央から外れた)。\n"+
				"用途ごとに違うクラス名を付けること。", rel, conflicts)
	}
}

// cssRuleBodies groups the bodies of top-level `.foo { ... }` rules by class.
//
// 入れ子を含むブロックでも、**最初に現れる閉じ括弧まで**を body とする — 入れ子
// (`&:hover`) の中のプロパティまで数えると、正当な書き方が衝突に見える。
func cssRuleBodies(style string) map[string][]string {
	out := map[string][]string{}
	locs := cssModuleClassRe.FindAllStringSubmatchIndex(style, -1)
	for _, loc := range locs {
		name := style[loc[2]:loc[3]]
		rest := style[loc[1]:]
		end := strings.IndexAny(rest, "}{")
		if end < 0 || rest[end] == '{' {
			// 入れ子が先に来るブロックは、プロパティの数え方が曖昧になるので
			// 対象外にする (先頭に宣言が無い = 衝突しようがない形)。
			end = len(rest)
			if i := strings.Index(rest, "{"); i >= 0 {
				end = i
			}
		}
		out[name] = append(out[name], rest[:end])
	}
	return out
}

// styleModuleBlock returns the contents of the `<style ... module>` block.
func styleModuleBlock(src string) string {
	open := regexp.MustCompile(`<style[^>]*\bmodule\b[^>]*>`).FindStringIndex(src)
	if open == nil {
		return ""
	}
	rest := src[open[1]:]
	end := strings.Index(rest, "</style>")
	if end < 0 {
		return ""
	}
	return rest[:end]
}
