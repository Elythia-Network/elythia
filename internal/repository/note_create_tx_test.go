package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestNoteRepository_CreateWithTxUsesSuppliedTransaction(t *testing.T) {
	user := insertTestUser(t, "nctx01", "nctxuser")
	defer cleanupUser(t, user.ID)
	repo := NewNoteRepository(testDB)
	note := &model.Note{ID: "nctx01", UserID: user.ID, Visibility: model.NoteVisibilityPublic}
	tx := testDB.Begin()
	require.NoError(t, tx.Error)
	require.NoError(t, repo.CreateWithTx(note, tx))
	require.NoError(t, tx.Rollback().Error)
	_, err := repo.FindByID(note.ID)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}
