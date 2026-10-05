package user_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newFullSvc(t *testing.T) (*user.Service, *testutil.MockUserRepository, *testutil.MockNoteRepository, *testutil.MockUserNotePiningRepository) {
	t.Helper()
	uRepo := testutil.NewMockUserRepository()
	nRepo := testutil.NewMockNoteRepository()
	pRepo := testutil.NewMockUserNotePiningRepository()
	idGen, _ := id.NewGenerator("aidx")
	return user.NewService(uRepo, nRepo, pRepo, idGen), uRepo, nRepo, pRepo
}

func ptr[T any](v T) *T { return &v }

func TestService_ShowByID_Success(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	desc := "hello"
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Description: &desc}
	svc := user.NewService(repo, nil, nil, nil)

	bundle, err := svc.ShowByID("u1")
	require.NoError(t, err)
	assert.Equal(t, "alice", bundle.User.Username)
	require.NotNil(t, bundle.Profile)
	assert.Equal(t, "hello", *bundle.Profile.Description)
}

func TestService_ShowByID_NotFound(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	svc := user.NewService(repo, nil, nil, nil)

	_, err := svc.ShowByID("missing")
	require.Error(t, err)
	assert.True(t, errors.Is(err, user.ErrUserNotFound))
}

func TestService_ShowByID_NoProfile(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	svc := user.NewService(repo, nil, nil, nil)

	bundle, err := svc.ShowByID("u1")
	require.NoError(t, err)
	assert.Nil(t, bundle.Profile)
}

func TestService_ShowManyByIDs_PreservesOrderAndSkipsMissing(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	repo.Users["u2"] = &model.User{ID: "u2", Username: "bob"}
	d1 := "d1"
	d2 := "d2"
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Description: &d1}
	repo.Profiles["u2"] = &model.UserProfile{UserID: "u2", Description: &d2}
	svc := user.NewService(repo, nil, nil, nil)

	out, err := svc.ShowManyByIDs([]string{"u2", "ghost", "u1"})
	require.NoError(t, err)
	require.Len(t, out, 2)
	// Order should follow input list; the missing "ghost" is dropped.
	assert.Equal(t, "u2", out[0].User.ID)
	assert.Equal(t, "u1", out[1].User.ID)
	require.NotNil(t, out[0].Profile)
	assert.Equal(t, "d2", *out[0].Profile.Description)
	require.NotNil(t, out[1].Profile)
	assert.Equal(t, "d1", *out[1].Profile.Description)
}

func TestService_ShowManyByIDs_EmptyInput(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	svc := user.NewService(repo, nil, nil, nil)

	out, err := svc.ShowManyByIDs(nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestService_ShowManyByIDs_AllMissing(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	svc := user.NewService(repo, nil, nil, nil)

	out, err := svc.ShowManyByIDs([]string{"x", "y"})
	require.NoError(t, err)
	assert.Nil(t, out)
}

// TestService_FindManyByIDs covers the bare-user batch helper added for
// /api/pages/featured owner resolution (#1134). 空入力は短絡で nil 返却、
// 通常入力は repo に委譲して User row のみ取れる (profile fetch 無し)。
func TestService_FindManyByIDs(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	repo.Users["u2"] = &model.User{ID: "u2", Username: "bob"}
	svc := user.NewService(repo, nil, nil, nil)

	// 空入力は repo を叩かずに nil 返却。
	out, err := svc.FindManyByIDs(nil)
	require.NoError(t, err)
	assert.Nil(t, out)

	// 通常入力は User row 2 件取得。
	out, err = svc.FindManyByIDs([]string{"u1", "u2"})
	require.NoError(t, err)
	assert.Len(t, out, 2)
}

func TestService_GetProfilesByUserIDs_BatchOK(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	d1, d2 := "p1", "p2"
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Description: &d1}
	repo.Profiles["u2"] = &model.UserProfile{UserID: "u2", Description: &d2}
	svc := user.NewService(repo, nil, nil, nil)

	out := svc.GetProfilesByUserIDs([]string{"u1", "u2", "ghost"})
	require.Len(t, out, 2)
	require.NotNil(t, out["u1"])
	require.NotNil(t, out["u2"])
	assert.Equal(t, "p1", *out["u1"].Description)
	assert.Equal(t, "p2", *out["u2"].Description)
	assert.NotContains(t, out, "ghost", "missing profiles must not appear in the map")
}

func TestService_GetProfilesByUserIDs_EmptyInput(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	svc := user.NewService(repo, nil, nil, nil)
	out := svc.GetProfilesByUserIDs(nil)
	assert.Empty(t, out)
}

// failingProfileRepo wraps the mock to make FindProfilesByUserIDs return an
// error, so we can exercise the service-side fallback (returns empty map,
// never nil).
type failingProfileRepo struct {
	*testutil.MockUserRepository
}

func (failingProfileRepo) FindProfilesByUserIDs(_ []string) ([]*model.UserProfile, error) {
	return nil, errors.New("db error")
}

func TestService_GetProfilesByUserIDs_RepoErrorYieldsEmptyMap(t *testing.T) {
	svc := user.NewService(failingProfileRepo{testutil.NewMockUserRepository()}, nil, nil, nil)
	out := svc.GetProfilesByUserIDs([]string{"u1"})
	require.NotNil(t, out, "must return a non-nil map even on repository error")
	assert.Empty(t, out)
}

func TestService_ShowManyByIDs_NoProfileSilentlyOmitted(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	// Profile 行が無くても User は返る
	svc := user.NewService(repo, nil, nil, nil)

	out, err := svc.ShowManyByIDs([]string{"u1"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "u1", out[0].User.ID)
	assert.Nil(t, out[0].Profile)
}

func TestService_ShowByUsername_Success(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", UsernameLower: "alice"}
	svc := user.NewService(repo, nil, nil, nil)

	bundle, err := svc.ShowByUsername("alice", nil)
	require.NoError(t, err)
	assert.Equal(t, "u1", bundle.User.ID)
}

func TestService_ShowByUsername_WithHost(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	host := "remote.example.com"
	repo.Users["u2"] = &model.User{ID: "u2", Username: "bob", UsernameLower: "bob", Host: &host}
	svc := user.NewService(repo, nil, nil, nil)

	bundle, err := svc.ShowByUsername("bob", &host)
	require.NoError(t, err)
	assert.Equal(t, "u2", bundle.User.ID)
}

func TestService_ShowByUsername_NotFound(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	svc := user.NewService(repo, nil, nil, nil)

	_, err := svc.ShowByUsername("nobody", nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, user.ErrUserNotFound))
}

// stubRemoteResolver implements user.RemoteUserResolver for unit tests.
type stubRemoteResolver struct {
	calls []struct{ username, host string }
	user  *model.User
	err   error
}

func (s *stubRemoteResolver) ResolveByUsernameHost(username, host string) (*model.User, error) {
	s.calls = append(s.calls, struct{ username, host string }{username, host})
	if s.err != nil {
		return nil, s.err
	}
	return s.user, nil
}

func TestService_ShowByUsername_RemoteFallback_NoResolver_ReturnsErrUserNotFound(t *testing.T) {
	// host が指定されていても resolver が未注入なら従来どおり ErrUserNotFound
	// を返すこと (後方互換)。
	repo := testutil.NewMockUserRepository()
	svc := user.NewService(repo, nil, nil, nil)

	host := "remote.example"
	_, err := svc.ShowByUsername("ghost", &host)
	require.Error(t, err)
	assert.True(t, errors.Is(err, user.ErrUserNotFound))
}

func TestService_ShowByUsername_RemoteFallback_EmptyHostIgnoresResolver(t *testing.T) {
	// host が空文字列の場合は resolver を呼ばずに ErrUserNotFound を返す。
	repo := testutil.NewMockUserRepository()
	svc := user.NewService(repo, nil, nil, nil)
	resolver := &stubRemoteResolver{}
	svc.SetRemoteUserResolver(resolver)

	empty := ""
	_, err := svc.ShowByUsername("ghost", &empty)
	require.Error(t, err)
	assert.True(t, errors.Is(err, user.ErrUserNotFound))
	assert.Empty(t, resolver.calls)
}

func TestService_ShowByUsername_RemoteFallback_ResolverSucceeds(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	svc := user.NewService(repo, nil, nil, nil)
	host := "remote.example"
	resolved := &model.User{ID: "u9", Username: "remote", UsernameLower: "remote", Host: &host}
	resolver := &stubRemoteResolver{user: resolved}
	svc.SetRemoteUserResolver(resolver)

	desc := "imported"
	repo.Profiles["u9"] = &model.UserProfile{UserID: "u9", Description: &desc}

	bundle, err := svc.ShowByUsername("remote", &host)
	require.NoError(t, err)
	assert.Equal(t, "u9", bundle.User.ID)
	require.NotNil(t, bundle.Profile)
	assert.Equal(t, "imported", *bundle.Profile.Description)
	require.Len(t, resolver.calls, 1)
	assert.Equal(t, "remote", resolver.calls[0].username)
	assert.Equal(t, host, resolver.calls[0].host)
}

func TestService_ShowByUsername_RemoteFallback_ResolverFails(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	svc := user.NewService(repo, nil, nil, nil)
	resolver := &stubRemoteResolver{err: errors.New("webfinger: dial timeout")}
	svc.SetRemoteUserResolver(resolver)

	host := "remote.example"
	_, err := svc.ShowByUsername("ghost", &host)
	require.Error(t, err)
	assert.True(t, errors.Is(err, user.ErrFailedToResolveRemoteUser))
	require.Len(t, resolver.calls, 1)
}

func TestService_ShowByUsername_RemoteFallback_ResolverReturnsNil(t *testing.T) {
	// resolver が (nil, nil) を返した場合も失敗扱いにする (防御的実装)。
	repo := testutil.NewMockUserRepository()
	svc := user.NewService(repo, nil, nil, nil)
	resolver := &stubRemoteResolver{user: nil}
	svc.SetRemoteUserResolver(resolver)

	host := "remote.example"
	_, err := svc.ShowByUsername("ghost", &host)
	require.Error(t, err)
	assert.True(t, errors.Is(err, user.ErrFailedToResolveRemoteUser))
}

func TestService_ShowByUsername_LocalHit_ResolverNotCalled(t *testing.T) {
	// ローカル DB hit の場合は resolver を呼ばずに即返す。
	repo := testutil.NewMockUserRepository()
	host := "remote.example"
	repo.Users["u3"] = &model.User{ID: "u3", Username: "cached", UsernameLower: "cached", Host: &host}
	svc := user.NewService(repo, nil, nil, nil)
	resolver := &stubRemoteResolver{err: errors.New("should not be called")}
	svc.SetRemoteUserResolver(resolver)

	bundle, err := svc.ShowByUsername("cached", &host)
	require.NoError(t, err)
	assert.Equal(t, "u3", bundle.User.ID)
	assert.Empty(t, resolver.calls)
}

func TestService_GetProfile_Found(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	desc := "hi"
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Description: &desc}
	svc := user.NewService(repo, nil, nil, nil)

	p := svc.GetProfile("u1")
	require.NotNil(t, p)
	assert.Equal(t, "hi", *p.Description)
}

