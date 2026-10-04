package entitycompat

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// tableRowRe matches a line that this gate treats as a table row.
//
// **先頭のパイプを要求する。** GFM は外側のパイプを省略できるが (`A | B` /
// `--- | ---` も表になる)、要求をやめると「表がどこで終わるか」を知る必要が出て、
// そのためには list / HTML ブロック / 引用といった**あらゆるブロックの開始**を
// 自前で判定することになる。それを試したところ、リスト直後の表という**ごく普通の
// 書き方で偽陽性**を出した (#2930 のレビュー 2 周目)。このリポジトリの表 227 個は
// すべて先頭パイプ付きなので、要求しても取りこぼしは無い。
var tableRowRe = regexp.MustCompile(`^ {0,3}\|`)

// tableDelimRe matches the `|---|---|` line that turns the line above it into
// a table header.
var tableDelimRe = regexp.MustCompile(`^ {0,3}\|[\s:|-]*-[\s:|-]*\|?\s*$`)

// delimCellRe matches one cell of a delimiter row.
var delimCellRe = regexp.MustCompile(`^\s*:?-+:?\s*$`)

// fenceRe matches the opening or closing line of a fenced code block.
//
// **行頭の空白は 3 つまで。** 4 つ以上はインデントされたコードブロックの中身で
// あってフェンスではない。`^\s*` にすると、フェンスの書き方をインデントブロックで
// 見せている doc で「開いたまま閉じない」状態になり、そのファイルの残りが丸ごと
// 未検査になる (= 検査していないのに緑)。現 corpus に該当は無いが、仕様どおりに
// しておく方が安い。
var fenceRe = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")

// TestMarkdownTablesDoNotDropContent asserts that no Markdown table row has
// more cells than its header, which is how GFM silently drops content (#2930).
//
// **GFM は列が増えた行を「崩して描画」しない。溢れたセルを黙って捨てる。**
// ヘッダ行が列数を決め、それを超えたセルは破棄されるので、**ソースには
// 書いてあるのに GitHub 上では読めない**という形で壊れる。ローカルで md を
// 読んでいる限り気付けない。
//
// 実際に踏んだのは `docs/divergence.md` の `2026.9.0-mk.7` の行で、コードスパンの
// 中に書いた権限式の `||` がセル区切りとして働き、**描画は 599 文字あるべき
// ところ 394 文字で止まって 205 文字 (34.2%) が読めなかった** (文字数は
// `gh api /markdown --mode gfm` の出力からタグを除いて数えた)。消えた中に
// 「純正へは還元できない行」という分類が入っており、この表を「還元不能な差分の
// 一覧」として読む運用が成立していなかった。
//
// **コードスパンの中でもパイプは区切りとして働く。** GFM のエスケープ
// (`\|` → `|`) は inline の解析より**前**に効くので、表セルの中では
// “ `a \|\| b` “ と書けば区切りにならずコード中の `||` になる。リテラルの `\|` を
// 見せたいときは “ `a \\| b` “。**表の外にはこの前処理が無い**ので、コードスパンに
// `\|` と書くとバックスラッシュがそのまま出る。
//
// **セル不足も見る。** GFM は足りない分を空セルで埋めるので描画は壊れないが、
// 書いたつもりの列が消えているのは同じこと。実測では 0 件。
//
// # 意図的に見ていないもの
//
// **列数が一致したままコードスパンが割れる形は捕まらない。** 3 列の表に
// `| ` + "`x|y`" + ` | z |` と書くと、コードスパンが割れてバッククォートが露出する
// のにセル数はヘッダと同じ 3 になる (実測)。これを捕まえるにはコードスパンの
// 対応付けを自前で持つ必要があり、**実装したところ CommonMark と食い違って正当な
// 行を落とした** (“ | “x`y“ | c`d | “ が偽陽性、#2930 のレビュー 2 周目)。
// 自前の inline パーサに継ぎ足す形は #2857 が「手当てするたびに隣の穴が開く」と
// 結論した型なので、ここでは**列数という 1 つの条件だけ**を見る。
//
// **外側のパイプを省いた表と、ヘッダ行自体が壊れた表も見ていない。** 前者は
// tableRowRe のコメント、後者はヘッダの列数が区切り行と合わないと GFM が表として
// 描画しないため (段落として全文が出るので、消えるのではなく明らかに崩れる)。
func TestMarkdownTablesDoNotDropContent(t *testing.T) {
	root := repoRoot(t)
	files := trackedMarkdownFiles(t, root)

	var problems []string
	tables := 0
	for _, rel := range files {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		require.NoError(t, err, "read %s", rel)

		lines := strings.Split(string(raw), "\n")
		for i := range lines {
			lines[i] = strings.TrimSuffix(lines[i], "\r")
		}

		n, found := scanMarkdownTables(rel, lines)
		tables += n
		problems = append(problems, found...)
	}

	// **1 つも読めなかったら落とす。** 書式が変わって表を拾えなくなると、
	// 検査していないのに緑になる。
	require.NotZerof(t, tables, "tracked な md から表を 1 つも拾えていない (検査していないのに緑になる)")

	// **全件出す。** 1 件目で止めると、複数壊れているときに往復が増える。
	require.Emptyf(t, problems, `md の表が描画時に内容を落とす。

**GFM は溢れたセルを黙って捨てる** (足りない分は空セルで埋める) ので、ソースに
書いた内容が GitHub 上で読めなくなる。原因はほぼ**セル区切りとして働くパイプ**で、
**コードスパンの中でも働く**。表セルの中では `+"`\\|`"+` へエスケープすること
(コード中の `+"`||`"+` を見せたいなら `+"`` `a \\|\\| b` ``"+`、リテラルの
`+"`\\|`"+` を見せたいなら `+"`` `a \\\\| b` ``"+`)。

%s`, strings.Join(problems, "\n"))
}

