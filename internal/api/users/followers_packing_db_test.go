package users

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

var (
	followersDBOnce sync.Once
	followersDB     *gorm.DB
)

func openFollowersDB(t *testing.T) *gorm.DB {
	t.Helper()
	followersDBOnce.Do(func() {
		followersDB = testutil.MustOpenTestDB()
		testutil.ApplyMigrations(followersDB)
	})
	return followersDB
}

// countingFollowersDB returns a handle on the same connections as base whose
// every statement increments the returned counter.
//
// 同じ *sql.DB を使うので search_path (このパッケージの schema) はそのまま。
// callback を base に登録すると他のテストも数えてしまうので、別の gorm.DB を作る。
func countingFollowersDB(t *testing.T, base *gorm.DB) (*gorm.DB, *atomic.Int64) {
	t.Helper()
	sqlDB, err := base.DB()
	require.NoError(t, err)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	require.NoError(t, err)
	var n atomic.Int64
	count := func(*gorm.DB) { n.Add(1) }
	require.NoError(t, db.Callback().Query().After("gorm:query").Register("test:count_query", count))
	require.NoError(t, db.Callback().Row().After("gorm:row").Register("test:count_row", count))
	require.NoError(t, db.Callback().Raw().After("gorm:raw").Register("test:count_raw", count))
	return db, &n
}

// dbFollowersHandler wires a users Handler on the real repositories of db.
func dbFollowersHandler(t *testing.T, db *gorm.DB) *Handler {
	t.Helper()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	userRepo := repository.NewUserRepository(db)
	noteRepo := repository.NewNoteRepository(db)
	pinRepo := repository.NewUserNotePiningRepository(db)
	fRepo := repository.NewFollowingRepository(db)
	frRepo := repository.NewFollowRequestRepository(db)
	h := NewHandler(coreuser.NewService(userRepo, noteRepo, pinRepo, idGen),
		corefollowing.NewService(userRepo, fRepo, frRepo, idGen), noteRepo, idGen)
	h.SetUserRepo(userRepo)
	h.SetPiningRepo(pinRepo)
	h.SetFollowingRepo(fRepo)
	h.SetFollowRequestRepo(frRepo)
	h.SetBlockingRepo(repository.NewBlockingRepository(db))
	h.SetMutingRepo(repository.NewMutingRepository(db))
	h.SetRenoteMutingRepo(repository.NewRenoteMutingRepository(db))
	h.SetMemoRepo(repository.NewUserMemoRepository(db))
	h.SetPageRepo(repository.NewPageRepository(db))
	h.SetInstanceRepo(repository.NewInstanceRepository(db))
	h.SetEmojiRepo(repository.NewEmojiRepository(db))
	return h
}

// followersFixture is a target user followed by the viewer and by n other
// users, each of which has one viewer relation chosen by its index.
type followersFixture struct {
	target, viewer *model.User
	followers      []*model.User
}

