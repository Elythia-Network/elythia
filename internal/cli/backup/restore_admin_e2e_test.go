package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	gomigrate "github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/datatypes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	bkp "github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/backup/daemon"
	"github.com/elythia-network/elythia/internal/backup/restoreop"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/maintmode"
	"github.com/elythia-network/elythia/internal/model"
)

// e2eMinioImage is the MinIO the backupadmin tests use.
const e2eMinioImage = "pgsty/minio:RELEASE.2026-08-04T00-00-00Z@sha256:b6bfe7239bfc83fb90d31612d9704d86039dd714f7904b3f1ad68f211e602372"

const (
	adminPassword = "correct horse battery"
	serviceToken  = "service-token-for-e2e"
	adminToken    = "tokadmin00000001"
	modToken      = "tokmoder00000001"
	no2faToken    = "tokno2fa00000001"
	guardToken    = "tokguard00000001"
	// late2faToken is an administrator who registers 2FA after the backup
	// was taken (#3463 (6d)).
	late2faToken = "toklate2fa000001"
)

// restoreE2E is the restore from the admin page wired to real servers: a
// PostgreSQL 18 (whose app role owns the database and may create others),
// Redis, MinIO and the backup service's control API.
type restoreE2E struct {
	t       *testing.T
	ctx     context.Context
	url     func(user, pass, db string) string
	rdb     *redis.Client
	base    string
	secret  string
	codes   []string
	codeMu  sync.Mutex
	genID   string
	maint   *maintmode.Store
	prefix  string
	stopped chan int
	cancel  context.CancelFunc
	// daemon is the backup service runDaemon built.
	daemon atomic.Pointer[daemon.Daemon]
}

func (r *restoreE2E) exec(db string, sqls ...string) {
	r.t.Helper()
	conn, err := pgx.Connect(r.ctx, r.url("app", "app-pass", db))
	require.NoError(r.t, err)
	defer conn.Close(r.ctx)
	for _, s := range sqls {
		_, err := conn.Exec(r.ctx, s)
		require.NoError(r.t, err, s)
	}
}

func (r *restoreE2E) notes(db string) []string {
	r.t.Helper()
	conn, err := pgx.Connect(r.ctx, r.url("app", "app-pass", db))
	require.NoError(r.t, err)
	defer conn.Close(r.ctx)
	rows, err := conn.Query(r.ctx, `SELECT id FROM note ORDER BY id`)
	require.NoError(r.t, err)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(r.t, err)
	return ids
}

// backupCode hands out one unused backup code of the administrator.
func (r *restoreE2E) backupCode() string {
	r.codeMu.Lock()
	defer r.codeMu.Unlock()
	c := r.codes[0]
	r.codes = r.codes[1:]
	return c
}

func (r *restoreE2E) call(path string, body map[string]any, clientIP string) (int, map[string]any) {
	r.t.Helper()
	b, err := json.Marshal(body)
	require.NoError(r.t, err)
	req, err := http.NewRequestWithContext(r.ctx, http.MethodPost, r.base+path, bytes.NewReader(b))
	require.NoError(r.t, err)
	req.Header.Set("Authorization", "Bearer "+serviceToken)
	req.Header.Set("Content-Type", "application/json")
	if clientIP == "" {
		clientIP = "192.0.2.10"
	}
	req.Header.Set("X-Elythia-Client-IP", clientIP)
	res, err := http.DefaultClient.Do(req)
	require.NoError(r.t, err)
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return res.StatusCode, out
}

func e2eCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func (r *restoreE2E) status(opID, ticket string) map[string]any {
	r.t.Helper()
	code, out := r.call("/restore/status", map[string]any{"i": adminToken, "opId": opID, "ticket": ticket}, "")
	require.Equal(r.t, http.StatusOK, code, out)
	rec, _ := out["record"].(map[string]any)
	return rec
}

func (r *restoreE2E) waitStep(opID, ticket string, steps ...string) map[string]any {
	r.t.Helper()
	var rec map[string]any
	require.Eventually(r.t, func() bool {
		rec = r.status(opID, ticket)
		for _, s := range steps {
			if rec["step"] == s {
				return true
			}
		}
		return false
	}, 3*time.Minute, 100*time.Millisecond, "waiting for %v", steps)
	return rec
}