func TestService_GetProfile_Missing(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	svc := user.NewService(repo, nil, nil, nil)

	assert.Nil(t, svc.GetProfile("missing"))
}

// --- Search ---

func TestService_Search_Empty(t *testing.T) {
	svc, _, _, _ := newFullSvc(t)
	out, err := svc.Search("   ", "", 10, 0, "")
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestService_Search_AtPrefix(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", UsernameLower: "alice"}
	out, err := svc.Search("@al", "", 10, 0, "")
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

func TestService_Search_DefaultLimit(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", UsernameLower: "alice"}
	out, err := svc.Search("a", "", 0, 0, "")
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

// --- UpdateProfile ---

func TestService_UpdateProfile_Success(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	bundle, err := svc.UpdateProfile("u1", user.UpdateInput{
		Name:              ptr(ptr("New Name")),
		Description:       ptr(ptr("hi there")),
		Location:          ptr(ptr("Tokyo")),
		Birthday:          ptr(ptr("1990-01-01")),
		Lang:              ptr(ptr("ja")),
		IsLocked:          ptr(true),
		IsBot:             ptr(true),
		IsCat:             ptr(true),
		IsExplorable:      ptr(false),
		HideOnlineStatus:  ptr(true),
		AlwaysMarkNsfw:    ptr(true),
		AutoSensitive:     ptr(true),
		NoCrawle:          ptr(true),
		PreventAiLearning: ptr(true),
	})
	require.NoError(t, err)
	require.NotNil(t, bundle)
	assert.True(t, bundle.User.IsLocked)
	assert.True(t, bundle.User.IsBot)
}

// #787: ワードミュート (mutedWords / hardMutedWords) の persist。
func TestService_UpdateProfile_MutedWords(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	mw := json.RawMessage([]byte(`[["foo"],["bar","baz"]]`))
	hmw := json.RawMessage([]byte(`["spoiler"]`))
	_, err := svc.UpdateProfile("u1", user.UpdateInput{
		MutedWords:     &mw,
		HardMutedWords: &hmw,
	})
	require.NoError(t, err)

	got := repo.Profiles["u1"]
	require.NotNil(t, got)
	assert.JSONEq(t, `[["foo"],["bar","baz"]]`, string(got.MutedWords))
	assert.JSONEq(t, `["spoiler"]`, string(got.HardMutedWords))

	// 後続更新で `[]` を渡すと clear。
	clear := json.RawMessage([]byte(`[]`))
	_, err = svc.UpdateProfile("u1", user.UpdateInput{MutedWords: &clear})
	require.NoError(t, err)
	got = repo.Profiles["u1"]
	assert.JSONEq(t, `[]`, string(got.MutedWords))
	// hardMutedWords は omit されたので不変。
	assert.JSONEq(t, `["spoiler"]`, string(got.HardMutedWords))
}

// #1546: followingVisibility / followersVisibility の persist。enum 検証は
// ハンドラ側なので service は受け取った string をそのまま列に書く。
func TestService_UpdateProfile_FollowVisibility(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	repo.Profiles["u1"] = &model.UserProfile{
		UserID:              "u1",
		FollowingVisibility: model.FollowingVisibilityPublic,
		FollowersVisibility: model.FollowingVisibilityPublic,
	}

	following := "followers"
	followers := "private"
	_, err := svc.UpdateProfile("u1", user.UpdateInput{
		FollowingVisibility: &following,
		FollowersVisibility: &followers,
	})
	require.NoError(t, err)
	got := repo.Profiles["u1"]
	require.NotNil(t, got)
	assert.Equal(t, model.FollowingVisibilityFollowers, got.FollowingVisibility)
	assert.Equal(t, model.FollowingVisibilityPrivate, got.FollowersVisibility)

	// 片方だけ omit すると他方は不変。
	onlyFollowing := "private"
	_, err = svc.UpdateProfile("u1", user.UpdateInput{FollowingVisibility: &onlyFollowing})
	require.NoError(t, err)
	got = repo.Profiles["u1"]
	assert.Equal(t, model.FollowingVisibilityPrivate, got.FollowingVisibility)
	assert.Equal(t, model.FollowingVisibilityPrivate, got.FollowersVisibility, "followersVisibility は omit で不変")
}

// #1546: jsonb passthrough (notificationRecieveConfig / mutedInstances /
// emailNotificationTypes) と pinnedPageId / requireSigninToViewContents /
// makeNotes*Before の set / clear を core 層で固定する。
func TestService_UpdateProfile_ExtraParams(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}

	nrc := json.RawMessage(`{"mention":{"type":"following"}}`)
	mi := json.RawMessage(`["evil.example"]`)
	ent := json.RawMessage(`["mention"]`)
	pid := "pg1"
	pidPtr := &pid
	sig := true
	fob := 1700000000
	fobPtr := &fob
	_, err := svc.UpdateProfile("u1", user.UpdateInput{
		NotificationRecieveConfig:    &nrc,
		MutedInstances:               &mi,
		EmailNotificationTypes:       &ent,
		PinnedPageID:                 &pidPtr,
		RequireSigninToViewContents:  &sig,
		MakeNotesFollowersOnlyBefore: &fobPtr,
	})
	require.NoError(t, err)
	p := repo.Profiles["u1"]
	assert.JSONEq(t, `{"mention":{"type":"following"}}`, string(p.NotificationRecieveConfig))
	assert.JSONEq(t, `["evil.example"]`, string(p.MutedInstances))
	assert.JSONEq(t, `["mention"]`, string(p.EmailNotificationTypes))
	require.NotNil(t, p.PinnedPageID)
	assert.Equal(t, "pg1", *p.PinnedPageID)
	assert.True(t, repo.Users["u1"].RequireSigninToViewContents)
	require.NotNil(t, repo.Users["u1"].MakeNotesFollowersOnlyBefore)
	assert.Equal(t, 1700000000, *repo.Users["u1"].MakeNotesFollowersOnlyBefore)

	// clear: pinnedPageId / makeNotesFollowersOnlyBefore を null クリア。
	var clearPage *string
	var clearInt *int
	_, err = svc.UpdateProfile("u1", user.UpdateInput{
		PinnedPageID:                 &clearPage,
		MakeNotesFollowersOnlyBefore: &clearInt,
	})
	require.NoError(t, err)
	assert.Nil(t, repo.Profiles["u1"].PinnedPageID)
	assert.Nil(t, repo.Users["u1"].MakeNotesFollowersOnlyBefore)
}

// #956: profile fields (name/value 配列) の persist + 正規化。
// upstream Misskey TS は trim + 空 entry 排除を行うので、mk-go も同挙動に
// 揃える (= name または value どちらかが trim 後に空なら drop)。
func TestService_UpdateProfile_Fields(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	in := []user.FieldItem{
		{Name: "  blog  ", Value: " https://example.com  "},
		{Name: "empty-value", Value: ""},
		{Name: "", Value: "empty-name-only"},
		{Name: "  whitespace-name  ", Value: "  v  "},
		{Name: "x", Value: "y"},
	}
	_, err := svc.UpdateProfile("u1", user.UpdateInput{Fields: &in})
	require.NoError(t, err)

	got := repo.Profiles["u1"]
	require.NotNil(t, got)
	// 空 entry が drop され、name/value とも trim される。順序は維持。
	assert.JSONEq(t,
		`[{"name":"blog","value":"https://example.com"},{"name":"whitespace-name","value":"v"},{"name":"x","value":"y"}]`,
		string(got.Fields),
	)

	// nil (= 省略) は不変。Fields=[{x,y}, ...] 状態で他 field のみ更新
	// しても Fields が消えないことを pinpoint で verify する (= "Fields が
	// nil なら touch しない" 性質)。Description ptr-to-ptr で更新する。
	desc := "new description"
	descPtr := &desc
	_, err = svc.UpdateProfile("u1", user.UpdateInput{Description: &descPtr})
	require.NoError(t, err)
	got = repo.Profiles["u1"]
	assert.JSONEq(t,
		`[{"name":"blog","value":"https://example.com"},{"name":"whitespace-name","value":"v"},{"name":"x","value":"y"}]`,
		string(got.Fields),
	)
	require.NotNil(t, got.Description)
	assert.Equal(t, desc, *got.Description)

	// 後続更新で空 slice を渡すと clear (= [] 書き込み)。
	empty := []user.FieldItem{}
	_, err = svc.UpdateProfile("u1", user.UpdateInput{Fields: &empty})
	require.NoError(t, err)
	got = repo.Profiles["u1"]
	assert.JSONEq(t, `[]`, string(got.Fields))
}

// #467: avatarId に SET / CLEAR / 不明 ID / 他人ファイル / 非画像 MIME を
// 与えたときの挙動を確認する。banner も同経路 (applyMediaUpdate を共有)
// なので avatar 側で代表させる。
func TestService_UpdateProfile_AvatarSet(t *testing.T) {
	svc, userRepo, _, _ := newFullSvc(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	driveRepo := testutil.NewMockDriveFileRepository()
	owner := "u1"
	bh := "L6PZfSi_.AyE_3t7t7R**0o#DgR4"
	driveRepo.Files["f1"] = &model.DriveFile{
		ID: "f1", UserID: &owner, Type: "image/png",
		URL: "https://cdn.example/avatar.png", Blurhash: &bh,
	}
	svc.SetDriveFileRepository(driveRepo)

	id := "f1"
	bundle, err := svc.UpdateProfile("u1", user.UpdateInput{AvatarID: &id})
	require.NoError(t, err)
	require.NotNil(t, bundle.User.AvatarID)
	assert.Equal(t, "f1", *bundle.User.AvatarID)
	require.NotNil(t, bundle.User.AvatarURL)
	assert.Equal(t, "https://cdn.example/avatar.png", *bundle.User.AvatarURL)
	require.NotNil(t, bundle.User.AvatarBlurhash)
	assert.Equal(t, bh, *bundle.User.AvatarBlurhash)
}

func TestService_UpdateProfile_AvatarClear(t *testing.T) {
	svc, userRepo, _, _ := newFullSvc(t)
	existingID := "f0"
	existingURL := "https://cdn.example/old.png"
	existingBh := "old"
	userRepo.Users["u1"] = &model.User{
		ID: "u1", Username: "alice",
		AvatarID:       &existingID,
		AvatarURL:      &existingURL,
		AvatarBlurhash: &existingBh,
	}
	driveRepo := testutil.NewMockDriveFileRepository()
	svc.SetDriveFileRepository(driveRepo)

	empty := ""
	bundle, err := svc.UpdateProfile("u1", user.UpdateInput{AvatarID: &empty})
	require.NoError(t, err)
	assert.Nil(t, bundle.User.AvatarID)
	assert.Nil(t, bundle.User.AvatarURL)
	assert.Nil(t, bundle.User.AvatarBlurhash)
}

func TestService_UpdateProfile_AvatarNotFound(t *testing.T) {
	svc, userRepo, _, _ := newFullSvc(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	svc.SetDriveFileRepository(testutil.NewMockDriveFileRepository())

	id := "missing"
	_, err := svc.UpdateProfile("u1", user.UpdateInput{AvatarID: &id})
	assert.True(t, errors.Is(err, user.ErrAvatarNotFound))
}

func TestService_UpdateProfile_AvatarNotOwned(t *testing.T) {
	svc, userRepo, _, _ := newFullSvc(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	driveRepo := testutil.NewMockDriveFileRepository()
	otherOwner := "u2"
	driveRepo.Files["f1"] = &model.DriveFile{
		ID: "f1", UserID: &otherOwner, Type: "image/png", URL: "https://x/x",
	}
	svc.SetDriveFileRepository(driveRepo)

	id := "f1"
	_, err := svc.UpdateProfile("u1", user.UpdateInput{AvatarID: &id})
	// owner mismatch も notFound で扱う (upstream 互換 + 列挙攻撃避け)。
	assert.True(t, errors.Is(err, user.ErrAvatarNotFound))
}

func TestService_UpdateProfile_AvatarNotImage(t *testing.T) {
	svc, userRepo, _, _ := newFullSvc(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	driveRepo := testutil.NewMockDriveFileRepository()
	owner := "u1"
	driveRepo.Files["f1"] = &model.DriveFile{
		ID: "f1", UserID: &owner, Type: "video/mp4", URL: "https://x/v.mp4",
	}
	svc.SetDriveFileRepository(driveRepo)

	id := "f1"
	_, err := svc.UpdateProfile("u1", user.UpdateInput{AvatarID: &id})
	assert.True(t, errors.Is(err, user.ErrAvatarNotImage))
}

// #521: UpdateInput.AvatarDecorations が user.avatarDecorations カラムに
// jsonb (= string) として書き込まれることを確認する。
func TestService_UpdateProfile_AvatarDecorationsSet(t *testing.T) {
	svc, userRepo, _, _ := newFullSvc(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	raw := []byte(`[{"id":"dec1","angle":0,"flipH":false,"offsetX":0,"offsetY":0}]`)
	bundle, err := svc.UpdateProfile("u1", user.UpdateInput{AvatarDecorations: &raw})
	require.NoError(t, err)
	require.NotNil(t, bundle.User)
	assert.JSONEq(t, string(raw), string(userRepo.Users["u1"].AvatarDecorations))
}

func TestService_UpdateProfile_AvatarDecorationsClear(t *testing.T) {
	svc, userRepo, _, _ := newFullSvc(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", AvatarDecorations: []byte(`[{"id":"dec1"}]`)}

	empty := []byte(`[]`)
	_, err := svc.UpdateProfile("u1", user.UpdateInput{AvatarDecorations: &empty})
	require.NoError(t, err)
	assert.JSONEq(t, `[]`, string(userRepo.Users["u1"].AvatarDecorations))
}

// アイコン / バナーに設定した画像の URL は**原本ではなく公開用**でなければ
// ならない。原本は EXIF / XMP が載ったままで、`avatarUrl` はタイムライン・
// `users/show`・ActivityPub の actor icon に出るため、入れると撮影位置を含む
// 画像がそのまま公開される。upstream も `webpublicUrl ?? url` を通している。
func TestService_UpdateProfile_MediaUsesPublicURL(t *testing.T) {
	webpublic := "https://cdn.example/webpublic.webp"
	emptyWebpublic := ""

	cases := []struct {
		name       string
		file       *model.DriveFile
		wantAvatar string
	}{
		{
			name: "webpublic があればそちらを入れる",
			file: &model.DriveFile{
				Type:         "image/jpeg",
				URL:          "https://cdn.example/original.jpg",
				WebpublicURL: &webpublic,
			},
			wantAvatar: webpublic,
		},
		{
			name: "webpublic が無ければ原本に落ちる",
			file: &model.DriveFile{
				Type: "image/png",
				URL:  "https://cdn.example/original.png",
			},
			wantAvatar: "https://cdn.example/original.png",
		},
		{
			name: "webpublic が空文字なら原本に落ちる",
			file: &model.DriveFile{
				Type:         "image/png",
				URL:          "https://cdn.example/original.png",
				WebpublicURL: &emptyWebpublic,
			},
			wantAvatar: "https://cdn.example/original.png",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, userRepo, _, _ := newFullSvc(t)
			userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
			driveRepo := testutil.NewMockDriveFileRepository()
			owner := "u1"

			avatar := *tc.file
			avatar.ID = "f1"
			avatar.UserID = &owner
			driveRepo.Files["f1"] = &avatar

			// banner も同じ経路 (applyMediaUpdate 共有) なので同時に見る。
			banner := *tc.file
			banner.ID = "b1"
			banner.UserID = &owner
			driveRepo.Files["b1"] = &banner

			svc.SetDriveFileRepository(driveRepo)

			avatarID, bannerID := "f1", "b1"
			bundle, err := svc.UpdateProfile("u1", user.UpdateInput{
				AvatarID: &avatarID,
				BannerID: &bannerID,
			})
			require.NoError(t, err)

			require.NotNil(t, bundle.User.AvatarURL)
			assert.Equal(t, tc.wantAvatar, *bundle.User.AvatarURL, "avatarUrl")
			require.NotNil(t, bundle.User.BannerURL)
			assert.Equal(t, tc.wantAvatar, *bundle.User.BannerURL, "bannerUrl")

			// **原本が漏れていないこと**を直接見る。上の Equal だけだと、
			// 将来 want をうっかり原本に書き換えたときに気付けない。
			if tc.file.WebpublicURL != nil && *tc.file.WebpublicURL != "" {
				assert.NotEqual(t, tc.file.URL, *bundle.User.AvatarURL,
					"原本の URL を avatarUrl に入れてはいけない")
				assert.NotEqual(t, tc.file.URL, *bundle.User.BannerURL,
					"原本の URL を bannerUrl に入れてはいけない")
			}
		})
	}
}

func TestService_UpdateProfile_BannerSet(t *testing.T) {
	// banner は avatar と applyMediaUpdate 共有なので smoke test 1 件のみ。
	svc, userRepo, _, _ := newFullSvc(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	driveRepo := testutil.NewMockDriveFileRepository()
	owner := "u1"
	driveRepo.Files["b1"] = &model.DriveFile{
		ID: "b1", UserID: &owner, Type: "image/jpeg",
		URL: "https://cdn.example/banner.jpg",
	}
	svc.SetDriveFileRepository(driveRepo)

	id := "b1"
	bundle, err := svc.UpdateProfile("u1", user.UpdateInput{BannerID: &id})
	require.NoError(t, err)
	require.NotNil(t, bundle.User.BannerID)
	assert.Equal(t, "b1", *bundle.User.BannerID)
	require.NotNil(t, bundle.User.BannerURL)
	assert.Equal(t, "https://cdn.example/banner.jpg", *bundle.User.BannerURL)
}

func TestService_UpdateProfile_NoFields(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	_, err := svc.UpdateProfile("u1", user.UpdateInput{})
	require.NoError(t, err)
}

func TestService_UpdateProfile_UserNotFound(t *testing.T) {
	svc, _, _, _ := newFullSvc(t)
	_, err := svc.UpdateProfile("missing", user.UpdateInput{})
	assert.True(t, errors.Is(err, user.ErrUserNotFound))
}

// stubMainStreamPublisher captures PublishMainEvent calls for assertion.
type stubMainStreamPublisher struct {
	calls []mainEventCall
}

type mainEventCall struct {
	userID    string
	eventType string
	body      any
}

func (s *stubMainStreamPublisher) PublishMainEvent(userID, eventType string, body any) {
	s.calls = append(s.calls, mainEventCall{userID, eventType, body})
}

func TestService_UpdateProfile_PublishesMeUpdated(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)

	_, err := svc.UpdateProfile("u1", user.UpdateInput{
		Name:        ptr(ptr("New Name")),
		Description: ptr(ptr("hello")),
	})
	require.NoError(t, err)

	require.Len(t, pub.calls, 1)
	assert.Equal(t, "u1", pub.calls[0].userID)
	assert.Equal(t, "meUpdated", pub.calls[0].eventType)
	// body は entity.UserDetailed 相当。JSON round-trip で id/name/description
	// が更新後の値になっていることを確認する。
	raw, err := json.Marshal(pub.calls[0].body)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, "u1", m["id"])
	assert.Equal(t, "New Name", m["name"])
	assert.Equal(t, "hello", m["description"])
}

// #1558 meUpdated は本人の self event なので follower-only の followedMessage も
// 含む。PackUserDetailed が followedMessage を set しなくなった回帰を防ぐ。
func TestService_UpdateProfile_MeUpdatedCarriesFollowedMessage(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)

	msg := "thanks for following"
	_, err := svc.UpdateProfile("u1", user.UpdateInput{
		FollowedMessage: ptr(ptr(msg)),
	})
	require.NoError(t, err)

	require.Len(t, pub.calls, 1)
	raw, err := json.Marshal(pub.calls[0].body)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, msg, m["followedMessage"], "meUpdated body は本人の followedMessage を含む")
}

func TestService_UpdateProfile_UserUpdateError_DoesNotPublish(t *testing.T) {
	mockUR := testutil.NewMockUserRepository()
	mockUR.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	uRepo := &failingUserRepo{MockUserRepository: mockUR, failUpdateUser: true}
	idGen, _ := id.NewGenerator("aidx")
	svc := user.NewService(uRepo, nil, nil, idGen)
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)

	_, err := svc.UpdateProfile("u1", user.UpdateInput{IsLocked: ptr(true)})
	require.Error(t, err)
	assert.Empty(t, pub.calls)
}

// --- PinNote / UnpinNote / ListPinnedNotes ---

func TestService_PinNote_Success(t *testing.T) {
	svc, repo, noteRepo, piningRepo := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1"}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1"}

	require.NoError(t, svc.PinNote("u1", "n1"))
	assert.Len(t, piningRepo.Pinings, 1)
}

func TestService_PinNote_NoteNotFound(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1"}

	err := svc.PinNote("u1", "ghost")
	assert.True(t, errors.Is(err, user.ErrNoteNotFound))
}

func TestService_PinNote_NotOwner(t *testing.T) {
	svc, repo, noteRepo, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1"}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "other"}

	err := svc.PinNote("u1", "n1")
	assert.True(t, errors.Is(err, user.ErrNoteNotFound))
}

func TestService_PinNote_AlreadyPinned(t *testing.T) {
	svc, repo, noteRepo, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1"}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1"}
	require.NoError(t, svc.PinNote("u1", "n1"))

	err := svc.PinNote("u1", "n1")
	assert.True(t, errors.Is(err, user.ErrAlreadyPinned))
}

func TestService_PinNote_LimitExceeded(t *testing.T) {
	svc, repo, noteRepo, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1"}
	for i := 1; i <= user.MaxPinnedNotes; i++ {
		noteID := "n" + string(rune('0'+i))
		noteRepo.Notes[noteID] = &model.Note{ID: noteID, UserID: "u1"}
		require.NoError(t, svc.PinNote("u1", noteID))
	}

	noteRepo.Notes["n_extra"] = &model.Note{ID: "n_extra", UserID: "u1"}
	err := svc.PinNote("u1", "n_extra")
	assert.True(t, errors.Is(err, user.ErrPinLimitExceeded))
}

func TestService_UnpinNote_Success(t *testing.T) {
	svc, repo, noteRepo, piningRepo := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1"}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1"}
	require.NoError(t, svc.PinNote("u1", "n1"))

	require.NoError(t, svc.UnpinNote("u1", "n1"))
	assert.Empty(t, piningRepo.Pinings)
}

