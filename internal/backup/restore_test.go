package backup

import (
	"bytes"
	"context"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/elythia-network/elythia/internal/core/selfcheck"
)

const (
	rsAppUser = "elythia"
	rsAppPass = "elythia-pass"
	rsAppDB   = "elythia"
	rsGenID   = "20261010T030000Z"
)

// rsSetupInstance creates the application role and database the way an
// operator does, migrates it and writes a root user, one note and
// per-database settings.
func rsSetupInstance(t *testing.T, pg *rsPG, createdb bool) {
	t.Helper()
	attr := "NOCREATEDB"
	if createdb {
		attr = "CREATEDB"
	}
	pg.exec(t, "postgres",
		"CREATE ROLE "+rsAppUser+" LOGIN PASSWORD '"+rsAppPass+"' "+attr,
		"CREATE DATABASE "+rsAppDB+" OWNER "+rsAppUser,
		"ALTER DATABASE "+rsAppDB+" SET work_mem = '8MB'",
		`ALTER DATABASE `+rsAppDB+` SET search_path = "$user", public, "a,b"`,
	)
	pg.migrate(t, rsAppUser, rsAppPass, rsAppDB)
	app, err := pgx.Connect(context.Background(), pg.url("postgres", rsAppUser, rsAppPass, rsAppDB))
	require.NoError(t, err)
	defer app.Close(context.Background())
	for _, s := range []string{
		`INSERT INTO "user" ("id", "username", "usernameLower") VALUES ('u1', 'root', 'root')`,
		`UPDATE meta SET "rootUserId" = 'u1'`,
		`INSERT INTO meta ("id", "rootUserId") SELECT 'x', 'u1' WHERE NOT EXISTS (SELECT 1 FROM meta)`,
		`INSERT INTO note ("id", "userId", "visibility") VALUES ('n1', 'u1', 'public')`,
	} {
		_, err := app.Exec(context.Background(), s)
		require.NoError(t, err, s)
	}
}

func rsGorm(t *testing.T, pg *rsPG, db string) (*gorm.DB, func()) {
	t.Helper()
	return rsGormAs(t, pg, rsAppUser, rsAppPass, db)
}

