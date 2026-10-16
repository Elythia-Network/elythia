package entitycompat

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// catalogFrom matches a FROM / JOIN / comma-separated FROM entry on a catalog
// view whose rows span schemas.
//
// `pg_indexes_size(` のような関数名を拾わないよう単語境界を要求する。
// `pg_catalog.` の修飾、`"pg_indexes"` のような引用符、JOIN、カンマで並べた FROM でも拾う。FROM の素の
// 名前だけを見ていると、普通に書ける別の綴りで検査を避けられてしまう。
//
// **`information_schema.schemata` は入れない。** あれは schema の一覧そのもので、
// 自分の schema に絞る概念が無い (`pluginstore` が plugin 用 schema を列挙するのに
// 使っている)。`pg_class` / `pg_attribute` も対象外 — join の書き方が多様で単純な
// 述語検査に乗らないため。規則は CLAUDE.md / docs/testing.md 側に書いてある。
var catalogFrom = regexp.MustCompile(tableRefLead + pgCatalogPrefix +
	`"?(?:pg_indexes\b|information_schema"?\s*\.\s*"?(?:columns|tables)\b)`)

// tableRefLead is what precedes a table reference: FROM, JOIN, or the comma of
// an implicit join (`FROM pg_class c, pg_indexes i`).
const tableRefLead = `(?i)(?:\b(?:from|join)\s+|,\s*)`

// pgCatalogPrefix is an optional `pg_catalog.` qualifier, quoted or not.
const pgCatalogPrefix = `(?:"?pg_catalog"?\s*\.\s*)?`

// scopedByPredicate matches a schema filter used as a predicate.
//
// **SELECT 句に列名があるだけでは不十分。** 単に `schemaname` を含むかで見ると
// `SELECT schemaname, indexdef FROM pg_indexes WHERE tablename = ?` が通って
// しまう (実際にこの gate の初版がその変異を見逃した)。列名は catalog によって
// 違う (`pg_indexes` は schemaname、`information_schema.*` は table_schema、
// `pg_namespace` を join する形は nspname)。
var scopedByPredicate = regexp.MustCompile(`(?i)\b(schemaname|table_schema|nspname)\s*(=|in\b)`)

// unscopedOptOut lets a query opt out when it deliberately looks across schemas.
//
// **理由を同じ行に書かせる。** `\s` は改行を含むので `\s*\S` だと次の行の
// 先頭にマッチしてしまい、理由なしのマーカーを通す (初版がそうだった)。
// 改行を除いた空白 `[^\S\n]` で受ける。
var unscopedOptOut = regexp.MustCompile(`--[^\S\n]*unscoped:[^\S\n]*\S`)

// indexdefCandidate matches a literal that can evaluate pg_get_indexdef: the
// `indexdef` column of pg_indexes, or a direct call to pg_get_indexdef.
var indexdefCandidate = regexp.MustCompile(tableRefLead + pgCatalogPrefix +
	`"?pg_indexes\b|\bpg_get_indexdef\s*\(`)

// whereKeyword marks where the predicate part of a query starts.
var whereKeyword = regexp.MustCompile(`(?i)\bwhere\b`)

// indexdefRef matches `indexdef` or `pg_get_indexdef` as a word.
var indexdefRef = regexp.MustCompile(`(?i)\b(pg_get_)?indexdef\b`)

// indexdefInWhere reports whether `indexdef` / `pg_get_indexdef` appears after
// the first WHERE of the query.
//
// #3365: `indexdef` の条件は index の pg_class だけを参照するので、plan によっては
// その scan まで押し下げられ、`schemaname = current_schema()` で絞る前に全 schema の
// index で `pg_get_indexdef` が評価される。並行する他パッケージの DDL がその index を
// 消していると `could not open relation with OID` で落ちる。schema で絞っても防げない
// ので、絞りの検査とは別に見る。`-- unscoped:` の opt-out も効かせない。
//
// 1 つのクエリの中の構文 (句の範囲、別名、CTE、副問い合わせ) は解析しない。最初の
// WHERE より後ろに語として現れたら違反にする。ORDER BY や副問い合わせの SELECT 句に
// 書いた形も落ちるが、誤検知は赤になって気付けるのに対し、取りこぼしは緑のまま残る
// ので、広く取る側に倒す。自前で読むのは、migration のファイルをコメントと文に
// 切り分ける字句の段だけ (理由は splitMigrationSQL)。
//
// 既知の限界: CTE や副問い合わせの SELECT 句で定義に別名を付け (`indexdef AS d`)、
// 外側の WHERE で別名を比べる形は拾わない。最初の WHERE より前に `indexdef` が
// 出るため。PG18 の実測では、MATERIALIZED でない CTE も副問い合わせもインライン
// 化され、`pg_get_indexdef(i.oid) ~~ ...` が index の pg_class の scan の Filter に
// 押し下げられる (#3365 と同じ形) ので、安全な形ではない。docs が勧める
// `MATERIALIZED` の有無も見ない (別名を付ければどちらも通り、`indexdef` のまま
// 外側の WHERE で比べればどちらも落ちる)。
func indexdefInWhere(sql string) bool {
	loc := whereKeyword.FindStringIndex(sql)
	return loc != nil && indexdefRef.MatchString(sql[loc[1]:])
}

// requiredCatalogFiles are real files whose catalog queries the gate must see.
//
// 抽出の下限。検査した件数が 0 でないことだけを見ると、抽出が一部だけ空振りしても
// 気付けない。#3365 で直した user_uri_index_test.go と、pg_get_indexdef を直接呼ぶ
// migration のテストを名指しで要求する。
var requiredCatalogFiles = map[string]string{
	"internal/repository/user_uri_index_test.go":                "pg_indexes",
	"internal/repository/index_lookup_test.go":                  "pg_indexes",
	"internal/repository/migration_note_reaction_index_test.go": "pg_get_indexdef",
	// internal/repository の外。列挙を repository に狭める変異を落とすため。
	"internal/pluginstore/pluginstore_test.go": "catalog",
	// migration の SQL (#3369)。pg_indexes を引いて定義を比べる唯一の migration。
	"migration/000068_drop_redundant_indexes.up.sql": "pg_get_indexdef",
}

