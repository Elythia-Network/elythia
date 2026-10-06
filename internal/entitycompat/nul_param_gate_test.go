package entitycompat

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repoLookupMinMethods is the number of single-row lookups the gate expects to
// find in internal/repository. **0 件で PASS させないための下限** — 抽出が壊れても
// 「違反 0 件」と区別が付かず、検査していないのに緑になる。実測に合わせてあるので、
// lookup を消したときは一緒に下げること。
const repoLookupMinMethods = 118

// likePatternMinSites is the number of functions that build a SQL LIKE pattern
// from caller input. 同じく実測に合わせた下限。
const likePatternMinSites = 16

// nulGuardFuncs are the repository predicates that reject a value which cannot
// match stored data.
var nulGuardFuncs = map[string]bool{
	"storable": true, "allStorable": true, "storableIDs": true, "Storable": true,
}

// likeEscapeFuncs are the helpers that escape a value into a LIKE pattern.
//
// **`multipleWordsToQuery` は含めない。** あれは自分の中で語ごとに落としている
// ので、呼び出し側に判定を要求すると偽陽性になる。あの関数自身は `escapeLike` を
// 呼ぶので、中の判定を外せばここで捕まる。
var likeEscapeFuncs = map[string]bool{
	"escapeLike":           true,
	"escapeSQLLikePattern": true,
}

// nulGuardViolation is one lookup that can put an unstorable value on the wire.
type nulGuardViolation struct {
	file   string
	line   int
	method string
	reason string
}

func (v nulGuardViolation) pos() string { return fmt.Sprintf("%s:%d", v.file, v.line) }

// paramKind describes how a parameter has to be guarded.
type paramKind int

const (
	paramString paramKind = iota // string / *string -> storable
	paramSlice                   // []string        -> allStorable / storableIDs
)

type guardedParam struct {
	name string
	kind paramKind
}

// parseGoDir parses every non-test Go file under root.
func parseGoDir(t *testing.T, root string, fset *token.FileSet) []*ast.File {
	t.Helper()
	var files []*ast.File
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		files = append(files, f)
		return nil
	})
	require.NoError(t, err, "walk %s", root)
	return files
}

// scanRepoLookupGuards reports single-row lookups (and the `*ByID*` family) that
// do not reject unstorable inputs before querying, plus the number inspected.
//
// 見るのは 3 つ:
//
//   - `storable` / `allStorable` / `storableIDs` を**呼んでいる**こと
//   - **どの値を見ているか** — string / []string / *string のパラメータが
//     **1 つ残らず** guard の引数に現れること。「関数のどこかに guard がある」だけ
//     だと、複数の値を取る lookup で片方が無防備なまま緑になる (#3025 のレビュー
//     H2。実際に `ListUsers` の `Hostname` がその形で漏れていた)
//   - **引く前に**呼んでいること。後ろに置くと SELECT が先に走って落ちるので、
//     guard があっても意味が無い
//
// `storableIDs` は**代入し直していること**まで見る。`storableIDs(ids)` と書くと
// 式文として合法で `go vet` も黙るが、フィルタは一切効かない。
func scanRepoLookupGuards(t *testing.T, root string) ([]nulGuardViolation, int) {
	t.Helper()

	fset := token.NewFileSet()
	var violations []nulGuardViolation
	methods := 0

	for _, file := range parseGoDir(t, root, fset) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv == nil || !isGuardedLookup(fn) {
				continue
			}
			methods++
			at := fset.Position(fn.Pos())
			reported := 0
			add := func(reason string) {
				reported++
				violations = append(violations, nulGuardViolation{at.Filename, at.Line, fn.Name.Name, reason})
			}

			guardArgs := guardArgumentPos(fn.Body, false)
			exiting := guardArgumentPos(fn.Body, true)
			reassigned := reassignedBy(fn.Body, "storableIDs")
			dbPos := firstDBCallPos(fn, receiverName(fn))
			params := guardedParams(fn.Type)
			for _, p := range params {
				pos, ok := reassigned[p.name]
				if !ok {
					pos, ok = exiting[p.name]
				}
				if !ok {
					switch {
					case p.kind == paramSlice && filteredBy(fn.Body, p.name, "storableIDs"):
						// `storableIDs(ids)` は式文として合法で `go vet` も黙るが、
						// 代入し直さないとフィルタは一切効かない。
						add(fmt.Sprintf("storableIDs(%s) の戻り値を捨てている (フィルタが効かない)", p.name))
					case len(guardArgs) > 0 && guardArgs[p.name] != token.NoPos && hasGuardArg(guardArgs, p.name):
						// **guard の結果を使っていない。** `if !storable(x) { ... }` の
						// 分岐が抜けない / guard を条件で囲んで通らない経路がある形。
						add(fmt.Sprintf("%s の guard が抜けない (結果を使っていない)", p.name))
					default:
						add(fmt.Sprintf("%s を guard に渡していない", p.name))
					}
					continue
				}
				// **順序はパラメータごとに見る (#3025 のレビュー H1)。** 関数単位で
				// 「最初の guard」と比べると、2 つ目以降の guard が SELECT の後ろでも通る。
				if dbPos != token.NoPos && dbPos < pos {
					add(fmt.Sprintf("%s の guard が最初の DB 呼び出しより後ろにある (引く前に弾いていない)", p.name))
				}
			}
			// **string 系のパラメータが 1 つも無い lookup には guard を要求しない。**
			// 要求すると、id を独自型で受ける lookup が「呼んでいない」で落ちる。
			if len(params) > 0 && reported == 0 && firstCallPos(fn.Body, nulGuardFuncs) == token.NoPos {
				add("storable / storableIDs を呼んでいない")
			}
		}
	}

	sortNulGuardViolations(violations)
	return violations, methods
}

