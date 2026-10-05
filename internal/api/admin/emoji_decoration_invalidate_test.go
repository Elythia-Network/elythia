package admin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

type countingInvalidator struct{ calls int }

func (c *countingInvalidator) Invalidate() { c.calls++ }

// 絵文字を変えたら、絵文字由来のアバターデコレーションのキャッシュを捨てる
// (#2975)。捨てないと、作った直後に装着した絵文字が cacheTTL のあいだ
// `avatarDecorations: []` として返る (#2258 が catalog 側で踏んだのと同じ形)。
func TestEmojiPublishHelpersInvalidateDecorationCache(t *testing.T) {
	e := &model.Emoji{ID: "e1", Name: "party"}

	for _, tc := range []struct {
		name string
		call func(h *Handler)
	}{
		{"added", func(h *Handler) { h.publishEmojiAdded(e) }},
		{"updated", func(h *Handler) { h.publishEmojiUpdated(e) }},
		{"deleted", func(h *Handler) { h.publishEmojiDeleted(e) }},
		{"updatedByIDs", func(h *Handler) { h.publishEmojiUpdatedByIDs([]string{"e1"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := &countingInvalidator{}
			h := &Handler{}
			h.SetEmojiDecorationInvalidator(inv)
			tc.call(h)
			assert.Equal(t, 1, inv.calls)
		})
	}

	// 上の表は `&Handler{}` = **broadcastPub が nil** の状態で回している。
	// publishEmoji* はどれもそこで早期 return するので、invalidate を後ろに
	// 置くと 4 件すべてが落ちる (= stream を使わない構成で効かなくなる形を
	// 表そのものが押さえている)。

	// 未配線なら no-op (unit test で毎回 stub を刺さずに済むため)。
	t.Run("invalidator 未配線でも落ちない", func(t *testing.T) {
		h := &Handler{}
		assert.NotPanics(t, func() { h.publishEmojiAdded(e) })
	})
}

// **publishEmoji* が増えたときに落ちる。** 上の表は手で並べたものなので、
// 新しい helper を足して表に書き忘れると検査から外れる。AST で「その接頭辞を
// 持つメソッドは全て、body の先頭で invalidateEmojiDecorationCache を呼ぶ」を
// 固定する。
//
// **「どこかで呼んでいる」では足りない。** publishEmoji* はどれも
// `broadcastPub == nil` で早期 return するので、後ろに置いたり `if` の中に
// 入れたりすると、stream を使わない構成や条件から外れた経路で効かなくなる。
// 実際その 2 形は「どこかで呼んでいる」判定を素通りすることが敵対的レビューで
// 実測された。先頭に固定すると両方落ちる。
func TestEveryEmojiPublishHelperCallsInvalidate(t *testing.T) {
	fset := token.NewFileSet()
	// **`parser.ParseDir` は使わない** (Go 1.25 で非推奨)。非推奨の理由は
	// 「build tag を見ないので package とファイルの対応が不正確」だが、ここは
	// ディレクトリ内の .go を全部見たいので、その不正確さがむしろ要件に合う。
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	found := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoErrorf(t, err, "%s を parse できない", name)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !strings.HasPrefix(fn.Name.Name, "publishEmoji") {
				continue
			}
			found[fn.Name.Name] = firstStmtInvalidatesDecorationCache(fn)
		}
	}

	require.NotEmpty(t, found, "publishEmoji* が 1 つも見つからない (書式が変わった?)")
	for name, ok := range found {
		assert.Truef(t, ok,
			"%s の **body の先頭** が h.invalidateEmojiDecorationCache() ではない。\n"+
				"後ろに置くと broadcastPub 未配線の構成で効かず、絵文字由来の\n"+
				"アバターデコレーション (#2975) が cacheTTL のあいだ消える。", name)
	}
}

// firstStmtInvalidatesDecorationCache reports whether fn's first statement is
// exactly `<receiver>.invalidateEmojiDecorationCache()`.
//
// **レシーバも見る。** 名前だけを比べると、たまたま同名のメソッドを持つ別の
// 値に対する呼び出しでも通ってしまう。引数個数まで見ている厳密さと揃える。
func firstStmtInvalidatesDecorationCache(fn *ast.FuncDecl) bool {
	if fn.Body == nil || len(fn.Body.List) == 0 || fn.Recv == nil || len(fn.Recv.List) == 0 {
		return false
	}
	recvNames := fn.Recv.List[0].Names
	if len(recvNames) == 0 {
		return false
	}
	recv := recvNames[0].Name
	expr, ok := fn.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := expr.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok || x.Name != recv {
		return false
	}
	return sel.Sel.Name == "invalidateEmojiDecorationCache"
}
