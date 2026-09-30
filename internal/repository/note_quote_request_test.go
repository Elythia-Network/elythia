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
	require.NoError(t, repo.MarkUpdateSent("qr_note1", model.QuoteRequestAccepted, "https://remote.example/approvals/1"))
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

// 取り消し (#3234 段階 4): 承認済みのものだけ revoked になり、承認 URI は残す
// (取り消しの Update を配り終えたかをそれで照合する)。配信の記録は状態ごと。
func TestNoteQuoteRequestRepository_Revoke(t *testing.T) {
	seedUser(t, "qv_author")
	seedQuoteAuthNote(t, "qv_note1", "qv_author")
	seedQuoteAuthNote(t, "qv_note2", "qv_author")
	repo := NewNoteQuoteRequestRepository(testDB)
	approval := "https://remote.example/approvals/qv1"

	_, err := repo.Ensure("qv_note1", "https://local.example/notes/qv_note1#quote-request")
	require.NoError(t, err)
	// 保留中のものは取り消せない。
	needSend, err := repo.MarkRevoked("qv_note1")
	require.NoError(t, err)
	assert.False(t, needSend)

	_, err = repo.MarkAccepted("qv_note1", approval)
	require.NoError(t, err)
	require.NoError(t, repo.MarkUpdateSent("qv_note1", model.QuoteRequestAccepted, approval))
	byApproval, err := repo.ListByApprovalURI(approval, 10)
	require.NoError(t, err)
	require.Len(t, byApproval, 1)
	assert.Equal(t, "qv_note1", byApproval[0].NoteID)

	needSend, err = repo.MarkRevoked("qv_note1")
	require.NoError(t, err)
	assert.True(t, needSend, "the approved Update was sent, the withdrawal was not")
	got, err := repo.FindByNoteID("qv_note1")
	require.NoError(t, err)
	assert.Equal(t, model.QuoteRequestRevoked, got.State)
	require.NotNil(t, got.ApprovalURI)
	// 承認済みのときの配信記録は、取り消しの配信には効かない。
	require.NoError(t, repo.MarkUpdateSent("qv_note1", model.QuoteRequestAccepted, approval))
	needSend, err = repo.MarkRevoked("qv_note1")
	require.NoError(t, err)
	assert.True(t, needSend)
	require.NoError(t, repo.MarkUpdateSent("qv_note1", model.QuoteRequestRevoked, approval))
	needSend, err = repo.MarkRevoked("qv_note1")
	require.NoError(t, err)
	assert.False(t, needSend)
	// 取り消された後の Accept では承認に戻さず、送り直しもさせない
	// (取り消しの配り直しが済む前でも)。
	require.NoError(t, testDB.Model(&model.NoteQuoteRequest{}).Where(`"noteId" = ?`, "qv_note1").Update("updateSent", false).Error)
	needSend, err = repo.MarkAccepted("qv_note1", approval)
	require.NoError(t, err)
	assert.False(t, needSend)

	// 同じ承認 URI の記録が複数あれば全部返す (上限まで)。
	_, err = repo.Ensure("qv_note2", "https://local.example/notes/qv_note2#quote-request")
	require.NoError(t, err)
	_, err = repo.MarkAccepted("qv_note2", approval)
	require.NoError(t, err)
	byApproval, err = repo.ListByApprovalURI(approval, 10)
	require.NoError(t, err)
	assert.Len(t, byApproval, 2)
	byApproval, err = repo.ListByApprovalURI(approval, 1)
	require.NoError(t, err)
	assert.Len(t, byApproval, 1)

	rows, err := repo.ListByApprovalURI("https://remote.example/approvals/none", 10)
	require.NoError(t, err)
	assert.Empty(t, rows)
	rows, err = repo.ListByApprovalURI("https://x/\x00", 10)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestNoteQuoteAuthorizationRepository_DeleteByAuthorAndQuoter(t *testing.T) {
	seedUser(t, "qd_author")
	seedUser(t, "qd_other")
	seedUser(t, "qd_quoter")
	seedUser(t, "qd_quoter2")
	seedQuoteAuthNote(t, "qd_note1", "qd_author")
	seedQuoteAuthNote(t, "qd_note2", "qd_author")
	seedQuoteAuthNote(t, "qd_note3", "qd_other")
	repo := NewNoteQuoteAuthorizationRepository(testDB)
	for _, a := range []model.NoteQuoteAuthorization{
		{ID: "qd_a1", NoteID: "qd_note1", QuoterID: "qd_quoter", QuotingURI: "https://r.example/q1"},
		{ID: "qd_a2", NoteID: "qd_note2", QuoterID: "qd_quoter", QuotingURI: "https://r.example/q2"},
		{ID: "qd_a3", NoteID: "qd_note3", QuoterID: "qd_quoter", QuotingURI: "https://r.example/q3"},
		{ID: "qd_a4", NoteID: "qd_note1", QuoterID: "qd_quoter2", QuotingURI: "https://r.example/q4"},
	} {
		a := a
		_, err := repo.Ensure(&a)
		require.NoError(t, err)
	}

	rows, err := repo.DeleteByAuthorAndQuoter("qd_author", "qd_quoter")
	require.NoError(t, err)
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.ID)
		assert.NotEmpty(t, r.QuotingURI)
	}
	assert.ElementsMatch(t, []string{"qd_a1", "qd_a2"}, ids)
	// 別の作者の投稿への承認と、別の相手への承認は残る。
	for _, keep := range [][2]string{{"qd_a3", "qd_note3"}, {"qd_a4", "qd_note1"}} {
		_, err := repo.FindByIDAndNoteID(keep[0], keep[1])
		assert.NoError(t, err, keep[0])
	}
	_, err = repo.FindByIDAndNoteID("qd_a1", "qd_note1")
	assert.True(t, IsNotFound(err))

	rows, err = repo.DeleteByAuthorAndQuoter("qd_author", "qd_quoter")
	require.NoError(t, err)
	assert.Empty(t, rows)
	rows, err = repo.DeleteByAuthorAndQuoter("a\x00", "qd_quoter")
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestNoteQuoteAuthorizationRepository_Remove(t *testing.T) {
	seedUser(t, "qm_author")
	seedUser(t, "qm_quoter")
	seedUser(t, "qm_other")
	seedQuoteAuthNote(t, "qm_note1", "qm_author")
	repo := NewNoteQuoteAuthorizationRepository(testDB)
	for _, a := range []model.NoteQuoteAuthorization{
		{ID: "qm_1", NoteID: "qm_note1", QuoterID: "qm_quoter", QuotingURI: "https://r.example/m1"},
		{ID: "qm_2", NoteID: "qm_note1", QuoterID: "qm_quoter", QuotingURI: "https://r.example/m2"},
		{ID: "qm_3", NoteID: "qm_note1", QuoterID: "qm_other", QuotingURI: "https://r.example/m3"},
	} {
		a := a
		_, err := repo.Ensure(&a)
		require.NoError(t, err)
	}
	require.NoError(t, repo.Remove("qm_note1", "https://r.example/m1", "qm_quoter"))
	_, err := repo.FindByNoteIDAndQuotingURI("qm_note1", "https://r.example/m1")
	assert.True(t, IsNotFound(err))
	_, err = repo.FindByNoteIDAndQuotingURI("qm_note1", "https://r.example/m2")
	assert.NoError(t, err, "other quotes keep their approval")
	// 他人の承認は、その引用 URI を指定されても消さない。
	require.NoError(t, repo.Remove("qm_note1", "https://r.example/m3", "qm_quoter"))
	_, err = repo.FindByNoteIDAndQuotingURI("qm_note1", "https://r.example/m3")
	assert.NoError(t, err)
	require.NoError(t, repo.Remove("qm_note1", "https://r.example/none", "qm_quoter"))
	require.NoError(t, repo.Remove("a\x00", "b", "c"))
}