// isGuardedLookup reports whether fn is a lookup the gate requires a guard on:
// a single-row `Find*` / `Get*`, or anything named `*ByID*`.
func isGuardedLookup(fn *ast.FuncDecl) bool {
	if strings.Contains(fn.Name.Name, "ByID") {
		return true
	}
	if !strings.HasPrefix(fn.Name.Name, "Find") && !strings.HasPrefix(fn.Name.Name, "Get") {
		return false
	}
	// 単一行の lookup = `(*model.X, error)` を返すもの。
	res := fn.Type.Results
	if res == nil || len(res.List) != 2 {
		return false
	}
	star, ok := res.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "model"
}

// guardedParams lists the parameters that have to be rejected when unstorable.
func guardedParams(sig *ast.FuncType) []guardedParam {
	var out []guardedParam
	if sig.Params == nil {
		return out
	}
	for _, field := range sig.Params.List {
		kind, ok := paramKindOf(field.Type)
		if !ok {
			continue
		}
		for _, name := range field.Names {
			if name.Name == "_" {
				continue
			}
			out = append(out, guardedParam{name.Name, kind})
		}
	}
	return out
}

// paramKindOf classifies string / *string / []string parameters.
func paramKindOf(expr ast.Expr) (paramKind, bool) {
	switch t := expr.(type) {
	case *ast.Ident:
		return paramString, t.Name == "string"
	case *ast.StarExpr:
		ident, ok := t.X.(*ast.Ident)
		return paramString, ok && ident.Name == "string"
	case *ast.ArrayType:
		ident, ok := t.Elt.(*ast.Ident)
		return paramSlice, ok && t.Len == nil && ident.Name == "string"
	}
	return paramString, false
}

