package entitycompat

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// routeRef is one `X.<METHOD>("/path", handler, ...)` registration.
type routeRef struct {
	pos    token.Position
	method string // GET / POST / PUT / DELETE
	path   string // "/notes/create"
	// apiRoot reports that the receiver is the /api group itself (an `api`
	// ident or a value of type apiRoutes), so path is relative to /api.
	apiRoot bool
	// apiDerived reports that the receiver is a group derived from /api
	// (`api.Group(...)`), so path is relative to that group.
	apiDerived bool
	recv       string // the handler's receiver expression, e.g. h / h.notes ("" when not a method value)
	handler    string // the handler method name ("" when not a method value)
	pkg        string // import path the receiver's type belongs to ("" when unresolved)
	local      string // type name in the package being read, when the receiver is a local type
}

// typeRef is a resolved static type: either a type of another package or a
// type declared in the package being read.
type typeRef struct {
	pkg   string // import path
	local string // type name in the package being read
}

type funcAt struct {
	decl *ast.FuncDecl
	file int
}

type typeAt struct {
	spec *ast.TypeSpec
	file int
}

type valueAt struct {
	spec *ast.ValueSpec
	idx  int
	file int
}

// routeIndex holds what resolving a handler needs across the files of one
// package.
type routeIndex struct {
	fset    *token.FileSet
	files   []*ast.File
	imports []map[string]string
	funcs   map[string][]funcAt
	types   map[string]typeAt
	vars    map[string]valueAt

	conflictCache map[declKey]bool
}

// declKey caches conflicting per function and name. setupRoutes は数千行あり、
// route ごとに本体を歩き直すと遅い。
type declKey struct {
	fn   ast.Node
	name string
}

// scope is where a name is looked up: the ancestors of the use, outermost
// first, and the position of the use. The zero value means package level.
type scope struct {
	stack []ast.Node
	pos   token.Pos
}

// collectRoutes parses the sources of one package and returns every route
// registration with its handler resolved to a package.
//
// **型で引く (#3672 レビュー H-1)。** 以前は `h := pkg.NewX(...)` の形を正規表現で
// 拾って名前の表を作り、route のファイルに無ければ他のファイルの表へ落として
// いた。それだと、段階 2 で普通に書く `func registerNotesRoutes(api apiRoutes,
// h *notes.Handler)` の `h` が、別のファイルの `h := pluginsecrets.New(...)` に
// 取り違えられ、notes の error id が黙って検査から外れた (敵対的レビューで実測。
// notesHandler を使う 1 行の登録 37 本を移した形で、全テストが緑のままだった)。
// 今は Go のスコープの順に引く。
//
//  1. route を囲むブロック (内側から): `{}`、if / for / switch の初期化、range、
//     case、関数の引数。ブロックの中では route より前の宣言だけを見る
//  2. パッケージのトップレベルの `var` (どのファイルでもよい。Go と同じ)
//
// 1 で宣言が見つかったら、型が分からなくても外側へは行かない (別の変数を拾わない)。
// **さらに、同じ関数の中で同じ名前の宣言 (`=` の代入は数えない) が 2 つ以上あり、
// 引いた型が食い違う (引けないものが混じる) ときは「引けない」に倒す** (#3672
// レビュー 2 周目 H-1)。ブロックの規則を読み違えても、黙って別の package に解決しない
// ため。引けない /api の route は TestAPIRouteHandlersResolve が落とす。
//
// 型は次から引く: 引数や `var` の型、`pkg.NewX(...)`、`&pkg.T{}` / `pkg.T{}`、
// 同じパッケージの関数やメソッドの戻り値の型 (切り出した関数が handler を返す形)、
// 同じパッケージの構造体のフィールドの型 (`h.notes.Create`)。
//
// **go/types で型検査しない理由。** internal/server の import 先 (約 930 package) の
// export data が要り、`go list -export` はキャッシュが冷えていると 8 コアで 38 秒
// (CPU 3 分 50 秒) かかった。CI の test は -race と -cover で build するので、
// -race 無しの export data はキャッシュに無く、毎回この compile が走る。
// `go list -race -export` なら test の build が温めたものを使えるが、それでも
// entitycompat が入る shard の build の後で約 18 秒かかった (レビュー 3 周目)。
// 残る限界 (import やパッケージの型を隠すローカル) は docs/gates.md にある。
func collectRoutes(names, srcs []string) (*routeIndex, []routeRef, error) {
	ix := &routeIndex{
		fset:  token.NewFileSet(),
		funcs: map[string][]funcAt{},
		types: map[string]typeAt{},
		vars:  map[string]valueAt{},

		conflictCache: map[declKey]bool{},
	}
	for i, name := range names {
		f, err := parser.ParseFile(ix.fset, name, srcs[i], 0)
		if err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", name, err)
		}
		fi := len(ix.files)
		ix.files = append(ix.files, f)
		imports := map[string]string{}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			n := p[strings.LastIndex(p, "/")+1:]
			if imp.Name != nil {
				n = imp.Name.Name
			}
			imports[n] = p
		}
		ix.imports = append(ix.imports, imports)
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				ix.funcs[d.Name.Name] = append(ix.funcs[d.Name.Name], funcAt{decl: d, file: fi})
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						ix.types[s.Name.Name] = typeAt{spec: s, file: fi}
					case *ast.ValueSpec:
						if d.Tok != token.VAR {
							continue
						}
						for j, n := range s.Names {
							ix.vars[n.Name] = valueAt{spec: s, idx: j, file: fi}
						}
					}
				}
			}
		}
	}

	var refs []routeRef
	for fi, f := range ix.files {
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "GET", "POST", "PUT", "DELETE":
			default:
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			path, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.HasPrefix(path, "/") {
				return true
			}
			sc := scope{stack: append([]ast.Node(nil), stack...), pos: call.Pos()}
			ref := routeRef{
				pos:    ix.fset.Position(call.Pos()),
				method: sel.Sel.Name,
				path:   path,
			}
			ref.apiRoot = ix.isAPIRoot(sel.X, fi, sc)
			ref.apiDerived = !ref.apiRoot && ix.isAPIDerived(sel.X, fi, sc, 0)
			if h, ok := call.Args[1].(*ast.SelectorExpr); ok {
				ref.recv = nodeString(h.X)
				ref.handler = h.Sel.Name
				if t, ok := ix.typeOfExpr(h.X, fi, sc, 0); ok {
					ref.pkg, ref.local = t.pkg, t.local
				}
			}
			refs = append(refs, ref)
			return true
		})
	}
	return ix, refs, nil
}

