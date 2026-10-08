package signup_test

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type countingCreatedHook struct{ n int32 }

func (h *countingCreatedHook) OnUserCreated(*model.User) { atomic.AddInt32(&h.n, 1) }

// CreatePluginManaged は、プラグインが管理するアカウント (#3468) を実 DB に作る。
// パスワードを持たず、必ず bot で、管理するプラグインの名前を記録する。
func TestCreatePluginManaged_RealDB(t *testing.T) {
	db := integrationDB(t)
	const prefix = "itpm_"
	cleanupSignupRows(t, db, prefix)
	t.Cleanup(func() { cleanupSignupRows(t, db, prefix) })

	idGen, _ := id.NewGenerator("aidx")
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", PreservedUsernames: []string{prefix + "reserved"}, ProhibitedWordsForNameOfUser: []string{"forbidden"}, MinimumUsernameLength: 15}
	svc := signup.NewService(repository.NewUserRepository(db), metaRepo, idGen)
	svc.SetUsedUsernameRepo(repository.NewUsedUsernameRepository(db))
	svc.SetKeypairRepo(repository.NewUserKeypairRepository(db))
	hook := &countingCreatedHook{}
	svc.SetWebhookHook(hook)

	_, err := svc.CreatePluginManaged("bot-plugin", prefix+"bot")
	require.ErrorContains(t, err, "SetDB", "tx が無ければ作らない")
	svc.SetDB(db)

	// 最小文字数 (15) は運営者の経路と同じく見ない。
	u, err := svc.CreatePluginManaged("bot-plugin", "  "+prefix+"Bot  ")
	require.NoError(t, err)
	assert.Equal(t, prefix+"Bot", u.Username)
	assert.Equal(t, int32(1), atomic.LoadInt32(&hook.n), "userCreated の webhook を出す")

	var stored model.User
	require.NoError(t, db.First(&stored, "id = ?", u.ID).Error)
	require.NotNil(t, stored.ManagedByPlugin)
	assert.Equal(t, "bot-plugin", *stored.ManagedByPlugin)
	assert.True(t, stored.IsBot)
	require.NotNil(t, stored.Token)
	assert.Nil(t, stored.Host)
	var profile model.UserProfile
	require.NoError(t, db.First(&profile, `"userId" = ?`, u.ID).Error)
	assert.Nil(t, profile.Password, "パスワードを持たせない")
	var keypairs int64
	require.NoError(t, db.Model(&model.UserKeypair{}).Where(`"userId" = ?`, u.ID).Count(&keypairs).Error)
	assert.Equal(t, int64(1), keypairs, "連合に出られるよう鍵を作る")
	var used int64
	require.NoError(t, db.Model(&model.UsedUsername{}).Where("username = ?", prefix+"bot").Count(&used).Error)
	assert.Equal(t, int64(1), used)

	for _, tc := range []struct {
		name     string
		plugin   string
		username string
		want     error
	}{
		{"duplicate", "other-plugin", prefix + "bot", signup.ErrUsernameUsed},
		{"invalid", "bot-plugin", "bad name", signup.ErrInvalidUsername},
		{"reserved", "bot-plugin", prefix + "reserved", signup.ErrUsernameReserved},
		{"prohibited", "bot-plugin", prefix + "forbidden", signup.ErrUsernameUsed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreatePluginManaged(tc.plugin, tc.username)
			assert.ErrorIs(t, err, tc.want)
		})
	}

	_, err = svc.CreatePluginManaged("", prefix+"noplugin")
	assert.Error(t, err)

	// 既存の普通の利用者と同じ名前 (used_username に無くても) は tx の中で弾く。
	tok := "itpmtoken0000016"
	require.NoError(t, db.Create(&model.User{ID: prefix + "plain", Username: prefix + "plain", UsernameLower: prefix + "plain", Token: &tok}).Error)
	_, err = svc.CreatePluginManaged("bot-plugin", prefix+"plain")
	assert.ErrorIs(t, err, signup.ErrUsernameAlreadyExists)

	// meta を読めないときは作らない。
	metaRepo.FetchErr = errors.New("db down")
	_, err = svc.CreatePluginManaged("bot-plugin", prefix+"nometa")
	assert.Error(t, err)
	assert.False(t, errors.Is(err, signup.ErrUsernameAlreadyExists))
	assert.Equal(t, int32(1), atomic.LoadInt32(&hook.n), "失敗したときは webhook を出さない")
}