// guardArgumentPos maps the names handed to a guard predicate to the position of
// that call. exitingOnly restricts it to guards whose `if` actually leaves the
// function (`if !storable(x) { return ... }`).
//
// `*host` は `host`、`filter.Query` は `filter.Query` に畳む。**名前まで見るのが
// 要点** — 呼んでいるかだけ見ると、同じ関数の別の値を guard しただけで通る。
//
// **結果を使っているかまで見る (#3025 のレビュー M3)。** `if !storable(x) { x = "" }`
// や、guard を別の条件で囲んで通らない経路を作る形は、呼んではいるが効かない。
func guardArgumentPos(body *ast.BlockStmt, exitingOnly bool) map[string]token.Pos {
	out := map[string]token.Pos{}
	record := func(call *ast.CallExpr) {
		for _, arg := range call.Args {
			name := exprName(arg)
			if name == "" {
				continue
			}
			if prev, ok := out[name]; !ok || call.Pos() < prev {
				out[name] = call.Pos()
			}
		}
	}
	if !exitingOnly {
		ast.Inspect(body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && isNamedCall(call, nulGuardFuncs) {
				record(call)
			}
			return true
		})
		return out
	}
	// **入れ子の `if` は数えない。** 外側の条件が付くと通らない経路ができるので、
	// guard が常に効くとは言えなくなる。
	for _, stmt := range body.List {
		ifStmt, ok := stmt.(*ast.IfStmt)
		if !ok || !blockLeaves(ifStmt.Body) {
			continue
		}
		ast.Inspect(ifStmt.Cond, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && isNamedCall(call, nulGuardFuncs) {
				record(call)
			}
			return true
		})
	}
	return out
}

// hasGuardArg reports whether name appears in guardArgs.
func hasGuardArg(guardArgs map[string]token.Pos, name string) bool {
	_, ok := guardArgs[name]
	return ok
}

// blockLeaves reports whether body returns (or panics).
func blockLeaves(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.ReturnStmt:
			found = true
		case *ast.CallExpr:
			if ident, ok := t.Fun.(*ast.Ident); ok && ident.Name == "panic" {
				found = true
			}
		}
		return true
	})
	return found
}

// filteredBy reports whether name is handed to the given helper.
func filteredBy(body *ast.BlockStmt, name, helper string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isNamedCall(call, map[string]bool{helper: true}) {
			return true
		}
		for _, arg := range call.Args {
			if exprName(arg) == name {
				found = true
			}
		}
		return true
	})
	return found
}

// reassignedBy maps the names assigned from a call to helper (`x = f(x)`) to the
// position of that call.
func reassignedBy(body *ast.BlockStmt, helper string) map[string]token.Pos {
	out := map[string]token.Pos{}
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok || !isNamedCall(call, map[string]bool{helper: true}) {
			return true
		}
		if name := exprName(assign.Lhs[0]); name != "" {
			out[name] = call.Pos()
		}
		return true
	})
	return out
}

// exprName renders an identifier, a `*x` deref or an `x.Field` selector.
func exprName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return exprName(t.X)
	case *ast.SelectorExpr:
		if base := exprName(t.X); base != "" {
			return base + "." + t.Sel.Name
		}
	}
	return ""
}

// isNamedCall reports whether call invokes one of names (package qualifier ignored).
func isNamedCall(call *ast.CallExpr, names map[string]bool) bool {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return names[fun.Name]
	case *ast.SelectorExpr:
		return names[fun.Sel.Name]
	}
	return false
}

// firstCallPos returns the position of the first call to one of names.
func firstCallPos(body *ast.BlockStmt, names map[string]bool) token.Pos {
	pos := token.NoPos
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isNamedCall(call, names) {
			return true
		}
		if pos == token.NoPos || call.Pos() < pos {
			pos = call.Pos()
		}
		return true
	})
	return pos
}

// firstDBCallPos returns the position of the first call that can reach the DB.
//
// **レシーバのメソッド呼び出しは DB ではない (#3025 のレビュー M4)。**
// `r.normalizeHost(host)` のような純粋な helper を guard の前に 1 行置いただけで
// 落ちるのでは、述語と意図が食い違う。DB とみなすのは:
//
//   - `tx.` / `db.` / `q.` — 直接 `*gorm.DB` を指す名前
//   - `r.db.` / `c.inner.` / `c.UserRepository.` — レシーバの**フィールド**越し
//   - `preloadNoteRelations(r.db).First(...)` — 部分木にそれらが現れる形
//
// 逆に `r.foo(...)` (レシーバ直下のメソッド) は helper として扱う。
func firstDBCallPos(fn *ast.FuncDecl, recv string) token.Pos {
	roots := map[string]bool{"tx": true, "db": true, "q": true}
	if recv != "" {
		roots[recv] = true
	}
	pos := token.NoPos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if ident, isIdent := sel.X.(*ast.Ident); isIdent {
			// `r.foo(...)` は helper、`tx.First(...)` は DB。
			if !roots[ident.Name] || ident.Name == recv {
				return true
			}
		} else if !containsRootIdent(sel.X, roots) {
			return true
		}
		if pos == token.NoPos || call.Pos() < pos {
			pos = call.Pos()
		}
		return true
	})
	return pos
}