// maxResolveDepth bounds the recursion through variable copies and calls.
const maxResolveDepth = 8

// isAPIRoot reports whether x is the /api group itself.
func (ix *routeIndex) isAPIRoot(x ast.Expr, fi int, sc scope) bool {
	if id, ok := x.(*ast.Ident); ok && id.Name == "api" {
		return true
	}
	t, ok := ix.typeOfExpr(x, fi, sc, 0)
	return ok && t.local == "apiRoutes"
}

// isAPIDerived reports whether x is a group made by `.Group(...)` from the /api
// group, directly or through a variable.
func (ix *routeIndex) isAPIDerived(x ast.Expr, fi int, sc scope, depth int) bool {
	if depth > maxResolveDepth {
		return false
	}
	switch v := x.(type) {
	case *ast.ParenExpr:
		return ix.isAPIDerived(v.X, fi, sc, depth+1)
	case *ast.CallExpr:
		sel, ok := v.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Group" {
			return false
		}
		return ix.isAPIRoot(sel.X, fi, sc) || ix.isAPIDerived(sel.X, fi, sc, depth+1)
	case *ast.Ident:
		d := ix.lookupVar(v.Name, fi, sc, depth+1)
		return d.rhs != nil && ix.isAPIDerived(d.rhs, d.file, d.scope, depth+1)
	}
	return false
}

// varDecl is what lookupVar found for a name.
type varDecl struct {
	typ      typeRef
	ok       bool     // typ is resolved
	declared bool     // a declaration was found
	rhs      ast.Expr // the value of the declaration, when there is one
	file     int      // file of the declaration
	scope    scope    // scope to resolve rhs in
}

// lookupVar resolves a variable name the way Go scopes it: the blocks
// enclosing the use innermost first (declarations before the use only), then
// the package-level vars of every file.
func (ix *routeIndex) lookupVar(name string, fi int, sc scope, depth int) varDecl {
	if depth > maxResolveDepth {
		return varDecl{}
	}
	for i := len(sc.stack) - 1; i >= 0; i-- {
		var child ast.Node
		if i+1 < len(sc.stack) {
			child = sc.stack[i+1]
		}
		at := scope{stack: sc.stack[:i+1], pos: sc.pos}
		d, found := ix.declaredIn(sc.stack[i], child, name, fi, at, depth)
		if !found {
			continue
		}
		if fn := enclosingFunc(sc.stack[:i+1]); fn != nil && ix.conflicting(fn, name, fi, at, depth) {
			return varDecl{declared: true, file: fi}
		}
		return d
	}
	if v, ok := ix.vars[name]; ok {
		d := varDecl{declared: true, file: v.file}
		if v.spec.Type != nil {
			d.typ, d.ok = ix.typeOfTypeExpr(v.spec.Type, v.file)
			return d
		}
		if v.idx < len(v.spec.Values) {
			d.rhs = v.spec.Values[v.idx]
			d.typ, d.ok = ix.typeOfValue(d.rhs, 0, v.file, scope{}, depth+1)
		}
		return d
	}
	return varDecl{}
}

// enclosingFunc returns the innermost function in stack.
func enclosingFunc(stack []ast.Node) ast.Node {
	for i := len(stack) - 1; i >= 0; i-- {
		switch stack[i].(type) {
		case *ast.FuncDecl, *ast.FuncLit:
			return stack[i]
		}
	}
	return nil
}

// declaredIn reports whether scope node n declares name where the use (under
// child, at sc.pos) can see it, and resolves that declaration.
func (ix *routeIndex) declaredIn(n, child ast.Node, name string, fi int, sc scope, depth int) (varDecl, bool) {
	unresolved := varDecl{declared: true, file: fi, scope: sc}
	fromFields := func(lists ...*ast.FieldList) (varDecl, bool) {
		for _, fl := range lists {
			if fl == nil {
				continue
			}
			for _, field := range fl.List {
				for _, id := range field.Names {
					if id.Name == name {
						t, ok := ix.typeOfTypeExpr(field.Type, fi)
						return varDecl{typ: t, ok: ok, declared: true, file: fi, scope: sc}, true
					}
				}
			}
		}
		return varDecl{}, false
	}
	// stmts は 1 つのブロックの文。use より前に終わる文の宣言だけを見て、
	// 最後のものを採る (`a, err := f()` は同じブロックの a への代入)。
	fromStmts := func(stmts []ast.Stmt) (varDecl, bool) {
		var d varDecl
		found := false
		for _, st := range stmts {
			if st.End() > sc.pos {
				break
			}
			// 右辺は宣言の位置で引く (Go と同じ)。route の位置で引くと、宣言より後に
			// 同じブロックで隠した名前を右辺が見てしまう (レビュー 3 周目 M-1)。
			if v, ok := ix.declaredByStmt(st, name, fi, scope{stack: sc.stack, pos: st.Pos()}, depth); ok {
				d, found = v, true
			}
		}
		return d, found
	}
	switch v := n.(type) {
	case *ast.FuncDecl:
		return fromFields(v.Recv, v.Type.Params, v.Type.Results)
	case *ast.FuncLit:
		return fromFields(v.Type.Params, v.Type.Results)
	case *ast.BlockStmt:
		return fromStmts(v.List)
	case *ast.CaseClause:
		return fromStmts(v.Body)
	case *ast.CommClause:
		if d, ok := fromStmts(v.Body); ok {
			return d, true
		}
		if v.Comm != nil && child != v.Comm {
			return ix.declaredByStmt(v.Comm, name, fi, sc, depth)
		}
	case *ast.IfStmt:
		if v.Init != nil && child != v.Init {
			return ix.declaredByStmt(v.Init, name, fi, sc, depth)
		}
	case *ast.ForStmt:
		if v.Init != nil && child != v.Init {
			return ix.declaredByStmt(v.Init, name, fi, sc, depth)
		}
	case *ast.SwitchStmt:
		if v.Init != nil && child != v.Init {
			return ix.declaredByStmt(v.Init, name, fi, sc, depth)
		}
	case *ast.TypeSwitchStmt:
		if v.Init != nil && child != v.Init {
			if d, ok := ix.declaredByStmt(v.Init, name, fi, sc, depth); ok {
				return d, true
			}
		}
		// `switch h := x.(type)` の h は case ごとに型が違う。引かない。
		if as, ok := v.Assign.(*ast.AssignStmt); ok && child != v.Assign {
			for _, l := range as.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name == name {
					return unresolved, true
				}
			}
		}
	case *ast.RangeStmt:
		if v.Tok == token.DEFINE && child == v.Body {
			for _, e := range []ast.Expr{v.Key, v.Value} {
				if id, ok := e.(*ast.Ident); ok && id.Name == name {
					return unresolved, true // 要素の型は引かない
				}
			}
		}
	}
	return varDecl{}, false
}