// minListedMigrationFiles is a lower bound on the migration files git ls-files
// returns.
//
// 2026-10-16 時点で 264 件 (`git ls-files -- 'migration/*.sql' | wc -l`)。
const minListedMigrationFiles = 200

// minListedGoFiles is a lower bound on the files git ls-files returns.
//
// 名指しのファイルが残る形に列挙を狭めても気付けるよう、件数にも下限を置く。
// 2026-10-16 時点で 2560 件 (`git ls-files -- 'internal/*.go' | wc -l`)。
const minListedGoFiles = 1000

// gateOwnFile is skipped because its fixtures are deliberately unscoped queries.
//
// **除外したファイルに実クエリを書かないこと。** ここだけは gate の外にある。
const gateOwnFile = "internal/entitycompat/catalog_query_test.go"

// #2777: `pg_indexes` や `information_schema.*` は **schema を跨いで全件返す**。
// テスト DB は #2450 でパッケージごとに schema を分けているので、絞らないと
// 2 つ壊れる:
//
//   - 他 schema の同名オブジェクトを自分のものと取り違え、migration が適用されて
//     いなくても regression guard が緑になる (実測で 3 テストが空振りしていた —
//     `internal_repository_ts` の index 定義が返っていた)
//   - 他パッケージの `ApplyMigrations` が DDL 中だと
//     `could not open relation with OID (SQLSTATE XX000)` で落ちる。**CI の
//     required check (`test`) が不定期に赤くなる** (PR #2778 で実際に踏んだ)
//
// doc に書くだけだと再発する (17-19 schema ある限り条件は揃ったまま) ので、
// 静的に検出する。
//
// **文字列の切り出しは `go/parser` で行う。** backtick を機械的にペアリングする
// 初版は、コメントや rune literal に含まれる backtick でずれ、**gate 自身の
// ファイルで既にずれていた** (backtick 41 個 = 奇数)。ずれると「どのチャンクにも
// 入らない」クエリができて黙って無検査になる。
func TestCatalogQueriesAreSchemaScoped(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	checked := 0
	// 名指しで要求したファイルごとに、実際に見た検査の種類を記録する。
	seen := map[string]map[string]bool{}

	// ディスクを走査すると git add し忘れたファイルが手元でだけ検査されるので、
	// git ls-files で列挙する。
	out, err := exec.Command("git", "-C", root, "ls-files", "--", "internal/*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files internal/: %v", err)
	}
	files := strings.Fields(string(out))
	if len(files) < minListedGoFiles {
		t.Fatalf("git ls-files が internal/ の .go を %d 件しか返さない (下限 %d)。列挙が狭まっている",
			len(files), minListedGoFiles)
	}
	for _, rel := range files {
		if rel == gateOwnFile {
			continue
		}
		kinds, findings, perr := scanCatalogSource(fset, filepath.Join(root, filepath.FromSlash(rel)), nil)
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
		}
		for _, k := range kinds {
			markSeen(seen, rel, k)
			if k == "catalog" {
				checked++
			}
		}
		for _, fd := range findings {
			t.Errorf("%s:%d: %s\n  query: %s", rel, fd.line, fd.problem, fd.sql)
		}
	}
	// migration の SQL も見る (#3369)。テストでは各パッケージの schema に並行して
	// 流れるので、Go のクエリと同じく並行する DDL と重なる。コメントを外し、文に
	// 分けてから、文ごとに判定する。
	out, err = exec.Command("git", "-C", root, "ls-files", "--", "migration/*.sql").Output()
	if err != nil {
		t.Fatalf("git ls-files migration/: %v", err)
	}
	migrations := strings.Fields(string(out))
	if len(migrations) < minListedMigrationFiles {
		t.Fatalf("git ls-files が migration/ の .sql を %d 件しか返さない (下限 %d)。列挙が狭まっている",
			len(migrations), minListedMigrationFiles)
	}
	for _, rel := range migrations {
		src, rerr := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if rerr != nil {
			t.Fatalf("read %s: %v", rel, rerr)
		}
		kinds, findings := scanMigrationSQL(string(src))
		for _, k := range kinds {
			markSeen(seen, rel, k)
			if k == "catalog" {
				checked++
			}
		}
		for _, fd := range findings {
			t.Errorf("%s:%d: %s\n  query: %s", rel, fd.line, fd.problem, fd.sql)
		}
	}
	// 列挙が空なら「検査していないのに緑」になる。
	if checked == 0 {
		t.Fatal("カタログを引くクエリが 1 つも見つからない。gate が空振りしている")
	}
	for file, kind := range requiredCatalogFiles {
		if !seen[file][kind] {
			t.Errorf("%s に %s を引くクエリが見つからない。抽出が空振りしているか、"+
				"ファイルが変わったなら requiredCatalogFiles を直すこと", file, kind)
		}
	}
}