// containsRootIdent reports whether e mentions one of roots.
func containsRootIdent(e ast.Expr, roots map[string]bool) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok && roots[ident.Name] {
			found = true
		}
		return true
	})
	return found
}

// receiverName returns the receiver variable name of fn.
func receiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 || len(fn.Recv.List[0].Names) == 0 {
		return ""
	}
	return fn.Recv.List[0].Names[0].Name
}

func sortNulGuardViolations(v []nulGuardViolation) {
	sort.Slice(v, func(i, j int) bool {
		if v[i].file != v[j].file {
			return v[i].file < v[j].file
		}
		if v[i].line != v[j].line {
			return v[i].line < v[j].line
		}
		return v[i].reason < v[j].reason
	})
}

// TestRepositoryLookupsRejectUnstorableValues is the repository lookup gate (#3025).
//
// **列に入らない値をそのまま SELECT に載せると 500 になる。** NUL を含む値は
// どの行とも一致しえないうえ、比較の右辺に置くだけで PostgreSQL が落とす (手元の
// simple protocol で SQLSTATE 08P01、本番の pgx extended protocol で 22021)。
// `IsNotFound` でもないので handler は `JSONInternalError` へ倒し、**認証済みの
// 一般利用者がパラメータ 1 文字で 5xx を立てられる**。
//
// **mock では検出できない。** `internal/testutil` の mock repository は NUL を
// 渡しても普通に「見つからない」を返すので、guard を外しても mock を使う
// handler テストは緑のまま通る。だから静的に見る。
//
// **既知の範囲**: 単一行の lookup (`Find*` / `Get*` が `(*model.X, error)` を
// 返すもの) と `*ByID*` だけ。`ListByUser(userID, ...)` のように値を受ける一覧系は
// 見ていない (docs/divergence/security.md の「射程外」)。
func TestRepositoryLookupsRejectUnstorableValues(t *testing.T) {
	root := filepath.Join(repoRoot(t), "internal", "repository")
	violations, methods := scanRepoLookupGuards(t, root)

	require.GreaterOrEqual(t, methods, repoLookupMinMethods,
		"lookup を %d 件しか拾えていない (抽出が壊れると検査していないのに緑になる)", methods)

	if len(violations) > 0 {
		var b strings.Builder
		b.WriteString("列に入らない値が SELECT に載る経路がある (#3025):\n")
		for _, v := range violations {
			fmt.Fprintf(&b, "  %s (%s): %s\n", v.pos(), v.method, v.reason)
		}
		b.WriteString("\n単体なら `if !storable(x) { return nil, ErrNotFound }`、\n")
		b.WriteString("複数なら `ids = storableIDs(ids)` を**引く前に**置くこと。\n")
		t.Fatal(b.String())
	}
}

// scanLikePatternGuards reports functions that build a LIKE pattern without
// first rejecting values that can never match, plus the number inspected.
//
// **どの値を見ているかまで照合する。** escape helper に渡している式から元の名前を
// 辿り (`for _, w := range strings.Fields(query)` の `w` なら `query` まで)、その
// どれかが guard の引数に現れることを求める。呼び出しの有無だけだと、同じ関数の
// 別の値を guard しただけで通る。
func scanLikePatternGuards(t *testing.T, roots ...string) ([]nulGuardViolation, int) {
	t.Helper()

	fset := token.NewFileSet()
	var violations []nulGuardViolation
	sites := 0

	for _, root := range roots {
		for _, file := range parseGoDir(t, root, fset) {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				escapePos := firstCallPos(fn.Body, likeEscapeFuncs)
				if escapePos == token.NoPos {
					continue
				}
				sites++
				at := fset.Position(fn.Pos())
				add := func(reason string) {
					violations = append(violations, nulGuardViolation{at.Filename, at.Line, fn.Name.Name, reason})
				}
				guardArgs := guardArgumentPos(fn.Body, false)
				if len(guardArgs) == 0 {
					add("LIKE パターンを作る前に一致しえない値を弾いていない")
					continue
				}
				// **escape ごとに、その値の guard が**手前にあるか**を見る
				// (#3025 のレビュー H1)。関数単位で「最初の guard」と比べると、
				// 別の値の guard が手前にあるだけで通ってしまう。
				for _, src := range likeSourceNames(fn.Body) {
					pos, ok := earliestGuardPos(src, guardArgs)
					switch {
					case !ok:
						add(fmt.Sprintf("LIKE に載る値 (%s) を guard に渡していない", strings.Join(src.names, " / ")))
					case pos > src.pos:
						add(fmt.Sprintf("LIKE に載る値 (%s) の判定が組み立てより後ろにある", strings.Join(src.names, " / ")))
					}
				}
			}
		}
	}

	sortNulGuardViolations(violations)
	return violations, sites
}

