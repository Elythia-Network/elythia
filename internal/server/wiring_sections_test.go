package server

import (
	"go/ast"
	"go/token"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mustFindWiringSections names the sections wiringSections must find, with
// the file each lives in.
//
// **何も拾えなかったら落とす (CLAUDE.md Section 4)。** 抽出が空振りすると、
// 「同じ名前の変数を渡す」も「書き換えない」も、見る対象が無いまま緑になる。
var mustFindWiringSections = map[string]string{
	"wireCacheInvalidation": "wiring_cache_invalidation.go",
	"wireFrontendRoutes":    "wiring_frontend_routes.go",
	"wireAdmin":             "wiring_admin.go",
	"wireStreaming":         "wiring_streaming.go",
	"wireFederationRoutes":  "wiring_federation_routes.go",
	"wireAccounts":          "wiring_accounts.go",
	"wireBackgroundJobs":    "wiring_background_jobs.go",
}

// wiringSectionName matches the name of a *Server method that holds one part
// of setupRoutes (#3901).
var wiringSectionName = regexp.MustCompile(`^wire[A-Z][A-Za-z]*$`)

// wiringSection is one part of setupRoutes moved into its own method (#3901).
type wiringSection struct {
	file   string
	decl   *ast.FuncDecl
	deps   string   // name of the parameter's struct type
	fields []string // the struct's field names, in order
}

// wiringSections returns the *Server methods of the package named wire…,
// keyed by method name. Each must take a single parameter `d` of the struct
// type named after it (wireCacheInvalidation takes cacheInvalidationDeps).
func wiringSections(t *testing.T, p parsedPackage) map[string]wiringSection {
	t.Helper()
	structs := map[string][]string{}
	for _, name := range p.names {
		for _, decl := range p.files[name].Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				var fields []string
				for _, f := range st.Fields.List {
					for _, n := range f.Names {
						fields = append(fields, n.Name)
					}
					if len(f.Names) == 0 {
						fields = append(fields, "")
					}
				}
				structs[ts.Name.Name] = fields
			}
		}
	}
	out := map[string]wiringSection{}
	for _, name := range p.names {
		for _, decl := range p.files[name].Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || !wiringSectionName.MatchString(fd.Name.Name) {
				continue
			}
			rest := strings.TrimPrefix(fd.Name.Name, "wire")
			deps := strings.ToLower(rest[:1]) + rest[1:] + "Deps"
			params := fd.Type.Params.List
			ok = len(params) == 1 && len(params[0].Names) == 1 && params[0].Names[0].Name == "d"
			if ok {
				id, isIdent := params[0].Type.(*ast.Ident)
				ok = isIdent && id.Name == deps
			}
			if !assert.Truef(t, ok, "%s の %s は、引数を `d %s` の 1 つだけにすること", name, fd.Name.Name, deps) {
				continue
			}
			fields, isStruct := structs[deps]
			if !assert.Truef(t, isStruct, "%s が構造体でない", deps) {
				continue
			}
			for _, f := range fields {
				require.NotEmptyf(t, f, "%s に名前の無いフィールドがある", deps)
			}
			_, dup := out[fd.Name.Name]
			require.Falsef(t, dup, "%s が 2 つある", fd.Name.Name)
			out[fd.Name.Name] = wiringSection{file: name, decl: fd, deps: deps, fields: fields}
		}
	}
	for fn, file := range mustFindWiringSections {
		sec, ok := out[fn]
		if assert.Truef(t, ok, "%s が見つからない (rename した?)", fn) {
			assert.Equalf(t, file, sec.file, "%s のファイル", fn)
		}
	}
	return out
}

