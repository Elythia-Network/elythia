package federation_test

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

	"github.com/elythia-network/elythia/internal/activitypub"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// actorDocWithPublished returns actorDoc with a raw `published` literal.
func actorDocWithPublished(t *testing.T, uri, username, publishedLiteral string) string {
	t.Helper()
	doc := actorDoc(uri, username)
	out := strings.TrimSuffix(doc, "}") + `,"published":` + publishedLiteral + `}`
	// 置換が空振りすると「published の無い actor」で検査が空虚に通るので落とす。
	require.Contains(t, out, `"published":`+publishedLiteral)
	return out
}

func newAccountCreatedAtResolver(t *testing.T, db *gorm.DB, f corefederation.HTTPFetcher) *corefederation.Resolver {
	t.Helper()
	idGen, _ := id.NewGenerator("aidx")
	return corefederation.NewResolver(repository.NewUserRepository(db), repository.NewNoteRepository(db),
		activitypub.NewURLBuilder("https://local.example"), f, idGen)
}

// cleanupActor removes what resolving uri stored.
func cleanupActor(t *testing.T, db *gorm.DB, uri string) {
	t.Helper()
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "user_profile" WHERE "userId" IN (SELECT "id" FROM "user" WHERE "uri" = ?)`, uri)
		db.Exec(`DELETE FROM "user_publickey" WHERE "userId" IN (SELECT "id" FROM "user" WHERE "uri" = ?)`, uri)
		db.Exec(`DELETE FROM "user" WHERE "uri" = ?`, uri)
	})
}

func storedAccountCreatedAt(t *testing.T, db *gorm.DB, uri string) *time.Time {
	t.Helper()
	var row model.User
	require.NoError(t, db.Where(`"uri" = ?`, uri).First(&row).Error)
	return row.AccountCreatedAt
}

// Mastodon の形の `published` を持つ actor を取得すると、作成日時が列に入る。
// 無ければ NULL のまま、更新のたびに最新の値で上書きし、読めない値は捨てる
// (#3465)。
func TestResolveActor_DB_StoresPublishedAsAccountCreatedAt(t *testing.T) {
	db := openResyncDB(t)
	const uri = "https://acct-created.example/users/ac_alice"
	cleanupActor(t, db, uri)
	f := newRouteFetcher()
	f.bodies[uri] = actorDocWithPublished(t, uri, "ac_alice", `"2017-04-08T00:00:00Z"`)
	r := newAccountCreatedAtResolver(t, db, f)

	u, err := r.ResolveActor(uri)
	require.NoError(t, err)
	want := time.Date(2017, 4, 8, 0, 0, 0, 0, time.UTC)
	require.NotNil(t, u.AccountCreatedAt)
	assert.True(t, want.Equal(*u.AccountCreatedAt))
	stored := storedAccountCreatedAt(t, db, uri)
	require.NotNil(t, stored, "作成時に列へ保存すること")
	assert.True(t, want.Equal(*stored))

	// 更新: 新しい値で上書きする (日付だけの形も読む)。
	f.bodies[uri] = actorDocWithPublished(t, uri, "ac_alice", `"2016-01-02"`)
	refreshed, err := r.ForceResolveActor(uri)
	require.NoError(t, err)
	// 返す行にも反映する (フォローの処理はこの行を見て取りに行くか決める)。
	require.NotNil(t, refreshed.AccountCreatedAt)
	assert.True(t, time.Date(2016, 1, 2, 0, 0, 0, 0, time.UTC).Equal(*refreshed.AccountCreatedAt))
	stored = storedAccountCreatedAt(t, db, uri)
	require.NotNil(t, stored)
	assert.True(t, time.Date(2016, 1, 2, 0, 0, 0, 0, time.UTC).Equal(*stored), "更新で上書きすること")

	// 更新: 送られてこない・読めないときは保存済みの値を残す。
	for _, body := range []string{
		actorDoc(uri, "ac_alice"),
		actorDocWithPublished(t, uri, "ac_alice", `"not a date"`),
		actorDocWithPublished(t, uri, "ac_alice", `"2999-01-01T00:00:00Z"`),
	} {
		f.bodies[uri] = body
		_, err = r.ForceResolveActor(uri)
		require.NoError(t, err)
		stored = storedAccountCreatedAt(t, db, uri)
		require.NotNil(t, stored, "published が無い・読めない更新で消さないこと")
		assert.True(t, time.Date(2016, 1, 2, 0, 0, 0, 0, time.UTC).Equal(*stored))
	}
}

func TestResolveActor_DB_LeavesAccountCreatedAtNullWithoutUsablePublished(t *testing.T) {
	db := openResyncDB(t)
	cases := map[string]string{
		"absent":     "",
		"unparsable": `"someday"`,
		"future":     `"2999-01-01T00:00:00Z"`,
		"epoch zero": `0`,
		"not string": `{"foo":1}`,
	}
	i := 0
	for name, literal := range cases {
		i++
		t.Run(name, func(t *testing.T) {
			uri := "https://acct-created.example/users/ac_null" + string(rune('a'+i))
			username := "ac_null" + string(rune('a'+i))
			cleanupActor(t, db, uri)
			f := newRouteFetcher()
			if literal == "" {
				f.bodies[uri] = actorDoc(uri, username)
			} else {
				f.bodies[uri] = actorDocWithPublished(t, uri, username, literal)
			}
			u, err := newAccountCreatedAtResolver(t, db, f).ResolveActor(uri)
			require.NoError(t, err, "published が読めなくても actor は取り込むこと")
			assert.Nil(t, u.AccountCreatedAt)
			assert.Nil(t, storedAccountCreatedAt(t, db, uri))
		})
	}
}

// insertInstance stores an instance row with the given software name.
func insertInstance(t *testing.T, db *gorm.DB, instanceID, host, software string) {
	t.Helper()
	inst := &model.Instance{ID: instanceID, Host: host, FirstRetrievedAt: time.Now(), SuspensionState: model.SuspensionStateNone}
	if software != "" {
		inst.SoftwareName = &software
	}
	require.NoError(t, db.Create(inst).Error)
	t.Cleanup(func() { db.Exec(`DELETE FROM "instance" WHERE "id" = ?`, instanceID) })
}

func newUsersShowServer(t *testing.T, status int, body string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Path != "/api/users/show" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// `published` を送らない Misskey 系の人は、`/api/users/show` の createdAt を
// 実 DB の列に入れ、一度取れたら二度と取りに行かない (#3465)。
func TestAccountCreatedAtFiller_DB_MisskeyFallback(t *testing.T) {
	db := openResyncDB(t)
	const host = "mk-fallback.example"
	insertInstance(t, db, "acInstMk000000000001", host, "Misskey")
	insertRemoteUser(t, db, "acMkUser000000000001", "mk_alice", host, "https://"+host+"/users/acMkUser", nil)
	srv, hits := newUsersShowServer(t, http.StatusOK, `{"id":"x","username":"mk_alice","host":null,"createdAt":"2021-03-04T05:06:07.890Z"}`)

	userRepo := repository.NewUserRepository(db)
	filler := corefederation.NewAccountCreatedAtFiller(corefederation.NewRemoteStatsFetcherRedirectingTo(srv.URL),
		repository.NewInstanceRepository(db), userRepo)

	u, err := userRepo.FindByID("acMkUser000000000001")
	require.NoError(t, err)
	require.Nil(t, u.AccountCreatedAt)
	filler.Fill(context.Background(), u)

	want := time.Date(2021, 3, 4, 5, 6, 7, 890000000, time.UTC)
	require.NotNil(t, u.AccountCreatedAt)
	assert.True(t, want.Equal(*u.AccountCreatedAt))
	var row model.User
	require.NoError(t, db.Where(`"id" = ?`, "acMkUser000000000001").First(&row).Error)
	require.NotNil(t, row.AccountCreatedAt, "users/show の createdAt を列に保存すること")
	assert.True(t, want.Equal(*row.AccountCreatedAt))
	assert.Equal(t, int32(1), atomic.LoadInt32(hits))

	// 保存済みなら (キャッシュとは無関係に) 二度と取りに行かない。DB から
	// 読み直した行で確かめる。
	reloaded, err := userRepo.FindByID("acMkUser000000000001")
	require.NoError(t, err)
	fresh := corefederation.NewAccountCreatedAtFiller(corefederation.NewRemoteStatsFetcherRedirectingTo(srv.URL),
		repository.NewInstanceRepository(db), userRepo)
	fresh.Fill(context.Background(), reloaded)
	assert.Equal(t, int32(1), atomic.LoadInt32(hits), "列が埋まっていれば取りに行かない")
}

func TestAccountCreatedAtFiller_DB_SkipsNonMisskeyAndKeepsNullOnFailure(t *testing.T) {
	db := openResyncDB(t)
	userRepo := repository.NewUserRepository(db)

	t.Run("non-Misskey instance", func(t *testing.T) {
		const host = "masto-fallback.example"
		insertInstance(t, db, "acInstMasto000000001", host, "mastodon")
		insertRemoteUser(t, db, "acMastoUser000000001", "masto_alice", host, "https://"+host+"/users/masto_alice", nil)
		srv, hits := newUsersShowServer(t, http.StatusOK, `{"username":"masto_alice","createdAt":"2021-03-04T05:06:07.890Z"}`)
		filler := corefederation.NewAccountCreatedAtFiller(corefederation.NewRemoteStatsFetcherRedirectingTo(srv.URL),
			repository.NewInstanceRepository(db), userRepo)
		u, err := userRepo.FindByID("acMastoUser000000001")
		require.NoError(t, err)
		filler.Fill(context.Background(), u)
		assert.Equal(t, int32(0), atomic.LoadInt32(hits), "Misskey 系でない相手の users/show は叩かない")
		assert.Nil(t, storedAccountCreatedAt(t, db, "https://"+host+"/users/masto_alice"))
	})

	t.Run("users/show fails", func(t *testing.T) {
		const host = "mk-broken.example"
		insertInstance(t, db, "acInstBroken00000001", host, "sharkey")
		insertRemoteUser(t, db, "acBrokenUser00000001", "broken_alice", host, "https://"+host+"/users/broken_alice", nil)
		srv, hits := newUsersShowServer(t, http.StatusInternalServerError, `{}`)
		filler := corefederation.NewAccountCreatedAtFiller(corefederation.NewRemoteStatsFetcherRedirectingTo(srv.URL),
			repository.NewInstanceRepository(db), userRepo)
		u, err := userRepo.FindByID("acBrokenUser00000001")
		require.NoError(t, err)
		filler.Fill(context.Background(), u)
		assert.Equal(t, int32(1), atomic.LoadInt32(hits))
		assert.Nil(t, u.AccountCreatedAt)
		assert.Nil(t, storedAccountCreatedAt(t, db, "https://"+host+"/users/broken_alice"))
	})
}

// フォローを受け取ったとき、フォローの処理より前に作成日時を埋めに行き、
// 失敗してもフォローは成立する (#3465)。
func TestProcess_FollowFillsAccountCreatedAtBestEffort(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *time.Time
	}{
		{"filled", func() *time.Time { v := time.Date(2020, 2, 3, 0, 0, 0, 0, time.UTC); return &v }()},
		{"lookup failed", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, followingRepo, acceptor := newProcessorForInboundFollowBlock(t)
			var calls []string
			var followingsAtCall int
			p.SetAccountCreatedAtFiller(func(_ context.Context, u *model.User) {
				calls = append(calls, u.ID)
				followingsAtCall = len(followingRepo.Followings)
				u.AccountCreatedAt = tc.result
			})

			require.NoError(t, p.Process(inboundFollowBody), "作成日時の取得に失敗してもフォローは失敗させない")
			assert.Equal(t, []string{"alice1"}, calls, "フォローしてきたリモートの人について 1 回だけ呼ぶ")
			assert.Zero(t, followingsAtCall, "フォローを作るより前に呼ぶ (#3466 の判定がこれを使う)")
			assert.Len(t, followingRepo.Followings, 1, "フォローは成立する")
			assert.Len(t, acceptor.calls, 1)
		})
	}
}

// フォローの処理が読んだ行は古いことがある。その間に refresh が actor の
// `published` を書いていたら、users/show の値で上書きしない (published を
// 優先する、#3465)。
func TestAccountCreatedAtFiller_DB_DoesNotOverwriteConcurrentPublished(t *testing.T) {
	db := openResyncDB(t)
	const host = "mk-race.example"
	insertInstance(t, db, "acInstRace0000000001", host, "misskey")
	insertRemoteUser(t, db, "acRaceUser0000000001", "race_alice", host, "https://"+host+"/users/race_alice", nil)
	srv, hits := newUsersShowServer(t, http.StatusOK, `{"username":"race_alice","createdAt":"2021-03-04T05:06:07.890Z"}`)
	userRepo := repository.NewUserRepository(db)

	stale, err := userRepo.FindByID("acRaceUser0000000001")
	require.NoError(t, err)
	require.Nil(t, stale.AccountCreatedAt)
	// 読んだ後に、refresh が published を書いた。
	published := time.Date(2017, 4, 8, 0, 0, 0, 0, time.UTC)
	require.NoError(t, userRepo.UpdateUser("acRaceUser0000000001", map[string]any{"accountCreatedAt": &published}))

	corefederation.NewAccountCreatedAtFiller(corefederation.NewRemoteStatsFetcherRedirectingTo(srv.URL),
		repository.NewInstanceRepository(db), userRepo).Fill(context.Background(), stale)

	assert.Equal(t, int32(1), atomic.LoadInt32(hits))
	got := storedAccountCreatedAt(t, db, "https://"+host+"/users/race_alice")
	require.NotNil(t, got)
	assert.True(t, published.Equal(*got), "refresh が書いた published を users/show の値で上書きしている")
}
