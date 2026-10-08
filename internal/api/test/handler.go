// Package test exposes test-only HTTP endpoints used by Cypress e2e against mk-go.
//
// Every endpoint in this package is guarded by config.Config.TestMode. In
// production (TestMode=false) the routes are not registered at all, and any
// request to them returns the default Echo 404.
package test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/elythia-network/elythia/internal/repository"
	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// preservedTables are tables that must not be truncated during ResetDB.
//
// schema_migrations is managed by golang-migrate; wiping it would force the
// migrator to re-run every migration on the next boot and in the worst case
// corrupt the migration state. Misskey 本家は TypeORM migrations テーブル
// (`migrations`) を使っていないのでこの exclusion は mk-go 独自の配慮。
//
// schema_migrations_local is the same for a fork's own migration track
// (migration/local/, #3428). Elythia 本体の DB には無いが、fork の DB で消すと
// 次の migrate で fork の migration が全部再実行される。
var preservedTables = map[string]struct{}{
	"schema_migrations":       {},
	"schema_migrations_local": {},
}

// resetDBMaxRetries mirrors Misskey 本家 (`packages/backend/src/misc/reset-db.ts`)
// which retries the CASCADE DELETE loop up to 3 times.
const resetDBMaxRetries = 3

// Handler is the HTTP handler for test-only endpoints.
type Handler struct {
	db       *gorm.DB
	redis    *redis.Client
	metaRepo repository.MetaRepository
	testMode bool
}

// NewHandler constructs a new test Handler.
//
// db and redis are both required when testMode is true. They are accepted
// regardless so that test code that spins up a zero-value Handler cannot
// accidentally pass a nil dependency without it being caught by the runtime.
func NewHandler(db *gorm.DB, redisClient *redis.Client, metaRepo repository.MetaRepository, testMode bool) *Handler {
	return &Handler{
		db:       db,
		redis:    redisClient,
		metaRepo: metaRepo,
		testMode: testMode,
	}
}

// ResetDB wipes all user-visible tables and flushes Redis.
//
// This mirrors Misskey 本家の `/api/reset-db` エンドポイントで、
// Cypress の `resetState` カスタムコマンドが必須依存している。
// TestMode=false の場合は 404 を返して存在しないように振る舞う。
func (h *Handler) ResetDB(c echo.Context) error {
	if !h.testMode {
		return echo.NewHTTPError(http.StatusNotFound)
	}

	ctx := c.Request().Context()

	if h.redis != nil {
		if err := h.redis.FlushDB(ctx).Err(); err != nil {
			slog.Error("reset-db: redis FLUSHDB failed", "err", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "redis flush failed")
		}
	}

	if h.db != nil {
		if err := resetTables(ctx, h.db); err != nil {
			slog.Error("reset-db: table reset failed", "err", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "db reset failed")
		}
		// meta テーブルは TRUNCATE で消えるが、初期セットアップフロー
		// (/api/admin/accounts/create) が `SELECT * FROM meta` を前提にしているので
		// 空行を 1 つ再挿入する。MetaRepository 経由で行うことで
		// CachedMetaRepository のキャッシュも無効化される。
		if h.metaRepo != nil {
			if err := h.metaRepo.EnsureInitial("meta-singleton"); err != nil {
				slog.Error("reset-db: meta re-init failed", "err", err)
				return echo.NewHTTPError(http.StatusInternalServerError, "db reset failed")
			}
		}
	}

	return c.NoContent(http.StatusNoContent)
}

// resetTables collects user tables via pg_class/pg_namespace and truncates
// each one with TRUNCATE ... CASCADE. The retry loop exists because under
// CASCADE FK clauses, PostgreSQL may transiently complain about ordering.
func resetTables(ctx context.Context, db *gorm.DB) error {
	var lastErr error
	for attempt := 1; attempt <= resetDBMaxRetries; attempt++ {
		tables, err := listUserTables(ctx, db)
		if err != nil {
			lastErr = err
			continue
		}

		allOK := true
		for _, t := range tables {
			if _, skip := preservedTables[t]; skip {
				continue
			}
			// Identifier は pg_class から取得した安全な値に限定される。
			// それでも念のためクォートしてから TRUNCATE する。
			// DELETE ... CASCADE は PostgreSQL では構文エラーになるので使えない。
			// CASCADE 節が許されるのは TRUNCATE と DROP TABLE のみ。
			stmt := fmt.Sprintf(`TRUNCATE %q CASCADE`, t)
			if err := db.WithContext(ctx).Exec(stmt).Error; err != nil {
				lastErr = fmt.Errorf("truncate %s: %w", t, err)
				allOK = false
			}
		}
		if allOK {
			return nil
		}
	}
	if lastErr == nil {
		lastErr = errors.New("reset-db: exceeded max retries without a concrete error")
	}
	return lastErr
}

// listUserTables returns the non-system, non-TOAST ordinary tables in the
// current PostgreSQL database. The query mirrors Misskey 本家の
// packages/backend/src/misc/reset-db.ts.
func listUserTables(ctx context.Context, db *gorm.DB) ([]string, error) {
	// **public schema だけを対象にする。**
	//
	// プラグインは自分の schema (plugin_<name>) を持つ (#2481)。schema を問わず
	// 集めると、そこにあるテーブルを本体のものとして TRUNCATE しようとし、
	// search_path の外なので解決できずに失敗する (#2477 で e2e が落ちた)。
	//
	// プラグインのデータを消さないのは意図どおり。reset-db は mk-go 本体の
	// 状態を初期化するためのもので、プラグインの後片付けはプラグイン自身の
	// テストの責務。
	const query = `
		SELECT relname AS table_name
		FROM pg_class C
		LEFT JOIN pg_namespace N ON (N.oid = C.relnamespace)
		WHERE nspname = 'public'
			AND C.relkind = 'r'
	`
	var names []string
	if err := db.WithContext(ctx).Raw(query).Scan(&names).Error; err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	return names, nil
}
