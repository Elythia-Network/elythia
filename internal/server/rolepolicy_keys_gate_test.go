package server

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/effectivepolicy"
)

// mk-go 固有の role policy キーは fork frontend の 2 箇所で列挙されている。
// どちらかが落ちると、その policy は管理画面から設定できない (#2898)。
//
// **型検査では捕まらない。** どちらも `Misskey.rolePolicies` に戻しても
// vue-tsc は通り、症状は「編集しても保存されない」「開き直すと無効表示」で、
// エラーもログも出ない。#2898 で実際に 2 段階で踏んだ (roles.editor を直した
// あと roles.policy-editor に同じループが残っていた)。
//
// **突き合わせ先は effectivepolicy の defaults。** upstream 由来のキーは
// misskey-js の rolePolicies が持つので、こちらが列挙するのは「defaults に
// あって upstream に無いもの」= mk-go 固有キーだけになる。
// notInFrontendUI は「管理画面の policy 編集 UI に出していない mk-go 固有キー」を
// 理由付きで持つ。**新規流入を止めるための allowlist** で、ここに足すのは
// 「UI に出さないと決めた」ときだけ。
//
// key ごとに理由を持つ (件数だけだと、1 つ足して 1 つ消す形で素通りする)。
//
// **現在は空。** 分割アップロードの 3 キー (#2313) が導入時から UI に無く、
// #2898 の gate が検出して #2900 で解消した。空のまま保つのが正しい状態で、
// 足すときは「UI に出さない」と決めた理由を書くこと。
var notInFrontendUI = map[string]string{}

func TestMkGoRolePolicyKeysAreListedInFrontend(t *testing.T) {
	root := filepath.Join(repoRootDir(t), "third_party", "misskey")
	consts := filepath.Join(root, "packages", "misskey-js", "src", "consts.ts")
	if _, err := os.Stat(consts); err != nil {
		if os.Getenv("MK_FRONTEND_GATES_REQUIRE_SUBMODULE") != "" {
			require.NoError(t, err, "submodule を要求する job なのに %s を読めない", consts)
		}
		t.Skipf("submodule が無い (checkout する job でのみ検査する)")
	}
	upstream := parseUpstreamRolePolicies(t, consts)
	require.NotEmpty(t, upstream, "misskey-js の rolePolicies を読めなかった")

	var mkGoKeys []string
	for key := range effectivepolicy.Defaults() {
		if !upstream[key] || notInFrontendUI[key] != "" {
			if notInFrontendUI[key] != "" {
				continue
			}
			mkGoKeys = append(mkGoKeys, key)
		}
	}
	sort.Strings(mkGoKeys)
	require.NotEmpty(t, mkGoKeys, "mk-go 固有の policy キーが 1 つも無い; この gate は何も検査していない")

	// **allowlist の陳腐化を検出する。** 消えた policy を許可し続けると、
	// 一覧に「もう存在しないキー」が残ったまま気付けない。
	defaults := effectivepolicy.Defaults()
	for key := range notInFrontendUI {
		_, ok := defaults[key]
		require.True(t, ok, "allowlist の %q は policy から消えている。allowlist から外すこと", key)
		require.False(t, upstream[key], "allowlist の %q は upstream の rolePolicies にある。allowlist から外すこと", key)
	}

	for _, file := range []string{
		filepath.Join("packages", "frontend", "src", "pages", "admin", "roles.editor.vue"),
		filepath.Join("packages", "frontend", "src", "pages", "admin", "roles.policy-editor.vue"),
	} {
		listed := parseMkGoPolicyKeyList(t, filepath.Join(root, file))
		require.NotEmpty(t, listed, "%s から mk-go 固有キーの一覧を読めなかった", file)
		for _, key := range mkGoKeys {
			require.Contains(t, listed, key,
				"%s の一覧に %q が無い。管理画面からこの policy を設定できなくなる", file, key)
		}
	}

	// **一覧に載っているだけでは編集できない (#2900)。** 枠 (role.policies の
	// キー) が作られるようになっても、編集フォーム (XFolder) が無ければ画面に
	// 出ない。#2900 が直したのはまさにその状態なので、フォームの実在も見る。
	editor := filepath.Join(root, "packages", "frontend", "src", "pages", "admin", "roles.policy-editor.vue")
	folders := parsePolicyEditorFolderKeys(t, editor)
	require.NotEmpty(t, folders, "roles.policy-editor.vue から XFolder の対象キーを読めなかった")
	for _, key := range mkGoKeys {
		require.Contains(t, folders, key,
			"roles.policy-editor.vue に %q の編集フォーム (XFolder) が無い。"+
				"キー一覧に載っていても画面に出ないので設定できない", key)
	}
}

