package invite

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/core/role"
	"github.com/shiroha-a/mk/internal/entitycompat/shapetest"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errStub = errors.New("stub error")

// stubInvitePolicy is a minimal role.PolicyProvider double.
type stubInvitePolicy struct {
	policies map[string]any
}

func (s *stubInvitePolicy) GetUserPolicies(_ string) map[string]any { return s.policies }

type recordingCountRepo struct {
	*testutil.MockRegistrationTicketRepository
	count     int64
	creatorID string
	sinceID   string
}

func (r *recordingCountRepo) CountByCreatorSince(creatorID, sinceID string) (int64, error) {
	r.creatorID = creatorID
	r.sinceID = sinceID
	return r.count, nil
}

type recordingIDGenerator struct {
	generated []time.Time
}

func (g *recordingIDGenerator) Generate(at time.Time) string {
	g.generated = append(g.generated, at)
	return fmt.Sprintf("generated-%d", len(g.generated)-1)
}

func (g *recordingIDGenerator) ParseTime(string) (time.Time, error) {
	return time.Time{}, errors.New("not parsed")
}

func newTestHandler(t *testing.T) (*Handler, *testutil.MockRegistrationTicketRepository) {
	t.Helper()
	repo := testutil.NewMockRegistrationTicketRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return NewHandler(repo, idGen), repo
}

func post(handler func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	_ = handler(c)
	return rec
}

// --- Create -----------------------------------------------------------------

func TestCreate_NoPolicyProviderSucceeds(t *testing.T) {
	h, repo := newTestHandler(t)
	rec := post(h.Create, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, repo.Tickets, 1)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["code"])
	// inviteExpirationTime 未設定 → expiresAt は null
	assert.Nil(t, resp["expiresAt"])
	assert.Equal(t, false, resp["used"])
	assert.NotEmpty(t, resp["createdAt"])
	shapetest.Assert(t, "InviteCode", resp) // L3 (#1270)
}

func TestCreate_LimitExceeded(t *testing.T) {
	h, repo := newTestHandler(t)
	// 既に 2 invite 保有 (CountByCreatorSince で count される)
	user := "u1"
	repo.Tickets["t1"] = &model.RegistrationTicket{ID: "z_t1", CreatedByID: &user}
	repo.Tickets["t2"] = &model.RegistrationTicket{ID: "z_t2", CreatedByID: &user}
	h.SetRolePolicyProvider(&stubInvitePolicy{policies: map[string]any{
		"inviteLimit":      2,
		"inviteLimitCycle": 60 * 24, // 1 day, ensure sinceID is well before now
	}})
	rec := post(h.Create, `{}`, &model.User{ID: user})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "EXCEEDED_LIMIT_OF_CREATE_INVITE_CODE")
	assert.Contains(t, rec.Body.String(), "8b165dd3-6f37-4557-8db1-73175d63c641")
}

func TestCreate_LimitPassesUnderLimit(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetRolePolicyProvider(&stubInvitePolicy{policies: map[string]any{
		"inviteLimit":      10,
		"inviteLimitCycle": 60,
	}})
	rec := post(h.Create, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code, "limit 内なら通常成功")
}