func (r *restoreE2E) prepare() (string, string) {
	r.t.Helper()
	code, out := r.call("/restore/prepare", map[string]any{"i": adminToken, "id": r.genID, "password": adminPassword, "token": r.backupCode()}, "")
	require.Equal(r.t, http.StatusOK, code, out)
	opID, _ := out["opId"].(string)
	ticket, _ := out["ticket"].(string)
	r.waitStep(opID, ticket, restoreop.StepReady)
	return opID, ticket
}

// fakeServer plays one Elythia process: it keeps a session on the database
// named application_name "elythia/server/<id>", lets go of it while the
// maintenance holds, and connects again when it resumes.
type fakeServer struct {
	r      *restoreE2E
	id     string
	mu     sync.Mutex
	conn   *pgx.Conn
	seen   []string
	cancel context.CancelFunc
	done   chan struct{}
}

func (f *fakeServer) run(ctx context.Context) {
	defer close(f.done)
	for {
		st, err := f.r.maint.Get(ctx)
		if err == nil {
			state := maintmode.ProcRunning
			f.mu.Lock()
			if st != nil && st.Holding() {
				if f.conn != nil {
					_ = f.conn.Close(context.Background())
					f.conn = nil
				}
				state = maintmode.ProcDrained
			} else if f.conn == nil {
				c, err := pgx.Connect(ctx, f.r.url("app", "app-pass", "elythia")+"&application_name=elythia/server/"+f.id)
				if err == nil {
					f.conn = c
					rows, err := c.Query(ctx, `SELECT id FROM note ORDER BY id`)
					if err == nil {
						f.seen, _ = pgx.CollectRows(rows, pgx.RowTo[string])
					}
				} else {
					state = maintmode.ProcResumeFailed
				}
			}
			f.mu.Unlock()
			_ = f.r.maint.Report(ctx, maintmode.ProcReport{ID: f.id, Role: "server", Version: config.MkGoVersion, Commit: config.MkGoCommit, State: state})
		}
		select {
		case <-ctx.Done():
			f.mu.Lock()
			if f.conn != nil {
				_ = f.conn.Close(context.Background())
			}
			f.mu.Unlock()
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (f *fakeServer) notesSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func (r *restoreE2E) startServer(id string) *fakeServer {
	ctx, cancel := context.WithCancel(r.ctx)
	f := &fakeServer{r: r, id: id, cancel: cancel, done: make(chan struct{})}
	go f.run(ctx)
	r.t.Cleanup(func() {
		cancel()
		<-f.done
	})
	require.Eventually(r.t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.conn != nil
	}, 30*time.Second, 50*time.Millisecond)
	return f
}

// TestRestoreFromAdminEndToEnd checks #3463 (6c) with real servers:
// the re-authentication the backup service runs (completion condition 4),
// a restore that switches and resumes on the restored database with Redis
// cleaned and doctor's checks passing (2), and a restore that cannot get
// the database let go and resumes on the current one (3).
func TestRestoreFromAdminEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI is not available")
	}
	r := setupRestoreE2E(t)
	server := r.startServer("fake-1")
	// keptBefore is the database the restore kept, which the rollback swaps back in.
	var keptBefore string

	t.Run("a job holds the slot", func(t *testing.T) {
		// 取る・確かめるが走っている間は、再認証 (TOTP の消費) の前に断る。
		release, err := r.daemon.Load().Acquire(daemon.JobVerify)
		require.NoError(t, err)
		defer release()
		code, out := r.call("/restore/prepare", map[string]any{"i": adminToken, "id": r.genID, "password": adminPassword, "token": r.codes[0]}, "")
		assert.Equal(t, http.StatusConflict, code)
		assert.Equal(t, "BACKUP_SERVICE_BUSY", e2eCode(out))
	})

	t.Run("re-authentication", func(t *testing.T) {
		body := func(token string, extra map[string]any) map[string]any {
			m := map[string]any{"i": token, "id": r.genID, "password": adminPassword}
			for k, v := range extra {
				m[k] = v
			}
			return m
		}
		for _, c := range []struct {
			name string
			body map[string]any
			want string
		}{
			{"moderator", body(modToken, map[string]any{"token": "000000"}), "ROLE_PERMISSION_DENIED"},
			{"app or unknown token", body("tokapp0000000001", map[string]any{"token": "000000"}), "ACCESS_DENIED"},
			{"administrator without 2FA", body(no2faToken, map[string]any{"token": "000000"}), "TWO_FACTOR_REQUIRED"},
			{"code missing", body(adminToken, nil), "REAUTHENTICATION_REQUIRED"},
			{"wrong password", map[string]any{"i": adminToken, "id": r.genID, "password": "wrong", "token": "BACKUPWRONG00001"}, "REAUTHENTICATION_FAILED"},
		} {
			code, out := r.call("/restore/prepare", c.body, "")
			assert.NotEqual(t, http.StatusOK, code, c.name)
			assert.Equal(t, c.want, e2eCode(out), c.name)
		}
		// passwordguard: 同じ利用者・接続元の範囲で 10 回失敗したら止める。
		var last int
		var out map[string]any
		for i := 0; i < 11; i++ {
			last, out = r.call("/restore/prepare", body(guardToken, map[string]any{"password": "wrong", "token": "000000"}), "198.51.100.7")
		}
		assert.Equal(t, http.StatusTooManyRequests, last, out)
		// TOTP のコードは 1 回しか通らない。
		code, err := totp.GenerateCode(r.secret, time.Now())
		require.NoError(t, err)
		st, out := r.call("/restore/prepare", body(adminToken, map[string]any{"token": code}), "")
		require.Equal(t, http.StatusOK, st, out)
		opID, _ := out["opId"].(string)
		ticket, _ := out["ticket"].(string)
		r.waitStep(opID, ticket, restoreop.StepReady)
		st, out = r.call("/restore/abort", map[string]any{"i": adminToken, "opId": opID, "ticket": ticket, "password": adminPassword, "token": code}, "")
		assert.Equal(t, http.StatusForbidden, st)
		assert.Equal(t, "TWO_FACTOR_CODE_ALREADY_USED", e2eCode(out))
		// ticket だけでは確定できない。別の利用者は ticket を持っていても読めない。
		st, out = r.call("/restore/confirm", map[string]any{"i": adminToken, "opId": opID, "ticket": ticket}, "")
		assert.Equal(t, http.StatusForbidden, st)
		assert.Equal(t, "REAUTHENTICATION_REQUIRED", e2eCode(out))
		st, out = r.call("/restore/status", map[string]any{"i": modToken, "opId": opID, "ticket": ticket}, "")
		assert.Equal(t, http.StatusForbidden, st)
		assert.Equal(t, "RESTORE_TICKET_INVALID", e2eCode(out))
		st, out = r.call("/restore/abort", map[string]any{"i": adminToken, "opId": opID, "ticket": ticket, "password": adminPassword, "token": r.backupCode()}, "")
		require.Equal(t, http.StatusNoContent, st, out)
		r.waitStep(opID, ticket, restoreop.StepAborted)
		assert.Equal(t, []string{"n_after", "n_before"}, r.notes("elythia"), "aborting does not touch the current database")
	})

	t.Run("not let go: resumes on the current database", func(t *testing.T) {
		opID, ticket := r.prepare()
		// Elythia ではない接続 (運営者の psql など) は切らない。残っている間は held にならない。
		other, err := pgx.Connect(r.ctx, r.url("app", "app-pass", "elythia")+"&application_name=psql")
		require.NoError(t, err)
		st, out := r.call("/restore/confirm", map[string]any{"i": adminToken, "opId": opID, "ticket": ticket, "password": adminPassword, "token": r.backupCode()}, "")
		require.Equal(t, http.StatusNoContent, st, out)
		rec := r.waitStep(opID, ticket, restoreop.StepAborted)
		assert.Contains(t, rec["error"], "did not let go")
		require.NoError(t, other.Close(r.ctx))
		assert.Equal(t, []string{"n_after", "n_before"}, r.notes("elythia"))
		require.Eventually(t, func() bool {
			s, err := r.maint.Get(r.ctx)
			return err == nil && s == nil
		}, time.Minute, 100*time.Millisecond, "the maintenance ends")
		require.Eventually(t, func() bool {
			return strings.Join(server.notesSeen(), ",") == "n_after,n_before"
		}, 30*time.Second, 100*time.Millisecond, "the process serves the current database again")
		assert.Equal(t, int64(0), dbCountLike(t, r, "elythia_restore_%"), "the new database is dropped")
	})

	t.Run("restore", func(t *testing.T) {
		opID, ticket := r.prepare()
		st, out := r.call("/restore/confirm", map[string]any{"i": adminToken, "opId": opID, "ticket": ticket, "password": adminPassword, "token": r.backupCode()}, "")
		require.Equal(t, http.StatusNoContent, st, out)
		rec := r.waitStep(opID, ticket, restoreop.StepDone, restoreop.StepAborted, restoreop.StepReverted, restoreop.StepUnknown)
		require.Equal(t, restoreop.StepDone, rec["step"], rec)
		assert.Equal(t, []string{"n_before"}, r.notes("elythia"), "the post made after the backup is gone")
		before, _ := rec["before"].(string)
		assert.Equal(t, []string{"n_after", "n_before"}, r.notes(before), "the previous database is kept")
		n, err := r.rdb.Exists(r.ctx, r.prefix+"list:localTimeline").Result()
		require.NoError(t, err)
		assert.Equal(t, int64(0), n, "the timeline that pointed at the newer post is cleaned")
		require.Eventually(t, func() bool {
			return strings.Join(server.notesSeen(), ",") == "n_before"
		}, 30*time.Second, 100*time.Millisecond, "the process serves the restored database")
		// モデレーションログ: 準備は前の DB に、名前の入れ替えは戻した DB に残る。
		assert.Equal(t, 1, modlogCount(t, r, "elythia", "switch"))
		assert.GreaterOrEqual(t, modlogCount(t, r, before, "prepare"), 1)
		keptBefore = before
	})

	// 画面からのロールバック (#3463 (6d))。照合は今の DB (戻した DB) で行う。
	t.Run("rollback refused without the credentials in the current database", func(t *testing.T) {
		require.NotEmpty(t, keptBefore)
		code, err := totp.GenerateCode(r.secret, time.Now())
		require.NoError(t, err)
		st, out := r.call("/restore/prepare", map[string]any{"i": late2faToken, "kind": "rollback", "database": keptBefore, "password": adminPassword, "token": code}, "")
		assert.Equal(t, http.StatusForbidden, st, out)
		assert.Equal(t, "TWO_FACTOR_REQUIRED", e2eCode(out), "u5 registered 2FA after the backup, so the restored database has none")
		cur, err := restoreop.NewStore(r.rdb, r.prefix).Current(r.ctx)
		require.NoError(t, err)
		assert.Nil(t, cur, "nothing is prepared")
	})

	t.Run("rollback", func(t *testing.T) {
		require.NotEmpty(t, keptBefore)
		st, out := r.call("/restore/candidates", map[string]any{}, "")
		require.Equal(t, http.StatusOK, st, out)
		assert.Equal(t, "elythia", out["database"])
		dbs, _ := out["databases"].([]any)
		require.Len(t, dbs, 1, out)
		first, _ := dbs[0].(map[string]any)
		assert.Equal(t, keptBefore, first["name"])
		assert.Equal(t, bkp.KeptBeforeRestore, first["kind"])
		assert.Nil(t, first["problem"])
		assert.Equal(t, false, first["needsMigration"])

		st, out = r.call("/restore/prepare", map[string]any{"i": adminToken, "kind": "rollback", "database": keptBefore, "password": adminPassword, "token": r.backupCode()}, "")
		require.Equal(t, http.StatusOK, st, out)
		opID, _ := out["opId"].(string)
		ticket, _ := out["ticket"].(string)
		r.waitStep(opID, ticket, restoreop.StepReady)
		// 戻した DB で動いていた間のタイムライン。ロールバックの後は消える。
		require.NoError(t, r.rdb.RPush(r.ctx, r.prefix+"list:localTimeline", "n_before").Err())
		st, out = r.call("/restore/confirm", map[string]any{"i": adminToken, "opId": opID, "ticket": ticket, "password": adminPassword, "token": r.backupCode()}, "")
		require.Equal(t, http.StatusNoContent, st, out)
		rec := r.waitStep(opID, ticket, restoreop.StepDone, restoreop.StepAborted, restoreop.StepReverted, restoreop.StepUnknown)
		require.Equal(t, restoreop.StepDone, rec["step"], rec)
		assert.Equal(t, restoreop.KindRollback, rec["kind"])

		assert.Equal(t, []string{"n_after", "n_before"}, r.notes("elythia"), "the database before the restore is back")
		keptAs, _ := rec["before"].(string)
		assert.Equal(t, []string{"n_before"}, r.notes(keptAs), "the restored database is kept")
		assert.Equal(t, []string{keptAs}, dbNamesWithPrefix(t, r, "elythia_rolled_back_"))
		assert.Empty(t, dbNamesWithPrefix(t, r, "elythia_before_restore_"), "the kept database now has the server's name")
		n, err := r.rdb.Exists(r.ctx, r.prefix+"list:localTimeline").Result()
		require.NoError(t, err)
		assert.Equal(t, int64(0), n, "redis is cleaned")
		require.Eventually(t, func() bool {
			return strings.Join(server.notesSeen(), ",") == "n_after,n_before"
		}, 30*time.Second, 100*time.Millisecond, "the process serves the database it rolled back to")
		// モデレーションログ: 準備は戻した DB (今は脇へ置いた方) に、入れ替えは戻った DB に残る。
		assert.Equal(t, 1, modlogCountOf(t, r, "elythia", "rollbackBackup", "switch"))
		assert.Equal(t, 1, modlogCountOf(t, r, keptAs, "rollbackBackup", "prepare"))
		info := modlogInfo(t, r, "elythia", "rollbackBackup", "switch")
		assert.Equal(t, keptBefore, info["from"])
		assert.Equal(t, keptAs, info["keptAs"])
		assert.Equal(t, "elythia", info["database"])
	})

	// 戻す先の版 (#3463 (6d))。新しい・dirty は断り、古ければ入れ替える前に
	// migrate を当てる。中止しても戻す先は消さない。
	t.Run("rollback by the version of the kept database", func(t *testing.T) {
		_, count, err := latestCoreMigration()
		require.NoError(t, err)
		kept := func(name string, steps int, sqls ...string) {
			t.Helper()
			admin, err := pgx.Connect(r.ctx, r.url("test", "test", "postgres"))
			require.NoError(t, err)
			_, err = admin.Exec(r.ctx, "CREATE DATABASE "+name+" OWNER app")
			require.NoError(t, err)
			require.NoError(t, admin.Close(r.ctx))
			m, err := gomigrate.New("file://"+rsCoreMigrations, strings.Replace(r.url("app", "app-pass", name), "postgres://", "pgx5://", 1))
			require.NoError(t, err)
			require.NoError(t, m.Steps(steps))
			_, _ = m.Close()
			if len(sqls) > 0 {
				r.exec(name, sqls...)
			}
		}
		const (
			newer = "elythia_before_restore_20260101000001"
			dirty = "elythia_before_restore_20260101000002"
			older = "elythia_before_restore_20260101000003"
		)
		kept(newer, count, `UPDATE schema_migrations SET version = version + 1`)
		kept(dirty, count, `UPDATE schema_migrations SET dirty = true`)
		kept(older, count-1)

		st, out := r.call("/restore/candidates", map[string]any{}, "")
		require.Equal(t, http.StatusOK, st, out)
		problems := map[string]any{}
		dbs, _ := out["databases"].([]any)
		for _, d := range dbs {
			m, _ := d.(map[string]any)
			problems[m["name"].(string)] = m["problem"]
		}
		assert.Equal(t, "migrationTooNew", problems[newer])
		assert.Equal(t, "migrationDirty", problems[dirty])
		assert.Nil(t, problems[older])

		for name, want := range map[string]string{newer: "KEPT_DATABASE_TOO_NEW", dirty: "KEPT_DATABASE_DIRTY"} {
			st, out := r.call("/restore/prepare", map[string]any{"i": adminToken, "kind": "rollback", "database": name, "password": adminPassword, "token": r.backupCode()}, "")
			assert.Equal(t, http.StatusBadRequest, st, out)
			assert.Equal(t, want, e2eCode(out), name)
		}

		st, out = r.call("/restore/prepare", map[string]any{"i": adminToken, "kind": "rollback", "database": older, "password": adminPassword, "token": r.backupCode()}, "")
		require.Equal(t, http.StatusOK, st, out)
		opID, _ := out["opId"].(string)
		ticket, _ := out["ticket"].(string)
		r.waitStep(opID, ticket, restoreop.StepReady)
		assert.Equal(t, migrationVersion(t, r, "elythia"), migrationVersion(t, r, older), "migrated to the bundled version before the switch")
		st, out = r.call("/restore/abort", map[string]any{"i": adminToken, "opId": opID, "ticket": ticket, "password": adminPassword, "token": r.backupCode()}, "")
		require.Equal(t, http.StatusNoContent, st, out)
		r.waitStep(opID, ticket, restoreop.StepAborted)
		assert.Equal(t, []string{"elythia_before_restore_20260101000001", "elythia_before_restore_20260101000002", older},
			dbNamesWithPrefix(t, r, "elythia_before_restore_"), "aborting a rollback keeps the kept database")
		assert.Equal(t, []string{"n_after", "n_before"}, r.notes("elythia"))
	})

	r.cancel()
	select {
	case code := <-r.stopped:
		assert.Equal(t, 0, code)
	case <-time.After(time.Minute):
		t.Fatal("the backup service did not stop")
	}
}