// declaredByStmt resolves a declaration of name made by one statement
// (`:=`, `var`, or a labeled statement wrapping them).
func (ix *routeIndex) declaredByStmt(st ast.Stmt, name string, fi int, sc scope, depth int) (varDecl, bool) {
	switch s := st.(type) {
	case *ast.LabeledStmt:
		return ix.declaredByStmt(s.Stmt, name, fi, sc, depth)
	case *ast.AssignStmt:
		if s.Tok != token.DEFINE {
			return varDecl{}, false
		}
		for j, lhs := range s.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != name {
				continue
			}
			d := varDecl{declared: true, file: fi, scope: sc}
			switch {
			case len(s.Lhs) == len(s.Rhs):
				d.rhs = s.Rhs[j]
				d.typ, d.ok = ix.typeOfValue(s.Rhs[j], 0, fi, sc, depth+1)
			case len(s.Rhs) == 1:
				d.typ, d.ok = ix.typeOfValue(s.Rhs[0], j, fi, sc, depth+1)
			}
			return d, true
		}
	case *ast.DeclStmt:
		gd, ok := s.Decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			return varDecl{}, false
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for j, id := range vs.Names {
				if id.Name != name {
					continue
				}
				d := varDecl{declared: true, file: fi, scope: sc}
				switch {
				case vs.Type != nil:
					d.typ, d.ok = ix.typeOfTypeExpr(vs.Type, fi)
				case j < len(vs.Values):
					d.rhs = vs.Values[j]
					d.typ, d.ok = ix.typeOfValue(vs.Values[j], 0, fi, sc, depth+1)
				}
				return d, true
			}
		}
	}
	return varDecl{}, false
}

// conflicting reports whether function fn declares name more than once with
// types that disagree (an unresolved one counts as different). Assignments
// with `=` do not count.
// Nested function literals are separate functions and are not looked into.
func (ix *routeIndex) conflicting(fn ast.Node, name string, fi int, sc scope, depth int) bool {
	key := declKey{fn: fn, name: name}
	if c, ok := ix.conflictCache[key]; ok {
		return c
	}
	types := map[typeRef]bool{}
	unresolved := false
	add := func(t typeRef, ok bool) {
		if ok {
			types[t] = true
		} else {
			unresolved = true
		}
	}
	var ftype *ast.FuncType
	var body *ast.BlockStmt
	var recv *ast.FieldList
	switch f := fn.(type) {
	case *ast.FuncDecl:
		ftype, body, recv = f.Type, f.Body, f.Recv
	case *ast.FuncLit:
		ftype, body = f.Type, f.Body
	}
	n := 0
	for _, fl := range []*ast.FieldList{recv, ftype.Params, ftype.Results} {
		if fl == nil {
			continue
		}
		for _, field := range fl.List {
			for _, id := range field.Names {
				if id.Name == name {
					n++
					add(ix.typeOfTypeExpr(field.Type, fi))
				}
			}
		}
	}
	// 右辺は use の位置のスコープで引く。`h := h` のように自分を引く形は、
	// 深さの上限 (maxResolveDepth) で止まる。
	if body != nil {
		ast.Inspect(body, func(x ast.Node) bool {
			switch s := x.(type) {
			case *ast.FuncLit:
				return false
			case *ast.AssignStmt:
				// `=` は数えない。Go では `=` で変数の静的な型は変わらず、別の型の
				// 値を入れると compile が通らない (レビュー 3 周目 L-1)。数えると、
				// wrapper に通して入れ直すような正当な形まで落ちる。
				if s.Tok != token.DEFINE {
					return true
				}
				for j, lhs := range s.Lhs {
					id, ok := lhs.(*ast.Ident)
					if !ok || id.Name != name {
						continue
					}
					n++
					switch {
					case len(s.Lhs) == len(s.Rhs):
						add(ix.typeOfValue(s.Rhs[j], 0, fi, sc, depth+1))
					case len(s.Rhs) == 1:
						add(ix.typeOfValue(s.Rhs[0], j, fi, sc, depth+1))
					default:
						unresolved = true
					}
				}
			case *ast.DeclStmt:
				gd, ok := s.Decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.VAR {
					return true
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for j, id := range vs.Names {
						if id.Name != name {
							continue
						}
						n++
						switch {
						case vs.Type != nil:
							add(ix.typeOfTypeExpr(vs.Type, fi))
						case j < len(vs.Values):
							add(ix.typeOfValue(vs.Values[j], 0, fi, sc, depth+1))
						default:
							unresolved = true
						}
					}
				}
			case *ast.RangeStmt:
				for _, e := range []ast.Expr{s.Key, s.Value} {
					if id, ok := e.(*ast.Ident); ok && id.Name == name && s.Tok == token.DEFINE {
						n++
						unresolved = true
					}
				}
			case *ast.TypeSwitchStmt:
				if as, ok := s.Assign.(*ast.AssignStmt); ok {
					for _, l := range as.Lhs {
						if id, ok := l.(*ast.Ident); ok && id.Name == name {
							n++
							unresolved = true
						}
					}
				}
			}
			return true
		})
	}
	c := n > 1 && (len(types) > 1 || (unresolved && len(types) > 0))
	ix.conflictCache[key] = c
	return c
}

