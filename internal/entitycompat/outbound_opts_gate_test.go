package entitycompat

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **outbound の共通設定を渡し忘れない (#3037、不変条件は #638)。**
//
// `config.proxy` / `outgoingAddress` / `outgoingAddressFamily` は
// `safehttp.Option` として transport に載る。`...safehttp.Option` を受け取る
// constructor を router から呼ぶときに渡し忘れると、**その経路だけがサーバーの
// 素の IP で外へ出る**。運営者は proxy を設定したつもりのままなので、抜けて
// いることに気付けない。
//
// 実際に `emojimeta.NewFetcher` と `oauthDiscoveryTransport` の 2 つが
// 抜けていた。どちらも build もテストも緑のまま。
//
// **この gate が見るのは `internal/server` から呼ぶ側だけ。** 受け取る側
// (constructor) は variadic をそのまま transport へ流すだけなので、渡って
// いれば効く。
func TestOutboundConstructorsReceiveSharedOptions(t *testing.T) {
	qualified := variadicSafehttpOptionFuncs(t)
	require.NotEmpty(t, qualified, "`...safehttp.Option` を受ける関数を 1 つも拾えていない")
	// 代表例が入っていること (抽出が壊れたら空振りする)。
	assert.True(t, qualified["github.com/elythia-network/elythia/internal/core/emojimeta.NewFetcher"],
		"emojimeta.NewFetcher を拾えていない")
	assert.True(t, qualified["github.com/elythia-network/elythia/internal/server.oauthDiscoveryTransport"],
		"oauthDiscoveryTransport を拾えていない")

	const serverPkg = "github.com/elythia-network/elythia/internal/server"
	var missing []string
	for _, file := range serverSourceFiles(t) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		require.NoError(t, err)
		imports := importIdents(f)

		// 自分自身が `...safehttp.Option` を受ける関数の中では、その引数を
		// そのまま流すのが正しい (`oauthDiscoveryTransport`)。
		forwardable := forwardableOptionParams(f)

		// **走査は宣言ごとに分ける (#3037 レビュー 2 周目)。**
		// 1 周目は `ast.Inspect` を 1 本で回して `currentFunc` を持ち回って
		// いたが、FuncDecl を抜けても戻さないので**package レベルの `var`
		// 初期化子が直前の関数の名前を引きずる**。実測で、variadic を持つ
		// 関数の後ろに `var x = func() { ... }` を書くと、その中の呼び出しが
		// 「自分の opts を流している」と誤判定された。
		for _, decl := range f.Decls {
			scope := ""
			if fn, ok := decl.(*ast.FuncDecl); ok {
				scope = fn.Name.Name
			}
			// この宣言の中で `outboundOpts()` から受けた局所変数。
			derived := outboundOptsDerivedVars(decl)

			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				key := ""
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					key = serverPkg + "." + fn.Name
				case *ast.SelectorExpr:
					pkgIdent, ok := fn.X.(*ast.Ident)
					if !ok {
						return true
					}
					path, ok := imports[pkgIdent.Name]
					if !ok {
						// レシーバ越しの呼び出し (`s.foo()`) は同 package。
						key = serverPkg + "." + fn.Sel.Name
						break
					}
					key = path + "." + fn.Sel.Name
				default:
					return true
				}
				if !qualified[key] || callPassesOutboundOpts(call, forwardable[scope], derived) {
					return true
				}
				missing = append(missing, filepath.Base(file)+":"+key)
				return true
			})
		}
	}
	sort.Strings(missing)
	assert.Empty(t, missing,
		"`...safehttp.Option` を受ける constructor に `s.outboundOpts()...` を渡していない。"+
			"渡さないとその経路だけ config.proxy / outgoingAddress を通らない (#638)")
}

// variadicSafehttpOptionFuncs collects functions declared with a trailing
// `...safehttp.Option` parameter anywhere under internal/.
func variadicSafehttpOptionFuncs(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	const modulePrefix = "github.com/elythia-network/elythia/"
	err := filepath.Walk(filepath.Join(".."), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		// `internal/...` からの相対パスを import path へ直す。
		rel, rerr := filepath.Rel("..", filepath.Dir(path))
		if rerr != nil {
			return nil
		}
		pkgPath := modulePrefix + "internal/" + filepath.ToSlash(rel)
		imports := importIdents(f)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Type.Params == nil {
				continue
			}
			for _, p := range fn.Type.Params.List {
				ell, ok := p.Type.(*ast.Ellipsis)
				if !ok {
					continue
				}
				if isSafehttpOption(ell.Elt, imports) {
					out[pkgPath+"."+fn.Name.Name] = true
				}
			}
		}
		return nil
	})
	require.NoError(t, err)
	// **transport を直に組む関数も対象にする (#3037 レビュー)。**
	//
	// `safehttp.NewSSRFSafeTransport` は同 package なので `opts ...Option` と
	// 宣言されており、`safehttp.Option` という selector にならないので上の
	// 走査に一度も当たらない。ところが**これこそが本命** — 修正前の
	// `oauthDiscoveryTransport` が書いていたのはまさに
	// `safehttp.NewSSRFSafeTransport(allowedPrivateNetworks)` (opts 無し) で、
	// この形が素通りすると gate が守りたいものを守れない。
	//
	// `mediaproxy.NewSSRFSafeTransport` も同じ (受け取った opts を流す
	// wrapper だが、**呼ぶ側の渡し忘れ**はここでしか見られない)。
	out[modulePrefix+"internal/safehttp.NewSSRFSafeTransport"] = true
	out[modulePrefix+"internal/core/mediaproxy.NewSSRFSafeTransport"] = true
	return out
}

// serverSourceFiles lists the non-test Go sources of internal/server.
func serverSourceFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "server"))
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		out = append(out, filepath.Join("..", "server", e.Name()))
	}
	require.NotEmpty(t, out)
	return out
}

