package transfer_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/core/transfer"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// visibilityFixture seeds notes that alice (the exporter) may or may not see,
// mirroring the rows upstream generateVisibilityQuery / shouldHideNoteByTime
// drop from the favorites / clips exports.
type visibilityFixture struct {
	visible []string
	hidden  []string
	notes   []*model.Note
}

func newVisibilityFixture(t *testing.T, deps transfer.ExporterDeps) visibilityFixture {
	t.Helper()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	userRepo := deps.UserRepo.(*testutil.MockUserRepository)
	bob := userRepo.Users["bob"]
	hiddenBefore := -3600
	dave := &model.User{ID: "dave", Username: "dave", UsernameLower: "dave", MakeNotesHiddenBefore: &hiddenBefore}
	userRepo.Users[dave.ID] = dave

	now := time.Now()
	mk := func(author *model.User, vis model.NoteVisibility, created time.Time, visibleTo ...string) *model.Note {
		text := "secret"
		return &model.Note{
			ID: idGen.Generate(created), UserID: author.ID, User: author, Text: &text,
			Visibility: vis, VisibleUserIDs: visibleTo,
		}
	}
	f := visibilityFixture{}
	add := func(n *model.Note, visible bool) {
		f.notes = append(f.notes, n)
		if visible {
			f.visible = append(f.visible, n.ID)
		} else {
			f.hidden = append(f.hidden, n.ID)
		}
	}
	add(mk(bob, model.NoteVisibilityPublic, now), true)
	add(mk(bob, model.NoteVisibilityFollowers, now), false)
	add(mk(bob, model.NoteVisibilitySpecified, now, "carol"), false)
	add(mk(bob, model.NoteVisibilitySpecified, now, "alice"), true)
	// dave は makeNotesHiddenBefore = 1 時間。2 時間前のノートは隠れ、直近のものは残る。
	add(mk(dave, model.NoteVisibilityPublic, now.Add(-2*time.Hour)), false)
	add(mk(dave, model.NoteVisibilityPublic, now), true)
	return f
}

func TestExport_Favorites_DropsNotesTheExporterCannotSee(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	deps.IDGen, _ = id.NewGenerator("aidx")
	f := newVisibilityFixture(t, deps)
	favRepo := deps.NoteFavoriteRepo.(*testutil.MockNoteFavoriteRepository)
	for i, n := range f.notes {
		fid := "fv" + string(rune('a'+i))
		favRepo.Favorites[fid] = &model.NoteFavorite{ID: fid, UserID: user.ID, NoteID: n.ID, Note: n}
	}

	_, err := transfer.NewExporter(deps).Export(context.Background(), user.ID, transfer.ExportFavorites)
	require.NoError(t, err)
	var arr []map[string]any
	require.NoError(t, json.Unmarshal(saver.uploads[0].Body, &arr))
	var got []string
	for _, row := range arr {
		got = append(got, row["note"].(map[string]any)["id"].(string))
	}
	assert.ElementsMatch(t, f.visible, got)
	for _, h := range f.hidden {
		assert.NotContains(t, string(saver.uploads[0].Body), h)
	}
}

func TestExport_Favorites_KeepsFollowersNoteForFollower(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	f := newVisibilityFixture(t, deps)
	require.NoError(t, deps.FollowingRepo.Create(&model.Following{ID: "fw1", FollowerID: user.ID, FolloweeID: "bob"}))
	favRepo := deps.NoteFavoriteRepo.(*testutil.MockNoteFavoriteRepository)
	fol := f.notes[1]
	require.Equal(t, model.NoteVisibilityFollowers, fol.Visibility)
	favRepo.Favorites["fv1"] = &model.NoteFavorite{ID: "fv1", UserID: user.ID, NoteID: fol.ID, Note: fol}

	_, err := transfer.NewExporter(deps).Export(context.Background(), user.ID, transfer.ExportFavorites)
	require.NoError(t, err)
	assert.Contains(t, string(saver.uploads[0].Body), fol.ID)
}

func TestExport_Clips_DropsNotesTheExporterCannotSee(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	deps.IDGen, _ = id.NewGenerator("aidx")
	f := newVisibilityFixture(t, deps)
	clipRepo := deps.ClipRepo.(*testutil.MockClipRepository)
	clipNoteRepo := deps.ClipNoteRepo.(*testutil.MockClipNoteRepository)
	noteRepo := deps.NoteRepo.(*testutil.MockNoteRepository)
	clipRepo.Clips["c1"] = &model.Clip{ID: "c1", UserID: user.ID, Name: "clip1"}
	for i, n := range f.notes {
		// FindByIDWithRelations が User を埋めない経路 (mock) でも作者設定を引けること。
		stored := *n
		stored.User = nil
		noteRepo.Notes[n.ID] = &stored
		require.NoError(t, clipNoteRepo.Create(&model.ClipNote{ID: "cn" + string(rune('a'+i)), ClipID: "c1", NoteID: n.ID}))
	}

	_, err := transfer.NewExporter(deps).Export(context.Background(), user.ID, transfer.ExportClips)
	require.NoError(t, err)
	var arr []map[string]any
	require.NoError(t, json.Unmarshal(saver.uploads[0].Body, &arr))
	require.Len(t, arr, 1)
	var got []string
	for _, n := range arr[0]["notes"].([]any) {
		got = append(got, n.(map[string]any)["id"].(string))
	}
	assert.ElementsMatch(t, f.visible, got)
}