func seedFollowersDB(t *testing.T, db *gorm.DB, prefix string, n int) followersFixture {
	t.Helper()
	mk := func(id string, vis model.FollowingVisibility) *model.User {
		u := &model.User{ID: id, Username: id, UsernameLower: strings.ToLower(id),
			FollowersCount: 7, FollowingCount: 3, AvatarDecorations: datatypes.JSON([]byte("[]"))}
		require.NoError(t, db.Create(u).Error)
		note := "note of " + id
		require.NoError(t, db.Create(&model.UserProfile{UserID: id, FollowersVisibility: vis,
			FollowingVisibility: model.FollowingVisibilityPublic, ModerationNote: &note}).Error)
		t.Cleanup(func() {
			db.Exec(`DELETE FROM "user_profile" WHERE "userId" = ?`, id)
			db.Exec(`DELETE FROM "user" WHERE id = ?`, id)
		})
		return u
	}
	fx := followersFixture{target: mk(prefix+"t", model.FollowingVisibilityPublic), viewer: mk(prefix+"v", model.FollowingVisibilityPublic)}
	t.Cleanup(func() {
		for _, table := range []string{"following", "blocking", "muting", "user_memo"} {
			db.Exec(`DELETE FROM "`+table+`" WHERE id LIKE ?`, prefix+"%")
		}
	})
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	// following の行の id は aidx で振る (createdAt を復元するため)。
	follow := func(from, to *model.User) {
		fid := idGen.Generate(time.Now())
		require.NoError(t, db.Create(&model.Following{ID: fid, FollowerID: from.ID, FolloweeID: to.ID}).Error)
		t.Cleanup(func() { db.Exec(`DELETE FROM "following" WHERE id = ?`, fid) })
	}
	// 閲覧者自身もフォロワーに混ぜる (本家は MeDetailed で返す)。
	follow(fx.viewer, fx.target)
	notify := "normal"
	for i := 0; i < n; i++ {
		vis := model.FollowingVisibilityPublic
		if i%5 == 4 {
			vis = model.FollowingVisibilityFollowers
		}
		u := mk(fmt.Sprintf("%sf%03d", prefix, i), vis)
		fx.followers = append(fx.followers, u)
		follow(u, fx.target)
		rid := fmt.Sprintf("%sr%03d", prefix, i)
		switch i % 5 {
		case 0:
			require.NoError(t, db.Create(&model.Following{ID: rid, FollowerID: fx.viewer.ID, FolloweeID: u.ID, Notify: &notify, WithReplies: true}).Error)
		case 1:
			require.NoError(t, db.Create(&model.UserMemo{ID: rid, UserID: fx.viewer.ID, TargetUserID: u.ID, Memo: "memo " + u.ID}).Error)
		case 2:
			require.NoError(t, db.Create(&model.Blocking{ID: rid, BlockerID: fx.viewer.ID, BlockeeID: u.ID}).Error)
		case 3:
			require.NoError(t, db.Create(&model.Muting{ID: rid, MuterID: fx.viewer.ID, MuteeID: u.ID}).Error)
		}
	}
	return fx
}

// fixedModerator reports the configured users as moderators.
type fixedModerator map[string]bool

func (m fixedModerator) IsModerator(userID string) bool     { return m[userID] }
func (m fixedModerator) IsAdministrator(userID string) bool { return false }

// users/followers on the real repositories packs each follower as upstream
// FollowingEntityService.packMany({populateFollower: true}) does, with a
// number of queries that does not grow with the page size.
func TestFollowers_DBPacksLikeUpstreamWithConstantQueries(t *testing.T) {
	base := openFollowersDB(t)
	fx := seedFollowersDB(t, base, "ufpd", 99)
	db, n := countingFollowersDB(t, base)
	h := dbFollowersHandler(t, db)

	body := fmt.Sprintf(`{"userId":%q,"limit":100}`, fx.target.ID)
	n.Store(0)
	rec := postStub(h.Followers, body, fx.viewer)
	queries := n.Load()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	t.Logf("users/followers queries for 100 rows: %d", queries)

	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 100)
	byFollower := map[string]map[string]any{}
	for _, r := range rows {
		assert.ElementsMatch(t, []string{"id", "createdAt", "followeeId", "followerId", "follower"}, keys(r))
		assert.Equal(t, fx.target.ID, r["followeeId"])
		assert.Regexp(t, `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`, r["createdAt"])
		f, _ := r["follower"].(map[string]any)
		require.NotNil(t, f, "follower is populated")
		assert.Equal(t, r["followerId"], f["id"])
		byFollower[f["id"].(string)] = f
	}

	// 閲覧者自身は MeDetailed (本家 pack は isMe なら MeDetailed の項目を足す)。
	self := byFollower[fx.viewer.ID]
	require.NotNil(t, self)
	assert.Contains(t, self, "avatarId")
	assert.Contains(t, self, "isModerator")
	assert.NotContains(t, self, "isFollowing", "自分には関係を付けない")
	assert.NotContains(t, self, "moderationNote", "モデレーターでなければ出さない")

	other := func(i int) map[string]any {
		f := byFollower[fx.followers[i].ID]
		require.NotNil(t, f)
		assert.NotContains(t, f, "avatarId", "他人は MeDetailed にしない")
		assert.NotContains(t, f, "moderationNote", "モデレーターでなければ出さない")
		assert.NotContains(t, f, "twoFactorEnabled")
		return f
	}
	f0 := other(0)
	assert.Equal(t, true, f0["isFollowing"])
	assert.Equal(t, "normal", f0["notify"])
	assert.Equal(t, true, f0["withReplies"])
	assert.Nil(t, f0["memo"])
	f1 := other(1)
	assert.Equal(t, "memo "+fx.followers[1].ID, f1["memo"])
	assert.Equal(t, "none", f1["notify"])
	assert.Equal(t, false, f1["withReplies"])
	assert.Equal(t, true, other(2)["isBlocking"])
	assert.Equal(t, true, other(3)["isMuted"])
	// followersVisibility=followers の相手は、フォローしていない閲覧者には伏せる。
	assert.InDelta(t, 0, other(4)["followersCount"], 0)
	assert.InDelta(t, 7, other(3)["followersCount"], 0)

	// 関係・ピン留め・ページ・instance・絵文字をまとめて引くので、行数に比例しない。
	// 内訳は handler の前段 4 回 (利用者・profile の解決、公開範囲の確認、行の取得)、
	// 一覧の利用者と profile 2 回、関係 9 種、ピン留め 1 回。
	n.Store(0)
	rec = postStub(h.Followers, fmt.Sprintf(`{"userId":%q,"limit":10}`, fx.target.ID), fx.viewer)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, n.Load(), queries, "10 行でも 100 行でも問い合わせの数は同じ")
	assert.Equal(t, int64(16), queries)
}

