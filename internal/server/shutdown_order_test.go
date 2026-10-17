package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/chart"
	corechartcharts "github.com/elythia-network/elythia/internal/core/chart/charts"
	coreinstance "github.com/elythia-network/elythia/internal/core/instance"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	queuemetrics "github.com/elythia-network/elythia/internal/queue/metrics"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

// lateJobDriver is a driver.Driver whose worker runs one in-flight job that
// finishes while the worker is being stopped. Shutdown が返る前にジョブの
// 書き込みを済ませるのは、mkq の Worker.Stop が handler の戻りを待つのと同じ。
// Other Driver methods are not used by the shutdown path and panic.
type lateJobDriver struct {
	driver.Driver
	srv *lateJobServer
}

func (d *lateJobDriver) Server() driver.Server { return d.srv }
func (d *lateJobDriver) Close() error          { return nil }

type lateJobServer struct {
	finish   func()
	once     sync.Once
	block    chan struct{} // nil: Shutdown returns once the job has finished
	shutdown chan struct{}
}

func (s *lateJobServer) Handle(string, driver.HandlerFunc) {}
func (s *lateJobServer) Start() error                      { return nil }
func (s *lateJobServer) Shutdown() {
	s.once.Do(func() { close(s.shutdown) })
	if s.block != nil {
		<-s.block
	}
	s.finish()
}

// fixedClock is a chart.Clock that always returns the same instant.
type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c) }

// shutdownOrderFixture wires the three instance buffers and a chart against
// the test DB the way setupRoutes does (Start, then Close in a shutdown hook)
// and a queue worker whose last job writes to all of them.
type shutdownOrderFixture struct {
	db      *gorm.DB
	host    string
	srv     *Server
	job     *lateJobServer
	touch   *coreinstance.TouchBuffer
	sigCap  *coreinstance.CapabilityBuffer
	counter *coreinstance.CounterBuffer
	apReq   *chart.Chart
	inbox0  int64
}

func newShutdownOrderFixture(t *testing.T) *shutdownOrderFixture {
	t.Helper()
	db, err := testutil.OpenTestDB()
	require.NoError(t, err)
	testutil.ApplyMigrations(db)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	f := &shutdownOrderFixture{db: db, host: idGen.Generate(time.Now()) + ".shutdown.example"}
	require.NoError(t, db.Create(&model.Instance{
		ID: idGen.Generate(time.Now()), Host: f.host, FirstRetrievedAt: time.Now(),
	}).Error)
	t.Cleanup(func() {
		db.Where("host = ?", f.host).Delete(&model.Instance{})
		db.Where("host = ?", f.host).Delete(&model.InstanceSignatureCapability{})
	})

	instanceRepo := repository.NewInstanceRepository(db)
	svc := coreinstance.NewService(instanceRepo, repository.NewMetaRepository(db), idGen)
	// flush の周期は待たない長さにして、停止時の最後の flush だけが書くようにする。
	f.touch = coreinstance.NewTouchBuffer(svc, time.Hour)
	f.sigCap = coreinstance.NewCapabilityBuffer(repository.NewInstanceSignatureCapabilityRepository(db), time.Hour)
	f.counter = coreinstance.NewCounterBuffer(instanceRepo, time.Hour)
	// 時計を止めておく。基準値を読んだ後に時 (hour) が変わると、別の行を比べてしまう。
	schema := corechartcharts.SchemaApRequest()
	f.apReq, err = chart.New(chart.Config{
		Schema: schema, Repo: chart.NewRepository(db, schema), Lock: chart.NewMemoryLocker(),
		Clock: fixedClock(time.Now().UTC()),
	})
	require.NoError(t, err)
	f.inbox0 = f.inboxReceived(t)

	f.job = &lateJobServer{shutdown: make(chan struct{})}
	f.job.finish = func() {
		_ = f.touch.MarkRequestReceived(f.host)
		f.sigCap.ObserveInboundAlg(f.host, model.SignatureAlgEd25519)
		f.counter.Add(f.host, coreinstance.CounterNotesCount, 1)
		assert.NoError(t, corechartcharts.NewApRequestChart(f.apReq).Inbox())
	}
	d := &lateJobDriver{srv: f.job}
	f.srv = &Server{
		echo: echo.New(), config: &config.Config{}, role: config.RoleBoth,
		queueDriver: d, queueServer: queue.NewServer(d),
	}
	// setupRoutes と同じ順で、同じ形の hook を登録する。
	for _, b := range []interface {
		Start(context.Context)
		Close()
	}{f.touch, f.sigCap, f.counter} {
		b.Start(context.Background())
		f.srv.registerShutdownHook(func(context.Context) { b.Close() })
	}
	mgmt := chart.NewManagementService([]*chart.Chart{f.apReq}, time.Hour)
	f.srv.setChartManagement(mgmt)
	require.NoError(t, mgmt.Start(context.Background()))
	return f
}