// IDGen 未配線で作成時刻が読めないときは、期間設定のある作者のノートを出さない。
func TestExport_Favorites_UnknownCreatedAtFailsClosed(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	f := newVisibilityFixture(t, deps)
	favRepo := deps.NoteFavoriteRepo.(*testutil.MockNoteFavoriteRepository)
	recent := f.notes[5]
	require.Equal(t, "dave", recent.UserID)
	favRepo.Favorites["fv1"] = &model.NoteFavorite{ID: "fv1", UserID: user.ID, NoteID: recent.ID, Note: recent}

	_, err := transfer.NewExporter(deps).Export(context.Background(), user.ID, transfer.ExportFavorites)
	require.NoError(t, err)
	assert.Equal(t, "[]", string(saver.uploads[0].Body))
}

type failingFindUserRepo struct {
	*testutil.MockUserRepository
	failID string
}

func (r failingFindUserRepo) FindByID(uid string) (*model.User, error) {
	if uid == r.failID {
		return nil, errors.New("db down")
	}
	return r.MockUserRepository.FindByID(uid)
}

// 作者の lookup が失敗したら黙って出力 / 省略せずエラーにする (#2792)。
func TestExport_Favorites_AuthorLookupErrorPropagates(t *testing.T) {
	_, _, deps, user := newExportDeps(t)
	deps.UserRepo = failingFindUserRepo{MockUserRepository: deps.UserRepo.(*testutil.MockUserRepository), failID: "bob"}
	favRepo := deps.NoteFavoriteRepo.(*testutil.MockNoteFavoriteRepository)
	text := "x"
	n := &model.Note{ID: "n1", UserID: "bob", Text: &text, Visibility: model.NoteVisibilityPublic}
	favRepo.Favorites["fv1"] = &model.NoteFavorite{ID: "fv1", UserID: user.ID, NoteID: "n1", Note: n}

	_, err := transfer.NewExporter(deps).Export(context.Background(), user.ID, transfer.ExportFavorites)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db down")
}

// clip 側の作者 lookup 失敗も同様に伝播する。
func TestExport_Clips_AuthorLookupErrorPropagates(t *testing.T) {
	_, _, deps, user := newExportDeps(t)
	deps.UserRepo = failingFindUserRepo{MockUserRepository: deps.UserRepo.(*testutil.MockUserRepository), failID: "bob"}
	clipRepo := deps.ClipRepo.(*testutil.MockClipRepository)
	clipNoteRepo := deps.ClipNoteRepo.(*testutil.MockClipNoteRepository)
	noteRepo := deps.NoteRepo.(*testutil.MockNoteRepository)
	clipRepo.Clips["c1"] = &model.Clip{ID: "c1", UserID: user.ID, Name: "clip1"}
	text := "x"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Text: &text, Visibility: model.NoteVisibilityPublic}
	require.NoError(t, clipNoteRepo.Create(&model.ClipNote{ID: "cn1", ClipID: "c1", NoteID: "n1"}))

	_, err := transfer.NewExporter(deps).Export(context.Background(), user.ID, transfer.ExportClips)
	require.Error(t, err)
}

// failingFollowRepo fails every follow lookup.
type failingFollowRepo struct {
	repository.FollowingRepository
}

func (failingFollowRepo) Exists(string, string) (bool, error) {
	return false, errors.New("follow lookup down")
}

// follow の lookup 障害で followers ノートを黙って落とさず、エラーにする (#2792)。
func TestExport_Favorites_FollowLookupErrorPropagates(t *testing.T) {
	_, _, deps, user := newExportDeps(t)
	deps.FollowingRepo = failingFollowRepo{FollowingRepository: deps.FollowingRepo}
	favRepo := deps.NoteFavoriteRepo.(*testutil.MockNoteFavoriteRepository)
	text := "x"
	n := &model.Note{ID: "n1", UserID: "bob", Text: &text, Visibility: model.NoteVisibilityFollowers}
	favRepo.Favorites["fv1"] = &model.NoteFavorite{ID: "fv1", UserID: user.ID, NoteID: "n1", Note: n}

	_, err := transfer.NewExporter(deps).Export(context.Background(), user.ID, transfer.ExportFavorites)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "follow lookup down")
}
