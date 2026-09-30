package repository

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/model"
)

func TestNoteQuoteRequestRepository(t *testing.T) {
	seedUser(t, "qr_author")
	seedQuoteAuthNote(t, "qr_note1", "qr_author")
	seedQuoteAuthNote(t, "qr_note2", "qr_author")
	repo := NewNoteQuoteRequestRepository(testDB)
	reqURI := "https://local.example/notes/qr_note1#quote-request"

	q, err := repo.Ensure("qr_note1", reqURI)
	require.NoError(t, err)
	assert.Equal(t, model.QuoteRequestPending, q.State)
	assert.Nil(t, q.ApprovalURI)

	byURI, err := repo.FindByRequestURI(reqURI)
	require.NoError(t, err)
	assert.Equal(t, "qr_note1", byURI.NoteID)

	needSend, err := repo.MarkAccepted("qr_note1", "https://remote.example/approvals/1")
	require.NoError(t, err)
	assert.True(t, needSend)
	// 配り終えるまでは、同じ承認でも「まだ配っていない」。
	needSend, err = repo.MarkAccepted("qr_note1", "https://remote.example/approvals/1")
	require.NoError(t, err)
	assert.True(t, needSend)
	require.NoError(t, repo.MarkUpdateSent("qr_note1", "https://remote.example/approvals/1"))
	needSend, err = repo.MarkAccepted("qr_note1", "https://remote.example/approvals/1")
	require.NoError(t, err)
	assert.False(t, needSend, "delivered once, not again for the same approval")
	// 承認済みのものは、送り直し (Ensure) でも Reject でも状態を変えない。
	again, err := repo.Ensure("qr_note1", reqURI)
	require.NoError(t, err)
	assert.Equal(t, model.QuoteRequestAccepted, again.State)
	require.NoError(t, repo.MarkRejected("qr_note1"))
	got, err := repo.FindByNoteID("qr_note1")
	require.NoError(t, err)
	assert.Equal(t, model.QuoteRequestAccepted, got.State)
	require.NotNil(t, got.ApprovalURI)
	assert.Equal(t, "https://remote.example/approvals/1", *got.ApprovalURI)

	// 承認済みのものは、別の承認 URI の Accept では変えない (配り直しもしない)。
	// まだ配り終えていないときも同じ (記録された承認とは別物なので)。
	require.NoError(t, testDB.Model(&model.NoteQuoteRequest{}).Where(`"noteId" = ?`, "qr_note1").Update("updateSent", false).Error)
	needSend, err = repo.MarkAccepted("qr_note1", "https://remote.example/approvals/2")
	require.NoError(t, err)
	assert.False(t, needSend)
	got, err = repo.FindByNoteID("qr_note1")
	require.NoError(t, err)
	assert.Equal(t, "https://remote.example/approvals/1", *got.ApprovalURI)

	// pending のものは Reject で rejected になる。
	_, err = repo.Ensure("qr_note2", "https://local.example/notes/qr_note2#quote-request")
	require.NoError(t, err)
	require.NoError(t, repo.MarkRejected("qr_note2"))
	got, err = repo.FindByNoteID("qr_note2")
	require.NoError(t, err)
	assert.Equal(t, model.QuoteRequestRejected, got.State)
	// 拒否されたものは、後から Accept が届いても承認に戻さない。
	needSend, err = repo.MarkAccepted("qr_note2", "https://remote.example/approvals/3")
	require.NoError(t, err)
	assert.False(t, needSend)
	got, err = repo.FindByNoteID("qr_note2")
	require.NoError(t, err)
	assert.Equal(t, model.QuoteRequestRejected, got.State)
	assert.Nil(t, got.ApprovalURI)

	// 投稿が消えたら記録も消える。
	require.NoError(t, testDB.Exec(`DELETE FROM "note" WHERE id = ?`, "qr_note1").Error)
	_, err = repo.FindByNoteID("qr_note1")
	assert.True(t, IsNotFound(err))
}

func TestNoteQuoteRequestRepository_UnstorableIsNotFound(t *testing.T) {
	repo := NewNoteQuoteRequestRepository(testDB)
	_, err := repo.FindByNoteID("a\x00b")
	assert.True(t, IsNotFound(err))
	_, err = repo.FindByRequestURI("https://x/\x00")
	assert.True(t, IsNotFound(err))
}
