package server

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// 組み立てただけの本体 (待ち受けない) から、管理 API と同じ削除の処理を呼べる (#4248)。
func TestDeleteMigrated_ThroughTheBuiltServer(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "")
	t.Setenv(config.EnvOnlyQueue, "")
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	db := serverIntegrationDB

	// ジョブが積まれることまで見るので、本物の Redis を使う (miniredis は mkq の
	// Lua が使う cmsgpack を持たない)。
	tr, err := testutil.SetupRedis(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { tr.Teardown(context.Background()) })
	redisClient := tr.Client
	redisClients := &cache.RedisClients{
		Default: redisClient, Pubsub: redisClient, JobQueue: redisClient,
		Timelines: redisClient, Reactions: redisClient,
	}
	host, port, err := net.SplitHostPort(tr.Addr)
	require.NoError(t, err)
	redisPort, err := strconv.Atoi(port)
	require.NoError(t, err)
	redisOptions := config.RedisOptions{Host: host, Port: redisPort}
	cfg := &config.Config{
		URL: "http://example.test", Host: "example.test", Hostname: "example.test",
		Scheme: "http", WsScheme: "ws", ID: "aidx", TestMode: true,
		Redis: redisOptions, RedisForPubsub: redisOptions, RedisForJobQueue: redisOptions,
		RedisForTimelines: redisOptions, RedisForReactions: redisOptions,
	}
	// パッケージの schema の meta は連合しない設定 (none) のことがあり、そのままだと配送先が
	// すべて除外される。組み立てる前に all にし、終わったら戻す (meta はプロセスの中で覚える)。
	var federation []string
	require.NoError(t, db.Raw(`SELECT federation FROM meta`).Scan(&federation).Error)
	require.Len(t, federation, 1)
	require.NoError(t, db.Exec(`UPDATE meta SET federation = 'all'`).Error)
	t.Cleanup(func() { assert.NoError(t, db.Exec(`UPDATE meta SET federation = ?`, federation[0]).Error) })

	srv, err := New(cfg, db, redisClients)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })

	// token は char(16)。
	for _, u := range []struct{ id, name, token string }{
		{"mig4248author", "mig4248author", "mig4248tokena00"},
		{"mig4248pending", "mig4248pending", "mig4248tokenb00"},
	} {
		token := u.token
		require.NoError(t, db.Create(&model.User{
			ID: u.id, Username: u.name, UsernameLower: u.name, Token: &token,
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		}).Error)
		t.Cleanup(func() { assert.NoError(t, db.Exec(`DELETE FROM "user" WHERE id = ?`, u.id).Error) })
		// 配送のジョブは署名鍵がある利用者のものだけが積まれる (中身は積むときには使わない)。
		require.NoError(t, db.Create(&model.UserKeypair{UserID: u.id, PublicKey: "public-" + u.id, PrivateKey: "private-" + u.id}).Error)
		t.Cleanup(func() { assert.NoError(t, db.Exec(`DELETE FROM user_keypair WHERE "userId" = ?`, u.id).Error) })
	}
	require.NoError(t, db.Create(&model.Note{ID: "mig4248note", UserID: "mig4248author", Visibility: model.NoteVisibilityPublic}).Error)
	t.Cleanup(func() { assert.NoError(t, db.Exec(`DELETE FROM note WHERE id = ?`, "mig4248note").Error) })

	// 知っているリモートの利用者の受信箱へ、削除の配送のジョブが積まれることを見る。
	remoteHost := "mig4248.remote.example"
	remoteInbox := "https://mig4248.remote.example/inbox"
	require.NoError(t, db.Create(&model.User{
		ID: "mig4248remote", Username: "mig4248remote", UsernameLower: "mig4248remote",
		Host: &remoteHost, Inbox: &remoteInbox, AvatarDecorations: datatypes.JSON([]byte("[]")),
	}).Error)
	t.Cleanup(func() { assert.NoError(t, db.Exec(`DELETE FROM "user" WHERE id = ?`, "mig4248remote").Error) })

	require.NoError(t, srv.DeleteMigratedNote("mig4248note"))
	require.NoError(t, srv.WaitNoteDeletes(t.Context()))

	var notes int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM note WHERE id = ?`, "mig4248note").Scan(&notes).Error)
	assert.Zero(t, notes)
	assert.Error(t, srv.DeleteMigratedNote("mig4248note"), "消えた投稿")

	require.NoError(t, srv.DeleteMigratedAccount("mig4248pending"))
	var flags struct{ IsDeleted, IsSuspended bool }
	require.NoError(t, db.Raw(`SELECT "isDeleted" AS is_deleted, "isSuspended" AS is_suspended FROM "user" WHERE id = ?`, "mig4248pending").
		Scan(&flags).Error)
	assert.True(t, flags.IsDeleted)
	assert.True(t, flags.IsSuspended)
	assert.Error(t, srv.DeleteMigratedAccount("mig4248nobody"))

	// 積んだジョブは止めるときに書き出される (convert は最後に Shutdown する)。
	require.NoError(t, srv.Shutdown(context.Background()))
	assert.Zero(t, srv.BackgroundDropped())
	jobs, err := redisClient.LRange(t.Context(), "bull:deliver:wait", 0, -1).Result()
	require.NoError(t, err)
	require.NotEmpty(t, jobs, "削除の配送のジョブが積まれる")
	// アカウントの削除の Delete(actor) も同じ受信箱へ積まれるので、本文に投稿の id を
	// 含むジョブ (投稿の削除の配送) を名指しで探す。
	noteDelete := 0
	for _, id := range jobs {
		d, err := redisClient.HGet(t.Context(), "bull:deliver:"+id, "data").Result()
		require.NoError(t, err)
		if strings.Contains(d, "mig4248note") && strings.Contains(d, remoteInbox) {
			noteDelete++
		}
	}
	assert.Equal(t, 1, noteDelete, "投稿の削除が、知っているリモートの受信箱へ積まれる")
	keys, err := redisClient.Keys(t.Context(), "*deleteAccount*").Result()
	require.NoError(t, err)
	assert.NotEmpty(t, keys, "アカウントの後片付けのジョブが積まれる")
}

// ルートを組み立てていない本体では、何もせずにエラーを返す。
func TestDeleteMigrated_NotWired(t *testing.T) {
	s := &Server{}
	assert.ErrorIs(t, s.DeleteMigratedAccount("u1"), errMigrationNotWired)
	assert.ErrorIs(t, s.DeleteMigratedNote("n1"), errMigrationNotWired)
}

// 削除の配送の処理が終わるまで待ち、時間切れならエラーを返す。世代の箱のほかの処理
// (プラグインの常駐の処理など) は待たない。
func TestWaitNoteDeletes(t *testing.T) {
	assert.NoError(t, (&Server{}).WaitNoteDeletes(t.Context()), "組み立てていなければ待つものが無い")

	group := bgwork.New()
	t.Cleanup(func() { group.Stop(context.Background()) })
	s := &Server{background: group, noteDeleteRunner: newAsyncHookRunner(group, 1)}
	other := make(chan struct{})
	require.True(t, group.Go(func(context.Context) { <-other }), "削除と関係の無い常駐の処理")
	defer close(other)
	assert.NoError(t, s.WaitNoteDeletes(t.Context()), "関係の無い処理は待たない")

	release := make(chan struct{})
	s.noteDeleteRunner.run("note delete delivery", "n1", func() { <-release })
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	assert.ErrorIs(t, s.WaitNoteDeletes(ctx), context.DeadlineExceeded)

	close(release)
	done, cancelDone := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelDone()
	require.NoError(t, s.WaitNoteDeletes(done))
	assert.Zero(t, s.BackgroundDropped())
}