func TestService_UnpinNote_NotFound(t *testing.T) {
	svc, _, _, _ := newFullSvc(t)
	err := svc.UnpinNote("u1", "n1")
	assert.True(t, errors.Is(err, user.ErrPinNotFound))
}

func TestService_PinNote_PublishesMeUpdated(t *testing.T) {
	svc, repo, noteRepo, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1"}
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)

	require.NoError(t, svc.PinNote("u1", "n1"))
	require.Len(t, pub.calls, 1)
	assert.Equal(t, "u1", pub.calls[0].userID)
	assert.Equal(t, "meUpdated", pub.calls[0].eventType)
	raw, err := json.Marshal(pub.calls[0].body)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, "u1", m["id"])
	// piningRepo から補完された pinnedNoteIds が含まれること
	ids, ok := m["pinnedNoteIds"].([]any)
	require.True(t, ok)
	assert.Equal(t, []any{"n1"}, ids)
}

func TestService_PinNote_NoPublisher_NoDBRead(t *testing.T) {
	// publisher 未注入時は ShowByID を呼ばないことを確認するため、
	// pin 直後に user を repo から削除しても PinNote は error にならない
	// (ShowByID されないため)。
	svc, repo, noteRepo, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1"}
	// publisher を意図的に設定しない
	require.NoError(t, svc.PinNote("u1", "n1"))
}

