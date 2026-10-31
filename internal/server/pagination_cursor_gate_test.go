package server

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The cursor normalizers of internal/misc/id the gate looks for.
const (
	cursorPerDirection = "NormalizeCursor"
	cursorIDFirst      = "NormalizeCursorIDFirst"
	cursorIDPkg        = "github.com/elythia-network/elythia/internal/misc/id"
	modulePrefix       = "github.com/elythia-network/elythia/"
)

// paginationCursorGolden is the output of tools/paginationspec: upstream's
// cursor form per endpoint.
const paginationCursorGolden = "internal/entitycompat/testdata/golden_pagination_cursor.json"

// paginationCursorExempt are the upstream endpoints whose Elythia handler the
// gate cannot match against the golden, with the path through which the
// handler reads the date cursors.
//
// #3369 の時点では空 (golden の 77 endpoint がすべて照合できた)。core の service
// で日付を扱うなど、handler のパッケージの中で正規化の関数に届かない形を足したら、
// ここに経路を書いて外す。
var paginationCursorExempt = map[string]string{}

// mustCheckPaginationCursor names endpoints the gate must resolve and check,
// per form. **何も拾えなかったら落とす (CLAUDE.md Section 4)。** 抽出が空振り
// すると、照合する対象が無いまま緑になる。
var mustCheckPaginationCursor = map[string]string{
	"federation/users":      cursorIDFirst,
	"drive/files":           cursorIDFirst,
	"channels/followed":     cursorIDFirst,
	"flash/search":          cursorIDFirst,
	"notes/global-timeline": cursorIDFirst,
	"notes/timeline":        cursorPerDirection,
	"notes/local-timeline":  cursorPerDirection,
	"i/notifications":       cursorPerDirection,
	"antennas/notes":        cursorPerDirection,
}

// handlerFuncRe parses the name echo gives a route's handler when it is a
// method value: "<import path>.(*<Type>).<Method>-fm".
var handlerFuncRe = regexp.MustCompile(`^(.+)\.\(\*?(\w+)\)\.(\w+)-fm$`)

// cursorReach is what the handler reaches.
type cursorReach struct {
	perDirection, idFirst bool
}

// handlerPackage is one parsed handler package.
type handlerPackage struct {
	idNames map[*ast.File]string // the import name of internal/misc/id per file
	files   map[*ast.FuncDecl]*ast.File
	funcs   map[string]*ast.FuncDecl   // top-level functions by name
	methods map[string][]*ast.FuncDecl // methods by name, any receiver
	imports map[*ast.File]map[string]bool
}

// parseHandlerPackage parses the git-tracked non-test files of the package
// at the import path.
func parseHandlerPackage(t *testing.T, root, importPath string) *handlerPackage {
	t.Helper()
	rel, ok := strings.CutPrefix(importPath, modulePrefix)
	require.Truef(t, ok, "handler package %s is outside the module", importPath)
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--", rel).Output()
	require.NoError(t, err, "git ls-files %s", rel)
	p := &handlerPackage{
		idNames: map[*ast.File]string{},
		files:   map[*ast.FuncDecl]*ast.File{},
		funcs:   map[string]*ast.FuncDecl{},
		methods: map[string][]*ast.FuncDecl{},
		imports: map[*ast.File]map[string]bool{},
	}
	fset := token.NewFileSet()
	n := 0
	for _, name := range strings.Split(string(out), "\x00") {
		base, ok := strings.CutPrefix(name, rel+"/")
		if !ok || strings.Contains(base, "/") || !strings.HasSuffix(base, ".go") || strings.HasSuffix(base, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(root, name), nil, 0)
		require.NoError(t, err)
		n++
		p.imports[f] = map[string]bool{}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			local := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				local = imp.Name.Name
			}
			p.imports[f][local] = true
			if path == cursorIDPkg {
				p.idNames[f] = local
			}
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			p.files[fd] = f
			if fd.Recv == nil {
				p.funcs[fd.Name.Name] = fd
			} else {
				p.methods[fd.Name.Name] = append(p.methods[fd.Name.Name], fd)
			}
		}
	}
	require.Positivef(t, n, "git ls-files returned no source of %s", rel)
	return p
}

