package admin_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	apiadmin "github.com/shiroha-a/mk/internal/api/admin"
	"github.com/shiroha-a/mk/internal/entitycompat/shapetest"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupAbuseRecipientHandler returns a handler with RecipientRepo wired and
// optional seed rows. RecipientRepo を使う test の boilerplate (handler 構築
// + repo 生成 + seed + SetRecipientRepo) を 1 行に圧縮する (#761)。
// 戻り値の repo を直接 mutate して CreateErr 等の error injection も可能。
func setupAbuseRecipientHandler(t *testing.T, seed ...*model.AbuseReportNotificationRecipient) (*apiadmin.Handler, *testutil.MockAbuseReportNotificationRecipientRepository) {
	t.Helper()
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockAbuseReportNotificationRecipientRepository()
	for _, r := range seed {
		require.NoError(t, repo.Create(r))
	}
	h.SetRecipientRepo(repo)
	return h, repo
}

// --- AbuseReportNotificationRecipient ---

func TestRecipientCreate_Success(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	repo := testutil.NewMockAbuseReportNotificationRecipientRepository()
	h.SetRecipientRepo(repo)
	// method=email は対象 user に verify 済 email が必要 (upstream create.ts)。
	email := "ops@example.test"
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "ops"}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Email: &email, EmailVerified: true}

	rec := doPost(h.AbuseReportNotificationRecipientCreate,
		`{"name":"ops","method":"email","isActive":true,"userId":"u1"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)

	var got model.AbuseReportNotificationRecipient
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "ops", got.Name)
	assert.Equal(t, "email", got.Method)
	assert.True(t, got.IsActive)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	shapetest.Assert(t, "AbuseReportNotificationRecipient", raw) // L3 (#1294)
	// method=email では systemWebhookId は省略される (omitempty)。
	_, hasSysWH := raw["systemWebhookId"]
	assert.False(t, hasSysWH, "systemWebhookId must be omitted when method=email")
	// userId set 時は user (UserLite) が resolve される。
	user, ok := raw["user"].(map[string]any)
	require.True(t, ok, "user must be resolved")
	assert.Equal(t, "u1", user["id"])
}

func TestRecipientCreate_MissingRequired(t *testing.T) {
	// upstream paramDef で name + method + isActive は required (#929 C)。
	h, _ := setupAbuseRecipientHandler(t)
	assert.Equal(t, http.StatusBadRequest, doPost(h.AbuseReportNotificationRecipientCreate, `{}`, adminUser).Code)
	// method 欠落
	assert.Equal(t, http.StatusBadRequest, doPost(h.AbuseReportNotificationRecipientCreate, `{"name":"ops","isActive":true}`, adminUser).Code)
	// isActive 欠落
	assert.Equal(t, http.StatusBadRequest, doPost(h.AbuseReportNotificationRecipientCreate, `{"name":"ops","method":"email"}`, adminUser).Code)
}

func TestRecipientCreate_CorrelationCheck(t *testing.T) {
	// method=email なら userId 必須、method=webhook なら systemWebhookId 必須 (#929 C)。
	h, _ := setupAbuseRecipientHandler(t)
	rec := doPost(h.AbuseReportNotificationRecipientCreate,
		`{"name":"ops","method":"email","isActive":true}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	rec = doPost(h.AbuseReportNotificationRecipientCreate,
		`{"name":"ops","method":"webhook","isActive":true}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRecipientCreate_InvalidMethod(t *testing.T) {
	// upstream paramDef は method.enum: ['email', 'webhook'] を強制 (#929 C)。
	h, _ := setupAbuseRecipientHandler(t)
	rec := doPost(h.AbuseReportNotificationRecipientCreate,
		`{"name":"ops","method":"slack","isActive":true,"userId":"u1"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRecipientCreate_RepoError(t *testing.T) {
	h, repo := setupAbuseRecipientHandler(t)
	repo.CreateErr = assertError{}
	rec := doPost(h.AbuseReportNotificationRecipientCreate,
		`{"name":"x","method":"webhook","isActive":true,"systemWebhookId":"w1"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRecipientList_ReturnsRows(t *testing.T) {
	h, _ := setupAbuseRecipientHandler(t,
		&model.AbuseReportNotificationRecipient{ID: "r1", Name: "a", Method: "email"},
		&model.AbuseReportNotificationRecipient{ID: "r2", Name: "b", Method: "webhook"},
	)

	rec := doPost(h.AbuseReportNotificationRecipientList, `{}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []model.AbuseReportNotificationRecipient
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Len(t, rows, 2)
}

func TestRecipientShow_FoundAndNotFound(t *testing.T) {
	h, _ := setupAbuseRecipientHandler(t,
		&model.AbuseReportNotificationRecipient{ID: "r1", Name: "ops", Method: "email"},
	)

	rec := doPost(h.AbuseReportNotificationRecipientShow, `{"id":"r1"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)

	rec2 := doPost(h.AbuseReportNotificationRecipientShow, `{"id":"missing"}`, adminUser)
	assert.Equal(t, http.StatusNotFound, rec2.Code)
}

func TestRecipientDelete_Removes(t *testing.T) {
	h, repo := setupAbuseRecipientHandler(t,
		&model.AbuseReportNotificationRecipient{ID: "r1"},
	)

	rec := doPost(h.AbuseReportNotificationRecipientDelete, `{"id":"r1"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotContains(t, repo.Recipients, "r1")
}

func TestRecipientUpdate_PartialFields(t *testing.T) {
	h, repo := setupAbuseRecipientHandler(t,
		&model.AbuseReportNotificationRecipient{
			ID: "r1", Name: "old", Method: "email", IsActive: true,
		},
	)

	// method='webhook' に切り替えるなら systemWebhookId も同 payload に含める
	// 必要がある (upstream correlation check と同 semantics、PR #1108 で追加)。
	rec := doPost(h.AbuseReportNotificationRecipientUpdate,
		`{"id":"r1","name":"new","method":"webhook","systemWebhookId":"w1","isActive":false}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "new", repo.Recipients["r1"].Name)
	assert.Equal(t, "webhook", repo.Recipients["r1"].Method)
	assert.False(t, repo.Recipients["r1"].IsActive)
}

// PR #1108: method 変更時の correlation check が動作することの regression guard。
// method='webhook' なのに systemWebhookId 未指定 → 400 (silent drift 防止)。
func TestRecipientUpdate_MethodWithoutCounterpartReturns400(t *testing.T) {
	h, _ := setupAbuseRecipientHandler(t,
		&model.AbuseReportNotificationRecipient{
			ID: "r1", Name: "old", Method: "email", IsActive: true,
			// 既存 row に SystemWebhookID は無い
		},
	)
	rec := doPost(h.AbuseReportNotificationRecipientUpdate,
		`{"id":"r1","method":"webhook"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "CORRELATION_CHECK_WEBHOOK")
}

// upstream enum: ['email', 'webhook'] 以外の値は 400 reject。
func TestRecipientUpdate_InvalidMethodReturns400(t *testing.T) {
	h, _ := setupAbuseRecipientHandler(t,
		&model.AbuseReportNotificationRecipient{
			ID: "r1", Name: "old", Method: "email", IsActive: true,
		},
	)
	rec := doPost(h.AbuseReportNotificationRecipientUpdate,
		`{"id":"r1","method":"sms"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// PR #1108 sweep follow-up: 不存在 ID + 不完全 method payload は
// **404 NotFound** が返る (correlation_check の 400 が先に発火しない)。
// エラー優先順位を「不存在 → 入力不正」の順に統一する regression guard。
// 旧版は before 取得失敗を捨てて correlation_check に進んでいたため、
// admin UI / CLI が ID typo を 400 として誤って受け取っていた edge case。
func TestRecipientUpdate_NotFoundTakesPrecedenceOverCorrelationCheck(t *testing.T) {
	h, _ := setupAbuseRecipientHandler(t) // recipient 0 件で開始
	// method=webhook + systemWebhookId 未指定 = 普段なら correlation_check で
	// 400 になるが、ID が存在しないので 404 が先に返る。
	rec := doPost(h.AbuseReportNotificationRecipientUpdate,
		`{"id":"ghost","method":"webhook"}`, adminUser)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	// correlation_check のメッセージは含まれていないこと
	assert.NotContains(t, rec.Body.String(), "CORRELATION_CHECK_WEBHOOK")
}

func TestRecipientUpdate_NotFound(t *testing.T) {
	h, _ := setupAbuseRecipientHandler(t)
	rec := doPost(h.AbuseReportNotificationRecipientUpdate, `{"id":"missing","name":"x"}`, adminUser)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestRecipientUpdate_MissingID(t *testing.T) {
	h, _ := setupAbuseRecipientHandler(t)
	rec := doPost(h.AbuseReportNotificationRecipientUpdate, `{}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- thin nil-repo smoke tests ---
//
// nil recipientRepo 経路 (newTestHandler は wire しない) で expected status を
// 返すことを担保。詳細テストは repo を wire して実挙動を検証するので、本群は
// nil 分岐の coverage 補完。

func TestAbuseReportNotificationRecipientCreate(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// required 欠落は 400 (validate-first ordering、#929 C)。
	assert.Equal(t, http.StatusBadRequest, doPost(h.AbuseReportNotificationRecipientCreate, `{}`, adminUser).Code)
	// required 揃いの正規 bind は nil recipientRepo で 204 (webhook は profile 不要)
	assert.Equal(t, http.StatusNoContent, doPost(h.AbuseReportNotificationRecipientCreate,
		`{"name":"ops","method":"webhook","isActive":true,"systemWebhookId":"w1"}`, adminUser).Code)
}

func TestAbuseReportNotificationRecipientDelete(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.AbuseReportNotificationRecipientDelete, `{}`, adminUser).Code)
}

func TestAbuseReportNotificationRecipientList(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusOK, doPost(h.AbuseReportNotificationRecipientList, `{}`, adminUser).Code)
}

func TestAbuseReportNotificationRecipientShow(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNotFound, doPost(h.AbuseReportNotificationRecipientShow, `{}`, adminUser).Code)
}

func TestAbuseReportNotificationRecipientUpdate(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.AbuseReportNotificationRecipientUpdate, `{}`, adminUser).Code)
}

// --- moderation log assertions (#665) ---

func TestRecipientCreate_WritesModerationLog(t *testing.T) {
	h, _ := setupAbuseRecipientHandler(t)
	repo := attachModLog(t, h)

	rec := doPost(h.AbuseReportNotificationRecipientCreate,
		`{"name":"r","method":"webhook","isActive":true,"systemWebhookId":"w1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "createAbuseReportNotificationRecipient", repo.Snapshot()[0].Type)
}

func TestRecipientUpdate_WritesModerationLog(t *testing.T) {
	h, _ := setupAbuseRecipientHandler(t,
		&model.AbuseReportNotificationRecipient{ID: "r1", Name: "old", Method: "email"},
	)
	repo := attachModLog(t, h)

	rec := doPost(h.AbuseReportNotificationRecipientUpdate, `{"id":"r1","name":"new"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	logs := repo.Snapshot()
	assert.Equal(t, "updateAbuseReportNotificationRecipient", logs[0].Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	require.NotNil(t, info["before"])
	require.NotNil(t, info["after"])
}

func TestRecipientDelete_WritesModerationLog(t *testing.T) {
	h, _ := setupAbuseRecipientHandler(t,
		&model.AbuseReportNotificationRecipient{ID: "r1", Name: "doomed", Method: "email"},
	)
	repo := attachModLog(t, h)

	rec := doPost(h.AbuseReportNotificationRecipientDelete, `{"id":"r1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "deleteAbuseReportNotificationRecipient", repo.Snapshot()[0].Type)
}

// --- #H-PR8g: notification-recipient packer + EMAIL_ADDRESS_NOT_SET + method filter ---

// method=email で対象 user の email が未 verify なら EMAIL_ADDRESS_NOT_SET。
func TestRecipientCreate_EmailAddressNotSet(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	h.SetRecipientRepo(testutil.NewMockAbuseReportNotificationRecipientRepository())
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "x"}
	// profile はあるが email 未設定 (未 verify)。
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}
	rec := doPost(h.AbuseReportNotificationRecipientCreate,
		`{"name":"ops","method":"email","isActive":true,"userId":"u1"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "EMAIL_ADDRESS_NOT_SET")
	assert.Contains(t, rec.Body.String(), "7cc1d85e-2f58-fc31-b644-3de8d0d3421f")
}

// profile が存在しない user は CORRELATION_CHECK_EMAIL (upstream と同じ)。
func TestRecipientCreate_EmailNoProfile(t *testing.T) {
	h, _ := setupAbuseRecipientHandler(t) // userRepo wired (newTestHandler), profile 無し
	rec := doPost(h.AbuseReportNotificationRecipientCreate,
		`{"name":"ops","method":"email","isActive":true,"userId":"ghost"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "CORRELATION_CHECK_EMAIL")
}

// webhook recipient のレスポンスに systemWebhook (SystemWebhook) が resolve される。
func TestRecipientCreate_ResolvesSystemWebhook(t *testing.T) {
	h, _ := setupAbuseRecipientHandler(t)
	whRepo := testutil.NewMockSystemWebhookRepository()
	whRepo.Webhooks["w1"] = &model.SystemWebhook{ID: "w1", Name: "ops-hook", URL: "https://hook.example", IsActive: true}
	h.SetSystemWebhookRepo(whRepo)

	rec := doPost(h.AbuseReportNotificationRecipientCreate,
		`{"name":"ops","method":"webhook","isActive":true,"systemWebhookId":"w1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	wh, ok := raw["systemWebhook"].(map[string]any)
	require.True(t, ok, "systemWebhook must be resolved")
	assert.Equal(t, "w1", wh["id"])
	assert.Equal(t, "ops-hook", wh["name"])
	// user は未設定なので省略。
	_, hasUser := raw["user"]
	assert.False(t, hasUser)
}

// list は method[] フィルタを尊重する。
func TestRecipientList_MethodFilter(t *testing.T) {
	wh := "w1"
	h, _ := setupAbuseRecipientHandler(t,
		&model.AbuseReportNotificationRecipient{ID: "r1", Name: "a", Method: "email"},
		&model.AbuseReportNotificationRecipient{ID: "r2", Name: "b", Method: "webhook", SystemWebhookID: &wh},
	)
	rec := doPost(h.AbuseReportNotificationRecipientList, `{"method":["webhook"]}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "r2", rows[0]["id"])
}

// update で method=email に切替時、対象 user の email が未 verify なら
// EMAIL_ADDRESS_NOT_SET (T1: update の email validation 回帰 guard)。
func TestRecipientUpdate_EmailAddressNotSet(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	wh := "w1"
	repo := testutil.NewMockAbuseReportNotificationRecipientRepository()
	require.NoError(t, repo.Create(&model.AbuseReportNotificationRecipient{
		ID: "r1", Name: "x", Method: "webhook", SystemWebhookID: &wh, IsActive: true,
	}))
	h.SetRecipientRepo(repo)
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1"} // email 未設定

	rec := doPost(h.AbuseReportNotificationRecipientUpdate,
		`{"id":"r1","method":"email","userId":"u1"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "EMAIL_ADDRESS_NOT_SET")
}

// update で webhook→email に切替えるが userId 未指定 (既存 row にも無し) なら
// CORRELATION_CHECK_EMAIL。
func TestRecipientUpdate_MethodBecomesEmailWithoutUserId(t *testing.T) {
	wh := "w1"
	h, _ := setupAbuseRecipientHandler(t,
		&model.AbuseReportNotificationRecipient{ID: "r1", Name: "x", Method: "webhook", SystemWebhookID: &wh},
	)
	rec := doPost(h.AbuseReportNotificationRecipientUpdate, `{"id":"r1","method":"email"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "CORRELATION_CHECK_EMAIL")
}

// method 切替で対応しない側の counterpart が null 化される (F2)。
func TestRecipientUpdate_MethodSwitchClearsCounterpart(t *testing.T) {
	u := "u1"
	h, repo := setupAbuseRecipientHandler(t,
		&model.AbuseReportNotificationRecipient{ID: "r1", Name: "x", Method: "email", UserID: &u, IsActive: true},
	)
	rec := doPost(h.AbuseReportNotificationRecipientUpdate,
		`{"id":"r1","method":"webhook","systemWebhookId":"w1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "webhook", repo.Recipients["r1"].Method)
	require.NotNil(t, repo.Recipients["r1"].SystemWebhookID)
	assert.Equal(t, "w1", *repo.Recipients["r1"].SystemWebhookID)
	assert.Nil(t, repo.Recipients["r1"].UserID, "email→webhook で userId は null 化される")
}

// create で method=webhook 時に userId は保存されない (F2)。
func TestRecipientCreate_WebhookNullsUserId(t *testing.T) {
	h, repo := setupAbuseRecipientHandler(t)
	rec := doPost(h.AbuseReportNotificationRecipientCreate,
		`{"name":"x","method":"webhook","isActive":true,"systemWebhookId":"w1","userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var r *model.AbuseReportNotificationRecipient
	for _, v := range repo.Recipients {
		r = v
	}
	require.NotNil(t, r)
	assert.Nil(t, r.UserID, "webhook では userId を保存しない")
	require.NotNil(t, r.SystemWebhookID)
}

// list の method[] filter の境界 (email/webhook/multi/empty) + 昇順 (F3/F4)。
func TestRecipientList_MethodFilterVariants(t *testing.T) {
	u := "u1"
	wh := "w1"
	h, _ := setupAbuseRecipientHandler(t,
		&model.AbuseReportNotificationRecipient{ID: "r2", Name: "b", Method: "webhook", SystemWebhookID: &wh},
		&model.AbuseReportNotificationRecipient{ID: "r1", Name: "a", Method: "email", UserID: &u},
	)
	count := func(body string) []map[string]any {
		rec := doPost(h.AbuseReportNotificationRecipientList, body, adminUser)
		require.Equal(t, http.StatusOK, rec.Code)
		var rows []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
		return rows
	}
	assert.Len(t, count(`{"method":["email"]}`), 1)
	assert.Len(t, count(`{"method":["webhook"]}`), 1)
	assert.Len(t, count(`{"method":["email","webhook"]}`), 2)
	assert.Len(t, count(`{"method":[]}`), 2, "空配列は全件")
	// 昇順 (id ASC): r1 が先。
	all := count(`{}`)
	require.Len(t, all, 2)
	assert.Equal(t, "r1", all[0]["id"])
	assert.Equal(t, "r2", all[1]["id"])
}

// setupRecipientWithUserAndWebhook wires recipient + user (verified email) +
// systemWebhook repos for update/resolve coverage (T1/T3).
func setupRecipientWithUserAndWebhook(t *testing.T, seed ...*model.AbuseReportNotificationRecipient) (
	*apiadmin.Handler, *testutil.MockUserRepository,
	*testutil.MockAbuseReportNotificationRecipientRepository, *testutil.MockSystemWebhookRepository) {
	t.Helper()
	h, userRepo, _, _ := newTestHandler(t)
	repo := testutil.NewMockAbuseReportNotificationRecipientRepository()
	for _, r := range seed {
		require.NoError(t, repo.Create(r))
	}
	h.SetRecipientRepo(repo)
	whRepo := testutil.NewMockSystemWebhookRepository()
	h.SetSystemWebhookRepo(whRepo)
	email := "ops@example.test"
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "ops"}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Email: &email, EmailVerified: true}
	return h, userRepo, repo, whRepo
}

// update で webhook→email に正常切替できる: systemWebhookId が null 化され
// userId がセットされ、レスポンスは user(UserLite) を resolve し systemWebhookId
// を omit する (T1)。
func TestRecipientUpdate_MethodSwitchToEmail(t *testing.T) {
	wh := "w1"
	h, _, repo, _ := setupRecipientWithUserAndWebhook(t,
		&model.AbuseReportNotificationRecipient{ID: "r1", Name: "x", Method: "webhook", SystemWebhookID: &wh, IsActive: true},
	)
	rec := doPost(h.AbuseReportNotificationRecipientUpdate,
		`{"id":"r1","method":"email","userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "email", repo.Recipients["r1"].Method)
	require.NotNil(t, repo.Recipients["r1"].UserID)
	assert.Equal(t, "u1", *repo.Recipients["r1"].UserID)
	assert.Nil(t, repo.Recipients["r1"].SystemWebhookID, "webhook→email で systemWebhookId は null 化される")

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	user, ok := raw["user"].(map[string]any)
	require.True(t, ok, "user must be resolved on update response")
	assert.Equal(t, "u1", user["id"])
	_, hasSysWH := raw["systemWebhookId"]
	assert.False(t, hasSysWH, "systemWebhookId must be omitted when method=email")
}

// method 未送出の partial update (else 分岐): counterpart id 単独更新で
// method は不変、指定 field のみ反映される (T2)。
func TestRecipientUpdate_PartialCounterpartOnly(t *testing.T) {
	// webhook row の systemWebhookId 単独差替 (email 検証は走らない)。
	wh := "w1"
	h, repo := setupAbuseRecipientHandler(t,
		&model.AbuseReportNotificationRecipient{ID: "r1", Name: "x", Method: "webhook", SystemWebhookID: &wh, IsActive: true},
	)
	rec := doPost(h.AbuseReportNotificationRecipientUpdate, `{"id":"r1","systemWebhookId":"w2"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "webhook", repo.Recipients["r1"].Method, "method は不変")
	require.NotNil(t, repo.Recipients["r1"].SystemWebhookID)
	assert.Equal(t, "w2", *repo.Recipients["r1"].SystemWebhookID)

	// email row の userId 単独差替 (新 user は verify 済 email を持つ)。
	h2, _, repo2, _ := setupRecipientWithUserAndWebhook(t,
		&model.AbuseReportNotificationRecipient{ID: "r2", Name: "y", Method: "email", UserID: ptr("u0"), IsActive: true},
	)
	rec = doPost(h2.AbuseReportNotificationRecipientUpdate, `{"id":"r2","userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "email", repo2.Recipients["r2"].Method)
	require.NotNil(t, repo2.Recipients["r2"].UserID)
	assert.Equal(t, "u1", *repo2.Recipients["r2"].UserID)
}

// EV-1: method を省いた partial update でも、既存 email row の userId 差替なら
// 対象 user の email verify を検証する (未 verify は EMAIL_ADDRESS_NOT_SET)。
func TestRecipientUpdate_PartialEmailValidation(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	repo := testutil.NewMockAbuseReportNotificationRecipientRepository()
	require.NoError(t, repo.Create(&model.AbuseReportNotificationRecipient{
		ID: "r1", Name: "x", Method: "email", UserID: ptr("u0"), IsActive: true,
	}))
	h.SetRecipientRepo(repo)
	userRepo.Users["u2"] = &model.User{ID: "u2"}
	userRepo.Profiles["u2"] = &model.UserProfile{UserID: "u2"} // email 未設定

	// method 省略 + userId を未 verify user に差替 → bypass せず 400。
	rec := doPost(h.AbuseReportNotificationRecipientUpdate, `{"id":"r1","userId":"u2"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "EMAIL_ADDRESS_NOT_SET")
}

// 既存 webhook row の name 単独更新 (method 省略) では email validation が走らず 200 (EV-3 境界)。
func TestRecipientUpdate_PartialNoEmailValidationForWebhook(t *testing.T) {
	wh := "w1"
	h, repo := setupAbuseRecipientHandler(t,
		&model.AbuseReportNotificationRecipient{ID: "r1", Name: "x", Method: "webhook", SystemWebhookID: &wh, IsActive: true},
	)
	rec := doPost(h.AbuseReportNotificationRecipientUpdate, `{"id":"r1","name":"renamed"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "renamed", repo.Recipients["r1"].Name)
}

// resolve した systemWebhook が全 field (secret 含む) を持つことを検証する (T3)。
func TestRecipientShow_ResolvesFullSystemWebhook(t *testing.T) {
	wh := "w1"
	sentAt := time.Now()
	status := 200
	h, _, _, whRepo := setupRecipientWithUserAndWebhook(t,
		&model.AbuseReportNotificationRecipient{ID: "r1", Name: "x", Method: "webhook", SystemWebhookID: &wh},
	)
	whRepo.Webhooks["w1"] = &model.SystemWebhook{
		ID: "w1", IsActive: true, UpdatedAt: time.Now(), LatestSentAt: &sentAt,
		LatestStatus: &status, Name: "ops-hook", On: []string{"abuseReport"},
		URL: "https://hook.example", Secret: "s3cr3t",
	}
	rec := doPost(h.AbuseReportNotificationRecipientShow, `{"id":"r1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	w, ok := raw["systemWebhook"].(map[string]any)
	require.True(t, ok)
	// upstream system-webhook schema は secret を含む全 9 field を露出する。
	for _, k := range []string{"id", "isActive", "updatedAt", "latestSentAt", "latestStatus", "name", "on", "url", "secret"} {
		_, has := w[k]
		assert.Truef(t, has, "systemWebhook must expose %q", k)
	}
	assert.Equal(t, "s3cr3t", w["secret"])
	assert.Equal(t, "https://hook.example", w["url"])
}

// update の DB 障害は 500 (T5: create の RepoError と対称)。
func TestRecipientUpdate_RepoError(t *testing.T) {
	h, repo := setupAbuseRecipientHandler(t,
		&model.AbuseReportNotificationRecipient{ID: "r1", Name: "x", Method: "webhook", SystemWebhookID: ptr("w1")},
	)
	repo.UpdateErr = assert.AnError
	rec := doPost(h.AbuseReportNotificationRecipientUpdate, `{"id":"r1","name":"y"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// list の method[] に enum 外の値を渡すと 400 (F1/F3、create/update と対称)。
func TestRecipientList_InvalidMethod(t *testing.T) {
	h, _ := setupAbuseRecipientHandler(t)
	rec := doPost(h.AbuseReportNotificationRecipientList, `{"method":["slack"]}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// recipientMatchesMethod の counterpart 不整合除外 (legacy 行) を直接検証する (F3)。
func TestRecipientList_CounterpartMismatchExcluded(t *testing.T) {
	// method=email だが userId=nil の legacy 行 / method=webhook だが userId!=nil の legacy 行。
	h, _ := setupAbuseRecipientHandler(t,
		&model.AbuseReportNotificationRecipient{ID: "r1", Name: "a", Method: "email"},                      // userId nil
		&model.AbuseReportNotificationRecipient{ID: "r2", Name: "b", Method: "webhook", UserID: ptr("u9")}, // userId 付き
	)
	count := func(body string) int {
		rec := doPost(h.AbuseReportNotificationRecipientList, body, adminUser)
		require.Equal(t, http.StatusOK, rec.Code)
		var rows []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
		return len(rows)
	}
	assert.Equal(t, 0, count(`{"method":["email"]}`), "method=email かつ userId=nil の行は除外")
	assert.Equal(t, 0, count(`{"method":["webhook"]}`), "method=webhook かつ userId!=nil の行は除外")
}

// PK-1: updatedAt は JS toISOString() 互換の .000Z 形式で返る。
func TestRecipientCreate_UpdatedAtFormat(t *testing.T) {
	wh := "w1"
	h, _ := setupAbuseRecipientHandler(t,
		&model.AbuseReportNotificationRecipient{ID: "r1", Name: "x", Method: "webhook", SystemWebhookID: &wh},
	)
	rec := doPost(h.AbuseReportNotificationRecipientShow, `{"id":"r1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`, raw["updatedAt"])
}

func ptr(s string) *string { return &s }

// method を省いた部分更新でも、method に合わない側は書かずに NULL にする (#3264)。
// 書けると、外部キーが CASCADE なので、無関係な利用者や System Webhook を
// 消しただけで通知先ごと消える。
func TestRecipientUpdate_PartialUpdateNullsMismatchedSide(t *testing.T) {
	uid, whID := "u1", "w1"
	t.Run("webhook row ignores userId", func(t *testing.T) {
		h, repo := setupAbuseRecipientHandler(t,
			// 以前の部分更新で userId が入ってしまった行も、更新で正す。
			&model.AbuseReportNotificationRecipient{ID: "r1", Name: "hook", Method: "webhook", SystemWebhookID: &whID, UserID: &uid},
		)
		rec := doPost(h.AbuseReportNotificationRecipientUpdate, `{"id":"r1","userId":"u1","systemWebhookId":"w2"}`, adminUser)
		require.Equal(t, http.StatusOK, rec.Code)
		r := repo.Recipients["r1"]
		assert.Nil(t, r.UserID, "webhook 方式の行に userId を書かない")
		require.NotNil(t, r.SystemWebhookID)
		assert.Equal(t, "w2", *r.SystemWebhookID)
	})
	t.Run("email row ignores systemWebhookId", func(t *testing.T) {
		h, repo := setupAbuseRecipientHandler(t,
			&model.AbuseReportNotificationRecipient{ID: "r1", Name: "mail", Method: "email", UserID: &uid, SystemWebhookID: &whID},
		)
		rec := doPost(h.AbuseReportNotificationRecipientUpdate, `{"id":"r1","systemWebhookId":"w1"}`, adminUser)
		require.Equal(t, http.StatusOK, rec.Code)
		r := repo.Recipients["r1"]
		assert.Nil(t, r.SystemWebhookID, "email 方式の行に systemWebhookId を書かない")
		require.NotNil(t, r.UserID, "email 方式の行の userId は残す")
		assert.Equal(t, "u1", *r.UserID)
	})
}
