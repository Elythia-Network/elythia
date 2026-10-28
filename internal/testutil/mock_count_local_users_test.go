package testutil

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mock の CountLocalUsers は、repository と同じく削除済みとプラグインが管理する
// アカウントを外す (#3881)。システムアカウントは mock が知らないので見ない。
func TestMockUserRepository_CountLocalUsersLeavesOutPluginAccounts(t *testing.T) {
	repo := NewMockUserRepository()
	plugin := "plugin-a"
	host := "remote.example"
	repo.Users["person"] = &model.User{ID: "person"}
	repo.Users["bot"] = &model.User{ID: "bot", ManagedByPlugin: &plugin}
	repo.Users["deleted"] = &model.User{ID: "deleted", IsDeleted: true}
	repo.Users["remote"] = &model.User{ID: "remote", Host: &host}
	n, err := repo.CountLocalUsers()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}
