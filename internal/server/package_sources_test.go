package server

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// mustReadPackageFiles names files packageSources must return.
//
// **何も拾えなかったら落とす (CLAUDE.md Section 4)。** `git ls-files` の
// 呼び方を誤って空の一覧を返すと、「呼び出しが無いこと」を見るゲートは緑の
// まま通る。組み立ての部品化 (#3666) で動かしにくいファイルを名指しする。
// `router.go` は分割の対象なので名指ししない。
var mustReadPackageFiles = []string{"server.go", "wiring_check.go"}

// packageSource is one non-test source file of this package.
type packageSource struct {
	name string // file name, e.g. router.go
	src  string
}

// packageSources returns the git-tracked non-test .go files of this package,
// sorted by name.
//
// **ファイルを名指しせず、パッケージ全体を読む (#3672)。** 組み立てのコードを
// 別のファイルへ移すたびに、ソースを読むテストが「呼び出しが無い」で落ちない
// ようにするため。
//
// **`git ls-files` で列挙する。** ディスクを走査すると、`git add` を忘れた
// 新しいファイルが手元でだけ読まれ、CI で初めて落ちる。サブディレクトリ
// (`middleware/`) は別のパッケージなので読まない。
func packageSources(t *testing.T) []packageSource {
	t.Helper()
	// go test はパッケージのディレクトリで走るので、出力はそこからの相対パス。
	out, err := exec.Command("git", "ls-files", "-z", "--", ".").Output()
	require.NoError(t, err, "git ls-files")
	untracked := untrackedPackageFiles(t)
	var files []packageSource
	for _, name := range strings.Split(string(out), "\x00") {
		if !isPackageSource(name) {
			continue
		}
		b, err := os.ReadFile(name)
		require.NoError(t, err)
		files = append(files, packageSource{name: name, src: string(b)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	got := map[string]bool{}
	for _, f := range files {
		got[f.name] = true
	}
	for _, want := range mustReadPackageFiles {
		require.Truef(t, got[want], "git ls-files が %s を返さない (%d ファイル)。列挙の仕方が壊れている%s",
			want, len(files), untrackedHint(untracked))
	}
	return files
}

// isPackageSource reports whether a path relative to this directory is a
// non-test .go file of this package.
func isPackageSource(name string) bool {
	return name != "" && !strings.Contains(name, "/") &&
		strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
}

var (
	untrackedHintMu   sync.Mutex
	untrackedHintSeen = map[*testing.T]bool{}
)

// untrackedPackageFiles returns the non-test .go files of this package that
// git does not track, and arranges for the test to name them if it fails.
//
// **git add を忘れた新しいファイルは、手元でも CI でも読まれない (#3672
// レビュー L-5)。** `go build` は通るので、落ちた人は「呼び出しが無い」の原因に
// 気付きにくい。
func untrackedPackageFiles(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "ls-files", "-z", "--others", "--exclude-standard", "--", ".").Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, name := range strings.Split(string(out), "\x00") {
		if isPackageSource(name) {
			files = append(files, name)
		}
	}
	if len(files) == 0 {
		return nil
	}
	untrackedHintMu.Lock()
	seen := untrackedHintSeen[t]
	untrackedHintSeen[t] = true
	untrackedHintMu.Unlock()
	if !seen {
		t.Cleanup(func() {
			if t.Failed() {
				t.Log(strings.TrimSpace(untrackedHint(files)))
			}
			untrackedHintMu.Lock()
			delete(untrackedHintSeen, t)
			untrackedHintMu.Unlock()
		})
	}
	return files
}

// untrackedHint formats the untracked files for a failure message.
func untrackedHint(files []string) string {
	if len(files) == 0 {
		return ""
	}
	return fmt.Sprintf("\ninternal/server に git add していない .go がある: %v。"+
		"ソースを読むテストは git ls-files で列挙するので、これらは読まれない (git add を忘れていないか)", files)
}

// packageSourceWithoutComments returns every packageSources file with comments
// stripped, joined by a newline.
//
// **改行でつなぐ。** 行の続きを見る正規表現 (`[^\n]*(?:\n\t\t+[^\n]*)*`) が
// ファイルの境目をまたがない。次のファイルは `package server` から始まるので、
// 字下げの続きとして読まれることも無い。
func packageSourceWithoutComments(t *testing.T) string {
	t.Helper()
	return joinWithoutComments(packageSources(t))
}

// joinWithoutComments strips comments from each file and joins them with a
// newline, so no line spans two files even when a file lacks a final newline.
func joinWithoutComments(files []packageSource) string {
	parts := make([]string, len(files))
	for i, f := range files {
		parts[i] = stripComments(f.src)
	}
	return strings.Join(parts, "\n")
}

// 列挙がこのパッケージの非テストの .go を全部返すこと (#3672)。
//
// **/api のルートの登録は routes_<領域>.go に、ほかの配線はほぼ router.go に
// ある (#3697)。router.go だけを読む形に戻すと、ルートを読むテストは落ちるが、
// 配線を読むテストの多くは緑のまま通る。** 組み立てを別のファイルへ移した時点で
// 初めて落ち、移した人がテストを直すことになる。ここで、列挙が
// ディレクトリの非テストの .go と一致すること、つないだ本文が各ファイルの
// 本文を含むことを直接見る。
func TestPackageSourcesCoverEveryFile(t *testing.T) {
	files := packageSources(t)

	// **列挙のフィルタとは別の経路で「追跡済みの非テストの .go」を数える。**
	// ディスクのファイルを 1 つずつ `git ls-files --error-unmatch` に問う。
	// 列挙のフィルタが 1 ファイルでも落とすと、ここと食い違う。
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	var tracked, listed []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if exec.Command("git", "ls-files", "--error-unmatch", "--", name).Run() != nil {
			continue // 未追跡 (git add 前) のファイルは CI に無い
		}
		tracked = append(tracked, name)
	}
	for _, f := range files {
		listed = append(listed, f.name)
	}
	require.Equal(t, tracked, listed, "列挙が、追跡済みの非テストの .go と一致しない")
	require.GreaterOrEqualf(t, len(listed), 30,
		"internal/server の非テストの .go が %d ファイルしかない (30 以上あるはず)", len(listed))

	joined := packageSourceWithoutComments(t)
	for _, f := range files {
		// 本文を診断に出さない (数千行になる)。
		require.Truef(t, strings.Contains(joined, stripComments(f.src)),
			"%s の本文が packageSourceWithoutComments に入っていない", f.name)
	}
}

// ファイルの境目で行がつながらないこと (#3672 レビュー M-1)。
//
// **最後の改行が無いファイルの次に来たファイルの先頭を、同じ行として読まない。**
// 行頭を見る正規表現 (`(?m)^\s*api := ...`) が、境目で見落としや取り違えをする。
func TestJoinWithoutCommentsSeparatesFiles(t *testing.T) {
	got := joinWithoutComments([]packageSource{
		{name: "a.go", src: "package server\n\nvar a = 1"},
		{name: "b.go", src: "\tapi := apiRoutes{s.echo.Group(\"/api\")}\n"},
	})
	require.Len(t, regexp.MustCompile(`(?m)^\s*api := `).FindAllString(got, -1), 1,
		"2 つ目のファイルの先頭が、1 つ目の最後の行につながっている: %q", got)
}