// callPassesOutboundOpts reports whether the call spreads outboundOpts().
//
// **展開 (`...`) まで見る。** `s.outboundOpts()` を引数に置いただけでは
// 型が合わずコンパイルも通らないが、判定を名前だけにすると将来
// `[]safehttp.Option` を受ける形に変えたときに素通りする。
func callPassesOutboundOpts(call *ast.CallExpr, forwardable string, derived map[string]bool) bool {
	if !call.Ellipsis.IsValid() || len(call.Args) == 0 {
		return false
	}
	// 展開されるのは最後の引数だけ。
	spread := call.Args[len(call.Args)-1]

	// **式の中に `outboundOpts()` があれば通す (#3037 レビュー 2 周目)。**
	// 1 周目は「引数そのものが `outboundOpts()` の呼び出しか」しか見て
	// いなかったので、共通 opts に 1 つ足す唯一の書き方である
	// `append(s.outboundOpts(), extra)...` を**偽陽性で落としていた**
	// (しかも診断は「渡していない」と事実と逆を指す)。
	if containsOutboundOptsCall(spread) {
		return true
	}
	if id, ok := spread.(*ast.Ident); ok {
		// **囲む関数自身の variadic (#3037 レビュー)。** 名前で判定すると、
		// `opts` という名前の空 slice を宣言して渡すだけで抜けられるので、
		// その宣言が実際にこの関数の引数であることまで見る。
		if forwardable != "" && id.Name == forwardable {
			return true
		}
		// **局所変数に受けてから展開する形も通す (レビュー 2 周目)。**
		// `emojiOpts := s.outboundOpts()` はごく普通の書き方で、1 周目は
		// これも偽陽性で落としていた。`outboundOpts()` を含む式から
		// 受けたものだけを認めるので、空 slice を渡す抜け道にはならない。
		if derived[id.Name] {
			return true
		}
	}
	return false
}

// containsOutboundOptsCall reports whether expr calls outboundOpts anywhere.
func containsOutboundOptsCall(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if ok && calleeName(call.Fun) == "outboundOpts" {
			found = true
			return false
		}
		return true
	})
	return found
}

// outboundOptsDerivedVars collects the local variables assigned from an
// expression that calls outboundOpts.
func outboundOptsDerivedVars(decl ast.Decl) map[string]bool {
	derived := map[string]bool{}
	// **`outboundOpts()` を含まない代入があった名前は認めない
	// (#3037 レビュー 3 周目)。** 位置もスコープも見ていないので、
	//
	//	emojiOpts := s.outboundOpts()
	//	emojiOpts = nil
	//
	// や、同じ関数の別ブロックで同名に空 slice を入れる形が素通りしていた
	// (どちらも実測)。`router.go` の `setupRoutes` は数千行あるので同名の
	// 再利用は起こりうる。**1 度でも別物を入れたら落とす**ほうへ倒す
	// (偽陽性側は「名前を分ける」で直せる)。
	tainted := map[string]bool{}
	ast.Inspect(decl, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || i >= len(assign.Rhs) {
				continue
			}
			if containsOutboundOptsCall(assign.Rhs[i]) {
				derived[id.Name] = true
			} else {
				tainted[id.Name] = true
			}
		}
		return true
	})
	out := map[string]bool{}
	for name := range derived {
		if !tainted[name] {
			out[name] = true
		}
	}
	return out
}

// forwardableOptionParams maps each function to the name of its own
// `...safehttp.Option` parameter (空文字なら持たない)。
//
// **その関数の引数だけを「流してよい」と認める。** 名前で判定すると、同じ
// 名前の空 slice を作って渡すだけで gate を抜けられる。
func forwardableOptionParams(f *ast.File) map[string]string {
	out := map[string]string{}
	imports := importIdents(f)
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Type.Params == nil {
			continue
		}
		for _, p := range fn.Type.Params.List {
			ell, ok := p.Type.(*ast.Ellipsis)
			if !ok {
				continue
			}
			if isSafehttpOption(ell.Elt, imports) && len(p.Names) > 0 {
				out[fn.Name.Name] = p.Names[0].Name
			}
		}
	}
	return out
}

// safehttpPkgPath is the package that declares Option.
const safehttpPkgPath = "github.com/elythia-network/elythia/internal/safehttp"

// isSafehttpOption reports whether expr names safehttp.Option, resolving the
// import alias.
//
// **alias を解決するのが要点 (#3037 レビュー 2 周目)。** `safehttp` という
// 識別子だけを見ていたので、`import sh "…/internal/safehttp"` と書いた
// package の constructor は**集合から黙って消えていた** (実測: urlpreview を
// alias にすると、その呼び出しから opts を落としても gate が緑のまま通る)。
// 「1 つも拾えなかったら落とす」の保護は、ハードコードした 2 つにしか効かない。
func isSafehttpOption(expr ast.Expr, imports map[string]string) bool {
	if id, ok := expr.(*ast.Ident); ok {
		// **dot import と同 package (#3037 レビュー 3 周目)。**
		// `import . "…/internal/safehttp"` した package の `opts ...Option` は
		// selector にならないので、selector だけを見ていると**集合から黙って
		// 消える**。実測で urlpreview を dot import して opts を落とすと
		// gate が緑のまま通った。
		return id.Name == "Option" && imports["."] == safehttpPkgPath
	}
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Option" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return imports[pkg.Name] == safehttpPkgPath
}

// importIdents maps the local identifier of each import to its path.
func importIdents(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = path
	}
	return out
}

// calleeName returns the identifier a call targets (`f` / `x.f`).
func calleeName(fun ast.Expr) string {
	switch t := fun.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	}
	return ""
}
