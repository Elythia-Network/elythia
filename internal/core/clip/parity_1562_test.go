package clip_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- #1562: Notes search ---

func TestNotes_SearchWordsFilter(t *testing.T) {
	svc, repo, noteRepo, notes := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1", IsPublic: true}
	hit := "misskey reversi notes"
	miss := "nothing here"
	notes.Notes["n1"] = &model.Note{ID: "n1", Text: &hit, Visibility: model.NoteVisibilityPublic}
	notes.Notes["n2"] = &model.Note{ID: "n2", Text: &miss, Visibility: model.NoteVisibilityPublic}
	noteRepo.Entries["cn1"] = &model.ClipNote{ID: "cn1", ClipID: "c1", NoteID: "n1"}
	noteRepo.Entries["cn2"] = &model.ClipNote{ID: "cn2", ClipID: "c1", NoteID: "n2"}

	// 空白区切りの 2 語は AND (両方含む note のみ)
	rows, err := svc.Notes("u1", "c1", "", "", 10, "misskey reversi")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "n1", rows[0].ID)

	// 片方の語しか持たない場合は AND を満たさない
	rows, err = svc.Notes("u1", "c1", "", "", 10, "misskey nothing")
	require.NoError(t, err)
	assert.Empty(t, rows)

	// 連続空白は upstream split(' ') の空要素 ('%%') と同じく無視される
	rows, err = svc.Notes("u1", "c1", "", "", 10, "misskey  reversi")
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

// --- #1562: Exists (unfavorite の生存在チェック) ---

func TestExists(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "u1", IsPublic: false}

	// private でも raw 存在は true (visibility 非依存)
	ok, err := svc.Exists("c1")
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = svc.Exists("missing")
	require.NoError(t, err)
	assert.False(t, ok)
}