func TestService_UnpinNote_PublishesMeUpdated(t *testing.T) {
	svc, repo, noteRepo, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1"}
	require.NoError(t, svc.PinNote("u1", "n1"))

	// Pin で emit された分を除外するため publisher を差し替え。
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)

	require.NoError(t, svc.UnpinNote("u1", "n1"))
	require.Len(t, pub.calls, 1)
	assert.Equal(t, "u1", pub.calls[0].userID)
	assert.Equal(t, "meUpdated", pub.calls[0].eventType)
}

func TestService_ListPinnedNotes_Success(t *testing.T) {
	svc, repo, noteRepo, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1"}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1"}
	noteRepo.Notes["n2"] = &model.Note{ID: "n2", UserID: "u1"}
	require.NoError(t, svc.PinNote("u1", "n1"))
	require.NoError(t, svc.PinNote("u1", "n2"))

	notes, err := svc.ListPinnedNotes("u1")
	require.NoError(t, err)
	assert.Len(t, notes, 2)
}

func TestService_ListPinnedNotes_Empty(t *testing.T) {
	svc, _, _, _ := newFullSvc(t)
	notes, err := svc.ListPinnedNotes("u1")
	require.NoError(t, err)
	assert.Empty(t, notes)
}

// --- Failing-repo error paths ---

