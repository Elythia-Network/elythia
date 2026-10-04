package entitycompat

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **購読キャッシュを捨てる側が、Web Push の配送が読むのと同じキャッシュを
// 受け取っていること (#3207 レビュー)。**
//
// `deleteAccount.pushSubscriptionCache` は criticalWiring で nil を起動時に
// 落とすが、`corewebpush.NewSubscriptionCache(...)` をもう 1 つ作って渡しても
// nil ではないので通る。その場合 Redis の層は共有されて消えるが、配送側の
// プロセス内の層 (3 分) は残り、削除した購読へ push され続ける。
// `internal/server` は router を組み立てるテストが無いので、ここで AST から
// 「キャッシュの生成は 1 回だけで、配送と無効化の両方に同じ変数を渡す」ことを見る。
func TestPushSubscriptionCacheIsShared(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset,
		filepath.Join("..", "server", "router.go"), nil, parser.ParseComments)
	require.NoError(t, err)

	var cacheVars []string
	var deliveryArgs, deleteAccountArgs, swArgs []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			if len(node.Lhs) != 1 || len(node.Rhs) != 1 {
				return true
			}
			if exprString(node.Rhs[0]) == "corewebpush.NewSubscriptionCache" {
				if id, ok := node.Lhs[0].(*ast.Ident); ok {
					cacheVars = append(cacheVars, id.Name)
				}
			}
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok || len(node.Args) == 0 {
				return true
			}
			switch {
			case exprString(sel) == "processors.NewWebPushProcessor":
				deliveryArgs = append(deliveryArgs, exprString(node.Args[0]))
			case sel.Sel.Name == "SetPushSubscriptionCache" && exprString(sel.X) == "deleteAccountProcessor":
				deleteAccountArgs = append(deleteAccountArgs, exprString(node.Args[0]))
			case exprString(sel) == "apisw.NewHandler":
				swArgs = append(swArgs, exprString(node.Args[len(node.Args)-1]))
			}
		}
		return true
	})

	require.Len(t, cacheVars, 1,
		"router.go で corewebpush.NewSubscriptionCache を変数に代入している箇所がちょうど 1 つではない。"+
			"キャッシュを複数作ると、無効化しても配送側のプロセス内の層が残る")
	cache := cacheVars[0]
	require.Len(t, deliveryArgs, 1, "router.go に processors.NewWebPushProcessor の呼び出しがちょうど 1 つではない (rename した?)")
	require.Len(t, deleteAccountArgs, 1, "router.go に deleteAccountProcessor.SetPushSubscriptionCache の呼び出しがちょうど 1 つではない")
	require.Len(t, swArgs, 1, "router.go に apisw.NewHandler の呼び出しがちょうど 1 つではない (rename した?)")

	assert.Equal(t, cache, deliveryArgs[0], "Web Push の配送が %s を読んでいない", cache)
	assert.Equal(t, cache, deleteAccountArgs[0],
		"アカウント削除が %s を無効化していない。削除後の通知が消した購読へ push され続ける", cache)
	assert.Equal(t, cache, swArgs[0],
		"sw/unregister などが %s を無効化していない。解除した購読へ push され続ける", cache)
}
