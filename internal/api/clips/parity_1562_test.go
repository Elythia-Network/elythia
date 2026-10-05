package clips

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// --- #1562: name / description 長さ検証と '' → null 正規化 ---

func TestCreate_NameTooLongRejected(t *testing.T) {
	h, _, _, _ := newHandler(t)
	name := strings.Repeat("a", clipNameMaxLength+1)
	c, rec := newReq(t, fmt.Sprintf(`{"name":%q}`, name))
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_NameAtMaxLengthAccepted(t *testing.T) {
	h, _, _, _ := newHandler(t)
	name := strings.Repeat("a", clipNameMaxLength)
	c, rec := newReq(t, fmt.Sprintf(`{"name":%q}`, name))
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestCreate_DescriptionTooLongRejected(t *testing.T) {
	h, _, _, _ := newHandler(t)
	desc := strings.Repeat("d", clipDescriptionMaxLength+1)
	c, rec := newReq(t, fmt.Sprintf(`{"name":"x","description":%q}`, desc))
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_EmptyDescriptionNormalizedToNull(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	c, rec := newReq(t, `{"name":"x","description":""}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	require.Equal(t, http.StatusOK, rec.Code)
	for _, cl := range repo.Clips {
		assert.Nil(t, cl.Description, "empty description must be stored as NULL (upstream `ps.description || null`)")
	}
}

func TestUpdate_NameTooLongRejected(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", Name: "old"}
	name := strings.Repeat("あ", clipNameMaxLength+1) // rune 数で判定する
	c, rec := newReq(t, fmt.Sprintf(`{"clipId":"c1","name":%q}`, name))
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_EmptyDescriptionNormalizedToNull(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	desc := "before"
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", Name: "old", Description: &desc}
	c, rec := newReq(t, `{"clipId":"c1","description":""}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Nil(t, repo.Clips["c1"].Description)
}

// --- #1562: favoritedCount / isFavorited / notesCount の出し分け ---

func newHandlerWithFavorites(t *testing.T) (
	*Handler,
	*testutil.MockClipRepository,
	*testutil.MockClipFavoriteRepository,
	*testutil.MockClipNoteRepository,
) {
	t.Helper()
	h, repo, clipNoteRepo, _ := newHandler(t)
	favRepo := testutil.NewMockClipFavoriteRepository()
	h.SetFavoriteRepo(favRepo)
	return h, repo, favRepo, clipNoteRepo
}

func TestShow_OwnerSeesCountsAndFavorited(t *testing.T) {
	h, repo, favRepo, clipNoteRepo := newHandlerWithFavorites(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: true}
	// notesCount は clip 行の非正規化カウンタではなく clip_note の実カウント (#2243)。
	for i := 0; i < 7; i++ {
		require.NoError(t, clipNoteRepo.Create(&model.ClipNote{
			ID: fmt.Sprintf("cn%d", i), ClipID: "c1", NoteID: fmt.Sprintf("n%d", i),
		}))
	}
	favRepo.Favorites["alice:c1"] = &model.ClipFavorite{ID: "f1", UserID: "alice", ClipID: "c1"}
	favRepo.Favorites["bob:c1"] = &model.ClipFavorite{ID: "f2", UserID: "bob", ClipID: "c1"}

	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"favoritedCount":2`)
	assert.Contains(t, body, `"isFavorited":true`)
	assert.Contains(t, body, `"notesCount":7`)
}

func TestShow_NonOwnerHidesNotesCount(t *testing.T) {
	h, repo, _, _ := newHandlerWithFavorites(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: true}

	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "bob")
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "notesCount", "notesCount must be omitted for non-owner viewers (#1562)")
	assert.Contains(t, body, `"isFavorited":false`)
	assert.Contains(t, body, `"favoritedCount":0`)
}

func TestShow_AnonymousOmitsIsFavorited(t *testing.T) {
	h, repo, _, _ := newHandlerWithFavorites(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: true}

	c, rec := newReq(t, `{"clipId":"c1"}`)
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "isFavorited", "isFavorited must be omitted for anonymous viewers (#1562)")
}

// --- #1562: clips/notes search param ---

func TestNotes_SearchValidation(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: true}

	// 空文字は minLength 1 違反
	c, rec := newReq(t, `{"clipId":"c1","search":""}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// 101 rune は maxLength 100 違反
	c, rec = newReq(t, fmt.Sprintf(`{"clipId":"c1","search":%q}`, strings.Repeat("x", clipNotesSearchMaxLength+1)))
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestNotes_SearchFiltersByTextAndCW(t *testing.T) {
	h, repo, noteRepo, notes := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: true}
	hitText := "golang rocks"
	missText := "unrelated"
	hitCW := "warning golang"
	notes.Notes["n1"] = &model.Note{ID: "n1", Text: &hitText, Visibility: model.NoteVisibilityPublic}
	notes.Notes["n2"] = &model.Note{ID: "n2", Text: &missText, Visibility: model.NoteVisibilityPublic}
	notes.Notes["n3"] = &model.Note{ID: "n3", CW: &hitCW, Visibility: model.NoteVisibilityPublic}
	noteRepo.Entries["cn1"] = &model.ClipNote{ID: "cn1", ClipID: "c1", NoteID: "n1"}
	noteRepo.Entries["cn2"] = &model.ClipNote{ID: "cn2", ClipID: "c1", NoteID: "n2"}
	noteRepo.Entries["cn3"] = &model.ClipNote{ID: "cn3", ClipID: "c1", NoteID: "n3"}

	c, rec := newReq(t, `{"clipId":"c1","search":"golang"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"id":"n1"`, "text hit must be returned")
	assert.Contains(t, body, `"id":"n3"`, "cw hit must be returned")
	assert.NotContains(t, body, `"id":"n2"`, "non-matching note must be filtered")
}

// --- #1562: clips/notes muted/blocked-user + blocked-host filter ---

func TestNotes_MutedAndBlockedUsersFiltered(t *testing.T) {
	h, repo, noteRepo, notes := newHandler(t)
	mutingRepo := testutil.NewMockMutingRepository()
	blockingRepo := testutil.NewMockBlockingRepository()
	h.SetMuteBlockRepos(mutingRepo, blockingRepo)

	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: true}
	notes.Notes["n1"] = &model.Note{ID: "n1", UserID: "muted", Visibility: model.NoteVisibilityPublic}
	notes.Notes["n2"] = &model.Note{ID: "n2", UserID: "blocker", Visibility: model.NoteVisibilityPublic}
	notes.Notes["n3"] = &model.Note{ID: "n3", UserID: "ok", Visibility: model.NoteVisibilityPublic}
	noteRepo.Entries["cn1"] = &model.ClipNote{ID: "cn1", ClipID: "c1", NoteID: "n1"}
	noteRepo.Entries["cn2"] = &model.ClipNote{ID: "cn2", ClipID: "c1", NoteID: "n2"}
	noteRepo.Entries["cn3"] = &model.ClipNote{ID: "cn3", ClipID: "c1", NoteID: "n3"}
	// viewer が muted を mute、blocker が viewer を block
	mutingRepo.Mutings["m1"] = &model.Muting{ID: "m1", MuterID: "viewer", MuteeID: "muted"}
	blockingRepo.Blockings["b1"] = &model.Blocking{ID: "b1", BlockerID: "blocker", BlockeeID: "viewer"}

	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "viewer")
	require.NoError(t, h.Notes(c))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, `"id":"n1"`, "muted author must be filtered")
	assert.NotContains(t, body, `"id":"n2"`, "blocking author must be filtered")
	assert.Contains(t, body, `"id":"n3"`)
}

func TestNotes_BlockedHostFiltered(t *testing.T) {
	h, repo, noteRepo, notes := newHandler(t)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", BlockedHosts: []string{"blocked.example"}}
	h.SetMetaRepo(metaRepo)

	host := "blocked.example"
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: true}
	notes.Notes["n1"] = &model.Note{ID: "n1", UserHost: &host, Visibility: model.NoteVisibilityPublic}
	notes.Notes["n2"] = &model.Note{ID: "n2", Visibility: model.NoteVisibilityPublic}
	noteRepo.Entries["cn1"] = &model.ClipNote{ID: "cn1", ClipID: "c1", NoteID: "n1"}
	noteRepo.Entries["cn2"] = &model.ClipNote{ID: "cn2", ClipID: "c1", NoteID: "n2"}

	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "viewer")
	require.NoError(t, h.Notes(c))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, `"id":"n1"`, "blocked-host author must be filtered")
	assert.Contains(t, body, `"id":"n2"`)
}