func dbCountLike(t *testing.T, r *restoreE2E, pattern string) int64 {
	t.Helper()
	conn, err := pgx.Connect(r.ctx, r.url("app", "app-pass", "postgres"))
	require.NoError(t, err)
	defer conn.Close(r.ctx)
	var n int64
	require.NoError(t, conn.QueryRow(r.ctx, `SELECT count(*) FROM pg_database WHERE datname LIKE $1`, pattern).Scan(&n))
	return n
}

func modlogCount(t *testing.T, r *restoreE2E, db, phase string) int {
	t.Helper()
	return modlogCountOf(t, r, db, "restoreBackup", phase)
}

func modlogCountOf(t *testing.T, r *restoreE2E, db, typ, phase string) int {
	t.Helper()
	conn, err := pgx.Connect(r.ctx, r.url("app", "app-pass", db))
	require.NoError(t, err)
	defer conn.Close(r.ctx)
	var n int
	require.NoError(t, conn.QueryRow(r.ctx, `SELECT count(*) FROM moderation_log WHERE type = $1 AND info->>'phase' = $2`, typ, phase).Scan(&n))
	return n
}

func modlogInfo(t *testing.T, r *restoreE2E, db, typ, phase string) map[string]any {
	t.Helper()
	conn, err := pgx.Connect(r.ctx, r.url("app", "app-pass", db))
	require.NoError(t, err)
	defer conn.Close(r.ctx)
	var info map[string]any
	require.NoError(t, conn.QueryRow(r.ctx, `SELECT info FROM moderation_log WHERE type = $1 AND info->>'phase' = $2`, typ, phase).Scan(&info))
	return info
}

