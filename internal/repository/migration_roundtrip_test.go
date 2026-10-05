package repository

import (
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	// pgx5 driver と file source は本番の `elythia migrate` (internal/cli/migrate) と同じものを使う。
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/testutil"
)

// migrateURL builds the golang-migrate URL for a dedicated schema.
//
// 既定値は `testutil.testDSN` と揃えてある。ずれると「手元だけ別の DB を叩く」
// 状態になり、しかも接続できてしまうので気付けない。
// **組み立ては `net/url` に任せる。** 手で `QueryEscape` すると userinfo で
// 空白が `+` になり (URL の userinfo では `+` はリテラル)、パスワードに空白が
// 入ると別の文字列で接続しに行く。host も `JoinHostPort` を通さないと IPv6
// リテラル (`::1`) が壊れる — `testDSN` の key=value 形式はどちらも素で扱えるので、
// URL 側だけが壊れて「同じ環境変数なのに片方だけ繋がらない」ことになる。
func migrateURL(schema string) string {
	q := url.Values{}
	q.Set("sslmode", testutil.EnvOrDefault("TEST_DB_SSLMODE", "disable"))
	q.Set("search_path", schema)
	u := url.URL{
		Scheme: "pgx5",
		User: url.UserPassword(
			testutil.EnvOrDefault("TEST_DB_USER", "mk"),
			testutil.EnvOrDefault("TEST_DB_PASS", "mk"),
		),
		Host: net.JoinHostPort(
			testutil.EnvOrDefault("TEST_DB_HOST", "localhost"),
			testutil.EnvOrDefault("TEST_DB_PORT", "5432"),
		),
		Path:     "/" + testutil.EnvOrDefault("TEST_DB_NAME", "misskey_test"),
		RawQuery: q.Encode(),
	}
	return u.String()
}

// maxMigrationVersion returns the highest migration number under migration/.
func maxMigrationVersion(t *testing.T) uint {
	t.Helper()
	files, err := filepath.Glob("../../migration/*.up.sql")
	require.NoError(t, err)
	require.NotEmpty(t, files, "migration ファイルが 1 つも見つからない")
	var maxN uint
	for _, f := range files {
		base := filepath.Base(f)
		i := strings.Index(base, "_")
		require.Positivef(t, i, "連番と名前を区切る _ が無い: %s", base)
		n, perr := strconv.ParseUint(base[:i], 10, 64)
		require.NoErrorf(t, perr, "連番を読めない: %s", base)
		if uint(n) > maxN {
			maxN = uint(n)
		}
	}
	return maxN
}

// runMigrate runs fn and treats ErrNoChange as success.
func runMigrate(fn func() error) error {
	if err := fn(); err != nil && err != migrate.ErrNoChange {
		return err
	}
	return nil
}

