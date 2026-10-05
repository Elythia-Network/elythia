package repository

import (
	"context"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelMutingRepository_ExpiryFilterAndDelete(t *testing.T) {
	repo := NewChannelMutingRepository(testDB)
	seedUser(t, "chexp_u1")
	seedChannel(t, "chexp_active", "chexp_u1")
	seedChannel(t, "chexp_future", "chexp_u1")
	seedChannel(t, "chexp_expired", "chexp_u1")
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "channel_muting" WHERE "userId" = ?`, "chexp_u1") })

	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	require.NoError(t, repo.Create(&model.ChannelMuting{ID: "chexp_m1", UserID: "chexp_u1", ChannelID: "chexp_active"}))
	require.NoError(t, repo.Create(&model.ChannelMuting{ID: "chexp_m2", UserID: "chexp_u1", ChannelID: "chexp_future", ExpiresAt: &future}))
	require.NoError(t, repo.Create(&model.ChannelMuting{ID: "chexp_m3", UserID: "chexp_u1", ChannelID: "chexp_expired", ExpiresAt: &past}))

	// 期限切れ (chexp_expired) は read で除外され、active / future のみ。
	list, err := repo.ListByUser("chexp_u1")
	require.NoError(t, err)
	assert.Len(t, list, 2, "expired は ListByUser から除外")

	ok, err := repo.Exists("chexp_u1", "chexp_expired")
	require.NoError(t, err)
	assert.False(t, ok, "expired は Exists=false")
	ok, _ = repo.Exists("chexp_u1", "chexp_future")
	assert.True(t, ok, "未期限切れは Exists=true")

	// DeleteExpired は expired 行のみ削除し、その userId を返す (#2453)。
	// streaming の snapshot を取り直す宛先になる。
	userIDs, err := repo.DeleteExpired(time.Now())
	require.NoError(t, err)
	assert.Equal(t, []string{"chexp_u1"}, userIDs)
	list, _ = repo.ListByUser("chexp_u1")
	assert.Len(t, list, 2, "active / future は残る")
}

func TestChannelMutingRepository_CreateUpsertOverExpired(t *testing.T) {
	repo := NewChannelMutingRepository(testDB)
	seedUser(t, "chups_u1")
	seedChannel(t, "chups_c1", "chups_u1")
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "channel_muting" WHERE "userId" = ?`, "chups_u1") })

	// 期限切れ行が残ったまま (cron prune 前)、同 (userId, channelId) で再muteする
	// と UNIQUE 制約 (migration 000032) に当たる。Create は upsert で expiresAt を
	// 更新し mute を再活性化させる (#1603 review HIGH)。
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	require.NoError(t, repo.Create(&model.ChannelMuting{ID: "chups_m1", UserID: "chups_u1", ChannelID: "chups_c1", ExpiresAt: &past}))
	// 期限切れなので Exists=false (再mute経路に入る)。
	ok, err := repo.Exists("chups_u1", "chups_c1")
	require.NoError(t, err)
	assert.False(t, ok, "expired は Exists=false")

	// 再mute: 別IDで同 (userId, channelId) を Create しても 500 ではなく upsert 成功。
	require.NoError(t, repo.Create(&model.ChannelMuting{ID: "chups_m2", UserID: "chups_u1", ChannelID: "chups_c1", ExpiresAt: &future}))
	ok, err = repo.Exists("chups_u1", "chups_c1")
	require.NoError(t, err)
	assert.True(t, ok, "再mute後は active (expiresAt が future に更新)")
	list, err := repo.ListByUser("chups_u1")
	require.NoError(t, err)
	assert.Len(t, list, 1, "upsert なので行は 1 つ")
}

func TestChannelMutingRepository_CRUD(t *testing.T) {
	repo := NewChannelMutingRepository(testDB)
	seedUser(t, "chmute_u1")
	seedChannel(t, "chmute_c1", "chmute_u1")
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "channel_muting" WHERE "userId" = ?`, "chmute_u1") })

	m := &model.ChannelMuting{ID: "chmute_1", UserID: "chmute_u1", ChannelID: "chmute_c1"}
	require.NoError(t, repo.Create(m))

	ok, err := repo.Exists("chmute_u1", "chmute_c1")
	require.NoError(t, err)
	assert.True(t, ok)

	list, err := repo.ListByUser("chmute_u1")
	require.NoError(t, err)
	assert.Len(t, list, 1)

	require.NoError(t, repo.Delete("chmute_u1", "chmute_c1"))
	ok, err = repo.Exists("chmute_u1", "chmute_c1")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestChannelMutingRepository_ListEmpty(t *testing.T) {
	repo := NewChannelMutingRepository(testDB)
	list, err := repo.ListByUser("chmute_nonexistent")
	require.NoError(t, err)
	assert.Empty(t, list)
}

// DeleteExpired の失敗は nil slice + error で返す。空 slice だと呼び出し側が
// 「失効 0 件」と区別できず、reload の取りこぼしに気付けない (#2453)。
func TestChannelMutingRepository_DeleteExpired_QueryError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewChannelMutingRepository(testDB.WithContext(ctx))

	userIDs, err := repo.DeleteExpired(time.Now())
	assert.Error(t, err)
	assert.Nil(t, userIDs)
}
