package reaction_test

import (
	"context"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/core/reaction"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingMaterializer promotes a note into the repo on demand and records
// that it was asked to.
type recordingMaterializer struct {
	promote map[string]*model.Note
	repo    interface{ Create(*model.Note) error }
	asked   []string
	err     error
}

func (m *recordingMaterializer) EnsureNote(_ context.Context, noteID string) (*model.Note, error) {
	m.asked = append(m.asked, noteID)
	if m.err != nil {
		return nil, m.err
	}
	n, ok := m.promote[noteID]
	if !ok {
		return nil, errors.New("not found")
	}
	if err := m.repo.Create(n); err != nil {
		return nil, err
	}
	return n, nil
}

func newServiceForMaterialize(t *testing.T) (*reaction.Service, *testutil.MockNoteRepository) {
	t.Helper()
	svc, repo, _, _, _ := newService(t)
	return svc, repo
}

// DB に無いノートへのリアクションで materialize が走り、成功すること。
// note_reaction.noteId は note への外部キーなので、行が無いと INSERT できない。
func TestCreate_MaterializesEphemeralNote(t *testing.T) {
	svc, repo := newServiceForMaterialize(t)
	host := "remote.example"
	ephID := "eph-note"
	mat := &recordingMaterializer{
		repo: repo,
		promote: map[string]*model.Note{
			ephID: {ID: ephID, UserID: "ra", UserHost: &host, Visibility: model.NoteVisibilityPublic},
		},
	}
	svc.SetNoteMaterializer(mat)

	_, err := svc.Create(&model.User{ID: "local1"}, ephID, "👍")
	require.NoError(t, err, "materialize 後にリアクションできること")
	assert.Equal(t, []string{ephID}, mat.asked)
	assert.Contains(t, repo.Notes, ephID, "DB 行が作られていること")
}

// 通常のノート (DB にある) では materializer を一切呼ばない。
// ホットパスに追加コストを載せないための保証。
func TestCreate_DoesNotCallMaterializerForDBNote(t *testing.T) {
	svc, repo := newServiceForMaterialize(t)
	repo.Notes["db1"] = &model.Note{ID: "db1", UserID: "author", Visibility: model.NoteVisibilityPublic}
	mat := &recordingMaterializer{repo: repo, promote: map[string]*model.Note{}}
	svc.SetNoteMaterializer(mat)

	_, err := svc.Create(&model.User{ID: "local1"}, "db1", "👍")
	require.NoError(t, err)
	assert.Empty(t, mat.asked, "DB にあるノートでは materializer を呼ばない")
}

// materialize にも失敗したら従来どおり ErrNoteNotFound。
func TestCreate_MaterializeFailureKeepsNotFound(t *testing.T) {
	svc, repo := newServiceForMaterialize(t)
	mat := &recordingMaterializer{repo: repo, err: errors.New("gone")}
	svc.SetNoteMaterializer(mat)

	_, err := svc.Create(&model.User{ID: "local1"}, "ghost", "👍")
	assert.ErrorIs(t, err, reaction.ErrNoteNotFound)
}

// materializer 未配線でも従来どおり動く。
func TestCreate_NoMaterializerWired(t *testing.T) {
	svc, _ := newServiceForMaterialize(t)
	_, err := svc.Create(&model.User{ID: "local1"}, "ghost", "👍")
	assert.ErrorIs(t, err, reaction.ErrNoteNotFound)
}
