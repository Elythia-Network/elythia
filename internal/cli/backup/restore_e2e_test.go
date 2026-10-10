package backup

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	gomigrate "github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	bkp "github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/config"
)

// TestRestoreCommandEndToEnd takes a generation with "elythia backup take"
// (#3458) into a marked directory, then runs "elythia backup restore" with
// the storage the config selects and the real database, Redis and check
// steps: a swap, then a rollback. pg_dump runs inside the database
// container and pg_restore in a postgres:18-alpine container on the host
// network, standing in for the backup image (the host's tools are older
// than the server).
func TestRestoreCommandEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI is not available")
	}
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("postgres"), tcpostgres.WithUsername("test"), tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(60*time.Second)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Terminate(context.Background()) })
	rc, err := tcredis.Run(ctx, "redis:7-alpine",
		testcontainers.WithWaitStrategy(wait.ForListeningPort("6379/tcp").WithStartupTimeout(30*time.Second)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rc.Terminate(context.Background()) })
	pgPort, err := pg.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)
	redisPort, err := rc.MappedPort(ctx, "6379/tcp")
	require.NoError(t, err)

	url := func(user, pass, db string) string {
		return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable", user, pass, pgPort.Num(), db)
	}
	exec1 := func(db string, sqls ...string) {
		conn, err := pgx.Connect(ctx, url("test", "test", db))
		require.NoError(t, err)
		defer conn.Close(ctx)
		for _, s := range sqls {
			_, err := conn.Exec(ctx, s)
			require.NoError(t, err, s)
		}
	}
	exec1("postgres", "CREATE ROLE app LOGIN PASSWORD 'app-pass' CREATEDB",
		"CREATE DATABASE src OWNER app", "CREATE DATABASE elythia OWNER app")
	for _, db := range []string{"src", "elythia"} {
		m, err := gomigrate.New("file://"+rsCoreMigrations, strings.Replace(url("app", "app-pass", db), "postgres://", "pgx5://", 1))
		require.NoError(t, err)
		require.NoError(t, m.Up())
		_, _ = m.Close()
		exec1(db, `INSERT INTO "user" ("id", "username", "usernameLower") VALUES ('u1', 'root', 'root')`,
			`INSERT INTO meta ("id", "rootUserId") SELECT 'x', 'u1' WHERE NOT EXISTS (SELECT 1 FROM meta)`,
			`UPDATE meta SET "rootUserId" = 'u1'`,
			`INSERT INTO note ("id", "userId", "visibility") VALUES ('in_`+db+`', 'u1', 'public')`)
	}

	// src のバックアップを、(1) の take で目印付きのディレクトリへ取る。
	dir := t.TempDir()
	require.NoError(t, bkp.CreateDirMarker(dir))
	cfgFor := func(db, extra string) string {
		path := filepath.Join(t.TempDir(), "default.yml")
		require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf(`url: https://elythia.example/
port: 3000
db:
  host: 127.0.0.1
  port: %d
  db: %s
  user: app
  pass: app-pass
redis:
  host: 127.0.0.1
  port: %d
backup:
  storage:
    type: dir
    dir:
      path: %s
%s`, pgPort.Num(), db, redisPort.Num(), dir, extra)), 0o600))
		return path
	}
	te, takeOut, takeErr := testEnv(nil)
	te.runner = dockerExecRunner{container: pg.GetContainerID()}
	te.dumpConn = func(*config.Config) (bkp.DumpConn, error) {
		return bkp.DumpConn{URI: "postgresql://app@localhost:5432/src?sslmode=disable", Password: "app-pass"}, nil
	}
	require.Equal(t, 0, take(ctx, te, []string{"-config", cfgFor("src", "")}), takeErr.String())
	id := regexp.MustCompile(`generation (\d{8}T\d{6}Z)`).FindStringSubmatch(takeOut.String())
	require.Len(t, id, 2, takeOut.String())

	wrapper := filepath.Join(t.TempDir(), "pg_restore")
	require.NoError(t, os.WriteFile(wrapper, []byte("#!/bin/sh\nexec docker run --rm -i --network host -e PGPASSWORD postgres:18-alpine pg_restore \"$@\"\n"), 0o700))
	cfgPath := cfgFor("elythia", "  tools:\n    pgRestore: "+wrapper+"\n")

	e := defaultRestoreEnv()
	var out, errOut bytes.Buffer
	e.stdout, e.stderr = &out, &errOut
	code := restore(e, []string{"-config", cfgPath, "-id", id[1], "-mode", "swap", "-confirm", "elythia", "-migrations", rsCoreMigrations})
	require.Equal(t, 0, code, out.String()+errOut.String())
	before := regexp.MustCompile(`kept as (elythia_before_restore_\d{14})`).FindStringSubmatch(out.String())
	require.Len(t, before, 2, out.String())

	noteIn := func(db string) string {
		conn, err := pgx.Connect(ctx, url("app", "app-pass", db))
		require.NoError(t, err)
		defer conn.Close(ctx)
		var id string
		require.NoError(t, conn.QueryRow(ctx, `SELECT id FROM note`).Scan(&id))
		return id
	}
	assert.Equal(t, "in_src", noteIn("elythia"))
	assert.Equal(t, "in_elythia", noteIn(before[1]))

	out.Reset()
	errOut.Reset()
	code = restore(e, []string{"-config", cfgPath, "-rollback", before[1], "-confirm", "elythia", "-migrations", rsCoreMigrations})
	require.Equal(t, 0, code, out.String()+errOut.String())
	assert.Equal(t, "in_elythia", noteIn("elythia"))
}
