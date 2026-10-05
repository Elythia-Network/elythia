package channel_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/channel"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSvc(t *testing.T) (
	*channel.Service,
	*testutil.MockChannelRepository,
	*testutil.MockChannelFollowingRepository,
	*testutil.MockNoteRepository,
) {
	t.Helper()
	repo := testutil.NewMockChannelRepository()
	followRepo := testutil.NewMockChannelFollowingRepository()
	noteRepo := testutil.NewMockNoteRepository()
	idGen, _ := id.NewGenerator("aidx")
	return channel.NewService(repo, followRepo, noteRepo, idGen), repo, followRepo, noteRepo
}

// --- Create ----------------------------------------------------------------

func TestCreate_HappyPath(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	c, err := svc.Create(channel.CreateInput{OwnerID: "u1", Name: "alpha"})
	require.NoError(t, err)
	assert.Equal(t, "alpha", c.Name)
	assert.Equal(t, "#86b300", c.Color)
	assert.Len(t, repo.Channels, 1)
}

func TestCreate_CustomColor(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	c, err := svc.Create(channel.CreateInput{OwnerID: "u1", Name: "alpha", Color: "#ff0000"})
	require.NoError(t, err)
	assert.Equal(t, "#ff0000", c.Color)
}

func TestCreate_NameRequired(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	_, err := svc.Create(channel.CreateInput{OwnerID: "u1"})
	assert.ErrorIs(t, err, channel.ErrChannelNameRequired)
}

func TestCreate_OwnerRequired(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	_, err := svc.Create(channel.CreateInput{Name: "alpha"})
	assert.Error(t, err)
}

func TestCreate_RepoError(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.CreateErr = errors.New("boom")
	_, err := svc.Create(channel.CreateInput{OwnerID: "u1", Name: "alpha"})
	assert.Error(t, err)
}

// --- Show ------------------------------------------------------------------

func TestShow_HappyPath(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha"}
	got, err := svc.Show("c1")
	require.NoError(t, err)
	assert.Equal(t, "alpha", got.Name)
}

func TestShow_NotFound(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	_, err := svc.Show("missing")
	assert.ErrorIs(t, err, channel.ErrChannelNotFound)
}

// --- Update ----------------------------------------------------------------

func TestUpdate_HappyPath(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	owner := "u1"
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha", UserID: &owner, Color: "#86b300"}
	newName := "alpha-v2"
	desc := "new"
	descPtr := &desc
	color := "#000"
	archived := true
	sensitive := true
	got, err := svc.Update("u1", "c1", channel.UpdateInput{
		Name:        &newName,
		Description: &descPtr,
		Color:       &color,
		IsArchived:  &archived,
		IsSensitive: &sensitive,
	})
	require.NoError(t, err)
	assert.Equal(t, "alpha-v2", got.Name)
}

func TestUpdate_NotFound(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	_, err := svc.Update("u1", "missing", channel.UpdateInput{})
	assert.ErrorIs(t, err, channel.ErrChannelNotFound)
}

func TestUpdate_AccessDenied(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	owner := "u1"
	repo.Channels["c1"] = &model.Channel{ID: "c1", UserID: &owner}
	_, err := svc.Update("u2", "c1", channel.UpdateInput{})
	assert.ErrorIs(t, err, channel.ErrAccessDenied)
}

func TestUpdate_NameEmpty(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	owner := "u1"
	repo.Channels["c1"] = &model.Channel{ID: "c1", UserID: &owner}
	empty := ""
	_, err := svc.Update("u1", "c1", channel.UpdateInput{Name: &empty})
	assert.ErrorIs(t, err, channel.ErrChannelNameRequired)
}

func TestUpdate_NoOwner(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"} // UserID nil
	_, err := svc.Update("u1", "c1", channel.UpdateInput{})
	assert.ErrorIs(t, err, channel.ErrAccessDenied)
}

func TestUpdate_RepoError(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	owner := "u1"
	repo.Channels["c1"] = &model.Channel{ID: "c1", UserID: &owner}
	repo.UpdateErr = errors.New("boom")
	name := "x"
	_, err := svc.Update("u1", "c1", channel.UpdateInput{Name: &name})
	assert.Error(t, err)
}

