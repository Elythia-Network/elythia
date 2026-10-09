package testutil

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// callerPackage は呼び出し元のパッケージから schema 名を決める (#2450)。
// ここが壊れると全パッケージが同じ schema に集まり、干渉が黙って戻る。
func TestCallerPackage_DerivesFromCallingPackage(t *testing.T) {
	// skip=1 = この test 自身のファイル -> internal/testutil。
	assert.Equal(t, "internal_testutil", callerPackage(1))
}

// runtime.Caller が取れないほど深い skip では空を返す。呼び出し元を特定できない
// まま適当な schema を選ぶより、共有側に倒して接続だけは通す。
func TestCallerPackage_UnknownCallerReturnsEmpty(t *testing.T) {
	assert.Empty(t, callerPackage(1000))
}

// identifier 上限を超えたら末尾を残す。silent に切り詰められて別パッケージと
// 同じ名前に化けるのを防ぐ。
func TestCallerPackage_LengthIsBounded(t *testing.T) {
	assert.LessOrEqual(t, len(callerPackage(1)), maxPackageSuffixLen)
}

// search_path は schema を渡したときだけ付く。空なら共有 (public) のまま。
func TestTestDSN_PinsSearchPath(t *testing.T) {
	assert.NotContains(t, testDSN(""), "search_path")
	assert.Contains(t, testDSN("internal_api_gallery"), "search_path=internal_api_gallery")
}

// 生成した schema 名がそのまま SQL identifier に入るので、英数字と `_` 以外が
// 残っていないことを担保する (injection の余地を作らない)。
func TestCallerPackage_SanitizesToIdentifierChars(t *testing.T) {
	name := callerPackage(1)
	require.NotEmpty(t, name)
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
		assert.Truef(t, ok, "unexpected character %q in schema name %q", r, name)
	}
}