// A moderator viewer gets moderationNote and the 2FA fields of every
// follower, and the counts hidden from ordinary viewers.
func TestFollowers_DBModeratorViewer(t *testing.T) {
	base := openFollowersDB(t)
	fx := seedFollowersDB(t, base, "ufpm", 5)
	h := dbFollowersHandler(t, base)
	h.SetModeratorChecker(fixedModerator{fx.viewer.ID: true})

	rec := postStub(h.Followers, fmt.Sprintf(`{"userId":%q}`, fx.target.ID), fx.viewer)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 6)
	for _, r := range rows {
		f := r["follower"].(map[string]any)
		assert.Equal(t, "note of "+f["id"].(string), f["moderationNote"], f["id"])
		assert.Contains(t, f, "twoFactorEnabled")
		assert.Contains(t, f, "securityKeys")
		assert.InDelta(t, 7, f["followersCount"], 0, f["id"])
	}
}

// users/following populates the followee side instead, and the viewer's own
// entry (the target follows the viewer) is MeDetailed.
func TestFollowing_DBPopulatesFollowee(t *testing.T) {
	base := openFollowersDB(t)
	fx := seedFollowersDB(t, base, "ufpf", 3)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	for _, to := range []*model.User{fx.viewer, fx.followers[0]} {
		fid := idGen.Generate(time.Now())
		require.NoError(t, base.Create(&model.Following{ID: fid, FollowerID: fx.target.ID, FolloweeID: to.ID}).Error)
		t.Cleanup(func() { base.Exec(`DELETE FROM "following" WHERE id = ?`, fid) })
	}
	h := dbFollowersHandler(t, base)

	rec := postStub(h.Following, fmt.Sprintf(`{"userId":%q}`, fx.target.ID), fx.viewer)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 2)
	byFollowee := map[string]map[string]any{}
	for _, r := range rows {
		assert.ElementsMatch(t, []string{"id", "createdAt", "followeeId", "followerId", "followee"}, keys(r))
		assert.Equal(t, fx.target.ID, r["followerId"])
		f := r["followee"].(map[string]any)
		assert.Equal(t, r["followeeId"], f["id"])
		byFollowee[f["id"].(string)] = f
	}
	assert.Contains(t, byFollowee[fx.viewer.ID], "avatarId", "閲覧者自身は MeDetailed")
	f0 := byFollowee[fx.followers[0].ID]
	assert.NotContains(t, f0, "avatarId")
	assert.Equal(t, "normal", f0["notify"])
	assert.Equal(t, true, f0["isFollowing"])
}