// judgeCatalogQuery returns which checks apply to sql ("pg_get_indexdef" for
// the #3365 check, "catalog" and "pg_indexes" for the #2777 check) and the
// violations found.
//
// 1 つのリテラルの判定をここにまとめ、gate の判定の配線ごと単体で固定する。
// 走査の側に判定を書くと、違反を報告する行を消す変異が、実際の違反が無い限り
// 素通りする (実測で素通りした)。
func judgeCatalogQuery(sql string) (kinds, problems []string) {
	if indexdefCandidate.MatchString(sql) {
		kinds = append(kinds, "pg_get_indexdef")
		if indexdefInWhere(sql) {
			problems = append(problems, "`indexdef` / `pg_get_indexdef` を WHERE に書いている (#3365)。\n"+
				"  schema で絞る前に全 schema の index で評価され、並行する DDL で\n"+
				"  `could not open relation with OID` になる。定義は SELECT で取り、Go の側で見ること。\n"+
				"  ORDER BY のように、絞った後に読む安全な形も落とす (WHERE より後ろを広く見るため)。\n"+
				"  その場合も SELECT 句で読み、並べ替えや絞り込みは Go の側で行うこと。\n"+
				"  CTE や副問い合わせで読んだ定義を外側の WHERE で比べる形は安全ではない (MATERIALIZED で\n"+
				"  ない CTE と副問い合わせはインライン化され、条件が index の scan まで押し下げられる)。")
		}
	}
	if !catalogFrom.MatchString(sql) {
		return kinds, problems
	}
	kinds = append(kinds, "catalog")
	if strings.Contains(strings.ToLower(sql), "pg_indexes") {
		kinds = append(kinds, "pg_indexes")
	}
	if !scopedByPredicate.MatchString(sql) && !unscopedOptOut.MatchString(sql) {
		problems = append(problems, "カタログを schema で絞っていないクエリがある (#2777)。\n"+
			"  `schemaname` / `table_schema` = current_schema() を WHERE に足すこと。\n"+
			"  意図的に schema を跨ぐ場合は SQL に `-- unscoped: <理由>` を書く。")
	}
	return kinds, problems
}

// catalogFinding is one violation found in a Go source file.
type catalogFinding struct {
	line    int
	problem string
	sql     string
}

// scanCatalogSource judges every string literal in a Go source, and every `+`
// chain whose leaves are all string literals as one concatenated query.
// src follows parser.ParseFile (nil reads filename).
//
// 長いクエリを `"..." +` で分けて書くと、1 つ目のリテラルに FROM、2 つ目に WHERE が
// 入り、リテラル単位では #3365 の形を見落とす。葉がすべて文字列の連結は、つないで
// から判定する。変数を挟む連結はつながず、リテラルごとに判定する。
func scanCatalogSource(fset *token.FileSet, filename string, src any) (kinds []string, findings []catalogFinding, err error) {
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, nil, err
	}
	judge := func(pos token.Pos, sql string) {
		k, problems := judgeCatalogQuery(sql)
		kinds = append(kinds, k...)
		for _, p := range problems {
			findings = append(findings, catalogFinding{
				line: fset.Position(pos).Line, problem: p,
				sql: strings.Join(strings.Fields(sql), " "),
			})
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BinaryExpr:
			if sql, ok := concatStringLits(n); ok {
				judge(n.Pos(), sql)
				return false
			}
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return true
			}
			if sql, uerr := strconv.Unquote(n.Value); uerr == nil {
				judge(n.Pos(), sql)
			}
		}
		return true
	})
	return kinds, findings, nil
}

// scanMigrationSQL judges each statement of a migration file as one query,
// after removing its SQL comments.
//
// コメントを外すのは、migration の冒頭の説明に `WHERE` や `indexdef` が地の文で
// 出るため (000068 がそう)。外さないと、説明の `WHERE` の後ろに来る語を拾って落ちる。
// `-- unscoped:` の opt-out はコメントなので残す。ただし `;` の後ろの同じ行に書いた
// ものは捨てる (どの文にも効かせない)。
//
// ファイル全体を 1 つのクエリとして見ると、1 つの文の絞りや opt-out がファイルの
// 全ての文を通し、前の文の `WHERE` の後ろに来た安全な文を誤検知する (#3369 の
// レビュー)。migration は DO ブロックに複数のクエリを並べるのが普通の形なので、
// `;` で文に分けてから判定する。読み切れないファイル (引用符などが閉じていない) は
// 取りこぼしを緑にしないよう、それ自体を違反として落とす。
func scanMigrationSQL(src string) (kinds []string, findings []catalogFinding) {
	stmts, stripped, err := splitMigrationSQL(src)
	if err != nil {
		return nil, []catalogFinding{{
			line: err.line,
			problem: "migration の SQL を読み切れない: " + err.msg + "。\n" +
				"  読み違えたまま判定すると検査を取りこぼすので落とす。gate の字句解析 (splitMigrationSQL) を直すこと。",
			sql: "(ファイル全体)",
		}}
	}
	for _, st := range stmts {
		sql := stripped[st.start:st.end]
		k, problems := judgeCatalogQuery(sql)
		kinds = append(kinds, k...)
		if len(problems) == 0 {
			continue
		}
		pos := st.start
		if loc := indexdefCandidate.FindStringIndex(sql); loc != nil {
			pos += loc[0]
		} else if loc := catalogFrom.FindStringIndex(sql); loc != nil {
			pos += loc[0]
		}
		line := 1 + strings.Count(stripped[:pos], "\n")
		for _, p := range problems {
			findings = append(findings, catalogFinding{
				line: line,
				problem: p + "\n  migration では、定義を SELECT で配列などに読み切ってから比べること" +
					" (migration/000068_drop_redundant_indexes.up.sql が例)。\n" +
					"  `-- unscoped:` は、その文の `;` より前に書くこと (`;` の後ろに書いた注記はどの文にも効かない)。",
				sql: strings.TrimSpace(sql),
			})
		}
	}
	return kinds, findings
}

// sqlSpan is one statement of a migration file as a byte range of the text
// splitMigrationSQL returns.
type sqlSpan struct{ start, end int }

// sqlLexError reports where splitMigrationSQL lost track of the quoting.
type sqlLexError struct {
	line int
	msg  string
}

// dollarQuoteOpen matches the opening delimiter of a dollar-quoted string,
// `$$` or `$tag$`.
var dollarQuoteOpen = regexp.MustCompile(`^\$(?:[A-Za-z_\x80-\xff][A-Za-z0-9_\x80-\xff]*)?\$`)

// dollarBodyLead matches the text right before a dollar quote that holds code
// (a DO block or a function body) rather than a string value.
var dollarBodyLead = regexp.MustCompile(`(?i)\b(?:do|as|do\s+language\s+\w+)\s*$`)

