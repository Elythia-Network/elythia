package entitycompat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestSchemaDrift_CreateOnlyColumns guards against a drop-in-only failure mode.
//
// mk-go の migration が列を `CREATE TABLE IF NOT EXISTS` の中でしか定義して
// いない場合、その CREATE は **Misskey TS が既に作ったテーブルに対しては
// no-op** になる。upstream にも存在する列なら TS 側が作っているので問題ない
// が、upstream に無い列は TS 製 DB にだけ生えず、mk-go がその列を読み書き
// すると drop-in 環境でのみ
// `column "..." of relation "..." does not exist` で落ちる。
//
// 実際に app.createdAt / auth_session.createdAt / clip.notesCount の 3 本が
// この形で紛れ込んでいた (#2243)。`ALTER TABLE ... ADD COLUMN` で追加した列は
// 両方の shape で冪等に効くので対象外。
//
// golden snapshot は `go run ./tools/schemadrift` で再生成する
// (UPSTREAM_MISSKEY_VERSION を上げたら実行すること)。
func TestSchemaDrift_CreateOnlyColumns(t *testing.T) {
	upstream := loadUpstreamColumns(t)
	createOnly, alterAdded, _ := parseMigrations(t)

	var findings []string
	for table, cols := range createOnly {
		up, known := upstream[table]
		if !known {
			// upstream に対応 entity が無いテーブル (chart 系 / mk-go 独自
			// テーブル)。TS が作ることは無いので CREATE は必ず実際に走る。
			continue
		}
		for _, col := range cols {
			if alterAdded[table][col] {
				continue // ALTER で冪等追加済 = drop-in 安全
			}
			if up[col] {
				continue // upstream にもある = TS 製 DB でも存在する
			}
			if _, ok := createOnlyAllowlist[table+"."+col]; ok {
				continue
			}
			findings = append(findings, table+"."+col)
		}
	}
	sort.Strings(findings)

	if len(findings) > 0 {
		t.Errorf(`CREATE TABLE 内でしか定義されていない upstream 非存在カラムが %d 件:
  %s

これらは Misskey TS が作った DB には生えないため、mk-go が読み書きすると
drop-in 環境でのみ失敗する。次のいずれかで解消すること:
  (a) その列への依存を外す (upstream に無い = 本来不要なはず。#2243 の方針)
  (b) どうしても必要なら ALTER TABLE ... ADD COLUMN IF NOT EXISTS の
      migration を足す (000039_dropin_compat.up.sql と同じ方式)`,
			len(findings), strings.Join(findings, "\n  "))
	}
}

// createOnlyAllowlist は「fresh な mk-go DB にだけ存在するが、mk-go の
// どのコードからも読み書きされない」列。TS 製 DB に生えなくても実害が無い
// ため gate の対象外にする。
//
// **列を新たに使い始めるときは必ずここから外すこと。** 使うのであれば
// ALTER TABLE ... ADD COLUMN IF NOT EXISTS の migration が必要になる。
var createOnlyAllowlist = map[string]string{
	// upstream が 1697420555911-deleteCreatedAt で DROP 済み。mk-go も #2243 で
	// model から除去し、INSERT/SELECT のどちらでも参照しなくなった。既存の
	// mk-go DB には NOT NULL DEFAULT now() の列が残るが未使用。
	"app.createdAt":          "#2243: 未使用 (upstream は DROP 済み)",
	"auth_session.createdAt": "#2243: 未使用 (upstream は DROP 済み)",
	// 旧・非正規化カウンタ。#2243 で撤去し、件数は upstream 同様
	// clip_note の実カウントで算出する。
	"clip.notesCount": "#2243: 未使用 (clip_note の実カウントに移行)",
}

func loadUpstreamColumns(t *testing.T) map[string]map[string]bool {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("testdata", "golden_upstream_columns.json"))
	if err != nil {
		t.Fatalf("read golden: %v (run `go run ./tools/schemadrift`)", err)
	}
	var raw map[string][]string
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("golden snapshot is empty; run `go run ./tools/schemadrift`")
	}
	out := make(map[string]map[string]bool, len(raw))
	for table, cols := range raw {
		set := make(map[string]bool, len(cols))
		for _, c := range cols {
			set[c] = true
		}
		out[table] = set
	}
	return out
}

