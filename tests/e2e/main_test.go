// Package e2e runs end-to-end tests against a real mk-go server.
//
// TestMain boots PostgreSQL + Redis via testcontainers, applies
// migrations, starts the full Echo server via server.New(), and
// exposes the base URL as a package-level variable for sub-tests.
package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	goredis "github.com/redis/go-redis/v9"
	"github.com/shiroha-a/mk/internal/config"
	"github.com/shiroha-a/mk/internal/core/cache"
	"github.com/shiroha-a/mk/internal/server"
	"github.com/shiroha-a/mk/internal/testutil"
)

// テスト全体で共有するサーバーとベースURL
var (
	ts      *httptest.Server
	baseURL string // "http://127.0.0.1:<port>"
	origin  string // テスト用origin (baseURLと同じ)
	host    string // "127.0.0.1:<port>"
)

func TestMain(m *testing.M) {
	ctx := context.Background()

	// PostgreSQL コンテナ起動 + マイグレーション
	testDB, err := testutil.SetupPostgres(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: postgres setup failed: %v\n", err)
		os.Exit(1)
	}
	defer testDB.Teardown(ctx)

	// Redis コンテナ起動
	testRedis, err := testutil.SetupRedis(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: redis setup failed: %v\n", err)
		os.Exit(1)
	}
	defer testRedis.Teardown(ctx)

	// ポートを事前確保して config に正しいURLを入れる。
	// server.New() がハンドラにURLを渡すので、事後変更では間に合わない。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: failed to allocate port: %v\n", err)
		os.Exit(1)
	}
	addr := ln.Addr().String()
	host = addr
	baseURL = "http://" + addr
	origin = baseURL

	redisOpts := config.RedisOptions{
		Host: testRedis.Host(),
		Port: testRedis.Port(),
	}
	cfg := &config.Config{
		URL:               baseURL,
		Port:              ln.Addr().(*net.TCPAddr).Port,
		Host:              host,
		Hostname:          "127.0.0.1",
		Scheme:            "http",
		WsScheme:          "ws",
		Version:           config.MisskeyVersion,
		TestMode:          true,
		ID:                "aidx",
		Redis:             redisOpts,
		RedisForPubsub:    redisOpts,
		RedisForJobQueue:  redisOpts,
		RedisForTimelines: redisOpts,
		RedisForReactions: redisOpts,
	}

	// cache.RedisClients を手動構築 (全チャネル同一Redis)
	redisClient := goredis.NewClient(&goredis.Options{
		Addr: fmt.Sprintf("%s:%d", testRedis.Host(), testRedis.Port()),
	})
	defer redisClient.Close()

	redisClients := &cache.RedisClients{
		Default:   redisClient,
		Pubsub:    redisClient,
		JobQueue:  redisClient,
		Timelines: redisClient,
		Reactions: redisClient,
	}

	// server.New でEchoサーバー構築
	srv, err := server.New(cfg, testDB.DB, redisClients)
	if err != nil {
		fmt.Fprintf(os.Stderr, "server.New: %v\n", err)
		os.Exit(1)
	}

	// 事前確保したlistenerでhttptestサーバー起動
	ts = &httptest.Server{
		Listener: ln,
		Config:   &http.Server{Handler: srv.Handler()},
	}
	ts.Start()
	defer ts.Close()

	os.Exit(m.Run())
}