// likeSource is one escape call: where it is, and the names its argument can
// come from.
type likeSource struct {
	pos   token.Pos
	names []string
}

// likeSourceNames returns, for each escape call, the names its argument can come
// from (the argument itself plus the range / assignment ancestry).
func likeSourceNames(body *ast.BlockStmt) []likeSource {
	origins := valueOrigins(body)
	var out []likeSource
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isNamedCall(call, likeEscapeFuncs) {
			return true
		}
		names := map[string]bool{}
		for _, arg := range call.Args {
			for _, name := range exprNames(arg) {
				names[name] = true
				for _, anc := range ancestry(name, origins, 0) {
					names[anc] = true
				}
			}
		}
		if len(names) > 0 {
			out = append(out, likeSource{call.Pos(), sortedNameKeys(names)})
		}
		return true
	})
	return out
}

// valueOrigins maps a local name to the names its value is derived from.
func valueOrigins(body *ast.BlockStmt) map[string][]string {
	out := map[string][]string{}
	record := func(lhs ast.Expr, rhs ast.Expr) {
		name := exprName(lhs)
		if name == "" {
			return
		}
		out[name] = append(out[name], exprNames(rhs)...)
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch st := n.(type) {
		case *ast.AssignStmt:
			if len(st.Rhs) == 1 {
				for _, lhs := range st.Lhs {
					record(lhs, st.Rhs[0])
				}
			}
		case *ast.RangeStmt:
			if st.Value != nil {
				record(st.Value, st.X)
			}
		}
		return true
	})
	return out
}

// ancestry walks up to 3 levels of origins for name.
func ancestry(name string, origins map[string][]string, depth int) []string {
	if depth >= 3 {
		return nil
	}
	var out []string
	for _, src := range origins[name] {
		if src == name {
			continue
		}
		out = append(out, src)
		out = append(out, ancestry(src, origins, depth+1)...)
	}
	return out
}

// earliestGuardPos returns the earliest guard position among src's names.
func earliestGuardPos(src likeSource, guardArgs map[string]token.Pos) (token.Pos, bool) {
	best, found := token.NoPos, false
	for _, n := range src.names {
		pos, ok := guardArgs[n]
		if !ok {
			continue
		}
		if !found || pos < best {
			best, found = pos, true
		}
	}
	return best, found
}

// exprNames collects the identifiers / selectors inside an expression.
func exprNames(e ast.Expr) []string {
	names := map[string]bool{}
	ast.Inspect(e, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.SelectorExpr:
			if name := exprName(t); name != "" {
				names[name] = true
			}
			return false
		case *ast.Ident:
			names[t.Name] = true
		}
		return true
	})
	return sortedNameKeys(names)
}

func sortedNameKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestLikePatternsRejectUnmatchableInput is the search-string gate (#3025).
//
// **検索語に NUL を 1 文字入れるだけで 500 になっていた。** 保存された text に
// NUL は現れないので「一致しえない」が正しい答えだが、そのまま bind parameter に
// 乗せると PostgreSQL がクエリごと落とす。escape helper は `%` / `_` / `\` しか
// 見ないので、ここを静的に見ないと新しい検索を足すたびに同じ穴が開く。
func TestLikePatternsRejectUnmatchableInput(t *testing.T) {
	root := repoRoot(t)
	violations, sites := scanLikePatternGuards(t,
		filepath.Join(root, "internal", "repository"),
		filepath.Join(root, "internal", "api"))

	require.GreaterOrEqual(t, sites, likePatternMinSites,
		"LIKE パターンを作る関数を %d 件しか拾えていない (抽出が壊れると検査していないのに緑になる)", sites)

	if len(violations) > 0 {
		var b strings.Builder
		b.WriteString("一致しえない検索語がそのまま SQL に載る経路がある (#3025):\n")
		for _, v := range violations {
			fmt.Fprintf(&b, "  %s (%s): %s\n", v.pos(), v.method, v.reason)
		}
		b.WriteString("\nAND で畳む語は `if !storable(q) { return nil, nil }`、\n")
		b.WriteString("OR で畳む語は要素ごとに落とすこと。\n")
		t.Fatal(b.String())
	}
}

// 抽出器自身のテスト。**これが無いと gate が空虚になる** — 違反 0 件が正常な
// 状態なので、述語を壊しても本物のソースからは何も落ちない。
func TestScanRepoLookupGuards_DetectsMissingLateAndPartialGuards(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "repository")
	writeGoFile(t, dir, "r.go", `
func (r *repo) FindByID(id string) (*model.Thing, error) {
	var v model.Thing
	return &v, r.db.First(&v, "id = ?", id).Error
}

func (r *repo) FindLateByID(id string) (*model.Thing, error) {
	var v model.Thing
	err := r.db.First(&v, "id = ?", id).Error
	if !storable(id) {
		return nil, ErrNotFound
	}
	return &v, err
}

func (r *repo) FindByIDAndUserID(id, userID string) (*model.Thing, error) {
	if !storable(userID) {
		return nil, ErrNotFound
	}
	var v model.Thing
	return &v, r.db.First(&v, "id = ? AND userId = ?", id, userID).Error
}

func (r *repo) FindManyByIDs(ids []string) ([]*model.Thing, error) {
	storableIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	var rows []*model.Thing
	return rows, r.db.Where("id IN ?", ids).Find(&rows).Error
}

func (r *repo) FindByPair(userID, noteID string) (*model.Thing, error) {
	if !storable(userID) || !storable(noteID) {
		return nil, ErrNotFound
	}
	var v model.Thing
	return &v, r.db.First(&v, "a = ? AND b = ?", userID, noteID).Error
}

func (r *repo) ListByUser(userID string) ([]*model.Thing, error) {
	var rows []*model.Thing
	return rows, r.db.Where("userId = ?", userID).Find(&rows).Error
}
`)
	violations, methods := scanRepoLookupGuards(t, root)

	assert.Equal(t, 5, methods, "単一行 lookup と *ByID* だけを数える (ListByUser は対象外)")
	require.Len(t, violations, 4, "違反 4 件だけを拾う: %v", violations)
	assert.Equal(t, "FindByID", violations[0].method)
	assert.Contains(t, violations[0].reason, "渡していない")
	assert.Equal(t, "FindLateByID", violations[1].method)
	assert.Contains(t, violations[1].reason, "後ろにある")
	assert.Equal(t, "FindByIDAndUserID", violations[2].method)
	assert.Contains(t, violations[2].reason, "id を guard に渡していない")
	assert.Equal(t, "FindManyByIDs", violations[3].method)
	assert.Contains(t, violations[3].reason, "戻り値を捨てている")
}

// **`time.Now()` を「DB 呼び出し」と数えない。** 数えると、guard の前に計測用の
// 1 行を置いただけで落ちる (`user_cached.go` が実際にその形)。
func TestScanRepoLookupGuards_IgnoresNonDBCallsWhenOrdering(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "repository")
	writeGoFile(t, dir, "r.go", `
func (c *cached) FindManyByIDs(ids []string) ([]*model.Thing, error) {
	readStart := time.Now()
	ids = storableIDs(ids)
	out := make([]*model.Thing, 0, len(ids))
	_ = readStart
	return out, c.inner.Find(&out).Error
}
`)
	violations, methods := scanRepoLookupGuards(t, root)
	assert.Equal(t, 1, methods)
	assert.Empty(t, violations, "DB を触らない呼び出しを数えている: %v", violations)
}

