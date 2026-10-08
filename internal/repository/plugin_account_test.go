package repository

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// ListByPlugin は、そのプラグインが管理する、削除されていないローカルの
// アカウントだけを作成順に返す (#3468)。
func TestPluginAccountRepository_ListByPlugin(t *testing.T) {
	db, err := testutil.OpenTestDB()
	require.NoError(t, err)
	testutil.ApplyMigrations(db)
	const prefix = "repo_pa_"
	clean := func() { db.Exec(`DELETE FROM "user" WHERE "id" LIKE ?`, prefix+"%") }
	clean()
	t.Cleanup(clean)

	a, b := "plugin-a", "plugin-b"
	host := "remote.example"
	for _, u := range []*model.User{
		{ID: prefix + "2", ManagedByPlugin: &a},
		{ID: prefix + "1", ManagedByPlugin: &a},
		{ID: prefix + "3", ManagedByPlugin: &b},
		{ID: prefix + "4"},
		{ID: prefix + "5", ManagedByPlugin: &a, IsDeleted: true},
		{ID: prefix + "6", ManagedByPlugin: &a, Host: &host},
	} {
		u.Username, u.UsernameLower = u.ID, u.ID
		require.NoError(t, db.Create(u).Error)
	}

	repo := NewPluginAccountRepository(db)
	ids := func(plugin string) []string {
		t.Helper()
		users, err := repo.ListByPlugin(plugin)
		require.NoError(t, err)
		out := []string{}
		for _, u := range users {
			out = append(out, u.ID)
		}
		return out
	}
	assert.Equal(t, []string{prefix + "1", prefix + "2"}, ids(a))
	assert.Equal(t, []string{prefix + "3"}, ids(b))
	assert.Empty(t, ids("plugin-c"))
	assert.Empty(t, ids("bad\x00name"), "NUL を含む名前はクエリを投げずに空")

	// DB 障害はエラーとして返す。
	closed, err := testutil.OpenTestDB()
	require.NoError(t, err)
	sqlDB, err := closed.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	_, err = NewPluginAccountRepository(closed).ListByPlugin(a)
	assert.Error(t, err)
}

// CountLocalUsersForSetup は、初回セットアップの判定のために、プラグインが
// 管理するアカウントを除いたローカル利用者を数える (#3468)。CountLocalUsers
// (nodeinfo などの利用者数) は管理するアカウントも数える。
func TestUserRepository_CountLocalUsersForSetup(t *testing.T) {
	db, err := testutil.OpenTestDBSchema("setupcount")
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	testutil.ApplyMigrations(db)
	clean := func() { require.NoError(t, db.Exec(`TRUNCATE "user" CASCADE`).Error) }
	clean()
	t.Cleanup(clean)

	repo := NewUserRepository(db)
	counts := func() (int64, int64) {
		t.Helper()
		all, err := repo.CountLocalUsers()
		require.NoError(t, err)
		setup, err := repo.CountLocalUsersForSetup()
		require.NoError(t, err)
		return all, setup
	}
	plugin := "plugin-a"
	host := "remote.example"
	require.NoError(t, db.Create(&model.User{ID: "cs-bot", Username: "cs_bot", UsernameLower: "cs_bot", ManagedByPlugin: &plugin}).Error)
	require.NoError(t, db.Create(&model.User{ID: "cs-remote", Username: "cs_remote", UsernameLower: "cs_remote", Host: &host}).Error)
	require.NoError(t, db.Create(&model.User{ID: "cs-deleted", Username: "cs_deleted", UsernameLower: "cs_deleted", IsDeleted: true}).Error)
	all, setup := counts()
	assert.Equal(t, int64(1), all, "利用者数には bot も数える")
	assert.Equal(t, int64(0), setup, "セットアップの判定には数えない")

	require.NoError(t, db.Create(&model.User{ID: "cs-plain", Username: "cs_plain", UsernameLower: "cs_plain"}).Error)
	all, setup = counts()
	assert.Equal(t, int64(2), all)
	assert.Equal(t, int64(1), setup)
}
