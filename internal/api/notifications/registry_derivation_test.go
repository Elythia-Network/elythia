package notifications

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/notification"
)

// TestTypeListsAreDerivedFromRegistry asserts the API-level type lists are
// derived from the core registry instead of being re-declared as literals.
//
// **値の一致だけでは足りない (#2898)。** リテラルに書き戻しても、書いた時点では
// 中身が同じなので値比較は通る。落ちるのは core 側に型を足した後 = 一番検出
// したい瞬間に検出できない。導出している「形」そのものを固定する。
func TestTypeListsAreDerivedFromRegistry(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "handler.go", nil, 0)
	require.NoError(t, err)

	// var 名 -> 呼び出している関数名 (`notification.Xxx()` の Xxx)。
	derivedFrom := map[string]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				call, ok := vs.Values[i].(*ast.CallExpr)
				if !ok {
					continue
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "notification" {
					continue
				}
				derivedFrom[name.Name] = sel.Sel.Name
			}
		}
	}

	require.Equal(t, "UpstreamTypeNames", derivedFrom["notificationTypeList"],
		"notificationTypeList must be derived from notification.UpstreamTypeNames(), not declared as a literal")
	require.Equal(t, "ObsoleteTypeNames", derivedFrom["obsoleteNotificationTypeList"],
		"obsoleteNotificationTypeList must be derived from notification.ObsoleteTypeNames(), not declared as a literal")

	// 導出元と実際の値も突き合わせる (形だけ合っていて中身が空でないこと)。
	require.Equal(t, notification.UpstreamTypeNames(), notificationTypeList)
	require.Equal(t, notification.ObsoleteTypeNames(), obsoleteNotificationTypeList)
	require.NotEmpty(t, notificationTypeList)
	require.NotEmpty(t, obsoleteNotificationTypeList)
}

// TestExcludeAllUpstreamTypesCoversEverything pins that a client sending only
// the upstream types still hits the early return (#2898).
//
// **ここを外すと既読位置が飛ぶ。** 早期 return を抜けると maybeMarkAsRead まで
// 進み、1 件も返していないのにユーザーが受け取っていない通知まで既読になる。
// misskey-js の notificationTypes を送るクライアント (fork frontend の
// 「すべて無効」を含む) が実際にこの入力を作る。
func TestExcludeAllUpstreamTypesCoversEverything(t *testing.T) {
	var upstream []string
	var mkgo []string
	for _, d := range notification.Descriptors() {
		switch d.Kind {
		case notification.KindUpstream:
			upstream = append(upstream, string(d.Type))
		case notification.KindMkGo:
			mkgo = append(mkgo, string(d.Type))
		}
	}
	require.NotEmpty(t, upstream)
	require.NotEmpty(t, mkgo, "no mk-go specific types; this gate is inspecting nothing")

	require.True(t, emptyByTypeFilter(ListRequest{ExcludeTypes: upstream}),
		"excluding every upstream type must still be treated as excluding everything")

	// 固有型は enum に入るので filter 値としては指定できる。
	require.True(t, validNotificationTypes(mkgo),
		"mk-go specific types must be accepted as filter values")

	// 1 つでも欠ければ被覆にならない (判定が件数ではなく集合であることの確認)。
	require.False(t, emptyByTypeFilter(ListRequest{ExcludeTypes: upstream[:len(upstream)-1]}))
}
