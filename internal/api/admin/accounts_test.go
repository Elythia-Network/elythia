package admin_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- UpdateProxyAccount ------------------------------------------------------

// stubSystemAccountFetcher is a minimal SystemAccountFetcher returning a
// pre-configured user for any kind.
type stubSystemAccountFetcher struct {
	user *model.User
	err  error
}

func (s *stubSystemAccountFetcher) Fetch(_ string) (*model.User, error) {
	return s.user, s.err
}

func TestUpdateProxyAccount_UpdatesDescription(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	proxy := &model.User{ID: "u-proxy", Username: "proxy.actor", UsernameLower: "proxy.actor"}
	require.NoError(t, userRepo.Create(proxy))
	require.NoError(t, userRepo.CreateProfile(&model.UserProfile{UserID: proxy.ID}))
	h.SetSystemAccountFetcher(&stubSystemAccountFetcher{user: proxy})

	rec := doPost(h.UpdateProxyAccount, `{"description":"hello"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	prof, err := userRepo.FindProfileByUserID(proxy.ID)
	require.NoError(t, err)
	require.NotNil(t, prof.Description)
	assert.Equal(t, "hello", *prof.Description)
}

func TestUpdateProxyAccount_NoFetcherIsNoOp(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.UpdateProxyAccount, `{"description":"x"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestUpdateProxyAccount_FetcherErrorReturnsInternal(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetSystemAccountFetcher(&stubSystemAccountFetcher{err: errors.New("boom")})
	rec := doPost(h.UpdateProxyAccount, `{"description":"x"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// description が undefined (null/省略) のときは profile を更新しないが、
// systemAccountFetcher 経由で取得した user は返す。
func TestUpdateProxyAccount_NoDescriptionDoesNotUpdateProfile(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	proxy := &model.User{ID: "u-noop", Username: "proxy.actor", UsernameLower: "proxy.actor"}
	require.NoError(t, userRepo.Create(proxy))
	require.NoError(t, userRepo.CreateProfile(&model.UserProfile{UserID: proxy.ID}))
	h.SetSystemAccountFetcher(&stubSystemAccountFetcher{user: proxy})

	rec := doPost(h.UpdateProxyAccount, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	prof, err := userRepo.FindProfileByUserID(proxy.ID)
	require.NoError(t, err)
	assert.Nil(t, prof.Description)
}

// --- /admin/accounts/delete + /admin/delete-account (#574 で stub から
// 移行した本実装テスト) ---

// stubDeleteAccountEnqueuer captures EnqueueDeleteAccount payloads so
// tests can assert that AccountsDelete / DeleteAccount schedule cascade.
type stubDeleteAccountEnqueuer struct {
	lastUserID string
	called     int
	err        error
}

func (s *stubDeleteAccountEnqueuer) EnqueueDeleteAccount(payload queue.DeleteAccountPayload) error {
	s.called++
	s.lastUserID = payload.UserID
	return s.err
}

func TestAccountsDelete(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusBadRequest, doPost(h.AccountsDelete, `{}`, adminUser).Code)
}

func TestAccountsDelete_EnqueuesCascade(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubDeleteAccountEnqueuer{}
	h.SetDeleteAccountEnqueuer(stub)
	assert.Equal(t, http.StatusNoContent,
		doPost(h.AccountsDelete, `{"userId":"u1"}`, adminUser).Code)
	assert.Equal(t, 1, stub.called)
	assert.Equal(t, "u1", stub.lastUserID)
}

func TestAccountsDelete_MissingUserIDSkipsEnqueue(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubDeleteAccountEnqueuer{}
	h.SetDeleteAccountEnqueuer(stub)
	assert.Equal(t, http.StatusBadRequest,
		doPost(h.AccountsDelete, `{}`, adminUser).Code)
	assert.Equal(t, 0, stub.called)
}

func TestAccountsDelete_EnqueueFailureIsLogged(t *testing.T) {
	// enqueue 失敗はログに残すだけで HTTP 応答は 204 のまま返ることを確認。
	h, _, _, _ := newTestHandler(t)
	h.SetDeleteAccountEnqueuer(&stubDeleteAccountEnqueuer{err: errors.New("boom")})
	assert.Equal(t, http.StatusNoContent,
		doPost(h.AccountsDelete, `{"userId":"u1"}`, adminUser).Code)
}

// root アカウントは削除できない (誤操作 / 権限昇格対策)。
func TestAccountsDelete_RejectsRoot(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["root"] = &model.User{ID: "root", IsRoot: true}
	stub := &stubDeleteAccountEnqueuer{}
	h.SetDeleteAccountEnqueuer(stub)
	rec := doPost(h.AccountsDelete, `{"userId":"root"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, stub.called, "root deletion must not enqueue cascade")
	assert.False(t, userRepo.Users["root"].IsDeleted)
}

func TestDeleteAccount_RejectsRoot(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["root"] = &model.User{ID: "root", IsRoot: true}
	rec := doPost(h.DeleteAccount, `{"userId":"root"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.False(t, userRepo.Users["root"].IsDeleted)
}

// ローカル system account (host=nil, username に '.' を含む, IsRoot=false) も
// 削除拒否される (upstream の system-account ガード相当)。
func TestAccountsDelete_RejectsSystemAccount(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["sys"] = &model.User{ID: "sys", Username: "relay.actor", Host: nil, IsRoot: false}
	stub := &stubDeleteAccountEnqueuer{}
	h.SetDeleteAccountEnqueuer(stub)
	rec := doPost(h.AccountsDelete, `{"userId":"sys"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, stub.called, "system account deletion must not enqueue cascade")
	assert.False(t, userRepo.Users["sys"].IsDeleted)
}

// root user id が meta.rootUserId で示されるケース (IsRoot 列が無い drop-in) も
// 削除拒否される。
func TestDeleteAccount_RejectsMetaRootUser(t *testing.T) {
	h, userRepo, metaRepo, _ := newTestHandler(t)
	rootID := "metaRoot"
	metaRepo.Meta = &model.Meta{ID: "x", RootUserID: &rootID}
	userRepo.Users["metaRoot"] = &model.User{ID: "metaRoot", Username: "alice", IsRoot: false}
	rec := doPost(h.DeleteAccount, `{"userId":"metaRoot"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.False(t, userRepo.Users["metaRoot"].IsDeleted)
}

// #965: 論理削除直後の auth bypass 防止のため、AccountsDelete / DeleteAccount
// 成功時に target user の全 token cache を即時 invalidate することを担保。
func TestAccountsDelete_InvalidatesTargetTokenCache(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	inv := &stubUserTokenInvalidator{}
	h.SetUserTokenInvalidator(inv)

	rec := doPost(h.AccountsDelete, `{"userId":"u1"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, []string{"u1"}, inv.calls,
		"AccountsDelete 成功時は target の全 token cache を invalidate するべき")
}

func TestDeleteAccount_InvalidatesTargetTokenCache(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u9"] = &model.User{ID: "u9"}
	inv := &stubUserTokenInvalidator{}
	h.SetUserTokenInvalidator(inv)

	rec := doPost(h.DeleteAccount, `{"userId":"u9"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, []string{"u9"}, inv.calls,
		"DeleteAccount (admin variant) 成功時も同様")
}

// userId 空のときは 400 で止まり、UpdateUser を呼ばないので invalidate も
// skip する (本家は paramDef の required で弾く、#3330)。
func TestAccountsDelete_EmptyUserIDDoesNotInvalidate(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	inv := &stubUserTokenInvalidator{}
	h.SetUserTokenInvalidator(inv)

	rec := doPost(h.AccountsDelete, `{}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, inv.calls, "userId 空のとき invalidate は呼ばれない")
}

func TestDeleteAccountAdmin(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusBadRequest, doPost(h.DeleteAccount, `{}`, adminUser).Code)
}

func TestDeleteAccount_EnqueuesCascade(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubDeleteAccountEnqueuer{}
	h.SetDeleteAccountEnqueuer(stub)
	assert.Equal(t, http.StatusNoContent,
		doPost(h.DeleteAccount, `{"userId":"u9"}`, adminUser).Code)
	assert.Equal(t, "u9", stub.lastUserID)
}

// --- /admin/accounts/find-by-email ---

func TestAccountsFindByEmail_Empty(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusBadRequest, doPost(h.AccountsFindByEmail, `{}`, adminUser).Code)
}

func TestAccountsFindByEmail_NotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusBadRequest, doPost(h.AccountsFindByEmail, `{"email":"ghost@example.com"}`, adminUser).Code)
}

func TestAccountsFindByEmail_Found(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	email := "alice@example.com"
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Profiles["alice"] = &model.UserProfile{UserID: "alice", Email: &email}

	rec := doPost(h.AccountsFindByEmail, `{"email":"alice@example.com"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"alice"`)
}

// TestAccountsFindByEmail_ResponseShape verifies that AccountsFindByEmail
// returns the UserDetailedNotMe shape (#1847): frontend-expected fields
// (createdAt, roles) present; internal model fields (inbox, sharedInbox,
// usernameLower) NOT leaked; and includeSecrets-only fields (email,
// emailVerified, securityKeysList) NOT leaked. policies is a MeDetailed-only
// field absent from UserDetailedNotMe (matches upstream).
func TestAccountsFindByEmail_ResponseShape(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)

	// aidx で生成した ID を使い、createdAt がパースされることを保証する
	idGen, _ := id.NewGenerator("aidx")
	uid := idGen.Generate(time.Now())

	inbox := "https://remote.example/inbox"
	sharedInbox := "https://remote.example/sharedInbox"
	email := "bob@example.com"
	userRepo.Users[uid] = &model.User{
		ID:                uid,
		Username:          "bob",
		UsernameLower:     "bob",
		Inbox:             &inbox,
		SharedInbox:       &sharedInbox,
		IsExplorable:      true,
		AvatarDecorations: []byte("[]"),
	}
	userRepo.Profiles[uid] = &model.UserProfile{
		UserID:          uid,
		Email:           &email,
		MutedWords:      []byte("[]"),
		HardMutedWords:  []byte("[]"),
		MutedInstances:  []byte("[]"),
		PublicReactions: true,
	}

	rec := doPost(h.AccountsFindByEmail, `{"email":"bob@example.com"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// UserDetailed が付与する frontend 必須フィールドの存在確認
	assert.Equal(t, uid, resp["id"])
	assert.NotNil(t, resp["createdAt"], "createdAt must be present")
	assert.NotNil(t, resp["roles"], "roles must be present")
	// policies は MeDetailed 限定 field で UserDetailedNotMe には無い (upstream 一致)。
	_, hasPolicies := resp["policies"]
	assert.False(t, hasPolicies, "policies must NOT be present (UserDetailedNotMe shape)")

	// includeSecrets 限定 field が漏れていないこと (#1847)。
	_, hasEmail := resp["email"]
	assert.False(t, hasEmail, "email must not be exposed (#1847)")
	_, hasEmailVerified := resp["emailVerified"]
	assert.False(t, hasEmailVerified, "emailVerified must not be exposed (#1847)")
	_, hasSecurityKeysList := resp["securityKeysList"]
	assert.False(t, hasSecurityKeysList, "securityKeysList must not be exposed (#1847)")

	// 内部フィールドが response に漏れていないことを確認
	_, hasInbox := resp["inbox"]
	assert.False(t, hasInbox, "inbox is an internal field and must not be exposed")
	_, hasSharedInbox := resp["sharedInbox"]
	assert.False(t, hasSharedInbox, "sharedInbox is an internal field and must not be exposed")
	_, hasUsernameLower := resp["usernameLower"]
	assert.False(t, hasUsernameLower, "usernameLower is an internal field and must not be exposed")
}

func TestUpdateProxyAccount_WritesModerationLog(t *testing.T) {
	// #664: updateProxyAccountDescription log を出す (before:null, after:string)。
	h, userRepo, _, _ := newTestHandler(t)
	proxy := &model.User{ID: "u-proxy", Username: "proxy.actor", UsernameLower: "proxy.actor"}
	require.NoError(t, userRepo.Create(proxy))
	require.NoError(t, userRepo.CreateProfile(&model.UserProfile{UserID: proxy.ID}))
	h.SetSystemAccountFetcher(&stubSystemAccountFetcher{user: proxy})
	repo := attachModLog(t, h)

	rec := doPost(h.UpdateProxyAccount, `{"description":"hello"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "updateProxyAccountDescription", repo.Snapshot()[0].Type)
}

func TestAccountsDelete_WritesModerationLog(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	repo := attachModLog(t, h)

	rec := doPost(h.AccountsDelete, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	logs := repo.Snapshot()
	assert.Equal(t, "deleteAccount", logs[0].Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	assert.Equal(t, "u1", info["userId"])
}

func TestDeleteAccount_WritesModerationLog(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["u9"] = &model.User{ID: "u9", Username: "bob"}
	repo := attachModLog(t, h)

	rec := doPost(h.DeleteAccount, `{"userId":"u9"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "deleteAccount", repo.Snapshot()[0].Type)
}
