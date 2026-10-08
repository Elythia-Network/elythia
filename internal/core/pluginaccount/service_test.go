package pluginaccount_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/pluginaccount"
	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

type fakeUploader struct {
	inputs []coredrive.UploadInput
	err    error
}

func (f *fakeUploader) Upload(_ context.Context, in coredrive.UploadInput) (*model.DriveFile, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.inputs = append(f.inputs, in)
	return &model.DriveFile{ID: "file-" + in.Name}, nil
}

type fakeDeleter struct {
	deleted []string
	err     error
}

func (f *fakeDeleter) DeletePluginManagedAccount(u *model.User) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, u.ID)
	return nil
}

type updateCall struct {
	userID string
	params map[string]any
}

type fixture struct {
	db       *gorm.DB
	svc      *pluginaccount.Service
	uploader *fakeUploader
	deleter  *fakeDeleter
	updates  []updateCall
	updErr   error
}

const prefix = "pa_core_"

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := testutil.OpenTestDB()
	if err != nil {
		t.Skipf("PostgreSQL test DB unavailable: %v", err)
	}
	testutil.ApplyMigrations(db)
	clean := func() {
		db.Exec(`DELETE FROM "user_profile" WHERE "userId" IN (SELECT id FROM "user" WHERE "usernameLower" LIKE ?)`, prefix+"%")
		db.Exec(`DELETE FROM "user" WHERE "usernameLower" LIKE ?`, prefix+"%")
		db.Exec(`DELETE FROM "used_username" WHERE "username" LIKE ?`, prefix+"%")
	}
	clean()
	t.Cleanup(clean)

	idGen, _ := id.NewGenerator("aidx")
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	users := repository.NewUserRepository(db)
	creator := signup.NewService(users, metaRepo, idGen)
	creator.SetDB(db)
	creator.SetUsedUsernameRepo(repository.NewUsedUsernameRepository(db))

	f := &fixture{db: db, uploader: &fakeUploader{}, deleter: &fakeDeleter{}}
	f.svc = pluginaccount.NewService(users, repository.NewPluginAccountRepository(db), creator)
	f.svc.SetUploader(f.uploader)
	f.svc.SetDeleter(f.deleter)
	f.svc.SetProfileUpdater(func(_ context.Context, userID string, params map[string]any) error {
		f.updates = append(f.updates, updateCall{userID: userID, params: params})
		return f.updErr
	})
	return f
}

func usernames(users []*model.User) []string {
	out := []string{}
	for _, u := range users {
		out = append(out, u.Username)
	}
	return out
}

// 一覧と所有の判定はプラグインの名前で分ける。他のプラグインのアカウント・
// 普通の利用者・削除済み・リモートは、どれも「無い」になる (#3468)。
func TestService_ScopesByPlugin(t *testing.T) {
	f := newFixture(t)
	a1, err := f.svc.Create("plugin-a", prefix+"a1")
	require.NoError(t, err)
	a2, err := f.svc.Create("plugin-a", prefix+"a2")
	require.NoError(t, err)
	b1, err := f.svc.Create("plugin-b", prefix+"b1")
	require.NoError(t, err)

	tok := "pacoretoken00016"
	require.NoError(t, f.db.Create(&model.User{ID: prefix + "plain", Username: prefix + "plain", UsernameLower: prefix + "plain", Token: &tok}).Error)
	host := "remote.example"
	name := "plugin-a"
	require.NoError(t, f.db.Create(&model.User{ID: prefix + "remote", Username: prefix + "remote", UsernameLower: prefix + "remote", Host: &host, ManagedByPlugin: &name}).Error)

	listA, err := f.svc.List("plugin-a")
	require.NoError(t, err)
	assert.Equal(t, []string{prefix + "a1", prefix + "a2"}, usernames(listA))
	listB, err := f.svc.List("plugin-b")
	require.NoError(t, err)
	assert.Equal(t, []string{prefix + "b1"}, usernames(listB))

	got, err := f.svc.Owned("plugin-a", a1.ID)
	require.NoError(t, err)
	assert.Equal(t, a1.ID, got.ID)
	for _, target := range []string{b1.ID, prefix + "plain", prefix + "remote", "no-such-user", ""} {
		_, err := f.svc.Owned("plugin-a", target)
		assert.ErrorIs(t, err, pluginaccount.ErrNotFound, target)
	}
	_, err = f.svc.Owned("", a1.ID)
	assert.ErrorIs(t, err, pluginaccount.ErrNotFound)

	// 削除済みは一覧にも所有にも出ない。
	require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", a2.ID).Update("isDeleted", true).Error)
	listA, err = f.svc.List("plugin-a")
	require.NoError(t, err)
	assert.Equal(t, []string{prefix + "a1"}, usernames(listA))
	_, err = f.svc.Owned("plugin-a", a2.ID)
	assert.ErrorIs(t, err, pluginaccount.ErrNotFound)

	_, err = f.svc.List("")
	assert.Error(t, err)
	_, err = f.svc.Create("", prefix+"x")
	assert.Error(t, err)
}