func TestCreate_MinInt64InviteCycleUsesSaturatingCutoff(t *testing.T) {
	cycleMinutes := float64(math.MinInt64) / float64(time.Minute)
	cycle, ok := role.PolicyMinutes(cycleMinutes)
	require.True(t, ok)
	require.Equal(t, time.Duration(math.MinInt64), cycle)

	repo := &recordingCountRepo{
		MockRegistrationTicketRepository: testutil.NewMockRegistrationTicketRepository(),
	}
	idGen := &recordingIDGenerator{}
	h := NewHandler(repo, idGen)
	h.SetRolePolicyProvider(&stubInvitePolicy{policies: map[string]any{
		"inviteLimit":      1,
		"inviteLimitCycle": cycleMinutes,
	}})

	rec := post(h.Create, `{}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "u1", repo.creatorID)
	assert.Equal(t, "generated-0", repo.sinceID)
	require.Len(t, idGen.generated, 2)
	assert.Equal(t, time.Duration(math.MaxInt64), idGen.generated[0].Sub(idGen.generated[1]))
}

func TestCreate_LimitZeroOrUnsetIsUnlimited(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetRolePolicyProvider(&stubInvitePolicy{policies: map[string]any{
		"inviteLimit": 0, // upstream falsy = skip
	}})
	rec := post(h.Create, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code, "policy 0 は upstream falsy 相当で gate skip")
}

func TestCreate_ExpiresAtSetFromPolicy(t *testing.T) {
	h, repo := newTestHandler(t)
	h.SetRolePolicyProvider(&stubInvitePolicy{policies: map[string]any{
		"inviteExpirationTime": 60, // 60 minutes
	}})
	rec := post(h.Create, `{}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, repo.Tickets, 1)
	var saved *model.RegistrationTicket
	for _, t := range repo.Tickets {
		saved = t
	}
	require.NotNil(t, saved.ExpiresAt, "expiresAt は inviteExpirationTime > 0 で設定される")
}

// countErrRepo は CountByCreatorSince だけ error を返す stub。
type countErrRepo struct {
	*testutil.MockRegistrationTicketRepository
}

func (r *countErrRepo) CountByCreatorSince(_, _ string) (int64, error) { return 0, errStub }