// typeOfTypeExpr resolves a type expression written in file fi.
func (ix *routeIndex) typeOfTypeExpr(e ast.Expr, fi int) (typeRef, bool) {
	switch v := e.(type) {
	case *ast.StarExpr:
		return ix.typeOfTypeExpr(v.X, fi)
	case *ast.ParenExpr:
		return ix.typeOfTypeExpr(v.X, fi)
	case *ast.IndexExpr:
		return ix.typeOfTypeExpr(v.X, fi)
	case *ast.SelectorExpr:
		if id, ok := v.X.(*ast.Ident); ok {
			if p, ok := ix.imports[fi][id.Name]; ok {
				return typeRef{pkg: p}, true
			}
		}
	case *ast.Ident:
		if _, ok := ix.types[v.Name]; ok {
			return typeRef{local: v.Name}, true
		}
	}
	return typeRef{}, false
}

// typeOfValue resolves the type of the idx-th value of expression e.
func (ix *routeIndex) typeOfValue(e ast.Expr, idx, fi int, sc scope, depth int) (typeRef, bool) {
	if depth > maxResolveDepth {
		return typeRef{}, false
	}
	switch v := e.(type) {
	case *ast.ParenExpr:
		return ix.typeOfValue(v.X, idx, fi, sc, depth+1)
	case *ast.UnaryExpr:
		if v.Op == token.AND {
			return ix.typeOfValue(v.X, idx, fi, sc, depth+1)
		}
	case *ast.CompositeLit:
		if idx == 0 && v.Type != nil {
			return ix.typeOfTypeExpr(v.Type, fi)
		}
	case *ast.CallExpr:
		return ix.typeOfCall(v, idx, fi, sc, depth+1)
	case *ast.Ident, *ast.SelectorExpr:
		if idx == 0 {
			return ix.typeOfExpr(v, fi, sc, depth+1)
		}
	}
	return typeRef{}, false
}

// typeOfCall resolves the idx-th result of a call.
func (ix *routeIndex) typeOfCall(call *ast.CallExpr, idx, fi int, sc scope, depth int) (typeRef, bool) {
	var name string
	var recv ast.Expr
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		name = fn.Name
	case *ast.SelectorExpr:
		if id, ok := fn.X.(*ast.Ident); ok {
			if p, ok := ix.imports[fi][id.Name]; ok {
				// 別のパッケージの constructor は、そのパッケージの型を返すとみなす
				// (`pkg.NewHandler(...)`)。それ以外の関数の戻り値は追わない。
				if idx == 0 && strings.HasPrefix(fn.Sel.Name, "New") {
					return typeRef{pkg: p}, true
				}
				return typeRef{}, false
			}
		}
		name, recv = fn.Sel.Name, fn.X
	default:
		return typeRef{}, false
	}
	// 同じパッケージの関数かメソッド。メソッドはレシーバの型で絞る。
	// **レシーバの型を同じパッケージの型として引けなければ、引けないとする**
	// (レビュー 3 周目 L-2)。名前だけで候補を探すと、他の package の値の
	// メソッド (`m.Handler()`) を、同じ名前のローカルのメソッドと取り違える。
	var recvType string
	if recv != nil {
		t, ok := ix.typeOfExpr(recv, fi, sc, depth+1)
		if !ok || t.local == "" {
			return typeRef{}, false
		}
		recvType = t.local
	}
	var cands []funcAt
	for _, f := range ix.funcs[name] {
		isMethod := f.decl.Recv != nil && len(f.decl.Recv.List) > 0
		if (recv != nil) != isMethod {
			continue
		}
		if isMethod && recvType != "" {
			if t, ok := ix.typeOfTypeExpr(f.decl.Recv.List[0].Type, f.file); !ok || t.local != recvType {
				continue
			}
		}
		cands = append(cands, f)
	}
	if len(cands) != 1 || cands[0].decl.Type.Results == nil {
		return typeRef{}, false
	}
	f := cands[0]
	i := 0
	for _, field := range f.decl.Type.Results.List {
		n := len(field.Names)
		if n == 0 {
			n = 1
		}
		if idx < i+n {
			return ix.typeOfTypeExpr(field.Type, f.file)
		}
		i += n
	}
	return typeRef{}, false
}

// typeOfExpr resolves the type of a value expression (a variable, a field of
// a local struct, a call).
func (ix *routeIndex) typeOfExpr(e ast.Expr, fi int, sc scope, depth int) (typeRef, bool) {
	if depth > maxResolveDepth {
		return typeRef{}, false
	}
	switch v := e.(type) {
	case *ast.ParenExpr:
		return ix.typeOfExpr(v.X, fi, sc, depth+1)
	case *ast.StarExpr:
		return ix.typeOfExpr(v.X, fi, sc, depth+1)
	case *ast.CallExpr:
		return ix.typeOfCall(v, 0, fi, sc, depth+1)
	case *ast.Ident:
		d := ix.lookupVar(v.Name, fi, sc, depth+1)
		return d.typ, d.ok
	case *ast.SelectorExpr:
		if id, ok := v.X.(*ast.Ident); ok {
			if _, ok := ix.imports[fi][id.Name]; ok {
				return typeRef{}, false // pkg.Var は追わない
			}
		}
		t, ok := ix.typeOfExpr(v.X, fi, sc, depth+1)
		if !ok || t.local == "" {
			return typeRef{}, false
		}
		return ix.fieldType(t.local, v.Sel.Name)
	}
	return typeRef{}, false
}

// fieldType resolves the type of a named field of a struct type declared in
// the package being read.
func (ix *routeIndex) fieldType(typeName, field string) (typeRef, bool) {
	ta, ok := ix.types[typeName]
	if !ok {
		return typeRef{}, false
	}
	st, ok := ta.spec.Type.(*ast.StructType)
	if !ok {
		return typeRef{}, false
	}
	for _, f := range st.Fields.List {
		for _, n := range f.Names {
			if n.Name == field {
				return ix.typeOfTypeExpr(f.Type, ta.file)
			}
		}
	}
	return typeRef{}, false
}

// serverRoutes returns collectRoutes over every non-test source of
// internal/server.
func serverRoutes(t *testing.T) []routeRef {
	t.Helper()
	var names, srcs []string
	for _, s := range serverSources(t) {
		names = append(names, s.rel)
		srcs = append(srcs, string(s.src))
	}
	_, refs, err := collectRoutes(names, srcs)
	if err != nil {
		t.Fatal(err)
	}
	return refs
}

var (
	methodSetMu    sync.Mutex
	methodSetCache = map[string]map[string]bool{}
)