// inboxReceived reads the saved inboxReceived count of the current hour.
func (f *shutdownOrderFixture) inboxReceived(t *testing.T) int64 {
	t.Helper()
	res, err := f.apReq.GetChart(context.Background(), chart.SpanHour, 1, nil, "")
	require.NoError(t, err)
	require.Len(t, res["inboxReceived"], 1)
	return res["inboxReceived"][0]
}

// assertJobWritesSaved checks that every write of the late job reached the DB
// and that inboxReceived grew by wantInbox.
func (f *shutdownOrderFixture) assertJobWritesSaved(t *testing.T, wantInbox int64) {
	t.Helper()
	var inst model.Instance
	require.NoError(t, f.db.Where("host = ?", f.host).First(&inst).Error)
	assert.NotNil(t, inst.LatestRequestReceivedAt, "TouchBuffer: 停止中のジョブが書いた受信時刻が消えた")
	assert.Equal(t, 1, inst.NotesCount, "CounterBuffer: 停止中のジョブが足した notesCount が消えた")
	var caps []model.InstanceSignatureCapability
	require.NoError(t, f.db.Where("host = ?", f.host).Find(&caps).Error)
	if assert.Len(t, caps, 1, "CapabilityBuffer: 停止中のジョブが観測した署名方式が消えた") {
		if assert.NotNil(t, caps[0].InboundAlg) {
			assert.Equal(t, model.SignatureAlgEd25519, *caps[0].InboundAlg)
		}
	}
	assert.Equal(t, f.inbox0+wantInbox, f.inboxReceived(t), "chart: 停止中のジョブが数えた inboxReceived が消えた")
}

// TestServer_StopBackgroundKeepsWritesOfJobsFinishingDuringQueueStop
// reproduces #3662: a queue job that finishes while the worker is being
// stopped writes to the instance buffers and a chart. The writes must be
// saved, so the worker has to stop before the buffers and the chart do.
func TestServer_StopBackgroundKeepsWritesOfJobsFinishingDuringQueueStop(t *testing.T) {
	f := newShutdownOrderFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, f.srv.StopBackground(ctx))
	f.assertJobWritesSaved(t, 1)
}

// TestServer_StopBackgroundDoesNotLetAStuckWorkerEatTheHooks checks the
// other side of stopping the queue first: a worker that does not stop (a
// handler ignoring its cancellation) is waited for only a bounded time, the
// hooks keep most of the budget, and what the job writes once it does finish
// (after the buffers closed and the chart stopped) is still saved (#3662).
func TestServer_StopBackgroundDoesNotLetAStuckWorkerEatTheHooks(t *testing.T) {
	f := newShutdownOrderFixture(t)
	f.job.block = make(chan struct{})
	// ジョブの書き込みを、chart の停止時の保存より後にする。hook で 1 件数えて
	// おき、それが保存された (= chartMgmt.Stop が保存を済ませた) のを見てから
	// ジョブを終わらせる。
	finish := f.job.finish
	f.job.finish = func() {
		assert.Eventually(t, func() bool { return f.inboxReceived(t) == f.inbox0+1 },
			5*time.Second, 5*time.Millisecond, "前提: chart の停止時の保存が走っていない")
		finish()
	}
	hookHadTime := make(chan time.Duration, 1)
	// バッファの hook の後に登録する。worker はここで初めて止まるので、ジョブの
	// 書き込みはバッファが閉じた後になる。
	f.srv.registerShutdownHook(func(ctx context.Context) {
		deadline, _ := ctx.Deadline()
		hookHadTime <- time.Until(deadline)
		assert.NoError(t, corechartcharts.NewApRequestChart(f.apReq).Inbox())
		close(f.job.block)
	})

	// queue の待ちは 6 秒の 1/3 = 2 秒まで。hook には約 4 秒が残るので、閾値の
	// 3 秒まで 1 秒の余裕がある (-race の CI で詰まっても揺れないように)。
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- f.srv.StopBackground(ctx) }()
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("止まらない worker を待ち続けて、shutdown hook を呼ばなかった")
	}
	assert.Greater(t, <-hookHadTime, 3*time.Second, "queue の待ちが shutdown hook の時間を食い潰した")
	f.assertJobWritesSaved(t, 2)
}