// splitMigrationSQL removes the comments of src and splits it into statements
// at the `;` outside quoted strings and identifiers. The returned text keeps
// every newline of src, so a byte offset in it maps to the same line of src.
func splitMigrationSQL(src string) (stmts []sqlSpan, text string, lexErr *sqlLexError) {
	// CLAUDE.md Section 4「自前でパースしない」の例外。PostgreSQL には SQL から字句や
	// 文の境界を返す関数が無く、サーバーに解かせる道が無い (EXPLAIN は plan に依る)。
	// Go のパーサは pg_query_go が cgo (libpg_query の C のビルド) を要り、PL/pgSQL の
	// 本体は別の API で解き直しになる。テストのためだけに重い依存を go.mod に足すより、
	// 対象を「コメント・引用符・文の境界」の字句に限って自前で読み、読み切れないもの
	// (閉じていない引用符など) は推測で続けずに落とす側に倒す。対象は自分たちで書く
	// migration で、ドル引用は DO の本体に使うだけなので、この範囲で足りる。
	//
	// PostgreSQL の字句に合わせて読む (本体の閉じのタグだけは、下の閉じの分岐の
	// とおり PostgreSQL より遅く閉じることがある): '...' は '' を、E'...' はそれに加えて \ の
	// エスケープを持つ。"..." は "" を持つ。ドル引用 ($$...$$、$tag$...$tag$) は、
	// 直前が DO / AS (DO LANGUAGE x を含む) なら PL/pgSQL などの本体として中まで
	// 読み (コメントを外し、`;` で分ける)、それ以外は文字列として中身をそのまま残す。
	// 文字列の中身を消さないのは、EXECUTE に渡す SQL を判定から外さないため。
	// `$1` のような位置パラメータや、識別子の途中の `$` はドル引用として扱わない。
	//
	// 読み違えると引用符の対がずれ、後ろの違反がコメントや文字列に飲まれて緑になる
	// (E'\'' と、ドル引用の中の ' で実際にずれた)。閉じていない引用符・コメント・
	// 本体は、推測で続けずに error を返す。
	//
	// 既知の限界: 文字列として扱ったドル引用の中は、コメントを外さず `;` でも
	// 分けない。DO / AS 以外の位置に置いた本体 (PostgreSQL の文法上は無い) は、
	// 本体として読まない。単一引用符で書いた関数の本体 (`AS '...'` / `AS E'...'`) も
	// 文字列として扱い、本体の中を 1 つの文として見る (中の 1 文目の絞りが 2 文目を
	// 通す)。今の migration には無く、本体はドル引用で書く。
	var b strings.Builder
	start := 0
	var bodies []string // 開いているドル引用の本体のタグ (内側が末尾)
	lineAt := func(i int) int { return 1 + strings.Count(src[:i], "\n") }
	flush := func() {
		if strings.TrimSpace(b.String()[start:]) != "" {
			stmts = append(stmts, sqlSpan{start, b.Len()})
		}
	}
	isIdent := func(c byte) bool {
		return c == '_' || c == '$' || c >= 0x80 || ('0' <= c && c <= '9') ||
			('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
	}
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\'':
			// E'...' は \ でエスケープする。E の前が識別子の文字なら、E は別の語の一部。
			escaped := i > 0 && (src[i-1] == 'E' || src[i-1] == 'e') && (i < 2 || !isIdent(src[i-2]))
			j := i + 1
			for ; j < len(src); j++ {
				if escaped && src[j] == '\\' {
					j++
					continue
				}
				if src[j] == '\'' {
					if j+1 < len(src) && src[j+1] == '\'' {
						j++
						continue
					}
					break
				}
			}
			if j >= len(src) {
				return nil, "", &sqlLexError{lineAt(i), "文字列が閉じていない"}
			}
			b.WriteString(src[i : j+1])
			i = j + 1
		case c == '"':
			j := i + 1
			for ; j < len(src); j++ {
				if src[j] == '"' {
					if j+1 < len(src) && src[j+1] == '"' {
						j++
						continue
					}
					break
				}
			}
			if j >= len(src) {
				return nil, "", &sqlLexError{lineAt(i), "引用符付きの識別子が閉じていない"}
			}
			b.WriteString(src[i : j+1])
			i = j + 1
		case len(bodies) > 0 && strings.HasPrefix(src[i:], bodies[len(bodies)-1]):
			// 開いている本体を閉じる。PostgreSQL は本体も文字列として読むので、閉じの
			// タグは直前の文字に関係なく閉じる (`END$$;` も閉じる)。開きの側だけが
			// 「直前が識別子の文字でない」を要求する。
			// 本体の中の文字列・コメント・引用符付き識別子に現れたタグでは閉じない
			// (読み飛ばすため)。PostgreSQL は本体も文字列として読むのでそこで閉じ、
			// 残りが構文エラーになる。差が出るのは適用できない SQL だけなので、
			// 適用できる migration の判定は変わらない。
			delim := bodies[len(bodies)-1]
			bodies = bodies[:len(bodies)-1]
			b.WriteString(delim)
			i += len(delim)
		case c == '$' && (i == 0 || !isIdent(src[i-1])) && dollarQuoteOpen.MatchString(src[i:]):
			delim := dollarQuoteOpen.FindString(src[i:])
			if dollarBodyLead.MatchString(b.String()[start:]) {
				bodies = append(bodies, delim)
				b.WriteString(delim)
				i += len(delim)
				continue
			}
			end := strings.Index(src[i+len(delim):], delim)
			if end < 0 {
				return nil, "", &sqlLexError{lineAt(i), "ドル引用 " + delim + " の文字列が閉じていない"}
			}
			end += i + 2*len(delim)
			b.WriteString(src[i:end])
			i = end
		case strings.HasPrefix(src[i:], "--"):
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				end = len(src) - i
			}
			// `-- unscoped:` は、その文の中 (`;` より前) に書いたときだけ効かせる。
			// `;` と同じ行の後ろに書いた注記は、見た目では閉じた文に付いているのに、
			// 残すと次の文の opt-out になってしまう (#3369 のレビュー)。どの文にも
			// 効かせず、閉じた文が絞れていなければ赤にする。
			cur := b.String()[start:]
			trailing := start > 0 && !strings.Contains(cur, "\n") && strings.TrimSpace(cur) == ""
			if comment := src[i : i+end]; unscopedOptOut.MatchString(comment) && !trailing {
				b.WriteString(comment)
			}
			i += end
		case strings.HasPrefix(src[i:], "/*"):
			// PostgreSQL のブロックコメントは入れ子にできる。
			depth, j := 1, i+2
			for j < len(src) && depth > 0 {
				switch {
				case strings.HasPrefix(src[j:], "/*"):
					depth++
					j += 2
				case strings.HasPrefix(src[j:], "*/"):
					depth--
					j += 2
				default:
					if src[j] == '\n' {
						b.WriteByte('\n')
					}
					j++
				}
			}
			if depth > 0 {
				return nil, "", &sqlLexError{lineAt(i), "ブロックコメントが閉じていない"}
			}
			b.WriteByte(' ')
			i = j
		case c == ';':
			flush()
			b.WriteByte(';')
			start = b.Len()
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	if len(bodies) > 0 {
		return nil, "", &sqlLexError{lineAt(len(src)), "ドル引用 " + bodies[len(bodies)-1] + " の本体が閉じていない"}
	}
	flush()
	return stmts, b.String(), nil
}