var errStub = errors.New("stub error")

type failingUserRepo struct {
	*testutil.MockUserRepository
	failUpdateUser    bool
	failUpdateProfile bool
}

func (f *failingUserRepo) UpdateUser(userID string, fields map[string]any) error {
	if f.failUpdateUser {
		return errStub
	}
	return f.MockUserRepository.UpdateUser(userID, fields)
}

func (f *failingUserRepo) UpdateProfile(userID string, fields map[string]any) error {
	if f.failUpdateProfile {
		return errStub
	}
	return f.MockUserRepository.UpdateProfile(userID, fields)
}

type failingPiningRepo struct {
	*testutil.MockUserNotePiningRepository
	failCount   bool
	failListByU bool
}

func (f *failingPiningRepo) CountByUser(userID string) (int, error) {
	if f.failCount {
		return 0, errStub
	}
	return f.MockUserNotePiningRepository.CountByUser(userID)
}

func (f *failingPiningRepo) ListByUser(userID string) ([]*model.UserNotePining, error) {
	if f.failListByU {
		return nil, errStub
	}
	return f.MockUserNotePiningRepository.ListByUser(userID)
}

func TestService_UpdateProfile_UserUpdateError(t *testing.T) {
	mockUR := testutil.NewMockUserRepository()
	mockUR.Users["u1"] = &model.User{ID: "u1"}
	uRepo := &failingUserRepo{MockUserRepository: mockUR, failUpdateUser: true}
	idGen, _ := id.NewGenerator("aidx")
	svc := user.NewService(uRepo, testutil.NewMockNoteRepository(), testutil.NewMockUserNotePiningRepository(), idGen)

	_, err := svc.UpdateProfile("u1", user.UpdateInput{IsLocked: ptr(true)})
	assert.ErrorIs(t, err, errStub)
}

func TestService_UpdateProfile_ProfileUpdateError(t *testing.T) {
	mockUR := testutil.NewMockUserRepository()
	mockUR.Users["u1"] = &model.User{ID: "u1"}
	uRepo := &failingUserRepo{MockUserRepository: mockUR, failUpdateProfile: true}
	idGen, _ := id.NewGenerator("aidx")
	svc := user.NewService(uRepo, testutil.NewMockNoteRepository(), testutil.NewMockUserNotePiningRepository(), idGen)

	_, err := svc.UpdateProfile("u1", user.UpdateInput{Description: ptr(ptr("hi"))})
	assert.ErrorIs(t, err, errStub)
}

func TestService_PinNote_CountError(t *testing.T) {
	mockUR := testutil.NewMockUserRepository()
	mockUR.Users["u1"] = &model.User{ID: "u1"}
	mockNR := testutil.NewMockNoteRepository()
	mockNR.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1"}
	piningRepo := &failingPiningRepo{MockUserNotePiningRepository: testutil.NewMockUserNotePiningRepository(), failCount: true}
	idGen, _ := id.NewGenerator("aidx")
	svc := user.NewService(mockUR, mockNR, piningRepo, idGen)

	err := svc.PinNote("u1", "n1")
	assert.ErrorIs(t, err, errStub)
}

func TestService_ListPinnedNotes_Error(t *testing.T) {
	mockUR := testutil.NewMockUserRepository()
	piningRepo := &failingPiningRepo{MockUserNotePiningRepository: testutil.NewMockUserNotePiningRepository(), failListByU: true}
	idGen, _ := id.NewGenerator("aidx")
	svc := user.NewService(mockUR, testutil.NewMockNoteRepository(), piningRepo, idGen)

	_, err := svc.ListPinnedNotes("u1")
	assert.ErrorIs(t, err, errStub)
}

func TestUpdateUserFields(t *testing.T) {
	svc, userRepo, _, _ := newFullSvc(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "test"}
	err := svc.UpdateUserFields("u1", map[string]any{"isSuspended": true})
	require.NoError(t, err)
}

func TestUpdateProfileFields(t *testing.T) {
	svc, userRepo, _, _ := newFullSvc(t)
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}
	err := svc.UpdateProfileFields("u1", map[string]any{"description": "new"})
	require.NoError(t, err)
}

// #1546: UpdateInput.AlsoKnownAs は要素ありなら csv で user.alsoKnownAs に
// 書き込み、空 slice なら NULL クリアする。
func TestUpdateProfile_AlsoKnownAs(t *testing.T) {
	t.Run("non-empty writes csv", func(t *testing.T) {
		svc, userRepo, _, _ := newFullSvc(t)
		userRepo.Users["u1"] = &model.User{ID: "u1"}
		userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}
		aka := []string{"https://a.example/users/x", "https://b.example/users/y"}
		_, err := svc.UpdateProfile("u1", user.UpdateInput{AlsoKnownAs: &aka})
		require.NoError(t, err)
		got := userRepo.Users["u1"]
		require.NotNil(t, got.AlsoKnownAs)
		assert.Equal(t, "https://a.example/users/x,https://b.example/users/y", *got.AlsoKnownAs)
	})
	t.Run("empty slice clears", func(t *testing.T) {
		svc, userRepo, _, _ := newFullSvc(t)
		existing := "https://a.example/users/x"
		userRepo.Users["u1"] = &model.User{ID: "u1", AlsoKnownAs: &existing}
		userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}
		empty := []string{}
		_, err := svc.UpdateProfile("u1", user.UpdateInput{AlsoKnownAs: &empty})
		require.NoError(t, err)
		assert.Nil(t, userRepo.Users["u1"].AlsoKnownAs)
	})
}

