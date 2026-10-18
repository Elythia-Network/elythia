package entitycompat

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// criticalWiringCount と recordCriticalWiring の表の実数を突き合わせる。
//
// **表を増やして定数を据え置くと、gate が全件について効かなくなる。**
// `verifyCriticalWiring` の判定は `len(deps) < criticalWiringCount` で、
// 定数が実数より小さいと「表から 1 行抜く」形の劣化を検出できなくなる。
// #2806 で 1 行足したときに実際に踏んだ (63 のまま 64 行にした)。
//
// `internal/server` のテストはすべて `criticalWiringCount` から導出されるので、
// 実数と突き合わせる検査はそちら側には置けない (定数を書き換えれば一緒に動く)。
// ソースを読む gate をここに置く理由も同じ。
//
// **AST で数える。** 文字列一致だと書き方を変えるだけで数え違える — 行頭の
// `{` で数える版は `{...}, {...}` と 1 行に 2 つ書くと 1 としか数えず、
// `gofmt -s` も通るので**どこにも引っかからないまま定数 < 実数を作れた**。
// 同 package の notfound / catalog gate と同じ方式に揃えてある。
func TestCriticalWiringCountMatchesTable(t *testing.T) {
	root := repoRoot(t)

	constSrc, err := os.ReadFile(filepath.Join(root, "internal/server/wiring_check.go"))
	if err != nil {
		t.Fatalf("read wiring_check.go: %v", err)
	}
	m := regexp.MustCompile(`(?m)^const criticalWiringCount = (\d+)$`).FindSubmatch(constSrc)
	if m == nil {
		t.Fatal("criticalWiringCount の宣言が見つからない。書き方を変えたならこの gate も直すこと")
	}
	want, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("parse criticalWiringCount: %v", err)
	}

	got := countCriticalWiringEntries(t, serverSourcePaths(t)...)
	if got != want {
		t.Fatalf("internal/server の recordCriticalWiring は %d 件だが、\n"+
			"criticalWiringCount は %d になっている。\n\n"+
			"**entry を増減したら wiring_check.go の定数も一緒に直すこと。** 定数が実数より\n"+
			"小さいと、verifyCriticalWiring の len(deps) < criticalWiringCount が\n"+
			"「表から 1 行抜く」形の劣化を検出できなくなる (全 %d 件について効かなくなる)。",
			got, want, got)
	}
}

// countCriticalWiringEntries counts the elements of the composite literal
// passed to recordCriticalWiring in the given files.
//
// **呼び出しはファイルをまたいでちょうど 1 つ (#3672)。** 表を別のファイルへ
// 移しても数え続け、2 つのファイルに分けたら (どれを数えるか決まらないので)
// 落とす。
func countCriticalWiringEntries(t *testing.T, paths ...string) int {
	t.Helper()
	if len(paths) == 0 {
		t.Fatal("countCriticalWiringEntries: 読むファイルが無い")
	}
	names := make([]string, len(paths))
	srcs := make([]string, len(paths))
	for i, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		names[i] = relPath(t, path)
		srcs[i] = string(src)
	}
	n, err := countCriticalWiringIn(names, srcs)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// countCriticalWiringIn is countCriticalWiringEntries on sources already read.
// The names are put into the FileSet so errors carry the file and the line.
func countCriticalWiringIn(names, srcs []string) (int, error) {
	fset := token.NewFileSet()
	found := -1
	foundIn := ""
	for i, name := range names {
		file, err := parser.ParseFile(fset, name, srcs[i], 0)
		if err != nil {
			return 0, fmt.Errorf("parse %s: %w", name, err)
		}
		var ierr error
		ast.Inspect(file, func(n ast.Node) bool {
			if ierr != nil {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "recordCriticalWiring" || len(call.Args) != 1 {
				return true
			}
			lit, ok := call.Args[0].(*ast.CompositeLit)
			if !ok {
				ierr = fmt.Errorf("%s: recordCriticalWiring の引数が composite literal ではない (%T)。"+
					"書き方を変えたならこの gate も直すこと", fset.Position(call.Pos()), call.Args[0])
				return false
			}
			if found >= 0 {
				ierr = fmt.Errorf("recordCriticalWiring の呼び出しが複数ある (%s と %s)。"+
					"どれを数えるかが決まらないので gate を直すこと", foundIn, fset.Position(call.Pos()))
				return false
			}
			found = len(lit.Elts)
			foundIn = fset.Position(call.Pos()).String()
			return true
		})
		if ierr != nil {
			return 0, ierr
		}
	}
	if found < 0 {
		return 0, fmt.Errorf("recordCriticalWiring の呼び出しが見つからない。書き方を変えたか、git add を忘れていないか")
	}
	return found, nil
}