// concatStringLits returns the concatenation when e is a `+` chain of string
// literals only.
func concatStringLits(e ast.Expr) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return concatStringLits(e.X)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		l, ok := concatStringLits(e.X)
		if !ok {
			return "", false
		}
		r, ok := concatStringLits(e.Y)
		return l + r, ok
	}
	return "", false
}

func markSeen(seen map[string]map[string]bool, file, kind string) {
	if seen[file] == nil {
		seen[file] = map[string]bool{}
	}
	seen[file][kind] = true
}

// gate 自身の判定を固定する。**これが無いと gate のロジックを弱める変異を
// 検出できない** — 実測で「列名を含むか」だけを見る初版に戻す変異が素通りした
// (対象クエリの SELECT 句に `schemaname` があるため)。
func TestCatalogQueryGate_Predicates(t *testing.T) {
	// ok = gate が「絞れている / 意図的に絞らない」と判定すること。
	for _, tc := range []struct {
		name string
		sql  string
		ok   bool
	}{
		{"WHERE で絞る", "SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND tablename = $1", true},
		{"IN でも絞れている", "SELECT indexdef FROM pg_indexes WHERE schemaname IN (current_schema())", true},
		{"information_schema は table_schema", "SELECT 1 FROM information_schema.columns WHERE table_schema = current_schema()", true},
		{"pg_catalog で修飾して絞らない", "SELECT indexdef FROM pg_catalog.pg_indexes WHERE tablename = $1", false},
		{"JOIN で引いて絞らない", "SELECT i.indexdef FROM pg_class c JOIN pg_indexes i ON i.indexname = c.relname WHERE i.tablename = $1", false},
		{"カンマで並べた FROM で絞らない", "SELECT i.indexdef FROM pg_class c, pg_indexes i WHERE i.indexname = c.relname", false},
		{"カンマで並べた information_schema で絞らない", "SELECT 1 FROM pg_class c, information_schema.columns col WHERE col.table_name = c.relname", false},
		{"引用符で囲んで絞らない", `SELECT indexdef FROM "pg_indexes" WHERE tablename = $1`, false},
		{"information_schema を引用符で囲んで絞らない", `SELECT 1 FROM "information_schema"."columns" WHERE table_name = $1`, false},
		{"nspname で絞る形", "SELECT 1 FROM information_schema.tables t JOIN x ON 1=1 WHERE nspname = current_schema()", true},
		{"SELECT 句にあるだけでは不足", "SELECT schemaname, indexdef FROM pg_indexes WHERE tablename = $1", false},
		{"絞りが無い", "SELECT indexdef FROM pg_indexes WHERE tablename = $1", false},
		{"information_schema の絞り無し", "SELECT 1 FROM information_schema.columns WHERE table_name = $1", false},
		{"schemata は対象外なので絞り無しでよい", "SELECT schema_name FROM information_schema.schemata WHERE schema_name LIKE $1", true},
		{"opt-out は理由が要る", "SELECT count(*) FROM pg_indexes -- unscoped:\n WHERE tablename = $1", false},
		{"理由付きの opt-out は通る", "SELECT count(*) FROM pg_indexes -- unscoped: 複数 schema を数える\n WHERE tablename = $1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 検査対象から外れているものは、それ自体が「通る」判定。
			if !catalogFrom.MatchString(tc.sql) {
				if !tc.ok {
					t.Fatalf("検査対象として拾えていない: %s", tc.sql)
				}
				return
			}
			got := scopedByPredicate.MatchString(tc.sql) || unscopedOptOut.MatchString(tc.sql)
			if got != tc.ok {
				t.Errorf("judged %v, want %v for: %s", got, tc.ok, tc.sql)
			}
			_, problems := judgeCatalogQuery(tc.sql)
			if (len(problems) == 0) != tc.ok {
				t.Errorf("judgeCatalogQuery problems = %q, want ok=%v for: %s", problems, tc.ok, tc.sql)
			}
		})
	}

	// indexdef を WHERE に書く形 (#3365)。want = 違反として落とすこと。
	for _, tc := range []struct {
		name string
		sql  string
		want bool
	}{
		// #3365 で直す前の user_uri_index_test.go のクエリそのまま。schema で絞っていても落とす。
		{"#3365 の修正前の形", `
		SELECT indexname, indexdef FROM pg_indexes
		WHERE schemaname = current_schema() AND tablename = 'user'
		  AND indexdef LIKE '%USING btree (uri)%'`, true},
		{"#3365 の修正後の形", `
		SELECT indexname, indexdef FROM pg_indexes
		WHERE schemaname = current_schema() AND tablename = 'user'`, false},
		{"大文字でも拾う", "SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() AND INDEXDEF ILIKE '%gin%'", true},
		{"pg_get_indexdef を WHERE で呼ぶ", "SELECT c.relname FROM pg_class c WHERE c.relnamespace = to_regnamespace(current_schema()) AND pg_get_indexdef(c.oid) LIKE '%uri%'", true},
		{"pg_get_indexdef を SELECT で呼ぶだけ", "SELECT pg_get_indexdef(c.oid) FROM pg_class c WHERE c.relnamespace = to_regnamespace(current_schema()) AND c.relname = ?", false},
		{"opt-out があっても落とす", "SELECT indexname FROM pg_indexes -- unscoped: 理由\n WHERE indexdef LIKE '%uri%'", true},
		{"WHERE が無い", "SELECT indexdef FROM pg_indexes", false},
		{"pg_catalog で修飾した形", "SELECT indexname FROM pg_catalog.pg_indexes WHERE schemaname = current_schema() AND indexdef LIKE '%uri%'", true},
		{"JOIN で引く形", "SELECT c.relname FROM pg_class c JOIN pg_indexes i ON i.indexname = c.relname WHERE i.schemaname = current_schema() AND i.indexdef LIKE '%uri%'", true},
		{"カンマで並べた FROM", "SELECT c.relname FROM pg_class c, pg_indexes i WHERE i.indexname = c.relname AND i.schemaname = current_schema() AND i.indexdef LIKE '%uri%'", true},
		{"引用符で囲んだ形", `SELECT indexname FROM "pg_indexes" WHERE schemaname = current_schema() AND indexdef LIKE '%uri%'`, true},
		// 外側の WHERE に indexdef があり、その後ろに副問い合わせの WHERE が続く。
		// 最後の WHERE より後ろだけを見ると見落とす。
		{"外側の WHERE の後ろに副問い合わせの WHERE", "SELECT indexname FROM pg_indexes WHERE indexdef LIKE '%x%' AND indexname IN (SELECT indexname FROM pg_indexes WHERE schemaname = current_schema())", true},
		{"小文字の where", "select indexname from pg_indexes\nwhere schemaname = current_schema() and indexdef like '%x%'", true},
		{"列名の一部は語として拾わない", "SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND indexdef_backup = 1", false},
	} {
		t.Run("indexdef/"+tc.name, func(t *testing.T) {
			if !indexdefCandidate.MatchString(tc.sql) {
				t.Fatalf("indexdef の検査対象として拾えていない: %s", tc.sql)
			}
			if got := indexdefInWhere(tc.sql); got != tc.want {
				t.Errorf("indexdefInWhere = %v, want %v for: %s", got, tc.want, tc.sql)
			}
			// 走査が使う判定の入口でも同じ結論になること。
			_, problems := judgeCatalogQuery(tc.sql)
			reported := false
			for _, p := range problems {
				reported = reported || strings.Contains(p, "#3365")
			}
			if reported != tc.want {
				t.Errorf("judgeCatalogQuery reported #3365 = %v, want %v for: %s", reported, tc.want, tc.sql)
			}
		})
	}

	// 検査対象の切り出し。`pg_indexes_size(...)` は view ではなく関数、
	// `pg_indexes_backup` は別テーブル。どちらも `\b` が無いと誤検知する。
	for _, tc := range []struct {
		sql  string
		want bool
	}{
		{"SELECT indexdef FROM pg_indexes WHERE tablename = $1", true},
		{"SELECT 1 FROM information_schema.columns WHERE table_name = $1", true},
		{"SELECT pg_indexes_size(c.oid) FROM pg_class c", false},
		{"SELECT * FROM pg_indexes_backup", false},
		{"SELECT * FROM information_schema_backup", false},
		{"SELECT indexdef FROM pg_catalog.pg_indexes", true},
		{"SELECT 1 FROM pg_class c JOIN pg_indexes i ON true", true},
		{`SELECT indexdef FROM "pg_indexes"`, true},
		{`SELECT 1 FROM "information_schema"."tables"`, true},
		{"SELECT * FROM pg_catalog.pg_indexes_backup", false},
		{"SELECT 1 FROM pg_class c, pg_indexes i", true},
		{"SELECT 1 FROM pg_class c,information_schema.tables t", true},
		{"SELECT schema_name FROM information_schema.schemata WHERE schema_name LIKE $1", false},
	} {
		if got := catalogFrom.MatchString(tc.sql); got != tc.want {
			t.Errorf("catalogFrom matched %v, want %v for: %s", got, tc.want, tc.sql)
		}
	}
}

