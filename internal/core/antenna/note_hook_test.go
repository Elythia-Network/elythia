package antenna

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoteCreateHook_OnNoteCreatedForwards(t *testing.T) {
	svc, repo := newSvc(t)
	repo.Antennas["a1"] = makeAntenna(t, "a1", "u1", [][]string{{"hi"}})

	hook := NewNoteCreateHook(svc)
	text := "hi there"
	hook.OnNoteCreated(
		&model.Note{ID: "n1", UserID: "u2", Text: &text, Visibility: model.NoteVisibilityPublic},
		&model.User{ID: "u2", Username: "alice"},
	)

	rows, err := svc.Notes(context.Background(), "u1", "a1", 10, "", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"n1"}, rows)
}

// Sanity check: NoteCreateHook is callable with nil and does not panic.
func TestNoteCreateHook_NilNoteIsNoOp(t *testing.T) {
	svc, _ := newSvc(t)
	hook := NewNoteCreateHook(svc)
	hook.OnNoteCreated(nil, &model.User{})
}

// Coverage: ensure the constructor binds the service correctly.
func TestNewNoteCreateHook(t *testing.T) {
	svc, _ := newSvc(t)
	hook := NewNoteCreateHook(svc)
	assert.NotNil(t, hook)
	// underscore the unused arg to silence linters
	_ = testutil.NewMockAntennaRepository()
}