// packageMethods returns the method names declared in the non-test sources of
// the package at import path p (which must be in this module).
func packageMethods(t *testing.T, p string) map[string]bool {
	t.Helper()
	methodSetMu.Lock()
	defer methodSetMu.Unlock()
	if m, ok := methodSetCache[p]; ok {
		return m
	}
	dir := filepath.Join(repoRoot(t), filepath.FromSlash(strings.TrimPrefix(p, modulePath)))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	m := map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv != nil {
				m[fd.Name.Name] = true
			}
		}
	}
	methodSetCache[p] = m
	return m
}

// apiRouteProblems returns, for every route registered on the /api group (or a
// group derived from it), why its handler cannot be checked against the
// internal/api package it belongs to. The key is "METHOD /path".
func apiRouteProblems(t *testing.T, refs []routeRef) (problems map[string]string, checked int) {
	t.Helper()
	problems = map[string]string{}
	for _, r := range refs {
		if !r.apiRoot && !r.apiDerived {
			continue
		}
		key := r.method + " " + r.path
		switch {
		case r.handler == "":
			problems[key] = fmt.Sprintf("%s: handler が `x.Method` の形ではない (closure / 関数呼び出し)", r.pos)
		case r.pkg == "" && r.local != "":
			problems[key] = fmt.Sprintf("%s: handler %s.%s が internal/server の型 %s のメソッド", r.pos, r.recv, r.handler, r.local)
		case r.pkg == "":
			problems[key] = fmt.Sprintf("%s: handler の受け手 %s の型を引けない"+
				" (同じ関数の中で、同じ名前を別の型で宣言し直していないか)", r.pos, r.recv)
		case !strings.HasPrefix(r.pkg, modulePath+"internal/api/"):
			problems[key] = fmt.Sprintf("%s: handler %s.%s が internal/api の外 (%s)", r.pos, r.recv, r.handler, r.pkg)
		case !packageMethods(t, r.pkg)[r.handler]:
			problems[key] = fmt.Sprintf("%s: %s にメソッド %s が無い (受け手 %s の型を取り違えている)", r.pos, r.pkg, r.handler, r.recv)
		default:
			checked++
		}
	}
	return problems, checked
}

// apiRouteResolveAllowlist lists /api routes whose handler is not a method of
// an internal/api package, keyed by "METHOD /path". The value says through
// which path the route is served.
//
// 今は空。足すときは「どの経路で出るか」を書く (CLAUDE.md Section 4)。
var apiRouteResolveAllowlist = map[string]string{}

// /api の route の handler が、すべて internal/api のパッケージのメソッドとして
// 解決できること (#3672 レビュー H-1 / M-2)。
//
// **error id / kind / status / limit のゲートは、handler の受け手の型から
// package を引き、そのパッケージが出す error と突き合わせる。** 引けない route は
// 黙って検査から外れ、取り違えた route は別の package と突き合わされる。どちらも
// 緑のまま起きる (レビューで実測: 引数名を変えると 37 本が外れ、`h` にすると
// pluginsecrets として解決された)。ここで、(a) 解決できないもの、(b) 解決先に
// そのメソッドが無いもの、を 0 件に保つ。
func TestAPIRouteHandlersResolve(t *testing.T) {
	refs := serverRoutes(t)
	problems, checked := apiRouteProblems(t, refs)

	// 下限: 名指しの route が notes の Create として解決されていること。
	found := false
	for _, r := range refs {
		if r.apiRoot && r.method == "POST" && r.path == "/notes/create" {
			found = true
			if r.pkg != modulePath+"internal/api/notes" || r.handler != "Create" {
				t.Errorf("/notes/create が %s.%s として解決された (internal/api/notes の Create のはず)", r.pkg, r.handler)
			}
		}
	}
	if !found {
		t.Fatal("internal/server から POST /notes/create の登録を読めない。登録の書き方が変わったか、git add を忘れていないか")
	}
	if checked < 500 {
		t.Fatalf("/api の route を %d 本しか解決できていない (500 以上あるはず)", checked)
	}

	bad, dead := allowlistDiff(problems, apiRouteResolveAllowlist)
	if len(bad) > 0 {
		t.Errorf("/api の route の handler を internal/api のメソッドとして解決できない (%d 本)。"+
			"error id などのゲートから黙って外れる:\n  %s\n"+
			"handler を `x.Method` の形で渡し、x の型を引数や変数の型で書くこと。"+
			"正当な理由で残すなら apiRouteResolveAllowlist に、どの経路で出るかを書くこと",
			len(bad), strings.Join(bad, "\n  "))
	}
	if len(dead) > 0 {
		t.Errorf("apiRouteResolveAllowlist に、もう解決できる (または存在しない) route がある。消すこと: %v", dead)
	}
}

// allowlistDiff returns the problems the allowlist does not cover ("key: why",
// sorted) and the allowlist entries no problem uses (sorted).
func allowlistDiff(problems, allow map[string]string) (bad, dead []string) {
	for key, why := range problems {
		if _, ok := allow[key]; ok {
			continue
		}
		bad = append(bad, key+": "+why)
	}
	for key := range allow {
		if _, ok := problems[key]; !ok {
			dead = append(dead, key)
		}
	}
	sort.Strings(bad)
	sort.Strings(dead)
	return bad, dead
}

// allowlist の過不足を両方向で見ること (#3672 レビュー 2 周目 L-3)。
// 本番の allowlist は空なので、使われていない項目の検査はここでしか守れない。
func TestAllowlistDiffReportsBothWays(t *testing.T) {
	bad, dead := allowlistDiff(
		map[string]string{"POST /a": "why a", "POST /b": "why b"},
		map[string]string{"POST /a": "served by x", "POST /gone": "served by y"},
	)
	if !slices.Equal(bad, []string{"POST /b: why b"}) {
		t.Errorf("bad = %v, want [POST /b: why b]", bad)
	}
	if !slices.Equal(dead, []string{"POST /gone"}) {
		t.Errorf("dead = %v, want [POST /gone]", dead)
	}
}

// fixtureRoutes runs collectRoutes over package server fixtures given as
// name -> body (imports and declarations, without the package clause).
func fixtureRoutes(t *testing.T, files map[string]string) map[string]routeRef {
	t.Helper()
	var names, srcs []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		srcs = append(srcs, "package server\n\n"+files[name])
	}
	_, refs, err := collectRoutes(names, srcs)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]routeRef{}
	for _, r := range refs {
		key := r.method + " " + r.path
		if _, dup := out[key]; dup {
			t.Fatalf("fixture に %s が 2 つある", key)
		}
		out[key] = r
	}
	return out
}

