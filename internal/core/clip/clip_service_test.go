package clip_test

import (
	"errors"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/core/clip"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSvc(t *testing.T) (*clip.Service, *testutil.MockClipRepository, *testutil.MockClipNoteRepository, *testutil.MockNoteRepository) {
	t.Helper()
	repo := testutil.NewMockClipRepository()
	noteRepo := testutil.NewMockClipNoteRepository()
	notes := testutil.NewMockNoteRepository()
	// ListByClipVisible の visibility push-down 再現のため、clip mock に note の
	// visibility lookup 用 map を共有させる (#1418 review)。
	noteRepo.Notes = notes.Notes
	idGen, _ := id.NewGenerator("aidx")
	return clip.NewService(repo, noteRepo, notes, idGen), repo, noteRepo, notes
}

// --- Create ----------------------------------------------------------------

func TestCreate_HappyPath(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	c, err := svc.Create(clip.CreateInput{OwnerID: "u1", Name: "alpha"})
	require.NoError(t, err)
	assert.Equal(t, "alpha", c.Name)
	assert.Len(t, repo.Clips, 1)
}

// #1029: clipLimit role policy gate test。
type stubRolePolicyProvider struct {
	policies map[string]any
}

func (s *stubRolePolicyProvider) GetUserPolicies(_ string) map[string]any { return s.policies }

func TestCreate_ClipLimitExceeded(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	repo.Clips["c2"] = &model.Clip{ID: "c2", UserID: "u1"}
	svc.SetRolePolicyProvider(&stubRolePolicyProvider{policies: map[string]any{
		"clipLimit": 2,
	}})
	_, err := svc.Create(clip.CreateInput{OwnerID: "u1", Name: "third"})
	require.ErrorIs(t, err, clip.ErrTooManyClips)
}

func TestCreate_ClipLimit_PassesUnderLimit(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	svc.SetRolePolicyProvider(&stubRolePolicyProvider{policies: map[string]any{
		"clipLimit": 10,
	}})
	_, err := svc.Create(clip.CreateInput{OwnerID: "u1", Name: "first"})
	require.NoError(t, err)
}

func TestAddNote_NoteEachClipsLimitExceeded(t *testing.T) {
	svc, repo, noteRepo, notes := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	notes.Notes["n1"] = &model.Note{ID: "n1"}
	noteRepo.Entries["cn1"] = &model.ClipNote{ID: "cn1", ClipID: "c1", NoteID: "old1"}
	noteRepo.Entries["cn2"] = &model.ClipNote{ID: "cn2", ClipID: "c1", NoteID: "old2"}
	svc.SetRolePolicyProvider(&stubRolePolicyProvider{policies: map[string]any{
		"noteEachClipsLimit": 2,
	}})
	err := svc.AddNote("u1", "c1", "n1")
	require.ErrorIs(t, err, clip.ErrTooManyClipNotes)
}

func TestCreate_NameRequired(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	_, err := svc.Create(clip.CreateInput{OwnerID: "u1"})
	assert.ErrorIs(t, err, clip.ErrClipNameRequired)
}

func TestCreate_OwnerRequired(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	_, err := svc.Create(clip.CreateInput{Name: "alpha"})
	assert.Error(t, err)
}

func TestCreate_RepoError(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.CreateErr = errors.New("boom")
	_, err := svc.Create(clip.CreateInput{OwnerID: "u1", Name: "alpha"})
	assert.Error(t, err)
}

// --- Show ------------------------------------------------------------------

func TestShow_HappyPath(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1", Name: "alpha"}
	got, err := svc.Show("u1", "c1")
	require.NoError(t, err)
	assert.Equal(t, "alpha", got.Name)
}

func TestShow_NotFound(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	_, err := svc.Show("u1", "missing")
	assert.ErrorIs(t, err, clip.ErrClipNotFound)
}

// TestGet_RawNoVisibilityGate: Get は visibility gate 無しで raw に引く
// (clips/my-favorites 用、#1830)。他人所有の非公開 clip も返す。
func TestGet_RawNoVisibilityGate(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "owner", IsPublic: false, Name: "private"}
	got, err := svc.Get("c1")
	require.NoError(t, err)
	assert.Equal(t, "private", got.Name)
}

func TestGet_NotFound(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	_, err := svc.Get("missing")
	assert.ErrorIs(t, err, clip.ErrClipNotFound)
}

