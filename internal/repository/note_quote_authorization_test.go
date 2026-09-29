package repository

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/model"
)

func seedQuoteAuthNote(t *testing.T, noteID, userID string) {
	t.Helper()
	require.NoError(t, testDB.Create(&model.Note{ID: noteID, UserID: userID, Visibility: model.NoteVisibilityPublic}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "note" WHERE id = ?`, noteID) })
}

func TestNoteQuoteAuthorizationRepository(t *testing.T) {
	seedUser(t, "qa_author")
	seedUser(t, "qa_quoter")
	seedQuoteAuthNote(t, "qa_note1", "qa_author")
	seedQuoteAuthNote(t, "qa_note2", "qa_author")
	repo := NewNoteQuoteAuthorizationRepository(testDB)
	req := "https://remote.example/quote-requests/1"

	a, err := repo.Ensure(&model.NoteQuoteAuthorization{ID: "qa_1", NoteID: "qa_note1", QuoterID: "qa_quoter",
		QuotingURI: "https://remote.example/notes/q1", RequestID: &req})
	require.NoError(t, err)
	assert.Equal(t, "qa_1", a.ID)

	// 同じ引用の再送は、最初の承認をそのまま返す (URI を変えない)。
	again, err := repo.Ensure(&model.NoteQuoteAuthorization{ID: "qa_2", NoteID: "qa_note1", QuoterID: "qa_quoter",
		QuotingURI: "https://remote.example/notes/q1"})
	require.NoError(t, err)
	assert.Equal(t, "qa_1", again.ID)
	require.NotNil(t, again.RequestID)
	assert.Equal(t, req, *again.RequestID)

	// 別の投稿への同じ引用は別の承認。
	other, err := repo.Ensure(&model.NoteQuoteAuthorization{ID: "qa_3", NoteID: "qa_note2", QuoterID: "qa_quoter",
		QuotingURI: "https://remote.example/notes/q1"})
	require.NoError(t, err)
	assert.Equal(t, "qa_3", other.ID)

	got, err := repo.FindByIDAndNoteID("qa_1", "qa_note1")
	require.NoError(t, err)
	assert.Equal(t, "https://remote.example/notes/q1", got.QuotingURI)
	// 投稿と組でしか引けない (別の投稿の URL で承認を引けない)。
	_, err = repo.FindByIDAndNoteID("qa_1", "qa_note2")
	assert.True(t, IsNotFound(err))
	_, err = repo.FindByIDAndNoteID("qa\x00", "qa_note1")
	assert.True(t, IsNotFound(err))
	_, err = repo.FindByNoteIDAndQuotingURI("qa_note1", "bad\x00")
	assert.True(t, IsNotFound(err))

	// 投稿が消えると承認も消える (相手側の引用は未承認に戻る)。
	require.NoError(t, testDB.Exec(`DELETE FROM "note" WHERE id = ?`, "qa_note1").Error)
	_, err = repo.FindByIDAndNoteID("qa_1", "qa_note1")
	assert.True(t, IsNotFound(err))
}
