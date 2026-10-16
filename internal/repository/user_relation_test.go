package repository

import (
	"context"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserRelationRepository_ReadsEachRelationInItsDirection(t *testing.T) {
	repo := NewUserRelationRepository(testDB)
	v := insertTestUser(t, "urr_v", "urrv")
	defer cleanupUser(t, v.ID)
	tg := insertTestUser(t, "urr_t", "urrt")
	defer cleanupUser(t, tg.ID)
	defer func() {
		for _, table := range []string{"following", "blocking", "muting", "renote_muting", "follow_request", "user_memo"} {
			testDB.Exec(`DELETE FROM "` + table + `" WHERE id LIKE 'urr\_%'`)
		}
	}()
	now := time.Now()
	read := func() UserRelationState {
		t.Helper()
		s, err := repo.ReadUserRelation(v.ID, tg.ID, now)
		require.NoError(t, err)
		return s
	}

	assert.Equal(t, UserRelationState{}, read())

	// 相手から自分への行だけがある状態。自分から相手への項目は立たない。
	require.NoError(t, testDB.Create(&model.Following{ID: "urr_f_in", FollowerID: tg.ID, FolloweeID: v.ID}).Error)
	require.NoError(t, testDB.Create(&model.Blocking{ID: "urr_b_in", BlockerID: tg.ID, BlockeeID: v.ID}).Error)
	require.NoError(t, testDB.Create(&model.Muting{ID: "urr_m_in", MuterID: tg.ID, MuteeID: v.ID}).Error)
	require.NoError(t, testDB.Create(&model.RenoteMuting{ID: "urr_rm_in", MuterID: tg.ID, MuteeID: v.ID}).Error)
	require.NoError(t, testDB.Create(&model.FollowRequest{ID: "urr_fr_in", FollowerID: tg.ID, FolloweeID: v.ID}).Error)
	require.NoError(t, testDB.Create(&model.UserMemo{ID: "urr_memo_in", UserID: tg.ID, TargetUserID: v.ID, Memo: "theirs"}).Error)
	assert.Equal(t, UserRelationState{IsFollowed: true, IsBlocked: true, HasPendingTo: true}, read())

	// 自分から相手への行を足す。
	notify := "normal"
	require.NoError(t, testDB.Create(&model.Following{ID: "urr_f_out", FollowerID: v.ID, FolloweeID: tg.ID, Notify: &notify, WithReplies: true}).Error)
	require.NoError(t, testDB.Create(&model.Blocking{ID: "urr_b_out", BlockerID: v.ID, BlockeeID: tg.ID}).Error)
	require.NoError(t, testDB.Create(&model.Muting{ID: "urr_m_out", MuterID: v.ID, MuteeID: tg.ID}).Error)
	require.NoError(t, testDB.Create(&model.RenoteMuting{ID: "urr_rm_out", MuterID: v.ID, MuteeID: tg.ID}).Error)
	require.NoError(t, testDB.Create(&model.FollowRequest{ID: "urr_fr_out", FollowerID: v.ID, FolloweeID: tg.ID}).Error)
	require.NoError(t, testDB.Create(&model.UserMemo{ID: "urr_memo_out", UserID: v.ID, TargetUserID: tg.ID, Memo: "mine"}).Error)
	assert.Equal(t, UserRelationState{
		IsFollowing: true, FollowNotify: &notify, FollowWithReplies: true,
		IsFollowed: true, IsBlocking: true, IsBlocked: true,
		IsMuted: true, IsRenoteMuted: true,
		HasPendingFrom: true, HasPendingTo: true,
		HasMemo: true, Memo: "mine",
	}, read())

	// notify が NULL のフォローと、空のメモ。
	require.NoError(t, testDB.Exec(`UPDATE "following" SET "notify" = NULL, "withReplies" = false WHERE id = 'urr_f_out'`).Error)
	require.NoError(t, testDB.Exec(`UPDATE "user_memo" SET "memo" = '' WHERE id = 'urr_memo_out'`).Error)
	s := read()
	assert.True(t, s.IsFollowing)
	assert.Nil(t, s.FollowNotify)
	assert.False(t, s.FollowWithReplies)
	assert.True(t, s.HasMemo)
	assert.Equal(t, "", s.Memo)
}

// A mute counts while its expiresAt is later than now, exactly like
// MutingRepository.Exists.
func TestUserRelationRepository_MuteExpiry(t *testing.T) {
	repo := NewUserRelationRepository(testDB)
	mutes := NewMutingRepository(testDB)
	v := insertTestUser(t, "urx_v", "urxv")
	defer cleanupUser(t, v.ID)
	tg := insertTestUser(t, "urx_t", "urxt")
	defer cleanupUser(t, tg.ID)
	defer testDB.Exec(`DELETE FROM "muting" WHERE id LIKE 'urx\_%'`)

	for _, tc := range []struct {
		name    string
		expires time.Duration
		want    bool
	}{
		{"expires in an hour", time.Hour, true},
		{"expires in a few seconds", 5 * time.Second, true},
		{"expired a few seconds ago", -5 * time.Second, false},
		{"expired an hour ago", -time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testDB.Exec(`DELETE FROM "muting" WHERE id LIKE 'urx\_%'`)
			at := time.Now().Add(tc.expires)
			require.NoError(t, testDB.Create(&model.Muting{ID: "urx_m", MuterID: v.ID, MuteeID: tg.ID, ExpiresAt: &at}).Error)
			s, err := repo.ReadUserRelation(v.ID, tg.ID, time.Now())
			require.NoError(t, err)
			assert.Equal(t, tc.want, s.IsMuted)
			old, err := mutes.Exists(v.ID, tg.ID)
			require.NoError(t, err)
			assert.Equal(t, old, s.IsMuted)
		})
	}

	// 判定は渡した時刻で行う。期限ちょうどは「切れた」側 (MutingRepository.Exists
	// と同じく expiresAt > now)。timestamptz はマイクロ秒なので丸めてから作る。
	testDB.Exec(`DELETE FROM "muting" WHERE id LIKE 'urx\_%'`)
	at := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	require.NoError(t, testDB.Create(&model.Muting{ID: "urx_m", MuterID: v.ID, MuteeID: tg.ID, ExpiresAt: &at}).Error)
	s, err := repo.ReadUserRelation(v.ID, tg.ID, at.Add(-time.Second))
	require.NoError(t, err)
	assert.True(t, s.IsMuted)
	s, err = repo.ReadUserRelation(v.ID, tg.ID, at.Add(time.Second))
	require.NoError(t, err)
	assert.False(t, s.IsMuted)
	s, err = repo.ReadUserRelation(v.ID, tg.ID, at.Add(-time.Microsecond))
	require.NoError(t, err)
	assert.True(t, s.IsMuted, "1 µs before expiry")
	s, err = repo.ReadUserRelation(v.ID, tg.ID, at)
	require.NoError(t, err)
	assert.False(t, s.IsMuted, "exactly at expiry")
}

func TestUserRelationRepository_UnstorableAndError(t *testing.T) {
	s, err := NewUserRelationRepository(testDB).ReadUserRelation("a\x00b", "c", time.Now())
	require.NoError(t, err)
	assert.Equal(t, UserRelationState{}, s)
	s, err = NewUserRelationRepository(testDB).ReadUserRelation("c", "a\x00b", time.Now())
	require.NoError(t, err)
	assert.Equal(t, UserRelationState{}, s)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = NewUserRelationRepository(testDB.WithContext(ctx)).ReadUserRelation("a", "b", time.Now())
	assert.Error(t, err)
}
