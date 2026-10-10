package backup

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/cli/migrate"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/testutil"
)

// daemonSandboxPort is the port of verify's throwaway server inside the
// container.
const daemonSandboxPort = "5433"

// containerSandbox starts verify's throwaway server inside the test
// container and lets this process reach it through the mapped port.
//
// ホストの pg_* は 15 で 18 のサーバーを扱えないので、postgres:18-alpine の中で
// 動かす。プログラムは本番と同じ LocalSandbox.Run に、docker exec で中へ入る
// Runner を渡して動かす (initdb は root では動かないので postgres で)。
type containerSandbox struct {
	run        backup.LocalSandbox
	host       string
	mappedPort int
}

func (s containerSandbox) Run(ctx context.Context, cmd backup.SandboxCmd) error {
	return s.run.Run(ctx, cmd)
}

func (s containerSandbox) sh(ctx context.Context, script string, args ...string) (string, error) {
	var out bytes.Buffer
	err := s.Run(ctx, backup.SandboxCmd{Program: "sh", Args: append([]string{"-c", script, "sh"}, args...), Stdout: &out})
	return out.String(), err
}

func (s containerSandbox) MkdirTemp(ctx context.Context) (string, error) {
	// このプロセスから TCP で繋ぐので、hba は全ての接続を trust にする。待ち受けは
	// container の中だけで、ホストへは testcontainers の割り当てた port だけが出る。
	out, err := s.sh(ctx, `d=$(mktemp -d /tmp/verify-XXXXXX) && mkdir "$d/sock" && printf 'local all all trust\nhost all all all trust\n' >"$d/pg_hba.conf" && printf %s "$d"`)
	return strings.TrimSpace(out), err
}

func (s containerSandbox) RemoveAll(ctx context.Context, dir string) error {
	_, err := s.sh(ctx, `rm -rf "$1"`, dir)
	return err
}

func (s containerSandbox) Server(dir string) backup.SandboxServer {
	local := backup.LocalSandbox{}.Server(dir)
	opts := []string{"-c", "listen_addresses=0.0.0.0", "-p", daemonSandboxPort, "-c", "hba_file=" + dir + "/pg_hba.conf"}
	opts = append(opts, local.Options[2:]...)
	p, _ := strconv.Atoi(daemonSandboxPort)
	return backup.SandboxServer{Options: opts, ToolHost: local.ToolHost, ToolPort: p, Host: s.host, Port: s.mappedPort}
}

// startDaemonDB starts a PostgreSQL 18 container whose database "elythia"
// has Elythia's schema, as verify's usable stage requires.
func startDaemonDB(t *testing.T) (*tcpostgres.PostgresContainer, int64) {
	t.Helper()
	testutil.SkipIfNoDocker(t)
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("elythia"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithExposedPorts(daemonSandboxPort+"/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })

	core, _, err := migrate.LatestVersion(filepath.Join("..", "..", "..", migrate.CoreDir))
	require.NoError(t, err)
	require.NotZero(t, core)
	url, err := c.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	db, err := gorm.Open(postgres.Open(url), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	testutil.ApplyMigrations(db)
	for _, q := range []string{
		// golang-migrate が当てた後と同じ 1 行にする。
		`CREATE TABLE IF NOT EXISTS schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`,
		`DELETE FROM schema_migrations`,
		fmt.Sprintf(`INSERT INTO schema_migrations (version, dirty) VALUES (%d, false)`, core),
		`CREATE TABLE daemon_fixture (id int PRIMARY KEY)`,
		`INSERT INTO daemon_fixture SELECT generate_series(1, 5)`,
	} {
		require.NoError(t, db.Exec(q).Error, q)
	}
	return c, int64(core)
}

// The daemon takes a backup with backup.Take into a directory storage,
// verifies it with backup.Verify in a throwaway server, and prunes the old
// generation, all through the wiring "backup daemon" uses.
func TestDaemonTakesVerifiesAndPrunesWithRealImplementations(t *testing.T) {
	c, _ := startDaemonDB(t)
	ctx := context.Background()
	dir := markedDir(t)
	cfgPath := writeConfig(t, c, dirBackupYAML(dir, "  schedule:\n    interval: 24h\n    keep: 1\n    verify: true\n"))

	te, _, _ := testEnv(c)
	ve := defaultVerifyEnv()
	ve.coreDir = filepath.Join("..", "..", "..", migrate.CoreDir)
	ve.localDir = filepath.Join("..", "..", "..", migrate.LocalDir)
	host, err := c.Host(ctx)
	require.NoError(t, err)
	mapped, err := c.MappedPort(ctx, daemonSandboxPort+"/tcp")
	require.NoError(t, err)
	ve.newSandbox = func(b *config.BackupOptions) backup.Sandbox {
		return containerSandbox{
			run:        backup.LocalSandbox{Tools: b.Tools, Runner: dockerExecRunner{container: c.GetContainerID(), user: "postgres"}},
			host:       host,
			mappedPort: int(mapped.Num()),
		}
	}

	// 2 日前の世代を 1 つ置く。起動したとき、最新の世代が間隔より古いので、すぐに取る。
	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)
	st, err := backup.OpenStorage(cfg.Backup.Storage)
	require.NoError(t, err)
	oldOpts, err := takeOptions(te, cfg, st, discardLogger())
	require.NoError(t, err)
	oldOpts.Now = func() time.Time { return time.Now().Add(-48 * time.Hour) }
	old, err := backup.Take(ctx, oldOpts)
	require.NoError(t, err)

	var stderr syncBuffer
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	e := daemonEnv{
		stderr:        &stderr,
		loadConfig:    config.Load,
		openDeps:      wireDeps(te, ve),
		signalContext: func() (context.Context, context.CancelFunc) { return runCtx, cancel },
		listen:        func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) },
		location:      time.UTC,
	}
	code := make(chan int, 1)
	go func() { code <- runDaemon(e, []string{"-config", cfgPath}) }()

	var gens []backup.Generation
	require.Eventually(t, func() bool {
		gens, err = backup.ListGenerations(ctx, st)
		return err == nil && len(gens) == 1 && gens[0].ID != old.ID && gens[0].Verify != nil
	}, 3*time.Minute, 200*time.Millisecond, "a new verified generation replaces the old one; log:\n%s", &stderr)
	cancel()
	require.Equal(t, 0, <-code, stderr.String())

	g := gens[0]
	assert.True(t, g.Complete())
	require.NotNil(t, g.Verify)
	assert.True(t, g.Verify.OK, "%+v", g.Verify)
	assert.Equal(t, int64(5), g.Meta.RowCounts["public.daemon_fixture"])
	assert.Empty(t, g.Verify.Mismatches)
	log := stderr.String()
	assert.Contains(t, log, "backup: deleted an old generation")
	assert.Contains(t, log, "generation="+old.ID)
	assert.NotContains(t, log, "level=ERROR")
	// 使い捨てのサーバーは残らない。
	sb := ve.newSandbox(cfg.Backup).(containerSandbox)
	out, err := sb.sh(ctx, `ls -d /tmp/verify-* 2>/dev/null || true`)
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(out))
	_, err = os.Stat(filepath.Join(dir, backup.DirMarkerFile))
	require.NoError(t, err, "pruning never touches the marker")
}