func rsGormAs(t *testing.T, pg *rsPG, user, pass, db string) (*gorm.DB, func()) {
	t.Helper()
	g, err := gorm.Open(postgres.Open(pg.url("postgres", user, pass, db)),
		&gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	return g, func() {
		if s, err := g.DB(); err == nil {
			_ = s.Close()
		}
	}
}

// rsDoctor runs CheckRestored and requires every check to pass.
func rsDoctor(t *testing.T, pg *rsPG, db string, rdb redis.UniversalClient) {
	t.Helper()
	g, closeDB := rsGorm(t, pg, db)
	defer closeDB()
	report, err := CheckRestored(context.Background(), RestoreCheckDeps{
		DB: g, Redis: rdb, CoreMigrationsDir: rsCoreMigrations,
	})
	require.NoError(t, err)
	for _, r := range report.Results {
		assert.Equal(t, selfcheck.StatusOK, r.Status, "%s: %s", r.Name, r.Detail)
	}
	assert.True(t, report.OK)
}

func rsStartRedis(t *testing.T) *redis.Client {
	t.Helper()
	ctx := context.Background()
	c, err := tcredis.Run(ctx, "redis:7-alpine",
		testcontainers.WithWaitStrategy(wait.ForListeningPort("6379/tcp").WithStartupTimeout(30*time.Second)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	s, err := c.ConnectionString(ctx)
	require.NoError(t, err)
	opts, err := redis.ParseURL(s)
	require.NoError(t, err)
	rdb := redis.NewClient(opts)
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func rsDatabaseSettings(t *testing.T, pg *rsPG, db string) []string {
	t.Helper()
	conn := pg.connect(t, rsSuperUser, rsSuperPass, "postgres")
	var out []string
	require.NoError(t, conn.QueryRow(context.Background(), `
		SELECT coalesce(array_agg(c ORDER BY c), '{}') FROM pg_db_role_setting s
		JOIN pg_database d ON d.oid = s.setdatabase, unnest(s.setconfig) c
		WHERE d.datname = $1 AND s.setrole = 0`, db).Scan(&out))
	return out
}

// TestRestoreSwap takes an encrypted generation into MinIO with Take
// (#3458), restores it on the same server, then checks the swapped
// database, the kept database, the per-database settings, Redis, the doctor
// checks, and that renaming back undoes the restore (#3461 完了条件 1).
func TestRestoreSwap(t *testing.T) {
	pg := rsStartPG(t, "postgres:18-alpine")
	rdb := rsStartRedis(t)
	rsSetupInstance(t, pg, true)
	storage := newS3Storage(t)

	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	meta := rsTakeBackup(t, pg, storage, rsAppDB, rsGenID, rsBackupOptions{encrypt: id.Recipient(), verified: true})
	// (1) は pg_db_role_setting.setconfig の "name=value" を並べ替えて記録する。
	// 一覧の設定は引用符付きのまま入り、戻すときに要素ごとに分けて入れ直す。
	require.Equal(t, []string{`search_path="$user", public, "a,b"`, "work_mem=8MB"}, meta.DatabaseSettings)
	require.True(t, meta.Encrypted)

	// バックアップより後の投稿。Redis のタイムラインにも載っている。
	pg.exec(t, rsAppDB, `INSERT INTO note ("id", "userId", "visibility") VALUES ('n2', 'u1', 'public')`)
	ctx := context.Background()
	const prefix = "elythia.example:"
	require.NoError(t, rdb.RPush(ctx, prefix+"list:homeTimeline:u1", "n2", "n1").Err())
	require.NoError(t, rdb.RPush(ctx, prefix+"list:localTimeline", "n2").Err())
	require.NoError(t, rdb.ZAdd(ctx, "antennaTimeline:a1", redis.Z{Score: 1, Member: "n2"}).Err())
	require.NoError(t, rdb.Set(ctx, prefix+"cleanRemoteNotes:cursor", "n2", 0).Err())
	// 消さないもの。
	require.NoError(t, rdb.XAdd(ctx, &redis.XAddArgs{Stream: prefix + "notificationTimeline:u1", Values: map[string]any{"data": "{}"}}).Err())
	require.NoError(t, rdb.Set(ctx, "passwordguard:failures:u1", "3", 0).Err())
	require.NoError(t, rdb.RPush(ctx, "bull:deliver:wait", "1").Err())

	var out bytes.Buffer
	r := pg.restorer(storage, rsAppUser, rsAppPass)
	r.Identities = []age.Identity{id}
	r.Out = &out
	res, err := r.Restore(ctx, RestoreOptions{ID: LatestGeneration, Mode: RestoreSwap, Database: rsAppDB, Confirm: rsAppDB})
	require.NoError(t, err, out.String())
	assert.Equal(t, rsGenID, res.ID)
	assert.True(t, res.Verified)
	assert.Equal(t, "elythia_before_restore_20261010120000", res.BeforeRestore)
	assert.Equal(t, meta.DatabaseSettings, res.Settings)
	require.Len(t, res.Migrations, 1)
	assert.Equal(t, meta.Migrations[0].Version, res.Migrations[0].Version)

	app := pg.connect(t, rsAppUser, rsAppPass, rsAppDB)
	assert.Equal(t, int64(1), rsCount(t, app, `SELECT count(*) FROM note`))
	assert.Equal(t, int64(0), rsCount(t, app, `SELECT count(*) FROM note WHERE id = 'n2'`))
	before := pg.connect(t, rsAppUser, rsAppPass, res.BeforeRestore)
	assert.Equal(t, int64(1), rsCount(t, before, `SELECT count(*) FROM note WHERE id = 'n2'`))
	// 戻した DB の持ち主は元の DB と同じ。
	assert.Equal(t, int64(1), rsCount(t, app, `SELECT count(*) FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba WHERE d.datname = $1 AND r.rolname = $2`, rsAppDB, rsAppUser))
	assert.ElementsMatch(t, meta.DatabaseSettings, rsDatabaseSettings(t, pg, rsAppDB))
	var sp string
	require.NoError(t, app.QueryRow(ctx, `SHOW search_path`).Scan(&sp))
	assert.Equal(t, `"$user", public, "a,b"`, sp)
	_ = app.Close(ctx)
	_ = before.Close(ctx)

	cleaned, err := CleanRedisAfterRestore(ctx, RedisCleanupTargets{
		Default:   RedisCleanupTarget{Client: rdb, Prefix: prefix},
		Timelines: RedisCleanupTarget{Client: rdb, Prefix: prefix},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(4), cleaned.Total())
	// タイムラインに、戻した DB に無い投稿が残っていない。
	keys, err := rdb.Keys(ctx, prefix+"list:*").Result()
	require.NoError(t, err)
	assert.Empty(t, keys)
	for _, k := range []string{prefix + "notificationTimeline:u1", "passwordguard:failures:u1", "bull:deliver:wait"} {
		assert.Equal(t, int64(1), rdb.Exists(ctx, k).Val(), k)
	}

	rsDoctor(t, pg, rsAppDB, rdb)

	// 退避した DB へ名前を戻すと、元に戻る。
	pg.exec(t, "postgres",
		"ALTER DATABASE "+rsAppDB+" RENAME TO elythia_restored",
		"ALTER DATABASE "+res.BeforeRestore+" RENAME TO "+rsAppDB)
	back := pg.connect(t, rsAppUser, rsAppPass, rsAppDB)
	assert.Equal(t, int64(2), rsCount(t, back, `SELECT count(*) FROM note`))
}

// TestRestoreEmptyFromPostgres16 takes a backup from a PostgreSQL 16 server
// with the pg_dump of 18 and restores it into an empty database on an 18
// server (#3461 完了条件 2).
func TestRestoreEmptyFromPostgres16(t *testing.T) {
	ctx := context.Background()
	nw, err := network.New(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = nw.Remove(context.Background()) })
	pg16 := rsStartPG(t, "postgres:16-alpine", network.WithNetwork([]string{"pg16"}, nw))
	pg18 := rsStartPG(t, "postgres:18-alpine", network.WithNetwork([]string{"pg18"}, nw))
	rdb := rsStartRedis(t)

	rsSetupInstance(t, pg16, false)

	// 18 の pg_restore では、16 のサーバーへは戻さない (何も作る前に止まる)。
	onto16 := pg16.restorer(newRSStorage(), rsSuperUser, rsSuperPass)
	onto16.Exec = rsDockerExec(pg18.id)
	for _, mode := range []RestoreMode{RestoreSwap, RestoreEmpty} {
		err := onto16.CheckTarget(ctx, RestoreOptions{Mode: mode, Database: rsAppDB, Confirm: rsAppDB})
		require.ErrorIs(t, err, ErrRestoreVersion, mode)
		assert.Contains(t, err.Error(), "pg_restore is 18 but the server is 16")
	}

	storage, err := NewDirStorage(markedDir(t))
	require.NoError(t, err)
	meta := rsTakeBackup(t, pg16, storage, rsAppDB, rsGenID, rsBackupOptions{dumper: pg18, dumpHost: "pg16", verified: true})
	assert.Equal(t, 16, meta.PostgresVersionNum/10000)
	assert.Contains(t, meta.PgDumpVersion, "18.")

	// 新しいサーバーでは、管理者が空の DB と role を用意する。
	pg18.exec(t, "postgres",
		"CREATE ROLE "+rsAppUser+" LOGIN PASSWORD '"+rsAppPass+"'",
		"CREATE DATABASE "+rsAppDB+" OWNER "+rsAppUser)

	var out bytes.Buffer
	r := pg18.restorer(storage, rsAppUser, rsAppPass)
	r.Out = &out
	res, err := r.Restore(ctx, RestoreOptions{ID: rsGenID, Mode: RestoreEmpty, Database: rsAppDB, Confirm: rsAppDB})
	require.NoError(t, err, out.String())
	assert.Empty(t, res.BeforeRestore)

	app := pg18.connect(t, rsAppUser, rsAppPass, rsAppDB)
	var ver int
	require.NoError(t, app.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&ver))
	assert.Equal(t, 18, ver/10000)
	assert.Equal(t, int64(1), rsCount(t, app, `SELECT count(*) FROM note WHERE id = 'n1'`))
	assert.ElementsMatch(t, meta.DatabaseSettings, rsDatabaseSettings(t, pg18, rsAppDB))
	rsDoctor(t, pg18, rsAppDB, rdb)
}