// **down が実際に戻せることを検査する。**
//
// `make migrate-down` は運用手順に載っていて、CLAUDE.md も「down スクリプトは必ず
// 書く」と要求しているのに、**down が SQL として通ることを確かめるものが無かった**。
// down は書いた時点でしか実行されないので、後から up 側だけ直して対応が崩れても
// 誰も気付けない。壊れているのは**戻したくなった当日**に分かる。
//
// **`testutil.ApplyMigrations` では代用できない。** あちらは冪等な DDL のために
// `db.Exec` のエラーを握り潰す (`continue`) ので、壊れた SQL でも緑になる。ここは
// 本番の `elythia migrate` と同じ golang-migrate + pgx5 driver に流す。
//
// **専用の schema を使う。** `internal/repository` の schema でやると、down が
// 他のテストの前提にしているテーブルを消す (#2450)。`OpenTestDBSchema` の兄弟
// schema なら閉じている。
//
// 往復にするのが要点で、**down のあとにもう一度 up が通ること**まで見る。down が
// 一部だけ戻して残骸を置いていくと、2 回目の up が「既に在る」で落ちる。
func TestMigrations_UpDownUpRoundTrip(t *testing.T) {
	db, err := testutil.OpenTestDBSchema("migrateroundtrip")
	require.NoError(t, err, "専用 schema を開けない")

	var schema string
	require.NoError(t, db.Raw("SELECT current_schema()").Scan(&schema).Error)
	// **名前そのものを要求する。** `NotEmpty` だと、`search_path` が効いていない接続で
	// `current_schema()` が返す `public` を通してしまい、直後の `DROP SCHEMA` が
	// **共有の public を落とす**。ガードの文言と実際に弾ける条件がずれていた。
	require.Equal(t, "internal_repository_migrateroundtrip", schema,
		"search_path が効いていない (このまま進むと public を DROP する)")

	// **毎回 schema を作り直す。** 途中で落ちると残骸が残り、**次の実行が別の理由で
	// 落ちて診断が事実と無関係になる** (実際に変異検証でそうなった)。golang-migrate は
	// version を `schema_migrations` で持つので、中途半端な状態だと起点が狂う。
	// schema 名は `OpenTestDBSchema` が `[a-z0-9_]` へ正規化済み。
	require.NoError(t, db.Exec(`DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`).Error)
	require.NoError(t, db.Exec(`CREATE SCHEMA "`+schema+`"`).Error)

	m, err := migrate.New("file://../../migration", migrateURL(schema))
	require.NoError(t, err, "migrator を作れない")
	defer func() {
		serr, derr := m.Close()
		require.NoError(t, serr)
		require.NoError(t, derr)
	}()

	require.NoError(t, runMigrate(m.Up), "1 回目の up")

	// **version は migration ファイルの最大連番と突き合わせる。** `v1 == v2` や
	// `dirty == false` は `m.Up()` が nil を返した時点で必ず真なので、アサーションに
	// しても何も守らない (実測で変異が素通りした)。外の事実と比べる。
	v1, _, err := m.Version()
	require.NoError(t, err, "up 後の version を読めない")
	require.Equal(t, maxMigrationVersion(t), v1, "up が最後まで進んでいない")

	require.NoError(t, runMigrate(m.Down), "down — **全ての down が SQL として通るか**")

	_, _, err = m.Version()
	require.ErrorIs(t, err, migrate.ErrNilVersion, "down しきれていない")

	// **戻しきれたかを schema の中身で直接見る。**
	//
	// 「2 回目の up が通るか」だけでは弱すぎる — up の 97 本中 96 本は
	// `IF NOT EXISTS` / `EXCEPTION WHEN duplicate_object` で守られているので、
	// **down が取りこぼしても再適用が通ってしまう**。実測 (down を 1 本ずつ空に
	// する ablation) でも、2 回目の up で検出できたのは非冪等な `ADD CONSTRAINT`
	// を持つ `000001` だけで、サンプルした他 19 本は緑のまま通った。
	//
	// `schema_migrations` は golang-migrate の管理テーブルなので除く。
	var leftovers []string
	require.NoError(t, db.Raw(`
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = ?
		  AND c.relkind IN ('r', 'v', 'm', 'S')
		  AND c.relname <> 'schema_migrations'
		ORDER BY c.relname`, schema).Scan(&leftovers).Error)
	require.Empty(t, leftovers, "down がテーブル / view / sequence を戻していない")

	var leftoverTypes []string
	require.NoError(t, db.Raw(`
		SELECT t.typname
		FROM pg_type t
		JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname = ? AND t.typtype = 'e'
		ORDER BY t.typname`, schema).Scan(&leftoverTypes).Error)
	require.Empty(t, leftoverTypes, "down が enum 型を戻していない")

	require.NoError(t, runMigrate(m.Up), "2 回目の up — down が残骸を置いていると落ちる")

	v2, _, err := m.Version()
	require.NoError(t, err)
	require.Equal(t, v1, v2, "往復で version が変わった")
}
