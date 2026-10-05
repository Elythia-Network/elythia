package repository

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelFollowingRepository_LifeCycle(t *testing.T) {
	chRepo := NewChannelRepository(testDB)
	repo := NewChannelFollowingRepository(testDB)
	user := insertTestUser(t, "u_cf_1", "cfuser1")
	defer cleanupUser(t, user.ID)
	uid := user.ID
	c := newTestChannel("ch_cf_1", "follow target", &uid)
	require.NoError(t, chRepo.Create(c))
	defer cleanupChannel(t, c.ID)

	f := &model.ChannelFollowing{ID: "cf_pair_1", FollowerID: user.ID, FolloweeID: c.ID}
	require.NoError(t, repo.Create(f))

	got, err := repo.FindByPair(user.ID, c.ID)
	require.NoError(t, err)
	assert.Equal(t, f.ID, got.ID)

	exists, err := repo.Exists(user.ID, c.ID)
	require.NoError(t, err)
	assert.True(t, exists)

	rows, err := repo.ListFollowed(user.ID, "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1)

	require.NoError(t, repo.Delete(f))
	exists, err = repo.Exists(user.ID, c.ID)
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestChannelFollowingRepository_FindByPair_NotFound(t *testing.T) {
	repo := NewChannelFollowingRepository(testDB)
	_, err := repo.FindByPair("nope", "nope")
	assert.Error(t, err)
}

func TestChannelFollowingRepository_ListFollowed_LimitClamp(t *testing.T) {
	repo := NewChannelFollowingRepository(testDB)
	rows, err := repo.ListFollowed("nobody", "", "", 9999, 0)
	require.NoError(t, err)
	assert.Empty(t, rows)
	rows, err = repo.ListFollowed("nobody", "", "", -1, 0)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestChannelFollowingRepository_ListFollowerIDsPage(t *testing.T) {
	chRepo := NewChannelRepository(testDB)
	repo := NewChannelFollowingRepository(testDB)
	user := insertTestUser(t, "u_page_1", "pageuser1")
	defer cleanupUser(t, user.ID)
	uid := user.ID
	c := newTestChannel("ch_page_1", "paging target", &uid)
	require.NoError(t, chRepo.Create(c))
	defer cleanupChannel(t, c.ID)

	// 複数ユーザーを用意して follower に登録する。
	userIDs := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		u := insertTestUser(t, "u_page_f"+string(rune('a'+i)), "pagef"+string(rune('a'+i)))
		defer cleanupUser(t, u.ID)
		userIDs = append(userIDs, u.ID)
		f := &model.ChannelFollowing{ID: "cf_page_" + string(rune('a'+i)), FollowerID: u.ID, FolloweeID: c.ID}
		require.NoError(t, repo.Create(f))
	}
	defer testDB.Exec(`DELETE FROM "channel_following" WHERE "followeeId" = ?`, c.ID)

	// 2件ずつ取って全 5 件を走査できること。
	var gathered []string
	cursor := ""
	for {
		ids, next, err := repo.ListFollowerIDsPage(c.ID, cursor, 2)
		require.NoError(t, err)
		if len(ids) == 0 {
			break
		}
		gathered = append(gathered, ids...)
		if len(ids) < 2 {
			break
		}
		cursor = next
	}
	assert.ElementsMatch(t, userIDs, gathered)

	// 存在しない channel は空。
	ids, next, err := repo.ListFollowerIDsPage("no-such-channel", "", 10)
	require.NoError(t, err)
	assert.Empty(t, ids)
	assert.Empty(t, next)
}

func TestChannelFollowingRepository_QueryErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewChannelFollowingRepository(db)
	_, err := repo.Exists("a", "b")
	assert.Error(t, err)
	_, err = repo.ListFollowed("a", "", "", 10, 0)
	assert.Error(t, err)
	_, _, err = repo.ListFollowerIDsPage("a", "", 10)
	assert.Error(t, err)
}