// method returns the method of the receiver type, nil when there is none.
func (p *handlerPackage) method(recv, name string) *ast.FuncDecl {
	for _, fd := range p.methods[name] {
		typ := fd.Recv.List[0].Type
		if st, ok := typ.(*ast.StarExpr); ok {
			typ = st.X
		}
		if id, ok := typ.(*ast.Ident); ok && id.Name == recv {
			return fd
		}
	}
	return nil
}

// reach walks from the function through every function and method of the
// same package that it names, and reports which cursor normalizers of
// internal/misc/id are named on the way.
//
// **名前で辿る (型を引かない)。** 呼び出したメソッドと、呼び出しの引数に
// 渡したメソッドは、同じ名前のものを受け手の型を問わず全部辿る。関数は、呼び出し
// でなく値として名前を出しただけでも辿る。広めに辿るので、ID 優先の handler が
// 向きごとの helper を共有していれば「両方に届く」で落ちる。同じ名前の別の型の
// メソッドを取り違えて落ちたら、helper の名前を分ける (notes の listRequest.resolve
// と TimelineRequest.normalize)。フィールドの読み出し (`note.Reactions`) は辿らない。
//
// 見えないもの: 別のパッケージ (core の service など) で日付を扱う形、変数に
// 入れてから呼ぶメソッド、internal/misc/id の dot import (`import . ".../id"`。
// 別名の import は idNames で解いているので見える)。どちらの関数にも届かなく
// なった handler は「どちらにも届かない」で落ちる。**ただし、正しい関数には
// 直接届き、逆の関数には変数に入れたメソッド値を経由してだけ届く形は見落とす。**
// 照合できないものは
// paginationCursorExempt に、日付を扱う経路を書いて外す。
func (p *handlerPackage) reach(start *ast.FuncDecl) cursorReach {
	var r cursorReach
	seen := map[*ast.FuncDecl]bool{}
	queue := []*ast.FuncDecl{start}
	for len(queue) > 0 {
		fd := queue[0]
		queue = queue[1:]
		if seen[fd] {
			continue
		}
		seen[fd] = true
		file := p.files[fd]
		idName := p.idNames[file]
		follow := func(e ast.Expr) {
			if sel, ok := e.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && p.imports[file][pkg.Name] {
					return
				}
				queue = append(queue, p.methods[sel.Sel.Name]...)
			}
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := x.X.(*ast.Ident); ok && idName != "" && pkg.Name == idName {
					switch x.Sel.Name {
					case cursorPerDirection:
						r.perDirection = true
					case cursorIDFirst:
						r.idFirst = true
					}
				}
			case *ast.CallExpr:
				follow(x.Fun)
				for _, arg := range x.Args {
					follow(arg)
				}
			case *ast.Ident:
				if fd, ok := p.funcs[x.Name]; ok {
					queue = append(queue, fd)
				}
			}
			return true
		})
	}
	return r
}

