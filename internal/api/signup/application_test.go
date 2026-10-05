package signup_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisignup "github.com/elythia-network/elythia/internal/api/signup"
	"github.com/elythia-network/elythia/internal/core/captcha"
	coresignup "github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/core/signupapplication"
	"github.com/elythia-network/elythia/internal/core/signupform"
	"github.com/elythia-network/elythia/internal/misc/id"
	miscsmtp "github.com/elythia-network/elythia/internal/misc/smtp"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// stubApplications is an in-memory stand-in for the state machine.
type stubApplications struct {
	app       *model.SignupApplication
	code      string
	applyErr  error
	lookupErr error
	markErr   error

	appliedAnswers []signupapplication.Answer
	completedApp   string
	completedUsr   string
	completedTkt   string
	lastCode       string
	markedTktApp   string
	markedTkt      string
	previousTkt    string
	markTicketErr  error
}

func (s *stubApplications) Apply(answers []signupapplication.Answer) (*model.SignupApplication, string, error) {
	s.appliedAnswers = answers
	if s.applyErr != nil {
		return nil, "", s.applyErr
	}
	return &model.SignupApplication{ID: "app-1", Status: model.SignupApplicationPending}, "claim-code-1", nil
}

func (s *stubApplications) ByClaimCode(code string) (*model.SignupApplication, error) {
	s.lastCode = code
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	if s.code != "" && code != s.code {
		return nil, nil
	}
	return s.app, nil
}

func (s *stubApplications) MarkCompleted(applicationID, userID, ticketID string) error {
	s.completedApp, s.completedUsr, s.completedTkt = applicationID, userID, ticketID
	return s.markErr
}

func (s *stubApplications) MarkTicket(applicationID, ticketID string) (string, error) {
	s.markedTktApp, s.markedTkt = applicationID, ticketID
	if s.markTicketErr != nil {
		return "", s.markTicketErr
	}
	previous := s.previousTkt
	s.previousTkt = ticketID
	return previous, nil
}

// stubTicketStore satisfies TicketStore plus the optional issue / discard halves.
type stubTicketStore struct {
	created   []*model.RegistrationTicket
	deleted   []string
	usedTkt   string
	createErr error

	pendingTkt     string
	pendingRow     string
	markPendingErr error
}

func (s *stubTicketStore) FindByCode(string) (*model.RegistrationTicket, error) {
	return nil, errors.New("not used")
}

func (s *stubTicketStore) MarkUsed(ticketID, userID string) error {
	s.usedTkt = ticketID
	return nil
}

func (s *stubTicketStore) ClaimForSignup(string, bool) (bool, error) { return true, nil }

func (s *stubTicketStore) ReleaseClaim(string) error { return nil }

func (s *stubTicketStore) MarkPending(ticketID, pendingID string) error {
	s.pendingTkt, s.pendingRow = ticketID, pendingID
	return s.markPendingErr
}

func (s *stubTicketStore) Create(t *model.RegistrationTicket) error {
	if s.createErr != nil {
		return s.createErr
	}
	s.created = append(s.created, t)
	return nil
}

func (s *stubTicketStore) Delete(id string) error {
	s.deleted = append(s.deleted, id)
	return nil
}

type approvalEnv struct {
	handler *apisignup.Handler
	apps    *stubApplications
	meta    *model.Meta
}

func newApprovalEnv(t *testing.T, enabled bool) *approvalEnv {
	t.Helper()
	h, _, metaRepo := newTestHandler(t)
	metaRepo.Meta = &model.Meta{
		ID: "x", ApprovalRequiredForSignup: enabled,
		SignupApplicationForm: datatypes.JSON(`[{"label":"参加の動機","type":"textarea","required":true}]`),
	}
	apps := &stubApplications{}
	h.SetSignupApplications(apps)
	return &approvalEnv{handler: h, apps: apps, meta: metaRepo.Meta}
}

func approvedApplication() *model.SignupApplication {
	return &model.SignupApplication{ID: "app-1", Status: model.SignupApplicationApproved}
}

// 承認制が無効なら、どの endpoint も 503。**500 にすると、機能を使っていない
// インスタンスで壊れているように見える。**
func TestApplication_DisabledReturns503(t *testing.T) {
	env := newApprovalEnv(t, false)

	assert.Equal(t, http.StatusServiceUnavailable,
		doPost(env.handler.ApplicationApply, `{}`).Code)
	assert.Equal(t, http.StatusServiceUnavailable,
		doPost(env.handler.ApplicationStatus, `{"claimCode":"x"}`).Code)
	assert.Equal(t, http.StatusServiceUnavailable,
		doPost(env.handler.ApplicationRegister, `{"claimCode":"x","username":"a","password":"b"}`).Code)
}

// 登録を停止したら、承認制の入口も閉じること。
//
// **承認制それ自体がゲートでも、停止の操作は別に効かせる。** 承認制を入れる更新は
// disableRegistration を false に正規化するので同時には立たないが、後から閉じた
// ときに承認経路だけ開いたままだと「止めたのに止まっていない」ことになる。
func TestApplication_DisableRegistrationClosesEntryPoints(t *testing.T) {
	env := newApprovalEnv(t, true)
	env.meta.DisableRegistration = true
	env.apps.app = approvedApplication()

	assert.Equal(t, http.StatusServiceUnavailable,
		doPost(env.handler.ApplicationApply, `{"answers":["よろしく"]}`).Code)
	assert.Equal(t, http.StatusServiceUnavailable,
		doPost(env.handler.ApplicationRegister, `{"claimCode":"c","username":"alice","password":"pw"}`).Code)
	assert.Nil(t, env.apps.appliedAnswers, "申請が作られてはいけない")

	// 状態の照会は読むだけなので閉じない。**閉じると、既に申請した人が結果を
	// 見られなくなる。**
	assert.Equal(t, http.StatusOK,
		doPost(env.handler.ApplicationStatus, `{"claimCode":"c"}`).Code)
}

