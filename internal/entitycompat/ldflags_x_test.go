package entitycompat

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ldflagsX matches a linker `-X importpath.name=value` flag, also right
// after `-ldflags=`. The path must contain a slash so that `curl -X POST`
// never matches.
var ldflagsX = regexp.MustCompile(`(?:^|[\s"'=])-X[ =]([^\s="']+/[^\s="']*)\.(\w+)=`)

// ldflagsXRequired names the -X flags that must be found, as "file: name".
//
// **名指しで要求する。** `-X` はパスを間違えても link が黙って無視するので、版と
// commit が空のまま出荷されてもビルドは通る。抽出の書式が変わって 1 件も拾えなく
// なったときに緑のままにならないよう、実在する指定を下限として置く。
var ldflagsXRequired = []string{
	"Makefile: MkGoVersion",
	"Makefile: MisskeyVersion",
	"Makefile: MkGoCommit",
	"Dockerfile: MkGoCommit",
	"deploy/uds/Dockerfile.mkgo: MkGoCommit",
}

// TestLdflagsXTargetsExist checks that every `-X importpath.name=` in the
// tracked files points at a package-level var of this module (#3394).
//
// モジュールパスを変えたとき (#3394)、置換から漏れた `-X` は警告なしに効かなくなる。
// 変数を const に変えた場合も同じく黙って無視される。
func TestLdflagsXTargetsExist(t *testing.T) {
	root := repoRoot(t)
	module := readModulePath(t, root)

	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--", ".", ":!frontend").Output()
	require.NoErrorf(t, err, "git ls-files に失敗した: %v", err)

	found := map[string]bool{}
	for _, rel := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || strings.IndexByte(string(b), 0) >= 0 {
			// 読めないもの (submodule の名残など) とバイナリは対象外。
			continue
		}
		for _, m := range ldflagsX.FindAllStringSubmatch(string(b), -1) {
			pkg, name := m[1], m[2]
			found[rel+": "+name] = true
			if problem := ldflagsXProblem(root, module, pkg, name); problem != "" {
				t.Errorf("%s: -X %s.%s: %s", rel, pkg, name, problem)
			}
		}
	}
	for _, want := range ldflagsXRequired {
		if !found[want] {
			t.Errorf("%s の -X が見つかりません (抽出が壊れたか、指定が消えています)", want)
		}
	}
}

// ldflagsXProblem describes why `-X pkg.name=` would be ignored by the
// linker, or returns "" when pkg is a package of module declaring var name.
func ldflagsXProblem(root, module, pkg, name string) string {
	rel, ok := strings.CutPrefix(pkg, module+"/")
	if !ok {
		return "このモジュール (" + module + ") のパッケージではありません。link は黙って無視します"
	}
	dir := filepath.Join(root, filepath.FromSlash(rel))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "パッケージ " + rel + " が見つかりません"
	}
	fset := token.NewFileSet()
	sources := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			return e.Name() + " を parse できません: " + err.Error()
		}
		sources++
		if declaresVar(f, name) {
			return ""
		}
	}
	if sources == 0 {
		return "パッケージ " + rel + " が見つかりません"
	}
	return "パッケージ " + rel + " に var " + name + " がありません (const は -X で書き換えられません)"
}

// declaresVar reports whether f has a package-level var named name.
func declaresVar(f *ast.File, name string) bool {
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			for _, id := range spec.(*ast.ValueSpec).Names {
				if id.Name == name {
					return true
				}
			}
		}
	}
	return false
}

func readModulePath(t *testing.T, root string) string {
	t.Helper()
	f, err := os.Open(filepath.Join(root, "go.mod"))
	require.NoError(t, err)
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if path, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
			return strings.Trim(strings.TrimSpace(path), `"`)
		}
	}
	t.Fatal("go.mod に module 行がありません")
	return ""
}

// 検査の枝そのものを固定する。実際のファイルは今は全部通るので、壊れた形を直接渡す。
func TestLdflagsXProblem_DetectsBrokenForms(t *testing.T) {
	root := repoRoot(t)
	module := readModulePath(t, root)
	if p := ldflagsXProblem(root, module, module+"/internal/config", "MkGoCommit"); p != "" {
		t.Fatalf("the correct form must pass: %s", p)
	}
	cases := map[string][2]string{
		"old module path":   {"github.com/shiroha-a/mk/internal/config", "MkGoCommit"},
		"missing package":   {module + "/internal/nosuchpkg", "MkGoCommit"},
		"missing var":       {module + "/internal/config", "NoSuchVar"},
		"const, not a var":  {module + "/internal/config", "MkGoRepositoryURL"},
		"test-only package": {module + "/internal/entitycompat", "ldflagsXRequired"},
	}
	for name, c := range cases {
		if ldflagsXProblem(root, module, c[0], c[1]) == "" {
			t.Errorf("%s: the broken form passed", name)
		}
	}

	// 例文は flag を連結で組み立てる。このファイル自身も走査の対象なので、
	// 書いたままの形だと TestLdflagsXTargetsExist が例文を拾って落ちる。
	const x = "-" + "X"
	for _, s := range []string{
		`LDFLAGS += ` + x + ` github.com/a/b/internal/config.V=1`,
		`ENV F="` + x + ` github.com/a/b/internal/config.V=${X}"`,
		`go build -ldflags "-s -w ` + x + `=github.com/a/b/c.V=x"`,
		`go build -ldflags=` + x + `=github.com/a/b/c.V=x`,
	} {
		if !ldflagsX.MatchString(s) {
			t.Errorf("must match: %s", s)
		}
	}
	for _, s := range []string{
		`curl -fsS -X POST -H 'Content-Type: application/json'`,
		`curl -X POST "https://example.com/api/meta"`,
	} {
		if ldflagsX.MatchString(s) {
			t.Errorf("must not match: %s", s)
		}
	}
}