// TestPaginationCursorFormMatchesUpstream is the gate of #3369: the handler
// of every upstream endpoint that passes the date cursors to
// makePaginationQuery (golden "id-first") reaches id.NormalizeCursorIDFirst
// and not id.NormalizeCursor, and the handler of every endpoint that combines
// them per direction ("per-direction") reaches id.NormalizeCursor and not
// id.NormalizeCursorIDFirst. The golden is tools/paginationspec's output.
//
// route から handler を引くのは、本番と同じく組み立てたサーバーの echo の route の
// 名前 (`<import path>.(*Handler).<Method>-fm`) から。ソースの登録を静的に読むと、
// 変数を経由した handler の型を引き損ねうる (TestAPIRoutesHaveParamDefs と同じ
// 判断)。handler の中は、同じパッケージの関数とメソッドを名前で辿る (reach の
// コメントに限界)。本家に無い Elythia 独自の endpoint (golden に無い) は見ない。
func TestPaginationCursorFormMatchesUpstream(t *testing.T) {
	if serverIntegrationDB == nil {
		// CIではTestMainが落とすので、ここには来ない(requireDBEnv)。
		t.Skip("PostgreSQL unavailable")
	}
	root := repoRootDir(t)
	raw, err := os.ReadFile(filepath.Join(root, paginationCursorGolden))
	require.NoError(t, err)
	var golden map[string]string
	require.NoError(t, json.Unmarshal(raw, &golden))

	echoRoutes, _ := builtEchoRoutes(t)
	handlers := map[string]map[string]bool{} // endpoint -> handler names
	for _, r := range echoRoutes {
		name, ok := strings.CutPrefix(r.Path, apiGroupPrefix+"/")
		if !ok || r.Method == echo.RouteNotFound {
			continue
		}
		if handlers[name] == nil {
			handlers[name] = map[string]bool{}
		}
		handlers[name][r.Name] = true
	}

	pkgs := map[string]*handlerPackage{}
	checked := map[string]string{}
	failing := map[string]bool{}
	names := make([]string, 0, len(golden))
	for ep := range golden {
		names = append(names, ep)
	}
	sort.Strings(names)
	for _, ep := range names {
		var want string
		switch golden[ep] {
		case "id-first", "id-first-ordered-by-since":
			want = cursorIDFirst
		case "per-direction":
			want = cursorPerDirection
		case "unused":
			continue
		default:
			t.Fatalf("%s: unknown form %q in %s", ep, golden[ep], paginationCursorGolden)
		}
		_, exempt := paginationCursorExempt[ep]
		require.NotEmptyf(t, handlers[ep], "/api/%s is not routed", ep)
		var problems []string
		for fn := range handlers[ep] {
			m := handlerFuncRe.FindStringSubmatch(fn)
			if m == nil {
				problems = append(problems, fn+" is not a method value")
				continue
			}
			pkg := pkgs[m[1]]
			if pkg == nil {
				pkg = parseHandlerPackage(t, root, m[1])
				pkgs[m[1]] = pkg
			}
			decl := pkg.method(m[2], m[3])
			if decl == nil {
				problems = append(problems, fn+" not found in its package")
				continue
			}
			r := pkg.reach(decl)
			got := map[string]bool{cursorPerDirection: r.perDirection, cursorIDFirst: r.idFirst}
			other := cursorPerDirection
			if want == cursorPerDirection {
				other = cursorIDFirst
			}
			switch {
			case got[other]:
				problems = append(problems, fn+" reaches id."+other)
			case !got[want]:
				problems = append(problems, fn+" reaches neither cursor normalizer")
			}
		}
		if len(problems) > 0 {
			failing[ep] = true
			if !exempt {
				sort.Strings(problems)
				t.Errorf("/api/%s is %s upstream, so its handler should call id.%s: %s", ep, golden[ep], want, strings.Join(problems, "; "))
			}
			continue
		}
		checked[ep] = want
	}

	// 抽出の下限。実在する endpoint を名指しで要求し、件数にも下限を置く。
	for ep, want := range mustCheckPaginationCursor {
		assert.Equalf(t, want, checked[ep], "/api/%s was not checked as %s", ep, want)
	}
	counts := map[string]int{}
	for _, want := range checked {
		counts[want]++
	}
	t.Logf("checked %d id-first and %d per-direction endpoints", counts[cursorIDFirst], counts[cursorPerDirection])
	assert.GreaterOrEqual(t, counts[cursorIDFirst], 50, "too few id-first endpoints were checked")
	assert.GreaterOrEqual(t, counts[cursorPerDirection], 15, "too few per-direction endpoints were checked")

	// 例外の一覧に、使われていない項目を残さない。
	for ep, why := range paginationCursorExempt {
		assert.Truef(t, failing[ep], "paginationCursorExempt has %s (%s), but its handler matches the golden now (or it is not in the golden); drop it", ep, why)
	}
}
