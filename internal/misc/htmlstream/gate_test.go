package htmlstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const xnetHTML = "golang.org/x/net/html"

// treeFuncs are the x/net/html functions that build a document tree.
var treeFuncs = map[string]bool{
	"Parse": true, "ParseWithOptions": true, "ParseFragment": true, "ParseFragmentWithOptions": true,
}

// knownHTMLImporters are non-test files that import x/net/html (without
// building a tree). The scan must find each of them, so that a scan that
// misses the files does not pass.
var knownHTMLImporters = []string{
	"internal/misc/htmlstream/htmlstream.go",
	"internal/misc/htmltext/htmltext.go",
}

// otherHTMLParsers lists packages that parse HTML into a tree internally (with
// their subpackages), the non-test files allowed to import each, and the route
// they are reached by. どれも今は許していない (fetch-rss のフィードは自前で読む)。
var otherHTMLParsers = map[string]map[string]string{
	"github.com/mmcdole/gofeed":      {},
	"github.com/PuerkitoBio/goquery": {},
}

// dependencyTreeParsers lists the packages outside this repository, reached
// from its non-test code, that build an x/net/html tree, and the route the
// HTML they parse comes by. どれも今は無い (fetch-rss は gofeed を使わない)。
var dependencyTreeParsers = map[string]string{}

// checkSource reports, for one Go file, the uses of x/net/html that build a
// tree (calls and function values, under any import name), a dot import of
// x/net/html, and imports of the packages in banned (with their
// subpackages). It also reports whether the file imports x/net/html.
func checkSource(name string, src []byte, banned map[string]bool) (problems []string, importsHTML bool, err error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, false, err
	}
	var locals []string
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		for pkg := range banned {
			if path == pkg || strings.HasPrefix(path, pkg+"/") {
				problems = append(problems, fmt.Sprintf("%s imports %s, which parses HTML into a tree", name, path))
			}
		}
		if path != xnetHTML {
			continue
		}
		importsHTML = true
		local := "html"
		if imp.Name != nil {
			local = imp.Name.Name
		}
		switch local {
		case ".":
			// ドット import では呼び出しが SelectorExpr にならず、下の検査を通り抜ける。
			problems = append(problems, fmt.Sprintf("%s dot-imports %s", name, xnetHTML))
		case "_":
		default:
			locals = append(locals, local)
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !treeFuncs[sel.Sel.Name] {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok {
			for _, local := range locals {
				if id.Name == local {
					problems = append(problems, fmt.Sprintf("%s: %s uses %s.%s", name, fset.Position(sel.Pos()), local, sel.Sel.Name))
				}
			}
		}
		return true
	})
	return problems, importsHTML, nil
}

// Each form of building a tree, and each banned import, is reported.
func TestCheckSource(t *testing.T) {
	banned := map[string]bool{"github.com/PuerkitoBio/goquery": true}
	for _, tc := range []struct {
		name, src string
		problems  int
		imports   bool
	}{
		{"call", `package p; import "golang.org/x/net/html"; func f() { html.Parse(nil) }`, 1, true},
		{"value", `package p; import "golang.org/x/net/html"; var g = html.ParseFragmentWithOptions`, 1, true},
		{"alias", `package p; import h "golang.org/x/net/html"; var g = h.ParseWithOptions`, 1, true},
		{"all four", `package p; import "golang.org/x/net/html"; var a, b, c, d = html.Parse, html.ParseWithOptions, html.ParseFragment, html.ParseFragmentWithOptions`, 4, true},
		{"dot", `package p; import . "golang.org/x/net/html"; var g = NewTokenizer`, 1, true},
		{"banned", `package p; import _ "github.com/PuerkitoBio/goquery"`, 1, false},
		{"banned subpackage", `package p; import "github.com/PuerkitoBio/goquery/x"; var _ = x.Y`, 1, false},
		{"tokenizer", `package p; import "golang.org/x/net/html"; var g = html.NewTokenizer`, 0, true},
		{"blank", `package p; import _ "golang.org/x/net/html"`, 0, true},
		{"other package", `package p; import "example.com/html"; var g = html.Parse`, 0, false},
		{"alias and other package", `package p; import (h "golang.org/x/net/html"; "example.com/html"); var g, k = html.Parse, h.NewTokenizer`, 0, true},
		{"not imported", `package p; func f(html struct{ Parse int }) int { return html.Parse }`, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems, imports, err := checkSource(tc.name+".go", []byte(tc.src), banned)
			require.NoError(t, err)
			assert.Len(t, problems, tc.problems, "%v", problems)
			assert.Equal(t, tc.imports, imports)
		})
	}
}

