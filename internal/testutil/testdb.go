package testutil

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// OpenTestDB connects to the test database with search_path pinned to a schema
// dedicated to the calling package, creating that schema on first use.
//
// 接続情報は環境変数 TEST_DB_HOST, TEST_DB_PORT, TEST_DB_NAME, TEST_DB_USER,
// TEST_DB_PASS, TEST_DB_SSLMODE、無ければ .env.test から読み取る。
//
// # なぜパッケージごとに schema を分けるか (#2450)
//
// CI は shard ごとに PostgreSQL を 1 つ立て、`go test <pkgs...>` が**パッケージの
// テストバイナリを並行実行**する。全パッケージが同じ DB を共有していると、
// 一方の後片付けが他方の前提を壊す。
//
// 実際に `internal/charttick` の `DELETE FROM "user"` が `internal/api/gallery` の
// 所有者 user を消し、無関係な PR で CI が落ちた。しかも charttick は
// **テーブル全体の絶対件数**をアサートするので、削除を絞ると今度は他パッケージの
// 行が混ざって charttick 自身が落ちる。**干渉は双方向で、削除範囲を絞るだけでは
// 解けない。**
//
// shard の割り当ては `go list` の出力順に対する `NR % 4` なので、テストパッケージを
// 1 つ足すだけで同居の組み合わせが変わる。個別の衝突を潰す対処では再発するため、
// **共有そのものをやめる**。
//
// schema 名は呼び出し元のパッケージから機械的に決める。新しいパッケージが
// `MustOpenTestDB` を呼んでも自動で隔離されるので、この問題を再び持ち込めない。
func OpenTestDB() (*gorm.DB, error) {
	return openTestDBForPackage(callerPackage(2))
}

// OpenTestDBSchema connects to a **sibling schema** of the calling package's
// one, named `<package>_<suffix>`, creating it on first use.
//
// **shared な schema を実行中に作り変えるテスト向け** (#2756)。列を DROP して
// 戻すような操作は、PostgreSQL が DROP した列も 1600 の上限に数えるため実行の
// たびに枠を食う。専用 schema を一度だけその形に作って使い回せば、繰り返し
// 回しても増えない。同じパッケージの他のテストと状態を共有しない利点もある。
//
// migration は呼び出し側が ApplyMigrations で流すこと (台帳があるので 2 回目
// 以降は skip される)。
func OpenTestDBSchema(suffix string) (*gorm.DB, error) {
	pkg := callerPackage(2)
	if pkg == "" || suffix == "" {
		return nil, fmt.Errorf("open test schema: empty package or suffix")
	}
	return openTestDBForPackage(schemaName(pkg + "_" + suffix))
}

// schemaName normalizes an arbitrary string into a schema identifier: `[a-z0-9_]`
// only, truncated from the front to the identifier limit.
//
// **正規化はここ 1 箇所に閉じる。** `ensureSchema` は identifier をプレース
// ホルダに出来ず自前で組むので、「呼び出し元が正規化済みである」ことに寄りかかって
// いる。切り詰めだけにすると、`OpenTestDBSchema` に任意の suffix を渡した経路で
// その前提が破れる (実際にレビューで schema 注入を実演された)。
//
// 切り詰めは**先頭を捨てる**。パッケージパス由来の名前では末端のパッケージ名が
// 残って区別が付くため。`OpenTestDBSchema` の `<package>_<suffix>` では逆に
// パッケージ名の側から削れるので、63 文字に近づく長さの組み合わせは避けること
// (現状の名前は最長でも 30 文字前後)。
func schemaName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > maxPackageSuffixLen {
		out = out[len(out)-maxPackageSuffixLen:]
	}
	return out
}

// OpenSharedTestDB connects to the shared `public` schema without creating a
// per-package one.
//
// `internal/db` のように **接続処理そのもの**を試すテスト専用。データを読み書き
// するテストからは使わないこと (#2450 の干渉が戻る)。
func OpenSharedTestDB() (*gorm.DB, error) {
	loadEnvFile()
	return openDSN(testDSN(""))
}

// TestDSNForSchema exposes the test DSN builder so tests that need to open a
// second connection (e.g. registering a replica for dbresolver) can reuse the
// same credentials. An empty schema leaves search_path at its default.
func TestDSNForSchema(schema string) string { return testDSN(schema) }

// testDSN builds the connection string, optionally pinning the search_path.
//
// pgx は未知の keyword を runtime parameter として送るので、search_path は DSN に
// 直接書ける。接続ごとに適用されるため、GORM の connection pool が新しい接続を
// 張っても schema が変わらない。
func testDSN(schema string) string {
	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		EnvOrDefault("TEST_DB_HOST", "localhost"),
		EnvOrDefault("TEST_DB_PORT", "5432"),
		EnvOrDefault("TEST_DB_USER", "mk"),
		EnvOrDefault("TEST_DB_PASS", "mk"),
		EnvOrDefault("TEST_DB_NAME", "misskey_test"),
		EnvOrDefault("TEST_DB_SSLMODE", "disable"))
	if schema != "" {
		dsn += " search_path=" + schema
	}
	return dsn
}