// 匿名 viewer でも blocked-host filter は適用される (admin 政策なので viewer
// 非依存、upstream notes.ts も me なしで適用する)。
func TestNotes_BlockedHostFilteredForAnonymous(t *testing.T) {
	h, repo, noteRepo, notes := newHandler(t)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", BlockedHosts: []string{"blocked.example"}}
	h.SetMetaRepo(metaRepo)

	host := "blocked.example"
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: true}
	notes.Notes["n1"] = &model.Note{ID: "n1", UserHost: &host, Visibility: model.NoteVisibilityPublic}
	noteRepo.Entries["cn1"] = &model.ClipNote{ID: "cn1", ClipID: "c1", NoteID: "n1"}

	c, rec := newReq(t, `{"clipId":"c1"}`)
	require.NoError(t, h.Notes(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"id":"n1"`)
}

// --- #1562: error branch coverage ---

// failingFavRepo wraps the mock to force errors on selected methods.
type failingFavRepo struct {
	*testutil.MockClipFavoriteRepository
	existsErr error
	countErr  error
	deleteErr error
}

func (f *failingFavRepo) Exists(userID, clipID string) (bool, error) {
	if f.existsErr != nil {
		return false, f.existsErr
	}
	return f.MockClipFavoriteRepository.Exists(userID, clipID)
}

func (f *failingFavRepo) CountByClip(clipID string) (int64, error) {
	if f.countErr != nil {
		return 0, f.countErr
	}
	return f.MockClipFavoriteRepository.CountByClip(clipID)
}

func (f *failingFavRepo) Delete(userID, clipID string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return f.MockClipFavoriteRepository.Delete(userID, clipID)
}

func TestClipFavorite_ExistsErrorIs500(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	fail := &failingFavRepo{MockClipFavoriteRepository: testutil.NewMockClipFavoriteRepository(), existsErr: assert.AnError}
	h.SetFavoriteRepo(fail)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1", IsPublic: true}
	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "u1")
	require.NoError(t, h.Favorite(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestClipUnfavorite_FavoriteExistsErrorIs500(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	fail := &failingFavRepo{MockClipFavoriteRepository: testutil.NewMockClipFavoriteRepository(), existsErr: assert.AnError}
	h.SetFavoriteRepo(fail)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1", IsPublic: true}
	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "u1")
	require.NoError(t, h.Unfavorite(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestClipUnfavorite_DeleteErrorIs500(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	mock := testutil.NewMockClipFavoriteRepository()
	mock.Favorites["u1:c1"] = &model.ClipFavorite{ID: "f1", UserID: "u1", ClipID: "c1"}
	fail := &failingFavRepo{MockClipFavoriteRepository: mock, deleteErr: assert.AnError}
	h.SetFavoriteRepo(fail)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1", IsPublic: true}
	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "u1")
	require.NoError(t, h.Unfavorite(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// clipExtras は count / exists の lookup error を degrade して 0 / false に
// 落とす (200 を維持、#1562)。
func TestShow_FavoriteLookupErrorDegrades(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	fail := &failingFavRepo{MockClipFavoriteRepository: testutil.NewMockClipFavoriteRepository(), existsErr: assert.AnError, countErr: assert.AnError}
	h.SetFavoriteRepo(fail)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: true}
	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "bob")
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"favoritedCount":0`)
	assert.Contains(t, rec.Body.String(), `"isFavorited":false`)
}