func TestApplication_NotWiredReturns503(t *testing.T) {
	h, _, metaRepo := newTestHandler(t)
	metaRepo.Meta = &model.Meta{ID: "x", ApprovalRequiredForSignup: true}

	assert.Equal(t, http.StatusServiceUnavailable, doPost(h.ApplicationApply, `{}`).Code)
}

// **クレームコードを返すのはここだけ。** 保存しているのは hash なので、以後
// サーバー側から平文を出す手段は無い。
func TestApplicationApply(t *testing.T) {
	env := newApprovalEnv(t, true)

	rec := doPost(env.handler.ApplicationApply, `{"answers":["よろしく"]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := parseResp(t, rec)

	assert.Equal(t, "claim-code-1", resp["claimCode"])
	// **ラベルはサーバーが定義から埋める。** クライアントに送らせると、申請者が
	// 審査画面に偽のラベルを流し込める (#2570)。
	require.Len(t, env.apps.appliedAnswers, 1)
	assert.Equal(t, "参加の動機", env.apps.appliedAnswers[0].Label)
	assert.Equal(t, "よろしく", env.apps.appliedAnswers[0].Value)

	app := resp["application"].(map[string]any)
	assert.Equal(t, "pending", app["status"])
	// **管理者向けの列は出さない。**
	assert.NotContains(t, app, "processedById")
	assert.NotContains(t, app, "id")
}

func TestApplicationApply_Errors(t *testing.T) {
	t.Run("required answer missing", func(t *testing.T) {
		env := newApprovalEnv(t, true)
		rec := doPost(env.handler.ApplicationApply, `{"answers":["  "]}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "ANSWER_REQUIRED")
	})

	t.Run("answer too long", func(t *testing.T) {
		env := newApprovalEnv(t, true)
		env.meta.SignupApplicationForm = datatypes.JSON(`[{"label":"x","type":"text","maxLength":3}]`)
		rec := doPost(env.handler.ApplicationApply, `{"answers":["toolong"]}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "ANSWER_TOO_LONG")
	})

	// フォーム定義が申請の途中で変わった。**黙って詰め合わせると答えと設問が
	// ずれる**ので、やり直してもらう。
	t.Run("form changed under the applicant", func(t *testing.T) {
		env := newApprovalEnv(t, true)
		rec := doPost(env.handler.ApplicationApply, `{"answers":["a","b"]}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "FORM_CHANGED")
	})

	t.Run("unexpected", func(t *testing.T) {
		env := newApprovalEnv(t, true)
		env.apps.applyErr = errors.New("boom")
		rec := doPost(env.handler.ApplicationApply, `{"answers":["x"]}`)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})
}

