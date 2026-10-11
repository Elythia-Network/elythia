package maintmode

import (
	"context"
	"fmt"
	"net"
	"sync"
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

/*
 * 判定役の結合テスト。共有のテスト用 DB (misskey_test) には、ほかのパッケージの
 * テストも繋ぐので、接続の数が 0 にならない。専用の PostgreSQL 18 を立てる。
 *
 * 本番と同じく、判定役は non-superuser (対象の DB の所有者) で動かす。
 */

const (
	jiSuperUser = "test"
	jiSuperPass = "test"
	jiAppUser   = "app"
	jiAppPass   = "app"
	jiAppDB     = "appdb"
)

var (
	jiOnce sync.Once
	jiPG   *tcpostgres.PostgresContainer
	jiErr  error
)

type jiServer struct {
	host string
	port string
}

func startJudgePG(t *testing.T) jiServer {
	t.Helper()
	testutil.SkipIfNoDocker(t)
	jiOnce.Do(func() {
		ctx := context.Background()
		jiPG, jiErr = tcpostgres.Run(ctx, "postgres:18-alpine",
			tcpostgres.WithDatabase("postgres"),
			tcpostgres.WithUsername(jiSuperUser),
			tcpostgres.WithPassword(jiSuperPass),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
			),
		)
		if jiErr != nil {
			return
		}
		var s jiServer
		if s, jiErr = jiAddr(ctx); jiErr != nil {
			return
		}
		conn, err := pgx.Connect(ctx, s.dsn(jiSuperUser, jiSuperPass, "postgres", ""))
		if err != nil {
			jiErr = err
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		for _, q := range []string{
			fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' CREATEDB`, jiAppUser, jiAppPass),
			fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, jiAppDB, jiAppUser),
		} {
			if _, jiErr = conn.Exec(ctx, q); jiErr != nil {
				return
			}
		}
	})
	require.NoError(t, jiErr)
	s, err := jiAddr(context.Background())
	require.NoError(t, err)
	return s
}

func jiAddr(ctx context.Context) (jiServer, error) {
	host, err := jiPG.Host(ctx)
	if err != nil {
		return jiServer{}, err
	}
	port, err := jiPG.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return jiServer{}, err
	}
	return jiServer{host: host, port: port.Port()}, nil
}

func (s jiServer) dsn(user, pass, db, app string) string {
	d := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", user, pass, net.JoinHostPort(s.host, s.port), db)
	if app != "" {
		d += "&application_name=" + app
	}
	return d
}

func (s jiServer) connect(t *testing.T, user, pass, db, app string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), s.dsn(user, pass, db, app))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func (s jiServer) counter() *PGCounter {
	return &PGCounter{Targets: []PGTarget{{
		Name: "primary", DSN: s.dsn(jiAppUser, jiAppPass, "postgres", "elythia-judge/test"), Database: jiAppDB,
	}}}
}

// waitNoSessions waits until the earlier subtests' sessions are gone.
func waitNoSessions(t *testing.T, s jiServer) {
	t.Helper()
	require.Eventually(t, func() bool {
		got, err := s.counter().Sessions(context.Background())
		return err == nil && len(got) == 0
	}, 10*time.Second, 50*time.Millisecond)
}