// htmlComment matches a `<!-- ... -->` block in a Vue template.
var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// matchQueryKey pulls the search key out of `matchQuery([..., 'key'])`.
//
// **拾うのは `])` の直前の文字列リテラル。** upstream は第 2 引数を policy キー
// そのものではなく検索キーワードとして使うことがある (`canImportUserLists` の
// XFolder は `'canImportUserList'` と単数で書いている)。mk-go 固有キーは
// 検索語 = キー名で書く前提で、ずれたら「フォームが無い」と落ちる (fail-loud)。
var matchQueryKey = regexp.MustCompile(`matchQuery\(\[[^\]]*?'([A-Za-z0-9_]+)'\s*\]\)`)

// parsePolicyEditorFolderKeys collects the policy keys each XFolder targets.
//
// **コメントアウトした XFolder は数えない。** 生テキストを走査すると
// `<!-- 一時的に無効化 -->` で囲んだフォームを実在扱いしてしまい、管理画面から
// 消えているのにゲートが緑になる。docs/gates.md の 2026-08-31 の経緯 (wiring-check) が
// `/* */` で同じ穴を踏んでおり (#2856)、#2701 が記録した事故は
// 「検証のために一時的に外して戻し忘れる」= まさにこの形。
func parsePolicyEditorFolderKeys(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	src := htmlComment.ReplaceAll(raw, nil)

	// コメント除去で全部消えたら落とす (書式が変わって空振りしたのを緑にしない)。
	require.NotEmpty(t, matchQueryKey.FindAll(src, -1),
		"%s から XFolder を 1 つも読めなかった (書式が変わった?)", path)

	var out []string
	for _, m := range matchQueryKey.FindAllSubmatch(src, -1) {
		out = append(out, string(m[1]))
	}
	return out
}

// parseUpstreamRolePolicies reads misskey-js の `export const rolePolicies`.
func parseUpstreamRolePolicies(t *testing.T, path string) map[string]bool {
	t.Helper()
	src, err := os.ReadFile(path)
	require.NoError(t, err)
	block := regexp.MustCompile(`(?s)export const rolePolicies = \[(.*?)\]`).FindSubmatch(src)
	require.NotNil(t, block, "rolePolicies の宣言が見つからない (書式が変わった?)")
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`'([A-Za-z0-9_]+)'`).FindAllSubmatch(block[1], -1) {
		out[string(m[1])] = true
	}
	return out
}

// parseMkGoPolicyKeyList reads a `const NAME = [...Misskey.rolePolicies, 'a']`
// declaration and verifies the declared name is what the file actually loops over.
//
// **リテラルの存在だけでは足りない。** 宣言を残したままループを
// `Misskey.rolePolicies` に戻す形が、まさに #2898 で 2 段階で踏んだ症状
// (固有キーの meta が落ちて「ベース値を使用」表示、再編集で priority が 0 に
// 戻る) を再現する。宣言だけ見る gate はそれを緑で通す (実測)。未使用 const は
// vue-tsc も拾わない (`noUnusedLocals: false`)。
func parseMkGoPolicyKeyList(t *testing.T, path string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	require.NoError(t, err)
	decl := regexp.MustCompile(`const\s+([A-Za-z0-9_]+)\s*:[^=]*=\s*\[\s*\.\.\.Misskey\.rolePolicies\s*,([^\]]*)\]`).FindSubmatch(src)
	require.NotNil(t, decl, "`const NAME = [...Misskey.rolePolicies, ...]` の宣言が見つからない (書式が変わった?)")

	name := string(decl[1])
	loop := regexp.MustCompile(`for\s*\(\s*const\s+[A-Za-z0-9_]+\s+of\s+` + regexp.QuoteMeta(name) + `\b`)
	require.True(t, loop.Match(src),
		"%s は宣言されているだけで、`for (const … of %s)` が無い。"+
			"宣言を残したままループを Misskey.rolePolicies に戻すと、"+
			"mk-go 固有 policy が管理画面から設定できなくなる", name, name)

	var out []string
	for _, m := range regexp.MustCompile(`'([A-Za-z0-9_]+)'`).FindAllSubmatch(decl[2], -1) {
		out = append(out, string(m[1]))
	}
	return out
}
