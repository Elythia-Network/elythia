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

// cursorGuardMinSites is the number of cursor call sites the gate expects to
// find: 直接の `id.NormalizeCursor` と、それを伝播する wrapper の呼び出しの合計。
//
// **0 件で PASS させないための下限。** 抽出が壊れて 1 件も拾えなくなっても
// 「違反 0 件」と区別が付かず、検査していないのに緑になる。
const cursorGuardMinSites = 88

// cursorGuardViolation is one call site that lets an unstorable cursor through.
type cursorGuardViolation struct {
	file   string
	line   int
	fn     string
	reason string
}

// pos renders the call site as `file:line`.
func (v cursorGuardViolation) pos() string { return fmt.Sprintf("%s:%d", v.file, v.line) }

// scanCursorGuards reports cursor call sites that do not act on the ok result,
// plus the number of call sites inspected.
//
// 見るのは 2 種類の呼び出し:
//
//   - `id.NormalizeCursor(...)` そのもの
//   - **それを伝播する wrapper** — 直接の呼び出しを含み、かつ戻り値の最後が
//     `bool` の関数。現在 5 つある (`chatPageParams.cursor` /
//     `paginationFromRequest` / `listRequest.resolve` / `parseHostPage` /
//     `bindListRequest`)。wrapper を見ないと、新しい呼び出し側が `_` で
//     捨てても gate が黙る
//
// **wrapper は名前でしか引かない。** 同名で cursor と無関係なメソッドがあると
// 過検出になる (#3369 より前は、`internal/api/notes` の `listRequest.normalize` と
// `TimelineRequest.normalize` が同名だった。あちらも `if !req.normalize()` で
// 受けていたので違反にはならなかった)。検出側に
// 倒れるぶんには安全側なので、型解決は入れていない。**追跡は 1 段だけ**で、
// wrapper の wrapper は追わない (現状そういう形は無い)。
//
// **`_` で捨てる形を落とすのが主目的。** signature を 3 値にしたので呼び出し側は
// 必ず書き換わるが、`_` を書けば**コンパイルは通ったまま NUL が DB へ流れる**。
//
// **既知の範囲**: wrapper の追跡は 1 段だけで、wrapper の wrapper は追わない。
// 現状そういう形は無く、増えたときは `cursorGuardMinSites` の下限では気付けない。
func scanCursorGuards(t *testing.T, root string) ([]cursorGuardViolation, int) {
	t.Helper()

	fset := token.NewFileSet()
	files := map[string][]*ast.File{} // package dir -> parsed files

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
		dir := filepath.Dir(path)
		files[dir] = append(files[dir], f)
		return nil
	})
	require.NoError(t, err, "walk %s", root)

	var violations []cursorGuardViolation
	sites := 0
	for dir, pkgFiles := range files {
		_ = dir
		// pass 1: cursor の ok を伝播する wrapper を集める。
		propagators := map[string]bool{}
		for _, f := range pkgFiles {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil || !containsNormalizeCursorCall(fn.Body) {
					continue
				}
				if endsWithBoolResult(fn.Type) {
					propagators[fn.Name.Name] = true
				}
			}
		}
		// pass 2: 呼び出しの形を見る。
		for _, f := range pkgFiles {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				v, n := scanFuncCursorGuards(fset, fn, propagators)
				violations = append(violations, v...)
				sites += n
			}
		}
	}

	// **行番号は数として並べる。** 文字列で並べると 10 行目が 5 行目より前に
	// 来るので、報告の順序が読む順と食い違う。
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].file != violations[j].file {
			return violations[i].file < violations[j].file
		}
		return violations[i].line < violations[j].line
	})
	return violations, sites
}

// scanFuncCursorGuards checks every cursor call inside fn.
//
// 受け入れる形は 3 つ:
//
//   - `a, b, ok := f(...)` のあと、**カーソルの値を使う前に** `if ... !ok ... { ... }`
//     があり、その分岐が関数を抜ける (return / continue / break / panic)
//   - `if !f(...) { ... }` / `if a, b, ok := f(...); !ok { ... }` で、同じく抜ける
//   - `return f(...)` (呼び出し元へ伝播する)
//
// **`!ok` は「その ok」でなければならない (#3025 のレビュー H2)。** 関数のどこかに
// `!x` があればよい形にすると、手前の `pagination.ResolveLimit` の `limitOK` へ
// 受け直すだけで gate が黙る (**全 handler がその形の `!limitOK` を持っている**)。
//
// **「カーソルの値を使う前」で切るのが要点 (同 M3)。** 直後の 1 文に限ると、
// guard を 2 つ積む形や条件を束ねた形まで落としてしまい、しかも診断が
// 「guard が無い」と事実と逆を指す。逆に無制限に後ろを見ると、guard を SELECT の
// 後ろへ動かしても通ってしまう。
//
// **`return` を要求するのは「空に倒す」形を落とすため。** `if !ok { s, u = "", "" }`
// は「カーソル無し = 先頭から」になり、利用者の指定と無関係なページを正しい
// 応答として返す。
func scanFuncCursorGuards(fset *token.FileSet, fn *ast.FuncDecl, propagators map[string]bool) ([]cursorGuardViolation, int) {
	propagators = withCursorNormalizerParams(fn, propagators)
	guarded := map[ast.Expr]bool{}
	reasons := map[ast.Expr]string{}
	var calls []*ast.CallExpr

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch st := n.(type) {
		case *ast.BlockStmt:
			scanCursorGuardBlock(st.List, propagators, guarded, reasons)
		case *ast.CaseClause:
			// **`switch` / `select` の case は BlockStmt ではない。** 拾わないと
			// 正しく書かれた guard を「見ていない」と報告する (診断が事実と逆)。
			scanCursorGuardBlock(st.Body, propagators, guarded, reasons)
		case *ast.CommClause:
			scanCursorGuardBlock(st.Body, propagators, guarded, reasons)
		}
		if call, ok := n.(*ast.CallExpr); ok && isCursorCall(call, propagators) {
			calls = append(calls, call)
		}
		return true
	})

	var out []cursorGuardViolation
	for _, call := range calls {
		if guarded[call] {
			continue
		}
		at := fset.Position(call.Pos())
		reason := reasons[call]
		if reason == "" {
			reason = fmt.Sprintf("%s の ok を見ていない", callName(call))
		}
		out = append(out, cursorGuardViolation{at.Filename, at.Line, fn.Name.Name, reason})
	}
	return out, len(calls)
}

