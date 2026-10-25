package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/misc/id"
)

// **buildHandlers は apiHandlers のすべての欄を埋めること (#3694)。**
//
// 欄を書き忘れると、setupRoutes の別名は nil の handler を受け取る。method value
// (`usersHandler.Show`) は nil のレシーバでも作れるので、ビルドもルートの登録も
// 通り、その route に最初のリクエストが来たときに初めて panic する。どの
// constructor も引数を構造体に入れるだけなので、依存はゼロ値で足りる。
func TestBuildHandlersFillsEveryField(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	s := &Server{config: &config.Config{URL: "https://example.tld", Host: "example.tld"}}

	h := s.buildHandlers(&wiring{}, core{}, fed{}, idGen, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	v := reflect.ValueOf(h)
	// 抽出の下限: 名指しの欄があり、欄が 1 つ以上あること。
	require.Positive(t, v.NumField(), "apiHandlers に欄が無い")
	_, ok := v.Type().FieldByName("users")
	require.True(t, ok, "apiHandlers に users の欄が無い (rename した?)")
	for i := range v.NumField() {
		f := v.Type().Field(i)
		require.Equal(t, reflect.Pointer, f.Type.Kind(), "apiHandlers.%s がポインタでない", f.Name)
		assert.False(t, v.Field(i).IsNil(), "buildHandlers が apiHandlers.%s を埋めていない", f.Name)
	}
}

// **setupRoutes は、buildHandlers で作った handler をすべて別名で受け、その名前を
// 入れ替えないこと (#3694)。**
//
// ルートの登録は setupRoutes の別名に対して行う。別名を別の式 (構築の式や
// w.handlers からの読み出し) にしたり、後で `=` で入れ替えたりすると、ビルドは
// 通るのに、登録される handler が buildHandlers の作ったもの (と、それに setter で
// 入れた依存) から外れる。なので次を見る。
//
//   - 本文の直下に `hSvcs := s.buildHandlers(...)` と `w.handlers = hSvcs` が 1 回ずつ、
//     この順にある
//   - apiHandlers の欄ごとに、本文の直下に `x := hSvcs.<欄>` が 1 回だけ、
//     `w.handlers = hSvcs` より後にある (別名は変数から取る。docs/architecture.md 4.2)
//   - 別名と hSvcs を、setupRoutes のどこでも (関数リテラルの中を含めて) `=` で
//     入れ替えず、`:=` で定義し直さない
//   - hSvcs の欄を書き換えない。`hSvcs.<欄> = …` (`hSvcs.<欄>.<…> = …` や `++` も)
//     と、書き換えの口になる `&hSvcs` (`&hSvcs.<欄>` も) を落とす。欄を書き換えると、
//     別名と `w.handlers` が buildHandlers の作ったものから外れるのに、構築・別名・
//     代入の行の文字列はどれも残るので、文字列で照合するゲートでは捕まらない
//   - パッケージのどこでも `<x>.handlers.<y>` の形でコンテナから読み戻さない
//
// 落とすのは普通に書いたときに踏む形だけで、次のような意図的に避ける形は
// 見ていない (CLAUDE.md Section 4「ゲートを書くとき」)。range の Key / Value
// への代入 (`for _, hSvcs.<欄> = range …`)、別名を通した中身の上書き
// (`*xHandler = …`)、apiHandlers のポインタレシーバのメソッドの呼び出し。
func TestSetupRoutesAliasesEveryBuiltHandler(t *testing.T) {
	_, fd := setupRoutesDecl(t)
	body := fd.Body

	fields := map[string]bool{}
	ht := reflect.TypeOf(apiHandlers{})
	for i := range ht.NumField() {
		fields[ht.Field(i).Name] = true
	}
	require.True(t, fields["users"], "apiHandlers に users の欄が無い (rename した?)")

	buildIdx, assignIdx := -1, -1
	aliasIdx := map[string][]int{} // 欄 -> 別名の文の位置
	aliasName := map[string]string{}
	for i, st := range body.List {
		as, ok := st.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			continue
		}
		switch as.Tok {
		case token.DEFINE:
			lhs, ok := as.Lhs[0].(*ast.Ident)
			if !ok {
				continue
			}
			if call, ok := as.Rhs[0].(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "buildHandlers" {
					require.Equal(t, "hSvcs", lhs.Name, "buildHandlers の戻り値を hSvcs で受けていない")
					require.Equal(t, -1, buildIdx, "setupRoutes が buildHandlers を 2 回呼んでいる")
					buildIdx = i
				}
				continue
			}
			sel, ok := as.Rhs[0].(*ast.SelectorExpr)
			if !ok {
				continue
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "hSvcs" {
				require.True(t, fields[sel.Sel.Name], "hSvcs.%s は apiHandlers の欄でない", sel.Sel.Name)
				aliasIdx[sel.Sel.Name] = append(aliasIdx[sel.Sel.Name], i)
				aliasName[sel.Sel.Name] = lhs.Name
			}
		case token.ASSIGN:
			sel, ok := as.Lhs[0].(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "handlers" {
				continue
			}
			rhs, ok := as.Rhs[0].(*ast.Ident)
			require.True(t, ok && rhs.Name == "hSvcs", "w.handlers に hSvcs 以外を入れている")
			require.Equal(t, -1, assignIdx, "w.handlers に 2 回入れている")
			assignIdx = i
		}
	}
	require.NotEqual(t, -1, buildIdx, "setupRoutes の本文の直下に hSvcs := s.buildHandlers(...) が無い")
	require.NotEqual(t, -1, assignIdx, "setupRoutes の本文の直下に w.handlers = hSvcs が無い")
	require.Less(t, buildIdx, assignIdx, "w.handlers = hSvcs が buildHandlers の呼び出しより前にある")
	for f := range fields {
		idx := aliasIdx[f]
		if !assert.Len(t, idx, 1, "setupRoutes の本文の直下に、hSvcs.%s の別名が 1 回だけあること", f) {
			continue
		}
		assert.Greater(t, idx[0], assignIdx, "hSvcs.%s の別名が w.handlers = hSvcs より前にある", f)
	}

	// 別名と hSvcs を入れ替えていないこと。関数リテラルの中も見る。
	guarded := map[string]bool{"hSvcs": true}
	for _, n := range aliasName {
		guarded[n] = true
	}
	defs := map[string]int{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.UnaryExpr:
			if v.Op == token.AND && isRootedAt(v.X, "hSvcs") {
				t.Errorf("setupRoutes が hSvcs のアドレスを取っている (&%s)。欄を書き換える口になる", exprName(rootIdent(v.X)))
			}
		case *ast.IncDecStmt:
			if isRootedAt(v.X, "hSvcs") {
				t.Errorf("setupRoutes が hSvcs の欄を書き換えている (%s)", v.Tok)
			}
		}
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, l := range as.Lhs {
			if _, isIdent := l.(*ast.Ident); !isIdent && isRootedAt(l, "hSvcs") {
				t.Errorf("setupRoutes が hSvcs の欄を書き換えている (%s)。別名と w.handlers が buildHandlers で作った handler から外れる", as.Tok)
				continue
			}
			id, ok := l.(*ast.Ident)
			if !ok || !guarded[id.Name] {
				continue
			}
			switch as.Tok {
			case token.DEFINE:
				defs[id.Name]++
			default:
				t.Errorf("setupRoutes が %s を入れ替えている (%s)。buildHandlers で作った handler と登録する handler が分かれうる", id.Name, as.Tok)
			}
		}
		return true
	})
	for n := range guarded {
		assert.Equal(t, 1, defs[n], "setupRoutes で %s を := で定義しているのは 1 回だけであること", n)
	}

	// コンテナから読み戻していないこと (w.handlers.x)。
	fset := token.NewFileSet()
	for _, src := range packageSources(t) {
		f, err := parser.ParseFile(fset, src.name, src.src, 0)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "handlers" {
				t.Errorf("%s が %s.handlers.%s を読んでいる。後の領域は hSvcs を引数で受けること (docs/architecture.md 4.2)", src.name, exprName(inner.X), sel.Sel.Name)
			}
			return true
		})
	}
}

// rootIdent returns the identifier at the root of a selector, index, star or
// parenthesized expression (hSvcs for hSvcs.users.x), or nil.
func rootIdent(e ast.Expr) *ast.Ident {
	for {
		switch v := e.(type) {
		case *ast.Ident:
			return v
		case *ast.SelectorExpr:
			e = v.X
		case *ast.IndexExpr:
			e = v.X
		case *ast.StarExpr:
			e = v.X
		case *ast.ParenExpr:
			e = v.X
		default:
			return nil
		}
	}
}

// isRootedAt reports whether e is name itself or an expression rooted at it.
func isRootedAt(e ast.Expr, name string) bool {
	id := rootIdent(e)
	return id != nil && id.Name == name
}

// exprName renders a short name for an identifier expression in a message.
func exprName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "<expr>"
}