// 申請者がコードを持って戻ってくる入口。**これが唯一の導線。**
func TestApplicationStatus(t *testing.T) {
	env := newApprovalEnv(t, true)
	env.apps.app = &model.SignupApplication{
		ID: "app-1", Status: model.SignupApplicationApproved,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}

	rec := doPost(env.handler.ApplicationStatus, `{"claimCode":"code-1"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "code-1", env.apps.lastCode)

	app := parseResp(t, rec)["application"].(map[string]any)
	assert.Equal(t, "approved", app["status"])
}

func TestApplicationStatus_Errors(t *testing.T) {
	t.Run("missing code", func(t *testing.T) {
		env := newApprovalEnv(t, true)
		rec := doPost(env.handler.ApplicationStatus, `{}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
	})

	// **存在しないコードと期限切れのコードを区別しない。** 区別できると、
	// 総当たりで「そのコードは実在する」ことだけ漏れる。
	t.Run("unknown code", func(t *testing.T) {
		env := newApprovalEnv(t, true)
		rec := doPost(env.handler.ApplicationStatus, `{"claimCode":"nope"}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "NO_SUCH_APPLICATION")
	})

	t.Run("lookup failure", func(t *testing.T) {
		env := newApprovalEnv(t, true)
		env.apps.lookupErr = errors.New("boom")
		rec := doPost(env.handler.ApplicationStatus, `{"claimCode":"x"}`)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})
}

// **申請の状態はサーバー側で引き直す。** 画面が古い状態を握っていても、承認
// されていないものが通ることは無い。
func TestApplicationRegister_RequiresApproval(t *testing.T) {
	for _, status := range []string{
		model.SignupApplicationPending,
		model.SignupApplicationRejected,
		model.SignupApplicationExpired,
		model.SignupApplicationCompleted,
	} {
		t.Run(status, func(t *testing.T) {
			env := newApprovalEnv(t, true)
			env.apps.app = &model.SignupApplication{ID: "app-1", Status: status}

			rec := doPost(env.handler.ApplicationRegister,
				`{"claimCode":"c","username":"newbie","password":"hunter22"}`)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "NOT_APPROVED")
		})
	}
}

func TestApplicationRegister_Success(t *testing.T) {
	env := newApprovalEnv(t, true)
	env.apps.app = approvedApplication()

	rec := doPost(env.handler.ApplicationRegister,
		`{"claimCode":"c","username":"newbie","password":"hunter22"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := parseResp(t, rec)
	assert.Equal(t, "newbie", resp["username"])
	assert.NotEmpty(t, resp["token"])

	assert.Equal(t, "app-1", env.apps.completedApp)
	assert.NotEmpty(t, env.apps.completedUsr)
}

func TestApplicationRegister_InvalidParams(t *testing.T) {
	env := newApprovalEnv(t, true)
	rec := doPost(env.handler.ApplicationRegister, `{"claimCode":"c"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
}

func TestApplicationRegister_UnknownCode(t *testing.T) {
	env := newApprovalEnv(t, true)
	env.apps.code = "right"

	rec := doPost(env.handler.ApplicationRegister,
		`{"claimCode":"wrong","username":"newbie","password":"hunter22"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_APPLICATION")
}

func TestApplicationRegister_LookupFailure(t *testing.T) {
	env := newApprovalEnv(t, true)
	env.apps.lookupErr = errors.New("boom")

	rec := doPost(env.handler.ApplicationRegister,
		`{"claimCode":"c","username":"newbie","password":"hunter22"}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// **即時作成では ticket を発行しない (#2813)。** 発行しても誰も参照しない —
// 一回性は settleApplicationTx の行ロックが担保しており (#2580)、ticket は
// `signup_application.ticketId` に記録されるだけだった。
func TestApplicationRegister_ImmediateDoesNotMintTicket(t *testing.T) {
	env := newApprovalEnv(t, true)
	tickets := &stubTicketStore{}
	env.handler.SetTicketStore(tickets)
	moderator := "mod-1"
	app := approvedApplication()
	app.ProcessedByID = &moderator
	env.apps.app = app

	rec := doPost(env.handler.ApplicationRegister,
		`{"claimCode":"c","username":"newbie","password":"hunter22"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Empty(t, tickets.created, "即時作成では発行しない")
	assert.Empty(t, tickets.usedTkt)
	assert.Empty(t, tickets.deleted)
	assert.Empty(t, env.apps.completedTkt, "ticketId は記録しない")

	// **監査は申請だけで辿れる。** 登録者は申請に残る。審査した管理者
	// (processedById) を DB から読み直して確かめるのは統合テスト側
	// (TestApplicationRegister_ImmediateCreatesNoTicket) — ここで
	// `env.apps.app` を読み直しても、テスト自身が入れた値を見るだけになる。
	assert.NotEmpty(t, env.apps.completedUsr, "登録者は申請に残る")
}

// **メール経路では発行の失敗を 500 にする。** ここを素通しにすると ticket が nil に
// なり、registerViaEmailConfirmation は MarkTicket を丸ごと飛ばす — 行ロック付きの
// 申請状態の再確認 (#2571 / #2576) を通らないまま確認メールが飛ぶ。
func TestApplicationRegister_EmailRequired_TicketIssueFailure(t *testing.T) {
	env, pendingRepo, _, sent := newApprovalEnvWithEmail(t)
	env.handler.SetTicketStore(&stubTicketStore{createErr: errors.New("boom")})

	rec := doPost(env.handler.ApplicationRegister,
		`{"claimCode":"c","username":"newbie","password":"hunter22","emailAddress":"newbie@example.com"}`)
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	// **ここまで来ていないこと**を見る。ステータスだけだと、pending を作って
	// メールを飛ばしてから 500 を返す形でも緑になる。
	assert.Empty(t, pendingRepo.Rows, "pending を作らない")
	// **待たずに select すると空振りする。** 送信は goroutine なので、
	// `default:` だとスケジュールされる前に通り抜ける (実測で 30 回中 0 回検出)。
	select {
	case to := <-sent:
		t.Fatalf("確認メールを送ってはいけない (to=%s)", to)
	case <-time.After(200 * time.Millisecond):
	}
}

// ticketStore が発行に失敗しても、即時作成は ticket を使わないので通る (#2813)。
func TestApplicationRegister_ImmediateIgnoresTicketStoreFailure(t *testing.T) {
	env := newApprovalEnv(t, true)
	env.handler.SetTicketStore(&stubTicketStore{createErr: errors.New("boom")})
	env.apps.app = approvedApplication()

	rec := doPost(env.handler.ApplicationRegister,
		`{"claimCode":"c","username":"newbie","password":"hunter22"}`)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// **メール必須を切る前に始まっていた確認待ちの残骸は破棄する。** 残すと、
// 届いた確認リンクが後から踏めてしまう。
func TestApplicationRegister_ImmediateDiscardsLeftoverTicket(t *testing.T) {
	env := newApprovalEnv(t, true)
	tickets := &stubTicketStore{}
	env.handler.SetTicketStore(tickets)
	app := approvedApplication()
	leftover := "old-ticket"
	app.TicketID = &leftover
	env.apps.app = app

	rec := doPost(env.handler.ApplicationRegister,
		`{"claimCode":"c","username":"newbie","password":"hunter22"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, tickets.created, "新しくは発行しない")
	assert.Equal(t, []string{leftover}, tickets.deleted)
}

// 登録に失敗したら申請を完了扱いにしない。
func TestApplicationRegister_ImmediateFailureDoesNotCompleteApplication(t *testing.T) {
	env := newApprovalEnv(t, true)
	tickets := &stubTicketStore{}
	env.handler.SetTicketStore(tickets)
	env.apps.app = approvedApplication()

	rec := doPost(env.handler.ApplicationRegister,
		`{"claimCode":"c","username":"!!!invalid!!!","password":"hunter22"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, tickets.created)
	assert.Empty(t, env.apps.completedApp, "失敗したら申請は完了扱いにしない")
}

// 完了記録に失敗してもアカウントは作られている。**ここで 500 を返すと、
// 利用者には「失敗したのに登録されている」状態になる。**
func TestApplicationRegister_BookkeepingFailuresDoNotFailSignup(t *testing.T) {
	env := newApprovalEnv(t, true)
	env.apps.app = approvedApplication()
	env.apps.markErr = errors.New("boom")

	rec := doPost(env.handler.ApplicationRegister,
		`{"claimCode":"c","username":"newbie","password":"hunter22"}`)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// 通常の /api/signup と同じ形のエラーを返すこと。**クライアントが 1 組の
// エラー処理で済むように。**
func TestApplicationRegister_SignupErrorShapes(t *testing.T) {
	tests := []struct {
		name     string
		username string
		password string
		wantBody string
	}{
		{name: "invalid username", username: "!!!", password: "hunter22", wantBody: "INVALID_USERNAME"},
		{name: "password too long", username: "newbie", password: string(make([]byte, 2000)), wantBody: "PASSWORD_TOO_LONG"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newApprovalEnv(t, true)
			env.apps.app = approvedApplication()

			body, err := json.Marshal(map[string]string{
				"claimCode": "c", "username": tt.username, "password": tt.password,
			})
			require.NoError(t, err)
			rec := doPost(env.handler.ApplicationRegister, string(body))
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), tt.wantBody)
		})
	}
}

func TestApplicationRegister_DuplicateUsername(t *testing.T) {
	env := newApprovalEnv(t, true)
	env.apps.app = approvedApplication()

	body := `{"claimCode":"c","username":"newbie","password":"hunter22"}`
	require.Equal(t, http.StatusOK, doPost(env.handler.ApplicationRegister, body).Code)

	rec := doPost(env.handler.ApplicationRegister, body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 承認制のときは通常の登録経路を閉じる (#2557)。承認フローは signupService を
// 直接呼ぶのでこのゲートを通らない。
func TestSignup_BlockedWhileApprovalRequired(t *testing.T) {
	h, _, metaRepo := newTestHandler(t)
	metaRepo.Meta = &model.Meta{ID: "x", ApprovalRequiredForSignup: true}

	rec := doPost(h.Signup, `{"username":"newbie","password":"hunter22"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "APPROVAL_REQUIRED")
}

func TestSignup_AllowedWhenApprovalIsOff(t *testing.T) {
	h, _, metaRepo := newTestHandler(t)
	metaRepo.Meta = &model.Meta{ID: "x", ApprovalRequiredForSignup: false}

	rec := doPost(h.Signup, `{"username":"newbie","password":"hunter22"}`)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// --- メール必須との併用 (#2571) ---

// newApprovalEnvWithEmail wires the pending repo and the mail sender so the
// approval flow can go through email confirmation.
func newApprovalEnvWithEmail(t *testing.T) (*approvalEnv, *testutil.MockUserPendingRepository, *stubTicketStore, chan string) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{
		ID: "x", ApprovalRequiredForSignup: true, EmailRequiredForSignup: true,
	}
	pendingRepo := testutil.NewMockUserPendingRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coresignup.NewService(userRepo, metaRepo, idGen)
	svc.SetUserPendingRepo(pendingRepo)
	h := apisignup.NewHandler(svc, metaRepo, idGen)

	// **審査した管理者を入れておく。** 入れないと「ticket に漏れない」(#2805) の
	// アサートが空振りする — 元が nil なら何をしても nil になる。
	app := approvedApplication()
	moderator := "mod-1"
	app.ProcessedByID = &moderator
	apps := &stubApplications{app: app}
	h.SetSignupApplications(apps)
	tickets := &stubTicketStore{}
	h.SetTicketStore(tickets)

	// 送信は goroutine なので、宛先を channel で受ける。
	sent := make(chan string, 1)
	h.SetEmailSender("https://example.test", func(to string, _ miscsmtp.Message) {
		sent <- to
	})
	return &approvalEnv{handler: h, apps: apps, meta: metaRepo.Meta}, pendingRepo, tickets, sent
}

// メール必須のときは即時作成せず、確認メールの経路に乗せる。**乗せないと、設定
// しているのに実際にはメールを要求しない状態になる。**
func TestApplicationRegister_EmailRequired_CreatesPendingAndSendsEmail(t *testing.T) {
	env, pendingRepo, tickets, sent := newApprovalEnvWithEmail(t)

	rec := doPost(env.handler.ApplicationRegister,
		`{"claimCode":"c","username":"newbie","password":"hunter22","emailAddress":"newbie@example.com"}`)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	require.Len(t, pendingRepo.Rows, 1)
	row := onlyPending(t, pendingRepo)
	assert.Equal(t, "newbie", row.Username)
	assert.Equal(t, "newbie@example.com", row.Email)

	// **申請 ID を持たせないと、確認完了時に申請を completed にできない。**
	require.NotNil(t, row.SignupApplicationID)
	assert.Equal(t, "app-1", *row.SignupApplicationID)

	// 発行した ticket は pending に結び付き、まだ消費されていない。
	require.Len(t, tickets.created, 1)
	issued := tickets.created[0]
	assert.NotEmpty(t, issued.Code)
	require.NotNil(t, issued.ExpiresAt, "期限を切ること")
	assert.True(t, issued.ExpiresAt.Before(time.Now().Add(time.Hour)),
		"渡していない credential を長く残さない")
	// **createdById は入れない (#2805)。** 入れると、承認するたびに審査した管理者
	// 名義の招待が 1 枚増え、`invite/create` の上限を食い `invite/list` にも出る。
	// どちらの query も `WHERE "createdById" = ?` なので NULL なら両方から外れる。
	// #2813 で即時作成が ticket を発行しなくなったので、**この保証が効くのは
	// この経路だけ**になった。
	assert.Nil(t, issued.CreatedByID, "承認は審査した管理者の招待枠を消費しない")
	// レスポンスにコードが漏れていないこと。
	assert.NotContains(t, rec.Body.String(), issued.Code)

	require.NotNil(t, row.InvitationTicketID)
	assert.Equal(t, issued.ID, *row.InvitationTicketID)
	assert.Equal(t, issued.ID, tickets.pendingTkt)
	assert.Equal(t, row.ID, tickets.pendingRow)
	assert.Empty(t, tickets.usedTkt, "確認が終わるまで消費しない")

	// 次の試行で破棄できるよう、申請が ticket を覚えている。
	assert.Equal(t, "app-1", env.apps.markedTktApp)
	assert.Equal(t, issued.ID, env.apps.markedTkt)

	// この時点ではまだアカウントが無いので完了扱いにしない。
	assert.Empty(t, env.apps.completedApp)

	select {
	case to := <-sent:
		assert.Equal(t, "newbie@example.com", to)
	case <-time.After(2 * time.Second):
		t.Fatal("確認メールが送られていない")
	}
}

func TestApplicationRegister_EmailRequired_RejectsMissingOrBadAddress(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		env, pendingRepo, _, _ := newApprovalEnvWithEmail(t)
		rec := doPost(env.handler.ApplicationRegister,
			`{"claimCode":"c","username":"newbie","password":"hunter22"}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Empty(t, pendingRepo.Rows)
	})

	t.Run("banned domain", func(t *testing.T) {
		env, pendingRepo, tickets, _ := newApprovalEnvWithEmail(t)
		env.meta.BannedEmailDomains = []string{"bad.example"}
		rec := doPost(env.handler.ApplicationRegister,
			`{"claimCode":"c","username":"newbie","password":"hunter22","emailAddress":"x@bad.example"}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		// **UNAVAILABLE は使わない。** この endpoint では既に「承認制が無効」の
		// 意味を持っており、同じ code だと client がどちらかしか出せない。
		assert.Contains(t, rec.Body.String(), "EMAIL_UNAVAILABLE")
		assert.Empty(t, pendingRepo.Rows)
		// **アドレスを見る前に ticket を発行しない。** 弾いた分だけ浮いた招待が残る。
		assert.Empty(t, tickets.created)
	})
}

// メール必須が無効なら従来どおり即時作成する。
func TestApplicationRegister_EmailNotRequired_CreatesImmediately(t *testing.T) {
	env := newApprovalEnv(t, true)
	env.apps.app = approvedApplication()

	rec := doPost(env.handler.ApplicationRegister,
		`{"claimCode":"c","username":"newbie","password":"hunter22"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "app-1", env.apps.completedApp, "その場で完了扱いになる")
	assert.Empty(t, env.apps.markedTkt, "確認待ちの記録は要らない")
}

// **やり直しは前回の ticket を破棄してから進む。** 積み上げると、届いたメールを
// すべて確認して 1 つの承認から複数アカウントを作れる。
func TestApplicationRegister_EmailRequired_RetryDiscardsPreviousTicket(t *testing.T) {
	env, pendingRepo, tickets, sent := newApprovalEnvWithEmail(t)

	rec := doPost(env.handler.ApplicationRegister,
		`{"claimCode":"c","username":"newbie","password":"hunter22","emailAddress":"typo@example.com"}`)
	require.Equal(t, http.StatusNoContent, rec.Code)
	<-sent
	first := tickets.created[0].ID
	assert.Empty(t, tickets.deleted, "1 回目は破棄しない")

	// 申請が 1 回目の ticket を覚えている状態を再現する。
	env.apps.app.TicketID = &first

	rec = doPost(env.handler.ApplicationRegister,
		`{"claimCode":"c","username":"newbie","password":"hunter22","emailAddress":"right@example.com"}`)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	<-sent

	assert.Equal(t, []string{first}, tickets.deleted, "前回の ticket を破棄する")
	require.Len(t, tickets.created, 2)
	assert.NotEqual(t, first, tickets.created[1].ID)
	require.Len(t, pendingRepo.Rows, 2)
	second := pendingByEmail(t, pendingRepo, "right@example.com")
	require.NotNil(t, second.InvitationTicketID)
	assert.Equal(t, tickets.created[1].ID, *second.InvitationTicketID)
}

// pending 作成に失敗したら ticket を残さない。
func TestApplicationRegister_EmailRequired_DiscardsTicketOnPendingFailure(t *testing.T) {
	env, _, tickets, _ := newApprovalEnvWithEmail(t)

	rec := doPost(env.handler.ApplicationRegister,
		`{"claimCode":"c","username":"!!!invalid!!!","password":"hunter22","emailAddress":"x@example.com"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	require.Len(t, tickets.created, 1)
	assert.Equal(t, []string{tickets.created[0].ID}, tickets.deleted)
	assert.Empty(t, env.apps.completedApp)
}

// **pending を作る前に、行ロック付きで申請の状態を見直す。** ここまでの状態確認は
// ロック無しの読みなので、その後に期限切れになったり別の確認が完了したりしうる。
// 素通しすると、期限切れの申請から確認メールが飛び、リンクを踏んだ時点で承認の
// 記録に残らないアカウントが出来上がる。
func TestApplicationRegister_EmailRequired_AbortsWhenApplicationSlippedAway(t *testing.T) {
	env, pendingRepo, tickets, _ := newApprovalEnvWithEmail(t)
	// 状態を見た後、ロックを取った時点では承認済みでなくなっていた状況。
	env.apps.markTicketErr = signupapplication.ErrExpired

	rec := doPost(env.handler.ApplicationRegister,
		`{"claimCode":"c","username":"newbie","password":"hunter22","emailAddress":"x@example.com"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NOT_APPROVED")

	assert.Empty(t, pendingRepo.Rows, "確認メールの経路に乗せない")
	require.Len(t, tickets.created, 1)
	assert.Equal(t, []string{tickets.created[0].ID}, tickets.deleted, "浮いた招待を残さない")
}

// MarkPending の失敗は再送防止窓が効かなくなるだけで、pending は成立している。
func TestApplicationRegister_EmailRequired_MarkPendingFailureStillSucceeds(t *testing.T) {
	env, pendingRepo, tickets, sent := newApprovalEnvWithEmail(t)
	tickets.markPendingErr = errors.New("boom")

	rec := doPost(env.handler.ApplicationRegister,
		`{"claimCode":"c","username":"newbie","password":"hunter22","emailAddress":"x@example.com"}`)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Len(t, pendingRepo.Rows, 1)
	<-sent
}

// onlyPending returns the single pending row, failing when there is not exactly one.
func onlyPending(t *testing.T, repo *testutil.MockUserPendingRepository) *model.UserPending {
	t.Helper()
	require.Len(t, repo.Rows, 1)
	for _, r := range repo.Rows {
		return r
	}
	return nil
}

func pendingByEmail(t *testing.T, repo *testutil.MockUserPendingRepository, email string) *model.UserPending {
	t.Helper()
	for _, r := range repo.Rows {
		if r.Email == email {
			return r
		}
	}
	t.Fatalf("pending row for %s not found", email)
	return nil
}

// 確認が終わって初めてアカウントになり、そこで申請が完了扱いになる。
// **completed にしないと approved のまま残り、同じクレームコードで何度でも
// 登録を始められる。**
func TestSignupPending_CompletesApplication(t *testing.T) {
	env, pendingRepo, tickets, sent := newApprovalEnvWithEmail(t)

	rec := doPost(env.handler.ApplicationRegister,
		`{"claimCode":"c","username":"newbie","password":"hunter22","emailAddress":"x@example.com"}`)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	<-sent
	row := onlyPending(t, pendingRepo)

	rec = doPost(env.handler.SignupPending, `{"code":"`+row.Code+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := parseResp(t, rec)

	assert.Equal(t, "app-1", env.apps.completedApp)
	assert.Equal(t, resp["id"], env.apps.completedUsr)
	assert.Equal(t, tickets.created[0].ID, env.apps.completedTkt)
}

// 申請と無関係な pending の確認では申請に触らない。
func TestSignupPending_WithoutApplicationLeavesApplicationsAlone(t *testing.T) {
	env, pendingRepo, _, _ := newApprovalEnvWithEmail(t)
	env.meta.ApprovalRequiredForSignup = false

	// 通常の /api/signup 経路で積まれた pending を模す。
	rec := doPost(env.handler.Signup,
		`{"username":"other","password":"hunter22","emailAddress":"other@example.com"}`)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	row := onlyPending(t, pendingRepo)
	require.Nil(t, row.SignupApplicationID)

	rec = doPost(env.handler.SignupPending, `{"code":"`+row.Code+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, env.apps.completedApp)
}

// --- 署名付きフォームトークン (#2806) ---
//
// **captcha の代替ではない。** 止まるのは「フォームを取得せずに endpoint を直接
// 叩く」bot だけで、待って送るスクリプトには 2 倍のリクエストしか課さない。
// 位置づけの全文は core/signupform の doc にある。

type fakeFormNonces struct {
	burnt map[string]bool
	// err は nonce store の障害 (Redis 断など) を注入する。
	err error
}

func (f *fakeFormNonces) Burn(_ context.Context, nonce string, _ time.Duration) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if f.burnt == nil {
		f.burnt = map[string]bool{}
	}
	if f.burnt[nonce] {
		return false, nil
	}
	f.burnt[nonce] = true
	return true, nil
}

// withFormTokens wires an issuer with no minimum dwell time. 滞在時間そのものは
// core/signupform の単体テストが押さえるので、ここでは要求の有無を見る。
func withFormTokens(t *testing.T, env *approvalEnv, minAge time.Duration) *signupform.Issuer {
	t.Helper()
	issuer, _ := withFormTokenNonces(t, env, minAge)
	return issuer
}

func withFormTokenNonces(t *testing.T, env *approvalEnv, minAge time.Duration) (*signupform.Issuer, *fakeFormNonces) {
	t.Helper()
	nonces := &fakeFormNonces{}
	issuer := signupform.NewIssuerWithTimings([]byte("test-key"), nonces, minAge, time.Minute)
	require.NotNil(t, issuer)
	env.handler.SetFormTokenIssuer(issuer)
	return issuer, nonces
}

func applyBody(t *testing.T, token string) string {
	t.Helper()
	return `{"answers":["理由"],"formToken":` + strconvQuote(token) + `}`
}

func strconvQuote(s string) string { return `"` + s + `"` }

// captcha の実 provider が 1 つも無いときは、フォームトークンが要る。
func TestApplicationApply_RequiresFormTokenWithoutRealCaptcha(t *testing.T) {
	env := newApprovalEnv(t, true)
	issuer := withFormTokens(t, env, 0)

	rec := doPost(env.handler.ApplicationApply, `{"answers":["理由"]}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "FORM_TOKEN_INVALID")
	// **応答を書くだけでは足りない。** `c.JSON` は成功時に nil を返すので、
	// 呼び出し側が error だけを見ると素通りして申請行を作ってしまう。
	assert.Nil(t, env.apps.appliedAnswers, "弾いたら申請行を作らない")

	token, err := issuer.Issue(signupform.PurposeApply)
	require.NoError(t, err)
	rec = doPost(env.handler.ApplicationApply, applyBody(t, token))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// **使い捨てにしないと 1 枚を無限に使い回される。**
func TestApplicationApply_FormTokenIsSingleUse(t *testing.T) {
	env := newApprovalEnv(t, true)
	issuer := withFormTokens(t, env, 0)
	token, err := issuer.Issue(signupform.PurposeApply)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, doPost(env.handler.ApplicationApply, applyBody(t, token)).Code)
	rec := doPost(env.handler.ApplicationApply, applyBody(t, token))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "FORM_TOKEN_INVALID")
}

// 早すぎる送信は専用のコードで返す。**nonce は焼かれていない**ので、待てば通る。
func TestApplicationApply_FormTokenTooSoon(t *testing.T) {
	env := newApprovalEnv(t, true)
	issuer := withFormTokens(t, env, time.Hour)
	token, err := issuer.Issue(signupform.PurposeApply)
	require.NoError(t, err)

	rec := doPost(env.handler.ApplicationApply, applyBody(t, token))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "FORM_TOKEN_TOO_SOON")
	assert.Nil(t, env.apps.appliedAnswers, "弾いたら申請行を作らない")
}

// **testcaptcha は実 provider として数えない。** 数えると「マジック文字列一致
// だけが効いていて、フォームトークンは要求されない」という最悪の組み合わせになる。
func TestApplicationApply_TestcaptchaStillRequiresFormToken(t *testing.T) {
	env := newApprovalEnv(t, true)
	env.meta.EnableTestcaptcha = true
	env.handler.SetCaptcha(captcha.NewService(env.meta))
	withFormTokens(t, env, 0)

	rec := doPost(env.handler.ApplicationApply,
		`{"answers":["理由"],"testcaptcha-response":"testcaptcha-passed"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "FORM_TOKEN_INVALID")
	assert.Nil(t, env.apps.appliedAnswers, "弾いたら申請行を作らない")
}

// 実 provider が有効なときは要求しない (二重の負担を課さない)。
func TestApplicationApply_RealCaptchaSkipsFormToken(t *testing.T) {
	env := newApprovalEnv(t, true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":true}`))
	}))
	defer srv.Close()
	secret, instanceURL := "s", srv.URL
	// **sitekey も要る (#3037 レビュー 2 周目)。** upstream
	// `SignupApiService.ts:86` は 3 つ揃って初めて mcaptcha を有効にする。
	siteKey := "sk"
	env.meta.EnableMcaptcha = true
	env.meta.McaptchaSecretKey = &secret
	env.meta.McaptchaSiteKey = &siteKey
	env.meta.McaptchaInstanceURL = &instanceURL
	env.handler.SetCaptcha(captcha.NewServiceWithClient(env.meta, srv.Client()))
	withFormTokens(t, env, 0)

	rec := doPost(env.handler.ApplicationApply,
		`{"answers":["理由"],"m-captcha-response":"ok"}`)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// testMode は既存 captcha と同じ扱い。**この endpoint を叩く e2e は Playwright にも
// 本家 backend e2e にも無い** (mk-go 独自なので upstream のテストは持たない) ので、
// 根拠は「captcha と扱いを分ける理由が無い」で足りる。
func TestApplicationApply_TestModeSkipsFormToken(t *testing.T) {
	env := newApprovalEnv(t, true)
	withFormTokens(t, env, 0)
	env.handler.SetTestMode(true)

	assert.Equal(t, http.StatusOK, doPost(env.handler.ApplicationApply, `{"answers":["理由"]}`).Code)
}

// 未配線なら要求しない (fail-open)。**router の recordCriticalWiring が起動時に
// 止める**ので、本番でこの状態にはならない。
func TestApplicationApply_UnwiredIssuerDoesNotRequireToken(t *testing.T) {
	env := newApprovalEnv(t, true)
	assert.False(t, env.handler.HasFormTokens())
	assert.Equal(t, http.StatusOK, doPost(env.handler.ApplicationApply, `{"answers":["理由"]}`).Code)
}

func TestApplicationFormToken_IssuesUsableToken(t *testing.T) {
	env := newApprovalEnv(t, true)
	withFormTokens(t, env, 0)

	rec := doPost(env.handler.ApplicationFormToken, `{}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Token          string `json:"token"`
		MinWaitSeconds int    `json:"minWaitSeconds"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Token)
	assert.Equal(t, 0, resp.MinWaitSeconds)

	// 発行したものがそのまま apply で通ること。
	assert.Equal(t, http.StatusOK, doPost(env.handler.ApplicationApply, applyBody(t, resp.Token)).Code)
}

// **minWaitSeconds は切り上げる。** 切り捨てにすると、非整数秒にした瞬間に画面の
// ほうが短く待ち、正規の利用者が FORM_TOKEN_TOO_SOON を見る。
func TestApplicationFormToken_MinWaitSecondsRoundsUp(t *testing.T) {
	env := newApprovalEnv(t, true)
	withFormTokens(t, env, 1500*time.Millisecond)

	rec := doPost(env.handler.ApplicationFormToken, `{}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		MinWaitSeconds int `json:"minWaitSeconds"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.MinWaitSeconds, "画面はサーバーより短く待ってはいけない")
}

// 実 provider が有効なら空文字を返す。画面は「空なら送信を抑えない」だけで済む。
func TestApplicationFormToken_EmptyWhenRealCaptcha(t *testing.T) {
	env := newApprovalEnv(t, true)
	secret, instanceURL := "s", "https://mcaptcha.example"
	// **sitekey も要る (#3037 レビュー 2 周目)。**
	siteKey := "sk"
	env.meta.EnableMcaptcha = true
	env.meta.McaptchaSecretKey = &secret
	env.meta.McaptchaSiteKey = &siteKey
	env.meta.McaptchaInstanceURL = &instanceURL
	env.handler.SetCaptcha(captcha.NewService(env.meta))
	withFormTokens(t, env, 0)

	rec := doPost(env.handler.ApplicationFormToken, `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"token":""`)
}

// 承認制が無効なら発行もしない。
func TestApplicationFormToken_DisabledReturns503(t *testing.T) {
	env := newApprovalEnv(t, false)
	withFormTokens(t, env, 0)
	assert.Equal(t, http.StatusServiceUnavailable,
		doPost(env.handler.ApplicationFormToken, `{}`).Code)
}

// **回答の検証で落ちても nonce を焼かない (#2806)。** 焼くと、入力を直して
// 送り直した利用者が「フォームを開き直せ」と言われる。
func TestApplicationApply_ValidationFailureKeepsFormToken(t *testing.T) {
	env := newApprovalEnv(t, true)
	issuer := withFormTokens(t, env, 0)
	token, err := issuer.Issue(signupform.PurposeApply)
	require.NoError(t, err)

	// 必須項目が空 → ANSWER_REQUIRED。
	rec := doPost(env.handler.ApplicationApply, `{"answers":[""],"formToken":`+strconvQuote(token)+`}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "ANSWER_REQUIRED")

	// 同じ token で通ること (焼かれていない)。
	assert.Equal(t, http.StatusOK, doPost(env.handler.ApplicationApply, applyBody(t, token)).Code)
}

// **nonce store が落ちても素通しにしない (#2806)。** 素通しにすると、captcha を
// 設定していないインスタンスで防波堤が 1 つも無い状態に戻る。`NOT_APPROVED` の
// ように利用者向けの答えへ丸めず 500 にするのは、障害をドメインの答えに化け
// させないため (#2799 と同じ)。
func TestApplicationApply_NonceStoreFailureIsNotFailOpen(t *testing.T) {
	env := newApprovalEnv(t, true)
	issuer, nonces := withFormTokenNonces(t, env, 0)
	token, err := issuer.Issue(signupform.PurposeApply)
	require.NoError(t, err)

	nonces.err = errors.New("redis down")
	rec := doPost(env.handler.ApplicationApply, applyBody(t, token))
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "FORM_TOKEN")
	assert.Nil(t, env.apps.appliedAnswers, "申請行を作らない")
}

// 申請経由の登録も最小文字数を受け、`USERNAME_TOO_SHORT` で返す (#3015)。
//
// **`signupServiceError` の写像を落とすと 500 になる。** 承認された人が
// 何度やっても登録できず、しかも原因が「サーバーの不具合」に見える。
// 即時作成とメール確認の**両方**を見る — 通る関数が違う
// (`SignupForApplication` / `CreatePendingForApplication`)。
func TestApplicationRegister_UsernameTooShort(t *testing.T) {
	t.Run("即時作成", func(t *testing.T) {
		env := newApprovalEnv(t, true)
		env.apps.app = approvedApplication()
		env.meta.MinimumUsernameLength = 5

		rec := doPost(env.handler.ApplicationRegister,
			`{"claimCode":"c","username":"abcd","password":"hunter22"}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "USERNAME_TOO_SHORT")

		// **弾きすぎていないこと。** 常に 400 でもこの subtest 単体は緑になる。
		rec = doPost(env.handler.ApplicationRegister,
			`{"claimCode":"c","username":"abcde","password":"hunter22"}`)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("メール確認経由", func(t *testing.T) {
		env, pendingRepo, _, _ := newApprovalEnvWithEmail(t)
		env.meta.MinimumUsernameLength = 5

		rec := doPost(env.handler.ApplicationRegister,
			`{"claimCode":"c","username":"abcd","password":"hunter22","emailAddress":"a@example.com"}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "USERNAME_TOO_SHORT")
		assert.Empty(t, pendingRepo.Rows, "弾いたのに pending を作っている")
	})
}

// 申請経路の error は **Misskey misc 形式** (`{"error":{"code":...}}`) で返す。
//
// **Fastify 形式だと code がクライアントに届かない。** あちらは code を
// `message` にしか載せず、frontend の `misskeyApi` は `body.error` —
// Fastify 形式では文字列 `"Bad Request"` — で reject する。受け側は
// `err.code` で分岐するので**どの case にも当たらず**、
// 「処理に失敗しました。時間をおいて試してください。」だけが出ていた。
//
// `signup-application/*` は upstream に存在しない mk-go 独自 endpoint なので、
// 形を揃える相手もいない (`apierr/fastify.go` が Fastify 化の対象を 4 endpoint に
// 限っている)。
func TestApplicationRegister_ErrorsCarryCodeInTheMiscShape(t *testing.T) {
	newEnv := func(t *testing.T) *approvalEnv {
		t.Helper()
		env := newApprovalEnv(t, true)
		env.apps.app = approvedApplication()
		return env
	}

	for name, tc := range map[string]struct {
		setup func(env *approvalEnv)
		body  string
		want  string
	}{
		"USERNAME_TOO_SHORT": {
			setup: func(env *approvalEnv) { env.meta.MinimumUsernameLength = 5 },
			body:  `{"claimCode":"c","username":"abcd","password":"hunter22"}`,
			want:  "USERNAME_TOO_SHORT",
		},
		"INVALID_USERNAME": {
			body: `{"claimCode":"c","username":"!!!","password":"hunter22"}`,
			want: "INVALID_USERNAME",
		},
		"USED_USERNAME": {
			setup: func(env *approvalEnv) { env.meta.PreservedUsernames = model.StringArray{"admin"} },
			body:  `{"claimCode":"c","username":"admin","password":"hunter22"}`,
			want:  "USED_USERNAME",
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := newEnv(t)
			if tc.setup != nil {
				tc.setup(env)
			}
			rec := doPost(env.handler.ApplicationRegister, tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code)

			// **`code` を名指しで読む。** body の文字列一致だと、Fastify 形式の
			// `"message":"Error: USERNAME_TOO_SHORT"` でも通ってしまう
			// (= 直したはずの形に戻しても緑になる)。
			var resp struct {
				Error struct {
					Code string `json:"code"`
					ID   string `json:"id"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
			assert.Equal(t, tc.want, resp.Error.Code, "クライアントが分岐に使う code が届いていない")
			assert.NotEmpty(t, resp.Error.ID)
		})
	}
}