var (
	createTableRe = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"([^"]+)"\s*\((.*?)\)\s*;`)
	createColRe   = regexp.MustCompile(`^"([^"]+)"\s+\S`)
	// **1 文が複数の句を持つ形が過半**なので、文単位で切ってから句を拾う。
	// `ALTER TABLE "x" ADD COLUMN "a"` の 1 句だけを取る正規表現では、実測 102 句の
	// うち 52 句 (51%) を取りこぼしていた (#2634)。取りこぼしは母集団の過小報告に
	// なるので、doc の件数 gate では**独自カラムを足しても落ちない**方向に倒れる。
	alterTableRe = regexp.MustCompile(`(?is)ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?"([^"]+)"(.*?);`)
	addColumnRe  = regexp.MustCompile(`(?i)ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?"([^"]+)"`)
	dropColumnRe = regexp.MustCompile(`(?i)DROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?"([^"]+)"`)
)

// dollarQuoteRe matches an opening dollar-quote tag (`$$` or `$tag$`).
var dollarQuoteRe = regexp.MustCompile(`^\$[A-Za-z_][A-Za-z0-9_]*\$|^\$\$`)

// stripSQLComments blanks out comments, string literals and dollar-quoted
// bodies, keeping byte offsets so the caller's positions stay meaningful.
//
// **文の終端を素の `;` で決めているので、`;` を含みうるものは全部潰す。**
// CREATE TABLE の本体に `);` を含むコメントや `DEFAULT 'f(x);'` が 1 つあるだけで
// body がそこで切れ、そのテーブルの列が母集団から丸ごと消える (= gate が黙って
// 通る)。ALTER 側も同じで、途中で文が切れると 2 句目以降を落とす。
//
// **中身を読み飛ばすのではなく空白化する。** 読み飛ばすと、その範囲に書かれた
// コメントが素通りして幻の DDL として拾われる (DO ブロックの中に例示として
// `ALTER TABLE ... ADD COLUMN` を書くと、drop-in gate がそれを「ALTER で足して
// あるから安全」と誤認する)。
//
// 抽出に使う識別子は全て二重引用符なので、単一引用符の中を消しても影響しない。
func stripSQLComments(text string) string {
	out := []byte(text)
	blank := func(i int) {
		if out[i] != '\n' {
			out[i] = ' '
		}
	}
	blankUntil := func(i int, isEnd func(int) bool, consume int) int {
		for i < len(text) && !isEnd(i) {
			blank(i)
			i++
		}
		for j := 0; j < consume && i < len(text); j++ {
			blank(i)
			i++
		}
		return i
	}

	for i := 0; i < len(text); {
		switch {
		case strings.HasPrefix(text[i:], "--"):
			i = blankUntil(i, func(j int) bool { return text[j] == '\n' }, 0)
		case strings.HasPrefix(text[i:], "/*"):
			i = blankUntil(i+2, func(j int) bool { return strings.HasPrefix(text[j:], "*/") }, 2)
		case text[i] == '\'':
			// `''` は文字列中のエスケープ。閉じ引用符の直後がまた引用符なら継続する。
			i++
			for i < len(text) {
				if text[i] == '\'' {
					if i+1 < len(text) && text[i+1] == '\'' {
						blank(i)
						blank(i + 1)
						i += 2
						continue
					}
					blank(i)
					i++
					break
				}
				blank(i)
				i++
			}
		case text[i] == '$' && dollarQuoteRe.MatchString(text[i:]):
			tag := dollarQuoteRe.FindString(text[i:])
			i = blankUntil(i+len(tag), func(j int) bool { return strings.HasPrefix(text[j:], tag) }, len(tag))
		default:
			i++
		}
	}
	return string(out)
}

