package server

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/gorilla/websocket"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// wakeStatementRecorder records the statements that clear "isHibernated" of
// one user, through GORM callbacks on db.
type wakeStatementRecorder struct {
	userID string
	mu     sync.Mutex
	n      int
}

func (r *wakeStatementRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

func recordWakeStatements(t *testing.T, db *gorm.DB, userID string) *wakeStatementRecorder {
	t.Helper()
	r := &wakeStatementRecorder{userID: userID}
	record := func(tx *gorm.DB) {
		if !strings.Contains(tx.Statement.SQL.String(), `SET "isHibernated" = false`) {
			return
		}
		for _, v := range tx.Statement.Vars {
			if s, ok := v.(string); ok && s == userID {
				r.mu.Lock()
				r.n++
				r.mu.Unlock()
				return
			}
		}
	}
	name := "test:wake4270:" + userID
	cb := db.Callback()
	require.NoError(t, cb.Raw().After("gorm:raw").Register(name, record))
	require.NoError(t, cb.Row().After("gorm:row").Register(name, record))
	require.NoError(t, cb.Query().After("gorm:query").Register(name, record))
	require.NoError(t, cb.Update().After("gorm:update").Register(name, record))
	t.Cleanup(func() {
		_ = cb.Raw().Remove(name)
		_ = cb.Row().Remove(name)
		_ = cb.Query().Remove(name)
		_ = cb.Update().Remove(name)
	})
	return r
}

// TestStreamingConnect_WakesOnlyHibernatedUsers opens an authenticated
// /streaming connection through the whole server and pins that the statement
// clearing the hibernation runs only for a hibernated user (#4270), and that
// such a user is still woken, like upstream UserService.updateLastActiveDate
// (#4019).
//
// 休眠は TS 版から移した DB にだけあり、Elythia は休眠させない。休眠していない利用者に
// 毎回流していた文は、0 行に当たるだけだった。
func TestStreamingConnect_WakesOnlyHibernatedUsers(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "1")
	t.Setenv(config.EnvOnlyQueue, "")
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	db := serverIntegrationDB

	const prefix = "hib4270"
	cleanup := func() {
		db.Exec(`DELETE FROM "following" WHERE id LIKE ?`, prefix+"%")
		db.Exec(`DELETE FROM "user" WHERE id LIKE ?`, prefix+"%")
	}
	cleanup()
	t.Cleanup(cleanup)
	users := repository.NewUserRepository(db)
	require.NoError(t, users.Create(&model.User{ID: prefix + "followee", Username: prefix + "followee", UsernameLower: prefix + "followee", AvatarDecorations: datatypes.JSON([]byte("[]"))}))

	cases := []struct {
		name       string
		hibernated bool
		wakes      int
	}{
		{name: "awake user", hibernated: false, wakes: 0},
		{name: "hibernated user", hibernated: true, wakes: 1},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := prefix + "u" + strconv.Itoa(i)
			token := prefix + "token" + strconv.Itoa(i)
			require.NoError(t, users.Create(&model.User{ID: id, Username: id, UsernameLower: id, Token: &token, IsHibernated: tc.hibernated, AvatarDecorations: datatypes.JSON([]byte("[]"))}))
			require.NoError(t, users.CreateProfile(&model.UserProfile{UserID: id}))
			followID := prefix + "f" + strconv.Itoa(i)
			require.NoError(t, db.Exec(`INSERT INTO "following" ("id", "followerId", "followeeId", "isFollowerHibernated") VALUES (?, ?, ?, ?)`,
				followID, id, prefix+"followee", tc.hibernated).Error)

			// callback は、DB を使うサーバーを組み立てる前に入れる。GORM の登録は
			// 処理の一覧をロックせずに作り直すので、クエリと並ぶと -race が落とす
			wakes := recordWakeStatements(t, db, id)

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
				Redis: redisOptions, RedisForPubsub: redisOptions, RedisForJobQueue: redisOptions,
				RedisForTimelines: redisOptions, RedisForReactions: redisOptions,
			}
			srv, err := New(cfg, db, redisClients)
			require.NoError(t, err)
			var shutdown sync.Once
			stop := func() {
				shutdown.Do(func() { require.NoError(t, srv.Shutdown(context.Background())) })
			}
			t.Cleanup(stop)
			ts := httptest.NewServer(srv.Handler())
			t.Cleanup(ts.Close)

			url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/streaming?i=" + token
			conn, _, err := (&websocket.Dialer{HandshakeTimeout: 5 * time.Second}).Dial(url, nil)
			require.NoError(t, err)
			// 接続を受けた証拠に lastActiveDate が書かれるのを待つ。休眠の解除は同じ
			// goroutine でその後に走るので、世代を止めて (background を待って) から数える。
			require.Eventually(t, func() bool {
				var n int64
				db.Model(&model.User{}).Where(`id = ? AND "lastActiveDate" IS NOT NULL`, id).Count(&n)
				return n == 1
			}, 5*time.Second, 10*time.Millisecond, "the connection did not record lastActiveDate")
			_ = conn.Close()
			stop()

			assert.Equal(t, tc.wakes, wakes.count(), "statements clearing the hibernation")
			var got model.User
			require.NoError(t, db.Where("id = ?", id).Take(&got).Error)
			assert.False(t, got.IsHibernated, "the user stays hibernated after connecting")
			var followerHibernated bool
			require.NoError(t, db.Raw(`SELECT "isFollowerHibernated" FROM "following" WHERE "id" = ?`, followID).Row().Scan(&followerHibernated))
			assert.False(t, followerHibernated, "the follow of the user stays hibernated, so the home timeline misses the notes")
		})
	}
}
