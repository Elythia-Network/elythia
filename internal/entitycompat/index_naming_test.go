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

// TestIndexNaming_NoNewUpstreamDuplicates は、mk-go が upstream と同じ内容の
// index を **別名で** 新規追加するのを防ぐ。
//
// mk-go は index を `IDX_<table>_<col>` で命名するが、upstream (TypeORM) は
// `IDX_e5848eac4940934e23dbc17581` のような hash 名を生成する。
// `CREATE INDEX IF NOT EXISTS` は **index 名**で存在判定するため、定義が同一
// でも名前が違えば新規作成され、TS 製 DB では index が二重化する (#2246)。
//
// 実測 (Misskey TS 2026.7.0 が作った DB に mk-go の全 migration を適用):
//
//	適用前 442 本 → 適用後 639 本 (mk-go が 197 本作成)
//
// note は最大テーブルなので GIN index の二重化は書き込みスループットと容量に
// 効く。既存分は 000068 が実行時に落とすが、**新規に増やさない**ことを静的に
// 縛るのが本 gate。
//
// 新しく index を足すとき upstream に同じ (table, unique, method, columns) の
// index があるなら、000058 のように **upstream の index 名をそのまま使う**。
// そうすれば IF NOT EXISTS が効いて二重化しない。
func TestIndexNaming_NoNewUpstreamDuplicates(t *testing.T) {
	upstream := loadUpstreamIndexes(t)
	known := loadKnownDuplicateIndexes(t)
	partialOK := loadIntendedPartialIndexes(t)
	usedPartial := map[string]bool{}

	var findings []string
	for _, idx := range parseMigrationIndexes(t) {
		names, ok := upstream[idx.key]
		if !ok {
			continue // upstream に対応 index が無い = mk-go 固有、問題なし
		}
		if names[idx.name] {
			continue // upstream と同名 = IF NOT EXISTS が効く
		}
		if known[idx.name] {
			continue // 既知の重複 (000068 が実行時に落とす)
		}
		if _, ok := partialOK[idx.name]; ok && idx.partial {
			usedPartial[idx.name] = true
			continue // 本家の index と並べて置く部分 index (理由は allowlist にある)
		}
		upNames := make([]string, 0, len(names))
		for n := range names {
			upNames = append(upNames, n)
		}
		sort.Strings(upNames)
		findings = append(findings,
			idx.name+" (upstream: "+strings.Join(upNames, ", ")+")")
	}
	sort.Strings(findings)

	if len(findings) > 0 {
		t.Errorf(`upstream と同内容の index を別名で作っているものが %d 件:
  %s

TS 製 DB では index 名が違うと二重に作られる (CREATE INDEX IF NOT EXISTS は
名前でしか存在判定しない)。upstream の index 名をそのまま使うこと
(000058_channel_muting_expires_at.up.sql が前例)。

意図的に upstream と同じ列の部分 index を並べる場合は、
testdata/intended_partial_indexes.json に理由と一緒に足すこと
(known_duplicate_indexes.json は 000068 が落とすものの一覧なので混ぜない)。`,
			len(findings), strings.Join(findings, "\n  "))
	}
	// 使われていない項目を残さない。部分 index でなくなった (WHERE を外した) ものも
	// ここで落ちる。
	for name := range partialOK {
		if !usedPartial[name] {
			t.Errorf("testdata/intended_partial_indexes.json の %s は、upstream と同じ列の部分 index として migration に現れない。項目を消すこと", name)
		}
	}
}

type migrationIndex struct {
	name    string
	key     indexKey
	partial bool // WHERE を持つ
}

// indexKey is the structural identity of an index: same key means the two
// indexes serve the same queries. 述語 (WHERE) は含めない。partial かどうかで
// 名前を変えてよいわけではないので、まず key 一致で拾って allowlist で捌く。
// 部分 index を upstream の index と並べて置くと決めたものは
// testdata/intended_partial_indexes.json に理由と一緒に載せ、
// migrationIndex.partial が真のときだけ許す。
type indexKey struct {
	table  string
	unique bool
	method string
	cols   string
}