// dbNamesWithPrefix lists the databases whose name starts with prefix
// (compared as text: LIKE would take _ for any character).
func dbNamesWithPrefix(t *testing.T, r *restoreE2E, prefix string) []string {
	t.Helper()
	conn, err := pgx.Connect(r.ctx, r.url("app", "app-pass", "postgres"))
	require.NoError(t, err)
	defer conn.Close(r.ctx)
	rows, err := conn.Query(r.ctx, `SELECT datname FROM pg_database WHERE left(datname, length($1)) = $1 ORDER BY datname`, prefix)
	require.NoError(t, err)
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return names
}

func migrationVersion(t *testing.T, r *restoreE2E, db string) int64 {
	t.Helper()
	conn, err := pgx.Connect(r.ctx, r.url("app", "app-pass", db))
	require.NoError(t, err)
	defer conn.Close(r.ctx)
	var v int64
	require.NoError(t, conn.QueryRow(r.ctx, `SELECT version FROM schema_migrations`).Scan(&v))
	return v
}

// latestCoreMigration returns the newest bundled core migration and the
// number of them.
func latestCoreMigration() (uint, int, error) {
	entries, err := os.ReadDir(rsCoreMigrations)
	if err != nil {
		return 0, 0, err
	}
	var latest uint
	n := 0
	for _, e := range entries {
		var v uint
		if _, err := fmt.Sscanf(e.Name(), "%d_", &v); err != nil || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		latest = max(latest, v)
		n++
	}
	return latest, n, nil
}