const apiPkg = modulePath + "internal/api/"

// handler の受け手を、Go のスコープと型で引くこと (#3672 レビュー H-1)。
//
// **段階 2 で普通に書く形を並べてある。** 以前の実装は `h := pkg.NewX(...)` の
// 名前の表を他のファイルへ落として引いたので、`registerUsersRoutes(api apiRoutes,
// h *users.Handler)` の `h` を、別のファイルの `h := pluginsecrets.New(...)` と
// 取り違えた (S5)。引数名を表に無い名前にすると、黙って解決されなかった (S6)。
func TestCollectRoutesResolvesHandlerTypes(t *testing.T) {
	routes := fixtureRoutes(t, map[string]string{
		"setup.go": `import (
	"github.com/elythia-network/elythia/internal/api/meta"
	"github.com/elythia-network/elythia/internal/api/notes"
)

var metaHandler = meta.NewHandler()

type apiRoutes struct{ *echo.Group }

type Server struct{}

func (s *Server) setupRoutes() {
	api := apiRoutes{s.echo.Group("/api")}
	h := notes.NewHandler(db)
	api.POST("/notes/create", h.Create)
	// 切り出した関数が handler を返す形。constructor は別のファイルにある。
	iHandler, _ := s.buildAccount()
	api.POST("/i", iHandler.Get)
	api.POST("/meta", metaHandler.Meta)
	promo := api.Group("/promo")
	promo.POST("/read", h.PromoRead)
	s.echo.GET("/tags/:tag", h.Tag)
	registerUsersRoutes(api, nil)
}
`,
		"routes_users.go": `import "github.com/elythia-network/elythia/internal/api/users"

// S5: 引数の名前が、別のファイルのローカル変数 h と同じ。
func registerUsersRoutes(api apiRoutes, h *users.Handler) {
	api.POST("/users/show", h.Show)
}
`,
		"routes_clips.go": `import "github.com/elythia-network/elythia/internal/api/clips"

// S6: group も handler も、ほかに無い名前の引数で受け取る。
func registerClipsRoutes(g apiRoutes, nh *clips.Handler) {
	g.POST("/clips/show", nh.Show)
}
`,
		"routes_struct.go": `import "github.com/elythia-network/elythia/internal/api/channels"

type routeHandlers struct {
	channels *channels.Handler
}

// S3: handler を構造体のフィールドで持つ。
func registerChannelsRoutes(api apiRoutes, h *routeHandlers) {
	api.POST("/channels/show", h.channels.Show)
}
`,
		"account.go": `import "github.com/elythia-network/elythia/internal/api/i"

func (s *Server) buildAccount() (*i.Handler, error) {
	iHandler := i.NewHandler()
	return iHandler, nil
}
`,
		"plugin_secrets.go": `import "github.com/elythia-network/elythia/internal/api/pluginsecrets"

// 引数や同じ関数のローカルは、パッケージのトップレベルの同名より優先する。
var h = pluginsecrets.New()
`,
		"routes_local.go": `// 同じ関数で宣言したが型を引けない変数は、他のファイルの同名へ落とさない。
func registerLocal(api apiRoutes) {
	h := compute()
	api.POST("/local", h.Run)
}
`,
	})

	for key, want := range map[string]struct {
		pkg, handler string
		root         bool
		derived      bool
	}{
		"POST /notes/create":  {apiPkg + "notes", "Create", true, false},
		"POST /i":             {apiPkg + "i", "Get", true, false},
		"POST /meta":          {apiPkg + "meta", "Meta", true, false},
		"POST /read":          {apiPkg + "notes", "PromoRead", false, true},
		"GET /tags/:tag":      {apiPkg + "notes", "Tag", false, false},
		"POST /users/show":    {apiPkg + "users", "Show", true, false},
		"POST /clips/show":    {apiPkg + "clips", "Show", true, false},
		"POST /channels/show": {apiPkg + "channels", "Show", true, false},
		"POST /local":         {"", "Run", true, false},
	} {
		got, ok := routes[key]
		if !ok {
			t.Errorf("%s を拾えていない", key)
			continue
		}
		if got.pkg != want.pkg || got.handler != want.handler || got.apiRoot != want.root || got.apiDerived != want.derived {
			t.Errorf("%s = {pkg:%q handler:%q root:%v derived:%v}, want {pkg:%q handler:%q root:%v derived:%v}",
				key, got.pkg, got.handler, got.apiRoot, got.apiDerived, want.pkg, want.handler, want.root, want.derived)
		}
	}
	if len(routes) != 9 {
		t.Errorf("route が %d 本 (9 本のはず)", len(routes))
	}
}

// /api の route のうち、解決できないもの・解決先にメソッドが無いもの・closure を
// 数えること (#3672 レビュー H-1 / M-2)。
func TestAPIRouteProblemsFlagsUncheckableHandlers(t *testing.T) {
	routes := fixtureRoutes(t, map[string]string{
		"setup.go": `import "github.com/elythia-network/elythia/internal/api/notes"

func (s *Server) setupRoutes(x *unknownThing) {
	api := apiRoutes{s.echo.Group("/api")}
	h := notes.NewHandler(db)
	api.POST("/ok", h.Create)
	api.POST("/missing-method", h.NoSuchMethodHere)
	api.POST("/unresolved", x.Run)
	api.POST("/closure", func(c echo.Context) error { return nil })
	api.POST("/local-type", s.OpenAPISpec)
	api.POST("/idx", hs[0].Create)
	promo := api.Group("/promo")
	promo.POST("/derived-missing", h.NoSuchMethodHere)
	s.echo.GET("/not-api", x.Run)
}
`,
	})
	refs := make([]routeRef, 0, len(routes))
	for _, r := range routes {
		refs = append(refs, r)
	}
	problems, checked := apiRouteProblems(t, refs)
	if checked != 1 {
		t.Errorf("checked = %d, want 1 (/ok だけ)", checked)
	}
	for _, key := range []string{"POST /missing-method", "POST /unresolved", "POST /closure", "POST /local-type", "POST /idx", "POST /derived-missing"} {
		if _, ok := problems[key]; !ok {
			t.Errorf("%s を問題として数えていない: %v", key, problems)
		}
	}
	if len(problems) != 6 {
		t.Errorf("問題が %d 件 (6 件のはず。/api の外の route は数えない): %v", len(problems), problems)
	}
	// 受け手が添字式でも、メッセージに受け手を出す (レビュー 2 周目 L-2)。
	if !strings.Contains(problems["POST /idx"], "hs[0]") {
		t.Errorf("POST /idx のメッセージに受け手が無い: %q", problems["POST /idx"])
	}
}