func TestShow_PrivateHiddenFromOthers(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1", IsPublic: false}
	// 非公開 clip の他人閲覧は missing と区別不能な ErrClipNotFound (#1562)
	_, err := svc.Show("u2", "c1")
	assert.ErrorIs(t, err, clip.ErrClipNotFound)
}

func TestShow_PublicReadableByAnyone(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1", IsPublic: true}
	got, err := svc.Show("u2", "c1")
	require.NoError(t, err)
	assert.NotNil(t, got)
}

// --- Update ----------------------------------------------------------------

func TestUpdate_HappyPath(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1", Name: "alpha"}
	newName := "alpha-v2"
	desc := "new"
	descPtr := &desc
	pub := true
	got, err := svc.Update("u1", "c1", clip.UpdateInput{
		Name:        &newName,
		Description: &descPtr,
		IsPublic:    &pub,
	})
	require.NoError(t, err)
	assert.Equal(t, "alpha-v2", got.Name)
}

func TestUpdate_NotFound(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	_, err := svc.Update("u1", "missing", clip.UpdateInput{})
	assert.ErrorIs(t, err, clip.ErrClipNotFound)
}

func TestUpdate_NonOwnerHidden(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "owner"}
	// 非 owner mutation は upstream findOneBy({id, userId}) と同じく
	// missing と区別不能な ErrClipNotFound (#1562)
	_, err := svc.Update("u1", "c1", clip.UpdateInput{})
	assert.ErrorIs(t, err, clip.ErrClipNotFound)
}

func TestUpdate_NameEmpty(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	empty := ""
	_, err := svc.Update("u1", "c1", clip.UpdateInput{Name: &empty})
	assert.ErrorIs(t, err, clip.ErrClipNameRequired)
}

func TestUpdate_RepoError(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	repo.UpdateErr = errors.New("boom")
	name := "x"
	_, err := svc.Update("u1", "c1", clip.UpdateInput{Name: &name})
	assert.Error(t, err)
}

// --- Delete ----------------------------------------------------------------

func TestDelete_HappyPath(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	require.NoError(t, svc.Delete("u1", "c1"))
	assert.Empty(t, repo.Clips)
}

func TestDelete_NotFound(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	err := svc.Delete("u1", "missing")
	assert.ErrorIs(t, err, clip.ErrClipNotFound)
}

func TestDelete_NonOwnerHidden(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "owner"}
	err := svc.Delete("u1", "c1")
	assert.ErrorIs(t, err, clip.ErrClipNotFound)
}

// --- ListByUser ------------------------------------------------------------