func setupRestoreE2E(t *testing.T) *restoreE2E {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
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
	mc, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        e2eMinioImage,
			ExposedPorts: []string{"9000/tcp"},
			Env:          map[string]string{"MINIO_ROOT_USER": "minioadmin", "MINIO_ROOT_PASSWORD": "minioadmin-secret"},
			Cmd:          []string{"server", "/data"},
			WaitingFor:   wait.ForHTTP("/minio/health/ready").WithPort("9000/tcp").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mc.Terminate(context.Background()) })
	minioEndpoint, err := mc.PortEndpoint(ctx, "9000/tcp", "http")
	require.NoError(t, err)
	s3c := s3.New(s3.Options{
		BaseEndpoint: aws.String(minioEndpoint), Region: "us-east-1", UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider("minioadmin", "minioadmin-secret", ""),
	})
	_, err = s3c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("backups")})
	require.NoError(t, err)

	pgPort, err := pg.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)
	redisPort, err := rc.MappedPort(ctx, "6379/tcp")
	require.NoError(t, err)
	r := &restoreE2E{t: t, ctx: ctx, cancel: cancel, prefix: "elythia.example:"}
	r.url = func(user, pass, db string) string {
		return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable", user, pass, pgPort.Num(), db)
	}
	r.rdb = redis.NewClient(&redis.Options{Addr: fmt.Sprintf("127.0.0.1:%d", redisPort.Num())})
	t.Cleanup(func() { _ = r.rdb.Close() })
	r.maint = maintmode.NewStore(r.rdb, r.rdb, r.prefix)

	// アプリのユーザーは DB の持ち主で、別名の DB を作れる (本番の compose の既定は
	// superuser だが、要るのはこの 2 つだけ)。
	admin, err := pgx.Connect(ctx, r.url("test", "test", "postgres"))
	require.NoError(t, err)
	for _, s := range []string{"CREATE ROLE app LOGIN PASSWORD 'app-pass' CREATEDB", "CREATE DATABASE elythia OWNER app"} {
		_, err := admin.Exec(ctx, s)
		require.NoError(t, err, s)
	}
	require.NoError(t, admin.Close(ctx))
	m, err := gomigrate.New("file://"+rsCoreMigrations, strings.Replace(r.url("app", "app-pass", "elythia"), "postgres://", "pgx5://", 1))
	require.NoError(t, err)
	require.NoError(t, m.Up())
	_, _ = m.Close()
	seedAdminUsers(t, r)
	r.exec("elythia", `INSERT INTO note ("id", "userId", "visibility") VALUES ('n_before', 'u1', 'public')`)

	cfgPath := filepath.Join(t.TempDir(), "default.yml")
	wrapper := filepath.Join(t.TempDir(), "pg_restore")
	require.NoError(t, os.WriteFile(wrapper, []byte("#!/bin/sh\nexec docker run --rm -i --network host -e PGPASSWORD postgres:18-alpine pg_restore \"$@\"\n"), 0o700))
	require.NoError(t, os.WriteFile(cfgPath, []byte(fmt.Sprintf(`url: https://elythia.example/
port: 3000
db:
  host: 127.0.0.1
  port: %d
  db: elythia
  user: app
  pass: app-pass
redis:
  host: 127.0.0.1
  port: %d
maintenance:
  drainTimeout: 3s
backup:
  storage:
    type: s3
    s3:
      endpoint: %s
      region: us-east-1
      bucket: backups
      prefix: e2e
      accessKey: minioadmin
      secretKey: minioadmin-secret
      forcePathStyle: true
  tools:
    pgRestore: %s
  schedule:
    listen: ":0"
  server:
    serviceToken: %s
`, pgPort.Num(), redisPort.Num(), minioEndpoint, wrapper, serviceToken)), 0o600))

	// バックアップを取り、確認に通った印 (verify.json) を置く。
	te, takeOut, takeErr := testEnv(nil)
	te.runner = dockerExecRunner{container: pg.GetContainerID()}
	te.dumpConn = func(*config.Config) (bkp.DumpConn, error) {
		return bkp.DumpConn{URI: "postgresql://app@localhost:5432/elythia?sslmode=disable", Password: "app-pass"}, nil
	}
	require.Equal(t, 0, take(ctx, te, []string{"-config", cfgPath}), takeErr.String())
	id := regexp.MustCompile(`generation (\d{8}T\d{6}Z)`).FindStringSubmatch(takeOut.String())
	require.Len(t, id, 2, takeOut.String())
	r.genID = id[1]
	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)
	st, err := bkp.OpenStorage(cfg.Backup.Storage)
	require.NoError(t, err)
	meta, err := bkp.ReadMeta(ctx, st, r.genID)
	require.NoError(t, err)
	vr, err := json.Marshal(bkp.VerifyResult{ID: r.genID, OK: true, VerifiedAt: time.Now(), DumpSHA256: meta.DumpSHA256})
	require.NoError(t, err)
	require.NoError(t, st.Put(ctx, bkp.Key(r.genID, bkp.VerifyFile), bytes.NewReader(vr)))

	// バックアップより後の投稿と、それを指すタイムライン。u5 はバックアップより後に
	// 2FA を登録した (戻した DB では 2FA が無い)。
	r.exec("elythia", `INSERT INTO note ("id", "userId", "visibility") VALUES ('n_after', 'u1', 'public')`,
		fmt.Sprintf(`UPDATE user_profile SET "twoFactorEnabled" = true, "twoFactorSecret" = '%s' WHERE "userId" = 'u5'`, r.secret))
	require.NoError(t, r.rdb.RPush(ctx, r.prefix+"list:localTimeline", "n_after").Err())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	r.base = "http://" + ln.Addr().String()
	var stderr syncBuffer
	runCtx, runCancel := context.WithCancel(ctx)
	r.cancel = runCancel
	e := daemonEnv{
		stderr:     &stderr,
		loadConfig: config.Load,
		openDeps: func(*config.Config, *slog.Logger) (Deps, error) {
			return Deps{Storage: st, Taker: nopTaker{}}, nil
		},
		signalContext: func() (context.Context, context.CancelFunc) { return runCtx, runCancel },
		listen:        func(string) (net.Listener, error) { return ln, nil },
		location:      time.UTC,
		openRestore: func(cfg *config.Config, d *daemon.Daemon, s bkp.Storage, l *slog.Logger) (*RestoreParts, error) {
			r.daemon.Store(d)
			return restoreWiring{coreDir: rsCoreMigrations, localDir: filepath.Join(rsCoreMigrations, "local"), heldGrace: 5 * time.Second}.open(cfg, d, s, l)
		},
	}
	r.stopped = make(chan int, 1)
	go func() { r.stopped <- runDaemon(e, []string{"-config", cfgPath}) }()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("backup service log:\n%s", stderr.String())
		}
	})
	require.Eventually(t, func() bool {
		res, err := http.Get(r.base + "/status")
		if err != nil {
			return false
		}
		_ = res.Body.Close()
		return res.StatusCode == http.StatusUnauthorized
	}, 30*time.Second, 50*time.Millisecond, "the control API listens")
	return r
}

