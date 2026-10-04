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

// TestMigrationSeed_CoversUpstream guards the drop-in **return** path
// (mk-go → Misskey TS).
//
// 復路は保証しない (#3191) が、この gate は required のまま残す。seed は、どこまで
// 戻れるかを測る `mkgo-born` (docs/dropin-e2e.md) の前提で、足す手間も小さいため。
//
// TypeORM は起動時に bookkeeping テーブル `migrations` を読み、
//
//	allMigrations.filter(m => !executed.find(e => e.name === m.name))
//
// で未実行 migration を選ぶ (MigrationExecutor)。比較キーは name 列の**文字列
// 一致**で、比較対象は `migration.name ?? migration.constructor.name`
// (= `DeleteCreatedAt1697420555911` 形式)。
//
// mk-go で動かした DB に本家を繋ぎ直したとき、seed に漏れがあるとその
// migration が未実行と判定されて再実行される。適用済み DDL への ADD COLUMN
// 重複や DROP COLUMN によるデータ喪失につながりうる (#2244)。
//
// upstream を bump したら `make shapecheck-gen` で golden を再生成し、増えた
// 分を seed する migration を追加すること。
func TestMigrationSeed_CoversUpstream(t *testing.T) {
	want := loadUpstreamMigrations(t)
	got := seededMigrationNames(t)

	var missing []string
	for _, name := range want {
		if !got[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf(`migrations テーブルに seed されていない upstream migration が %d 件:
  %s

mk-go で動かした DB に Misskey TS を繋ぎ直すと、これらが未実行と判定されて
再実行される。migration/ に seed を追加すること (000067 と同じ形式):

  INSERT INTO "migrations" ("timestamp", "name")
  SELECT v.ts, v.n FROM (VALUES (<timestamp>::bigint, '<Name><timestamp>')) AS v(ts, n)
  WHERE NOT EXISTS (SELECT 1 FROM "migrations" m WHERE m."name" = v.n);

seed する前に、その migration の DDL が mk-go 側にも入っているか必ず確認する
こと。入っていないまま seed すると TS 側が「適用済み」と誤認して skip し、
schema がずれたまま放置される。`,
			len(missing), strings.Join(missing, "\n  "))
	}
}

func loadUpstreamMigrations(t *testing.T) []string {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("testdata", "golden_upstream_migrations.json"))
	if err != nil {
		t.Fatalf("read golden: %v (run `go run ./tools/schemadrift`)", err)
	}
	var names []string
	if err := json.Unmarshal(blob, &names); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("golden snapshot is empty; run `go run ./tools/schemadrift`")
	}
	return names
}

// typeormNameRe matches a TypeORM migration name literal: an identifier
// followed by the 13-digit timestamp TypeORM always appends. 000029 が seed
// している旧短縮名 ('deleteCreatedAt' 等) は suffix を持たないので拾わない。
var typeormNameRe = regexp.MustCompile(`'([A-Za-z][A-Za-z0-9_-]*\d{13})'`)

// seededMigrationNames scans every migration file for TypeORM-shaped migration
// name literals. 000067 のように UPDATE で書き換える形でも INSERT で足す形でも
// 拾えるよう、SQL 構文ではなく name literal を見る。
func seededMigrationNames(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "migration", "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found (err=%v)", err)
	}
	out := map[string]bool{}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range typeormNameRe.FindAllSubmatch(src, -1) {
			out[string(m[1])] = true
		}
	}
	return out
}