// 「他が先に作った」系のエラーは成功扱いにする。並行して同じ schema を作りに来た
// ときに、片方だけ落とさないため。
func TestIsBenignCreateSchemaError(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{"duplicate schema", `ERROR: schema "x" already exists (SQLSTATE 42P06)`, true},
		{"sqlstate only", "failed: SQLSTATE 42P06", true},
		{"pg_namespace unique violation", "failed: SQLSTATE 23505", true},
		{"permission denied", "ERROR: permission denied for database (SQLSTATE 42501)", false},
		{"connection refused", "dial tcp: connection refused", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isBenignCreateSchemaError(errString(tc.msg)))
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// projectRoot はこの file の位置から repo root を導く。ずれると callerPackage が
// 相対化に失敗してフルパス由来の名前になる (壊れはしないが名前が変わる)。
func TestProjectRoot_ContainsMigrationDir(t *testing.T) {
	root := projectRoot()
	require.NotEmpty(t, root)
	assert.True(t, strings.HasSuffix(root, "mk") || strings.Contains(root, "/"),
		"unexpected project root: %s", root)
}

// #2756: ApplyMigrations が実行のたびに全 migration を流し直すと列枠を食う。
// **PostgreSQL は DROP した列も 1 テーブル 1600 列の上限に数える**ので、繰り返す
// と最後は `tables can have at most 1600 columns` で落ちる。実際に枠が増えるのは
// `note` だけ (`000033` が ADD し `000036` が DROP する)。
//
// 台帳が効いていれば 2 回目以降は 1 本も流れない。
// **台帳テストは使い捨ての schema を使う。** 1 本ごとに全 migration を流すので
// 実測 1-3.5 秒かかるが、共有して残す形にはしない: このテスト群は
// `DROP TABLE ... CASCADE` のように schema を壊す操作を含み、残すと壊れたまま
// 次回以降ずっと落ち続ける (`CREATE TABLE IF NOT EXISTS` は既存テーブルに
// 何もしないので、CASCADE で落ちた FK が戻らない)。**実行をまたいで残る schema を
// 壊すな**というのが #2756 で入れたルールそのもの。
func ledgerSchemaDB(t *testing.T, suffix string) *gorm.DB {
	t.Helper()
	db, err := OpenTestDBSchema(suffix)
	// DB が無い環境で skip すると無検証のまま緑になる。手元にも PostgreSQL が
	// 要る前提 (CLAUDE.md Section 4) なので落とす。
	require.NoError(t, err, "test DB unavailable")
	t.Cleanup(func() {
		_ = db.Exec(`DROP SCHEMA IF EXISTS "` + schemaName("internal_testutil_"+suffix) + `" CASCADE`).Error
	})
	return db
}

func TestApplyMigrations_SkipsAlreadyApplied(t *testing.T) {
	db := ledgerSchemaDB(t, "ledger")
	ApplyMigrations(db)

	// 台帳が全 migration を記録していること。
	files, err := findMigrationFiles(filepath.Join(projectRoot(), "migration"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	var recorded int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM "`+migrationLedgerTable+`"`).Scan(&recorded).Error)
	assert.EqualValues(t, len(files), recorded, "全 migration が記録されること")

	// **列枠が増えないこと** — 2 回目の適用で attnum の最大値が動かない。
	maxAttnum := func() int64 {
		var v int64
		require.NoError(t, db.Raw(`
			SELECT COALESCE(max(a.attnum), 0) FROM pg_attribute a
			JOIN pg_class c ON c.oid = a.attrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = current_schema() AND c.relname = 'note'`).Scan(&v).Error)
		return v
	}
	before := maxAttnum()
	require.Positive(t, before, "note テーブルが作られていること")
	ApplyMigrations(db)
	assert.Equal(t, before, maxAttnum(), "2 回目の適用で列枠を消費しないこと")
}

// migration を書き換えたら流し直すこと。名前だけの台帳だと、開発中に migration を
// 直しても反映されなくなる。
//
// **schema への実効果で見る。** 台帳の sha が戻ったかだけを見ると、「実行せずに
// sha だけ押し直す」実装でも通ってしまう (実測で素通りした)。対象の migration が
// 作るテーブルを消してから sha を stale にし、**戻ってくること**を確かめる。
func TestApplyMigrations_RerunsOnContentChange(t *testing.T) {
	db := ledgerSchemaDB(t, "ledgerhash")
	ApplyMigrations(db)
	const target = "000008_clip.up.sql"
	tableExists := func() bool {
		var n int64
		require.NoError(t, db.Raw(`SELECT count(*) FROM information_schema.tables
			WHERE table_schema = current_schema() AND table_name = 'clip'`).Scan(&n).Error)
		return n > 0
	}
	require.True(t, tableExists(), "前提: clip が作られていること")

	require.NoError(t, db.Exec(`DROP TABLE "clip" CASCADE`).Error)
	require.False(t, tableExists())

	// 台帳が一致している間は流し直さない (= 消えたまま)。
	ApplyMigrations(db)
	require.False(t, tableExists(), "内容が変わっていなければ流し直さない")

	// 対象 1 本だけ内容が変わった状態にする。
	require.NoError(t, db.Exec(`UPDATE "`+migrationLedgerTable+`" SET sha = 'stale' WHERE name = ?`, target).Error)
	ApplyMigrations(db)
	assert.True(t, tableExists(), "内容が変わった migration は流し直す")

	// **記録されている値がファイルの内容ハッシュであること。** ファイルごとに
	// 定数を入れるだけでも上の assert は通ってしまうので、実物と突き合わせる。
	body, err := os.ReadFile(filepath.Join(projectRoot(), "migration", target))
	require.NoError(t, err)
	var got string
	require.NoError(t, db.Raw(`SELECT sha FROM "`+migrationLedgerTable+`" WHERE name = ?`, target).Scan(&got).Error)
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256(body)), got,
		"台帳の値は内容の sha256 であること")
}

// **失敗した migration は記録しない。** 記録すると一過性の失敗 (並行 DDL /
// lock timeout / 新しい migration の実バグ) が恒久的に隠れる。
func TestApplyMigrations_DoesNotRecordFailures(t *testing.T) {
	db := ledgerSchemaDB(t, "ledgerfail")

	// 前提: fresh schema なので台帳は空。
	require.NoError(t, db.Exec(`CREATE TABLE IF NOT EXISTS "`+migrationLedgerTable+`" (
		name text PRIMARY KEY, sha text NOT NULL)`).Error)
	var n int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM "`+migrationLedgerTable+`"`).Scan(&n).Error)
	require.Zero(t, n, "前提: 台帳は空")

	// fresh schema では全本が成功するので、まず「成功した本数 == ファイル数」。
	ApplyMigrations(db)
	files, err := findMigrationFiles(filepath.Join(projectRoot(), "migration"))
	require.NoError(t, err)
	var recorded int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM "`+migrationLedgerTable+`"`).Scan(&recorded).Error)
	assert.EqualValues(t, len(files), recorded, "fresh schema では全 migration が成功して記録される")

	// **失敗を記録しないことは、既存 schema への再適用で見る。** 全 sha を stale に
	// して流し直すと、実測 120 本中 2 本 (`000001_initial` の制約重複 /
	// `000075_signup_application` の列不在) だけが失敗する。記録するように変えると
	// stale が 0 になるので、この assert が落ちる。
	//
	// **この 2 本が失敗し続けることに依存している。** 冪等に直す (望ましい方向の
	// 変更) と落ちるので、そのときは検出の作り方ごと見直すこと。
	before := recorded
	require.NoError(t, db.Exec(`UPDATE "`+migrationLedgerTable+`" SET sha = 'stale'`).Error)
	ApplyMigrations(db)
	var stale int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM "`+migrationLedgerTable+`" WHERE sha = 'stale'`).Scan(&stale).Error)
	assert.Positive(t, stale, "再適用で失敗する migration は記録し直されない (実測で 2 本)")
	var after int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM "`+migrationLedgerTable+`"`).Scan(&after).Error)
	assert.Equal(t, before, after, "行数は増えない")
}

// OpenTestDBSchema は呼び出し元パッケージの兄弟 schema を作る。
func TestOpenTestDBSchema_CreatesSiblingSchema(t *testing.T) {
	db := ledgerSchemaDB(t, "sibling")

	var got string
	require.NoError(t, db.Raw(`SELECT current_schema()`).Scan(&got).Error)
	assert.Equal(t, "internal_testutil_sibling", got)
}

// suffix / package が空なら作らない (どの schema に繋がるか分からない接続を
// 返すより、失敗させる)。
func TestOpenTestDBSchema_RejectsEmptySuffix(t *testing.T) {
	_, err := OpenTestDBSchema("")
	assert.Error(t, err)
}

// **`schemaName` は identifier に使えない文字を残さない。** `ensureSchema` は
// identifier をプレースホルダに出来ず自前で組むので、ここが素通しになると
// `OpenTestDBSchema` に渡した suffix がそのまま SQL に入る。
func TestSchemaName_SanitizesHostileInput(t *testing.T) {
	got := schemaName(`x"; DROP SCHEMA "public" CASCADE; --`)
	for _, r := range got {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
		assert.Truef(t, ok, "unexpected character %q in %q", r, got)
	}
}