// seedAdminUsers creates the root administrator u1 (TOTP and backup codes),
// a moderator, an administrator without 2FA and an administrator for the
// failure budget.
func seedAdminUsers(t *testing.T, r *restoreE2E) {
	t.Helper()
	db, err := gorm.Open(postgres.Open(r.url("app", "app-pass", "elythia")), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	defer sqlDB.Close()
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "e2e", AccountName: "root"})
	require.NoError(t, err)
	r.secret = key.Secret()
	hash, err := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.MinCost)
	require.NoError(t, err)
	h := string(hash)
	r.codes = nil
	for i := 1; i <= 12; i++ {
		r.codes = append(r.codes, fmt.Sprintf("BACKUPCODE%06d", i))
	}
	for _, u := range []struct {
		id, token string
		twoFA     bool
	}{{"u1", adminToken, true}, {"u2", modToken, true}, {"u3", no2faToken, false}, {"u4", guardToken, true}, {"u5", late2faToken, false}} {
		tok := u.token
		require.NoError(t, db.Create(&model.User{ID: u.id, Username: u.id, UsernameLower: u.id, Token: &tok, AvatarDecorations: datatypes.JSON([]byte("[]"))}).Error)
		p := &model.UserProfile{UserID: u.id, Password: &h, TwoFactorEnabled: u.twoFA}
		if u.twoFA {
			p.TwoFactorSecret = &r.secret
		}
		if u.id == "u1" {
			p.TwoFactorBackupSecret = model.StringArray(r.codes)
		}
		require.NoError(t, db.Create(p).Error)
	}
	require.NoError(t, db.Create(&model.Role{ID: "r_admin", Name: "admin", IsAdministrator: true}).Error)
	require.NoError(t, db.Create(&model.Role{ID: "r_mod", Name: "mod", IsModerator: true}).Error)
	for _, a := range [][3]string{{"a1", "u2", "r_mod"}, {"a2", "u3", "r_admin"}, {"a3", "u4", "r_admin"}, {"a4", "u5", "r_admin"}} {
		require.NoError(t, db.Create(&model.RoleAssignment{ID: a[0], UserID: a[1], RoleID: a[2]}).Error)
	}
	require.NoError(t, db.Exec(`INSERT INTO meta ("id", "rootUserId") SELECT 'x', 'u1' WHERE NOT EXISTS (SELECT 1 FROM meta)`).Error)
	require.NoError(t, db.Exec(`UPDATE meta SET "rootUserId" = 'u1'`).Error)
}