// An anonymous viewer gets no relation block and no MeDetailed entry, like
// upstream packMany(users, null).
func TestFollowers_DBAnonymousViewer(t *testing.T) {
	base := openFollowersDB(t)
	fx := seedFollowersDB(t, base, "ufpa", 2)
	h := dbFollowersHandler(t, base)

	rec := postStub(h.Followers, fmt.Sprintf(`{"userId":%q}`, fx.target.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 3)
	for _, r := range rows {
		f := r["follower"].(map[string]any)
		for _, k := range []string{"isFollowing", "isFollowed", "notify", "withReplies", "isBlocking", "avatarId", "moderationNote", "twoFactorEnabled"} {
			assert.NotContains(t, f, k, "%s for %s", k, f["id"])
		}
		assert.Nil(t, f["memo"])
	}
}

// The followersVisibility / followingVisibility gate is applied before any
// packing: private lists are refused to others and served to the owner and
// to moderators.
func TestFollowers_DBVisibilityGate(t *testing.T) {
	base := openFollowersDB(t)
	fx := seedFollowersDB(t, base, "ufpg", 1)
	require.NoError(t, base.Model(&model.UserProfile{}).Where(`"userId" = ?`, fx.target.ID).
		Updates(map[string]any{"followersVisibility": "private", "followingVisibility": "followers"}).Error)
	h := dbFollowersHandler(t, base)
	h.SetModeratorChecker(fixedModerator{"ufpgmod": true})
	body := fmt.Sprintf(`{"userId":%q}`, fx.target.ID)

	rec := postStub(h.Followers, body, fx.viewer)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "3c6a84db-d619-26af-ca14-06232a21df8a")
	assert.Equal(t, http.StatusBadRequest, postStub(h.Followers, body, nil).Code)
	assert.Equal(t, http.StatusOK, postStub(h.Followers, body, fx.target).Code, "本人は見られる")
	assert.Equal(t, http.StatusOK, postStub(h.Followers, body, &model.User{ID: "ufpgmod"}).Code, "モデレーターは見られる")

	// following は followers 限定。閲覧者は target をフォローしているので見られる。
	assert.Equal(t, http.StatusOK, postStub(h.Following, body, fx.viewer).Code)
	rec = postStub(h.Following, body, &model.User{ID: "ufpgstranger"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "f6cdb0df-c19f-ec5c-7dbb-0ba84a1f92ba")
}

// The remote stats override (mk-go only, #1146) re-applies the count gate on
// the overridden values, so a moderator sees the origin's count of a remote
// user whose list is private even when the local count is 0.
func TestFollowers_RemoteStatsOverrideRegatesCounts(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	host := "remote.example"
	repo.Users["remote-zed"] = &model.User{ID: "remote-zed", Username: "zed", UsernameLower: "zed", Host: &host,
		AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Profiles["remote-zed"] = &model.UserProfile{UserID: "remote-zed",
		FollowersVisibility: model.FollowingVisibilityPrivate, FollowingVisibility: model.FollowingVisibilityPrivate}
	_, err := h.followingService.Follow("remote-zed", "user1", corefollowing.FollowOptions{})
	require.NoError(t, err)
	h.SetRemoteStatsFetcher(&fakeRemoteStatsFetcher{stats: map[string]*RemoteUserStatsView{
		"remote.example|zed": {NotesCount: 4, FollowersCount: 40, FollowingCount: 30},
	}})

	pick := func(viewer *model.User) map[string]any {
		rec := postStub(h.Followers, `{"userId":"user1"}`, viewer)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var rows []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
		require.Len(t, rows, 1)
		return rows[0]["follower"].(map[string]any)
	}
	h.SetModeratorChecker(fixedModerator{"mod": true})
	mod := pick(&model.User{ID: "mod"})
	assert.InDelta(t, 4, mod["notesCount"], 0)
	assert.InDelta(t, 40, mod["followersCount"], 0)
	assert.InDelta(t, 30, mod["followingCount"], 0)
	other := pick(&model.User{ID: "someone"})
	assert.InDelta(t, 4, other["notesCount"], 0)
	assert.InDelta(t, 0, other["followersCount"], 0, "非公開のカウントは差し替えても伏せる")
	assert.InDelta(t, 0, other["followingCount"], 0)
}

// A remote user whose lists are followers-only shows the origin's counts to
// a viewer who follows them and 0 to one who does not, after the override.
func TestFollowers_RemoteStatsOverrideRegatesFollowersOnly(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	h.SetBlockingRepo(testutil.NewMockBlockingRepository())
	h.SetMutingRepo(testutil.NewMockMutingRepository())
	h.SetRenoteMutingRepo(testutil.NewMockRenoteMutingRepository())
	host := "remote.example"
	repo.Users["remote-yui"] = &model.User{ID: "remote-yui", Username: "yui", UsernameLower: "yui", Host: &host,
		AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Profiles["remote-yui"] = &model.UserProfile{UserID: "remote-yui",
		FollowersVisibility: model.FollowingVisibilityFollowers, FollowingVisibility: model.FollowingVisibilityFollowers}
	for _, u := range []string{"fan", "stranger"} {
		repo.Users[u] = &model.User{ID: u, Username: u, UsernameLower: u, AvatarDecorations: datatypes.JSON([]byte("[]"))}
	}
	_, err := h.followingService.Follow("remote-yui", "user1", corefollowing.FollowOptions{})
	require.NoError(t, err)
	_, err = h.followingService.Follow("fan", "remote-yui", corefollowing.FollowOptions{})
	require.NoError(t, err)
	h.SetRemoteStatsFetcher(&fakeRemoteStatsFetcher{stats: map[string]*RemoteUserStatsView{
		"remote.example|yui": {NotesCount: 4, FollowersCount: 40, FollowingCount: 30},
	}})

	pick := func(viewer string) map[string]any {
		rec := postStub(h.Followers, `{"userId":"user1"}`, repo.Users[viewer])
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var rows []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
		require.Len(t, rows, 1)
		return rows[0]["follower"].(map[string]any)
	}
	fan := pick("fan")
	assert.Equal(t, true, fan["isFollowing"])
	assert.InDelta(t, 40, fan["followersCount"], 0, "フォローしている閲覧者には origin の値")
	assert.InDelta(t, 30, fan["followingCount"], 0)
	stranger := pick("stranger")
	assert.Equal(t, false, stranger["isFollowing"])
	assert.InDelta(t, 0, stranger["followersCount"], 0, "フォローしていない閲覧者には伏せる")
	assert.InDelta(t, 0, stranger["followingCount"], 0)
}

// A failure to load the related users is a 500, not a list of rows without
// follower / followee.
func TestFollowers_RelatedUsersLoadFailureIs500(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(h *Handler) echo.HandlerFunc
	}{
		{"followers", func(h *Handler) echo.HandlerFunc { return h.Followers }},
		{"following", func(h *Handler) echo.HandlerFunc { return h.Following }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repo := newTestHandler(t)
			addTestUser(repo)
			repo.Users["alice"] = &model.User{ID: "alice", Username: "alice", UsernameLower: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
			_, err := h.followingService.Follow("alice", "user1", corefollowing.FollowOptions{})
			require.NoError(t, err)
			_, err = h.followingService.Follow("user1", "alice", corefollowing.FollowOptions{})
			require.NoError(t, err)
			repo.FindManyByIDsErr = errors.New("db down")

			rec := postStub(tc.call(h), `{"userId":"user1"}`, nil)
			assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		})
	}
}

// A following row whose related user does not exist is dropped, like
// upstream's innerJoinAndSelect, instead of being returned without the user.
func TestFollowers_DropsRowsWithoutRelatedUser(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	repo.Users["alice"] = &model.User{ID: "alice", Username: "alice", UsernameLower: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	fRepo := h.followingRepo.(*testutil.MockFollowingRepository)
	require.NoError(t, fRepo.Create(&model.Following{ID: "f1", FollowerID: "alice", FolloweeID: "user1"}))
	require.NoError(t, fRepo.Create(&model.Following{ID: "f2", FollowerID: "ghost", FolloweeID: "user1"}))
	require.NoError(t, fRepo.Create(&model.Following{ID: "f3", FollowerID: "user1", FolloweeID: "ghost"}))

	for _, tc := range []struct {
		name  string
		call  echo.HandlerFunc
		field string
	}{
		{"followers", h.Followers, "follower"},
		{"following", h.Following, "followee"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postStub(tc.call, `{"userId":"user1"}`, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var rows []map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
			for _, r := range rows {
				assert.NotNil(t, r[tc.field], "row %v", r["id"])
			}
			if tc.name == "followers" {
				require.Len(t, rows, 1)
				assert.Equal(t, "f1", rows[0]["id"])
			} else {
				assert.Empty(t, rows)
			}
		})
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
