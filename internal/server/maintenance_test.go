package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/maintmode"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// maintenanceRig is a Supervisor following the maintenance state of a real
// Redis, with generations on a real PostgreSQL schema.
type maintenanceRig struct {
	env   *lifecycleEnv
	store *maintmode.Store
	// jobs receives the generation number of the server that processed a
	// reaction flush job.
	jobs chan int32
	gens atomic.Int32
}

func newMaintenanceRig(t *testing.T) *maintenanceRig {
	t.Helper()
	// queue の worker も止めて作り直すことを見るので、両方の役割で動かす。
	t.Setenv(config.EnvOnlyServer, "")
	t.Setenv(config.EnvOnlyQueue, "")
	env := newLifecycleEnv(t)
	return &maintenanceRig{
		env:   env,
		store: maintmode.NewStore(env.rdb, env.rdb, "mt:"),
		jobs:  make(chan int32, 16),
	}
}

func (r *maintenanceRig) deps(id string) SupervisorDeps {
	d := r.env.deps()
	inner := d.NewServer
	d.NewServer = func(cfg *config.Config, db *gorm.DB, rc *cache.RedisClients) (*Server, error) {
		srv, err := inner(cfg, db, rc)
		if err != nil {
			return nil, err
		}
		gen := r.gens.Add(1)
		// どの世代の worker がジョブを処理したかを見るため、reaction flush の
		// handler を差し替える (Start の前なので差し替えられる)。
		srv.queueServer.Handle(queue.TaskTypeReactionFlush, func(context.Context, driver.Task) error {
			r.jobs <- gen
			return nil
		})
		return srv, nil
	}
	d.Maintenance = r.store
	d.ProcessID = id
	d.MaintenancePoll = 50 * time.Millisecond
	return d
}

// start builds and starts a Supervisor and waits until it listens.
func (r *maintenanceRig) start(t *testing.T, id string, deps SupervisorDeps) *Supervisor {
	t.Helper()
	cfg := *r.env.cfg
	cfg.Port = 0
	sup, err := NewSupervisor(&cfg, r.env.redis, deps)
	require.NoError(t, err)
	startErr := make(chan error, 1)
	go func() { startErr <- sup.Start() }()
	t.Cleanup(func() {
		_ = sup.Shutdown(context.Background())
		<-startErr
	})
	select {
	case <-sup.listening:
	case err := <-startErr:
		t.Fatalf("Start returned before listening: %v", err)
	}
	return sup
}

func waitProc(t *testing.T, sup *Supervisor, want string) {
	t.Helper()
	require.Eventually(t, func() bool { return sup.maint.procState() == want },
		15*time.Second, 20*time.Millisecond, "process did not reach %q (now %q)", want, sup.maint.procState())
}

func enterMaintenance(t *testing.T, store *maintmode.Store) {
	t.Helper()
	require.NoError(t, store.Enter(context.Background(), maintmode.State{
		OpID: "op-1", Kind: maintmode.KindManual, Reason: "testing",
	}))
}