// scanCursorGuardBlock marks the cursor calls in one statement list that are
// guarded before their values are used.
func scanCursorGuardBlock(list []ast.Stmt, propagators map[string]bool, guarded map[ast.Expr]bool, reasons map[ast.Expr]string) {
	for i, stmt := range list {
		switch st := stmt.(type) {
		case *ast.ReturnStmt:
			for _, res := range st.Results {
				if call, ok := res.(*ast.CallExpr); ok && isCursorCall(call, propagators) {
					guarded[call] = true
				}
			}
		case *ast.IfStmt:
			if call, ok := negatedCall(st.Cond); ok && isCursorCall(call, propagators) {
				markCursorGuard(call, st, guarded, reasons)
				continue
			}
			init, ok := st.Init.(*ast.AssignStmt)
			if !ok {
				continue
			}
			call, okIdent := cursorAssign(init, propagators)
			if call == nil {
				continue
			}
			if okIdent == "" {
				reasons[call] = "ok を `_` で捨てている"
				continue
			}
			if !condNegates(st.Cond, okIdent) {
				reasons[call] = fmt.Sprintf("ok (%s) を `!%s` で見ていない", okIdent, okIdent)
				continue
			}
			markCursorGuard(call, st, guarded, reasons)
		case *ast.AssignStmt:
			call, okIdent := cursorAssign(st, propagators)
			if call == nil {
				continue
			}
			if okIdent == "" {
				reasons[call] = "ok を `_` で捨てている"
				continue
			}
			guardStmt, used := findCursorGuardStmt(list[i+1:], okIdent, cursorValueNames(st))
			if guardStmt == nil {
				if used {
					reasons[call] = fmt.Sprintf("カーソルの値を使う前に `!%s` を見ていない (SELECT が先に走る)", okIdent)
				} else {
					reasons[call] = fmt.Sprintf("`if !%s { ... }` が無い", okIdent)
				}
				continue
			}
			markCursorGuard(call, guardStmt, guarded, reasons)
		}
	}
}

// findCursorGuardStmt looks for the `if ... !ok ...` that precedes any use of the
// cursor values. used=true means a value was consumed before the guard appeared.
func findCursorGuardStmt(rest []ast.Stmt, okIdent string, values map[string]bool) (*ast.IfStmt, bool) {
	for _, stmt := range rest {
		if ifStmt, ok := stmt.(*ast.IfStmt); ok && condNegates(ifStmt.Cond, okIdent) {
			return ifStmt, false
		}
		if stmtUsesAny(stmt, values) {
			return nil, true
		}
	}
	return nil, false
}

// cursorValueNames returns the non-ok results of a cursor assignment.
func cursorValueNames(assign *ast.AssignStmt) map[string]bool {
	out := map[string]bool{}
	for _, lhs := range assign.Lhs[:max(0, len(assign.Lhs)-1)] {
		if ident, ok := lhs.(*ast.Ident); ok && ident.Name != "_" {
			out[ident.Name] = true
		}
	}
	return out
}

// stmtUsesAny reports whether stmt reads one of names.
func stmtUsesAny(stmt ast.Stmt, names map[string]bool) bool {
	found := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok && names[ident.Name] {
			found = true
		}
		return true
	})
	return found
}

// markCursorGuard accepts call only when the guard body leaves the flow.
func markCursorGuard(call *ast.CallExpr, ifStmt *ast.IfStmt, guarded map[ast.Expr]bool, reasons map[ast.Expr]string) {
	if !blockExits(ifStmt.Body) {
		reasons[call] = "ok=false の分岐が抜けない (空に倒して 200 を返すと、利用者の指定と無関係なページが正しい応答として返る)"
		return
	}
	guarded[call] = true
}

