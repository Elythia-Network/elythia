package userrelation

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var (
	dbOnce sync.Once
	testDB *gorm.DB
)

func openDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbOnce.Do(func() {
		testDB = testutil.MustOpenTestDB()
		testutil.ApplyMigrations(testDB)
	})
	return testDB
}

// countingDB returns a handle on the same connections as base whose every
// statement increments the returned counter.
//
// 同じ *sql.DB を使うので search_path (このパッケージの schema) はそのまま。
// callback を base に登録すると他のテストも数えてしまうので、別の gorm.DB を作る。
func countingDB(t *testing.T, base *gorm.DB) (*gorm.DB, *atomic.Int64) {
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

func reposOn(db *gorm.DB) Repos {
	return Repos{
		Following:     repository.NewFollowingRepository(db),
		Blocking:      repository.NewBlockingRepository(db),
		Muting:        repository.NewMutingRepository(db),
		RenoteMuting:  repository.NewRenoteMutingRepository(db),
		FollowRequest: repository.NewFollowRequestRepository(db),
		Memo:          repository.NewUserMemoRepository(db),
	}
}

// seedDBRelations creates the viewer plus n targets and gives every target one
// relation chosen by its index, so a list of n users covers every field.
func seedDBRelations(t *testing.T, db *gorm.DB, prefix string, n int) (*model.User, []*model.User, []*model.UserProfile) {
	t.Helper()
	mk := func(id string) *model.User {
		u := &model.User{
			ID: id, Username: id, UsernameLower: strings.ToLower(id),
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		}
		require.NoError(t, db.Create(u).Error)
		t.Cleanup(func() { db.Exec(`DELETE FROM "user" WHERE id = ?`, id) })
		return u
	}
	viewer := mk(prefix + "v")
	users := make([]*model.User, 0, n+1)
	profiles := make([]*model.UserProfile, 0, n+1)
	notify := "normal"
	past := time.Now().Add(-time.Hour)
	msg := "hi"
	for i := 0; i < n; i++ {
		u := mk(fmt.Sprintf("%st%03d", prefix, i))
		id := fmt.Sprintf("%sr%03d", prefix, i)
		var row any
		switch i % 11 {
		case 0:
			row = &model.Following{ID: id, FollowerID: viewer.ID, FolloweeID: u.ID, Notify: &notify, WithReplies: true}
		case 1:
			row = &model.Following{ID: id, FollowerID: u.ID, FolloweeID: viewer.ID}
		case 2:
			row = &model.Blocking{ID: id, BlockerID: viewer.ID, BlockeeID: u.ID}
		case 3:
			row = &model.Blocking{ID: id, BlockerID: u.ID, BlockeeID: viewer.ID}
		case 4:
			row = &model.Muting{ID: id, MuterID: viewer.ID, MuteeID: u.ID}
		case 5:
			row = &model.Muting{ID: id, MuterID: viewer.ID, MuteeID: u.ID, ExpiresAt: &past}
		case 6:
			row = &model.RenoteMuting{ID: id, MuterID: viewer.ID, MuteeID: u.ID}
		case 7:
			row = &model.FollowRequest{ID: id, FollowerID: viewer.ID, FolloweeID: u.ID}
		case 8:
			row = &model.FollowRequest{ID: id, FollowerID: u.ID, FolloweeID: viewer.ID}
		case 9:
			row = &model.UserMemo{ID: id, UserID: viewer.ID, TargetUserID: u.ID, Memo: "memo " + u.ID}
		}
		if row != nil {
			require.NoError(t, db.Create(row).Error)
		}
		users = append(users, u)
		profiles = append(profiles, &model.UserProfile{UserID: u.ID, FollowedMessage: &msg})
	}
	// 自分自身も一覧に混ぜる (メモだけ載る)。
	require.NoError(t, db.Create(&model.UserMemo{ID: prefix + "selfmemo", UserID: viewer.ID, TargetUserID: viewer.ID, Memo: "self"}).Error)
	users = append(users, viewer)
	profiles = append(profiles, &model.UserProfile{UserID: viewer.ID})
	t.Cleanup(func() {
		for _, table := range []string{"following", "blocking", "muting", "renote_muting", "follow_request", "user_memo"} {
			db.Exec(`DELETE FROM "`+table+`" WHERE id LIKE ?`, prefix+"%")
		}
	})
	return viewer, users, profiles
}

// ApplyMany on the real repositories writes exactly what Apply writes for
// every target, with one query per relation however long the list is
// (upstream getRelations). Apply per target is ten queries per user.
func TestApplyMany_DBMatchesApplyWithConstantQueries(t *testing.T) {
	base := openDB(t)
	viewer, users, profiles := seedDBRelations(t, base, "uramd", 100)
	db, n := countingDB(t, base)
	r := reposOn(db)

	want := make([]entity.UserDetailed, len(users))
	wantFollowing := make([]bool, len(users))
	n.Store(0)
	for i, u := range users {
		wantFollowing[i] = r.Apply(&want[i], viewer.ID, u, profiles[i])
	}
	perUser := n.Load()

	got := make([]entity.UserDetailed, len(users))
	details := make([]*entity.UserDetailed, len(users))
	for i := range got {
		details[i] = &got[i]
	}
	n.Store(0)
	gotFollowing := r.ApplyMany(viewer.ID, details, users, profiles)
	batched := n.Load()

	t.Logf("relation queries for %d users: Apply per user=%d, ApplyMany=%d", len(users), perUser, batched)
	assert.Equal(t, wantFollowing, gotFollowing)
	for i, u := range users {
		wj, _ := json.Marshal(want[i])
		gj, _ := json.Marshal(got[i])
		require.JSONEq(t, string(wj), string(gj), "target %s", u.ID)
	}
	// 関係 9 種 (following の行 = isFollowing と notify・withReplies、被フォロー、
	// ブロック 2 方向、ミュート、リノートミュート、フォロー申請 2 方向、メモ) を
	// 1 回ずつ。Apply は following を Exists 2 回と FindByPair で引くので 1 人 10 回。
	assert.Equal(t, int64(9), batched)
	assert.Greater(t, perUser, int64(900))

	// 代表値 (Apply 自体が壊れて両方一致してしまう場合の保険)。
	assert.True(t, *got[0].IsFollowing)
	assert.Equal(t, "normal", *got[0].Notify)
	assert.JSONEq(t, `"hi"`, string(got[0].FollowedMessage))
	assert.True(t, *got[1].IsFollowed)
	assert.True(t, *got[2].IsBlocking)
	assert.True(t, *got[3].IsBlocked)
	assert.True(t, *got[4].IsMuted)
	assert.False(t, *got[5].IsMuted, "期限切れの mute は数えない")
	assert.True(t, *got[6].IsRenoteMuted)
	assert.True(t, *got[7].HasPendingFollowRequestFromYou)
	assert.True(t, *got[8].HasPendingFollowRequestToYou)
	assert.Equal(t, "memo "+users[9].ID, *got[9].Memo)
	last := got[len(got)-1]
	assert.Nil(t, last.IsFollowing)
	assert.Equal(t, "self", *last.Memo)
}
