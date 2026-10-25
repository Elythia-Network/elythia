package entitycompat

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **購読キャッシュを捨てる側が、Web Push の配送が読むのと同じキャッシュを
// 受け取っていること (#3207 レビュー)。**
//
// アカウント削除の購読キャッシュは `processors.NewDeleteAccountProcessor` の必須の
// 引数で (#3829。以前は criticalWiring の `deleteAccount.pushSubscriptionCache`)、
// nil は `TestBuiltHandlersHaveNoNilDependencies` が落とすが、
// `corewebpush.NewSubscriptionCache(...)` をもう 1 つ作って渡しても nil ではないので通る。その場合 Redis の層は共有されて消えるが、配送側の
// プロセス内の層 (3 分) は残り、削除した購読へ push され続ける。
// `internal/server` は router を組み立てるテストが無いので、ここで AST から
// 「キャッシュの生成は 1 回だけで、配送と無効化の両方に同じ変数を渡す」ことを見る。
func TestPushSubscriptionCacheIsShared(t *testing.T) {
	// internal/server の全ファイルを読む (#3672)。
	fset, files := parseServerFiles(t, parser.ParseComments)
	for _, p := range pushSubscriptionCacheProblems(fset, files) {
		t.Error(p)
	}
}

// pushSubscriptionCacheProblems returns why the files do not share one push
// subscription cache between delivery and invalidation (nil when they do).
//
// **変数の名前で突き合わせるので、4 つが同じファイルにあることまで見る。**
// ローカル変数は別のファイルの同名の変数と別物で、ファイルをまたいで名前が
// 一致しても同じ instance とは言えない。
func pushSubscriptionCacheProblems(fset *token.FileSet, files []*ast.File) []string {
	var cacheVars []string
	var deliveryArgs, deleteAccountArgs, swArgs []string
	sites := map[string]bool{}
	fileOf := func(n ast.Node) string { return fset.Position(n.Pos()).Filename }
	inspectServerFiles(files, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			if len(node.Lhs) != 1 || len(node.Rhs) != 1 {
				return true
			}
			if exprString(node.Rhs[0]) == "corewebpush.NewSubscriptionCache" {
				if id, ok := node.Lhs[0].(*ast.Ident); ok {
					cacheVars = append(cacheVars, id.Name)
					sites[fileOf(node)] = true
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
				sites[fileOf(node)] = true
			case exprString(sel) == "processors.NewDeleteAccountProcessor":
				// 購読キャッシュは最後の引数 (#3829)。
				deleteAccountArgs = append(deleteAccountArgs, exprString(node.Args[len(node.Args)-1]))
				sites[fileOf(node)] = true
			case exprString(sel) == "apisw.NewHandler":
				swArgs = append(swArgs, exprString(node.Args[len(node.Args)-1]))
				sites[fileOf(node)] = true
			}
		}
		return true
	})

	var problems []string
	for _, c := range []struct {
		got  []string
		what string
	}{
		{cacheVars, "corewebpush.NewSubscriptionCache を変数に代入している箇所 (キャッシュを複数作ると、無効化しても配送側のプロセス内の層が残る)"},
		{deliveryArgs, "processors.NewWebPushProcessor の呼び出し"},
		{deleteAccountArgs, "processors.NewDeleteAccountProcessor の呼び出し"},
		{swArgs, "apisw.NewHandler の呼び出し"},
	} {
		if len(c.got) != 1 {
			problems = append(problems, fmt.Sprintf("internal/server の %s が %d 個 (ちょうど 1 つのはず。rename した?)", c.what, len(c.got)))
		}
	}
	if len(problems) > 0 {
		return problems
	}
	if len(sites) != 1 {
		names := make([]string, 0, len(sites))
		for n := range sites {
			names = append(names, n)
		}
		sort.Strings(names)
		return []string{fmt.Sprintf("キャッシュの生成と、配送・無効化への受け渡しが別々のファイルにある (%v)。"+
			"名前が同じでも同じ変数とは言えないので、この gate では判定できない。同じファイルに置くこと", names)}
	}
	cache := cacheVars[0]
	if deliveryArgs[0] != cache {
		problems = append(problems, fmt.Sprintf("Web Push の配送が %s を読んでいない (%s)", cache, deliveryArgs[0]))
	}
	if deleteAccountArgs[0] != cache {
		problems = append(problems, fmt.Sprintf("アカウント削除が %s を無効化していない (%s)。削除後の通知が消した購読へ push され続ける", cache, deleteAccountArgs[0]))
	}
	if swArgs[0] != cache {
		problems = append(problems, fmt.Sprintf("sw/unregister などが %s を無効化していない (%s)。解除した購読へ push され続ける", cache, swArgs[0]))
	}
	return problems
}

// 生成と受け渡しが別々のファイルにあるときに落とすこと (#3672 レビュー M-1)。
func TestPushSubscriptionCacheProblemsRequireOneFile(t *testing.T) {
	const create = "func a() {\n\tc := corewebpush.NewSubscriptionCache(r, d)\n\t_ = c\n}\n"
	const use = "func b() {\n\tprocessors.NewWebPushProcessor(c)\n\tprocessors.NewDeleteAccountProcessor(n, d, f, p, c)\n\tapisw.NewHandler(x, c)\n}\n"
	parse := func(srcs map[string]string) (*token.FileSet, []*ast.File) {
		fset := token.NewFileSet()
		var names []string
		for n := range srcs {
			names = append(names, n)
		}
		sort.Strings(names)
		var files []*ast.File
		for _, n := range names {
			f, err := parser.ParseFile(fset, n, "package server\n\n"+srcs[n], parser.ParseComments)
			require.NoError(t, err)
			files = append(files, f)
		}
		return fset, files
	}

	fset, files := parse(map[string]string{"a.go": create + use})
	assert.Empty(t, pushSubscriptionCacheProblems(fset, files), "同じファイルなら問題は無い")

	fset, files = parse(map[string]string{"a.go": create, "b.go": use})
	got := pushSubscriptionCacheProblems(fset, files)
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "別々のファイル")

	// 同じファイルでも、別の変数を渡したら落とす。
	fset, files = parse(map[string]string{"a.go": create + strings.Replace(use, "NewDeleteAccountProcessor(n, d, f, p, c)", "NewDeleteAccountProcessor(n, d, f, p, other)", 1)})
	assert.Len(t, pushSubscriptionCacheProblems(fset, files), 1)
}