func TestCreate_CountErrorReturns500(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&countErrRepo{testutil.NewMockRegistrationTicketRepository()}, idGen)
	h.SetRolePolicyProvider(&stubInvitePolicy{policies: map[string]any{"inviteLimit": 5}})
	rec := post(h.Create, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// failCreateRepo は Create だけ error を返す stub。
type failCreateRepo struct {
	*testutil.MockRegistrationTicketRepository
}

func (r *failCreateRepo) Create(_ *model.RegistrationTicket) error { return errStub }

func TestCreate_RepoCreateError(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&failCreateRepo{testutil.NewMockRegistrationTicketRepository()}, idGen)
	rec := post(h.Create, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- List -------------------------------------------------------------------

func TestList_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	user := "u1"
	repo.Tickets["t1"] = &model.RegistrationTicket{ID: "z_a1", Code: "code-a1", CreatedByID: &user}
	repo.Tickets["t2"] = &model.RegistrationTicket{ID: "z_a2", Code: "code-a2", CreatedByID: &user}
	otherUser := "u2"
	repo.Tickets["t3"] = &model.RegistrationTicket{ID: "z_a3", Code: "code-a3", CreatedByID: &otherUser}
	rec := post(h.List, `{"limit":50}`, &model.User{ID: user})
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Len(t, out, 2, "他 user の invite は除外される")
}

func TestList_Empty(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := post(h.List, `{}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]", strings.TrimSpace(rec.Body.String()))
}

// #1776: list は createdBy を呼び出し元自身の UserLite で、usedBy を使用済 ticket の
// 利用者 UserLite で埋める (null 固定ではない)。
func TestList_ResolvesCreatedByAndUsedBy(t *testing.T) {
	h, repo := newTestHandler(t)
	me := "u1"
	usedBy := "u2"
	repo.Tickets["t1"] = &model.RegistrationTicket{ID: "z_a1", Code: "code-a1", CreatedByID: &me}
	// 実運用では MarkUsed が usedById と usedAt を両方立てる。片方だけの
	// fixture にすると `used` の由来 (#2812) を取り違える。
	usedAt := time.Now()
	repo.Tickets["t2"] = &model.RegistrationTicket{ID: "z_a2", Code: "code-a2", CreatedByID: &me, UsedByID: &usedBy, UsedAt: &usedAt}

	userRepo := testutil.NewMockUserRepository()
	userRepo.Users[me] = &model.User{ID: me, Username: "alice", UsernameLower: "alice"}
	userRepo.Users[usedBy] = &model.User{ID: usedBy, Username: "bob", UsernameLower: "bob"}
	h.SetUserRepo(userRepo)

	rec := post(h.List, `{"limit":50}`, &model.User{ID: me, Username: "alice", UsernameLower: "alice"})
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 2)
	byID := map[string]map[string]any{}
	for _, e := range out {
		byID[e["id"].(string)] = e
	}
	// 全 entry に createdBy (= me) が乗る。
	for _, e := range out {
		cb, ok := e["createdBy"].(map[string]any)
		require.True(t, ok, "createdBy must be a UserLite object")
		assert.Equal(t, "alice", cb["username"])
	}
	// 未使用 ticket は usedBy=null、使用済 ticket は利用者の UserLite。
	assert.Nil(t, byID["z_a1"]["usedBy"])
	ub, ok := byID["z_a2"]["usedBy"].(map[string]any)
	require.True(t, ok, "used ticket must resolve usedBy UserLite")
	assert.Equal(t, "bob", ub["username"])
}

// #1948-10: List の expiresAt / usedAt は upstream toISOString() (.000Z / null)。
// raw *time.Time の RFC3339Nano だと wire-byte が乖離する。
func TestList_DateFormat(t *testing.T) {
	h, repo := newTestHandler(t)
	me := "u1"
	exp := time.Date(2026, 6, 21, 1, 2, 3, 456_000_000, time.UTC)
	used := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	repo.Tickets["t1"] = &model.RegistrationTicket{ID: "z_a1", Code: "c1", CreatedByID: &me, ExpiresAt: &exp, UsedAt: &used}
	repo.Tickets["t2"] = &model.RegistrationTicket{ID: "z_a2", Code: "c2", CreatedByID: &me}

	rec := post(h.List, `{"limit":50}`, &model.User{ID: me})
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	byID := map[string]map[string]any{}
	for _, e := range out {
		byID[e["id"].(string)] = e
	}
	assert.Equal(t, "2026-06-21T01:02:03.456Z", byID["z_a1"]["expiresAt"], "expiresAt は .000Z (#1948-10)")
	assert.Equal(t, "2026-01-02T03:04:05.000Z", byID["z_a1"]["usedAt"], "usedAt は .000Z (#1948-10)")
	assert.Nil(t, byID["z_a2"]["expiresAt"], "nil expiresAt は null (#1948-10)")
	assert.Nil(t, byID["z_a2"]["usedAt"], "nil usedAt は null (#1948-10)")
}

// listErrRepo は ListByCreator だけ error を返す stub。
type listErrRepo struct {
	*testutil.MockRegistrationTicketRepository
}

func (r *listErrRepo) ListByCreator(_, _, _ string, _ int) ([]*model.RegistrationTicket, error) {
	return nil, errStub
}

func TestList_RepoErrorReturnsEmptyArray(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&listErrRepo{testutil.NewMockRegistrationTicketRepository()}, idGen)
	rec := post(h.List, `{}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code, "DB error 時も 200 + 空 array で graceful degrade する")
	assert.Equal(t, "[]", strings.TrimSpace(rec.Body.String()))
}

// --- Delete -----------------------------------------------------------------

func TestDelete_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	user := "u1"
	repo.Tickets["t1"] = &model.RegistrationTicket{ID: "t1", CreatedByID: &user}
	rec := post(h.Delete, `{"inviteId":"t1"}`, &model.User{ID: user})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotContains(t, repo.Tickets, "t1")
}

