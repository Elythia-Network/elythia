// Package e2e_federation runs federation end-to-end tests against two
// mk-go servers communicating via ActivityPub over localhost.
//
// TestMain boots a single PostgreSQL container with two databases
// (misskey_a, misskey_b), a single Redis container with two DB numbers
// (0, 1), and starts two full Echo servers via server.New().
package e2e_federation

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/config"
	"github.com/shiroha-a/mk/internal/core/cache"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/server"
	"github.com/shiroha-a/mk/internal/testutil"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// makeSyncDeliverHook builds a synchronous AP deliver function for tests
// (#780). 本番経路は queue 経由だが test では queue worker pickup
// が確認できないので、sign + HTTP POST を inline 実行する。
//
// HTTP client は SSRF transport を含めない (e2e は loopback で localhost
// のみ POST するため)。失敗時 (非 2xx / 接続エラー) は error を返すので
// DeliverService 側で deliver 経路の error として扱われる。
func makeSyncDeliverHook() func(queue.DeliverPayload) error {
	apClient := activitypub.NewClient(&http.Client{Timeout: 10 * time.Second}, "mk-go-e2e-test")
	return func(p queue.DeliverPayload) error {
		key, err := activitypub.NewPrivateKey(p.KeyID, p.KeyPEM)
		if err != nil {
			return fmt.Errorf("parse key: %w", err)
		}
		resp, err := apClient.PostSigned(p.Inbox, p.Body, key)
		if err != nil {
			return fmt.Errorf("post signed to %s: %w", p.Inbox, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("inbox %s returned %d", p.Inbox, resp.StatusCode)
		}
		return nil
	}
}

// testServer はテスト用サーバーの接続情報を保持する。
type testServer struct {
	BaseURL string
	Origin  string
	Host    string
}

// テスト全体で共有するサーバー情報
var (
	serverA *testServer
	serverB *testServer
	// redisAddr は両 server が共有する Redis container の addr。reversi
	// federation check 用 cache key を直接 pre-populate するなど、test 側で
	// Redis を直接触る必要があるシナリオ (#435) で使う。
	redisAddr string
	// dbAGlobal / dbBGlobal は両 server の gorm.DB。reset-db 後に
	// meta.federation を上書きする等、test 側で DB を直接触る必要が
	// あるシナリオ (#780) で使う。
	dbAGlobal *gorm.DB
	dbBGlobal *gorm.DB
)

func TestMain(m *testing.M) {
	ctx := context.Background()

	// PostgreSQLコンテナ起動 (misskey_test = サーバーA用)
	testDB, err := testutil.SetupPostgres(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e_federation: postgres setup failed: %v\n", err)
		os.Exit(1)
	}
	defer testDB.Teardown(ctx)

	// サーバーB用のデータベースを同一コンテナ内に作成
	dbB, err := createSecondDB(testDB.DB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e_federation: second db setup failed: %v\n", err)
		os.Exit(1)
	}

	// Redisコンテナ起動
	testRedis, err := testutil.SetupRedis(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e_federation: redis setup failed: %v\n", err)
		os.Exit(1)
	}
	defer testRedis.Teardown(ctx)

	// サーバーA: ポート事前確保 + config + httptest
	lnA, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e_federation: listen A failed: %v\n", err)
		os.Exit(1)
	}
	addrA := lnA.Addr().String()
	serverA = &testServer{
		BaseURL: "http://" + addrA,
		Origin:  "http://" + addrA,
		Host:    addrA,
	}

	lnB, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e_federation: listen B failed: %v\n", err)
		os.Exit(1)
	}
	addrB := lnB.Addr().String()
	serverB = &testServer{
		BaseURL: "http://" + addrB,
		Origin:  "http://" + addrB,
		Host:    addrB,
	}

	redisAddr = fmt.Sprintf("%s:%d", testRedis.Host(), testRedis.Port())
	// 後続の resetDB / 個別 test から meta.federation を 'all' に上書きする
	// ために両 DB を package-level variable に export する (#780)。
	dbAGlobal = testDB.DB
	dbBGlobal = dbB

	// サーバーA: Redis DB 0
	redisOptsA := config.RedisOptions{
		Host: testRedis.Host(),
		Port: testRedis.Port(),
		DB:   0,
	}
	redisClientA := goredis.NewClient(&goredis.Options{Addr: redisAddr, DB: 0})
	defer redisClientA.Close()
	redisClientsA := &cache.RedisClients{
		Default:   redisClientA,
		Pubsub:    redisClientA,
		JobQueue:  redisClientA,
		Timelines: redisClientA,
		Reactions: redisClientA,
	}

	cfgA := &config.Config{
		URL:               serverA.BaseURL,
		Port:              lnA.Addr().(*net.TCPAddr).Port,
		Host:              addrA,
		Hostname:          "127.0.0.1",
		Scheme:            "http",
		WsScheme:          "ws",
		Version:           config.MisskeyVersion,
		TestMode:          true,
		ID:                "aidx",
		Redis:             redisOptsA,
		RedisForPubsub:    redisOptsA,
		RedisForJobQueue:  redisOptsA,
		RedisForTimelines: redisOptsA,
		RedisForReactions: redisOptsA,
		// e2e tests bind httptest servers on 127.0.0.1; explicitly allow
		// loopback so the SSRF-safe transport (#323) does not block them.
		AllowedPrivateNetworks: []string{"127.0.0.0/8"},
	}

	srvA, err := server.New(cfgA, testDB.DB, redisClientsA)
	if err != nil {
		fmt.Fprintf(os.Stderr, "server.New A: %v\n", err)
		os.Exit(1)
	}
	tsA := &httptest.Server{
		Listener: lnA,
		Config:   &http.Server{Handler: srvA.Handler()},
	}
	tsA.Start()
	defer tsA.Close()
	// reversi 等の deliver queue 経由のテスト用に queue worker を起動
	// (#435)。Server.Start() は HTTP listener も込みで動かしてしまうので、
	// e2e は test 側で listener を握る都合上 background 部分だけ起動する。
	if err := srvA.StartBackgroundForTest(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e_federation: queue worker A start: %v\n", err)
		os.Exit(1)
	}
	// queue 経由の deliver が test 環境で動かないので sync deliver hook で
	// bypass する (#780)。inbox 受信側 (= 相手 server) は sign POST を 200/202
	// で受けて federation processor を回す。
	srvA.SetSyncDeliverHookForTest(makeSyncDeliverHook())

	// サーバーB: Redis DB 1
	redisOptsB := config.RedisOptions{
		Host: testRedis.Host(),
		Port: testRedis.Port(),
		DB:   1,
	}
	redisClientB := goredis.NewClient(&goredis.Options{Addr: redisAddr, DB: 1})
	defer redisClientB.Close()
	redisClientsB := &cache.RedisClients{
		Default:   redisClientB,
		Pubsub:    redisClientB,
		JobQueue:  redisClientB,
		Timelines: redisClientB,
		Reactions: redisClientB,
	}

	cfgB := &config.Config{
		URL:                    serverB.BaseURL,
		Port:                   lnB.Addr().(*net.TCPAddr).Port,
		Host:                   addrB,
		Hostname:               "127.0.0.1",
		Scheme:                 "http",
		WsScheme:               "ws",
		Version:                config.MisskeyVersion,
		TestMode:               true,
		ID:                     "aidx",
		Redis:                  redisOptsB,
		RedisForPubsub:         redisOptsB,
		RedisForJobQueue:       redisOptsB,
		RedisForTimelines:      redisOptsB,
		RedisForReactions:      redisOptsB,
		AllowedPrivateNetworks: []string{"127.0.0.0/8"},
	}

	srvB, err := server.New(cfgB, dbB, redisClientsB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "server.New B: %v\n", err)
		os.Exit(1)
	}
	tsB := &httptest.Server{
		Listener: lnB,
		Config:   &http.Server{Handler: srvB.Handler()},
	}
	tsB.Start()
	defer tsB.Close()
	if err := srvB.StartBackgroundForTest(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e_federation: queue worker B start: %v\n", err)
		os.Exit(1)
	}
	srvB.SetSyncDeliverHookForTest(makeSyncDeliverHook())

	os.Exit(m.Run())
}

