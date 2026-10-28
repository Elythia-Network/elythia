package repository

import (
	"testing"
	"time"

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
// 管理するアカウント (#3468) とシステムアカウントを除いたローカル利用者を数える。
// CountLocalUsers (nodeinfo の利用者数) も同じものを外す (#3881)。
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
	assert.Equal(t, int64(0), all, "利用者数には bot を数えない")
	assert.Equal(t, int64(0), setup, "セットアップの判定には bot を数えない")

	// システムアカウントは、どちらにも数えない。
	require.NoError(t, db.Create(&model.User{ID: "cs-actor", Username: "instance.actor", UsernameLower: "instance.actor"}).Error)
	require.NoError(t, db.Create(&model.SystemAccount{ID: "cs-sa", UserID: "cs-actor", Type: "instance"}).Error)
	all, setup = counts()
	assert.Equal(t, int64(0), all, "利用者数にはシステムアカウントも数えない")
	assert.Equal(t, int64(0), setup, "セットアップの判定にもシステムアカウントを数えない")

	require.NoError(t, db.Create(&model.User{ID: "cs-plain", Username: "cs_plain", UsernameLower: "cs_plain"}).Error)
	all, setup = counts()
	assert.Equal(t, int64(1), all)
	assert.Equal(t, int64(1), setup)

	// セットアップの判定は、ログインできる利用者をすべて数える。bot になった利用者、
	// 一度も活動していないか長く活動していない利用者、凍結された利用者も、
	// rootUserId が無いまま居れば未認証の窓を閉じる理由になる。nodeinfo の利用者数の
	// 数え方を変えても、ここが一緒に動かないことを確かめる。
	clean()
	old := time.Now().AddDate(-2, 0, 0)
	for _, u := range []*model.User{
		{ID: "cs-botflag", Username: "cs_botflag", UsernameLower: "cs_botflag", IsBot: true},
		{ID: "cs-inactive", Username: "cs_inactive", UsernameLower: "cs_inactive", LastActiveDate: &old},
		{ID: "cs-suspended", Username: "cs_suspended", UsernameLower: "cs_suspended", IsSuspended: true},
	} {
		require.NoError(t, db.Create(u).Error)
		_, setup = counts()
		assert.Equal(t, int64(1), setup, "%s はセットアップの判定に数える", u.ID)
		require.NoError(t, db.Exec(`DELETE FROM "user" WHERE id = ?`, u.ID).Error)
	}
}
