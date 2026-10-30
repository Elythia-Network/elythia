package server

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// routeRegistrarName matches the name of a function that registers the /api
// routes of one area (#3697).
var routeRegistrarName = regexp.MustCompile(`^register[A-Z][A-Za-z]*Routes$`)

// mustFindRouteRegistrars names registrars routeRegistrars must find.
//
// **何も拾えなかったら落とす (CLAUDE.md Section 4)。** 抽出が空振りすると、
// 「setupRoutesが1回ずつ呼ぶ」も「引数が同じ名前」も、見る対象が無いまま
// 緑になる。件数の多い領域と、引数の多い領域を名指しする。
var mustFindRouteRegistrars = map[string]string{
	"registerNotesRoutes": "routes_notes.go",
	"registerAdminRoutes": "routes_admin.go",
	"registerIRoutes":     "routes_i.go",
	"registerChatRoutes":  "routes_chat.go",
}

// routeRegistrar is one register…Routes function of this package.
type routeRegistrar struct {
	file   string
	decl   *ast.FuncDecl
	params []string // parameter names, api first
}

// parsedPackage is every git-tracked non-test file of this package, parsed
// into one FileSet.
type parsedPackage struct {
	fset  *token.FileSet
	files map[string]*ast.File
	names []string
}

func parsePackage(t *testing.T) parsedPackage {
	t.Helper()
	p := parsedPackage{fset: token.NewFileSet(), files: map[string]*ast.File{}}
	for _, src := range packageSources(t) {
		f, err := parser.ParseFile(p.fset, src.name, src.src, 0)
		require.NoError(t, err)
		p.files[src.name] = f
		p.names = append(p.names, src.name)
	}
	return p
}

// routeRegistrars returns the functions of the package whose first parameter
// is an apiRoutes, keyed by name. It fails the test on one that is not a
// plain function named register…Routes in a routes_<area>.go file.
func routeRegistrars(t *testing.T, p parsedPackage) map[string]routeRegistrar {
	t.Helper()
	out := map[string]routeRegistrar{}
	for _, name := range p.names {
		for _, decl := range p.files[name].Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Type.Params == nil || len(fd.Type.Params.List) == 0 {
				continue
			}
			first := fd.Type.Params.List[0]
			if id, ok := first.Type.(*ast.Ident); !ok || id.Name != "apiRoutes" {
				// プラグインのregisterPluginSecretRoutesのような、*Serverのメソッドで
				// *echo.Groupを受けるものは/apiのgroupの登録の関数ではない。
				if fd.Recv == nil && routeRegistrarName.MatchString(fd.Name.Name) {
					t.Errorf("%s の %s は最初の引数が apiRoutes でない", name, fd.Name.Name)
				}
				continue
			}
			// apiRoutesを受ける関数は、登録の関数の形に限る。
			if fd.Recv != nil {
				t.Errorf("%s の %s はメソッド。ルートの登録の関数は *Server を持たない普通の関数にする", name, fd.Name.Name)
				continue
			}
			if !routeRegistrarName.MatchString(fd.Name.Name) {
				t.Errorf("%s の %s は apiRoutes を受けるのに register…Routes の名前でない", name, fd.Name.Name)
				continue
			}
			if !strings.HasPrefix(name, "routes_") {
				t.Errorf("%s は routes_<領域>.go ではない %s にある", fd.Name.Name, name)
			}
			r := routeRegistrar{file: name, decl: fd}
			for _, field := range fd.Type.Params.List {
				require.NotEmptyf(t, field.Names, "%s に名前の無い引数がある", fd.Name.Name)
				for _, n := range field.Names {
					r.params = append(r.params, n.Name)
				}
			}
			require.Equalf(t, "api", r.params[0], "%s の最初の引数の名前が api でない", fd.Name.Name)
			_, dup := out[fd.Name.Name]
			require.Falsef(t, dup, "%s が 2 つある", fd.Name.Name)
			out[fd.Name.Name] = r
		}
	}
	for fn, file := range mustFindRouteRegistrars {
		r, ok := out[fn]
		if assert.Truef(t, ok, "%s が見つからない (rename した?)", fn) {
			assert.Equalf(t, file, r.file, "%s のファイル", fn)
		}
	}
	require.GreaterOrEqual(t, len(out), 20, "登録の関数が少なすぎる。抽出が壊れていないか")
	return out
}