// **setupRoutesから切り出した節は、setupRoutesの同じ名前の変数を受け取り、
// 同じ名前で使うこと (#3901)。**
//
// 節の中の文は、setupRoutesにあったときの文をそのまま移したもので、
// entitycompatのassertWiredなどは、その文を変数の名前まで含めて照合する。
// その名前がsetupRoutesのどの値を指すかは見ないので、次を見て名前を揃える。
//
//   - 節(名前がwire…の*Serverのメソッド。引数はd 1つで、型はwireXに対してxDeps)の本文は、Depsのすべてのフィールドを
//     `f := d.f`で同じ名前に受け直す文で、フィールドの順に始まる。dはほかで使わない
//   - setupRoutesは、節ごとに本文の直下で1回だけ呼ぶ(パッケージのほかの場所では
//     呼ばない)。呼ぶ位置が文を移す前の位置で、中の文の順も元のまま
//   - 渡すDepsの値は、すべてのフィールドを`f: f`の形で1回ずつ書いた複合リテラル。
//     書き忘れたフィールドはゼロ値(nil)で黙って渡り、別の名前の変数を渡すと、
//     照合する文の名前と指す値がずれる
//   - 渡す変数は、setupRoutesの引数か、本文の直下で:=か値付きのvarで1回だけ定義した名前。
//     呼び出しより後と、関数リテラルの中で書き換えず、アドレスも取らない。
//     節は呼び出しの時点の値を受け取るので、後で書き換えると、文がsetupRoutesに
//     あったときと違う値を指す
//   - 節が値を返すときは、最後の文が`return a, b`で、setupRoutesは同じ名前の
//     変数に`a, b := s.wireX(...)`で受ける
func TestSetupRoutesSectionsReceiveSameNamedVariables(t *testing.T) {
	p := parsePackage(t)
	secs := wiringSections(t, p)
	_, fd := setupRoutesDecl(t)
	body := fd.Body
	defined, rewrites := setupRoutesNames(t, body)
	params := map[string]bool{}
	for _, f := range fd.Type.Params.List {
		for _, n := range f.Names {
			params[n.Name] = true
		}
	}

	// 本文の直下の呼び出し (式文か、:= の右辺)。
	type topCall struct {
		stmt int
		call *ast.CallExpr
		lhs  []ast.Expr
	}
	top := map[string][]topCall{}
	for i, st := range body.List {
		var call *ast.CallExpr
		var lhs []ast.Expr
		switch v := st.(type) {
		case *ast.ExprStmt:
			call, _ = v.X.(*ast.CallExpr)
		case *ast.AssignStmt:
			if v.Tok == token.DEFINE && len(v.Rhs) == 1 {
				call, _ = v.Rhs[0].(*ast.CallExpr)
				lhs = v.Lhs
			}
		}
		if call == nil {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if recv, ok := sel.X.(*ast.Ident); !ok || recv.Name != "s" {
			continue
		}
		if _, isSec := secs[sel.Sel.Name]; isSec {
			top[sel.Sel.Name] = append(top[sel.Sel.Name], topCall{stmt: i, call: call, lhs: lhs})
		}
	}
	// パッケージ全体での呼び出し (関数リテラルの中も、ほかの関数も数える)。
	all := map[string]int{}
	for _, name := range p.names {
		ast.Inspect(p.files[name], func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if _, isSec := secs[sel.Sel.Name]; isSec {
					all[sel.Sel.Name]++
				}
			}
			return true
		})
	}

	var names []string
	for n := range secs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, fn := range names {
		sec := secs[fn]
		require.NotEmptyf(t, sec.fields, "%s に 1 つもフィールドが無い", sec.deps)

		// 節の本文: フィールドの順に f := d.f で始まり、d をほかで使わない。
		stmts := sec.decl.Body.List
		if assert.GreaterOrEqualf(t, len(stmts), len(sec.fields), "%s の本文が短すぎる", fn) {
			for i, f := range sec.fields {
				as, ok := stmts[i].(*ast.AssignStmt)
				ok = ok && as.Tok == token.DEFINE && len(as.Lhs) == 1 && len(as.Rhs) == 1
				if ok {
					l, lok := as.Lhs[0].(*ast.Ident)
					r, rok := as.Rhs[0].(*ast.SelectorExpr)
					var rx *ast.Ident
					if rok {
						rx, rok = r.X.(*ast.Ident)
					}
					ok = lok && rok && l.Name == f && rx.Name == "d" && r.Sel.Name == f
				}
				assert.Truef(t, ok, "%s の %d 番目の文が `%s := d.%s` でない。フィールドの順に同じ名前で受け直すこと", fn, i+1, f, f)
			}
		}
		dUses := 0
		ast.Inspect(sec.decl.Body, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "d" {
				dUses++
			}
			return true
		})
		assert.Equalf(t, len(sec.fields), dUses, "%s が d をフィールドの受け直し以外でも使っている (受け直しは %d 個)", fn, len(sec.fields))

		// setupRoutes からの呼び出し。
		if !assert.Lenf(t, top[fn], 1, "setupRoutes の本文の直下で s.%s を 1 回だけ呼ぶ", fn) {
			continue
		}
		assert.Equalf(t, 1, all[fn], "%s を setupRoutes の本文の直下以外 (関数リテラル・if の中、ほかの関数) でも参照している", fn)
		tc := top[fn][0]
		require.Lenf(t, tc.call.Args, 1, "s.%s に渡す引数の数", fn)
		lit, ok := tc.call.Args[0].(*ast.CompositeLit)
		if !assert.Truef(t, ok, "s.%s に %s の複合リテラルをその場で渡していない", fn, sec.deps) {
			continue
		}
		if typ, ok := lit.Type.(*ast.Ident); !assert.Truef(t, ok && typ.Name == sec.deps, "s.%s に渡すリテラルの型が %s でない", fn, sec.deps) {
			continue
		}
		seen := map[string]int{}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !assert.Truef(t, ok, "%s のリテラルにキーの無い要素がある", sec.deps) {
				continue
			}
			k, _ := kv.Key.(*ast.Ident)
			v, vok := kv.Value.(*ast.Ident)
			if k == nil {
				continue
			}
			seen[k.Name]++
			if !assert.Truef(t, vok && v.Name == k.Name, "%s のフィールド %s に、同じ名前の変数でない値を渡している", sec.deps, k.Name) {
				continue
			}
			assert.Truef(t, params[v.Name] || defined[v.Name] == 1,
				"%s に渡す %s は、setupRoutes の引数か、本文の直下で := か値付きの var で 1 回だけ定義した名前であること", sec.deps, v.Name)
			for _, rw := range rewrites[v.Name] {
				if rw.inLit || rw.stmt >= tc.stmt {
					t.Errorf("%s に渡す %s を、setupRoutes の中で呼び出しより後か関数リテラルの中で書き換えている (%s)。節が受け取る値が、文が setupRoutes にあったときと違いうる", sec.deps, v.Name, rw.tok)
				}
			}
		}
		for _, f := range sec.fields {
			assert.Equalf(t, 1, seen[f], "%s のリテラルでフィールド %s を 1 回だけ書くこと (書き忘れるとゼロ値が黙って渡る)", sec.deps, f)
		}

		// 戻り値: 最後の文の return a, b と、setupRoutes の a, b := 。
		var results []string
		if sec.decl.Type.Results != nil && sec.decl.Type.Results.NumFields() > 0 {
			last, ok := stmts[len(stmts)-1].(*ast.ReturnStmt)
			if !assert.Truef(t, ok, "%s は値を返すのに最後の文が return でない", fn) {
				continue
			}
			for _, r := range last.Results {
				id, ok := r.(*ast.Ident)
				if !assert.Truef(t, ok, "%s の return に識別子でない値がある", fn) {
					continue
				}
				results = append(results, id.Name)
			}
		}
		var lhs []string
		for _, l := range tc.lhs {
			if id, ok := l.(*ast.Ident); ok {
				lhs = append(lhs, id.Name)
			}
		}
		assert.Equalf(t, results, lhs, "setupRoutes が %s の戻り値を受ける名前が、%s の return の名前と違う", fn, fn)
	}
}