// #766 / #1064: SearchByUsernameAndHost の各分岐を service レイヤーで覆う。
// upstream UserSearchService.generateUserQueryBuilder と同じく、host=falsy は
// filter 無し (local + remote)、host="." or self-hostname は local 限定、
// それ以外は host prefix match。
func TestService_SearchByUsernameAndHost(t *testing.T) {
	svc, userRepo, _, _ := newFullSvc(t)
	remoteHost := "remote.example"
	otherHost := "other.example"
	userRepo.Users["u_local"] = &model.User{ID: "u_local", Username: "alice", UsernameLower: "alice"}
	userRepo.Users["u_remote"] = &model.User{ID: "u_remote", Username: "alice", UsernameLower: "alice", Host: &remoteHost}
	userRepo.Users["u_other"] = &model.User{ID: "u_other", Username: "alice", UsernameLower: "alice", Host: &otherHost}

	// upstream の paramDef は username / host の anyOf なので、username 省略
	// (= host だけの検索) も有効。username 空文字は「username で絞らない」。
	t.Run("empty query searches without a username filter", func(t *testing.T) {
		out, err := svc.SearchByUsernameAndHost("", nil, 10)
		require.NoError(t, err)
		assert.Len(t, out, 3)
	})

	t.Run("@ prefix is stripped and host=nil returns local+remote", func(t *testing.T) {
		out, err := svc.SearchByUsernameAndHost("@alice", nil, 10)
		require.NoError(t, err)
		ids := make(map[string]bool, len(out))
		for _, u := range out {
			ids[u.ID] = true
		}
		assert.True(t, ids["u_local"], "@ prefix should strip and host=nil returns local")
		assert.True(t, ids["u_remote"], "host=nil should include remote")
		assert.True(t, ids["u_other"], "host=nil should include other host too")
	})

	// #1064: host=nil は upstream `params.host` falsy 経路で host filter なし
	// = local + remote 両方返す。旧来は local 限定だったが drift だった。
	t.Run("host=nil returns local and remote", func(t *testing.T) {
		out, err := svc.SearchByUsernameAndHost("alice", nil, 10)
		require.NoError(t, err)
		ids := make(map[string]bool, len(out))
		for _, u := range out {
			ids[u.ID] = true
		}
		assert.True(t, ids["u_local"])
		assert.True(t, ids["u_remote"])
		assert.True(t, ids["u_other"])
	})

	t.Run("host=empty string is treated as nil (no filter)", func(t *testing.T) {
		empty := ""
		out, err := svc.SearchByUsernameAndHost("alice", &empty, 10)
		require.NoError(t, err)
		ids := make(map[string]bool, len(out))
		for _, u := range out {
			ids[u.ID] = true
		}
		assert.True(t, ids["u_local"])
		assert.True(t, ids["u_remote"])
		assert.True(t, ids["u_other"])
	})

	// #1064: "." は upstream で local 限定への shortcut。
	t.Run("host=. returns only local", func(t *testing.T) {
		dot := "."
		out, err := svc.SearchByUsernameAndHost("alice", &dot, 10)
		require.NoError(t, err)
		require.Len(t, out, 1)
		assert.Equal(t, "u_local", out[0].ID)
	})

	// #1064: host が自 hostname と一致したら local 限定に remap される。
	// 各 selfHostname 関連 subtest は冒頭で明示的に初期状態を Set し、
	// `t.Cleanup` の実行順序に依存しない (= 個別 `-run` 実行や test reorder
	// 耐性のため)。
	t.Run("host=self-hostname returns only local", func(t *testing.T) {
		svc.SetSelfHostname("my.example")
		t.Cleanup(func() { svc.SetSelfHostname("") })
		self := "my.example"
		out, err := svc.SearchByUsernameAndHost("alice", &self, 10)
		require.NoError(t, err)
		require.Len(t, out, 1)
		assert.Equal(t, "u_local", out[0].ID)
	})

	// 自 hostname 比較は case-insensitive。
	t.Run("self-hostname remap is case-insensitive", func(t *testing.T) {
		svc.SetSelfHostname("My.Example")
		t.Cleanup(func() { svc.SetSelfHostname("") })
		upper := "MY.EXAMPLE"
		out, err := svc.SearchByUsernameAndHost("alice", &upper, 10)
		require.NoError(t, err)
		require.Len(t, out, 1)
		assert.Equal(t, "u_local", out[0].ID)
	})

	// SetSelfHostname は input を TrimSpace してから保存するので、whitespace
	// 込みで設定しても "." 一致のみ local 限定に degrade することは無く、
	// hostname だけ抽出されて正しく remap が機能する。
	t.Run("self-hostname is trimmed on set", func(t *testing.T) {
		svc.SetSelfHostname("  my.example  ")
		t.Cleanup(func() { svc.SetSelfHostname("") })
		self := "my.example"
		out, err := svc.SearchByUsernameAndHost("alice", &self, 10)
		require.NoError(t, err)
		require.Len(t, out, 1)
		assert.Equal(t, "u_local", out[0].ID)
	})

	// whitespace のみの input は空文字と同じ扱い (= remap disabled、"." 一致のみ
	// local 限定)。SetSelfHostname の TrimSpace が空にしたケースで remap が
	// 誤って効かないことを regression guard。
	t.Run("self-hostname whitespace-only is treated as unset", func(t *testing.T) {
		svc.SetSelfHostname("   ")
		t.Cleanup(func() { svc.SetSelfHostname("") })
		// 任意の host 文字列を与えても local 限定にならない (= host filter として動く)。
		other := "my.example"
		out, err := svc.SearchByUsernameAndHost("alice", &other, 10)
		require.NoError(t, err)
		// 仕込み user に `my.example` host を持つものは無いので 0 件。
		// remap が誤って効いて local 1 件返ったらこの assertion が落ちる。
		assert.Empty(t, out)
	})

	// selfHostname 未設定なら "." 以外の input は host filter として扱われる。
	// 前の subtest の cleanup に依存せず冒頭で明示的に "" を Set する。
	t.Run("self-hostname unset leaves other inputs as host filter", func(t *testing.T) {
		svc.SetSelfHostname("")
		other := "my.example"
		out, err := svc.SearchByUsernameAndHost("alice", &other, 10)
		require.NoError(t, err)
		// selfHostname 未設定なので "my.example" prefix match。仕込み user に
		// `my.example` host を持つものは無いので 0 件。
		assert.Empty(t, out)
	})

	t.Run("host=remoteHost narrows to host prefix match", func(t *testing.T) {
		out, err := svc.SearchByUsernameAndHost("alice", &remoteHost, 10)
		require.NoError(t, err)
		require.Len(t, out, 1)
		assert.Equal(t, "u_remote", out[0].ID)
	})

	t.Run("host comparison is case-insensitive", func(t *testing.T) {
		upper := "REMOTE.EXAMPLE"
		out, err := svc.SearchByUsernameAndHost("alice", &upper, 10)
		require.NoError(t, err)
		require.Len(t, out, 1)
		assert.Equal(t, "u_remote", out[0].ID)
	})

	// #1054 / #1060: host prefix match は維持される。`rem` で remote.example が hit。
	t.Run("host prefix matches remote host", func(t *testing.T) {
		prefix := "rem"
		out, err := svc.SearchByUsernameAndHost("alice", &prefix, 10)
		require.NoError(t, err)
		require.Len(t, out, 1)
		assert.Equal(t, "u_remote", out[0].ID)
	})

	t.Run("limit defaults to 10 when zero", func(t *testing.T) {
		// 11 件 local user を仕込んで limit 未指定で 10 に clamp されることを確認
		repo2 := testutil.NewMockUserRepository()
		for i := 0; i < 11; i++ {
			id := fmt.Sprintf("u_many_%02d", i)
			repo2.Users[id] = &model.User{ID: id, Username: "many", UsernameLower: "many"}
		}
		idGen, _ := id.NewGenerator("aidx")
		s2 := user.NewService(repo2, testutil.NewMockNoteRepository(), testutil.NewMockUserNotePiningRepository(), idGen)
		out, err := s2.SearchByUsernameAndHost("many", nil, 0)
		require.NoError(t, err)
		assert.Len(t, out, 10)
	})
}

