package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/labstack/echo/v4"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/elythia-network/elythia/plugin"
)

type constructionCloseDriver struct {
	driver.Driver
	workerStopped  <-chan struct{}
	closeCalls     int
	closedTooEarly bool
	closeErr       error
}

func (d *constructionCloseDriver) Close() error {
	d.closeCalls++
	select {
	case <-d.workerStopped:
	default:
		d.closedTooEarly = true
	}
	if d.Driver != nil {
		if err := d.Driver.Close(); err != nil {
			return err
		}
	}
	return d.closeErr
}

func TestNew_QueueOnlyRegistersEffectivePolicyBeforePluginJobs(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "")
	t.Setenv(config.EnvOnlyQueue, "1")

	db, err := testutil.OpenTestDB()
	require.NoError(t, err)
	testutil.ApplyMigrations(db)

	mr := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	redisClients := &cache.RedisClients{
		Default: redisClient, Pubsub: redisClient, JobQueue: redisClient,
		Timelines: redisClient, Reactions: redisClient,
	}
	redisPort, err := strconv.Atoi(mr.Port())
	require.NoError(t, err)
	redisOptions := config.RedisOptions{Host: mr.Host(), Port: redisPort}
	cfg := &config.Config{
		URL: "http://example.test", Host: "example.test", Hostname: "example.test",
		Scheme: "http", WsScheme: "ws", ID: "aidx", TestMode: true,
		JobQueueDriver: "mkq", MediaProxySecret: []byte("test-secret"),
		Redis: redisOptions, RedisForPubsub: redisOptions, RedisForJobQueue: redisOptions,
		RedisForTimelines: redisOptions, RedisForReactions: redisOptions,
	}

	var serviceSeen bool
	var policyService *roleEffectivePolicyInvalidator
	var pluginCtx plugin.Context
	finalized := false
	finalization := make(chan struct {
		finalized bool
		err       error
	}, 1)
	pluginWork := make(chan string, 4)
	def := plugin.Definition{
		Name:       "queue-policy",
		APIVersion: plugin.APIVersion,
		EffectivePolicies: func(ctx plugin.Context, invalidator plugin.EffectivePolicyInvalidator) (plugin.EffectivePolicyRegistration, error) {
			pluginCtx = ctx
			pluginCtx.Go(func() {
				pluginWork <- "factory"
				_, err := pluginCtx.API().Anonymous().Call(context.Background(), "round4-finalization-probe", nil)
				finalization <- struct {
					finalized bool
					err       error
				}{finalized: finalized, err: err}
			})
			var ok bool
			policyService, ok = invalidator.(*roleEffectivePolicyInvalidator)
			if !ok {
				return plugin.EffectivePolicyRegistration{}, fmt.Errorf("unexpected invalidator %T", invalidator)
			}
			return plugin.EffectivePolicyRegistration{
				Keys: []string{"canSearchNotes"},
				Resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
					return []plugin.EffectivePolicyContribution{{Key: "canSearchNotes", Value: true}}, nil
				},
			}, nil
		},
		Jobs: func(_ plugin.Context, jobs plugin.Jobs) error {
			pluginCtx.Go(func() { pluginWork <- "jobs" })
			if policyService == nil {
				return fmt.Errorf("role policy service was not supplied")
			}
			policies, err := policyService.service.GetUserPoliciesChecked("queue-user")
			if err != nil {
				return err
			}
			if policies["canSearchNotes"] != true {
				return fmt.Errorf("provider is not registered before jobs")
			}
			serviceSeen = true
			jobs.Handle("refresh", func(context.Context, json.RawMessage) error { return nil })
			return nil
		},
	}

	cleanupCalls := 0
	// **`newServer` はプロセス共有の状態を張り替える** (#2795)。
	restoreProcessGlobals(t)
	srv, err := newServer(cfg, db, redisClients, []plugin.Definition{def}, noopStorage, func(s *Server) {
		s.pluginGoStarter = func(fn func()) { fn() }
		s.beforePluginGoRelease = func() { finalized = true }
		s.registerShutdownHook(func(context.Context) { cleanupCalls++ })
	})
	require.NoError(t, err)
	require.Zero(t, cleanupCalls)
	require.Equal(t, config.RoleQueue, srv.role)
	require.NotNil(t, srv.roleService)
	require.Same(t, srv.roleService, policyService.service)
	require.True(t, serviceSeen)
	require.ElementsMatch(t, []string{"factory", "jobs"}, []string{<-pluginWork, <-pluginWork})
	finalizationResult := <-finalization
	require.True(t, finalizationResult.finalized)
	require.NoError(t, finalizationResult.err)
	pluginCtx.Go(func() { pluginWork <- "runtime" })
	require.Equal(t, "runtime", <-pluginWork)
	require.NoError(t, srv.Shutdown(context.Background()))
	require.NoError(t, srv.Shutdown(context.Background()))
	require.Equal(t, 1, cleanupCalls)
	select {
	case got := <-pluginWork:
		t.Fatalf("queued plugin work ran more than once: %s", got)
	default:
	}
}