// cursorAssign returns the cursor call on the RHS of assign and the name the ok
// result is bound to ("" when it is discarded).
func cursorAssign(assign *ast.AssignStmt, propagators map[string]bool) (*ast.CallExpr, string) {
	if len(assign.Rhs) != 1 || len(assign.Lhs) == 0 {
		return nil, ""
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok || !isCursorCall(call, propagators) {
		return nil, ""
	}
	ident, isIdent := assign.Lhs[len(assign.Lhs)-1].(*ast.Ident)
	if !isIdent || ident.Name == "_" {
		return call, ""
	}
	return call, ident.Name
}

// negatedCall reports the call inside a `!f(...)` expression.
func negatedCall(e ast.Expr) (*ast.CallExpr, bool) {
	unary, ok := e.(*ast.UnaryExpr)
	if !ok || unary.Op != token.NOT {
		return nil, false
	}
	call, ok := unary.X.(*ast.CallExpr)
	return call, ok
}

// condNegates reports whether cond contains `!name` anywhere.
//
// **名前まで見る。** 「`!` が出てくるか」だけだと、手前の別の bool の否定を
// 根拠にできてしまう。**ただし `!name` 単独には限らない** — `if !ok || x < 0`
// のように条件を束ねるのは普通の書き方で、落とすと診断が事実と逆になる。
func condNegates(cond ast.Expr, name string) bool {
	found := false
	ast.Inspect(cond, func(n ast.Node) bool {
		unary, ok := n.(*ast.UnaryExpr)
		if !ok || unary.Op != token.NOT {
			return true
		}
		if ident, ok := unary.X.(*ast.Ident); ok && ident.Name == name {
			found = true
		}
		return true
	})
	return found
}

// blockExits reports whether body leaves the current flow.
func blockExits(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.ReturnStmt, *ast.BranchStmt:
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

// cursorNormalizers are the functions of internal/misc/id that normalize a
// cursor and report an unstorable one with ok=false.
//
// 本家のページ送りの形に合わせて2つある (#3369)。どちらも ok を返すので、同じく
// 捨てていないことを見る。
var cursorNormalizers = map[string]bool{"NormalizeCursor": true, "NormalizeCursorIDFirst": true}

// cursorNormalizerType is the type of a parameter that carries one of
// cursorNormalizers (`id.CursorNormalizer`).
const cursorNormalizerType = "CursorNormalizer"

// withCursorNormalizerParams returns propagators plus the names of fn's
// parameters of type id.CursorNormalizer, so that a call through such a
// parameter counts as a cursor call.
//
// **関数の値で受けて呼ぶ形も数える (#3369)。** 本家の形が endpoint ごとに違うので、
// 共有の handler (notes の serveTimeline、flash の my) は正規化の関数を引数で
// 受ける。その呼び出しを数えないと、ok を捨てても gate が黙る。
func withCursorNormalizerParams(fn *ast.FuncDecl, propagators map[string]bool) map[string]bool {
	var names []string
	for _, field := range fn.Type.Params.List {
		sel, ok := field.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != cursorNormalizerType {
			continue
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "id" {
			continue
		}
		for _, n := range field.Names {
			names = append(names, n.Name)
		}
	}
	if len(names) == 0 {
		return propagators
	}
	out := make(map[string]bool, len(propagators)+len(names))
	for k, v := range propagators {
		out[k] = v
	}
	for _, n := range names {
		out[n] = true
	}
	return out
}

// isCursorCall reports whether call is one of cursorNormalizers or one of the
// package-local wrappers that propagate its ok.
func isCursorCall(call *ast.CallExpr, propagators map[string]bool) bool {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := fun.X.(*ast.Ident); ok && pkg.Name == "id" && cursorNormalizers[fun.Sel.Name] {
			return true
		}
		return propagators[fun.Sel.Name]
	case *ast.Ident:
		return propagators[fun.Name]
	}
	return false
}

// containsNormalizeCursorCall reports whether body calls one of
// cursorNormalizers.
func containsNormalizeCursorCall(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !cursorNormalizers[sel.Sel.Name] {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "id" {
			found = true
		}
		return true
	})
	return found
}

// endsWithBoolResult reports whether the last result of sig is a bare `bool`.
func endsWithBoolResult(sig *ast.FuncType) bool {
	if sig.Results == nil || len(sig.Results.List) == 0 {
		return false
	}
	last := sig.Results.List[len(sig.Results.List)-1]
	ident, ok := last.Type.(*ast.Ident)
	return ok && ident.Name == "bool"
}

// callName renders the callee for the failure message.
func callName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name
	case *ast.Ident:
		return fun.Name
	}
	return "cursor call"
}