// 文字列の連結で組み立てた SQL も 1 つのクエリとして判定すること (#3369)。
func TestCatalogQueryGate_Concatenation(t *testing.T) {
	const src = `package p

var a = ` + "`SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = current_schema()`" + ` +
	" AND indexdef LIKE '%uri%'"

var b = ("SELECT indexdef FROM pg_indexes " +
	"WHERE schemaname = current_schema()") + " AND tablename = 'user'"

var c = "SELECT indexdef FROM pg_indexes WHERE tablename = " + name

var name = "'user'"

var d = ("SELECT indexname FROM pg_indexes WHERE schemaname = current_schema()") +
	" AND indexdef LIKE '%uri%'"
`
	_, findings, err := scanCatalogSource(token.NewFileSet(), "concat.go", src)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int][]string{}
	for _, fd := range findings {
		tag := "#2777"
		if strings.Contains(fd.problem, "#3365") {
			tag = "#3365"
		}
		got[fd.line] = append(got[fd.line], tag)
	}
	// a: 連結すると #3365 の形。b: 連結すると絞れている安全な形。
	// c: 変数を挟む連結は追わず、リテラル単体で #2777 として落ちる。
	// d: 括弧で囲んだ葉も辿って連結し、#3365 の形として落とす。
	want := map[int][]string{3: {"#3365"}, 9: {"#2777"}, 13: {"#3365"}}
	if len(got) != len(want) || strings.Join(got[3], ",") != "#3365" ||
		strings.Join(got[9], ",") != "#2777" || strings.Join(got[13], ",") != "#3365" {
		t.Errorf("findings by line = %v, want %v", got, want)
	}
}

