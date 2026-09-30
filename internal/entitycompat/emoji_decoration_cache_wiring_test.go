package entitycompat

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 絵文字由来のアバターデコレーション (#2975) のキャッシュを router が配線しないと、
// 絵文字を作った直後にそれを装着しても **保存されているのに API が
// `avatarDecorations: []` を返す** — #2258 が catalog 側で踏んだ形そのもの。
//
// **`entity.SetEmojiDecorationLookup` だけは `TestProcessGlobalsAreRestored` が
// 見ているが、invalidator 側は誰も見ていなかった。** どちらも消してもビルドも
// テストも通り、症状は「30 秒だけ消える」なのでログにも出ない。
//
// **criticalWiring の表には載せない。** あちらは GoDoc が「認証や一回性の保証」
// と tier を定義しており、未配線で起きるのが「空配列 / 0 / 使えない」のものは
// 意図的に載せていない (兄弟の `SetAvatarDecorationInvalidator` も同様)。
func TestEmojiDecorationCacheIsWired(t *testing.T) {
	assertWired(t, routerGo, "adminHandler.SetEmojiDecorationInvalidator(emojiDecorationResolver)",
		"admin の絵文字変更 (追加 / 更新 / 削除 / bulk / 申請の承認) が\n"+
			"絵文字デコレーションのキャッシュに伝わらず、作った直後の絵文字を\n"+
			"装着しても最大 30 秒 avatarDecorations が空で返る。")
	assertWired(t, routerGo, "DecorationCache: emojiDecorationResolver",
		"絵文字 zip インポートがキャッシュに伝わらない。インポート直後の\n"+
			"絵文字は装着しても出ず、置き換えで消えた絵文字は残り続ける。")
}

// emojiMutatingMethods are the EmojiRepository methods that change rows.
//
// 読み取り系は含めない。`repository.EmojiRepository` の宣言から機械的に取れる
// 形にはしていない — interface には read と write が混在しており、どちらかを
// 機械判定する基準が無いため。**増やすときはここに足す。**
var emojiMutatingMethods = map[string]bool{
	"Create":           true,
	"UpdateFields":     true,
	"UpdateFieldsMany": true,
	"Delete":           true,
	"DeleteMany":       true,
}

// emojiCacheNotifiers are the calls that (directly or through a helper) drop the
// emoji decoration cache.
//
// **既知の非検出形**: ここに無関係な名前 (`fmt` など) を足すと、その名前を呼ぶ
// だけで「捨てている」と判定されるようになる。これはゲート自身の受け入れ条件を
// 広げる変異なので、このテストからは原理的に捕まえられない (捕まえるには一覧を
// 別の一覧で固定することになり循環する)。`publishEmoji*` の側は
// `TestEveryEmojiPublishHelperCallsInvalidate` が body の先頭を AST で固定して
// いるので、そちらは名前だけでは通らない。
var emojiCacheNotifiers = map[string]bool{
	"publishEmojiAdded":              true,
	"publishEmojiUpdated":            true,
	"publishEmojiDeleted":            true,
	"publishEmojiUpdatedByIDs":       true,
	"invalidateEmojiDecorationCache": true,
	"invalidateDecorationCache":      true,
}

// emojiMutationDirs are the packages that write emoji rows.
//
// **`internal/api/admin` だけでは足りない。** zip インポートは
// `internal/core/emojiimport` にあり、`publishEmoji*` を通らないので
// admin だけ見る gate では素通りする (敵対的レビューで実測された)。
//
// **`internal/core/federation` は対象外。** あそこも `emoji` を書くが
// (`resolver.go` の `Create` / `UpdateFields`)、作るのは必ず `Host: &host` の
// リモート行で、resolver の map は `ListLocal` (= `host IS NULL`) しか載せない。
// **ローカル行に触るようになったらここへ足すこと** — そうなるまでこの gate は
// あちらについて何も言わない。
var emojiMutationDirs = []string{
	"internal/api/admin",
	"internal/core/emojiimport",
}

// mustDetectEmojiMutators pins functions that **must** be seen as emoji
// mutators, so that narrowing the detection itself is caught.
//
// **`seen > 0` では足りない。** `emojiMutatingMethods` から 1 つ抜いても他の
// メソッドで件数は残るので、検査が静かに狭まったまま緑になる (実測)。
// docs/gates.md の secretfield-check が `mustDetectSecretFields` で塞いだのと同じ形。
//
// **選び方**: 変更メソッド 5 つと対象ディレクトリ 2 つを、それぞれ「抜けたら
// どれかが検出されなくなる」ように 1 つずつ覆う。全件を並べると正当な endpoint
// 追加のたびに落ちるので、代表だけを固定する。
var mustDetectEmojiMutators = map[string]string{
	"internal/api/admin.EmojiAdd":             "Create",
	"internal/api/admin.EmojiUpdate":          "UpdateFields",
	"internal/api/admin.EmojiSetCategoryBulk": "UpdateFieldsMany",
	"internal/api/admin.EmojiDelete":          "Delete",
	"internal/api/admin.EmojiDeleteBulk":      "DeleteMany",
	"internal/core/emojiimport.replaceEmoji":  "zip インポート (admin 以外のディレクトリ)",
}

// emojiMutationExempt lists functions that change emoji rows without touching
// the decoration cache, with the reason. **現在は空。**
//
// 件数ではなく理由を持つ (件数だけだと 1 つ足して 1 つ消す形で素通りする)。
var emojiMutationExempt = map[string]string{}

// **「絵文字を変える経路は必ずキャッシュを捨てる」を関数単位で固定する。**
//
// `TestEveryEmojiPublishHelperCallsInvalidate` が押さえるのは helper の中だけで、
// **その手前 (endpoint → helper) は空いていた**。新しい絵文字 endpoint が
// `publishEmoji*` を通さずに書くと、helper 側の gate は何も言わない。
func TestEmojiMutationsDropDecorationCache(t *testing.T) {
	root := repoRoot(t)

	type site struct{ pkg, fn string }
	var offenders []site
	detected := map[string]bool{}
	seen := 0

	for _, dir := range emojiMutationDirs {
		fset := token.NewFileSet()
		files, err := parseNonTestGoFiles(fset, filepath.Join(root, dir))
		require.NoErrorf(t, err, "%s を読めない", dir)

		for _, file := range files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				if !callsAny(fn, emojiMutatingMethods, isEmojiRepoReceiver) {
					continue
				}
				seen++
				key := dir + "." + fn.Name.Name
				detected[key] = true
				if emojiMutationExempt[key] != "" {
					continue
				}
				if !callsAny(fn, emojiCacheNotifiers, nil) {
					offenders = append(offenders, site{dir, fn.Name.Name})
				}
			}
		}
	}

	// 1 つも拾えなかったら落とす。書式や変数名が変わって空振りすると、
	// 検査していないのに緑になる。
	require.NotZero(t, seen, "絵文字 row を変える関数が 1 つも見つからない (判定が空振りしている?)")

	// **検出集合そのものを固定する。** 件数だけだと一覧を狭めても素通りする。
	for key, why := range mustDetectEmojiMutators {
		require.Truef(t, detected[key],
			"%s が絵文字 row を変える関数として検出されていない (%s を覆うために固定してある)。\n"+
				"emojiMutatingMethods / emojiMutationDirs / isEmojiRepoReceiver を"+
				"狭めていないか確認すること。", key, why)
	}

	for _, o := range offenders {
		t.Errorf("%s の %s が絵文字 row を変えているのに、絵文字デコレーションの"+
			"キャッシュを捨てていない。\n"+
			"`publishEmoji*` を通すか、invalidator を直接呼ぶこと。"+
			"意図して捨てないなら emojiMutationExempt に理由付きで足す。", o.pkg, o.fn)
	}

	// allowlist の陳腐化も見る。
	for key, reason := range emojiMutationExempt {
		require.NotEmptyf(t, reason, "emojiMutationExempt の %q に理由が無い", key)
	}
}

// isEmojiRepoReceiver reports whether x looks like the emoji repository.
func isEmojiRepoReceiver(x ast.Expr) bool {
	var sb strings.Builder
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		switch v := e.(type) {
		case *ast.Ident:
			sb.WriteString(v.Name)
		case *ast.SelectorExpr:
			walk(v.X)
			sb.WriteString(".")
			sb.WriteString(v.Sel.Name)
		}
	}
	walk(x)
	return strings.Contains(strings.ToLower(sb.String()), "emojirepo")
}

// callsAny reports whether fn calls any method in names. When recv is non-nil
// the call's receiver expression must satisfy it.
func callsAny(fn *ast.FuncDecl, names map[string]bool, recv func(ast.Expr) bool) bool {
	hit := false
	ast.Inspect(fn, func(n ast.Node) bool {
		if hit {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.SelectorExpr:
			if !names[f.Sel.Name] {
				return true
			}
			if recv != nil && !recv(f.X) {
				return true
			}
			hit = true
			return false
		case *ast.Ident:
			if recv == nil && names[f.Name] {
				hit = true
				return false
			}
		}
		return true
	})
	return hit
}