// UpdateProfile で description 内の #tag が正規化 (lowercase) されて user.tags に
// 入る。hashtags/users の containment query 用 (#1360, Part 2)。
func TestService_UpdateProfile_PopulatesTags(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	bundle, err := svc.UpdateProfile("u1", user.UpdateInput{
		Description: ptr(ptr("I love #Golang and #ActivityPub")),
	})
	require.NoError(t, err)
	require.NotNil(t, bundle)
	assert.ElementsMatch(t, []string{"golang", "activitypub"}, []string(repo.Users["u1"].Tags))
}

// description をクリア (空文字) すると user.tags も空になる。
func TestService_UpdateProfile_ClearsTagsOnEmptyDescription(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", Tags: []string{"golang"}}

	_, err := svc.UpdateProfile("u1", user.UpdateInput{
		Description: ptr(ptr("")),
	})
	require.NoError(t, err)
	// tags は非nilの空配列 ('{}') でなければならない。nil の model.StringArray を
	// Updates() に渡すと SQL NULL になり user.tags (NOT NULL) 制約に違反して
	// profile 更新全体が atomic に失敗する (#1372)。
	require.NotNil(t, repo.Users["u1"].Tags)
	assert.Empty(t, []string(repo.Users["u1"].Tags))
}

// description を更新しない場合は user.tags を触らない。
func TestService_UpdateProfile_LeavesTagsWhenDescriptionOmitted(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", Tags: []string{"golang"}}

	_, err := svc.UpdateProfile("u1", user.UpdateInput{
		Name: ptr(ptr("Alice")),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"golang"}, []string(repo.Users["u1"].Tags))
}

// stubUsertagHook records UpdateUsertags invocations for the local populate
// hook test (#1362).
type stubUsertagHook struct {
	called  bool
	userID  string
	isLocal bool
	oldTags []string
	newTags []string
}

func (h *stubUsertagHook) UpdateUsertags(userID string, isLocal bool, oldTags, newTags []string) {
	h.called = true
	h.userID = userID
	h.isLocal = isLocal
	h.oldTags = oldTags
	h.newTags = newTags
}

// description 変更時、usertagHook へ old/new tags が渡る (local=isLocal true)。
func TestService_UpdateProfile_CallsUsertagHook(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", Tags: []string{"old"}}
	hook := &stubUsertagHook{}
	svc.SetUsertagHook(hook)

	_, err := svc.UpdateProfile("u1", user.UpdateInput{
		Description: ptr(ptr("now #Golang")),
	})
	require.NoError(t, err)
	require.True(t, hook.called)
	assert.Equal(t, "u1", hook.userID)
	assert.True(t, hook.isLocal)
	assert.Equal(t, []string{"old"}, hook.oldTags)
	assert.Equal(t, []string{"golang"}, hook.newTags)
}

// description を更新しない場合は hook を呼ばない。
func TestService_UpdateProfile_SkipsUsertagHookWhenDescriptionOmitted(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", Tags: []string{"old"}}
	hook := &stubUsertagHook{}
	svc.SetUsertagHook(hook)

	_, err := svc.UpdateProfile("u1", user.UpdateInput{Name: ptr(ptr("Alice"))})
	require.NoError(t, err)
	assert.False(t, hook.called)
}

// ShowByUsernameDB は resolver が注入されていても**絶対に呼ばない** (#2506)。
// 公開 AP route (/@:acct) から使うため、DB miss が未認証 GET 起点の
// WebFinger + actor fetch に化けてはならない。
func TestService_ShowByUsernameDB_NeverResolves(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	svc := user.NewService(repo, nil, nil, nil)
	resolver := &stubRemoteResolver{}
	svc.SetRemoteUserResolver(resolver)

	host := "remote.example"
	_, err := svc.ShowByUsernameDB("ghost", &host)
	require.ErrorIs(t, err, user.ErrUserNotFound)
	assert.Empty(t, resolver.calls, "DB miss で resolver を呼ばない")
}

// DB にあるものは host 付きでもそのまま返す。
func TestService_ShowByUsernameDB_FindsCachedRemote(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	host := "remote.example"
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", UsernameLower: "alice", Host: &host}
	svc := user.NewService(repo, nil, nil, nil)

	got, err := svc.ShowByUsernameDB("alice", &host)
	require.NoError(t, err)
	assert.Equal(t, "u1", got.User.ID)
}

// recordingResolver captures what host the remote fallback was asked for.
type recordingResolver struct {
	calls []string
	user  *model.User
	err   error
}

func (r *recordingResolver) ResolveByUsernameHost(username, host string) (*model.User, error) {
	r.calls = append(r.calls, username+"@"+host)
	return r.user, r.err
}

// **IDN ホストの acct 解決** (#2704)。
//
// 引く側は Unicode で来ることがある (フロントの mention リンクは
// `toUnicode(host)` で URL を組む)。正規化しないと「通知からは開けるのに
// メンションからは開けない」という形で出る。引く前に正規化し、正規形の完全一致で
// 当てる (repository の hostMatch、#2996)。
func TestShowByUsername_IDNHost(t *testing.T) {
	const puny = "xn--eckve.example"

	newSvc := func() (*user.Service, *recordingResolver) {
		repo := testutil.NewMockUserRepository()
		host := puny
		uri := "https://" + puny + "/users/idn"
		repo.Users["idn1"] = &model.User{
			ID: "idn1", Username: "IdnUser", UsernameLower: "idnuser", Host: &host, URI: &uri,
		}
		svc := user.NewService(repo, nil, nil, nil)
		res := &recordingResolver{}
		svc.SetRemoteUserResolver(res)
		return svc, res
	}

	for _, tc := range []struct {
		name string
		host string
	}{
		{"punycode", puny},
		{"unicode", "パイ.example"},
		{"unicode uppercase ascii tail", "パイ.EXAMPLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, res := newSvc()
			h := tc.host
			got, err := svc.ShowByUsername("idnuser", &h)
			require.NoError(t, err, "host=%q で引けること", tc.host)
			assert.Equal(t, "idn1", got.User.ID)
			// **存在するユーザーで WebFinger へ落ちないこと。** 落ちると
			// ページを開くたびに outbound の名前解決 + actor fetch が走る。
			assert.Empty(t, res.calls, "DB に居るのにリモート解決へ落ちないこと")
		})
	}

	t.Run("未知の acct は punycode でリモートに問い合わせる", func(t *testing.T) {
		svc, res := newSvc()
		res.err = errors.New("not found")
		h := "パイ.example"
		_, err := svc.ShowByUsername("nobody", &h)
		require.Error(t, err)
		require.Len(t, res.calls, 1)
		assert.Equal(t, "nobody@"+puny, res.calls[0],
			"WebFinger の宛先も punycode へ揃えること")
	})

	t.Run("引数を書き換えないこと", func(t *testing.T) {
		svc, _ := newSvc()
		h := "パイ.example"
		_, _ = svc.ShowByUsername("idnuser", &h)
		assert.Equal(t, "パイ.example", h)
	})
}

// **自ホストの acct は WebFinger を踏まずローカル行へ短絡すること** (#2704 review
// MEDIUM-2)。upstream も toPuny を掛けた host を `toPuny(config.host)` と比べる
// (RemoteUserResolveService.ts:59)。片側だけだと、`url` を Unicode IDN で書いた
// instance で短絡が効かなくなり、自分自身への WebFinger 往復を誘発する。
func TestShowByUsername_LocalHostShortCircuit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		localHost string
		asked     string
	}{
		{"ascii", "local.example", "local.example"},
		{"ascii uppercase", "local.example", "LOCAL.EXAMPLE"},
		{"unicode local host, unicode ask", "パイ.example", "パイ.example"},
		{"unicode local host, punycode ask", "パイ.example", "xn--eckve.example"},
		{"punycode local host, unicode ask", "xn--eckve.example", "パイ.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			repo.Users["me"] = &model.User{ID: "me", Username: "Alice", UsernameLower: "alice"}
			svc := user.NewService(repo, nil, nil, nil)
			// resolver は「呼ばれたら失敗する」ので、短絡できないと error になる。
			svc.SetRemoteUserResolver(federation.NewRemoteUserResolver(nil, nil, repo, tc.localHost))

			h := tc.asked
			got, err := svc.ShowByUsername("alice", &h)
			require.NoError(t, err, "localHost=%q に対し host=%q が短絡すること", tc.localHost, tc.asked)
			assert.Equal(t, "me", got.User.ID)
		})
	}
}