// --- Create / Update: allowRenoteToExternal --------------------------------

func TestCreate_AllowRenoteToExternal_DefaultTrue(t *testing.T) {
	// nil は upstream の `?? true` 既定に従う。
	svc, _, _, _ := newSvc(t)
	c, err := svc.Create(channel.CreateInput{OwnerID: "u1", Name: "alpha"})
	require.NoError(t, err)
	assert.True(t, c.AllowRenoteToExternal)
}

func TestCreate_AllowRenoteToExternal_ExplicitFalse(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	no := false
	c, err := svc.Create(channel.CreateInput{OwnerID: "u1", Name: "alpha", AllowRenoteToExternal: &no})
	require.NoError(t, err)
	assert.False(t, c.AllowRenoteToExternal)
}

func TestCreate_AllowRenoteToExternal_ExplicitTrue(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	yes := true
	c, err := svc.Create(channel.CreateInput{OwnerID: "u1", Name: "alpha", AllowRenoteToExternal: &yes})
	require.NoError(t, err)
	assert.True(t, c.AllowRenoteToExternal)
}

func TestUpdate_AllowRenoteToExternal_Set(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	owner := "u1"
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha", UserID: &owner, AllowRenoteToExternal: true}
	no := false
	got, err := svc.Update("u1", "c1", channel.UpdateInput{AllowRenoteToExternal: &no})
	require.NoError(t, err)
	assert.False(t, got.AllowRenoteToExternal)
}

func TestUpdate_AllowRenoteToExternal_NilLeavesUnchanged(t *testing.T) {
	// nil のときは無変更 (upstream は typeof boolean のときだけ更新)。
	svc, repo, _, _ := newSvc(t)
	owner := "u1"
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha", UserID: &owner, AllowRenoteToExternal: true}
	name := "alpha-v2"
	got, err := svc.Update("u1", "c1", channel.UpdateInput{Name: &name})
	require.NoError(t, err)
	assert.True(t, got.AllowRenoteToExternal)
}

// --- Update: moderator bypass ----------------------------------------------

// stubModerator is a ModeratorChecker test double.
type stubModerator struct {
	moderators map[string]bool
}

func (s stubModerator) IsModerator(userID string) bool { return s.moderators[userID] }

func TestUpdate_ModeratorCanEditOthersChannel(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	svc.SetModeratorChecker(stubModerator{moderators: map[string]bool{"mod": true}})
	owner := "u1"
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha", UserID: &owner}
	name := "edited-by-mod"
	got, err := svc.Update("mod", "c1", channel.UpdateInput{Name: &name})
	require.NoError(t, err)
	assert.Equal(t, "edited-by-mod", got.Name)
}

func TestUpdate_OwnerStillEditsWithModeratorChecker(t *testing.T) {
	// moderatorChecker 配線後も owner は引き続き編集できる。
	svc, repo, _, _ := newSvc(t)
	svc.SetModeratorChecker(stubModerator{moderators: map[string]bool{"mod": true}})
	owner := "u1"
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha", UserID: &owner}
	name := "edited-by-owner"
	got, err := svc.Update("u1", "c1", channel.UpdateInput{Name: &name})
	require.NoError(t, err)
	assert.Equal(t, "edited-by-owner", got.Name)
}

func TestUpdate_NonModeratorNonOwnerDenied(t *testing.T) {
	// fail-closed: moderator でも owner でもない第三者は弾く。
	svc, repo, _, _ := newSvc(t)
	svc.SetModeratorChecker(stubModerator{moderators: map[string]bool{"mod": true}})
	owner := "u1"
	repo.Channels["c1"] = &model.Channel{ID: "c1", UserID: &owner}
	name := "x"
	_, err := svc.Update("intruder", "c1", channel.UpdateInput{Name: &name})
	assert.ErrorIs(t, err, channel.ErrAccessDenied)
}