// TestNoHTMLTreeParse fails when non-test Go code of this repository builds a
// tree from HTML with x/net/html, dot-imports x/net/html, or imports a package
// that parses HTML into a tree outside the allowlist.
//
// 外から来る HTML は木を作らずに読む (htmlstream、または x/net/html のトークナイザ)。
// 前の実装と突き合わせるテストは html.Parse を使うので、テストのファイルは見ない。
func TestNoHTMLTreeParse(t *testing.T) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	root := strings.TrimSpace(string(out))
	ls := exec.Command("git", "ls-files", "--", "*.go")
	ls.Dir = root
	files, err := ls.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	banned := map[string]bool{}
	for pkg := range otherHTMLParsers {
		banned[pkg] = true
	}
	seenHTML := map[string]bool{}
	seenOther := map[string]bool{}
	for _, name := range strings.Fields(string(files)) {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		problems, imports, err := checkSource(name, src, banned)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		seenHTML[name] = imports
		for _, p := range problems {
			allowed := false
			for pkg, files := range otherHTMLParsers {
				if _, ok := files[name]; ok && strings.Contains(p, " imports "+pkg) {
					allowed = true
					seenOther[pkg+" "+name] = true
				}
			}
			if !allowed {
				t.Errorf("%s; read HTML with htmlstream", p)
			}
		}
	}
	// 一覧の項目は、実際に import しているものだけにする (使われていない項目を残さない)。
	// 名指しした importer は、走査が空振りしたときに落とす下限を兼ねる。
	var missing []string
	for _, name := range knownHTMLImporters {
		if !seenHTML[name] {
			missing = append(missing, name)
		}
	}
	for path, allowed := range otherHTMLParsers {
		for name := range allowed {
			if !seenOther[path+" "+name] {
				missing = append(missing, path+" "+name)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("listed files that no longer import the package (remove them, or the scan missed the files): %v", missing)
	}
}

type listedPackage struct {
	ImportPath string
	Dir        string
	Standard   bool
	GoFiles    []string
	CgoFiles   []string
	Imports    []string
	Error      *struct{ Err string }
	DepsErrors []*struct{ Err string }
}

// listDependencies returns the packages that the non-test code of this module
// reaches (go list -deps ./...).
//
// CI と同じく go.work を使わずに読み (GOWORK=off)、make plugins が作る生成物は
// 無いものとして読む。同梱プラグインは別の module なので、ここには入らない
// (外から来る HTML を読むプラグインは今は無い)。
func listDependencies(t *testing.T) []listedPackage {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	root := strings.TrimSpace(string(out))
	// go test は自分の toolchain の bin を PATH の先頭に置くので、PATH の go がそれになる。
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"list", "-e", "-deps", "-json=ImportPath,Dir,Standard,GoFiles,CgoFiles,Imports,Error,DepsErrors"}
	if overlay := withoutGeneratedPlugins(t, root); overlay != "" {
		args = append(args, "-overlay", overlay)
	}
	cmd := exec.Command(goBin, append(args, "./...")...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	listed, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	var pkgs []listedPackage
	dec := json.NewDecoder(bytes.NewReader(listed))
	for {
		var p listedPackage
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

// generatedPlugins is the file that `make plugins` writes (gitignored). It
// imports the plugins' modules, which go.mod does not have.
const generatedPlugins = "cmd/elythia/plugins_generated.go"

// withoutGeneratedPlugins returns a go list -overlay file that leaves out
// generatedPlugins when the checkout has it, or "" when it does not, so that
// the dependencies are the same as in a clean checkout (CI).
func withoutGeneratedPlugins(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, generatedPlugins)
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	b, err := json.Marshal(map[string]map[string]string{"Replace": {path: ""}})
	if err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(t.TempDir(), "overlay.json")
	if err := os.WriteFile(overlay, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return overlay
}

// dependencyProblems checks the listed packages: the ones that failed to load,
// and the ones that build an x/net/html tree outside allowed. It also returns
// the packages that import x/net/html and how many of their files it read.
func dependencyProblems(pkgs []listedPackage, allowed map[string]string) (problems []string, used, importers map[string]bool, files int, err error) {
	used, importers = map[string]bool{}, map[string]bool{}
	for _, p := range pkgs {
		if p.Error != nil {
			problems = append(problems, fmt.Sprintf("%s: %s", p.ImportPath, p.Error.Err))
		}
		for _, e := range p.DepsErrors {
			problems = append(problems, fmt.Sprintf("%s: %s", p.ImportPath, e.Err))
		}
		if p.Standard || p.ImportPath == xnetHTML || !contains(p.Imports, xnetHTML) {
			continue
		}
		importers[p.ImportPath] = true
		var found []string
		for _, f := range append(append([]string{}, p.GoFiles...), p.CgoFiles...) {
			src, err := os.ReadFile(filepath.Join(p.Dir, f))
			if err != nil {
				return nil, nil, nil, 0, err
			}
			files++
			pr, _, err := checkSource(filepath.Join(p.ImportPath, f), src, nil)
			if err != nil {
				return nil, nil, nil, 0, err
			}
			found = append(found, pr...)
		}
		if len(found) == 0 {
			continue
		}
		if _, ok := allowed[p.ImportPath]; ok {
			used[p.ImportPath] = true
			continue
		}
		problems = append(problems, fmt.Sprintf("%s builds an x/net/html tree (%v); list the route in dependencyTreeParsers or avoid the package", p.ImportPath, found))
	}
	return problems, used, importers, files, nil
}

// dependencyProblems reads the files of the packages that import x/net/html,
// reports the ones that build a tree, and reports the packages go list failed
// to load.
func TestDependencyProblems(t *testing.T) {
	pkgs := []listedPackage{
		{ImportPath: "example.com/tree", Dir: "testdata/gate", GoFiles: []string{"tree.go.txt"}, Imports: []string{xnetHTML}},
		{ImportPath: "example.com/tokens", Dir: "testdata/gate", GoFiles: []string{"tokens.go.txt"}, Imports: []string{xnetHTML}},
		{ImportPath: "example.com/other", Dir: "testdata/gate", GoFiles: []string{"tree.go.txt"}},
		{ImportPath: "example.com/broken", Error: &struct{ Err string }{"cannot find module"}},
		{ImportPath: "example.com/dep", DepsErrors: []*struct{ Err string }{{"missing go.sum entry"}}},
	}
	problems, used, importers, files, err := dependencyProblems(pkgs, nil)
	require.NoError(t, err)
	assert.Len(t, problems, 3, "%v", problems)
	assert.Contains(t, strings.Join(problems, "\n"), "example.com/tree builds")
	assert.Empty(t, used)
	assert.Equal(t, map[string]bool{"example.com/tree": true, "example.com/tokens": true}, importers)
	assert.Equal(t, 2, files)

	problems, used, _, _, err = dependencyProblems(pkgs[:2], map[string]string{"example.com/tree": "a route"})
	require.NoError(t, err)
	assert.Empty(t, problems)
	assert.Equal(t, map[string]bool{"example.com/tree": true}, used)
}

// TestNoHTMLTreeParseInDependencies fails when a package reached from the
// non-test code (go list -deps ./...) builds an x/net/html tree and is not in
// dependencyTreeParsers, or when go list failed to load a package.
// 依存を足したとき、その依存が中で木を作るのも拾う。
func TestNoHTMLTreeParseInDependencies(t *testing.T) {
	problems, used, importers, files, err := dependencyProblems(listDependencies(t), dependencyTreeParsers)
	require.NoError(t, err)
	for _, p := range problems {
		t.Error(p)
	}
	// 走査が空振りしても緑にならないよう、x/net/html を使う自分のパッケージに届き、
	// そのファイルを読んだことを要求する。
	assert.True(t, importers["github.com/elythia-network/elythia/internal/misc/htmlstream"], "go list did not reach htmlstream: %v", importers)
	assert.GreaterOrEqual(t, files, 2, "the files of the importers were not read")
	for pkg := range dependencyTreeParsers {
		assert.True(t, used[pkg], "dependencyTreeParsers has an entry for a package that no longer builds a tree (or is no longer reached): %s", pkg)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