// OpenTestDBSchema が正規化を通していること (schemaName を素通りさせる変異を
// 落とす)。schema 名は接続後の current_schema() で確かめる。
func TestOpenTestDBSchema_NormalizesSuffix(t *testing.T) {
	db := ledgerSchemaDB(t, "Odd-Suffix.1")

	const want = "internal_testutil_odd_suffix_1"
	var n int64
	require.NoError(t, db.Raw(
		`SELECT count(*) FROM information_schema.schemata WHERE schema_name = ?`, want).Scan(&n).Error)
	require.EqualValues(t, 1, n, "正規化した名前の schema が作られること")

	var got string
	require.NoError(t, db.Raw(`SELECT current_schema()`).Scan(&got).Error)
	assert.Equal(t, want, got, "接続先も正規化した名前であること")
}

// 長い名前は末尾を残して切る (identifier 上限。silent に別 schema へ化けない)。
func TestSchemaName_TruncatesFromTheFront(t *testing.T) {
	long := strings.Repeat("a", maxPackageSuffixLen+5) + "_tail"
	got := schemaName(long)
	assert.Len(t, got, maxPackageSuffixLen)
	assert.True(t, strings.HasSuffix(got, "_tail"))
	assert.Equal(t, "short", schemaName("short"))
}

// fork の系列 (migration/local/、#3428) も core の後に流す。台帳のキーは
// `local/<name>` にして、core と同じ番号・名前でも取り違えない。
func TestFindMigrationFiles_IncludesLocalTrackAfterCore(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "local", "nested"), 0o755))
	for _, name := range []string{
		"000002_b.up.sql", "000001_a.up.sql", "000001_a.down.sql",
		"local/000001_a.up.sql", "local/000001_a.down.sql", "local/nested/000009_x.up.sql",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), nil, 0o600))
	}
	files, err := findMigrationFiles(dir)
	require.NoError(t, err)
	var keys []string
	for _, f := range files {
		keys = append(keys, migrationLedgerKey(dir, f))
	}
	assert.Equal(t, []string{"000001_a.up.sql", "000002_b.up.sql", "local/000001_a.up.sql"}, keys)

	// local が無いときは core だけ (Elythia 本体の状態)。
	core := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(core, "000001_a.up.sql"), nil, 0o600))
	files, err = findMigrationFiles(core)
	require.NoError(t, err)
	assert.Len(t, files, 1)
}
