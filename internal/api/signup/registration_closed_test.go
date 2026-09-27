package signup_test

import (
	"net/http"
	"testing"

	apisignup "github.com/shiroha-a/mk/internal/api/signup"
	coresignup "github.com/shiroha-a/mk/internal/core/signup"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 「受け付けない」(#3186) の間は、途中まで進んでいる登録も含めて 4 経路すべてを
// REGISTRATION_CLOSED で拒否する。

func assertRegistrationClosed(t *testing.T, code int, body map[string]any) {
	t.Helper()
	assert.Equal(t, http.StatusForbidden, code)
	errObj, ok := body["error"].(map[string]any)
	require.True(t, ok, "error object: %v", body)
	assert.Equal(t, "REGISTRATION_CLOSED", errObj["code"])
	assert.Equal(t, "2776429f-df02-4b56-a708-30d5ddda7b95", errObj["id"])
}

// **有効な招待コードがあっても拒否する。** 招待コードの誤り (INVITATION_CODE_INVALID)
// と区別するのが要点 — 同じだと利用者はコードを打ち間違えたと読む。
func TestSignup_RegistrationClosed_RejectsValidInvitation(t *testing.T) {
	h, userRepo, metaRepo := newTestHandler(t)
	metaRepo.Meta.RegistrationClosed = true
	metaRepo.Meta.DisableRegistration = true
	store := newMockTicketStore()
	store.tickets["good"] = &model.RegistrationTicket{ID: "t1", Code: "good"}
	h.SetTicketStore(store)

	rec := doPost(h.Signup, `{"username":"alice","password":"pass","invitationCode":"good"}`)
	assertRegistrationClosed(t, rec.Code, parseResp(t, rec))
	assert.Empty(t, userRepo.Users, "アカウントが作られてはいけない")
	assert.Empty(t, store.markUsed, "招待コードを消費してはいけない")
}

// 列だけが立っている (disableRegistration が立っていない) 状態でも拒否する。
// 正規化を経ずに列を書き換えられた場合の備え。
func TestSignup_RegistrationClosed_WithoutDisableRegistration(t *testing.T) {
	h, userRepo, metaRepo := newTestHandler(t)
	metaRepo.Meta.RegistrationClosed = true

	rec := doPost(h.Signup, `{"username":"alice","password":"pass"}`)
	assertRegistrationClosed(t, rec.Code, parseResp(t, rec))
	assert.Empty(t, userRepo.Users)
}

// testMode (upstream の e2e 用) でも迂回しない。
func TestSignup_RegistrationClosed_TestModeDoesNotBypass(t *testing.T) {
	h, userRepo, metaRepo := newTestHandler(t)
	h.SetTestMode(true)
	metaRepo.Meta.RegistrationClosed = true

	rec := doPost(h.Signup, `{"username":"alice","password":"pass"}`)
	assertRegistrationClosed(t, rec.Code, parseResp(t, rec))
	assert.Empty(t, userRepo.Users)
}

// 承認制の入口は、承認制でないサーバーでも「受け付けない」と答える
// (UNAVAILABLE だと「承認制ではない」と読まれる)。
func TestApplication_RegistrationClosed(t *testing.T) {
	for _, approval := range []bool{true, false} {
		env := newApprovalEnv(t, approval)
		env.meta.RegistrationClosed = true
		env.meta.DisableRegistration = true
		env.apps.app = approvedApplication()

		rec := doPost(env.handler.ApplicationApply, `{"answers":["よろしく"]}`)
		assertRegistrationClosed(t, rec.Code, parseResp(t, rec))
		assert.Nil(t, env.apps.appliedAnswers, "申請が作られてはいけない")

		// 承認済みの申請からの登録も止める (途中まで進んでいる登録)。
		rec = doPost(env.handler.ApplicationRegister, `{"claimCode":"c","username":"alice","password":"pw"}`)
		assertRegistrationClosed(t, rec.Code, parseResp(t, rec))
	}
}

// 照会は閉じない。閉じている間も、申請者が結果を見られるように。
func TestApplicationStatus_OpenWhileRegistrationClosed(t *testing.T) {
	env := newApprovalEnv(t, true)
	env.meta.RegistrationClosed = true
	env.meta.DisableRegistration = true
	env.apps.app = approvedApplication()

	rec := doPost(env.handler.ApplicationStatus, `{"claimCode":"c"}`)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// メール確認の完了も止める。**行は消さない** — 解除すれば有効期限内はそのまま完了できる。
func TestSignupPending_RegistrationClosed(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", EmailRequiredForSignup: true}
	pendingRepo := testutil.NewMockUserPendingRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coresignup.NewService(userRepo, metaRepo, idGen)
	svc.SetUserPendingRepo(pendingRepo)
	h := apisignup.NewHandler(svc, metaRepo, idGen)

	row, err := svc.CreatePending("bob", "bob@example.com", "secret", nil)
	require.NoError(t, err)

	metaRepo.Meta.RegistrationClosed = true
	rec := doPost(h.SignupPending, `{"code":"`+row.Code+`"}`)
	assertRegistrationClosed(t, rec.Code, parseResp(t, rec))
	assert.Empty(t, userRepo.Users)
	require.Len(t, pendingRepo.Rows, 1, "確認待ちの行は残す")

	metaRepo.Meta.RegistrationClosed = false
	rec = doPost(h.SignupPending, `{"code":"`+row.Code+`"}`)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, pendingRepo.Rows)
}

// 閉じている間は、コードの有無も答えない (存在しないコードでも同じ応答)。
func TestSignupPending_RegistrationClosed_UnknownCode(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", RegistrationClosed: true}
	pendingRepo := testutil.NewMockUserPendingRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coresignup.NewService(userRepo, metaRepo, idGen)
	svc.SetUserPendingRepo(pendingRepo)
	h := apisignup.NewHandler(svc, metaRepo, idGen)

	rec := doPost(h.SignupPending, `{"code":"nope"}`)
	assertRegistrationClosed(t, rec.Code, parseResp(t, rec))
}

// 承認制のまま閉じたサーバーでも、「承認制なので申請して」(APPROVAL_REQUIRED) では
// なく「受け付けていない」と答える。申請しても通らないので。
func TestSignup_RegistrationClosed_BeforeApprovalCheck(t *testing.T) {
	h, userRepo, metaRepo := newTestHandler(t)
	metaRepo.Meta.RegistrationClosed = true
	metaRepo.Meta.DisableRegistration = true
	metaRepo.Meta.ApprovalRequiredForSignup = true

	rec := doPost(h.Signup, `{"username":"alice","password":"pass"}`)
	assertRegistrationClosed(t, rec.Code, parseResp(t, rec))
	assert.Empty(t, userRepo.Users)
}
