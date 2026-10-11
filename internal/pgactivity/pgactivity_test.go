package pgactivity

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/elythia-network/elythia/internal/testutil"
)

// 共有のテスト用 DB には、ほかのパッケージのテストも繋ぐ。専用のサーバーを立てる。
func startPG(t *testing.T) func(user, pass, db, app string) *pgx.Conn {
	t.Helper()
	testutil.SkipIfNoDocker(t)
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	testcontainers.CleanupContainer(t, c)
	require.NoError(t, err)
	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)
	connect := func(user, pass, db, app string) *pgx.Conn {
		dsn := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable&application_name=%s",
			user, pass, net.JoinHostPort(host, port.Port()), db, app)
		conn, err := pgx.Connect(ctx, dsn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
		return conn
	}
	super := connect("test", "test", "postgres", "setup")
	for _, q := range []string{
		`CREATE ROLE app LOGIN PASSWORD 'app'`,
		`CREATE DATABASE appdb OWNER app`,
	} {
		_, err := super.Exec(ctx, q)
		require.NoError(t, err)
	}
	return connect
}

func TestListAndTerminate(t *testing.T) {
	connect := startPG(t)
	ctx := context.Background()
	// 数える側は non-superuser で、対象とは別の DB に繋ぐ。
	counter := connect("app", "app", "postgres", "counter")
	mine := connect("app", "app", "appdb", "elythia/server/1")
	other := connect("test", "test", "appdb", "psql")
	_ = connect("app", "app", "postgres", "elsewhere")
	var minePID, otherPID int
	require.NoError(t, mine.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&minePID))
	require.NoError(t, other.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&otherPID))

	sessions, err := List(ctx, counter, "appdb")
	require.NoError(t, err)
	require.Len(t, sessions, 2, "%v", sessions)
	byPID := map[int]Session{}
	for _, s := range sessions {
		byPID[s.PID] = s
	}
	assert.Equal(t, Session{PID: minePID, User: "app", Application: "elythia/server/1", Addr: byPID[minePID].Addr, SameRole: true}, byPID[minePID])
	assert.NotEmpty(t, byPID[minePID].Addr, "同じ role の行は接続元が読める")
	// 別の role の行は backend_type と client_addr が null になる。それでも数える。
	assert.Equal(t, Session{PID: otherPID, User: "test", Application: "psql"}, byPID[otherPID])
	assert.Contains(t, byPID[otherPID].String(), "from=unknown")
	assert.Contains(t, Join(sessions), "; ")

	// 自分の接続は数えない。
	self, err := List(ctx, mine, "appdb")
	require.NoError(t, err)
	assert.Len(t, self, 1)

	// 同じ role は切れる。別の role は権限で失敗する。
	ok, err := Terminate(ctx, counter, minePID)
	require.NoError(t, err)
	assert.True(t, ok)
	_, err = Terminate(ctx, counter, otherPID)
	assert.Error(t, err)
	require.Eventually(t, func() bool {
		s, err := List(ctx, counter, "appdb")
		return err == nil && len(s) == 1 && s[0].PID == otherPID
	}, 5*time.Second, 50*time.Millisecond)

	require.NoError(t, counter.Close(ctx))
	_, err = List(ctx, counter, "appdb")
	assert.Error(t, err)
	_, err = Terminate(ctx, counter, 1)
	assert.Error(t, err)
}