func TestDelete_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := post(h.Delete, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDelete_NotFoundReturnsNoContent(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := post(h.Delete, `{"inviteId":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code, "not found は idempotent に 204")
}

func TestDelete_AccessDeniedWhenNotCreator(t *testing.T) {
	h, repo := newTestHandler(t)
	owner := "u2"
	repo.Tickets["t1"] = &model.RegistrationTicket{ID: "t1", CreatedByID: &owner}
	rec := post(h.Delete, `{"inviteId":"t1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "ACCESS_DENIED")
}

// --- Delete: モデレーターの bypass と使用済みの保護 (#2812) ---

// stubModerator は指定した user だけをモデレーターとして返す。
type stubModerator struct{ ids map[string]bool }

func (s *stubModerator) IsModerator(userID string) bool { return s.ids[userID] }

func moderatorOf(ids ...string) *stubModerator {
	m := &stubModerator{ids: map[string]bool{}}
	for _, userID := range ids {
		m.ids[userID] = true
	}
	return m
}

// upstream の `ticket.createdById !== me.id && !isModerator` を再現する。
// **mk-go はこの bypass を持っておらず、管理画面の招待一覧から他人の招待を
// 消せなかった。** #2805 で承認由来の ticket が createdById NULL になったので、
// 消せない行の割合が増えていた。
func TestDelete_ModeratorBypass(t *testing.T) {
	used := time.Now()
	owner := "u2"
	mod := "mod1"
	tests := []struct {
		name       string
		ticket     *model.RegistrationTicket
		actor      string
		moderators *stubModerator
		wantCode   int
		wantBody   string
		wantGone   bool
	}{
		{
			name:       "モデレーターは他人の招待を消せる",
			ticket:     &model.RegistrationTicket{ID: "t1", CreatedByID: &owner},
			actor:      "mod1",
			moderators: moderatorOf("mod1"),
			wantCode:   http.StatusNoContent,
			wantGone:   true,
		},
		{
			// #2805 で承認由来の ticket は createdById NULL になった。
			name:       "モデレーターは createdById が NULL の招待を消せる",
			ticket:     &model.RegistrationTicket{ID: "t1"},
			actor:      "mod1",
			moderators: moderatorOf("mod1"),
			wantCode:   http.StatusNoContent,
			wantGone:   true,
		},
		{
			name:       "モデレーターは使用済みの招待も消せる",
			ticket:     &model.RegistrationTicket{ID: "t1", CreatedByID: &owner, UsedAt: &used},
			actor:      "mod1",
			moderators: moderatorOf("mod1"),
			wantCode:   http.StatusNoContent,
			wantGone:   true,
		},
		{
			name:       "モデレーターは自分の使用済み招待も消せる",
			ticket:     &model.RegistrationTicket{ID: "t1", CreatedByID: &mod, UsedAt: &used},
			actor:      "mod1",
			moderators: moderatorOf("mod1"),
			wantCode:   http.StatusNoContent,
			wantGone:   true,
		},
		{
			name:       "非モデレーターは他人の招待を消せない",
			ticket:     &model.RegistrationTicket{ID: "t1", CreatedByID: &owner},
			actor:      "u1",
			moderators: moderatorOf("mod1"),
			wantCode:   http.StatusBadRequest,
			wantBody:   "ACCESS_DENIED",
		},
		{
			name:       "非モデレーターは createdById が NULL の招待を消せない",
			ticket:     &model.RegistrationTicket{ID: "t1"},
			actor:      "u1",
			moderators: moderatorOf("mod1"),
			wantCode:   http.StatusBadRequest,
			wantBody:   "ACCESS_DENIED",
		},
		{
			// **消せると、誰の招待から入ったアカウントなのかを招待した本人が
			// 消せてしまう。**
			name:       "作成者でも使用済みの招待は消せない",
			ticket:     &model.RegistrationTicket{ID: "t1", CreatedByID: &owner, UsedAt: &used},
			actor:      owner,
			moderators: moderatorOf("mod1"),
			wantCode:   http.StatusBadRequest,
			wantBody:   "CAN_NOT_DELETE_INVITE_CODE",
		},
		{
			name:       "作成者は未使用の招待を消せる",
			ticket:     &model.RegistrationTicket{ID: "t1", CreatedByID: &owner},
			actor:      owner,
			moderators: moderatorOf("mod1"),
			wantCode:   http.StatusNoContent,
			wantGone:   true,
		},
		{
			// **第三者に「その招待は使用済み」と教えない。** 所有者判定を
			// 先にする upstream の順序をここで固定する。
			name:       "第三者には使用済みかどうかを教えない",
			ticket:     &model.RegistrationTicket{ID: "t1", CreatedByID: &owner, UsedAt: &used},
			actor:      "u1",
			moderators: moderatorOf("mod1"),
			wantCode:   http.StatusBadRequest,
			wantBody:   "ACCESS_DENIED",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, repo := newTestHandler(t)
			h.SetModeratorChecker(tt.moderators)
			repo.Tickets["t1"] = tt.ticket

			rec := post(h.Delete, `{"inviteId":"t1"}`, &model.User{ID: tt.actor})
			require.Equal(t, tt.wantCode, rec.Code, rec.Body.String())
			if tt.wantBody != "" {
				assert.Contains(t, rec.Body.String(), tt.wantBody)
			}
			if tt.wantGone {
				assert.NotContains(t, repo.Tickets, "t1", "消えていること")
			} else {
				assert.Contains(t, repo.Tickets, "t1", "消えていないこと")
			}
		})
	}
}

// checker 未配線なら bypass は効かない。**緩む側ではなく厳しい側**に倒れる。
//
// 作成者本人 + 使用済みで見る。他人の招待で見ると
// TestDelete_AccessDeniedWhenNotCreator と同じ経路になり、未配線かどうかを
// 区別できない (nil deref を守るだけのテストになる)。
func TestDelete_UnwiredModeratorCheckerDeniesBypass(t *testing.T) {
	h, repo := newTestHandler(t)
	owner := "u2"
	usedAt := time.Now()
	repo.Tickets["t1"] = &model.RegistrationTicket{ID: "t1", CreatedByID: &owner, UsedAt: &usedAt}

	rec := post(h.Delete, `{"inviteId":"t1"}`, &model.User{ID: owner})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "CAN_NOT_DELETE_INVITE_CODE")
	assert.Contains(t, repo.Tickets, "t1")
}

// failDeleteRepo は Delete だけ error を返す stub。
type failDeleteRepo struct {
	*testutil.MockRegistrationTicketRepository
}

func (r *failDeleteRepo) Delete(_ string) error { return errStub }

func TestDelete_RepoErrorReturns500(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	mock := testutil.NewMockRegistrationTicketRepository()
	user := "u1"
	mock.Tickets["t1"] = &model.RegistrationTicket{ID: "t1", CreatedByID: &user}
	h := NewHandler(&failDeleteRepo{mock}, idGen)
	rec := post(h.Delete, `{"inviteId":"t1"}`, &model.User{ID: user})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Limit ------------------------------------------------------------------

func TestLimit_UnlimitedWhenNoPolicy(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := post(h.Limit, `{}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Nil(t, out["remaining"], "provider 未配線は null = 無制限")
}

func TestLimit_UnlimitedWhenInviteLimitZero(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetRolePolicyProvider(&stubInvitePolicy{policies: map[string]any{"inviteLimit": 0}})
	rec := post(h.Limit, `{}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Nil(t, out["remaining"], "policy 0 は falsy で null = 無制限")
}

func TestLimit_ReturnsRemaining(t *testing.T) {
	h, repo := newTestHandler(t)
	user := "u1"
	// 既に 3 invite (= z_a* で時刻 prefix > sinceID になるよう)
	repo.Tickets["t1"] = &model.RegistrationTicket{ID: "z_a1", CreatedByID: &user}
	repo.Tickets["t2"] = &model.RegistrationTicket{ID: "z_a2", CreatedByID: &user}
	repo.Tickets["t3"] = &model.RegistrationTicket{ID: "z_a3", CreatedByID: &user}
	h.SetRolePolicyProvider(&stubInvitePolicy{policies: map[string]any{
		"inviteLimit":      10,
		"inviteLimitCycle": 60 * 24,
	}})
	rec := post(h.Limit, `{}`, &model.User{ID: user})
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	// json.Unmarshal は int を float64 にする
	assert.Equal(t, float64(7), out["remaining"], "10 - 3 = 7")
}

func TestLimit_MinInt64InviteCycleUsesSaturatingCutoff(t *testing.T) {
	cycleMinutes := float64(math.MinInt64) / float64(time.Minute)
	cycle, ok := role.PolicyMinutes(cycleMinutes)
	require.True(t, ok)
	require.Equal(t, time.Duration(math.MinInt64), cycle)

	repo := &recordingCountRepo{
		MockRegistrationTicketRepository: testutil.NewMockRegistrationTicketRepository(),
	}
	idGen := &recordingIDGenerator{}
	h := NewHandler(repo, idGen)
	h.SetRolePolicyProvider(&stubInvitePolicy{policies: map[string]any{
		"inviteLimit":      1,
		"inviteLimitCycle": cycleMinutes,
	}})

	before := time.Now()
	rec := post(h.Limit, `{}`, &model.User{ID: "u1"})
	after := time.Now()
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "u1", repo.creatorID)
	assert.Equal(t, "generated-0", repo.sinceID)
	require.Len(t, idGen.generated, 1)
	base := idGen.generated[0].Add(-time.Duration(math.MaxInt64))
	assert.False(t, base.Before(before))
	assert.False(t, base.After(after))
	var out struct {
		Remaining *int64 `json:"remaining"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.NotNil(t, out.Remaining)
	assert.Equal(t, int64(1), *out.Remaining)
}

func inviteRemaining(t *testing.T, limit float64, count int64) int64 {
	t.Helper()
	repo := &recordingCountRepo{
		MockRegistrationTicketRepository: testutil.NewMockRegistrationTicketRepository(),
		count:                            count,
	}
	idGen := &recordingIDGenerator{}
	h := NewHandler(repo, idGen)
	h.SetRolePolicyProvider(&stubInvitePolicy{policies: map[string]any{
		"inviteLimit": limit,
	}})
	rec := post(h.Limit, `{}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		Remaining *int64 `json:"remaining"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.NotNil(t, out.Remaining)
	return *out.Remaining
}

func TestLimit_InviteLimitSaturatesAtInt64Max(t *testing.T) {
	assert.Equal(t, int64(math.MaxInt64), inviteRemaining(t, float64(math.MaxInt64), 0))
}

func TestLimit_MaxLimitMinusMaxCountIsZero(t *testing.T) {
	assert.Equal(t, int64(0), inviteRemaining(t, float64(math.MaxInt64), math.MaxInt64))
}

func TestLimit_MinInt64CountUsesSaturatingNegation(t *testing.T) {
	assert.Equal(t, int64(math.MaxInt64), inviteRemaining(t, 1, math.MinInt64))
}

func TestLimit_RemainingClampedToZero(t *testing.T) {
	h, repo := newTestHandler(t)
	user := "u1"
	for i := 0; i < 5; i++ {
		repo.Tickets[string(rune('a'+i))] = &model.RegistrationTicket{
			ID:          "z_" + string(rune('a'+i)),
			CreatedByID: &user,
		}
	}
	h.SetRolePolicyProvider(&stubInvitePolicy{policies: map[string]any{
		"inviteLimit":      3,
		"inviteLimitCycle": 60 * 24,
	}})
	rec := post(h.Limit, `{}`, &model.User{ID: user})
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, float64(0), out["remaining"], "limit < count でも 0 で打ち切り")
}

func TestLimit_CountErrorReturns500(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&countErrRepo{testutil.NewMockRegistrationTicketRepository()}, idGen)
	h.SetRolePolicyProvider(&stubInvitePolicy{policies: map[string]any{"inviteLimit": 5}})
	rec := post(h.Limit, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// **小数の分が切り捨てられないこと。**
//
// `time.Duration(f) * time.Minute` と書くと Duration への変換で小数が
// 落ちてから掛かるので、0.5 分が 0 になり expiresAt が「今」になる。
// 掛けてから変換しないと、招待コードが発行直後に失効する (#2613)。
func TestCreate_ExpiresAtFractionalMinutes(t *testing.T) {
	h, repo := newTestHandler(t)
	h.SetRolePolicyProvider(&stubInvitePolicy{policies: map[string]any{
		"inviteExpirationTime": 0.5, // 30 秒
	}})
	before := time.Now()
	rec := post(h.Create, `{}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, repo.Tickets, 1)
	var saved *model.RegistrationTicket
	for _, tk := range repo.Tickets {
		saved = tk
	}
	require.NotNil(t, saved.ExpiresAt, "0.5 分でも expiresAt が設定される")
	// 切り捨てられていれば before とほぼ同時刻になる。30 秒後になっていること。
	assert.Greater(t, saved.ExpiresAt.Sub(before), 25*time.Second,
		"expiresAt が %v しか先でない。分が切り捨てられている", saved.ExpiresAt.Sub(before))
	assert.Less(t, saved.ExpiresAt.Sub(before), 35*time.Second)
}

// 小数の inviteLimit でもゲートが効くこと。
func TestCreate_InviteLimitFractional(t *testing.T) {
	h, repo := newTestHandler(t)
	h.SetRolePolicyProvider(&stubInvitePolicy{policies: map[string]any{
		"inviteLimit":      1.5,
		"inviteLimitCycle": 60 * 24,
	}})
	// 判定は「作成前の件数」に対して行う。0 >= 1.5 は false なので 1 件目は通る。
	require.Equal(t, http.StatusOK, post(h.Create, `{}`, &model.User{ID: "u1"}).Code)
	// 1 >= 1.5 も false なので 2 件目も通る。
	require.Equal(t, http.StatusOK, post(h.Create, `{}`, &model.User{ID: "u1"}).Code)
	require.Len(t, repo.Tickets, 2)
	// 2 >= 1.5 で 3 件目を弾く。int に丸めていると上限が 1 になって
	// 2 件目で弾かれ、素の `.(int)` だとゲートごと消えて 3 件目も通る。
	assert.NotEqual(t, http.StatusOK, post(h.Create, `{}`, &model.User{ID: "u1"}).Code,
		"上限 1.5 に対し 3 件目が通っている。ゲートが消えている")
}

// `used` は `usedAt` 由来 (#2812)。**`usedById` で見ると確認メール待ちの ticket が
// 未使用に見える** — `MarkPending` は `usedAt` だけ立てて `usedById` は nil のまま
// 残すので、画面は削除ボタンを出すのに `invite/delete` は使用済みとして 400 を返す。
// upstream は `used: !!target.usedAt`、mk-go の `admin/invite/list` も `UsedAt` 由来で、
// この endpoint だけが違っていた。
func TestList_UsedComesFromUsedAt(t *testing.T) {
	h, repo := newTestHandler(t)
	me := "u1"
	usedBy := "u2"
	usedAt := time.Now()
	// 確認メール待ち (MarkPending 済み): usedAt だけ立っている。
	repo.Tickets["t1"] = &model.RegistrationTicket{ID: "z_b1", Code: "c1", CreatedByID: &me, UsedAt: &usedAt}
	// 消費済み: 両方立っている。
	repo.Tickets["t2"] = &model.RegistrationTicket{ID: "z_b2", Code: "c2", CreatedByID: &me, UsedByID: &usedBy, UsedAt: &usedAt}
	// 未使用。
	repo.Tickets["t3"] = &model.RegistrationTicket{ID: "z_b3", Code: "c3", CreatedByID: &me}

	rec := post(h.List, `{"limit":50}`, &model.User{ID: me})
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 3)
	byID := map[string]map[string]any{}
	for _, e := range out {
		byID[e["id"].(string)] = e
	}
	assert.Equal(t, true, byID["z_b1"]["used"], "確認メール待ちも使用済みとして出す")
	assert.Equal(t, true, byID["z_b2"]["used"])
	assert.Equal(t, false, byID["z_b3"]["used"])
}

// findErrRepo は FindByID だけ not-found 以外の error を返す stub。
type findErrRepo struct {
	*testutil.MockRegistrationTicketRepository
}

func (r *findErrRepo) FindByID(_ string) (*model.RegistrationTicket, error) { return nil, errStub }

// **DB 障害を「削除成功」に化けさせない (#2812)。** 取り消し系で 204 を返すと、
// モデレーターは消えたと思って戻り、ticket は生きている。not-found は従来どおり
// idempotent に 204 (TestDelete_NotFoundReturnsNoContent)。
func TestDelete_LookupFailureIsNot204(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&findErrRepo{testutil.NewMockRegistrationTicketRepository()}, idGen)
	rec := post(h.Delete, `{"inviteId":"t1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code, "DB 障害を 204 に潰さない")
}