// parseMigrations returns (table -> columns defined inside CREATE TABLE),
// (table -> columns that some migration ALTERs in or drops) and
// (table -> columns present after replaying every ALTER in source order).
//
// 3 つ目は「今の schema にある列」なので、ALTER で足して後から落とした列は
// 含まれない。**同一ファイル内の DROP → ADD (型変更の定番) を正しく扱うため、
// ADD と DROP をファイル単位でまとめず出現位置順に適用する** (#2634)。
// まとめると ADD が先に効いて DROP が最後に消し、実在する列が消えたことになる。
//
// 既知の制約。**いずれも現行 migration に該当が 0 件**だが、踏むと母集団が過小に
// なる (= doc の件数 gate が落ちない方向) ので、書くときは避けること。
//
//   - `RENAME COLUMN` を追跡しない (旧名が残り新名を知らない)
//   - `DO $$ ... $$` の中の DDL は見えない。中身を空白化しているため
//     (幻の DDL を拾わないための選択で、実 DDL も一緒に見えなくなる)。
//     `TestMigrationIdempotency_RequiresIfExists` も DO を含むファイルを丸ごと
//     skip するので、この形はどの gate にも捕まらない
//   - ネストしたブロックコメント `/* /* */ */` を 1 段しか閉じない
//   - `COLUMN` を省いた `ADD "x"` と、引用符なしの識別子を拾わない
//   - 同一ファイル内で ALTER が CREATE TABLE より前にある場合は CREATE を
//     先に処理する (現行の 2 ファイルは対象テーブルが重ならない)
func parseMigrations(t *testing.T) (map[string][]string, map[string]map[string]bool, map[string]map[string]bool) {
	t.Helper()
	dir := filepath.Join("..", "..", "migration")
	files, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found under %s (err=%v)", dir, err)
	}
	sort.Strings(files)

	createOnly := map[string][]string{}
	seen := map[string]map[string]bool{}
	altered := map[string]map[string]bool{}
	current := map[string]map[string]bool{}
	mark := func(m map[string]map[string]bool, table, col string) {
		if m[table] == nil {
			m[table] = map[string]bool{}
		}
		m[table][col] = true
	}

	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		text := stripSQLComments(string(src))
		for _, m := range createTableRe.FindAllStringSubmatch(text, -1) {
			table, body := m[1], m[2]
			for _, line := range strings.Split(body, "\n") {
				cm := createColRe.FindStringSubmatch(strings.TrimSpace(line))
				if cm == nil {
					continue
				}
				mark(current, table, cm[1])
				if seen[table] == nil {
					seen[table] = map[string]bool{}
				}
				if seen[table][cm[1]] {
					continue
				}
				seen[table][cm[1]] = true
				createOnly[table] = append(createOnly[table], cm[1])
			}
		}
		for _, m := range alterStatementsInOrder(text) {
			mark(altered, m.table, m.column)
			if m.drop {
				delete(current[m.table], m.column)
			} else {
				mark(current, m.table, m.column)
			}
		}
	}
	return createOnly, altered, current
}

type alterColumn struct {
	table  string
	column string
	drop   bool
	offset int
}

// alterStatementsInOrder returns every ADD/DROP COLUMN clause in the file, in
// the order they appear.
//
// 文単位で切ってから句を拾うので `ALTER TABLE "meta" ADD COLUMN "a", ADD COLUMN "b";`
// の 2 句目以降も取れる。句の順序は文をまたいでも文の中でも保つ。
func alterStatementsInOrder(text string) []alterColumn {
	var out []alterColumn
	for _, stmt := range alterTableRe.FindAllStringSubmatchIndex(text, -1) {
		table := text[stmt[2]:stmt[3]]
		body := text[stmt[4]:stmt[5]]

		var clauses []alterColumn
		collect := func(re *regexp.Regexp, drop bool) {
			for _, loc := range re.FindAllStringSubmatchIndex(body, -1) {
				clauses = append(clauses, alterColumn{
					table:  table,
					column: body[loc[2]:loc[3]],
					drop:   drop,
					offset: loc[0],
				})
			}
		}
		collect(addColumnRe, false)
		collect(dropColumnRe, true)
		sort.Slice(clauses, func(i, j int) bool { return clauses[i].offset < clauses[j].offset })
		out = append(out, clauses...)
	}
	return out
}