// nodeString prints an expression for diagnostics (`hs[0]`, `h.notes`).
func nodeString(e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, token.NewFileSet(), e); err != nil {
		return fmt.Sprintf("%T", e)
	}
	return b.String()
}

// ブロックのスコープと、取り違えやすい形 (#3672 レビュー 2 周目 H-1 / M-1)。
//
// **ブロックで同じ名前を宣言し直す形は、黙って取り違えない。** 同じ関数の中で
// 同じ名前を別の型で宣言していたら「引けない」に倒し、TestAPIRouteHandlersResolve
// で落とす (レビューの S7: antennas の 6 本が clips として解決され、error id の
// 検査から外れた)。名前が別なら、ブロックごとに正しく引く。
func TestCollectRoutesHandlesBlocksAndShapes(t *testing.T) {
	routes := fixtureRoutes(t, map[string]string{
		"setup.go": `import (
	"github.com/elythia-network/elythia/internal/api/antennas"
	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/clips"
	"github.com/elythia-network/elythia/internal/api/notes"
	"github.com/elythia-network/elythia/internal/api/users"
)

type apiRoutes struct{ *echo.Group }

type Server struct{}

type other struct{}

// フィールドが 2 つの構造体。フィールド名で引く。
type handlers struct {
	notes *notes.Handler
	users *users.Handler
}

// パッケージのトップレベルの pv。関数の中の同じ名前の宣言は、宣言より後でだけ
// それを隠す。
var pv = notes.NewHandler()

func (s *Server) beforeLocal(api apiRoutes) {
	api.POST("/before-local", pv.Create)
	pv := users.NewHandler()
	_ = pv
}

func (s *Server) rangeOnly(api apiRoutes, hs []*users.Handler) {
	for _, pv := range hs {
		api.POST("/range-only", pv.Show)
	}
}

func (s *Server) build() *notes.Handler { return nil }

func (o *other) build() *users.Handler { return nil }

func (s *Server) pair() (*notes.Handler, *users.Handler) { return nil, nil }

// 同じ名前を、ブロックごとに別の型で宣言し直す。どちらも引けない。
func (s *Server) shadow(api apiRoutes, ah *antennas.Handler, ch *clips.Handler) {
	{
		h := ah
		api.POST("/antennas/show", h.Show)
	}
	{
		h := ch
		api.POST("/clips/show", h.Show)
	}
}

// if の初期化、range、switch の case、内側の var で宣言し直す。どれも引けない。
func (s *Server) shadowStmts(api apiRoutes, hs []*users.Handler) {
	h := notes.NewHandler()
	if h := users.NewHandler(); h != nil {
		api.POST("/if", h.Show)
	}
	for _, h := range hs {
		api.POST("/range", h.Show)
	}
	switch {
	case true:
		var h = users.NewHandler()
		api.POST("/case", h.Show)
	}
	api.POST("/outer", h.Create)
}

// 外側で型を引けない h と、内側で引ける h。引けない。
func (s *Server) outerUnresolved(api apiRoutes) {
	h := s.mystery()
	{
		h := users.NewHandler()
		api.POST("/inner", h.Show)
	}
	_ = h
}

// 名前が別なら、ブロックごとに引ける。
func (s *Server) blocks(api apiRoutes) {
	{
		a := antennas.NewHandler()
		api.POST("/antennas/list", a.List)
	}
	{
		c := clips.NewHandler()
		api.POST("/clips/list", c.List)
	}
}

func (s *Server) shapes(api apiRoutes, hs *handlers, m *mystery) {
	// closure の中のローカルは、外の route から見えない。
	h := notes.NewHandler()
	func() {
		h := users.NewHandler()
		_ = h
	}()
	api.POST("/closure-outer", h.Create)

	// 同じ名前のメソッドが 2 つある。受け手の型で絞れれば引ける。絞れなければ引けない。
	b := s.build()
	api.POST("/build-server", b.Create)
	u := m.thing.build()
	api.POST("/build-unknown", u.Show)

	// 2 つ目の戻り値。
	_, second := s.pair()
	api.POST("/pair-second", second.Show)

	// フィールドが 2 つの構造体。
	api.POST("/field-users", hs.users.Show)

	// New で始まらない別の package の関数は、その package の型を返すとは限らない。
	w := apierr.Wrap(h)
	api.POST("/wrap", w.Create)

	// 入れ子の派生 group。
	g1 := api.Group("/a")
	g2 := g1.Group("/b")
	g2.POST("/nested", h.Create)
}
`,
	})
	want := map[string]struct {
		pkg     string
		derived bool
	}{
		"POST /antennas/show": {"", false},
		"POST /clips/show":    {"", false},
		"POST /if":            {"", false},
		"POST /range":         {"", false},
		"POST /case":          {"", false},
		"POST /outer":         {"", false},
		"POST /inner":         {"", false},
		"POST /antennas/list": {apiPkg + "antennas", false},
		"POST /clips/list":    {apiPkg + "clips", false},
		"POST /closure-outer": {apiPkg + "notes", false},
		"POST /build-server":  {apiPkg + "notes", false},
		"POST /build-unknown": {"", false},
		"POST /pair-second":   {apiPkg + "users", false},
		"POST /field-users":   {apiPkg + "users", false},
		"POST /wrap":          {"", false},
		"POST /nested":        {apiPkg + "notes", true},
		"POST /before-local":  {apiPkg + "notes", false},
		"POST /range-only":    {"", false},
	}
	for key, w := range want {
		got, ok := routes[key]
		if !ok {
			t.Errorf("%s を拾えていない", key)
			continue
		}
		if got.pkg != w.pkg || got.apiDerived != w.derived {
			t.Errorf("%s = {pkg:%q derived:%v}, want {pkg:%q derived:%v}", key, got.pkg, got.apiDerived, w.pkg, w.derived)
		}
	}
	if len(routes) != len(want) {
		t.Errorf("route が %d 本 (%d 本のはず)", len(routes), len(want))
	}
}