func TestUpdate_NoModeratorCheckerFallsBackToOwnerOnly(t *testing.T) {
	// moderatorChecker 未配線なら owner-only にフォールバックする。
	svc, repo, _, _ := newSvc(t)
	owner := "u1"
	repo.Channels["c1"] = &model.Channel{ID: "c1", UserID: &owner}
	name := "x"
	_, err := svc.Update("mod", "c1", channel.UpdateInput{Name: &name})
	assert.ErrorIs(t, err, channel.ErrAccessDenied)
}

// --- Follow / Unfollow -----------------------------------------------------

func TestFollow_HappyPath(t *testing.T) {
	svc, repo, followRepo, _ := newSvc(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	require.NoError(t, svc.Follow("u1", "c1"))
	assert.Len(t, followRepo.Followings, 1)
	assert.Equal(t, 1, repo.Channels["c1"].UsersCount)
}

func TestFollow_ChannelNotFound(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	err := svc.Follow("u1", "missing")
	assert.ErrorIs(t, err, channel.ErrChannelNotFound)
}

func TestFollow_AlreadyFollowing(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	require.NoError(t, svc.Follow("u1", "c1"))
	err := svc.Follow("u1", "c1")
	assert.ErrorIs(t, err, channel.ErrAlreadyFollowing)
}

func TestUnfollow_HappyPath(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	require.NoError(t, svc.Follow("u1", "c1"))
	require.NoError(t, svc.Unfollow("u1", "c1"))
	assert.Equal(t, 0, repo.Channels["c1"].UsersCount)
}

func TestUnfollow_NotFollowing(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	err := svc.Unfollow("u1", "c1")
	assert.ErrorIs(t, err, channel.ErrNotFollowing)
}

// failingFollowRepo causes Exists / Create / Delete to fail.
type failingFollowRepo struct {
	*testutil.MockChannelFollowingRepository
	existsErr error
	createErr error
	deleteErr error
}

func (f *failingFollowRepo) Exists(followerID, channelID string) (bool, error) {
	if f.existsErr != nil {
		return false, f.existsErr
	}
	return f.MockChannelFollowingRepository.Exists(followerID, channelID)
}
func (f *failingFollowRepo) Create(fw *model.ChannelFollowing) error {
	if f.createErr != nil {
		return f.createErr
	}
	return f.MockChannelFollowingRepository.Create(fw)
}
func (f *failingFollowRepo) Delete(fw *model.ChannelFollowing) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return f.MockChannelFollowingRepository.Delete(fw)
}

func TestFollow_ExistsError(t *testing.T) {
	repo := testutil.NewMockChannelRepository()
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	followRepo := &failingFollowRepo{
		MockChannelFollowingRepository: testutil.NewMockChannelFollowingRepository(),
		existsErr:                      errors.New("exists boom"),
	}
	idGen, _ := id.NewGenerator("aidx")
	svc := channel.NewService(repo, followRepo, testutil.NewMockNoteRepository(), idGen)
	err := svc.Follow("u1", "c1")
	assert.Error(t, err)
}

func TestFollow_CreateError(t *testing.T) {
	repo := testutil.NewMockChannelRepository()
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	followRepo := &failingFollowRepo{
		MockChannelFollowingRepository: testutil.NewMockChannelFollowingRepository(),
		createErr:                      errors.New("create boom"),
	}
	idGen, _ := id.NewGenerator("aidx")
	svc := channel.NewService(repo, followRepo, testutil.NewMockNoteRepository(), idGen)
	err := svc.Follow("u1", "c1")
	assert.Error(t, err)
}

func TestUnfollow_DeleteError(t *testing.T) {
	repo := testutil.NewMockChannelRepository()
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	mock := testutil.NewMockChannelFollowingRepository()
	mock.Followings["f1"] = &model.ChannelFollowing{ID: "f1", FollowerID: "u1", FolloweeID: "c1"}
	followRepo := &failingFollowRepo{
		MockChannelFollowingRepository: mock,
		deleteErr:                      errors.New("delete boom"),
	}
	idGen, _ := id.NewGenerator("aidx")
	svc := channel.NewService(repo, followRepo, testutil.NewMockNoteRepository(), idGen)
	err := svc.Unfollow("u1", "c1")
	assert.Error(t, err)
}