// **非正規化で保存された行は主経路 (users/show) から引けなくなる (#2996)。**
//
// 生の形にも当てる互換経路を撤去したので、backfill 前の行 (#2706 以前の
// `hostFromURI` は `url.Parse(uri).Host` をそのまま入れる) は DB から引けず、
// **リモート解決 (WebFinger) へ落ちる**。
//
// **行は増えない** — 解決先の actor URI は変わらないので `ResolveActor` の
// `FindByURI` が既存行に当たる。増えるのは**呼ばれるたびの外向きリクエスト**の
// ほうで、`LookupActorURI` にキャッシュは無い。「引けない」ではなく「WebFinger へ
// 落ちる」ところまで固定するのが要点。
// アップグレード前に `elythia backfill remote-host` を流す前提 (docs/deployment.md)。
func TestShowByUsername_NonNormalizedStoredHostFallsBackToRemote(t *testing.T) {
	for _, stored := range []string{"Mixed.Example", "XN--ECKVE.EXAMPLE"} {
		t.Run(stored, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			h := stored
			repo.Users["u1"] = &model.User{
				ID: "u1", Username: "Alice", UsernameLower: "alice", Host: &h,
			}
			svc := user.NewService(repo, nil, nil, nil)
			res := &recordingResolver{err: errors.New("not resolvable in test")}
			svc.SetRemoteUserResolver(res)

			q := stored
			_, err := svc.ShowByUsername("alice", &q)
			require.Error(t, err)
			assert.NotEmpty(t, res.calls, "DB で引けないのにリモート解決へ落ちていない")
		})
	}
}

// **DB 障害を DB miss として扱わない (#2792 / #2799)。**
//
// `ShowByUsername` は「引けなかった」ことをリモート解決の合図に使うので、接続断の
// ような一過性の障害を not-found に丸めると**外向きリクエストに化ける**。
// `users/show` は未認証でも叩けるため、DB が不調なあいだ外部から任意に焚き付け
// られる形になる。**リモート解決を撃たないこと**まで固定するのが要点。
func TestShowByUsername_DBErrorIsNotTreatedAsMiss(t *testing.T) {
	boom := errors.New("connection reset by peer")
	repo := testutil.NewMockUserRepository()
	repo.FindByUsernameLowerFn = func(string, *string) (*model.User, error) { return nil, boom }
	svc := user.NewService(repo, nil, nil, nil)
	res := &recordingResolver{}
	svc.SetRemoteUserResolver(res)

	h := "remote.example"
	_, err := svc.ShowByUsername("alice", &h)
	require.ErrorIs(t, err, boom, "DB のエラーをそのまま返すこと")
	assert.Empty(t, res.calls, "DB 障害でリモート解決を撃たないこと")
}

// RemoveBackupCode は repo へそのまま委譲する (#2852)。
//
// **消費を DB 側の array_remove に移した経路。** service が引数を落とすと
// 「消したはずのコードが残る」形で壊れるので、委譲そのものを固定する。
func TestService_RemoveBackupCode(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	var gotUser, gotCode string
	repo.RemoveBackupCodeFn = func(userID, code string) error {
		gotUser, gotCode = userID, code
		return nil
	}
	idGen, _ := id.NewGenerator("aidx")
	svc := user.NewService(repo, testutil.NewMockNoteRepository(),
		testutil.NewMockUserNotePiningRepository(), idGen)

	require.NoError(t, svc.RemoveBackupCode("u1", "c2"))
	assert.Equal(t, "u1", gotUser)
	assert.Equal(t, "c2", gotCode)
}

// **self event は本人のカウントを載せること。**
//
// packer は `followersVisibility` / `followingVisibility` が public でない
// カウントを既定で伏せる (呼び忘れても漏れないようにするため)。self event で
// ゲートを通さないと 0 が流れ、fork frontend は `meUpdated` を `$i` にそのまま
// merge するので**プロフィール更新や pin のたびに自分のフォロー数の表示が 0 に
// 化ける** (リロードまで戻らない)。
func TestService_UpdateProfile_MeUpdatedCarriesOwnCounts(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{
		ID: "u1", Username: "alice",
		FollowersCount: 42, FollowingCount: 7,
	}
	repo.Profiles["u1"] = &model.UserProfile{
		UserID: "u1", FollowersVisibility: "followers", FollowingVisibility: "private",
	}
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)

	_, err := svc.UpdateProfile("u1", user.UpdateInput{Name: ptr(ptr("Alice"))})
	require.NoError(t, err)

	require.Len(t, pub.calls, 1)
	raw, err := json.Marshal(pub.calls[0].body)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	// 期待値はリテラル。非公開の設定でも本人には実数が出る。
	assert.EqualValues(t, 42, m["followersCount"], "本人の meUpdated にフォロワー数が出ること")
	assert.EqualValues(t, 7, m["followingCount"], "本人の meUpdated にフォロー数が出ること")
}

// #3270: i/update は本家と同じく、名前・プロフィール・補足情報・フォローされた
// ときのメッセージから絵文字を取り出して user.emojis に書く。Person の `tag` の
// 絵文字はこの列から描画するので、書かないと連合先で `:emoji:` が文字のまま出る。
func TestService_UpdateProfile_WritesEmojis(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	_, err := svc.UpdateProfile("u1", user.UpdateInput{
		Name:            ptr(ptr("Alice :cat: :cat:")),
		Description:     ptr(ptr("hello :dog: and :cat:")),
		Fields:          &[]user.FieldItem{{Name: ":key:", Value: "v :val:"}},
		FollowedMessage: ptr(ptr("thanks :wave:")),
	})
	require.NoError(t, err)
	// 項目ごとに重複を除き、項目をまたいでは除かない (本家と同じ)。
	assert.Equal(t, []string{"cat", "dog", "cat", "key", "val", "wave"}, []string(repo.Users["u1"].Emojis))
}

// 名前と補足情報は parseSimple、プロフィールとフォローされたときのメッセージは
// 完全な parser で読む (本家と同じ)。コードの中の `:emoji:` は後者では数えない。
func TestService_UpdateProfile_EmojisParsedLikeUpstream(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	_, err := svc.UpdateProfile("u1", user.UpdateInput{
		Name:            ptr(ptr("`:inname:`")),
		Description:     ptr(ptr("`:indesc:`")),
		Fields:          &[]user.FieldItem{{Name: "`:inkey:`", Value: "`:inval:`"}},
		FollowedMessage: ptr(ptr("`:inmsg:`")),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"inname", "inkey", "inval"}, []string(repo.Users["u1"].Emojis))
}

// 送らなかった項目は保存済みの値を使い、どの項目を更新しても毎回書き直す (本家と
// 同じ)。名前だけを変えても、プロフィールの絵文字は残る。
func TestService_UpdateProfile_EmojisUseStoredValuesForOmittedFields(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", Emojis: model.StringArray{"stale"}}
	_, err := svc.UpdateProfile("u1", user.UpdateInput{
		Description:     ptr(ptr("desc :dog:")),
		Fields:          &[]user.FieldItem{{Name: "k", Value: ":val:"}},
		FollowedMessage: ptr(ptr("thanks :wave:")),
	})
	require.NoError(t, err)

	_, err = svc.UpdateProfile("u1", user.UpdateInput{Name: ptr(ptr(":cat:"))})
	require.NoError(t, err)
	assert.Equal(t, []string{"cat", "dog", "val", "wave"}, []string(repo.Users["u1"].Emojis))

	// 絵文字を使わなくなったら空にする (NULL ではなく空配列)。
	_, err = svc.UpdateProfile("u1", user.UpdateInput{
		Name:            ptr(ptr("plain")),
		Description:     ptr(ptr("")),
		Fields:          &[]user.FieldItem{},
		FollowedMessage: ptr(ptr("")),
	})
	require.NoError(t, err)
	require.NotNil(t, repo.Users["u1"].Emojis)
	assert.Empty(t, []string(repo.Users["u1"].Emojis))
}

// プロフィールを読めない DB 障害は、更新ごと失敗させる (絵文字を空で書き直すと
// 連合先から絵文字が消える)。行が無いだけなら保存値が無いものとして続ける。
func TestService_UpdateProfile_EmojisProfileLookupErrorFails(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", Emojis: model.StringArray{"keep"}}
	repo.FindProfileErr = errors.New("connection reset")

	_, err := svc.UpdateProfile("u1", user.UpdateInput{Name: ptr(ptr("x"))})
	require.Error(t, err)
	assert.Equal(t, []string{"keep"}, []string(repo.Users["u1"].Emojis), "書き換えない")
}