// スコープの規則を、1 つずつ単独で守る (#3672 レビュー 3 周目 M-1 / M-2 / L-1 / L-2)。
//
// **同じ名前のパッケージの var を外側に置き、関数の中の宣言は 1 つだけにする。**
// 食い違いの規則は関数の中の宣言しか数えないので、この形ではブロックの規則が
// 壊れると、黙ってパッケージの var (clips) に解決する。2 周目のブロックの
// fixture は、どれも食い違いの規則が代わりに落とすので、ブロックの規則を単独で
// 守れていなかった。
func TestCollectRoutesScopeRules(t *testing.T) {
	const head = `import (
	"github.com/elythia-network/elythia/internal/api/antennas"
	"github.com/elythia-network/elythia/internal/api/clips"
)

type apiRoutes struct{ *echo.Group }

type Server struct{}

`
	for name, tc := range map[string]struct {
		body string
		want map[string]string // "METHOD /path" -> import path ("" = must not resolve)
	}{
		"named result": {`var h = clips.NewHandler()

func r(api apiRoutes) (h *antennas.Handler) {
	api.POST("/antennas/show", h.Show)
	return
}`, map[string]string{"POST /antennas/show": apiPkg + "antennas"}},
		"case body": {`var h = clips.NewHandler()

func r(api apiRoutes, k int) {
	switch k {
	case 1:
		h := antennas.NewHandler()
		api.POST("/antennas/show", h.Show)
	}
}`, map[string]string{"POST /antennas/show": apiPkg + "antennas"}},
		"labeled": {`var h = clips.NewHandler()

func r(api apiRoutes) {
L:
	h := antennas.NewHandler()
	api.POST("/antennas/show", h.Show)
	goto L
}`, map[string]string{"POST /antennas/show": apiPkg + "antennas"}},
		"select case": {`var h = clips.NewHandler()

func r(api apiRoutes, c chan *antennas.Handler) {
	select {
	case h := <-c:
		api.POST("/antennas/show", h.Show)
	}
}`, map[string]string{"POST /antennas/show": ""}},
		"for init": {`var h = clips.NewHandler()

func r(api apiRoutes) {
	for h := antennas.NewHandler(); ; {
		api.POST("/antennas/show", h.Show)
	}
}`, map[string]string{"POST /antennas/show": apiPkg + "antennas"}},
		"type switch": {`var h = clips.NewHandler()

func r(api apiRoutes, x any) {
	switch h := x.(type) {
	case *antennas.Handler:
		api.POST("/antennas/show", h.Show)
	}
}`, map[string]string{"POST /antennas/show": ""}},
		"if init": {`var h = clips.NewHandler()

func r(api apiRoutes) {
	if h := antennas.NewHandler(); h != nil {
		api.POST("/antennas/show", h.Show)
	}
}`, map[string]string{"POST /antennas/show": apiPkg + "antennas"}},
		"switch init": {`var h = clips.NewHandler()

func r(api apiRoutes, k int) {
	switch h := antennas.NewHandler(); k {
	case 1:
		api.POST("/antennas/show", h.Show)
	}
}`, map[string]string{"POST /antennas/show": apiPkg + "antennas"}},
		// 右辺は宣言の位置で引く (M-1)。
		"copy then shadow (package var)": {`var h = antennas.NewHandler()

func r(api apiRoutes, ch *clips.Handler) {
	x := h
	h := ch
	_ = h
	api.POST("/antennas/show", x.Show)
}`, map[string]string{"POST /antennas/show": apiPkg + "antennas"}},
		"copy then shadow (closure)": {`func r(api apiRoutes, ah *antennas.Handler, ch *clips.Handler) {
	h := ah
	func() {
		x := h
		h := ch
		_ = h
		api.POST("/antennas/show", x.Show)
	}()
}`, map[string]string{"POST /antennas/show": apiPkg + "antennas"}},
		// "=" は食い違いに数えない (L-1)。
		"reassign through a wrapper": {`func withMetrics[T any](t T) T { return t }

func r(api apiRoutes, a int) {
	h := antennas.NewHandler(a)
	h = withMetrics(h)
	api.POST("/antennas/show", h.Show)
}`, map[string]string{"POST /antennas/show": apiPkg + "antennas"}},
		"typed var reassigned": {`func r(api apiRoutes) {
	var h *antennas.Handler = antennas.Default
	api.POST("/antennas/show", h.Show)
	h = antennas.Default2
	api.POST("/antennas/list", h.List)
}`, map[string]string{"POST /antennas/show": apiPkg + "antennas", "POST /antennas/list": apiPkg + "antennas"}},
		"for init and post": {`func r(api apiRoutes, ah *antennas.Handler) {
	for h := ah; h != nil; h = nil {
		api.POST("/antennas/show", h.Show)
	}
}`, map[string]string{"POST /antennas/show": apiPkg + "antennas"}},
		// "=" で interface の変数に別の型の値を入れても、静的な型 (interface) は
		// 引けないので、黙って取り違えない。
		"interface var reassigned": {`type shower interface{ Show() }

func r(api apiRoutes, ch *clips.Handler) {
	var h shower = antennas.NewHandler()
	h = ch
	api.POST("/antennas/show", h.Show)
}`, map[string]string{"POST /antennas/show": ""}},
		// レシーバの型が他の package の型なら、同じ名前のローカルのメソッドに
		// 当てない (L-2)。
		"external receiver method": {`type srv struct{}

func (s *srv) Handler() *antennas.Handler { return nil }

func r(api apiRoutes, m *clips.Module) {
	h := m.Handler()
	api.POST("/clips/show", h.Show)
}`, map[string]string{"POST /clips/show": ""}},
	} {
		t.Run(name, func(t *testing.T) {
			routes := fixtureRoutes(t, map[string]string{"setup.go": head + tc.body + "\n"})
			for key, want := range tc.want {
				got, ok := routes[key]
				if !ok {
					t.Fatalf("%s を拾えていない", key)
				}
				if got.pkg != want {
					t.Errorf("%s = %q, want %q", key, got.pkg, want)
				}
			}
		})
	}
}