// **setupRoutesは、ルートの登録の関数を本文の直下で1回ずつ呼び、引数には
// 同じ名前の変数をそのまま渡すこと (#3697)。**
//
// /apiのルートは領域ごとのregister…Routesが登録する。ルートの行の文字列は
// 移す前と同じで、handlerやmiddlewareの依存は関数の引数の名前で書いてある。
// ルートの行を文字列で照合するゲート(entitycompatのassertWiredなど)は、その
// 名前がsetupRoutesのどの値を指すかまでは見ない。なので次を見て、引数の名前と
// setupRoutesの変数の名前を一致させる。
//
//   - 登録の関数ごとに、setupRoutesの本文の直下の式文としての呼び出しが1つ
//     だけあり、パッケージのほかの場所(関数リテラル・ifの中、ほかの関数)から
//     呼んでいない。条件の中で呼ぶと、そのルートが登録されない経路ができる
//   - 呼び出しは、すべてのapi.Useより後、setupRoutesのどのreturnよりも前、
//     setupPluginsより前にある。echoのgroupはUseより前に登録したルートに
//     middlewareを付けない。returnの後に置くと、プラグインの準備に失敗した
//     経路で/apiのルートが無いまま止まる(元の登録はすべてreturnより前だった)
//   - 引数は、関数の引数と同じ名前の識別子。setupRoutesの本文の直下で:=か
//     値付きのvarで1回だけ定義し、どこでも(関数リテラルの中を含めて)=などで書き換えず、
//     アドレスも取らない。書き換えると、登録が呼び出しの時点の値を受け取る
//     ので、元の位置で登録していたときと違う値を指しうる
func TestSetupRoutesCallsEveryRouteRegistrarOnce(t *testing.T) {
	p := parsePackage(t)
	regs := routeRegistrars(t, p)
	_, fd := setupRoutesDecl(t)
	body := fd.Body
	fset := p.fset

	// setupRoutesを読み直したFileSetは別なので、本文の文の位置は行で比べず、
	// 本文の直下の文の添字で比べる。
	stmtIndex := func(match func(ast.Node) bool) []int {
		var idx []int
		for i, st := range body.List {
			found := false
			ast.Inspect(st, func(n ast.Node) bool {
				if found {
					return false
				}
				if _, ok := n.(*ast.FuncLit); ok {
					return false
				}
				if match(n) {
					found = true
					return false
				}
				return true
			})
			if found {
				idx = append(idx, i)
			}
		}
		return idx
	}
	isSelCall := func(n ast.Node, recv, method string) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != method {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return recv == "" || (ok && id.Name == recv)
	}
	useIdx := stmtIndex(func(n ast.Node) bool { return isSelCall(n, "api", "Use") })
	returnIdx := stmtIndex(func(n ast.Node) bool { _, ok := n.(*ast.ReturnStmt); return ok })
	pluginIdx := stmtIndex(func(n ast.Node) bool { return isSelCall(n, "", "setupPlugins") })
	require.GreaterOrEqual(t, len(useIdx), 5, "setupRoutes の api.Use を拾えていない")
	require.NotEmpty(t, returnIdx, "setupRoutes の return を 1 つも拾えていない")
	require.Len(t, pluginIdx, 1, "setupRoutes に setupPlugins の呼び出しが無い (rename した?)")
	lastUse := useIdx[len(useIdx)-1]

	defined, rewrites := setupRoutesNames(t, body)
	rewritten := map[string]string{}
	for name, rs := range rewrites {
		rewritten[name] = rs[0].tok
	}

	// 本文の直下の式文としての呼び出し。
	top := map[string][]int{}
	topCall := map[string]*ast.CallExpr{}
	for i, st := range body.List {
		es, ok := st.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := es.X.(*ast.CallExpr)
		if !ok {
			continue
		}
		if id, ok := call.Fun.(*ast.Ident); ok {
			if _, isReg := regs[id.Name]; isReg {
				top[id.Name] = append(top[id.Name], i)
				topCall[id.Name] = call
			}
		}
	}
	// パッケージ全体での呼び出し (関数リテラルの中も、ほかの関数も数える)。
	all := map[string]int{}
	for _, name := range p.names {
		ast.Inspect(p.files[name], func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok {
				if _, isReg := regs[id.Name]; isReg {
					all[id.Name]++
				}
			}
			return true
		})
	}

	var names []string
	for n := range regs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, fn := range names {
		r := regs[fn]
		if !assert.Lenf(t, top[fn], 1, "setupRoutes の本文の直下で %s を 1 回だけ呼ぶ", fn) {
			continue
		}
		assert.Equalf(t, 1, all[fn], "%s を setupRoutes の本文の直下以外 (関数リテラル・if の中、ほかの関数) でも呼んでいる", fn)
		at := top[fn][0]
		assert.Greaterf(t, at, lastUse, "%s の呼び出しが api.Use より前にある。Use より前に登録したルートには group の middleware が付かない", fn)
		for _, ri := range returnIdx {
			assert.Lessf(t, at, ri, "%s の呼び出しが setupRoutes の return より後にある", fn)
		}
		assert.Lessf(t, at, pluginIdx[0], "%s の呼び出しが setupPlugins より後にある", fn)

		call := topCall[fn]
		if !assert.Lenf(t, call.Args, len(r.params), "%s に渡す引数の数", fn) {
			continue
		}
		for i, arg := range call.Args {
			id, ok := arg.(*ast.Ident)
			if !assert.Truef(t, ok, "%s の %d 番目の引数 (%s) に式を渡している。setupRoutes の同じ名前の変数を渡すこと (%s)",
				fn, i+1, r.params[i], fset.Position(arg.Pos())) {
				continue
			}
			assert.Equalf(t, r.params[i], id.Name,
				"%s の引数 %s に、別の名前の変数 %s を渡している。ルートの行を照合するゲートは引数の名前で書いた行を見るので、名前を揃えること",
				fn, r.params[i], id.Name)
			assert.Equalf(t, 1, defined[id.Name],
				"%s に渡す %s は、setupRoutes の本文の直下で := か値付きの var で 1 回だけ定義した名前であること", fn, id.Name)
			if tok, ok := rewritten[id.Name]; ok {
				t.Errorf("%s に渡す %s を、setupRoutes の中で書き換えている (%s)。登録が受け取る値が、元の位置で登録していたときと違いうる", fn, id.Name, tok)
			}
		}
	}
}