// LIKE 側の抽出器テスト。**「どの値を見ているか」まで見る** — 呼び出しの有無だけ
// だと、同じ関数の別の値を guard しただけで通る。
func TestScanLikePatternGuards_DetectsMissingLateAndWrongValue(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "repository")
	writeGoFile(t, dir, "r.go", `
func (r *repo) SearchUnguarded(query string) ([]*model.Thing, error) {
	like := "%" + escapeLike(query) + "%"
	return r.find(like)
}

func (r *repo) SearchLate(query string) ([]*model.Thing, error) {
	like := "%" + escapeLike(query) + "%"
	if !storable(query) {
		return nil, nil
	}
	return r.find(like)
}

func (r *repo) SearchWrongValue(query, userID string) ([]*model.Thing, error) {
	if !storable(userID) {
		return nil, nil
	}
	like := "%" + escapeLike(query) + "%"
	return r.find(like, userID)
}

func (r *repo) SearchWords(query string) ([]*model.Thing, error) {
	if !storable(query) {
		return nil, nil
	}
	for _, word := range strings.Fields(query) {
		_ = "%" + escapeLike(word) + "%"
	}
	return nil, nil
}

func (r *repo) SearchGuarded(filter Filter) ([]*model.Thing, error) {
	if !storable(filter.Query) {
		return nil, nil
	}
	like := "%" + escapeSQLLikePattern(strings.ToLower(filter.Query)) + "%"
	return r.find(like)
}

func (r *repo) NoLikeAtAll(id string) error {
	return r.db.Where("id = ?", id).Error
}
`)
	violations, sites := scanLikePatternGuards(t, root)

	assert.Equal(t, 5, sites, "LIKE を組み立てる関数だけを数える")
	require.Len(t, violations, 3, "違反 3 件だけを拾う: %v", violations)
	assert.Equal(t, "SearchUnguarded", violations[0].method)
	assert.Contains(t, violations[0].reason, "弾いていない")
	assert.Equal(t, "SearchLate", violations[1].method)
	assert.Contains(t, violations[1].reason, "後ろにある")
	assert.Equal(t, "SearchWrongValue", violations[2].method)
	assert.Contains(t, violations[2].reason, "query")
}

// `for _, w := range strings.Fields(q)` のように**語へ分解してから** escape する
// 形で、`q` の guard を根拠として認めること (認めないと正当な実装が落ちる)。
func TestScanLikePatternGuards_TracesRangeAndAssignment(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "repository")
	writeGoFile(t, dir, "r.go", `
func (r *repo) SearchViaLocal(search string) ([]*model.Thing, error) {
	if !storable(search) {
		return nil, nil
	}
	words := strings.Fields(search)
	for _, word := range words {
		_ = "%" + escapeLike(word) + "%"
	}
	return nil, nil
}
`)
	violations, sites := scanLikePatternGuards(t, root)
	assert.Equal(t, 1, sites)
	assert.Empty(t, violations, "語へ分解する正当な形を落としている: %v", violations)
}

// **`*string` のパラメータも見る。** self-test に `*string` が 1 つも無いと、
// `paramKindOf` の該当枝を落としても実モデル側から違反が出ず気付けない
// (#3025 のレビュー H2。in-scope の lookup で `*string` は実測 4 件しかない)。
func TestScanRepoLookupGuards_ChecksPointerAndSliceParams(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "repository")
	writeGoFile(t, dir, "r.go", `
func (r *repo) FindByNameAndHost(name string, host *string) (*model.Thing, error) {
	if !storable(name) {
		return nil, ErrNotFound
	}
	var v model.Thing
	return &v, r.db.First(&v, "name = ?", name).Error
}

func (r *repo) GetScoped(userID string, scope []string) (*model.Thing, error) {
	if !storable(userID) {
		return nil, ErrNotFound
	}
	var v model.Thing
	return &v, r.db.First(&v, "u = ?", userID).Error
}

func (r *repo) FindGuardedByNameAndHost(name string, host *string) (*model.Thing, error) {
	if !storable(name) || (host != nil && !storable(*host)) {
		return nil, ErrNotFound
	}
	var v model.Thing
	return &v, r.db.First(&v, "name = ?", name).Error
}
`)
	violations, methods := scanRepoLookupGuards(t, root)

	assert.Equal(t, 3, methods)
	require.Len(t, violations, 2, "*string / []string の取りこぼし: %v", violations)
	assert.Contains(t, violations[0].reason, "host を guard に渡していない")
	assert.Contains(t, violations[1].reason, "scope を guard に渡していない")
}

