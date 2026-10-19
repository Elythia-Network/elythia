package entitycompat

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path"
	"strings"
	"testing"
)

// assertRouteRegisteredBy fails unless route is registered by the function fn
// declared in file (a routes_<area>.go of internal/server), and setupRoutes
// calls fn once as a statement of its body, passing for each parameter the
// setupRoutes variable of the same name.
//
// **ルートの行の文字列だけを照合しない (#3697)。** /api のルートは領域ごとの
// register…Routes の中にあり、行に出る `adminHandler` や `roleService` は関数の
// 引数の名前になった。行だけを見ると、setupRoutes が別の値を渡しても (別名を
// 作って渡す、引数の名前を変えて別の変数を受ける) 緑のまま通る。なので
// 「どのファイルのどの関数にあるか」と「setupRoutes がその関数に、引数と同じ
// 名前の変数を渡しているか」まで照合する。setupRoutes の変数を書き換えていない
// ことは、internal/server の TestSetupRoutesCallsEveryRouteRegistrarOnce が見る。
func assertRouteRegisteredBy(t *testing.T, file, fn, route, symptom string) {
	t.Helper()
	assertWired(t, path.Join(serverPkg, file), route, symptom)
	fset, files := parseServerFiles(t, 0)
	if err := routeRegisteredBy(fset, files, path.Join(serverPkg, file), fn, route); err != nil {
		t.Errorf("%v\n%s", err, symptom)
	}
}

// routeRegisteredBy reports why route is not registered by fn in file, with
// setupRoutes passing fn the variables of its parameter names, or nil.
func routeRegisteredBy(fset *token.FileSet, files []*ast.File, file, fn, route string) error {
	want := normalizeWiring(route)
	var decl *ast.FuncDecl
	var setup *ast.FuncDecl
	for _, f := range files {
		name := fset.Position(f.Pos()).Filename
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fd.Name.Name == "setupRoutes" && fd.Recv != nil {
				setup = fd
			}
			if fd.Name.Name == fn && fd.Recv == nil {
				if name != file {
					return fmt.Errorf("%s は %s ではなく %s にある", fn, file, name)
				}
				decl = fd
			}
		}
	}
	if decl == nil {
		return fmt.Errorf("%s に %s が無い (rename した?)", file, fn)
	}
	if setup == nil {
		return fmt.Errorf("internal/server に setupRoutes が無い (rename した?)")
	}
	found := false
	for _, st := range decl.Body.List {
		if normalizeWiring(renderNode(st)) == want {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("%s の本文の直下に %q が無い", fn, route)
	}
	var params []string
	for _, field := range decl.Type.Params.List {
		for _, n := range field.Names {
			params = append(params, n.Name)
		}
	}
	var calls []*ast.CallExpr
	for _, st := range setup.Body.List {
		es, ok := st.(*ast.ExprStmt)
		if !ok {
			continue
		}
		if call, ok := es.X.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == fn {
				calls = append(calls, call)
			}
		}
	}
	if len(calls) != 1 {
		return fmt.Errorf("setupRoutes の本文の直下で %s を 1 回だけ呼んでいない (%d 回)", fn, len(calls))
	}
	if len(calls[0].Args) != len(params) {
		return fmt.Errorf("setupRoutes が %s に渡す引数の数が %d (引数は %d)", fn, len(calls[0].Args), len(params))
	}
	for i, a := range calls[0].Args {
		id, ok := a.(*ast.Ident)
		if !ok || id.Name != params[i] {
			return fmt.Errorf("setupRoutes が %s の引数 %s に %s を渡している (同じ名前の変数を渡すこと)", fn, params[i], renderNode(a))
		}
	}
	return nil
}

// renderNode prints n on one line, without the source's line breaks.
func renderNode(n ast.Node) string {
	var buf bytes.Buffer
	// collectWiringNodes と同じく、無関係な FileSet を渡して 1 行の正規形で出す。
	if err := printer.Fprint(&buf, token.NewFileSet(), n); err != nil {
		return fmt.Sprintf("<%v>", err)
	}
	return buf.String()
}

// 照合の関数が、ファイル・関数・呼び出し・引数の名前の取り違えを落とすこと。
func TestRouteRegisteredByRejectsMismatches(t *testing.T) {
	const registrar = `package server

func registerXRoutes(api apiRoutes, xHandler *x.Handler, roleService *role.Service) {
	api.POST("/x", xHandler.Do, middleware.RequireModerator(roleService))
}
`
	const otherRegistrar = `package server

func registerYRoutes(api apiRoutes, xHandler *x.Handler, roleService *role.Service) {
	api.POST("/x", xHandler.Do, middleware.RequireModerator(roleService))
}
`
	setup := func(body string) string {
		return "package server\n\nfunc (s *Server) setupRoutes() {\n" + body + "\n}\n"
	}
	const route = `api.POST("/x", xHandler.Do, middleware.RequireModerator(roleService))`
	parse := func(srcs map[string]string) (*token.FileSet, []*ast.File) {
		fset := token.NewFileSet()
		var files []*ast.File
		for _, name := range []string{"internal/server/router.go", "internal/server/routes_x.go", "internal/server/routes_y.go"} {
			src, ok := srcs[name]
			if !ok {
				continue
			}
			f, err := parser.ParseFile(fset, name, src, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			files = append(files, f)
		}
		return fset, files
	}
	ok := setup("\txHandler := x.New()\n\troleService := role.New()\n\tregisterXRoutes(api, xHandler, roleService)")
	for _, tc := range []struct {
		name    string
		srcs    map[string]string
		fn      string
		wantErr string
	}{
		{"ok", map[string]string{"internal/server/router.go": ok, "internal/server/routes_x.go": registrar}, "registerXRoutes", ""},
		{"another variable passed", map[string]string{
			"internal/server/router.go":   setup("\th2 := xHandler\n\tregisterXRoutes(api, h2, roleService)"),
			"internal/server/routes_x.go": registrar}, "registerXRoutes", "xHandler に h2"},
		{"expression passed", map[string]string{
			"internal/server/router.go":   setup("\tregisterXRoutes(api, xHandler, s.roles)"),
			"internal/server/routes_x.go": registrar}, "registerXRoutes", "roleService に s.roles"},
		{"called inside an if", map[string]string{
			"internal/server/router.go":   setup("\tif s != nil {\n\t\tregisterXRoutes(api, xHandler, roleService)\n\t}"),
			"internal/server/routes_x.go": registrar}, "registerXRoutes", "1 回だけ呼んでいない (0 回)"},
		{"called twice", map[string]string{
			"internal/server/router.go":   setup("\tregisterXRoutes(api, xHandler, roleService)\n\tregisterXRoutes(api, xHandler, roleService)"),
			"internal/server/routes_x.go": registrar}, "registerXRoutes", "(2 回)"},
		{"route in another registrar", map[string]string{
			"internal/server/router.go":   ok,
			"internal/server/routes_x.go": "package server\n\nfunc registerXRoutes(api apiRoutes, xHandler *x.Handler, roleService *role.Service) {}\n",
			"internal/server/routes_y.go": otherRegistrar}, "registerXRoutes", "本文の直下に"},
		{"registrar in another file", map[string]string{
			"internal/server/router.go":   ok,
			"internal/server/routes_y.go": registrar}, "registerXRoutes", "ではなく internal/server/routes_y.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset, files := parse(tc.srcs)
			err := routeRegisteredBy(fset, files, "internal/server/routes_x.go", tc.fn, route)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}