func TestJudgeIntegration(t *testing.T) {
	pg := startJudgePG(t)
	ctx := context.Background()

	// P1: application_name の無い生の接続が残っている間は held にしない。
	t.Run("a raw session keeps the maintenance entering", func(t *testing.T) {
		waitNoSessions(t, pg)
		s, _ := newTestStore(t)
		c := newClock()
		j := c.judge(s, pg.counter(), "j")
		raw := pg.connect(t, jiAppUser, jiAppPass, jiAppDB, "")
		var pid int
		require.NoError(t, raw.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
		// 別の DB への接続は数えない。
		_ = pg.connect(t, jiAppUser, jiAppPass, "postgres", "elythia/server/other-db")
		require.NoError(t, s.Enter(ctx, State{OpID: "op"}))

		out, err := j.Step(ctx)
		require.NoError(t, err)
		assert.Equal(t, Waiting, out)
		st, _ := s.Get(ctx)
		require.Len(t, st.Blockers, 1)
		assert.Equal(t, pid, st.Blockers[0].PID)
		assert.True(t, st.Blockers[0].SameRole)

		require.NoError(t, raw.Close(ctx))
		require.Eventually(t, func() bool {
			out, err := j.Step(ctx)
			return err == nil && out == MovedToHeld
		}, 10*time.Second, 50*time.Millisecond)
	})

	// P4: 長いクエリを抱えた Elythia の接続は、上限の後で切られて held になる。
	// 次の enter も詰まらない (4 周目の High: 報告が 1 時間残って enter が全部失敗した)。
	t.Run("a long Elythia query is terminated after the drain timeout", func(t *testing.T) {
		waitNoSessions(t, pg)
		s, _ := newTestStore(t)
		c := newClock()
		j := c.judge(s, pg.counter(), "j")
		long := pg.connect(t, jiAppUser, jiAppPass, jiAppDB, "elythia/server/p1")
		sleepDone := make(chan error, 1)
		go func() {
			_, err := long.Exec(ctx, `SELECT pg_sleep(600)`)
			sleepDone <- err
		}()
		require.Eventually(t, func() bool {
			got, _ := pg.counter().Sessions(ctx)
			return len(got) == 1
		}, 10*time.Second, 50*time.Millisecond)
		// 持ち主のプロセスは手放し終えた (drained) と言っているが、見捨てたクエリが
		// 接続を掴んでいる。
		require.NoError(t, s.Report(ctx, ProcReport{ID: "p1", State: ProcDrained}))
		require.NoError(t, s.Enter(ctx, State{OpID: "op1"}))
		_, err := j.Step(ctx)
		require.NoError(t, err)
		assert.Equal(t, PhaseEntering, phase(t, s))

		c.add(31 * time.Second)
		_, err = j.Step(ctx)
		require.NoError(t, err)
		select {
		case err := <-sleepDone:
			require.Error(t, err, "切られたクエリが成功した")
		case <-time.After(10 * time.Second):
			t.Fatal("上限を過ぎても Elythia の接続を切らなかった")
		}
		require.Eventually(t, func() bool {
			out, err := j.Step(ctx)
			return err == nil && out == MovedToHeld
		}, 10*time.Second, 50*time.Millisecond)

		// 抜けて (プロセスは戻って running)、すぐ入り直せる (プロセスはすぐ手放す)。
		require.NoError(t, s.Report(ctx, ProcReport{ID: "p1", State: ProcRunning}))
		require.NoError(t, s.SetPhase(ctx, "op1", PhaseResuming))
		out, err := j.Step(ctx)
		require.NoError(t, err)
		require.Equal(t, Cleared, out)
		require.NoError(t, s.Enter(ctx, State{OpID: "op2"}))
		require.NoError(t, s.Report(ctx, ProcReport{ID: "p1", State: ProcDrained}))
		out, err = j.Step(ctx)
		require.NoError(t, err)
		assert.Equal(t, MovedToHeld, out, "抜けた直後の enter が詰まった")
	})

	// P5: 運営者の psql と、別の role の接続は切らない。
	t.Run("other sessions are not terminated", func(t *testing.T) {
		waitNoSessions(t, pg)
		s, _ := newTestStore(t)
		c := newClock()
		j := c.judge(s, pg.counter(), "j")
		psql := pg.connect(t, jiAppUser, jiAppPass, jiAppDB, "psql")
		other := pg.connect(t, jiSuperUser, jiSuperPass, jiAppDB, "elythia/server/other-role")
		require.NoError(t, s.Enter(ctx, State{OpID: "op"}))
		_, err := j.Step(ctx)
		require.NoError(t, err)
		c.add(time.Hour)
		_, err = j.Step(ctx)
		require.NoError(t, err)
		st, _ := s.Get(ctx)
		assert.Equal(t, PhaseEntering, st.Phase)
		assert.Empty(t, st.Terminated)
		require.Len(t, st.Blockers, 2, "別の role の接続を見落とした: %v", st.Blockers)
		for _, conn := range []*pgx.Conn{psql, other} {
			assert.NoError(t, conn.Ping(ctx), "切られた")
		}
	})
}
