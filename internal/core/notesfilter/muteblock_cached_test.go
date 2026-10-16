package notesfilter

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestLoadMuteBlockSetsCached_MatchesRepositories pins that reading through
// the relation cache yields the same sets as reading the repositories, and
// that a dimension passed as nil stays off (#3607).
func TestLoadMuteBlockSetsCached_MatchesRepositories(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)
	const prefix = "nfc3607_"
	cleanup := func() {
		db.Exec(`DELETE FROM "channel" WHERE id LIKE ?`, prefix+"%")
		db.Exec(`DELETE FROM "user" WHERE id LIKE ?`, prefix+"%")
	}
	cleanup()
	t.Cleanup(cleanup)
	mk := func(name string) string {
		id := prefix + name
		require.NoError(t, db.Create(&model.User{ID: id, Username: id, UsernameLower: id, AvatarDecorations: datatypes.JSON([]byte("[]"))}).Error)
		return id
	}
	v, x, y := mk("v"), mk("x"), mk("y")
	ch := prefix + "ch"
	require.NoError(t, db.Exec(`INSERT INTO "channel" (id, name, "userId") VALUES (?, ?, ?)`, ch, ch, x).Error)
	require.NoError(t, db.Create(&model.Muting{ID: prefix + "m", MuterID: v, MuteeID: x}).Error)
	require.NoError(t, db.Create(&model.Blocking{ID: prefix + "b", BlockerID: y, BlockeeID: v}).Error)
	require.NoError(t, db.Create(&model.ChannelMuting{ID: prefix + "cm", UserID: v, ChannelID: ch}).Error)

	muting := repository.NewMutingRepository(db)
	blocking := repository.NewBlockingRepository(db)
	channelMuting := repository.NewChannelMutingRepository(db)
	viewer := &model.User{ID: v}
	rc := repository.NewRelationCache(db)

	want, err := LoadMuteBlockSets(viewer, muting, blocking, channelMuting, nil)
	require.NoError(t, err)
	require.Len(t, want.MutedUserIDs, 1)
	require.Len(t, want.BlockerIDs, 1)
	require.Len(t, want.MutedChannelIDs, 1)

	got, err := LoadMuteBlockSetsCached(rc, viewer, muting, blocking, channelMuting, nil)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	// 読み込みがキャッシュを通っていること: キャッシュには項目が入る。
	ids, err := rc.MutedUserIDs(v)
	require.NoError(t, err)
	assert.Equal(t, []string{x}, ids)
	db.Exec(`DELETE FROM "muting" WHERE id = ?`, prefix+"m")
	got, err = LoadMuteBlockSetsCached(rc, viewer, muting, blocking, channelMuting, nil)
	require.NoError(t, err)
	assert.Len(t, got.MutedUserIDs, 1, "read from the cache, not the repository")

	// nil で渡した次元は、キャッシュがあっても掛けない。
	got, err = LoadMuteBlockSetsCached(rc, viewer, nil, blocking, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, got.MutedUserIDs)
	assert.Nil(t, got.MutedChannelIDs)
	assert.Len(t, got.BlockerIDs, 1)

	// rc が nil なら repository をそのまま使う。
	m, b, cm := CachedListers(nil, muting, blocking, channelMuting)
	assert.Equal(t, MuteeIDLister(muting), m)
	assert.Equal(t, BlockerIDLister(blocking), b)
	assert.Equal(t, ChannelMuteLister(channelMuting), cm)

	// キャッシュの読み込みの失敗は fail-closed で返す。
	broken := testutil.MustOpenTestDB()
	sqlDB, err := broken.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	brokenRC := repository.NewRelationCache(broken)
	_, err = LoadMuteBlockSetsCached(brokenRC, viewer, nil, nil, channelMuting, nil)
	assert.Error(t, err)
}