func loadUpstreamIndexes(t *testing.T) map[indexKey]map[string]bool {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("testdata", "golden_upstream_indexes.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var rows []struct {
		Table string `json:"table"`
		Name  string `json:"name"`
		Def   string `json:"def"`
	}
	if err := json.Unmarshal(blob, &rows); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("golden snapshot is empty")
	}
	out := map[indexKey]map[string]bool{}
	for _, r := range rows {
		k, ok := parseGoldenDef(r.Def)
		if !ok {
			continue
		}
		if out[k] == nil {
			out[k] = map[string]bool{}
		}
		out[k][r.Name] = true
	}
	return out
}

// goldenDefRe parses the pg_indexes indexdef body stored in the golden:
// "UNIQUE|public.note USING gin (tags)" / "|public.note USING btree (a, b) WHERE (...)".
var goldenDefRe = regexp.MustCompile(`^\s*(?:public\.)?("?[\w.]+"?)\s+USING\s+(\w+)\s+\((.*?)\)(?:\s+WHERE\s+.*)?$`)

func parseGoldenDef(def string) (indexKey, bool) {
	unique := strings.HasPrefix(def, "UNIQUE|")
	_, rest, found := strings.Cut(def, "|")
	if !found {
		return indexKey{}, false
	}
	m := goldenDefRe.FindStringSubmatch(rest)
	if m == nil {
		return indexKey{}, false
	}
	return indexKey{
		table:  strings.Trim(m[1], `"`),
		unique: unique,
		method: m[2],
		cols:   normalizeCols(m[3]),
	}, true
}

// loadIntendedPartialIndexes reads testdata/intended_partial_indexes.json: name
// -> why the partial index is kept next to upstream's full index on the same
// columns.
//
// known_duplicate_indexes.json とは分ける。あちらは「000068 が TS 製 DB で落とす、
// upstream と定義が同じ index」の一覧で、部分 index は 000068 の対象ではない
// (定義が違うので落とさない。TS 製 DB では upstream の index と並んで残る)。
func loadIntendedPartialIndexes(t *testing.T) map[string]string {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("testdata", "intended_partial_indexes.json"))
	if err != nil {
		t.Fatalf("read partial allowlist: %v", err)
	}
	var out map[string]string
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("parse partial allowlist: %v", err)
	}
	for name, why := range out {
		if strings.TrimSpace(why) == "" {
			t.Errorf("intended_partial_indexes.json の %s に理由が無い", name)
		}
	}
	return out
}

func loadKnownDuplicateIndexes(t *testing.T) map[string]bool {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("testdata", "known_duplicate_indexes.json"))
	if err != nil {
		t.Fatalf("read allowlist: %v", err)
	}
	var names []string
	if err := json.Unmarshal(blob, &names); err != nil {
		t.Fatalf("parse allowlist: %v", err)
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

var migrationIndexRe = regexp.MustCompile(
	`(?is)CREATE\s+(UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?"([^"]+)"\s+ON\s+"([^"]+)"\s*(?:USING\s+(\w+)\s*)?\((.*?)\)\s*(WHERE[^;]*)?;`)

func parseMigrationIndexes(t *testing.T) []migrationIndex {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "migration", "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found (err=%v)", err)
	}
	sort.Strings(files)
	var out []migrationIndex
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range migrationIndexRe.FindAllStringSubmatch(string(src), -1) {
			method := strings.ToLower(m[4])
			if method == "" {
				method = "btree"
			}
			out = append(out, migrationIndex{
				name:    m[2],
				partial: strings.TrimSpace(m[6]) != "",
				key: indexKey{
					table:  m[3],
					unique: strings.TrimSpace(m[1]) != "",
					method: method,
					cols:   normalizeCols(m[5]),
				},
			})
		}
	}
	return out
}

func normalizeCols(s string) string {
	parts := strings.Split(s, ",")
	for i, p := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(p), `"`)
	}
	return strings.Join(parts, ",")
}
