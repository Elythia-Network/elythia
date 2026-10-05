package poll_test

import (
	"context"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// promotingMaterializer creates the note in the repo when asked, mimicking
// core/ephemeral.Materializer.
type promotingMaterializer struct {
	seed  func()
	asked []string
	err   error
}

func (m *promotingMaterializer) EnsureNote(_ context.Context, noteID string) (*model.Note, error) {
	m.asked = append(m.asked, noteID)
	if m.err != nil {
		return nil, m.err
	}
	m.seed()
	return nil, nil
}

// DB に無い投票対象で materialize が走ること。
// poll_vote.noteId は note への外部キーなので、行が無いと INSERT できない。
func TestVote_MaterializesEphemeralNote(t *testing.T) {
	svc, noteRepo, pollRepo, _ := newSvc(t)
	const noteID = "eph-poll"
	mat := &promotingMaterializer{seed: func() {
		seedPollNote(noteRepo, pollRepo, noteID, "ra", false, nil)
	}}
	svc.SetNoteMaterializer(mat)

	err := svc.Vote(&model.User{ID: "local1"}, noteID, 0)
	require.NoError(t, err, "materialize 後に投票できること")
	assert.Equal(t, []string{noteID}, mat.asked)
}

// DB にある投票対象では materializer を呼ばない (ホットパスに追加コストを載せない)。
func TestVote_DoesNotCallMaterializerForDBNote(t *testing.T) {
	svc, noteRepo, pollRepo, _ := newSvc(t)
	seedPollNote(noteRepo, pollRepo, "db-poll", "author", false, nil)
	mat := &promotingMaterializer{seed: func() {}}
	svc.SetNoteMaterializer(mat)

	require.NoError(t, svc.Vote(&model.User{ID: "local1"}, "db-poll", 0))
	assert.Empty(t, mat.asked)
}

func TestVote_MaterializeFailureKeepsError(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	svc.SetNoteMaterializer(&promotingMaterializer{seed: func() {}, err: errors.New("gone")})

	assert.Error(t, svc.Vote(&model.User{ID: "local1"}, "ghost", 0))
}

func TestVote_NoMaterializerWired(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	assert.Error(t, svc.Vote(&model.User{ID: "local1"}, "ghost", 0))
}