func TestNew_PluginSetupFailureRollsBackConstructionResources(t *testing.T) {
	db, err := testutil.OpenTestDB()
	require.NoError(t, err)
	testutil.ApplyMigrations(db)
	require.NoError(t, repository.NewMetaRepository(db).EnsureInitial("constructor-cleanup-meta"))

	tests := []struct {
		name      string
		onlyQueue string
	}{
		{name: "role both"},
		{name: "queue only", onlyQueue: "1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(config.EnvOnlyServer, "")
			t.Setenv(config.EnvOnlyQueue, tt.onlyQueue)
			require.NoError(t, db.Model(&model.Meta{}).Where("1 = 1").Update("enableReactionsBuffering", true).Error)
			defer func() {
				require.NoError(t, db.Model(&model.Meta{}).Where("1 = 1").Update("enableReactionsBuffering", false).Error)
			}()

			mr := miniredis.RunT(t)
			redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
			redisClients := &cache.RedisClients{
				Default: redisClient, Pubsub: redisClient, JobQueue: redisClient,
				Timelines: redisClient, Reactions: redisClient,
			}
			redisPort, err := strconv.Atoi(mr.Port())
			require.NoError(t, err)
			redisOptions := config.RedisOptions{Host: mr.Host(), Port: redisPort}
			cfg := testDBConfig(t)
			cfg.URL = "http://example.test"
			cfg.Host = "example.test"
			cfg.Hostname = "example.test"
			cfg.Scheme = "http"
			cfg.WsScheme = "ws"
			cfg.ID = "aidx"
			cfg.TestMode = true
			cfg.JobQueueDriver = "mkq"
			cfg.MediaProxySecret = []byte("test-secret")
			cfg.Redis = redisOptions
			cfg.RedisForPubsub = redisOptions
			cfg.RedisForJobQueue = redisOptions
			cfg.RedisForTimelines = redisOptions
			cfg.RedisForReactions = redisOptions
			for _, name := range []string{"constructor-cleanup-success", "cleanup-secret-plugin"} {
				dropPluginSchema(t, cfg, name)
				defer dropPluginSchema(t, cfg, name)
			}

			var stores []*sql.DB
			var preparationCtx plugin.Context
			callbackCalled := false
			callbackWork := make(chan struct{}, 4)
			plugins := []plugin.Definition{
				{
					Name:       "constructor-cleanup-success",
					APIVersion: plugin.APIVersion,
					EffectivePolicies: func(ctx plugin.Context, _ plugin.EffectivePolicyInvalidator) (plugin.EffectivePolicyRegistration, error) {
						preparationCtx = ctx
						stores = append(stores, ctx.Storage().DB())
						ctx.Go(func() { callbackWork <- struct{}{} })
						return plugin.EffectivePolicyRegistration{
							Keys: []string{"canSearchNotes"},
							Resolve: func(context.Context, plugin.EffectivePolicyRequest) ([]plugin.EffectivePolicyContribution, error) {
								return nil, nil
							},
						}, nil
					},
					Routes: func(ctx plugin.Context, _ plugin.Router) error {
						callbackCalled = true
						ctx.Go(func() { callbackWork <- struct{}{} })
						return nil
					},
					Jobs: func(ctx plugin.Context, _ plugin.Jobs) error {
						callbackCalled = true
						ctx.Go(func() { callbackWork <- struct{}{} })
						return nil
					},
				},
				{
					Name:       "cleanup-secret-plugin",
					APIVersion: plugin.APIVersion,
					EffectivePolicies: func(ctx plugin.Context, _ plugin.EffectivePolicyInvalidator) (plugin.EffectivePolicyRegistration, error) {
						stores = append(stores, ctx.Storage().DB())
						return plugin.EffectivePolicyRegistration{}, errors.New("raw secret provider failure")
					},
				},
			}

			cleanupCalls := 0
			workerStarted := make(chan struct{})
			workerStopped := make(chan struct{})
			var failedServer *Server
			var closeDriver *constructionCloseDriver
			// **`newServer` はプロセス共有の状態を張り替える** (#2795)。
			restoreProcessGlobals(t)
			srv, err := newServer(cfg, db, redisClients, plugins, nil, func(s *Server) {
				failedServer = s
				s.pluginGoStarter = func(fn func()) { fn() }
				s.reactionFlushWorker = func(ctx context.Context, _ func() error) {
					close(workerStarted)
					<-ctx.Done()
					close(workerStopped)
				}
				closeDriver = &constructionCloseDriver{
					Driver: s.queueDriver, workerStopped: workerStopped,
					closeErr: errors.New("queue close failed during rollback"),
				}
				s.queueDriver = closeDriver
				s.registerShutdownHook(func(context.Context) { cleanupCalls++ })
			})
			require.Nil(t, srv)
			require.ErrorIs(t, err, errPluginEffectivePolicySetup)
			require.Equal(t, errPluginEffectivePolicySetup.Error(), err.Error())
			require.NotContains(t, err.Error(), "cleanup-secret-plugin")
			require.NotContains(t, err.Error(), "raw secret provider failure")
			require.False(t, callbackCalled)
			require.NotNil(t, preparationCtx)
			preparationCtx.Go(func() { callbackWork <- struct{}{} })
			select {
			case <-callbackWork:
				t.Fatal("an earlier plugin started work before global preparation completed")
			default:
			}
			require.Len(t, stores, 2)
			for _, store := range stores {
				require.Error(t, store.PingContext(context.Background()))
			}
			require.Equal(t, 1, cleanupCalls)
			select {
			case <-workerStarted:
			default:
				t.Fatal("reaction flush worker did not start")
			}
			select {
			case <-workerStopped:
			default:
				t.Fatal("reaction flush worker was still active after rollback")
			}
			require.Equal(t, 1, closeDriver.closeCalls)
			require.False(t, closeDriver.closedTooEarly)
			failedServer.cleanupConstruction()
			require.NoError(t, failedServer.Shutdown(context.Background()))
			require.Equal(t, 1, closeDriver.closeCalls)

			// 注入されたDBとRedisはnewServerの所有物ではない。
			require.NoError(t, redisClient.Ping(context.Background()).Err())
			sqlDB, err := db.DB()
			require.NoError(t, err)
			require.NoError(t, sqlDB.PingContext(context.Background()))
		})
	}
}