// scanMarkdownTables returns the number of tables in lines and every row whose
// cell count differs from its header.
//
// **表は「先頭パイプの行が続く間」とする。** GFM はパイプを含まない行も表の行に
// するが、そこまで追うには全ブロックの開始判定が要り、リスト直後の表で偽陽性を
// 出した (tableRowRe のコメント)。取りこぼす側に倒してある。
func scanMarkdownTables(rel string, lines []string) (int, []string) {
	var problems []string
	tables := 0
	fence := ""
	inHTMLComment := false
	header := -1
	headerLine := 0

	for i, line := range lines {
		if inHTMLComment {
			if strings.Contains(line, "-->") {
				inHTMLComment = false
			}
			continue
		}
		if fence != "" {
			if m := fenceRe.FindStringSubmatch(line); m != nil &&
				m[1][0] == fence[0] && len(m[1]) >= len(fence) && strings.TrimSpace(m[2]) == "" {
				fence = ""
			}
			continue
		}
		if m := fenceRe.FindStringSubmatch(line); m != nil {
			fence = m[1]
			header = -1
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "<!--") {
			// コメントの中の表は描画されないので検査しない。
			if !strings.Contains(line, "-->") {
				inHTMLComment = true
			}
			header = -1
			continue
		}
		if !tableRowRe.MatchString(line) {
			header = -1
			continue
		}

		if header < 0 {
			// 表の開始: 次の行が区切りなら、この行がヘッダ。
			if i+1 < len(lines) && isDelimiterRow(lines[i+1]) &&
				len(splitTableCells(line)) == len(splitTableCells(lines[i+1])) {
				header = len(splitTableCells(line))
				headerLine = i + 1
				tables++
			}
			continue
		}
		if i == headerLine && isDelimiterRow(line) {
			continue
		}
		// ヘッダ直後の区切り行だけを飛ばす。本文の `| - | - |` は普通の行。
		if i == headerLine+1 && isDelimiterRow(line) {
			continue
		}

		if n := len(splitTableCells(line)); n != header {
			problems = append(problems, fmt.Sprintf(
				"%s:%d は %d 列だが、%s:%d のヘッダは %d 列", rel, i+1, n, rel, headerLine, header))
		}
	}
	return tables, problems
}

// isDelimiterRow reports whether line is a `|---|:--:|` delimiter row.
//
// セルごとに `:?-+:?` を要求する。空セル (`| --- | |`) を含む行を GFM は表として
// 認めないので、通すと表でないものを表として検査する。
func isDelimiterRow(line string) bool {
	if !tableRowRe.MatchString(line) || !tableDelimRe.MatchString(line) {
		return false
	}
	for _, c := range splitTableCells(line) {
		if !delimCellRe.MatchString(c) {
			return false
		}
	}
	return true
}

// splitTableCells splits one GFM table row into its cells.
//
// cmark-gfm と同じ手順にしてある。**先頭のパイプを 1 つ、末尾の未エスケープな
// パイプを 1 つ落としてから**、未エスケープのパイプで切る (外側のパイプは省略
// できる仕様なので、空セルとして数えない)。
//
// **エスケープの判定は「直前の 1 文字が `\` か」だけ。** バックスラッシュの偶奇を
// 数える実装にすると `\\|` を区切りとして数えるが、GFM は数えない (実測で
// `| a | b\\|c |` は 2 セルで `b|c` と描画される)。正規表現やパスを載せる表で
// 誤検出する。
func splitTableCells(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "|")
	if strings.HasSuffix(s, "|") && !(len(s) >= 2 && s[len(s)-2] == '\\') {
		s = s[:len(s)-1]
	}

	var cells []string
	var cur strings.Builder
	var prev byte
	for i := 0; i < len(s); i++ {
		if s[i] == '|' && prev != '\\' {
			cells = append(cells, cur.String())
			cur.Reset()
		} else {
			cur.WriteByte(s[i])
		}
		prev = s[i]
	}
	return append(cells, cur.String())
}

// trackedMarkdownFiles returns every tracked `*.md` path, relative to root.
//
// **`git ls-files` で見る** (`migrationdoc-check` と同じ理由、#2857)。ディスクを
// 走査すると、`git add` を忘れた新規 doc が手元では検査されて CI で初めてずれる。
// **`frontend/` は除く** (#3379)。本家から取り込んだ木で、md も本家のもの
// (`locales/README.md`、misskey-js の API レポートなど)。本家の書き方で落ちると、
// 追従のたびに他人の書いた表を直すことになる。
func trackedMarkdownFiles(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "*.md")
	cmd.Dir = root
	out, err := cmd.Output()
	require.NoError(t, err, "git ls-files '*.md' が失敗した")

	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" && !strings.HasPrefix(line, "frontend/") {
			files = append(files, line)
		}
	}
	require.NotEmpty(t, files, "tracked な md が 1 つも見つからない (検査していないのに緑になる)")
	sort.Strings(files)
	return files
}