func TestService_UpdateProfile(t *testing.T) {
	f := newFixture(t)
	a1, err := f.svc.Create("plugin-a", prefix+"u1")
	require.NoError(t, err)
	b1, err := f.svc.Create("plugin-b", prefix+"u2")
	require.NoError(t, err)
	ctx := context.Background()
	name, desc := "Bot", ""

	require.NoError(t, f.svc.UpdateProfile(ctx, "plugin-a", a1.ID, pluginaccount.ProfileInput{
		Name: &name, Description: &desc,
		Avatar: &pluginaccount.Image{Data: []byte("png"), Filename: "a.png"},
		Banner: &pluginaccount.Image{Data: []byte("png")},
	}))
	require.Len(t, f.updates, 1)
	assert.Equal(t, a1.ID, f.updates[0].userID)
	assert.Equal(t, map[string]any{
		"name": "Bot", "description": "", "avatarId": "file-a.png", "bannerId": "file-banner",
	}, f.updates[0].params)
	require.Len(t, f.uploader.inputs, 2)
	for _, in := range f.uploader.inputs {
		require.NotNil(t, in.User)
		assert.Equal(t, a1.ID, in.User.ID, "画像はそのアカウントのドライブに置く")
	}

	// 変える項目が無ければ何も呼ばない。
	require.NoError(t, f.svc.UpdateProfile(ctx, "plugin-a", a1.ID, pluginaccount.ProfileInput{}))
	assert.Len(t, f.updates, 1)

	// 他のプラグインのアカウントは、画像も置かずに断る。
	err = f.svc.UpdateProfile(ctx, "plugin-a", b1.ID, pluginaccount.ProfileInput{
		Name: &name, Avatar: &pluginaccount.Image{Data: []byte("png")},
	})
	assert.ErrorIs(t, err, pluginaccount.ErrNotFound)
	assert.Len(t, f.uploader.inputs, 2)
	assert.Len(t, f.updates, 1)

	// 空の画像・アップロードの失敗・i/update の失敗は、そのまま返す。
	err = f.svc.UpdateProfile(ctx, "plugin-a", a1.ID, pluginaccount.ProfileInput{Avatar: &pluginaccount.Image{}})
	assert.ErrorContains(t, err, "empty")
	err = f.svc.UpdateProfile(ctx, "plugin-a", a1.ID, pluginaccount.ProfileInput{Banner: &pluginaccount.Image{}})
	assert.ErrorContains(t, err, "empty")
	f.uploader.err = errors.New("quota")
	err = f.svc.UpdateProfile(ctx, "plugin-a", a1.ID, pluginaccount.ProfileInput{Banner: &pluginaccount.Image{Data: []byte("x")}})
	assert.ErrorContains(t, err, "quota")
	f.uploader.err = nil
	f.updErr = errors.New("rejected")
	err = f.svc.UpdateProfile(ctx, "plugin-a", a1.ID, pluginaccount.ProfileInput{Name: &name})
	assert.ErrorContains(t, err, "rejected")
}

func TestService_Delete(t *testing.T) {
	f := newFixture(t)
	a1, err := f.svc.Create("plugin-a", prefix+"d1")
	require.NoError(t, err)
	b1, err := f.svc.Create("plugin-b", prefix+"d2")
	require.NoError(t, err)

	assert.ErrorIs(t, f.svc.Delete("plugin-a", b1.ID), pluginaccount.ErrNotFound)
	assert.Empty(t, f.deleter.deleted)
	require.NoError(t, f.svc.Delete("plugin-a", a1.ID))
	assert.Equal(t, []string{a1.ID}, f.deleter.deleted)

	f.deleter.err = errors.New("boom")
	assert.ErrorContains(t, f.svc.Delete("plugin-b", b1.ID), "boom")
}

// 配線が欠けていたら、黙って成功せずにエラーを返す。
func TestService_Unwired(t *testing.T) {
	f := newFixture(t)
	a1, err := f.svc.Create("plugin-a", prefix+"w1")
	require.NoError(t, err)
	users := repository.NewUserRepository(f.db)
	bare := pluginaccount.NewService(users, repository.NewPluginAccountRepository(f.db), nil)
	ctx := context.Background()
	name := "x"

	_, err = bare.Create("plugin-a", prefix+"w2")
	assert.ErrorContains(t, err, "not wired")
	assert.ErrorContains(t, bare.UpdateProfile(ctx, "plugin-a", a1.ID, pluginaccount.ProfileInput{Name: &name}), "not wired")
	assert.ErrorContains(t, bare.UpdateProfile(ctx, "plugin-a", a1.ID, pluginaccount.ProfileInput{Avatar: &pluginaccount.Image{Data: []byte("x")}}), "not wired")
	assert.ErrorContains(t, bare.Delete("plugin-a", a1.ID), "not wired")
}

type failingUsers struct{ repository.UserRepository }

func (failingUsers) FindByID(string) (*model.User, error) { return nil, errors.New("db down") }

// DB 障害を「無い」にしない (#2792)。
func TestService_OwnedLookupFailure(t *testing.T) {
	f := newFixture(t)
	svc := pluginaccount.NewService(failingUsers{}, repository.NewPluginAccountRepository(f.db), nil)
	_, err := svc.Owned("plugin-a", "u1")
	require.Error(t, err)
	assert.False(t, errors.Is(err, pluginaccount.ErrNotFound))
}

// 凍結されたアカウントのプロフィールは、画像を置く前に断る。ドライブへの
// アップロードは凍結の gate を通らないので、先に断らないとファイルだけが残る。
func TestService_UpdateProfileRefusesSuspended(t *testing.T) {
	f := newFixture(t)
	a1, err := f.svc.Create("plugin-a", prefix+"s1")
	require.NoError(t, err)
	require.NoError(t, f.db.Model(&model.User{}).Where("id = ?", a1.ID).Update("isSuspended", true).Error)
	name := "x"
	err = f.svc.UpdateProfile(context.Background(), "plugin-a", a1.ID, pluginaccount.ProfileInput{
		Name: &name, Avatar: &pluginaccount.Image{Data: []byte("png")},
	})
	assert.ErrorIs(t, err, pluginaccount.ErrSuspended)
	assert.Empty(t, f.uploader.inputs, "画像を置かない")
	assert.Empty(t, f.updates)
	// 削除は凍結中でもできる (管理画面から消すのと同じ)。
	require.NoError(t, f.svc.Delete("plugin-a", a1.ID))
}
