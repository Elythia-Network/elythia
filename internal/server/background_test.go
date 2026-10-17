package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/repository"
)

// signinWriteProbe holds the INSERT of a signin row until the test releases
// it, and reports the error the INSERT ended with.
//
// 失敗したサインインは、応答を返した後に goroutine で signin の行を書く
// (api/signin の fail)。その INSERT を GORM の callback で止めておけば、「世代を
// 止める時点で、まだ DB に書こうとしている goroutine」を確実に作れる。止めるのは
// transaction を始める前にする。始めた後だと接続を掴んでいるので、プールを閉じても
// その接続で書けてしまい、閉じた DB への書き込みが再現しない。
type signinWriteProbe struct {
	entered chan struct{}
	release chan struct{}
	result  chan error
}

func newSigninWriteProbe() *signinWriteProbe {
	return &signinWriteProbe{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
		result:  make(chan error, 1),
	}
}

func (p *signinWriteProbe) install(t *testing.T, db *gorm.DB) {
	t.Helper()
	isSignin := func(tx *gorm.DB) bool { return tx.Statement.Table == "signin" }
	require.NoError(t, db.Callback().Create().Before("gorm:begin_transaction").Register("test:hold_signin", func(tx *gorm.DB) {
		if !isSignin(tx) {
			return
		}
		select {
		case p.entered <- struct{}{}:
		default:
		}
		select {
		case <-p.release:
		case <-time.After(30 * time.Second):
		}
	}))
	require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:report_signin", func(tx *gorm.DB) {
		if !isSignin(tx) {
			return
		}
		select {
		case p.result <- tx.Error:
		default:
		}
	}))
}

// startSupervisorWithSigninProbe builds a Supervisor on the lifecycle
// environment whose generations hold every signin INSERT on probe, and a
// local user whose password is "right-pass".
func startSupervisorWithSigninProbe(t *testing.T, drain time.Duration) (*Supervisor, *signinWriteProbe) {
	t.Helper()
	env := newLifecycleEnv(t)
	cleanSigninFixture(t, env.observer)
	createLocalUser(t, env.observer, "bgw-user", "bgw_user", "bgwusertoken0001", "right-pass", false)

	probe := newSigninWriteProbe()
	deps := env.deps()
	deps.DrainTimeout = drain
	deps.OpenDB = func(cfg *config.Config) (*gorm.DB, error) {
		db, err := env.openDB(cfg)
		if err == nil {
			probe.install(t, db)
		}
		return db, err
	}
	sup, err := NewSupervisor(env.cfg, env.redis, deps)
	require.NoError(t, err)
	t.Cleanup(func() {
		select {
		case <-probe.release:
		default:
			close(probe.release)
		}
		_ = sup.Shutdown(context.Background())
	})
	return sup, probe
}

func cleanSigninFixture(t *testing.T, db *gorm.DB) {
	t.Helper()
	clean := func() {
		db.Exec(`DELETE FROM "signin" WHERE "userId" = ?`, "bgw-user")
		db.Exec(`DELETE FROM "user_profile" WHERE "userId" = ?`, "bgw-user")
		db.Exec(`DELETE FROM "user" WHERE "id" = ?`, "bgw-user")
	}
	clean()
	t.Cleanup(clean)
}

// failSignin makes a signin with a wrong password, which answers at once and
// records the failure in a goroutine.
func failSignin(t *testing.T, h http.Handler) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/signin-flow",
		strings.NewReader(`{"username":"bgw_user","password":"wrong-pass"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestSupervisor_StopGenerationWaitsForBackgroundWrites reproduces #3661: a
// goroutine started after a response (here the record of a failed signin)
// must finish its DB write before the generation's pool is closed.
//
// 直す前は、stopGeneration は goroutine を待たずに DB を閉じ、止めておいた
// INSERT は "sql: database is closed" で失敗した。
func TestSupervisor_StopGenerationWaitsForBackgroundWrites(t *testing.T) {
	sup, probe := startSupervisorWithSigninProbe(t, 5*time.Second)
	failSignin(t, sup)
	recvWithin(t, probe.entered, "失敗したサインインの記録が始まらない (前提)")

	stopped := make(chan StopReport, 1)
	go func() { stopped <- sup.stopGeneration(http.HandlerFunc(unavailable)) }()
	var rep StopReport
	stoppedEarly := false
	select {
	case rep = <-stopped:
		stoppedEarly = true
	case <-time.After(300 * time.Millisecond):
	}
	close(probe.release)
	if !stoppedEarly {
		rep = recvWithin(t, stopped, "goroutine が終わっても世代の停止が戻らない")
	}
	writeErr := recvWithin(t, probe.result, "記録の INSERT が終わらない")
	assert.False(t, stoppedEarly, "DB に書いている goroutine を待たずに世代を止めた")
	assert.NoError(t, writeErr, "世代を止めた後に、閉じた DB へ書きに行った")
	require.NoError(t, rep.Err)
	assert.Zero(t, rep.OpenConns)
}

// TestSupervisor_StopGenerationBoundsTheBackgroundWait checks that a
// goroutine that does not return cannot hold the generation stop (#3661).
func TestSupervisor_StopGenerationBoundsTheBackgroundWait(t *testing.T) {
	sup, probe := startSupervisorWithSigninProbe(t, 4*time.Second)
	failSignin(t, sup)
	recvWithin(t, probe.entered, "失敗したサインインの記録が始まらない (前提)")

	start := time.Now()
	stopped := make(chan StopReport, 1)
	go func() { stopped <- sup.stopGeneration(http.HandlerFunc(unavailable)) }()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("止まらない goroutine のせいで、世代の停止が固まった")
	}
	elapsed := time.Since(start)
	t.Logf("stopGeneration took %v", elapsed)
	// 待つのは背景処理の停止に渡す期限 (DrainTimeout の 4 秒) の残りの 1/4 (約 1 秒)
	// まで。期限を使い切ると、同じ期限を分け合う後の段 (保存や DB を閉じる待ち) の
	// 時間が無くなる。それより十分に短く戻っていれば、待たずに閉じたことになる。
	assert.GreaterOrEqual(t, elapsed, 700*time.Millisecond, "goroutine を待たずに世代を止めた")
	assert.Less(t, elapsed, 1900*time.Millisecond, "待ちが期限の 1/4 で打ち切られていない")
}

type countingUserUpdates struct {
	repository.UserRepository
	n atomic.Int32
}

func (c *countingUserUpdates) UpdateUser(string, map[string]any) error {
	c.n.Add(1)
	return nil
}

// TestLastActiveRecorder_RunsInTheGenerationGroup checks that the
// lastActiveDate update of a streaming connection starts in the group of the
// generation (#3661). 接続の close handler からも呼ばれるので、世代の停止の後に
// 呼ばれうる。
func TestLastActiveRecorder_RunsInTheGenerationGroup(t *testing.T) {
	repo := &countingUserUpdates{}
	g := bgwork.New()
	a := &lastActiveRecorderAdapter{userRepo: repo, background: g}
	a.RecordActive("u1")
	require.Zero(t, g.Stop(context.Background()))
	assert.EqualValues(t, 1, repo.n.Load(), "世代の停止が更新を待たなかった")

	a.RecordActive("u1")
	time.Sleep(50 * time.Millisecond)
	assert.EqualValues(t, 1, repo.n.Load(), "世代を止めた後に lastActiveDate を書きに行った")
	assert.EqualValues(t, 1, g.Dropped())
}
