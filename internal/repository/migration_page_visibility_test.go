package repository

import (
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/testutil"
)

// pageVisibilities returns page id -> visibility in the given handle's schema.
func pageVisibilities(t *testing.T, db *gorm.DB) map[string]string {
	t.Helper()
	type row struct {
		ID         string
		Visibility string
	}
	var rows []row
	require.NoError(t, db.Raw(`SELECT "id", "visibility"::text AS "visibility" FROM "page" ORDER BY "id"`).Scan(&rows).Error)
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.Visibility
	}
	return out
}

// enumLabels returns the labels of page_visibility_enum in sort order.
func enumLabels(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	var labels []string
	require.NoError(t, db.Raw(`SELECT enumlabel FROM pg_enum
		WHERE enumtypid = 'page_visibility_enum'::regtype ORDER BY enumsortorder`).Scan(&labels).Error)
	return labels
}

// TestMigration121_PageVisibilityPrivate checks that 000121 rewrites existing
// rows (#3479), that down maps them back, and that re-running the up on an
// already two-valued enum leaves rows untouched.
//
// 往復テスト (TestMigrations_UpDownUpRoundTrip) は空の表で SQL が通るかしか見ないので、
// 行の変換はここで見る。followers / specified を private に倒すのが要点で、
// 取りこぼすと作者が限定したつもりの Page が型の変換で失敗するか、公開に化ける。
func TestMigration121_PageVisibilityPrivate(t *testing.T) {
	db, err := testutil.OpenTestDBSchema("migrate121")
	require.NoError(t, err, "専用 schema を開けない")

	var schema string
	require.NoError(t, db.Raw("SELECT current_schema()").Scan(&schema).Error)
	// 往復テストと同じく、名前を名指しで要求してから DROP する (public を落とさない)。
	require.Equal(t, "internal_repository_migrate121", schema)
	require.NoError(t, db.Exec(`DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`).Error)
	require.NoError(t, db.Exec(`CREATE SCHEMA "`+schema+`"`).Error)

	m, err := migrate.New("file://../../migration", migrateURL(schema))
	require.NoError(t, err)
	defer func() {
		serr, derr := m.Close()
		require.NoError(t, serr)
		require.NoError(t, derr)
	}()

	require.NoError(t, runMigrate(func() error { return m.Migrate(120) }), "120 まで up")
	require.Equal(t, []string{"public", "followers", "specified"}, enumLabels(t, db))

	require.NoError(t, db.Exec(`INSERT INTO "user" ("id", "username", "usernameLower") VALUES ('u1', 'alice', 'alice')`).Error)
	for _, p := range []struct{ id, vis string }{
		{"p_public", "public"},
		{"p_followers", "followers"},
		{"p_specified", "specified"},
	} {
		require.NoError(t, db.Exec(`INSERT INTO "page" ("id", "title", "name", "userId", "visibility")
			VALUES (?, ?, ?, 'u1', ?::page_visibility_enum)`, p.id, p.id, p.id, p.vis).Error)
	}

	require.NoError(t, m.Steps(1), "121 の up")
	require.Equal(t, []string{"public", "private"}, enumLabels(t, db))
	require.Equal(t, map[string]string{
		"p_public":    "public",
		"p_followers": "private",
		"p_specified": "private",
	}, pageVisibilities(t, db))

	// 既定値も新しい型で残っていること (DROP DEFAULT したまま戻し忘れると、visibility を
	// 渡さない INSERT が NOT NULL で落ちる)。
	require.NoError(t, db.Exec(`INSERT INTO "page" ("id", "title", "name", "userId") VALUES ('p_default', 'd', 'd', 'u1')`).Error)
	require.Equal(t, "public", pageVisibilities(t, db)["p_default"])

	// 2 値の enum (派生版から移ってきた DB) で up をもう一度流しても、行は変わらない。
	require.NoError(t, m.Force(120))
	require.NoError(t, m.Steps(1), "2 値の enum での 121 の up")
	require.Equal(t, []string{"public", "private"}, enumLabels(t, db))
	require.Equal(t, map[string]string{
		"p_public":    "public",
		"p_followers": "private",
		"p_specified": "private",
		"p_default":   "public",
	}, pageVisibilities(t, db))

	// down は private を specified に戻す (visibleUserIds は空なので作者以外に見えない)。
	require.NoError(t, m.Steps(-1), "121 の down")
	require.Equal(t, []string{"public", "followers", "specified"}, enumLabels(t, db))
	require.Equal(t, map[string]string{
		"p_public":    "public",
		"p_followers": "specified",
		"p_specified": "specified",
		"p_default":   "public",
	}, pageVisibilities(t, db))
	require.NoError(t, db.Exec(`INSERT INTO "page" ("id", "title", "name", "userId") VALUES ('p_default2', 'e', 'e', 'u1')`).Error)
	require.Equal(t, "public", pageVisibilities(t, db)["p_default2"])
}

// visibilityHasDefault reports whether page.visibility has a column default.
func visibilityHasDefault(t *testing.T, db *gorm.DB) bool {
	t.Helper()
	var n int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM pg_attrdef d
		JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
		WHERE d.adrelid = '"page"'::regclass AND a.attname = 'visibility'`).Scan(&n).Error)
	return n > 0
}

// TestMigration121_KeepsMissingDefault checks that 000121 does not add a
// default to a TS-made column, which has none (#3479).
//
// 本家 (TypeORM) の page.visibility は NOT NULL だけで既定値が無い。up / down が
// 無条件に SET DEFAULT すると、TS 版から移った DB に本家に無い既定値が付く。
func TestMigration121_KeepsMissingDefault(t *testing.T) {
	db, err := testutil.OpenTestDBSchema("migrate121ts")
	require.NoError(t, err, "専用 schema を開けない")

	var schema string
	require.NoError(t, db.Raw("SELECT current_schema()").Scan(&schema).Error)
	require.Equal(t, "internal_repository_migrate121ts", schema)
	require.NoError(t, db.Exec(`DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`).Error)
	require.NoError(t, db.Exec(`CREATE SCHEMA "`+schema+`"`).Error)

	m, err := migrate.New("file://../../migration", migrateURL(schema))
	require.NoError(t, err)
	defer func() {
		serr, derr := m.Close()
		require.NoError(t, serr)
		require.NoError(t, derr)
	}()

	require.NoError(t, runMigrate(func() error { return m.Migrate(120) }), "120 まで up")
	// TS が作った列の形にする。
	require.NoError(t, db.Exec(`ALTER TABLE "page" ALTER COLUMN "visibility" DROP DEFAULT`).Error)
	require.NoError(t, db.Exec(`INSERT INTO "user" ("id", "username", "usernameLower") VALUES ('u1', 'alice', 'alice')`).Error)
	require.NoError(t, db.Exec(`INSERT INTO "page" ("id", "title", "name", "userId", "visibility")
		VALUES ('p1', 't', 'n', 'u1', 'public')`).Error)

	require.NoError(t, m.Steps(1), "121 の up")
	assert.False(t, visibilityHasDefault(t, db), "up が既定値を足した")
	assert.Equal(t, map[string]string{"p1": "public"}, pageVisibilities(t, db))

	require.NoError(t, m.Steps(-1), "121 の down")
	assert.False(t, visibilityHasDefault(t, db), "down が既定値を足した")
}