func TestStartConstructionWorkerCleanupWaitsAndRunsOnce(t *testing.T) {
	cancelled := make(chan struct{})
	allowExit := make(chan struct{})
	runCalls := 0
	s := &Server{}
	s.startConstructionWorker(func(ctx context.Context) {
		runCalls++
		<-ctx.Done()
		close(cancelled)
		<-allowExit
	})

	returned := make(chan struct{})
	go func() {
		s.cleanupConstruction()
		close(returned)
	}()
	<-cancelled
	select {
	case <-returned:
		t.Fatal("cleanup returned before the worker stopped")
	default:
	}
	close(allowExit)
	<-returned
	s.cleanupConstruction()
	require.Equal(t, 1, runCalls)
}

func TestServerShutdown_DeadlineBoundsReactionFlushDrain(t *testing.T) {
	workerStarted := make(chan struct{})
	workerStopping := make(chan struct{})
	workerStopped := make(chan struct{})
	allowWorkerExit := make(chan struct{})
	closeDriver := &constructionCloseDriver{workerStopped: workerStopped}
	s := &Server{
		echo:        echo.New(),
		config:      &config.Config{},
		role:        config.RoleServer,
		queueDriver: closeDriver,
		reactionFlushWorker: func(ctx context.Context, _ func() error) {
			close(workerStarted)
			<-ctx.Done()
			close(workerStopping)
			<-allowWorkerExit
			close(workerStopped)
		},
	}
	s.startConstructionWorker(func(ctx context.Context) {
		s.reactionFlushWorker(ctx, func() error { return nil })
	})
	<-workerStarted

	shutdownCtx, cancel := context.WithCancel(context.Background())
	cancel()
	shutdownReturned := make(chan error, 1)
	go func() { shutdownReturned <- s.Shutdown(shutdownCtx) }()
	<-workerStopping
	select {
	case err := <-shutdownReturned:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Shutdown ignored its canceled context while draining the reaction flush worker")
	}
	close(allowWorkerExit)
	<-workerStopped
	require.NoError(t, s.Shutdown(context.Background()))
	require.Equal(t, 1, closeDriver.closeCalls)
}