// setupRoutesRewrite is one place in setupRoutes that changes a variable
// after it was defined, or takes its address.
type setupRoutesRewrite struct {
	stmt  int    // index of the top-level statement of setupRoutes that holds it
	inLit bool   // inside a function literal (may run at any later time)
	tok   string // "=", "+=", "++", "&", "range =" ...
}

// setupRoutesNames returns how many times each name is defined with := or a
// var with a value by a top-level statement of the setupRoutes body, and every place that rewrites
// a name or takes its address (anywhere in the body, function literals
// included).
func setupRoutesNames(t *testing.T, body *ast.BlockStmt) (map[string]int, map[string][]setupRoutesRewrite) {
	t.Helper()
	defined := map[string]int{}
	for _, st := range body.List {
		switch v := st.(type) {
		case *ast.AssignStmt:
			if v.Tok != token.DEFINE {
				continue
			}
			for _, l := range v.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name != "_" {
					defined[id.Name]++
				}
			}
		case *ast.DeclStmt:
			// `var metaRepo repository.MetaRepository = cachedMeta` のように、
			// interface の型で持つ名前は var で定義している。値の無い var はゼロ値
			// (nil) の宣言なので、定義に数えない。
			gd, ok := v.Decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				if len(vs.Values) == 0 {
					continue
				}
				for _, id := range vs.Names {
					if id.Name != "_" {
						defined[id.Name]++
					}
				}
			}
		}
	}
	require.NotEmpty(t, defined, "setupRoutes の := を 1 つも拾えていない")
	rewrites := map[string][]setupRoutesRewrite{}
	for i, st := range body.List {
		var walk func(n ast.Node, inLit bool)
		walk = func(n ast.Node, inLit bool) {
			ast.Inspect(n, func(n ast.Node) bool {
				add := func(e ast.Expr, tok string) {
					if id, ok := e.(*ast.Ident); ok {
						rewrites[id.Name] = append(rewrites[id.Name], setupRoutesRewrite{stmt: i, inLit: inLit, tok: tok})
					}
				}
				switch v := n.(type) {
				case *ast.FuncLit:
					if !inLit {
						walk(v.Body, true)
						return false
					}
				case *ast.AssignStmt:
					if v.Tok != token.DEFINE {
						for _, l := range v.Lhs {
							add(l, v.Tok.String())
						}
					}
				case *ast.IncDecStmt:
					add(v.X, v.Tok.String())
				case *ast.UnaryExpr:
					if v.Op == token.AND {
						add(v.X, "&")
					}
				case *ast.RangeStmt:
					if v.Tok == token.ASSIGN {
						add(v.Key, "range =")
						add(v.Value, "range =")
					}
				}
				return true
			})
		}
		walk(st, false)
	}
	return defined, rewrites
}

// routeMethodsOnAPI are the apiRoutes methods a registrar may call.
var routeMethodsOnAPI = map[string]bool{"POST": true, "GET": true, "Match": true}

