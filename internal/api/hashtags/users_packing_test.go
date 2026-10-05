package hashtags_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/api/hashtags"
	"github.com/elythia-network/elythia/internal/api/userrelation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type moderatorSet map[string]bool

func (m moderatorSet) IsModerator(userID string) bool { return m[userID] }

func seedTagProfile(t *testing.T, userID string, vis model.FollowingVisibility, note string) {
	t.Helper()
	require.NoError(t, testDB.Create(&model.UserProfile{
		UserID: userID, FollowersVisibility: vis, FollowingVisibility: vis, ModerationNote: &note,
	}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "user_profile" WHERE "userId" = ?`, userID) })
}

// 本家 hashtags/users は packMany(users, me, {schema: 'UserDetailed'}) なので、
// モデレーターの閲覧者には moderationNote と 2FA の項目が付き、非公開のカウントも
// 見える (#3330)。それ以外の閲覧者には付かず、カウントは 0。
func TestUsers_ModeratorViewerGetsModeratorFields(t *testing.T) {
	now := time.Now()
	seedTagUser(t, "u_htu_mod1", "htumod1", []string{"modtag"}, nil, false, 42, now)
	seedTagProfile(t, "u_htu_mod1", model.FollowingVisibilityPrivate, "watch this")
	h := newHandler()
	h.SetModeratorChecker(moderatorSet{"mod1": true})

	decode := func(viewerID string) map[string]any {
		t.Helper()
		var rec = doPost(h.Users, `{"tag":"modtag","sort":"+follower"}`)
		if viewerID != "" {
			rec = doPostAs(h.Users, `{"tag":"modtag","sort":"+follower"}`, viewerID)
		}
		require.Equal(t, http.StatusOK, rec.Code)
		var rows []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
		require.Len(t, rows, 1)
		return rows[0]
	}

	mod := decode("mod1")
	assert.Equal(t, "watch this", mod["moderationNote"])
	assert.Equal(t, false, mod["twoFactorEnabled"])
	assert.Contains(t, mod, "securityKeys")
	assert.Equal(t, float64(42), mod["followersCount"], "モデレーターには非公開のカウントも見せる")

	for _, viewer := range []string{"other1", ""} {
		row := decode(viewer)
		assert.NotContains(t, row, "moderationNote", "viewer=%q", viewer)
		assert.NotContains(t, row, "twoFactorEnabled", "viewer=%q", viewer)
		assert.Equal(t, float64(0), row["followersCount"], "viewer=%q", viewer)
	}
}

func TestHandler_HasModeratorChecker(t *testing.T) {
	h := newHandler()
	assert.False(t, h.HasModeratorChecker())
	h.SetModeratorChecker(moderatorSet{})
	assert.True(t, h.HasModeratorChecker())
}

// countingDB returns a handle on testDB's connections that counts every
// statement. callback を testDB に登録すると他のテストも数えるので別の gorm.DB を作る。
func countingDB(t *testing.T) (*gorm.DB, *atomic.Int64) {
	t.Helper()
	sqlDB, err := testDB.DB()
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

// hashtags/users の問い合わせの数は利用者の数に比例しない (#3330)。本家
// packMany は getRelations で閲覧者の関係を関係ごとに 1 回で引く。以前は
// 利用者 1 人ごとに 10 回引いていた。
func TestUsers_QueriesDoNotScaleWithUsers(t *testing.T) {
	now := time.Now()
	seedTagUser(t, "u_htq_viewer", "htqviewer", nil, nil, false, 0, now)
	const total = 100
	for i := 0; i < total; i++ {
		uid := fmt.Sprintf("u_htq_%03d", i)
		seedTagUser(t, uid, fmt.Sprintf("htq%03d", i), []string{"querytag"}, nil, false, total-i, now)
		seedTagProfile(t, uid, model.FollowingVisibilityPublic, "")
		if i%3 == 0 {
			fid := fmt.Sprintf("f_htq_%03d", i)
			require.NoError(t, testDB.Create(&model.Following{ID: fid, FollowerID: "u_htq_viewer", FolloweeID: uid}).Error)
			t.Cleanup(func() { testDB.Exec(`DELETE FROM "following" WHERE id = ?`, fid) })
		}
	}

	db, n := countingDB(t)
	h := hashtags.NewHandler(db)
	gen, _ := id.NewGenerator("aidx")
	h.SetIDGen(gen)
	h.SetRelationRepos(userrelation.Repos{
		Following:     repository.NewFollowingRepository(db),
		Blocking:      repository.NewBlockingRepository(db),
		Muting:        repository.NewMutingRepository(db),
		RenoteMuting:  repository.NewRenoteMutingRepository(db),
		FollowRequest: repository.NewFollowRequestRepository(db),
		Memo:          repository.NewUserMemoRepository(db),
	})

	queries := func(limit int) int64 {
		t.Helper()
		n.Store(0)
		rec := doPostAs(h.Users, fmt.Sprintf(`{"tag":"querytag","sort":"+follower","limit":%d}`, limit), "u_htq_viewer")
		require.Equal(t, http.StatusOK, rec.Code)
		var rows []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
		require.Len(t, rows, limit)
		following := 0
		for _, r := range rows {
			if r["isFollowing"] == true {
				following++
			}
		}
		require.Positive(t, following)
		return n.Load()
	}
	q10, q100 := queries(10), queries(total)
	t.Logf("hashtags/users queries: limit=10 -> %d, limit=%d -> %d", q10, total, q100)
	assert.Equal(t, q10, q100, "利用者の数に比例して問い合わせが増えない")
}