// createSecondDB は同一PostgreSQLコンテナ内に misskey_b データベースを作成し、
// マイグレーションを適用した gorm.DB を返す。
func createSecondDB(primaryDB *gorm.DB) (*gorm.DB, error) {
	// primaryDB の underlying SQL connection で CREATE DATABASE
	sqlDB, err := primaryDB.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql.DB: %w", err)
	}

	// misskey_b が存在しなければ作成
	var exists bool
	row := sqlDB.QueryRow("SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = 'misskey_b')")
	if err := row.Scan(&exists); err != nil {
		return nil, fmt.Errorf("check db exists: %w", err)
	}
	if !exists {
		if _, err := sqlDB.Exec("CREATE DATABASE misskey_b"); err != nil {
			return nil, fmt.Errorf("create misskey_b: %w", err)
		}
	}

	// primaryDB の DSN から misskey_b 用の DSN を構築
	// testcontainers の接続文字列はホスト名:ポートが動的なので、
	// primaryDB の接続情報を流用して dbname だけ差し替える
	var host, port string
	// host は下のフォールバックが効く。port は失敗すると空のまま DSN に入るが、
	// 変更前と同じ挙動なのでここでは扱いを変えない。
	_ = sqlDB.QueryRow("SELECT inet_server_addr()").Scan(&host)
	_ = sqlDB.QueryRow("SELECT inet_server_port()").Scan(&port)
	if host == "" {
		host = "127.0.0.1"
	}

	dsn := fmt.Sprintf("host=%s port=%s user=test password=test dbname=misskey_b sslmode=disable",
		host, port)

	dbB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger:                   logger.Default.LogMode(logger.Silent),
		DisableNestedTransaction: true,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to misskey_b: %w", err)
	}

	testutil.ApplyMigrations(dbB)

	return dbB, nil
}