// openDSN opens a GORM connection with the settings shared by every test DB.
func openDSN(dsn string) (*gorm.DB, error) {
	// PreferSimpleProtocol: pgx の auto-prepared statement cache を無効化する。
	// 同じ DB を多 test で再利用する CI 環境で、軽微な schema 差分 (= AutoMigrate
	// による additive 変更や test 経路で DDL 相当が走るケース) のあと再 query すると
	// `ERROR: cached plan must not change result type` が偶発的に出る (#1089)。
	// simple protocol に倒すと server 側 prepared statement を作らないため本問題が
	// 構造的に消える。性能は落ちるが test では問題ない。
	return gorm.Open(postgres.New(postgres.Config{
		DSN:                  dsn,
		PreferSimpleProtocol: true,
	}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
}

// openTestDBForPackage ensures the package's schema exists, then connects with
// search_path pinned to it.
//
// **database ではなく schema で分ける。** database を分けると `CREATE DATABASE`
// が要り、開発環境の test user は権限を持たないことがある (実際にローカルの `mk`
// は CREATEDB を持っていない)。schema なら DB の所有者権限だけで足りるので、
// 既存の環境をそのまま使える。
func openTestDBForPackage(pkg string) (*gorm.DB, error) {
	loadEnvFile()
	if pkg == "" {
		// 呼び出し元を特定できないときは共有 schema に倒す。隔離できないより、
		// 接続できずに全テストが落ちる方が困る。
		return openDSN(testDSN(""))
	}
	db, err := openDSN(testDSN(pkg))
	if err != nil {
		return nil, err
	}
	if err := ensureSchema(db, pkg); err != nil {
		if sqlDB, derr := db.DB(); derr == nil {
			_ = sqlDB.Close()
		}
		return nil, err
	}
	return db, nil
}

// ensureSchema creates the schema if it does not exist yet.
//
// `CREATE SCHEMA` は search_path に依存しないので、search_path が未作成の schema を
// 指した接続からでも実行できる。
//
// 同じ schema を複数のパッケージが同時に作りに来ることは無い (名前が呼び出し元
// パッケージ由来なので 1 対 1) が、**同じパッケージ内で並行に開く**ことはある
// (例: init と、接続を閉じた broken handler を作るテスト)。`IF NOT EXISTS` でも
// PostgreSQL は競合すると pg_namespace の unique 違反を上げるので、その場合は
// 「他が先に作った」として成功扱いにする。
func ensureSchema(db *gorm.DB, schema string) error {
	// identifier は自前で組む (プレースホルダを使えない)。schema は
	// callerPackage が [a-z0-9_] に正規化しているので注入の余地は無い。
	stmt := `CREATE SCHEMA IF NOT EXISTS "` + schema + `"`
	var lastErr error
	for attempt := 0; attempt < createSchemaAttempts; attempt++ {
		lastErr = db.Exec(stmt).Error
		if lastErr == nil || isBenignCreateSchemaError(lastErr) {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * createSchemaRetryBase)
	}
	return fmt.Errorf("create schema %s: %w", schema, lastErr)
}

const (
	// createSchemaAttempts bounds the concurrent-creation retry.
	createSchemaAttempts = 5
	// createSchemaRetryBase is multiplied by the attempt number (linear backoff).
	createSchemaRetryBase = 100 * time.Millisecond
)

// isBenignCreateSchemaError reports whether the error means "someone else got
// there first".
func isBenignCreateSchemaError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "SQLSTATE 42P06") || // duplicate_schema
		strings.Contains(msg, "SQLSTATE 23505") // pg_namespace unique violation
}

// callerPackage returns a sanitized identifier for the package that called the
// exported entry point, e.g. `internal/api/gallery` -> `internal_api_gallery`.
//
// リポジトリ root からの相対パスに直せない場合は**パス全体をそのまま**使う。
// `-trimpath` 付きでビルドすると runtime.Caller はモジュールパス始まりの文字列を
// 返し、相対化に失敗する。ここで空文字を返すと共有 schema に落ちて隔離が黙って
// 外れるので、一意でありさえすれば形は問わない方に倒す。
//
// PostgreSQL の identifier 上限を超える場合は末尾を残す (`internal/` 側より末端の
// パッケージ名の方が識別に効く)。
func callerPackage(skip int) string {
	_, file, _, ok := runtime.Caller(skip)
	if !ok {
		return ""
	}
	dir := filepath.Dir(file)
	rel, err := filepath.Rel(projectRoot(), dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = dir
	}
	return schemaName(filepath.ToSlash(rel))
}

// maxPackageSuffixLen is PostgreSQL's identifier limit. 超えると silent に
// 切り詰められ、別パッケージと同じ schema 名に化けうるので明示的に切る。
const maxPackageSuffixLen = 63