// TestCursorGuardsAreChecked is the pagination-cursor gate (#3025).
//
// **列に入らないカーソルをそのまま repository へ渡すと 500 になる。** NUL を
// 含む値は `id < ?` の bind parameter に載せた時点で PostgreSQL が落とす
// (手元の simple protocol で SQLSTATE 08P01、本番の pgx extended protocol で
// 22021) ので、認証済みの一般利用者がパラメータ 1 文字で 5xx を立てられる。
// `id.NormalizeCursor` はそれを ok=false で返すが、**`_` で捨てれば元通りに
// なる**ので、捨てていないことを静的に見る。
func TestCursorGuardsAreChecked(t *testing.T) {
	root := filepath.Join(repoRoot(t), "internal", "api")
	violations, sites := scanCursorGuards(t, root)

	require.GreaterOrEqual(t, sites, cursorGuardMinSites,
		"カーソルの呼び出しを %d 件しか拾えていない (抽出が壊れると検査していないのに緑になる)", sites)

	if len(violations) > 0 {
		var b strings.Builder
		b.WriteString("列に入らないカーソルが repository へ抜ける経路がある (#3025):\n")
		for _, v := range violations {
			fmt.Fprintf(&b, "  %s (%s): %s\n", v.pos(), v.fn, v.reason)
		}
		b.WriteString("\nok=false のときは 400 を返すこと (`return apierr.JSONInvalidParam(c)`)。\n")
		t.Fatal(b.String())
	}
}

// 抽出器自身のテスト。**これが無いと gate が空振りする** — 違反 0 件が正常な
// 状態なので、述語を壊しても本物のソースからは何も落ちない (notfound gate が
// 同じ理由で抽出器のテストを持っている)。
func TestScanCursorGuards_DetectsDiscardedOK(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "api", "probe")
	writeGoFile(t, dir, "h.go", `
func (h *Handler) Discarded(c echo.Context) error {
	sinceID, untilID, _ := id.NormalizeCursor(req.SinceID, req.UntilID, nil, nil)
	return h.list(sinceID, untilID)
}

func (h *Handler) Unchecked(c echo.Context) error {
	sinceID, untilID, ok := id.NormalizeCursor(req.SinceID, req.UntilID, nil, nil)
	_ = ok
	return h.list(sinceID, untilID)
}

func (h *Handler) Guarded(c echo.Context) error {
	sinceID, untilID, ok := id.NormalizeCursor(req.SinceID, req.UntilID, nil, nil)
	if !ok {
		return apierr.JSONInvalidParam(c)
	}
	return h.list(sinceID, untilID)
}

func (p params) cursor() (string, string, bool) {
	return id.NormalizeCursor(p.SinceID, p.UntilID, p.SinceDate, p.UntilDate)
}
`)
	violations, sites := scanCursorGuards(t, root)

	assert.Equal(t, 4, sites, "呼び出しの数え落とし (return 伝播も 1 件として数える)")
	require.Len(t, violations, 2, "違反 2 件だけを拾う: %v", violations)
	assert.Contains(t, violations[0].fn, "Discarded")
	assert.Contains(t, violations[1].fn, "Unchecked")
}

// **ID 優先の正規化と、引数で受けた正規化の関数も見る (#3369)。** 本家の
// ページ送りの形に合わせて id.NormalizeCursorIDFirst を使う handler と、共有の
// handler が id.CursorNormalizer を引数で受けて呼ぶ形を数えないと、ok を捨てても
// gate が黙る。
func TestScanCursorGuards_ChecksIDFirstAndNormalizerParams(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "api", "probe")
	writeGoFile(t, dir, "h.go", `
func (h *Handler) IDFirstDiscarded(c echo.Context) error {
	sinceID, untilID, _ := id.NormalizeCursorIDFirst(req.SinceID, req.UntilID, nil, nil)
	return h.list(sinceID, untilID)
}

func (h *Handler) ParamDiscarded(c echo.Context, normalizeCursor id.CursorNormalizer) error {
	sinceID, untilID, _ := normalizeCursor(req.SinceID, req.UntilID, nil, nil)
	return h.list(sinceID, untilID)
}

func (h *Handler) ParamGuarded(c echo.Context, normalizeCursor id.CursorNormalizer) error {
	sinceID, untilID, ok := normalizeCursor(req.SinceID, req.UntilID, nil, nil)
	if !ok {
		return apierr.JSONInvalidParam(c)
	}
	return h.list(sinceID, untilID)
}
`)
	violations, sites := scanCursorGuards(t, root)

	assert.Equal(t, 3, sites, "ID 優先の正規化か、引数の正規化の関数の呼び出しを数えていない")
	require.Len(t, violations, 2, "違反 2 件だけを拾う: %v", violations)
	assert.Contains(t, violations[0].fn, "IDFirstDiscarded")
	assert.Contains(t, violations[1].fn, "ParamDiscarded")
}

// **wrapper 経由の呼び出しも見る。** `id.NormalizeCursor` だけを見ていると、
// ok を伝播する helper (chat の `cursor()` 等) の新しい呼び出し側が `_` で
// 捨てても gate が黙る。
func TestScanCursorGuards_ChecksWrapperCallSites(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "api", "probe")
	writeGoFile(t, dir, "h.go", `
func (p params) cursor() (string, string, bool) {
	return id.NormalizeCursor(p.SinceID, p.UntilID, p.SinceDate, p.UntilDate)
}

func (h *Handler) Discarded(c echo.Context) error {
	sinceID, untilID, _ := req.cursor()
	return h.list(sinceID, untilID)
}

func (h *Handler) Guarded(c echo.Context) error {
	sinceID, untilID, ok := req.cursor()
	if !ok {
		return apierr.JSONInvalidParam(c)
	}
	return h.list(sinceID, untilID)
}
`)
	violations, sites := scanCursorGuards(t, root)

	assert.Equal(t, 3, sites, "wrapper の呼び出しを数えていない")
	require.Len(t, violations, 1, "違反 1 件だけを拾う: %v", violations)
	assert.Contains(t, violations[0].fn, "Discarded")
	assert.Contains(t, violations[0].reason, "`_`")
}