// metaRepo.Fetch error は fail-closed で 500 (#1544 と同方針)。
func TestNotes_MetaFetchErrorIs500(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	h.SetMetaRepo(testutil.NewMockMetaRepository()) // Meta 未設定 → Fetch error
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: true}
	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// upstream update.ts は `ps.description || null` を常に渡すため、description
// 未指定 (undefined) でも NULL にクリアされる (#1562)。
func TestUpdate_OmittedDescriptionClearedToNull(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	desc := "before"
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", Name: "old", Description: &desc}
	c, rec := newReq(t, `{"clipId":"c1","name":"new"}`)
	setUser(c, "alice")
	require.NoError(t, h.Update(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Nil(t, repo.Clips["c1"].Description, "omitted description must clear to NULL (upstream `ps.description || null`)")
	assert.Equal(t, "new", repo.Clips["c1"].Name)
}

// #1630: clips/notes が viewer の mutedInstances に属する author の note を
// 除外する (notesfilter の muted-instances 次元の配線確認)。
func TestNotes_MutedInstancesFiltered(t *testing.T) {
	h, repo, noteRepo, notes := newHandler(t)
	userRepo := testutil.NewMockUserRepository()
	userRepo.Profiles["viewer"] = &model.UserProfile{
		UserID:         "viewer",
		MutedInstances: datatypes.JSON(`["muted.example"]`),
	}
	h.SetUserRepo(userRepo)
	h.SetMuteBlockRepos(testutil.NewMockMutingRepository(), testutil.NewMockBlockingRepository())

	mutedHost := "muted.example"
	okHost := "ok.example"
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: true}
	notes.Notes["n1"] = &model.Note{ID: "n1", UserID: "remote1", UserHost: &mutedHost, Visibility: model.NoteVisibilityPublic}
	notes.Notes["n2"] = &model.Note{ID: "n2", UserID: "remote2", UserHost: &okHost, Visibility: model.NoteVisibilityPublic}
	noteRepo.Entries["cn1"] = &model.ClipNote{ID: "cn1", ClipID: "c1", NoteID: "n1"}
	noteRepo.Entries["cn2"] = &model.ClipNote{ID: "cn2", ClipID: "c1", NoteID: "n2"}

	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "viewer")
	require.NoError(t, h.Notes(c))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, `"id":"n1"`, "note from a muted instance must be filtered")
	assert.Contains(t, body, `"id":"n2"`)
}