// **順序はパラメータごとに見る (#3025 のレビュー H1)。** 関数単位で「最初の
// guard」と比べると、2 つ目以降の guard が SELECT の後ろでも通る。
func TestScanRepoLookupGuards_DetectsPerParameterLateGuard(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "repository")
	writeGoFile(t, dir, "r.go", `
func (r *repo) FindByPair(userID, key string) (*model.Thing, error) {
	if !storable(userID) {
		return nil, ErrNotFound
	}
	var v model.Thing
	err := r.db.First(&v, "u = ? AND k = ?", userID, key).Error
	if !storable(key) {
		return nil, ErrNotFound
	}
	return &v, err
}
`)
	violations, _ := scanRepoLookupGuards(t, root)
	require.Len(t, violations, 1, "片方の guard だけ後ろでも通している: %v", violations)
	assert.Contains(t, violations[0].reason, "key の guard が最初の DB 呼び出しより後ろ")
}

// **guard の結果を使っていない形も落とす (#3025 のレビュー M3)。** 呼んではいるが
// 効かない — 分岐が抜けない / 別の条件で囲んで通らない経路がある。
func TestScanRepoLookupGuards_DetectsGuardWithoutEffect(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "repository")
	writeGoFile(t, dir, "r.go", `
func (r *repo) FindNoReturnByID(id string) (*model.Thing, error) {
	if !storable(id) {
		id = ""
	}
	var v model.Thing
	return &v, r.db.First(&v, "id = ?", id).Error
}

func (r *repo) FindConditionalByID(id string) (*model.Thing, error) {
	if len(id) > 0 {
		if !storable(id) {
			return nil, ErrNotFound
		}
	}
	var v model.Thing
	return &v, r.db.First(&v, "id = ?", id).Error
}
`)
	violations, _ := scanRepoLookupGuards(t, root)
	require.Len(t, violations, 2, "効かない guard を通している: %v", violations)
	for _, v := range violations {
		assert.Contains(t, v.reason, "抜けない")
	}
}

// **レシーバのメソッドは DB 呼び出しではない (#3025 のレビュー M4)。** 純粋な
// helper を guard の前に 1 行置いただけで落ちるのでは、述語と意図が食い違う。
func TestScanRepoLookupGuards_ReceiverHelperIsNotADBCall(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "repository")
	writeGoFile(t, dir, "r.go", `
func (r *repo) FindByHost(host string) (*model.Thing, error) {
	host = r.normalizeHost(host)
	if !storable(host) {
		return nil, ErrNotFound
	}
	var v model.Thing
	return &v, r.db.First(&v, "host = ?", host).Error
}
`)
	violations, _ := scanRepoLookupGuards(t, root)
	assert.Empty(t, violations, "レシーバの helper を DB 呼び出しと数えている: %v", violations)
}

// **LIKE 側の順序もパラメータごとに見る (#3025 のレビュー H1)。**
func TestScanLikePatternGuards_DetectsPerValueLateGuard(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "repository")
	writeGoFile(t, dir, "r.go", `
func (r *repo) Search(query, meID string) ([]*model.Thing, error) {
	if !storable(meID) {
		return nil, nil
	}
	like := "%" + escapeLike(query) + "%"
	if !storable(query) {
		return nil, nil
	}
	return r.find(like, meID)
}
`)
	violations, _ := scanLikePatternGuards(t, root)
	require.Len(t, violations, 1, "別の値の guard が手前にあるだけで通している: %v", violations)
	assert.Contains(t, violations[0].reason, "後ろにある")
}