// **裸の関数呼び出しの wrapper も見る。** `paginationFromRequest` /
// `parseHostPage` はメソッドではないので、`*ast.Ident` の分岐を落とすと**この 6
// 箇所だけが黙って検査対象から外れる** (実測で下限 75 をちょうど満たして PASS
// した)。セレクタ形の fixture しか無いとその変異が生き残る。
func TestScanCursorGuards_ChecksBareFunctionWrapper(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "api", "probe")
	writeGoFile(t, dir, "h.go", `
func pageParams(c echo.Context) (int, string, bool) {
	limit, untilID, ok := id.NormalizeCursor("", "", nil, nil)
	if !ok {
		return 0, "", false
	}
	return limit, untilID, true
}

func (h *Handler) Discarded(c echo.Context) error {
	limit, untilID, _ := pageParams(c)
	return h.list(limit, untilID)
}

func (h *Handler) Guarded(c echo.Context) error {
	limit, untilID, ok := pageParams(c)
	if !ok {
		return apierr.JSONInvalidParam(c)
	}
	return h.list(limit, untilID)
}
`)
	violations, sites := scanCursorGuards(t, root)

	assert.Equal(t, 3, sites, "裸の関数 wrapper の呼び出しを数えていない")
	require.Len(t, violations, 1, "違反 1 件だけを拾う: %v", violations)
	assert.Contains(t, violations[0].fn, "Discarded")
}

// **手前にある別の `!ok` を根拠にしない (レビュー H2)。** 全 handler は直前に
// `pagination.ResolveLimit` の `if !limitOK` を持つので、cursor の ok をそこへ
// 受け直すだけで gate を黙らせられた (実測で go build / vet / gate すべて緑)。
func TestScanCursorGuards_DoesNotBorrowEarlierNegation(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "api", "probe")
	writeGoFile(t, dir, "h.go", `
func (h *Handler) Reused(c echo.Context) error {
	limit, limitOK := pagination.ResolveLimit(req.Limit, 30, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	sinceID, untilID, limitOK := id.NormalizeCursor(req.SinceID, req.UntilID, nil, nil)
	_ = limitOK
	return h.list(sinceID, untilID, limit)
}
`)
	violations, _ := scanCursorGuards(t, root)
	require.Len(t, violations, 1, "手前の `!limitOK` を根拠にしている: %v", violations)
	assert.Contains(t, violations[0].reason, "limitOK")
}

// **guard が DB 呼び出しの後ろにあったら落とす (レビュー M3)。** 順序を見ないと
// 「引く前に弾く」という設計原則を gate が固定できない。
func TestScanCursorGuards_DetectsLateGuard(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "api", "probe")
	writeGoFile(t, dir, "h.go", `
func (h *Handler) Late(c echo.Context) error {
	sinceID, untilID, ok := id.NormalizeCursor(req.SinceID, req.UntilID, nil, nil)
	rows, err := h.repo.List(sinceID, untilID)
	if !ok {
		return apierr.JSONInvalidParam(c)
	}
	return c.JSON(200, rows)
}
`)
	violations, _ := scanCursorGuards(t, root)
	require.Len(t, violations, 1, "guard が SELECT より後ろでも通している: %v", violations)
	assert.Contains(t, violations[0].reason, "使う前に")
}

// **空に倒して 200 を返す形も落とす (レビュー M3)。** 空文字は「カーソル無し =
// 先頭から」の意味なので、利用者の指定と無関係なページを正しい応答として返す。
func TestScanCursorGuards_DetectsEmptyFallback(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "api", "probe")
	writeGoFile(t, dir, "h.go", `
func (h *Handler) Fallback(c echo.Context) error {
	sinceID, untilID, ok := id.NormalizeCursor(req.SinceID, req.UntilID, nil, nil)
	if !ok {
		sinceID, untilID = "", ""
	}
	return h.list(sinceID, untilID)
}
`)
	violations, _ := scanCursorGuards(t, root)
	require.Len(t, violations, 1, "空に倒す形を通している: %v", violations)
	assert.Contains(t, violations[0].reason, "抜けない")
}

// `if !f(...)` の形も受け入れる (戻り値が bool 1 つの wrapper はこう書かれる)。
func TestScanCursorGuards_AcceptsInlineNegation(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "api", "probe")
	writeGoFile(t, dir, "h.go", `
func (r *listRequest) normalize() bool {
	s, u, ok := id.NormalizeCursor(r.SinceID, r.UntilID, r.SinceDate, r.UntilDate)
	if !ok {
		return false
	}
	r.SinceID, r.UntilID = s, u
	return true
}

func (h *Handler) Guarded(c echo.Context) error {
	if !req.normalize() {
		return apierr.JSONInvalidParam(c)
	}
	return nil
}
`)
	violations, sites := scanCursorGuards(t, root)
	assert.Equal(t, 2, sites)
	assert.Empty(t, violations, "正しい形を検出し続けている: %v", violations)
}