// **登録の関数はルートを登録するだけにし、受けた引数を書き換えないこと (#3697)。**
//
// 本文の直下の文は、apiのPOST / GET / Matchの式文と、その関数の中だけで使う
// 値の:=に限る。ifやforの中に置くと条件次第で登録されないルートができ、
// api.Useやapi.Groupを呼ぶと、ほかの領域のルートのmiddlewareの並びが呼ぶ順で
// 変わる。引数を=で書き換えたりアドレスを取ったりすると、setupRoutesが渡した
// 値と登録が使う値が分かれる。
func TestRouteRegistrarsOnlyRegisterRoutes(t *testing.T) {
	p := parsePackage(t)
	regs := routeRegistrars(t, p)
	routes := 0
	for fn, r := range regs {
		params := map[string]bool{}
		for _, n := range r.params {
			params[n] = true
		}
		for _, st := range r.decl.Body.List {
			switch v := st.(type) {
			case *ast.ExprStmt:
				call, ok := v.X.(*ast.CallExpr)
				if !ok {
					t.Errorf("%s (%s) にルートの登録でない式文がある (%s)", fn, r.file, p.fset.Position(v.Pos()))
					continue
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					t.Errorf("%s (%s) にルートの登録でない式文がある (%s)", fn, r.file, p.fset.Position(v.Pos()))
					continue
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok || id.Name != "api" || !routeMethodsOnAPI[sel.Sel.Name] {
					t.Errorf("%s (%s) で api の POST / GET / Match 以外を呼んでいる (%s)", fn, r.file, p.fset.Position(v.Pos()))
					continue
				}
				routes++
			case *ast.AssignStmt:
				if v.Tok != token.DEFINE {
					t.Errorf("%s (%s) で := 以外の代入をしている (%s)", fn, r.file, p.fset.Position(v.Pos()))
				}
			default:
				t.Errorf("%s (%s) の本文の直下に、ルートの登録と := 以外の文がある (%s)", fn, r.file, p.fset.Position(v.Pos()))
			}
		}
		ast.Inspect(r.decl.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.AssignStmt:
				for _, l := range v.Lhs {
					if id, ok := l.(*ast.Ident); ok && params[id.Name] {
						t.Errorf("%s (%s) が引数 %s を %s で書き換えている", fn, r.file, id.Name, v.Tok)
					}
				}
			case *ast.UnaryExpr:
				if id, ok := v.X.(*ast.Ident); ok && v.Op == token.AND && params[id.Name] {
					t.Errorf("%s (%s) が引数 %s のアドレスを取っている", fn, r.file, id.Name)
				}
			}
			return true
		})
	}
	require.Greater(t, routes, 500, "登録の関数のルートを読めていない")
}

// apiRoutesOutsideRegistrars are the apiRoutes registrations this package
// makes outside the register…Routes functions, with the path by which each is
// reached.
var apiRoutesOutsideRegistrars = map[string]string{
	// test modeのときだけ登録する。handlerもifの中で作るので、そのifごと
	// setupRoutesに残した(#3697)。
	`api.POST("/reset-db", testHandler.ResetDB)`: "setupRoutes",
}

// **apiRoutesへのルートの登録は、register…Routesの中で行うこと (#3697)。**
//
// setupRoutesやほかの関数でapi.POSTなどを呼ぶと、上の2つのテストが見る
// 「呼ぶ位置」と「引数の名前」の外に登録ができる。パッケージのtrackedな
// 非テストのファイルすべてで、apiという名前の値へのPOST / GET / Matchの
// 呼び出しを集め、登録の関数の外にあるものは許可リストと突き合わせる。
func TestAPIRoutesAreRegisteredByRegistrars(t *testing.T) {
	p := parsePackage(t)
	regs := routeRegistrars(t, p)
	used := map[string]bool{}
	inside := 0
	for _, name := range p.names {
		for _, decl := range p.files[name].Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			_, isReg := regs[fd.Name.Name]
			isReg = isReg && fd.Recv == nil
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !routeMethodsOnAPI[sel.Sel.Name] {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "api" {
					return true
				}
				if isReg {
					inside++
					return true
				}
				var b strings.Builder
				b.WriteString("api." + sel.Sel.Name + "(")
				for i, a := range call.Args {
					if i > 0 {
						b.WriteString(", ")
					}
					b.WriteString(nodeSource(t, p.fset, a))
				}
				b.WriteString(")")
				text := b.String()
				where, ok := apiRoutesOutsideRegistrars[text]
				if !ok {
					t.Errorf("%s の %s が、register…Routes の外で /api のルートを登録している: %s", name, fd.Name.Name, text)
					return true
				}
				assert.Equalf(t, where, fd.Name.Name, "%s を登録している関数", text)
				used[text] = true
				return true
			})
		}
	}
	require.Greater(t, inside, 500, "登録の関数の中のルートを読めていない")
	for text, where := range apiRoutesOutsideRegistrars {
		assert.Truef(t, used[text], "apiRoutesOutsideRegistrars に %s (%s) があるが、ソースにはもう無い", text, where)
	}
}

// nodeSource renders n as it is written in the source.
func nodeSource(t *testing.T, fset *token.FileSet, n ast.Node) string {
	t.Helper()
	var b strings.Builder
	require.NoError(t, printer.Fprint(&b, fset, n))
	return b.String()
}