// TestShutdownStepContext pins the bound of one waiting step: at most the
// given limit and a third of what is left.
func TestShutdownStepContext(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx, c := shutdownStepContext(parent, queueStopMax)
	defer c()
	deadline, ok := ctx.Deadline()
	require.True(t, ok)
	assert.InDelta(t, float64(queueStopMax), float64(time.Until(deadline)), float64(100*time.Millisecond))
}

// TestServer_StopBackgroundSavesTheChartOfAWorkerStoppingAfterTheDeadline
// checks the chart save after a worker that stops only once the shutdown ctx
// has expired: it must not use the expired ctx (#3662 review M-1).
func TestServer_StopBackgroundSavesTheChartOfAWorkerStoppingAfterTheDeadline(t *testing.T) {
	f := newShutdownOrderFixture(t)
	f.job.block = make(chan struct{})
	// worker は停止の期限が切れてから止まる。
	f.srv.registerShutdownHook(func(ctx context.Context) {
		<-ctx.Done()
		close(f.job.block)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- f.srv.StopBackground(ctx) }()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("StopBackground が返らない")
	}
	require.Error(t, ctx.Err(), "前提: worker が止まった時点で期限が切れている")
	f.assertJobWritesSaved(t, 1)
}

// TestServer_StopQueueDoesNotWaitWhenThereIsNothingToStop checks that a
// generation without queue work reports the queue as stopped even with an
// expired ctx, so it neither warns nor saves the charts twice (#3662 review
// L-3).
func TestServer_StopQueueDoesNotWaitWhenThereIsNothingToStop(t *testing.T) {
	s := &Server{config: &config.Config{}, role: config.RoleServer}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// 待ちの select は準備のできた case を無作為に選ぶので、何度か試す。
	for range 50 {
		done, stopped := s.stopQueue(ctx)
		require.True(t, stopped, "止めるものが無いのに、止まらなかった扱いになった")
		<-done
	}
}

// TestServer_StopBackgroundStopsTheAutoscaler checks that stopping a
// generation stops the auto-scale controller started with the queue
// (#3662 review L-6 G).
func TestServer_StopBackgroundStopsTheAutoscaler(t *testing.T) {
	runner, err := startAutoScale(context.Background(), &config.Config{JobQueueAutoScale: true},
		newScriptableDriver(nil), queuemetrics.New(), nil)
	require.NoError(t, err)
	require.NotNil(t, runner)
	t.Cleanup(func() { runner.Stop(context.Background()) })
	s := &Server{echo: echo.New(), config: &config.Config{}, role: config.RoleServer, autoscale: runner}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, s.StopBackground(ctx))
	exited := make(chan struct{})
	go func() {
		runner.wg.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("停止の後も autoscale の goroutine が動いている")
	}
}

// TestLateSaveContext pins the deadline of the chart save after a late
// worker: the later of ctx's deadline and now+lateChartSaveMax, and never
// canceled with ctx (#3662 review r2 L-1).
func TestLateSaveContext(t *testing.T) {
	long, cancelLong := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelLong()
	ctx, c := lateSaveContext(long)
	defer c()
	deadline, ok := ctx.Deadline()
	require.True(t, ok)
	assert.InDelta(t, float64(10*time.Second), float64(time.Until(deadline)), float64(200*time.Millisecond),
		"ctx に時間が残っているのに 2 秒で切った")

	expired, cancelExpired := context.WithCancel(context.Background())
	cancelExpired()
	ctx2, c2 := lateSaveContext(expired)
	defer c2()
	require.NoError(t, ctx2.Err(), "取り消された ctx の取り消しを引き継いだ")
	deadline, ok = ctx2.Deadline()
	require.True(t, ok)
	assert.InDelta(t, float64(lateChartSaveMax), float64(time.Until(deadline)), float64(200*time.Millisecond))
}