// **別の関数にある `!ok` を根拠にしない。** 関数をまたいで探すと、隣の handler が
// 正しく書かれているだけで穴が塞がったことになる。
func TestScanCursorGuards_DoesNotBorrowGuardFromAnotherFunc(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "api", "probe")
	writeGoFile(t, dir, "h.go", `
func (h *Handler) Neighbour(c echo.Context) error {
	ok := h.check()
	if !ok {
		return apierr.JSONInvalidParam(c)
	}
	return nil
}

func (h *Handler) Unchecked(c echo.Context) error {
	sinceID, untilID, ok := id.NormalizeCursor(req.SinceID, req.UntilID, nil, nil)
	return h.list(sinceID, untilID, ok)
}
`)
	violations, _ := scanCursorGuards(t, root)
	require.Len(t, violations, 1, "隣の関数の `!ok` を根拠にしている: %v", violations)
	assert.Contains(t, violations[0].fn, "Unchecked")
}

// 抽出器が propagator を**関数名だけ**で引くので、同名で cursor と無関係な
// メソッドがあると過検出になる (`internal/api/notes` の `TimelineRequest.normalize`
// が該当。あちらも `if !req.normalize()` で受けているので違反にはならない)。
// 検出側に倒れるぶんには安全側なので、型解決は入れていない。
func TestScanCursorGuards_OvercountsSameNamedMethod(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "api", "probe")
	writeGoFile(t, dir, "h.go", `
func (r *listRequest) normalize() bool {
	s, u, ok := id.NormalizeCursor(r.SinceID, r.UntilID, nil, nil)
	if !ok {
		return false
	}
	r.SinceID, r.UntilID = s, u
	return true
}

func (r *otherRequest) normalize() bool {
	return r.Limit > 0
}

func (h *Handler) UsesOther(c echo.Context) error {
	if !other.normalize() {
		return apierr.JSONInvalidParam(c)
	}
	return nil
}
`)
	violations, sites := scanCursorGuards(t, root)
	assert.Equal(t, 2, sites, "同名メソッドも cursor 呼び出しとして数える (過検出、既知)")
	assert.Empty(t, violations, "正しく受けている形を落としている: %v", violations)
}

// **正当な書き方を落とさない (#3025 のレビュー M2)。** 「直後の 1 文」に限ると、
// guard を 2 つ積む / 条件を束ねる / `switch` の case に置く / ループで
// `continue` する、のどれも偽陽性になり、しかも診断が「guard が無い」と
// 事実と逆を指す。
func TestScanCursorGuards_AcceptsOrdinaryGuardShapes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "api", "probe")
	writeGoFile(t, dir, "h.go", `
func (h *Handler) Stacked(c echo.Context) error {
	limit, limitOK := pagination.ResolveLimit(req.Limit, 30, 100)
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, nil, nil)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	return h.list(sinceID, untilID, limit)
}

func (h *Handler) Combined(c echo.Context) error {
	sinceID, untilID, ok := id.NormalizeCursor(req.SinceID, req.UntilID, nil, nil)
	if !ok || req.Offset < 0 {
		return apierr.JSONInvalidParam(c)
	}
	return h.list(sinceID, untilID)
}

func (h *Handler) Switched(c echo.Context) error {
	switch req.Kind {
	case "a":
		sinceID, untilID, ok := id.NormalizeCursor(req.SinceID, req.UntilID, nil, nil)
		if !ok {
			return apierr.JSONInvalidParam(c)
		}
		return h.list(sinceID, untilID)
	}
	return nil
}

func (h *Handler) Looping(c echo.Context) error {
	for _, r := range reqs {
		sinceID, untilID, ok := id.NormalizeCursor(r.SinceID, r.UntilID, nil, nil)
		if !ok {
			continue
		}
		h.collect(sinceID, untilID)
	}
	return nil
}
`)
	violations, sites := scanCursorGuards(t, root)
	assert.Equal(t, 4, sites)
	assert.Empty(t, violations, "正当な書き方を落としている: %v", violations)
}

// cursorParamMinHandlers is the number of functions that bind a cursor param in
// an inline request struct.
// **実測に合わせた下限** — 抽出が壊れても「違反 0 件」と区別が付かない。
const cursorParamMinHandlers = 44

// cursorQueryParamMinHandlers is the same lower bound for handlers that read a
// cursor straight off the query string (`c.QueryParam("cursor")` など)。
//
// **struct タグだけを見ていると視界に入らない形がある。** `internal/api/ap` の
// followers / following / outbox は `c.QueryParam` で cursor を読んで
// `id < ?` に直接載せており、**未認証で叩けるのに #3025 の両ゲートの射程外**
// だった (bind 側ゲートは camelCase の `untilId` / `sinceId` を struct タグで
// 探すので、snake_case の `since_id` にも `cursor` にも当たらない)。
//
// 別々に数えるのが要点 — 合算すると、こちらの抽出が丸ごと壊れても struct タグ
// 側の 44 件で下限を満たしてしまう。
const cursorQueryParamMinHandlers = 2

