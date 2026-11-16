package note_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/model"
)

// BeforeInsert sees the ID the note is stored under, before it is stored
// (#4199).
func TestCreateService_BeforeInsert_SeesStoredID(t *testing.T) {
	svc, noteRepo, _ := newCreateService(t)
	user := &model.User{ID: "user1", Username: "alice"}
	text := "Hello"
	var seen string
	created, err := svc.Create(note.CreateInput{
		User: user, Text: &text, Visibility: model.NoteVisibilityPublic,
		BeforeInsert: func(noteID string) error {
			seen = noteID
			assert.Empty(t, noteRepo.Notes, "the note must not be stored yet")
			return nil
		},
	})
	require.NoError(t, err)
	assert.Equal(t, created.ID, seen)
	assert.Len(t, noteRepo.Notes, 1)
}

func TestCreateService_BeforeInsert_ErrorAborts(t *testing.T) {
	svc, noteRepo, _ := newCreateService(t)
	user := &model.User{ID: "user1", Username: "alice"}
	text := "Hello"
	errRejected := errors.New("rejected")
	created, err := svc.Create(note.CreateInput{
		User: user, Text: &text, Visibility: model.NoteVisibilityPublic,
		BeforeInsert: func(string) error { return errRejected },
	})
	require.ErrorIs(t, err, errRejected)
	assert.Nil(t, created)
	assert.Empty(t, noteRepo.Notes)
}

// A request that fails an earlier check never reaches BeforeInsert, so a
// plugin does not store targets for a note that is never created.
func TestCreateService_BeforeInsert_NotCalledWhenInvalid(t *testing.T) {
	svc, _, _ := newCreateService(t)
	user := &model.User{ID: "user1", Username: "alice"}
	called := false
	_, err := svc.Create(note.CreateInput{
		User: user, Visibility: model.NoteVisibilityPublic,
		BeforeInsert: func(string) error { called = true; return nil },
	})
	require.Error(t, err)
	assert.False(t, called)
}