// --- Listing ---------------------------------------------------------------

func TestListFollowed(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha"}
	repo.Channels["c2"] = &model.Channel{ID: "c2", Name: "beta"}
	require.NoError(t, svc.Follow("u1", "c1"))
	require.NoError(t, svc.Follow("u1", "c2"))

	rows, err := svc.ListFollowed("u1", "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

// brokenChannelRepo makes FindByIDs fail so ListFollowed propagates the
// error instead of returning a partial list.
type brokenChannelRepo struct {
	*testutil.MockChannelRepository
}

func (r *brokenChannelRepo) FindByIDs(_ []string) ([]*model.Channel, error) {
	return nil, errors.New("boom")
}

// TestListFollowed_FindByIDsFailure verifies that ListFollowed propagates
// the FindByIDs error (not silent skip). 8th-pass refactor switched from
// per-row FindByID with continue-on-error to batch FindByIDs, so the
// failure mode now surfaces to the caller (more correct than hiding DB
// errors).
func TestListFollowed_FindByIDsFailure(t *testing.T) {
	mock := testutil.NewMockChannelRepository()
	repo := &brokenChannelRepo{MockChannelRepository: mock}
	followRepo := testutil.NewMockChannelFollowingRepository()
	followRepo.Followings["f1"] = &model.ChannelFollowing{ID: "f1", FollowerID: "u1", FolloweeID: "c1"}
	idGen, _ := id.NewGenerator("aidx")
	svc := channel.NewService(repo, followRepo, testutil.NewMockNoteRepository(), idGen)

	_, err := svc.ListFollowed("u1", "", "", 10, 0)
	require.Error(t, err)
}

// TestListFollowed_MissingChannelSkipped verifies that channels missing
// from the FindByIDs result (e.g., deleted) are silently skipped while
// other rows are returned normally.
func TestListFollowed_MissingChannelSkipped(t *testing.T) {
	repo := testutil.NewMockChannelRepository()
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha"}
	// c_missing is followed but not in repo (deleted)
	followRepo := testutil.NewMockChannelFollowingRepository()
	followRepo.Followings["f1"] = &model.ChannelFollowing{ID: "f1", FollowerID: "u1", FolloweeID: "c1"}
	followRepo.Followings["f2"] = &model.ChannelFollowing{ID: "f2", FollowerID: "u1", FolloweeID: "c_missing"}
	idGen, _ := id.NewGenerator("aidx")
	svc := channel.NewService(repo, followRepo, testutil.NewMockNoteRepository(), idGen)

	rows, err := svc.ListFollowed("u1", "", "", 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "c1", rows[0].ID)
}

// listFailFollowRepo causes ListFollowed to fail.
type listFailFollowRepo struct {
	*testutil.MockChannelFollowingRepository
}

func (r *listFailFollowRepo) ListFollowed(_, _, _ string, _, _ int) ([]*model.ChannelFollowing, error) {
	return nil, errors.New("list boom")
}

func TestListFollowed_ListError(t *testing.T) {
	repo := testutil.NewMockChannelRepository()
	followRepo := &listFailFollowRepo{MockChannelFollowingRepository: testutil.NewMockChannelFollowingRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := channel.NewService(repo, followRepo, testutil.NewMockNoteRepository(), idGen)
	_, err := svc.ListFollowed("u1", "", "", 10, 0)
	assert.Error(t, err)
}

func TestListOwned(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	owner := "u1"
	repo.Channels["c1"] = &model.Channel{ID: "c1", UserID: &owner}
	repo.Channels["c2"] = &model.Channel{ID: "c2", UserID: &owner}
	rows, err := svc.ListOwned("u1", "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

// #1540: owned は isArchived=FALSE を AND する (archived な自分の channel は出ない)。
func TestListOwned_ExcludesArchived(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	owner := "u1"
	repo.Channels["c1"] = &model.Channel{ID: "c1", UserID: &owner}
	repo.Channels["c2"] = &model.Channel{ID: "c2", UserID: &owner, IsArchived: true}
	rows, err := svc.ListOwned("u1", "", "", 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "c1", rows[0].ID)
}

func TestListFeatured(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	now := time.Now()
	// 投稿のある非アーカイブ channel のみ featured に出る (#1540)。
	repo.Channels["c1"] = &model.Channel{ID: "c1", LastNotedAt: &now}
	// lastNotedAt が無い channel は除外される。
	repo.Channels["c2"] = &model.Channel{ID: "c2"}
	// archived channel も除外される。
	repo.Channels["c3"] = &model.Channel{ID: "c3", LastNotedAt: &now, IsArchived: true}
	rows, err := svc.ListFeatured()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "c1", rows[0].ID)
}

func TestSearch(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1", Name: "alpha"}
	repo.Channels["c2"] = &model.Channel{ID: "c2", Name: "beta"}
	rows, err := svc.Search("alp", "nameAndDescription", "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

// --- Timeline --------------------------------------------------------------

func TestTimeline_HappyPath(t *testing.T) {
	svc, repo, _, noteRepo := newSvc(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	cid := "c1"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", ChannelID: &cid, Visibility: model.NoteVisibilityPublic}
	rows, err := svc.Timeline("c1", "", "", "", 10, false)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

// localOnly は投稿者が local のノートだけを返す (匿名 visitor への
// ugcVisibilityForVisitor = local)。false なら remote の行も残る。
func TestTimeline_LocalOnly(t *testing.T) {
	svc, repo, _, noteRepo := newSvc(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	cid := "c1"
	host := "remote.example"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", ChannelID: &cid, Visibility: model.NoteVisibilityPublic}
	noteRepo.Notes["n2"] = &model.Note{ID: "n2", ChannelID: &cid, UserHost: &host, Visibility: model.NoteVisibilityPublic}

	rows, err := svc.Timeline("c1", "", "", "", 10, true)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "n1", rows[0].ID)

	rows, err = svc.Timeline("c1", "", "", "", 10, false)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

func TestTimeline_ChannelNotFound(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	_, err := svc.Timeline("missing", "", "", "", 10, false)
	assert.ErrorIs(t, err, channel.ErrChannelNotFound)
}

// TestTimeline_VisibilityFilter は public channel に投稿された followers /
// specified note が viewer 単位で適切に除外されることを固定する (#1440)。
// 匿名 / 非フォロワー / specified 対象外の各 viewer ごとに、見えるはずの
// note しか返ってこないことを確認する。
func TestTimeline_VisibilityFilter(t *testing.T) {
	svc, repo, _, noteRepo := newSvc(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	cid := "c1"
	author := "author"
	follower := "follower"
	allowed := "allowed"
	stranger := "stranger"
	// author を follow しているのは follower のみ。Mock の Following は
	// followerID -> []followeeIDs (testutil/mock_repository.go の
	// noteVisibleToViewer 経路で使われる)。
	noteRepo.Following[follower] = []string{author}
	noteRepo.Notes["n_pub"] = &model.Note{
		ID: "n_pub", ChannelID: &cid, UserID: author, Visibility: model.NoteVisibilityPublic,
	}
	noteRepo.Notes["n_fol"] = &model.Note{
		ID: "n_fol", ChannelID: &cid, UserID: author, Visibility: model.NoteVisibilityFollowers,
	}
	noteRepo.Notes["n_spec"] = &model.Note{
		ID: "n_spec", ChannelID: &cid, UserID: author,
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: []string{allowed},
	}

	cases := []struct {
		name    string
		viewer  string
		visible []string
	}{
		{"anonymous", "", []string{"n_pub"}},
		{"stranger", stranger, []string{"n_pub"}},
		{"follower", follower, []string{"n_pub", "n_fol"}},
		{"specified target", allowed, []string{"n_pub", "n_spec"}},
		{"author", author, []string{"n_pub", "n_fol", "n_spec"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := svc.Timeline("c1", tc.viewer, "", "", 50, false)
			require.NoError(t, err)
			got := make([]string, 0, len(rows))
			for _, n := range rows {
				got = append(got, n.ID)
			}
			assert.ElementsMatch(t, tc.visible, got)
		})
	}
}

// --- OnNotePosted ----------------------------------------------------------

func TestOnNotePosted_UpdatesCounters(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	svc.OnNotePosted("c1", "", "")
	assert.Equal(t, 1, repo.Channels["c1"].NotesCount)
	require.NotNil(t, repo.Channels["c1"].LastNotedAt)
}

func TestOnNotePosted_FansOutUnreadToFollowers(t *testing.T) {
	svc, repo, followRepo, _ := newSvc(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	// 2 followers, 1 is the author (self) — skipped
	followRepo.Followings["f1"] = &model.ChannelFollowing{ID: "f1", FollowerID: "alice", FolloweeID: "c1"}
	followRepo.Followings["f2"] = &model.ChannelFollowing{ID: "f2", FollowerID: "author", FolloweeID: "c1"}
	unread := testutil.NewMockChannelNoteUnreadRepository()
	svc.SetUnreadRepo(unread)

	svc.OnNotePosted("c1", "n1", "author")

	// 1 row expected (alice), author is skipped
	require.Len(t, unread.Rows, 1)
	assert.Equal(t, "alice", unread.Rows[0].UserID)
	assert.Equal(t, "c1", unread.Rows[0].ChannelID)
	assert.Equal(t, "n1", unread.Rows[0].NoteID)
}

func TestOnNotePosted_FansOutUnreadToManyFollowersAcrossPages(t *testing.T) {
	// #320: 1000 を超えるフォロワー数でも全員に unread row が作られることを確認。
	svc, repo, followRepo, _ := newSvc(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	const n = 1500
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("f_%05d", i)
		userID := fmt.Sprintf("u_%05d", i)
		followRepo.Followings[id] = &model.ChannelFollowing{ID: id, FollowerID: userID, FolloweeID: "c1"}
	}
	unread := testutil.NewMockChannelNoteUnreadRepository()
	svc.SetUnreadRepo(unread)

	svc.OnNotePosted("c1", "n1", "author")

	// 全 1500 followers に対応する unread row が作られる
	require.Len(t, unread.Rows, n)
	// ユーザーが重複していないこと
	seen := map[string]struct{}{}
	for _, r := range unread.Rows {
		assert.Equal(t, "c1", r.ChannelID)
		assert.Equal(t, "n1", r.NoteID)
		_, dup := seen[r.UserID]
		assert.False(t, dup, "userID %s is duplicated across pages", r.UserID)
		seen[r.UserID] = struct{}{}
	}
}

func TestOnNotePosted_FansOutSkipsAuthorAcrossPages(t *testing.T) {
	// 作者 ID はどのページに入ってもスキップされる。
	svc, repo, followRepo, _ := newSvc(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	for i := 0; i < 600; i++ {
		id := fmt.Sprintf("f_%05d", i)
		userID := fmt.Sprintf("u_%05d", i)
		followRepo.Followings[id] = &model.ChannelFollowing{ID: id, FollowerID: userID, FolloweeID: "c1"}
	}
	authorID := "u_00501"
	unread := testutil.NewMockChannelNoteUnreadRepository()
	svc.SetUnreadRepo(unread)

	svc.OnNotePosted("c1", "n1", authorID)

	// 599 行 (著者以外)
	require.Len(t, unread.Rows, 599)
	for _, r := range unread.Rows {
		assert.NotEqual(t, authorID, r.UserID)
	}
}

func TestOnNotePosted_NoUnreadRepoSkipsFanout(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	// unread repo未配線でも counter更新は動作する
	svc.OnNotePosted("c1", "n1", "author")
	assert.Equal(t, 1, repo.Channels["c1"].NotesCount)
}

// --- SetClock --------------------------------------------------------------

func TestSetClock(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	fixed := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return fixed })
	svc.SetClock(nil) // nil 渡し無視
	c, err := svc.Create(channel.CreateInput{OwnerID: "u1", Name: "alpha"})
	require.NoError(t, err)
	// id は idGen で fixed タイムから派生する
	assert.NotEmpty(t, c.ID)
}