// scanCursorParamBinders reports functions that bind `untilId` / `sinceId` in an
// inline request struct without normalizing it, plus the number inspected.
//
// **これが無いと「新しく足す」方向が塞がらない (#3025 のレビュー M2)。** 既存の
// guard を外す変異は件数の下限で捕まるが、`untilId` を bind して
// `q.Where("id < ?", req.UntilID)` に直接載せる handler を**新しく足す**形は、
// カーソル gate の視界に一度も入らない。`list-mine` と
// `admin/emoji-application/*` が実際にその状態だった。
//
// **既知の範囲**: 関数の中で宣言した匿名 struct だけを見る。package レベルの
// request 型 (`chatPageParams` / `ListRequest` / `TimelineRequest` /
// `listRequest` / `hostPageRequest`) は wrapper 側で正規化しており、そちらは
// wrapper の呼び出し側を見る形で押さえてある。
func scanCursorParamBinders(t *testing.T, root string) ([]cursorGuardViolation, int, int) {
	t.Helper()

	fset := token.NewFileSet()
	var violations []cursorGuardViolation
	binders := 0
	queryBinders := 0

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
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		propagators := map[string]bool{
			"cursor": true, "resolve": true, "paginationFromRequest": true,
			"parseHostPage": true, "bindListRequest": true,
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			viaTag := bindsCursorParam(fn.Body)
			viaQuery := bindsCursorQueryParam(fn.Body)
			if !viaTag && !viaQuery {
				continue
			}
			if viaTag {
				binders++
			}
			if viaQuery {
				queryBinders++
			}
			if hasCursorCall(fn.Body, withCursorNormalizerParams(fn, propagators)) {
				continue
			}
			reason := "untilId / sinceId を bind しているのに正規化していない"
			if !viaTag {
				reason = "クエリ文字列からカーソルを読んでいるのに正規化していない"
			}
			at := fset.Position(fn.Pos())
			violations = append(violations, cursorGuardViolation{at.Filename, at.Line, fn.Name.Name, reason})
		}
		return nil
	})
	require.NoError(t, err, "walk %s", root)

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].file != violations[j].file {
			return violations[i].file < violations[j].file
		}
		return violations[i].line < violations[j].line
	})
	return violations, binders, queryBinders
}

// cursorQueryParamNames are the query-string names that carry a pagination
// cursor. **snake_case と camelCase の両方を見る** — `internal/api/ap` は AP の
// 慣習で snake_case を使い、REST 側は camelCase を使う。
var cursorQueryParamNames = map[string]bool{
	"cursor": true, "since_id": true, "until_id": true,
	"sinceId": true, "untilId": true, "sinceid": true, "untilid": true,
	"max_id": true, "min_id": true,
}

// bindsCursorQueryParam reports whether body reads a cursor straight off the
// query string (`c.QueryParam("cursor")` など)。
//
// **レシーバ名は見ない。** echo の context は慣習的に `c` だが、そこに依存すると
// 別名を付けた handler が黙って検査対象から外れる。メソッド名と引数の
// リテラルだけで判定する。
func bindsCursorQueryParam(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "QueryParam" && sel.Sel.Name != "FormValue") {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if len(lit.Value) >= 2 && cursorQueryParamNames[lit.Value[1:len(lit.Value)-1]] {
			found = true
		}
		return true
	})
	return found
}

// bindsCursorParam reports whether body declares a struct field tagged untilId
// or sinceId.
func bindsCursorParam(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		field, ok := n.(*ast.Field)
		if !ok || field.Tag == nil {
			return true
		}
		if strings.Contains(field.Tag.Value, `json:"untilId"`) || strings.Contains(field.Tag.Value, `json:"sinceId"`) {
			found = true
		}
		return true
	})
	return found
}

// hasCursorCall reports whether body calls NormalizeCursor or one of the wrappers.
func hasCursorCall(body *ast.BlockStmt, propagators map[string]bool) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && isCursorCall(call, propagators) {
			found = true
		}
		return true
	})
	return found
}

// TestCursorParamsAreNormalized is the other half of the cursor gate (#3025).
//
// **カーソルを受け取ったのに正規化しない handler を足せてしまう**のを止める。
// `id.NormalizeCursor` の呼び出し側だけを見ていると、そもそも呼んでいない
// handler は視界に入らない。
func TestCursorParamsAreNormalized(t *testing.T) {
	root := filepath.Join(repoRoot(t), "internal", "api")
	violations, binders, queryBinders := scanCursorParamBinders(t, root)

	require.GreaterOrEqual(t, binders, cursorParamMinHandlers,
		"カーソルを bind する handler を %d 件しか拾えていない (抽出が壊れると検査していないのに緑になる)", binders)
	require.GreaterOrEqual(t, queryBinders, cursorQueryParamMinHandlers,
		"クエリ文字列からカーソルを読む handler を %d 件しか拾えていない (抽出が壊れると検査していないのに緑になる)", queryBinders)

	if len(violations) > 0 {
		var b strings.Builder
		b.WriteString("カーソルを受け取っているのに正規化していない handler がある (#3025):\n")
		for _, v := range violations {
			fmt.Fprintf(&b, "  %s (%s): %s\n", v.pos(), v.fn, v.reason)
		}
		b.WriteString("\n`id.NormalizeCursor` を通し、ok=false なら 400 を返すこと。\n")
		b.WriteString("結果に使わないパラメータでも通すこと (upstream の ajv も `misskey:id` で弾く)。\n")
		t.Fatal(b.String())
	}
}