func metaName(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := serveRequest(t, h, http.MethodPost, "/api/meta")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Name *string `json:"name"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	if body.Name == nil {
		return ""
	}
	return *body.Name
}

// TestMaintenance_EnterReleasesDBAndExitRestoresEverything covers the
// completion criteria of #3463 (6b): entering drops every DB connection of
// this process (seen in pg_stat_activity by application_name), and after
// exiting the API, the queue worker and streaming work again.
func TestMaintenance_EnterReleasesDBAndExitRestoresEverything(t *testing.T) {
	r := newMaintenanceRig(t)
	sup := r.start(t, "proc-a", r.deps("proc-a"))
	ts := httptest.NewServer(sup)
	t.Cleanup(ts.Close)

	require.Equal(t, http.StatusOK, serveStatus(t, sup, http.MethodPost, "/api/meta"))
	require.Positive(t, r.env.backends(t), "前提: 世代が DB に繋がっている")
	ws := dialStreaming(t, ts, r.env.cfg.URL)
	waitProc(t, sup, maintmode.ProcRunning)

	enterMaintenance(t, r.store)
	waitProc(t, sup, maintmode.ProcDrained)
	r.env.requireNoBackends(t)
	assert.Equal(t, 1012, closeCodeOf(t, ws))

	// メンテナンス中の応答。
	rec := serveRequest(t, sup, http.MethodPost, "/api/meta")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"MAINTENANCE"`)
	assert.Equal(t, "1", rec.Header().Get(maintmode.HeaderMarker))
	rec = serveRequest(t, sup, http.MethodGet, "/healthz")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"maintenance":true`)

	// 報告: drained で、残った接続は 0。
	reports, err := r.store.Reports(context.Background())
	require.NoError(t, err)
	require.Len(t, reports, 1)
	assert.Equal(t, "proc-a", reports[0].ID)
	assert.Equal(t, maintmode.ProcDrained, reports[0].State)
	assert.Zero(t, reports[0].OpenConns)
	assert.Equal(t, config.MkGoVersion, reports[0].Version)

	// メンテナンス中にジョブを積んでも、誰も処理しない (worker は止まっている)。
	qc := queue.NewClient(mustDriver(t, r.env.cfg))
	require.NoError(t, qc.EnqueueReactionFlush())
	select {
	case gen := <-r.jobs:
		t.Fatalf("job was processed during the maintenance by generation %d", gen)
	case <-time.After(300 * time.Millisecond):
	}

	require.NoError(t, r.store.Clear(context.Background(), "op-1"))
	waitProc(t, sup, maintmode.ProcRunning)

	// API・queue・streaming が元どおり動く。
	assert.Equal(t, http.StatusOK, serveStatus(t, sup, http.MethodPost, "/api/meta"))
	select {
	case gen := <-r.jobs:
		assert.EqualValues(t, 2, gen, "作り直した世代の worker が処理していない")
	case <-time.After(15 * time.Second):
		t.Fatal("メンテナンスの後、積んであったジョブが処理されない")
	}
	ws2 := dialStreaming(t, ts, r.env.cfg.URL)
	sup.genMu.Lock()
	newSrv := sup.gen.srv
	sup.genMu.Unlock()
	require.Eventually(t, func() bool { return newSrv.streamManager.Count() == 1 },
		5*time.Second, 10*time.Millisecond, "streaming に繋がらない")
	_ = ws2
	reports, err = r.store.Reports(context.Background())
	require.NoError(t, err)
	require.Len(t, reports, 1)
	assert.Equal(t, maintmode.ProcRunning, reports[0].State)
}

// mustDriver opens a queue driver of its own, standing in for another
// process that enqueues while this one is in maintenance.
func mustDriver(t *testing.T, cfg *config.Config) driver.Driver {
	t.Helper()
	d, err := buildQueueDriver(context.Background(), cfg, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// TestMaintenance_ExitPicksUpRowsChangedDuringMaintenance checks that what
// the database says after the maintenance is what the server serves: the
// generation is rebuilt, so no cache of the old one survives.
//
// 作り直さずに同じ Server を再開する形に変えると、meta の 5 分のキャッシュが
// 古い名前を返して落ちる。
func TestMaintenance_ExitPicksUpRowsChangedDuringMaintenance(t *testing.T) {
	r := newMaintenanceRig(t)
	sup := r.start(t, "proc-a", r.deps("proc-a"))
	_ = metaName(t, sup) // meta の行とキャッシュを作る

	enterMaintenance(t, r.store)
	waitProc(t, sup, maintmode.ProcDrained)
	newName := fmt.Sprintf("renamed-during-maintenance-%d", time.Now().UnixNano())
	require.NoError(t, r.env.observer.Exec(`UPDATE "meta" SET "name" = ?`, newName).Error)

	require.NoError(t, r.store.Clear(context.Background(), ""))
	waitProc(t, sup, maintmode.ProcRunning)
	assert.Equal(t, newName, metaName(t, sup))
}

// TestMaintenance_TwoProcessesFollowTheSameState runs two Supervisors on the
// same Redis (two Web nodes) and checks that both leave and both come back.
func TestMaintenance_TwoProcessesFollowTheSameState(t *testing.T) {
	r := newMaintenanceRig(t)
	a := r.start(t, "proc-a", r.deps("proc-a"))
	b := r.start(t, "proc-b", r.deps("proc-b"))

	enterMaintenance(t, r.store)
	waitProc(t, a, maintmode.ProcDrained)
	waitProc(t, b, maintmode.ProcDrained)
	r.env.requireNoBackends(t)
	reports, err := r.store.Reports(context.Background())
	require.NoError(t, err)
	require.Len(t, reports, 2)
	for _, rep := range reports {
		assert.Equal(t, maintmode.ProcDrained, rep.State, rep.ID)
	}

	require.NoError(t, r.store.Clear(context.Background(), ""))
	waitProc(t, a, maintmode.ProcRunning)
	waitProc(t, b, maintmode.ProcRunning)
	assert.Equal(t, http.StatusOK, serveStatus(t, a, http.MethodPost, "/api/meta"))
	assert.Equal(t, http.StatusOK, serveStatus(t, b, http.MethodPost, "/api/meta"))
}

// TestMaintenance_StartingDuringMaintenanceDoesNotOpenTheDB checks §3.4:
// a process started while a maintenance is active serves the maintenance
// handler without opening the database, and opens it once the maintenance
// is over.
func TestMaintenance_StartingDuringMaintenanceDoesNotOpenTheDB(t *testing.T) {
	r := newMaintenanceRig(t)
	enterMaintenance(t, r.store)
	sup := r.start(t, "proc-a", r.deps("proc-a"))
	waitProc(t, sup, maintmode.ProcDrained)
	assert.Zero(t, r.env.opens.Load(), "メンテナンス中に起動したのに DB を開いた")
	assert.Zero(t, r.env.backends(t))
	assert.Equal(t, http.StatusServiceUnavailable, serveStatus(t, sup, http.MethodPost, "/api/meta"))

	require.NoError(t, r.store.Clear(context.Background(), ""))
	waitProc(t, sup, maintmode.ProcRunning)
	assert.EqualValues(t, 1, r.env.opens.Load())
	assert.Equal(t, http.StatusOK, serveStatus(t, sup, http.MethodPost, "/api/meta"))
}

// TestMaintenance_FailedResumeKeepsMaintenanceAndRetries checks that when
// the database cannot be reopened after the maintenance, the process keeps
// answering with the maintenance handler, reports resume_failed, and comes
// back by itself once the database is reachable again.
func TestMaintenance_FailedResumeKeepsMaintenanceAndRetries(t *testing.T) {
	r := newMaintenanceRig(t)
	deps := r.deps("proc-a")
	var failOpen atomic.Bool
	open := deps.OpenDB
	deps.OpenDB = func(cfg *config.Config) (*gorm.DB, error) {
		if failOpen.Load() {
			return nil, errors.New("database is unreachable")
		}
		return open(cfg)
	}
	sup := r.start(t, "proc-a", deps)

	enterMaintenance(t, r.store)
	waitProc(t, sup, maintmode.ProcDrained)
	failOpen.Store(true)
	require.NoError(t, r.store.Clear(context.Background(), ""))
	waitProc(t, sup, maintmode.ProcResumeFailed)
	assert.Equal(t, http.StatusServiceUnavailable, serveStatus(t, sup, http.MethodPost, "/api/meta"))
	require.Eventually(t, func() bool {
		reports, err := r.store.Reports(context.Background())
		return err == nil && len(reports) == 1 && reports[0].State == maintmode.ProcResumeFailed &&
			reports[0].Error != ""
	}, 5*time.Second, 20*time.Millisecond)

	failOpen.Store(false)
	waitProc(t, sup, maintmode.ProcRunning)
	assert.Equal(t, http.StatusOK, serveStatus(t, sup, http.MethodPost, "/api/meta"))
}

// TestMaintenance_ReenterWhileResumeFailed checks that a maintenance started
// again while the process could not come back leaves it drained.
func TestMaintenance_ReenterWhileResumeFailed(t *testing.T) {
	r := newMaintenanceRig(t)
	deps := r.deps("proc-a")
	var failOpen atomic.Bool
	open := deps.OpenDB
	deps.OpenDB = func(cfg *config.Config) (*gorm.DB, error) {
		if failOpen.Load() {
			return nil, errors.New("database is unreachable")
		}
		return open(cfg)
	}
	sup := r.start(t, "proc-a", deps)
	enterMaintenance(t, r.store)
	waitProc(t, sup, maintmode.ProcDrained)
	failOpen.Store(true)
	require.NoError(t, r.store.Clear(context.Background(), ""))
	waitProc(t, sup, maintmode.ProcResumeFailed)
	enterMaintenance(t, r.store)
	waitProc(t, sup, maintmode.ProcDrained)
}

// TestMaintenance_RedisOutageKeepsTheCurrentState checks §3.5: when the
// state cannot be read, a process in maintenance stays in maintenance.
func TestMaintenance_RedisOutageKeepsTheCurrentState(t *testing.T) {
	r := newMaintenanceRig(t)
	sup := r.start(t, "proc-a", r.deps("proc-a"))
	enterMaintenance(t, r.store)
	waitProc(t, sup, maintmode.ProcDrained)

	// 監視を止めてから、読めない Redis を指す store に差し替えて 1 周回す。
	sup.maint.stop(context.Background())
	sup.maint.store = maintmode.NewStore(unreachableRedis(t), nil, "mt:")
	sup.maint.reconcile(context.Background())
	assert.Equal(t, maintmode.ProcDrained, sup.maint.procState())
	assert.EqualValues(t, 1, r.env.opens.Load(), "Redis が読めないのに DB を開き直した")
	assert.Equal(t, http.StatusServiceUnavailable, serveStatus(t, sup, http.MethodPost, "/api/meta"))
}

func unreachableRedis(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestMaintenance_WarnsWhenRedisMayEvictTheState checks the startup warning
// for a Redis whose policy may evict the state key (#3463 review L2).
func TestMaintenance_WarnsWhenRedisMayEvictTheState(t *testing.T) {
	r := newMaintenanceRig(t)
	require.NoError(t, r.env.rdb.ConfigSet(context.Background(), "maxmemory-policy", "allkeys-lru").Err())
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	r.start(t, "proc-a", r.deps("proc-a"))
	assert.Contains(t, buf.String(), "maxmemory-policy=allkeys-lru")
}
