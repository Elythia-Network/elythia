package testutil

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The mock's FindManyByIDsWithKnownUsers attaches the authors in known like the
// repository does, keeps the stored User for the others, leaves the stored
// notes untouched, and fails with FindManyErr (#4273).
func TestMockNoteRepository_FindManyByIDsWithKnownUsers(t *testing.T) {
	repo := NewMockNoteRepository()
	storedA := &model.User{ID: "a", Username: "stored_a"}
	storedB := &model.User{ID: "b", Username: "stored_b"}
	repo.Notes["n1"] = &model.Note{ID: "n1", UserID: "a", User: storedA}
	repo.Notes["n2"] = &model.Note{ID: "n2", UserID: "b", User: storedB}

	knownA := &model.User{ID: "a", Username: "known_a"}
	out, err := repo.FindManyByIDsWithKnownUsers([]string{"n1", "n2"}, map[string]*model.User{"a": knownA})
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Same(t, knownA, out[0].User, "an author in known must be attached as is")
	assert.Same(t, storedB, out[1].User, "an author missing from known keeps the stored User")
	assert.Same(t, storedA, repo.Notes["n1"].User, "the stored note must not be modified")

	repo.FindManyErr = errors.New("boom")
	_, err = repo.FindManyByIDsWithKnownUsers([]string{"n1"}, nil)
	assert.ErrorIs(t, err, repo.FindManyErr)
	_, err = repo.FindManyByIDsWithUser([]string{"n1"})
	assert.ErrorIs(t, err, repo.FindManyErr)
}