// 引数で受けた正規化の関数を呼ぶ handler は、正規化しているとみなす (#3369)。
func TestScanCursorParamBinders_AcceptsNormalizerParam(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "api", "probe")
	writeGoFile(t, dir, "h.go", `
func (h *Handler) my(c echo.Context, normalizeCursor id.CursorNormalizer) error {
	var req struct {
		SinceID string `+"`"+`json:"sinceId"`+"`"+`
	}
	_ = c.Bind(&req)
	sinceID, _, ok := normalizeCursor(req.SinceID, "", nil, nil)
	if !ok {
		return apierr.JSONInvalidParam(c)
	}
	return h.repo.List(sinceID)
}

func (h *Handler) otherType(c echo.Context, normalizeCursor id.Generator) error {
	var req struct {
		SinceID string `+"`"+`json:"sinceId"`+"`"+`
	}
	_ = c.Bind(&req)
	return h.repo.List(normalizeCursor(req.SinceID))
}

func (h *Handler) otherPackage(c echo.Context, normalizeCursor strutil.CursorNormalizer) error {
	var req struct {
		SinceID string `+"`"+`json:"sinceId"`+"`"+`
	}
	_ = c.Bind(&req)
	return h.repo.List(normalizeCursor(req.SinceID))
}
`)
	violations, binders, _ := scanCursorParamBinders(t, root)

	assert.Equal(t, 3, binders)
	require.Len(t, violations, 2, "id.CursorNormalizer でない引数は正規化とみなさない: %v", violations)
	assert.Equal(t, "otherType", violations[0].fn)
	assert.Equal(t, "otherPackage", violations[1].fn)
}

// 抽出器自身のテスト。
func TestScanCursorParamBinders_DetectsUnnormalizedBinder(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "api", "probe")
	writeGoFile(t, dir, "h.go", `
func (h *Handler) Raw(c echo.Context) error {
	var req struct {
		UntilID string `+"`"+`json:"untilId"`+"`"+`
	}
	_ = c.Bind(&req)
	return h.repo.List(req.UntilID)
}

func (h *Handler) Normalized(c echo.Context) error {
	var req struct {
		SinceID string `+"`"+`json:"sinceId"`+"`"+`
	}
	_ = c.Bind(&req)
	sinceID, _, ok := id.NormalizeCursor(req.SinceID, "", nil, nil)
	if !ok {
		return apierr.JSONInvalidParam(c)
	}
	return h.repo.List(sinceID)
}

func (h *Handler) ViaWrapper(c echo.Context) error {
	var req struct {
		UntilID string `+"`"+`json:"untilId"`+"`"+`
	}
	_ = c.Bind(&req)
	sinceID, untilID, ok := req.cursor()
	if !ok {
		return apierr.JSONInvalidParam(c)
	}
	return h.repo.List(sinceID, untilID)
}

func (h *Handler) NoCursor(c echo.Context) error {
	var req struct {
		Limit int `+"`"+`json:"limit"`+"`"+`
	}
	_ = c.Bind(&req)
	return h.repo.List(req.Limit)
}
`)
	violations, binders, queryBinders := scanCursorParamBinders(t, root)

	assert.Equal(t, 3, binders, "カーソルを bind する関数だけを数える")
	assert.Equal(t, 0, queryBinders, "struct タグ経由はクエリ側に数えない")
	require.Len(t, violations, 1, "違反 1 件だけを拾う: %v", violations)
	assert.Contains(t, violations[0].fn, "Raw")
}

// クエリ文字列からカーソルを読む形も拾う。
//
// **`internal/api/ap` がこの形で漏れていた** — struct タグを探すだけの抽出では
// 一度も視界に入らない。
func TestScanCursorParamBinders_DetectsQueryStringCursor(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "api", "probe")
	writeGoFile(t, dir, "h.go", `
func (h *Handler) RawCursor(c echo.Context) error {
	return h.repo.List(c.QueryParam("cursor"))
}

func (h *Handler) RawSnakeCase(c echo.Context) error {
	return h.repo.List(c.QueryParam("until_id"), c.QueryParam("since_id"))
}

func (h *Handler) NormalizedQuery(c echo.Context) error {
	_, untilID, ok := id.NormalizeCursor("", c.QueryParam("cursor"), nil, nil)
	if !ok {
		return apierr.JSONInvalidParam(c)
	}
	return h.repo.List(untilID)
}

func (h *Handler) UnrelatedQuery(c echo.Context) error {
	return h.repo.List(c.QueryParam("page"))
}
`)
	violations, binders, queryBinders := scanCursorParamBinders(t, root)

	assert.Equal(t, 0, binders, "struct タグは 1 つも無い")
	assert.Equal(t, 3, queryBinders, "カーソル名のクエリを読む関数だけを数える")
	require.Len(t, violations, 2, "違反 2 件を拾う: %v", violations)
	assert.Contains(t, violations[0].fn+violations[1].fn, "RawCursor")
	assert.Contains(t, violations[0].fn+violations[1].fn, "RawSnakeCase")
}
