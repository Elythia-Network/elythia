package admin_test

import (
	"net/http"
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubUserStreamRevoker struct {
	calls []string
}

func (s *stubUserStreamRevoker) RevokeUserStreams(userID string) {
	s.calls = append(s.calls, userID)
}

// 凍結した利用者の既存 WebSocket を閉じる。tokenCache を落とすだけでは、接続時に
// 1 度しか認証しない WebSocket は閉じない。
func TestSuspendUser_RevokesTargetStreams(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "target"}
	rev := &stubUserStreamRevoker{}
	h.SetUserStreamRevoker(rev)
	assert.True(t, h.HasUserStreamRevoker())

	rec := doPost(h.SuspendUser, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"u1"}, rev.calls)
}

// 凍結を拒否したときは閉じない。
func TestSuspendUser_RejectedDoesNotRevokeStreams(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["root"] = &model.User{ID: "root", IsRoot: true}
	rev := &stubUserStreamRevoker{}
	h.SetUserStreamRevoker(rev)

	rec := doPost(h.SuspendUser, `{"userId":"root"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, rev.calls)
}

// 凍結解除では閉じない (凍結中の利用者の token では接続できない)。
func TestUnsuspendUser_DoesNotRevokeStreams(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "target", IsSuspended: true}
	rev := &stubUserStreamRevoker{}
	h.SetUserStreamRevoker(rev)

	rec := doPost(h.UnsuspendUser, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rev.calls)
}

func TestAccountsDelete_RevokesTargetStreams(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	rev := &stubUserStreamRevoker{}
	h.SetUserStreamRevoker(rev)

	rec := doPost(h.AccountsDelete, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"u1"}, rev.calls)
}

func TestDeleteAccount_RevokesTargetStreams(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u9"] = &model.User{ID: "u9"}
	rev := &stubUserStreamRevoker{}
	h.SetUserStreamRevoker(rev)

	rec := doPost(h.DeleteAccount, `{"userId":"u9"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"u9"}, rev.calls)
}

// userId が空なら何もしない (空の userId は「全員」の意味ではない)。
func TestAccountsDelete_EmptyUserIDDoesNotRevokeStreams(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rev := &stubUserStreamRevoker{}
	h.SetUserStreamRevoker(rev)

	rec := doPost(h.AccountsDelete, `{}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rev.calls)
}

func TestHasUserStreamRevoker_Unwired(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.False(t, h.HasUserStreamRevoker())
}