// assembledSetupRoutes returns setupRoutes with the call of each section
// replaced by the statements of the section's body, without the statements
// that bind the fields of d and without the final return (#3901).
//
// 節は setupRoutes の範囲をそのまま移したもので、受け渡しは同じ名前の変数で
// 行う (TestSetupRoutesSectionsReceiveSameNamedVariables)。なので展開した本文は、
// 移す前の 1 つの関数と同じ文を同じ順に持つ。setupRoutes の本文の直下の文を
// 見るテストは、この本文を読めば、文が節へ移っても同じ検査になる。
//
// **文の位置は、ファイルをまたぐと行で比べられない。** 順序は本文の添字で比べる。
func assembledSetupRoutes(t *testing.T) (*token.FileSet, *ast.FuncDecl) {
	t.Helper()
	p := parsePackage(t)
	secs := wiringSections(t, p)
	var fd *ast.FuncDecl
	for _, name := range p.names {
		for _, decl := range p.files[name].Decls {
			if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == "setupRoutes" && d.Recv != nil {
				require.Nil(t, fd, "(*Server).setupRoutes が 2 つある")
				fd = d
			}
		}
	}
	require.NotNil(t, fd, "internal/server に (*Server).setupRoutes が無い (rename した?)")
	sectionOf := func(st ast.Stmt) (wiringSection, bool) {
		var call *ast.CallExpr
		switch v := st.(type) {
		case *ast.ExprStmt:
			call, _ = v.X.(*ast.CallExpr)
		case *ast.AssignStmt:
			if v.Tok == token.DEFINE && len(v.Rhs) == 1 {
				call, _ = v.Rhs[0].(*ast.CallExpr)
			}
		}
		if call == nil {
			return wiringSection{}, false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return wiringSection{}, false
		}
		if recv, ok := sel.X.(*ast.Ident); !ok || recv.Name != "s" {
			return wiringSection{}, false
		}
		sec, ok := secs[sel.Sel.Name]
		return sec, ok
	}
	var list []ast.Stmt
	inlined := 0
	for _, st := range fd.Body.List {
		sec, ok := sectionOf(st)
		if !ok {
			list = append(list, st)
			continue
		}
		body := sec.decl.Body.List
		require.GreaterOrEqualf(t, len(body), len(sec.fields), "%s の本文が短すぎる", sec.decl.Name.Name)
		body = body[len(sec.fields):]
		if sec.decl.Type.Results != nil && sec.decl.Type.Results.NumFields() > 0 {
			require.NotEmptyf(t, body, "%s の本文が空", sec.decl.Name.Name)
			_, isReturn := body[len(body)-1].(*ast.ReturnStmt)
			require.Truef(t, isReturn, "%s は値を返すのに最後の文が return でない", sec.decl.Name.Name)
			body = body[:len(body)-1]
		}
		list = append(list, body...)
		inlined++
	}
	require.Equal(t, len(secs), inlined, "setupRoutes が本文の直下で呼んでいない節がある")
	out := *fd
	out.Body = &ast.BlockStmt{Lbrace: fd.Body.Lbrace, List: list, Rbrace: fd.Body.Rbrace}
	return p.fset, &out
}
