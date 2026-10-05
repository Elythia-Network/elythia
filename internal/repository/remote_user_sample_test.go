package repository

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/model"
)

func insertRemoteSampleUser(t *testing.T, id, username, host string, fetched *time.Time, suspended, deleted bool) {
	t.Helper()
	u := &model.User{
		ID: id, Username: username, UsernameLower: username, Host: &host,
		LastFetchedAt: fetched, IsSuspended: suspended, IsDeleted: deleted,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(u).Error)
	t.Cleanup(func() { cleanupUser(t, id) })
}

// 最近取得したユーザーを選び、凍結 / 削除済みと別ホストは選ばない。
func TestRemoteUserSampler_FindRecentByHost(t *testing.T) {
	repo := NewRemoteUserSampler(testDB)
	old := time.Now().Add(-48 * time.Hour)
	recent := time.Now().Add(-time.Hour)
	newest := time.Now()
	insertRemoteSampleUser(t, "rs_old", "old", "sample.example", &old, false, false)
	insertRemoteSampleUser(t, "rs_recent", "recent", "sample.example", &recent, false, false)
	insertRemoteSampleUser(t, "rs_never", "never", "sample.example", nil, false, false)
	insertRemoteSampleUser(t, "rs_susp", "susp", "sample.example", &newest, true, false)
	insertRemoteSampleUser(t, "rs_del", "del", "sample.example", &newest, false, true)
	insertRemoteSampleUser(t, "rs_other", "other", "other-sample.example", &newest, false, false)

	u, err := repo.FindRecentByHost("sample.example")
	require.NoError(t, err)
	assert.Equal(t, "rs_recent", u.ID)
}

func TestRemoteUserSampler_FindRecentByHost_None(t *testing.T) {
	repo := NewRemoteUserSampler(testDB)
	_, err := repo.FindRecentByHost("nobody-sample.example")
	assert.ErrorIs(t, err, ErrNotFound)

	_, err = repo.FindRecentByHost("bad\x00host")
	assert.ErrorIs(t, err, ErrNotFound, "a host that can never match is not found without querying")
}