// #1630: clips/notes が renote 先の入れ子 author を mute 判定で除外する
// (renote 入れ子変種の配線確認。h.noteRepo が renote 先行を引く)。
func TestNotes_RenoteNestedMuteFiltered(t *testing.T) {
	h, repo, noteRepo, notes := newHandler(t)
	h.SetUserRepo(testutil.NewMockUserRepository())
	mutingRepo := testutil.NewMockMutingRepository()
	h.SetMuteBlockRepos(mutingRepo, testutil.NewMockBlockingRepository())
	// renote 先行を引く NoteRepository を配線 (#1630)
	h.SetNoteRepo(notes)

	// viewer は muted を mute。clip 内の note は muted への reply を renote した
	// もの (note 自身の author は clean だが renote 先が muted への reply)。
	mutingRepo.Mutings["m1"] = &model.Muting{ID: "m1", MuterID: "viewer", MuteeID: "muted"}
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: true}
	renoteID := "rt1"
	mutedUser := "muted"
	notes.Notes["rt1"] = &model.Note{ID: "rt1", UserID: "author", ReplyUserID: &mutedUser, Visibility: model.NoteVisibilityPublic}
	authorOfWrapper := "author"
	notes.Notes["n1"] = &model.Note{ID: "n1", UserID: "wrapper", RenoteID: &renoteID, RenoteUserID: &authorOfWrapper, Visibility: model.NoteVisibilityPublic}
	notes.Notes["n2"] = &model.Note{ID: "n2", UserID: "clean", Visibility: model.NoteVisibilityPublic}
	noteRepo.Entries["cn1"] = &model.ClipNote{ID: "cn1", ClipID: "c1", NoteID: "n1"}
	noteRepo.Entries["cn2"] = &model.ClipNote{ID: "cn2", ClipID: "c1", NoteID: "n2"}

	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "viewer")
	require.NoError(t, h.Notes(c))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, `"id":"n1"`, "renote whose target replies to a muted user must be filtered")
	assert.Contains(t, body, `"id":"n2"`)
}