// migration の SQL は、コメントを外して文に分け、文ごとに判定する (#3369)。
func TestCatalogQueryGate_MigrationSQL(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want []string // 報告される問題の issue 番号 ("lex" は読み切れないこと)
	}{
		{"#3369 で直す前の 000068 の形", `-- 説明の WHERE は地の文
DO $$
DECLARE
    mk_index_names text[] := ARRAY['IDX_a'];
    dup record;
BEGIN
    FOR dup IN
        SELECT mine.indexname FROM pg_indexes mine
        WHERE mine.schemaname = 'public'
          AND mine.indexname = ANY(mk_index_names)
          AND EXISTS (SELECT 1 FROM pg_indexes other
              WHERE other.schemaname = 'public'
                AND other.indexdef = mine.indexdef)
    LOOP
        RAISE NOTICE 'drop %', dup.indexname;
        EXECUTE format('DROP INDEX IF EXISTS public.%I', dup.indexname);
    END LOOP;
END $$;`, []string{"#3365"}},
		{"#3369 で直した後の 000068 の形", `-- 以前は EXISTS の中の WHERE で indexdef を比べていた
DO $$
DECLARE
    idx_names text[];
    idx_defs text[];
    dup record;
BEGIN
    -- WHERE で indexdef を読まない
    SELECT array_agg(indexname ORDER BY indexname), array_agg(indexdef ORDER BY indexname)
      INTO idx_names, idx_defs
      FROM pg_indexes WHERE schemaname = 'public';
    FOR dup IN
        WITH idx AS (SELECT * FROM unnest(idx_names, idx_defs) AS t(indexname, def))
        SELECT indexname FROM idx WHERE def !~ ' WHERE '
    LOOP
        EXECUTE format('DROP INDEX IF EXISTS public.%I', dup.indexname);
    END LOOP;
END $$;`, nil},
		{"ブロックコメントの中は見ない", "SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' /* a /* 入れ子 */ AND indexdef LIKE '%x%' */;", nil},
		{"文字列の中の -- で行の残りを捨てない", "SELECT indexname FROM pg_indexes WHERE schemaname = 'public' AND indexname <> '--' AND indexdef LIKE '%x%';", []string{"#3365"}},
		{"識別子の中の -- で行の残りを捨てない", `SELECT indexname FROM pg_indexes WHERE schemaname = 'public' AND "a--b" IS NULL AND indexdef LIKE '%x%';`, []string{"#3365"}},
		{"引用符を重ねた文字列", "SELECT indexname FROM pg_indexes WHERE schemaname = 'public' AND indexname <> 'it''s --' AND indexdef LIKE '%x%';", []string{"#3365"}},
		{"絞りが無い", "SELECT indexname FROM pg_indexes WHERE tablename = 'user';", []string{"#2777"}},
		{"絞りがコメントの中にしか無い", "SELECT indexname FROM pg_indexes WHERE tablename = 'user'; -- schemaname = 'public'", []string{"#2777"}},
		{"理由付きの opt-out はコメントでも残す", "SELECT indexname FROM pg_indexes -- unscoped: 全 schema を数える\nWHERE tablename = 'user';", nil},
		{"pg_get_indexdef を WHERE で呼ぶ", "SELECT 1 FROM pg_class c WHERE c.relnamespace = to_regnamespace(current_schema()) AND pg_get_indexdef(c.oid) LIKE '%x%';", []string{"#3365"}},

		// 文ごとに判定する (M-1)。
		{"絞った文の後の、絞っていない文", "SELECT 1 FROM pg_indexes WHERE schemaname = 'public';\nSELECT indexname FROM pg_indexes WHERE tablename = 'note';", []string{"#2777"}},
		{"DO の本体の中も文ごとに分ける", "DO $$ BEGIN\n  PERFORM 1 FROM pg_indexes WHERE schemaname = 'public';\n  PERFORM 1 FROM pg_indexes WHERE tablename = 'note';\nEND $$;", []string{"#2777"}},
		{"opt-out は次の文に効かない", "SELECT 1 FROM pg_indexes -- unscoped: 全 schema を数える\n;\nSELECT indexname FROM pg_indexes WHERE tablename = 'note';", []string{"#2777"}},
		{"前の文の WHERE で、後の安全な文を誤検知しない", "UPDATE note SET x = 1 WHERE id = 'a';\nSELECT indexdef FROM pg_indexes WHERE schemaname = 'public';", nil},

		// 引用符の対をずらさない (L-1)。
		{"E 文字列の \\' で文字列を閉じない", `SELECT indexname FROM pg_indexes WHERE schemaname = 'public' AND indexname <> E'a\'b--c' AND indexdef LIKE '%x%';`, []string{"#3365"}},
		{"E 文字列の中の '' の後の \\' で文字列を閉じない", `SELECT indexname FROM pg_indexes WHERE schemaname = 'public' AND indexname <> E'it''s \' --' AND indexdef LIKE '%x%';`, []string{"#3365"}},
		{"E でない文字列の \\ はエスケープではない", `SELECT indexname FROM pg_indexes WHERE schemaname = 'public' AND indexname <> 'a\' AND indexdef LIKE '%x%';`, []string{"#3365"}},
		{"ドル引用の文字列の中の ' で対をずらさない", "COMMENT ON TABLE note IS $c$it's$c$;\nSELECT indexname FROM pg_indexes WHERE schemaname = 'public' AND indexname <> '--' AND indexdef LIKE '%x%';", []string{"#3365"}},
		{"$$ の文字列の中の ; で文を分けない", "SELECT $$a; WHERE$$, indexdef FROM pg_indexes WHERE schemaname = 'public';", []string{"#3365"}},
		{"タグ付きの本体と、その中のドル引用の文字列", "DO $body$ BEGIN\n  EXECUTE $q$it's$q$;\n  PERFORM 1 FROM pg_indexes WHERE tablename = 'note';\nEND $body$;", []string{"#2777"}},
		{"位置パラメータはドル引用ではない", "PREPARE p AS SELECT indexname FROM pg_indexes WHERE schemaname = $1 AND indexdef LIKE '%x%';", []string{"#3365"}},
		{"閉じていない文字列は落とす", "SELECT indexname FROM pg_indexes WHERE schemaname = 'public;", []string{"lex"}},
		{"閉じていないドル引用は落とす", "COMMENT ON TABLE note IS $c$it's;\nSELECT indexname FROM pg_indexes WHERE tablename = 'note';", []string{"lex"}},
		{"閉じていない本体は落とす", "DO $$ BEGIN PERFORM 1; END;", []string{"lex"}},
		{"閉じていないブロックコメントは落とす", "SELECT 1; /* a\nSELECT indexname FROM pg_indexes WHERE tablename = 'note';", []string{"lex"}},

		// 字句の分岐をそれぞれ固定する。
		{"AS の後のドル引用は関数の本体として文に分ける", "CREATE FUNCTION f() RETURNS void LANGUAGE plpgsql AS $$ BEGIN\n  PERFORM 1 FROM pg_indexes WHERE schemaname = 'public';\n  PERFORM 1 FROM pg_indexes WHERE tablename = 'note';\nEND $$;", []string{"#2777"}},
		{"DO LANGUAGE x の後のドル引用も本体として文に分ける", "DO LANGUAGE plpgsql $$ BEGIN\n  PERFORM 1 FROM pg_indexes WHERE schemaname = 'public';\n  PERFORM 1 FROM pg_indexes WHERE tablename = 'note';\nEND $$;", []string{"#2777"}},
		{"直前に空白の無い閉じ (END$$) で本体を閉じる", "DO $$ BEGIN\n  PERFORM 1 FROM pg_indexes WHERE schemaname = 'public';\n  PERFORM 1 FROM pg_indexes WHERE tablename = 'note';\nEND$$;", []string{"#2777"}},
		{"直前に空白の無い閉じ (END$$) の本体を読み切る", "DO $$ BEGIN PERFORM 1; END$$;", nil},
		{"ブロックコメントは空白に置き換える", "SELECT 1 FROM/**/pg_indexes WHERE tablename = 'x';", []string{"#2777"}},
		{"小文字の e 文字列も \\ でエスケープする", `SELECT indexname FROM pg_indexes WHERE schemaname = 'public' AND indexname <> e'a\'b--c' AND indexdef LIKE '%x%';`, []string{"#3365"}},
		{"識別子の末尾の E の後の ' は E 文字列ではない", `SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND x = ELSE'a\' AND indexdef LIKE 'x';`, []string{"#3365"}},
		{"識別子の途中の $tag$ はドル引用ではない", "SELECT x$y$, indexname FROM pg_indexes WHERE schemaname = 'public' AND indexdef LIKE '%x%';", []string{"#3365"}},
		{"; の後ろの同じ行の opt-out は次の文に効かない", "SELECT 1 FROM pg_indexes WHERE schemaname = 'public'; -- unscoped: 全 schema を数える\nSELECT indexname FROM pg_indexes WHERE tablename = 'note';", []string{"#2777"}},
		{"前の文の次の行に書いた opt-out は、その後の文に効く", "SELECT 1;\n-- unscoped: 全 schema を数える\nSELECT indexname FROM pg_indexes WHERE tablename = 'note';", nil},
		{"; の後ろの opt-out は閉じた文にも効かない", "SELECT indexname FROM pg_indexes WHERE tablename = 'note'; -- unscoped: 全 schema を数える", []string{"#2777"}},
		{"同じ行で ; の後に始めた文に書いた opt-out は、その文に効く", "SELECT 1; SELECT x FROM pg_indexes -- unscoped: 全 schema を数える\nWHERE tablename = 'n';", nil},
		{"ファイル冒頭の opt-out は、最初の文に効く", "-- unscoped: 全 schema を数える\nSELECT x FROM pg_indexes WHERE tablename = 'n';", nil},
		{"閉じていない引用符付きの識別子は落とす", "SELECT \"a FROM pg_indexes WHERE tablename = 'n';", []string{"lex"}},
		{"入れ子の本体は内側のタグで閉じる", "DO $b$ BEGIN\n  CREATE FUNCTION f() RETURNS void LANGUAGE plpgsql AS $f$ BEGIN PERFORM 1; END $f$;\n  PERFORM 1 FROM pg_indexes WHERE tablename = 'n';\nEND $b$;", []string{"#2777"}},
		{"識別子の中の $ の後の $tag$ はドル引用ではない", "SELECT a$$b$$, indexname FROM pg_indexes WHERE schemaname = 'public' AND indexdef LIKE '%x%';", []string{"#3365"}},
		{"非 ASCII の識別子の末尾の E の後の ' は E 文字列ではない", `SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND x = 日本E'a\' AND indexdef LIKE 'x';`, []string{"#3365"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, findings := scanMigrationSQL(tc.sql)
			var got []string
			for _, fd := range findings {
				tag := "#2777"
				switch {
				case strings.Contains(fd.problem, "#3365"):
					tag = "#3365"
				case strings.Contains(fd.problem, "読み切れない"):
					tag = "lex"
				}
				got = append(got, tag)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("findings = %v, want %v\n%+v", got, tc.want, findings)
			}
		})
	}

	// 報告する行は、その文の中で catalog を引く位置にする。コメントを外しても
	// 改行は残すので、元のファイルの行番号と一致する。
	_, findings := scanMigrationSQL("-- 1\n/* 2\n3 */\nSELECT 1\nFROM pg_indexes WHERE indexdef LIKE '%x%' AND schemaname = 'public';\n" +
		"SELECT 1 FROM pg_indexes WHERE schemaname = 'public';\n'7\n';\nSELECT 1 FROM\n pg_indexes WHERE tablename = 'x';")
	var lines []int
	for _, fd := range findings {
		lines = append(lines, fd.line)
	}
	if fmt.Sprint(lines) != "[5 9]" {
		t.Errorf("finding lines = %v, want [5 9]\n%+v", lines, findings)
	}
}