// projectRoot returns the repository root derived from this file's location.
func projectRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	// testutil/ -> internal/ -> project root
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// migrationLedgerTable records which migration files have been applied to the
// current schema.
//
// **これが無いと dropped column が実行のたびに増える** (#2756)。手元の schema は
// 実行をまたいで残るので、migration が毎回流れて列枠を食う。PostgreSQL は DROP
// した列も 1 テーブル 1600 列の上限に数えるため、繰り返すと最後は
// `tables can have at most 1600 columns` で落ちる。
//
// **再適用で実際に枠を食うのは `note` だけ** (実測。migration が作る 121 テーブル
// 中 1 つ。`internal_repository` を数えると 121 だが、増えた 1 つはこの台帳自身)。
// `000033` が `ADD COLUMN IF NOT EXISTS` で足し `000036` が落とすため。`DROP
// COLUMN` を含む migration は他に 3 本あるが、落とす列は `CREATE TABLE` 由来で
// 再適用時は `IF NOT EXISTS` の no-op になるので枠を食わない。
const migrationLedgerTable = "testutil_applied_migrations"

// ApplyMigrations applies the up migration SQL files that this schema has not
// seen yet.
//
// 台帳は **ファイル名 + 内容のハッシュ**で持つ。名前だけだと、migration を書き
// 換えながら開発しているときに反映されなくなる。
//
// **記録するのは成功したときだけ。** 失敗したものを「適用済み」にすると、
// 一過性の失敗 (並行 DDL / lock timeout / 新しい migration の実バグ) が恒久的に
// 隠れる。
//
// 既存 schema への再適用でエラーになるのは実測で **116 本中 2 本だけ**
// (`000001_initial` の制約重複と `000075_signup_application`)。どちらも
// 1 ファイル = 1 つの暗黙トランザクションなので丸ごとロールバックし、列枠を
// 食わない。**列枠を食う `000033` (ADD) / `000036` (DROP) は成功する側**なので、
// 成功時のみの記録でも #2756 は解消する。
func ApplyMigrations(db *gorm.DB) {
	files, err := findMigrationFiles()
	if err != nil {
		panic("failed to find migration files: " + err.Error())
	}
	applied := loadAppliedMigrations(db)
	for _, path := range files {
		sql, err := os.ReadFile(path)
		if err != nil {
			panic("failed to read migration file: " + err.Error())
		}
		key := filepath.Base(path)
		sum := fmt.Sprintf("%x", sha256.Sum256(sql))
		if applied[key] == sum {
			continue
		}
		if err := db.Exec(string(sql)).Error; err != nil {
			// 冪等なDDLのエラーは無視 (既にテーブルが存在する場合等)。
			// 記録しないので、次回また流して自己修復の余地を残す。
			continue
		}
		recordAppliedMigration(db, key, sum)
	}
}

// loadAppliedMigrations returns name → content hash for this schema. 台帳が
// 作れない / 読めないときは空を返す = 全部流す (今までの挙動)。
func loadAppliedMigrations(db *gorm.DB) map[string]string {
	out := map[string]string{}
	stmt := `CREATE TABLE IF NOT EXISTS "` + migrationLedgerTable + `" (
		name text PRIMARY KEY,
		sha  text NOT NULL
	)`
	if err := db.Exec(stmt).Error; err != nil {
		return out
	}
	var rows []struct {
		Name string
		Sha  string
	}
	if err := db.Raw(`SELECT name, sha FROM "` + migrationLedgerTable + `"`).Scan(&rows).Error; err != nil {
		return out
	}
	for _, r := range rows {
		out[r.Name] = r.Sha
	}
	return out
}

// recordAppliedMigration marks one migration as applied. 同じパッケージが並行に
// 開くことがあるので upsert にする。
func recordAppliedMigration(db *gorm.DB, name, sum string) {
	_ = db.Exec(`INSERT INTO "`+migrationLedgerTable+`" (name, sha) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET sha = EXCLUDED.sha`, name, sum).Error
}

func findMigrationFiles() ([]string, error) {
	_, thisFile, _, _ := runtime.Caller(0)
	projectRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	dir := filepath.Join(projectRoot, "migration")
	matches, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		return nil, err
	}
	return matches, nil
}

// MustOpenTestDB は OpenTestDB のパニック版。init() で使う。
func MustOpenTestDB() *gorm.DB {
	// callerPackage を OpenTestDB 経由で呼ぶと 1 段深くなり、この関数自身の
	// パッケージ (testutil) を拾ってしまうので直接組む。
	db, err := openTestDBForPackage(callerPackage(2))
	if err != nil {
		panic("failed to open test DB: " + err.Error())
	}
	return db
}

// EnvOrDefault returns the environment variable value or the fallback.
func EnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadEnvFile loads .env.test from the project root (best-effort).
func loadEnvFile() {
	// プロジェクトルートの .env.test を探す
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return
	}
	// testutil/ → internal/ → project root
	root := filepath.Join(filepath.Dir(file), "..", "..")
	envPath := filepath.Join(root, ".env.test")

	data, err := os.ReadFile(envPath)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		// 既に環境変数が設定されていればスキップ
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}