func TestListByUser(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	repo.Clips["c2"] = &model.Clip{ID: "c2", UserID: "u1"}
	rows, err := svc.ListByUser("u1", "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

// --- AddNote ---------------------------------------------------------------

func TestAddNote_HappyPath(t *testing.T) {
	svc, repo, noteRepo, notes := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	notes.Notes["n1"] = &model.Note{ID: "n1"}
	require.NoError(t, svc.AddNote("u1", "c1", "n1"))
	assert.Len(t, noteRepo.Entries, 1)
	// notesCount は clip 行に非正規化せず clip_note の実カウントで出す (#2243)。
	n, err := svc.CountNotes("c1")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	require.NotNil(t, repo.Clips["c1"].LastClippedAt)
	// #1768: note の clippedCount も increment される。
	assert.Equal(t, int16(1), notes.Notes["n1"].ClippedCount)
}

// #1768: add -> remove で note の clippedCount が 1 -> 0 に維持される。
func TestClippedCount_MaintainedOnAddRemove(t *testing.T) {
	svc, repo, _, notes := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	notes.Notes["n1"] = &model.Note{ID: "n1"}
	require.NoError(t, svc.AddNote("u1", "c1", "n1"))
	assert.Equal(t, int16(1), notes.Notes["n1"].ClippedCount)
	require.NoError(t, svc.RemoveNote("u1", "c1", "n1"))
	assert.Equal(t, int16(0), notes.Notes["n1"].ClippedCount)
}

func TestAddNote_ClipNotFound(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	err := svc.AddNote("u1", "missing", "n1")
	assert.ErrorIs(t, err, clip.ErrClipNotFound)
}

func TestAddNote_NonOwnerHidden(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "owner"}
	err := svc.AddNote("u1", "c1", "n1")
	assert.ErrorIs(t, err, clip.ErrClipNotFound)
}

func TestAddNote_NoteNotFound(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	err := svc.AddNote("u1", "c1", "missing")
	assert.ErrorIs(t, err, clip.ErrNoteNotFound)
}

func TestAddNote_AlreadyClipped(t *testing.T) {
	svc, repo, _, notes := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	notes.Notes["n1"] = &model.Note{ID: "n1"}
	require.NoError(t, svc.AddNote("u1", "c1", "n1"))
	err := svc.AddNote("u1", "c1", "n1")
	assert.ErrorIs(t, err, clip.ErrAlreadyClipped)
}

// failingClipNoteRepo causes Create to fail.
type failingClipNoteRepo struct {
	*testutil.MockClipNoteRepository
}

func (r *failingClipNoteRepo) Create(_ *model.ClipNote) error { return errors.New("boom") }

func TestAddNote_RepoError(t *testing.T) {
	repo := testutil.NewMockClipRepository()
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	noteRepo := &failingClipNoteRepo{MockClipNoteRepository: testutil.NewMockClipNoteRepository()}
	notes := testutil.NewMockNoteRepository()
	notes.Notes["n1"] = &model.Note{ID: "n1"}
	idGen, _ := id.NewGenerator("aidx")
	svc := clip.NewService(repo, noteRepo, notes, idGen)
	err := svc.AddNote("u1", "c1", "n1")
	assert.Error(t, err)
}

// --- RemoveNote ------------------------------------------------------------

func TestRemoveNote_HappyPath(t *testing.T) {
	svc, repo, _, notes := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	notes.Notes["n1"] = &model.Note{ID: "n1"}
	require.NoError(t, svc.AddNote("u1", "c1", "n1"))
	require.NoError(t, svc.RemoveNote("u1", "c1", "n1"))
	n, err := svc.CountNotes("c1")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestRemoveNote_ClipNotFound(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	err := svc.RemoveNote("u1", "missing", "n1")
	assert.ErrorIs(t, err, clip.ErrClipNotFound)
}

func TestRemoveNote_NonOwnerHidden(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "owner"}
	err := svc.RemoveNote("u1", "c1", "n1")
	assert.ErrorIs(t, err, clip.ErrClipNotFound)
}

// upstream 2026.10.0 (4682d44cae): note は実在するが clip に含まれない場合も
// NO_SUCH_NOTE (ErrNoteNotFound) になり、clippedCount は減らない。
func TestRemoveNote_NotInClipIsNoSuchNote(t *testing.T) {
	svc, repo, _, notes := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	notes.Notes["n1"] = &model.Note{ID: "n1", ClippedCount: 1}
	err := svc.RemoveNote("u1", "c1", "n1")
	assert.ErrorIs(t, err, clip.ErrNoteNotFound)
	assert.Equal(t, int16(1), notes.Notes["n1"].ClippedCount, "clip に無い note の clippedCount を減らしている")
}

// 2 回目の remove は NO_SUCH_NOTE になり、clippedCount は 1 回分しか減らない。
func TestRemoveNote_RepeatedRemoveDecrementsOnce(t *testing.T) {
	svc, repo, _, notes := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	notes.Notes["n1"] = &model.Note{ID: "n1"}
	require.NoError(t, svc.AddNote("u1", "c1", "n1"))
	require.Equal(t, int16(1), notes.Notes["n1"].ClippedCount)
	require.NoError(t, svc.RemoveNote("u1", "c1", "n1"))
	assert.ErrorIs(t, svc.RemoveNote("u1", "c1", "n1"), clip.ErrNoteNotFound)
	assert.Equal(t, int16(0), notes.Notes["n1"].ClippedCount)
}

// #1768: note 自体が存在しなければ NO_SUCH_NOTE (ErrNoteNotFound)。
func TestRemoveNote_NoSuchNote(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	err := svc.RemoveNote("u1", "c1", "n1")
	assert.ErrorIs(t, err, clip.ErrNoteNotFound)
}

// failingDeleteRepo causes DeleteByPair to fail.
type failingDeleteRepo struct {
	*testutil.MockClipNoteRepository
}

func (r *failingDeleteRepo) DeleteByPair(string, string) (int64, error) {
	return 0, errors.New("boom")
}

func TestRemoveNote_RepoError(t *testing.T) {
	repo := testutil.NewMockClipRepository()
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	mock := testutil.NewMockClipNoteRepository()
	mock.Entries["cn1"] = &model.ClipNote{ID: "cn1", ClipID: "c1", NoteID: "n1"}
	noteRepo := &failingDeleteRepo{MockClipNoteRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	notes := testutil.NewMockNoteRepository()
	notes.Notes["n1"] = &model.Note{ID: "n1", ClippedCount: 1}
	svc := clip.NewService(repo, noteRepo, notes, idGen)
	err := svc.RemoveNote("u1", "c1", "n1")
	assert.Error(t, err)
	assert.False(t, errors.Is(err, clip.ErrNoteNotFound), "DB 障害が NO_SUCH_NOTE に丸められている")
	assert.Equal(t, int16(1), notes.Notes["n1"].ClippedCount)
}

// --- Notes -----------------------------------------------------------------

func TestNotes_HappyPath(t *testing.T) {
	svc, repo, _, notes := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	notes.Notes["n1"] = &model.Note{ID: "n1", Visibility: model.NoteVisibilityPublic}
	notes.Notes["n2"] = &model.Note{ID: "n2", Visibility: model.NoteVisibilityPublic}
	require.NoError(t, svc.AddNote("u1", "c1", "n1"))
	require.NoError(t, svc.AddNote("u1", "c1", "n2"))

	rows, err := svc.Notes("u1", "c1", "", "", 10, "")
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

func TestNotes_NotFound(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	_, err := svc.Notes("u1", "missing", "", "", 10, "")
	assert.ErrorIs(t, err, clip.ErrClipNotFound)
}

func TestNotes_PrivateHiddenFromOthers(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1", IsPublic: false}
	_, err := svc.Notes("u2", "c1", "", "", 10, "")
	assert.ErrorIs(t, err, clip.ErrClipNotFound)
}

func TestNotes_PublicReadableByAnyone(t *testing.T) {
	svc, repo, _, notes := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1", IsPublic: true}
	notes.Notes["n1"] = &model.Note{ID: "n1", Visibility: model.NoteVisibilityPublic}
	require.NoError(t, svc.AddNote("u1", "c1", "n1"))
	rows, err := svc.Notes("u2", "c1", "", "", 10, "")
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

func TestNotes_EmptyClipReturnsNil(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	rows, err := svc.Notes("u1", "c1", "", "", 10, "")
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// listFailRepo causes the clip note listing to fail. Service.Notes は
// ListByClipVisible を呼ぶためそちらを override する。
type listFailRepo struct {
	*testutil.MockClipNoteRepository
}

func (r *listFailRepo) ListByClipVisible(_, _, _, _ string, _ int, _ []string) ([]*model.ClipNote, error) {
	return nil, errors.New("boom")
}

func TestNotes_RepoError(t *testing.T) {
	repo := testutil.NewMockClipRepository()
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	noteRepo := &listFailRepo{MockClipNoteRepository: testutil.NewMockClipNoteRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := clip.NewService(repo, noteRepo, testutil.NewMockNoteRepository(), idGen)
	_, err := svc.Notes("u1", "c1", "", "", 10, "")
	assert.Error(t, err)
}

// --- SetClock --------------------------------------------------------------

func TestSetClock(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	fixed := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return fixed })
	svc.SetClock(nil)
	c, err := svc.Create(clip.CreateInput{OwnerID: "u1", Name: "alpha"})
	require.NoError(t, err)
	assert.NotEmpty(t, c.ID)
}

// **小数の policy でもゲートが効くこと。**
//
// policy の数値は小数を取りうる (role.PolicyNumber の doc 参照)。素の
// `.(int)` で読むと型アサーションに失敗し、上限違反で弾くのではなく
// **上限そのものが消える** (#2613)。
func TestCreate_ClipLimit_Fractional(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	repo.Clips["c2"] = &model.Clip{ID: "c2", UserID: "u1"}
	// 上限 1.5 に対し既に 2 件あるので弾く。
	svc.SetRolePolicyProvider(&stubRolePolicyProvider{policies: map[string]any{
		"clipLimit": 1.5,
	}})
	_, err := svc.Create(clip.CreateInput{OwnerID: "u1", Name: "third"})
	require.ErrorIs(t, err, clip.ErrTooManyClips)
}

// 小数の上限を下回っていれば通ること (常に弾く実装になっていないこと)。
func TestCreate_ClipLimit_FractionalPasses(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1"}
	// 1 件 < 1.5 なのでまだ作れる。
	svc.SetRolePolicyProvider(&stubRolePolicyProvider{policies: map[string]any{
		"clipLimit": 1.5,
	}})
	_, err := svc.Create(clip.CreateInput{OwnerID: "u1", Name: "second"})
	require.NoError(t, err)
}
