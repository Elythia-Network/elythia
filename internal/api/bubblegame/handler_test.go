package bubblegame

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errMock = assert.AnError

type mockBubbleGameRepo struct {
	records   []*model.BubbleGameRecord
	createErr error
}

func (m *mockBubbleGameRepo) Create(r *model.BubbleGameRecord) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.records = append(m.records, r)
	return nil
}

func (m *mockBubbleGameRepo) Ranking(_ string, _ int) ([]*model.BubbleGameRecord, error) {
	return m.records, nil
}

func newTestHandler() (*Handler, *mockBubbleGameRepo) {
	repo := &mockBubbleGameRepo{}
	idGen, _ := id.NewGenerator("aidx")
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

var u1 = &model.User{ID: "u1", Username: "alice"}

func validSeed() string {
	return fmt.Sprintf("%d", time.Now().UnixMilli())
}

// --- Register ---

func TestRegister_Success(t *testing.T) {
	h, repo := newTestHandler()
	body := fmt.Sprintf(`{"score":100,"seed":"%s","logs":[[1,2]],"gameMode":"normal","gameVersion":1}`, validSeed())
	rec := post(h.Register, body, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, repo.records, 1)
}

func TestRegister_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Register, `{}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRegister_InvalidSeed_NotNumber(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Register, `{"score":1,"seed":"abc","logs":[],"gameMode":"normal","gameVersion":1}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRegister_InvalidSeed_Future(t *testing.T) {
	h, _ := newTestHandler()
	future := fmt.Sprintf("%d", time.Now().Add(time.Hour).UnixMilli())
	body := fmt.Sprintf(`{"score":1,"seed":"%s","logs":[],"gameMode":"normal","gameVersion":1}`, future)
	rec := post(h.Register, body, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRegister_InvalidSeed_TooOld(t *testing.T) {
	h, _ := newTestHandler()
	old := fmt.Sprintf("%d", time.Now().Add(-seedMaxAge-time.Hour).UnixMilli())
	body := fmt.Sprintf(`{"score":1,"seed":"%s","logs":[],"gameMode":"normal","gameVersion":1}`, old)
	rec := post(h.Register, body, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #3192: 途中から再開したゲームは元のシードを使い続けるので、upstream の 5 時間を
// 超えても登録できる。**翌日に再開したゲームのスコアが黙って落ちない**ことを見る。
func TestRegister_ResumedSeedOlderThanUpstreamLimit(t *testing.T) {
	h, repo := newTestHandler()
	resumed := fmt.Sprintf("%d", time.Now().Add(-30*time.Hour).UnixMilli())
	body := fmt.Sprintf(`{"score":1,"seed":"%s","logs":[],"gameMode":"normal","gameVersion":4}`, resumed)
	rec := post(h.Register, body, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, repo.records, 1)
}

func TestSeedMaxAgeIsSevenDays(t *testing.T) {
	assert.Equal(t, 7*24*time.Hour, seedMaxAge)
}

func TestRegister_CreateError(t *testing.T) {
	h, repo := newTestHandler()
	repo.createErr = errMock
	body := fmt.Sprintf(`{"score":1,"seed":"%s","logs":[],"gameMode":"normal","gameVersion":1}`, validSeed())
	rec := post(h.Register, body, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Ranking ---

func TestRanking_Success(t *testing.T) {
	h, repo := newTestHandler()
	repo.records = []*model.BubbleGameRecord{
		{ID: "r1", Score: 200, User: &model.User{ID: "u1", Username: "alice"}},
		{ID: "r2", Score: 100, User: &model.User{ID: "u2", Username: "bob"}},
	}
	rec := post(h.Ranking, `{"gameMode":"normal"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 2)
}

// TestRanking_UserIsUserLite guards that ranking items embed a full UserLite
// (#1553). Upstream ranking.ts declares user as ref:'UserLite', so ad-hoc
// 4-field maps break clients that expect avatarUrl / onlineStatus etc.
func TestRanking_UserIsUserLite(t *testing.T) {
	h, repo := newTestHandler()
	repo.records = []*model.BubbleGameRecord{
		{ID: "r1", Score: 200, User: &model.User{ID: "u1", Username: "alice"}},
	}
	rec := post(h.Ranking, `{"gameMode":"normal"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	user, ok := resp[0]["user"].(map[string]any)
	require.True(t, ok, "user must be an object")
	// UserLite 必須フィールドが揃っていること (ad-hoc map では欠けていた組)。
	for _, key := range []string{"id", "username", "name", "host", "avatarUrl", "isBot", "isCat", "emojis", "onlineStatus", "badgeRoles"} {
		_, has := user[key]
		assert.True(t, has, "UserLite field %q must be present", key)
	}
	assert.Equal(t, "u1", user["id"])
}

func TestRanking_Empty(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Ranking, `{"gameMode":"normal"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// getRanking issues a GET request with gameMode in the query string, mirroring
// the frontend's misskeyApiGet call (#1774)。
func getRanking(handler func(echo.Context) error, query string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?"+query, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = handler(c)
	return rec
}

// #1774: ranking は allowGet。frontend は GET + ?gameMode= で叩くため、query から
// gameMode を読めること (JSON body 無しでも 400 にならないこと) を保証する。
func TestRanking_GetWithQueryParam(t *testing.T) {
	h, repo := newTestHandler()
	repo.records = []*model.BubbleGameRecord{
		{ID: "r1", Score: 200, User: &model.User{ID: "u1", Username: "alice"}},
	}
	rec := getRanking(h.Ranking, "gameMode=normal")
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
	// #2027: 未認証 GET には cacheSec:60 由来の Cache-Control が付く。
	assert.Equal(t, "public, max-age=60", rec.Header().Get("Cache-Control"))
}

// #2027: POST (= 非 GET) には Cache-Control を付けない。
func TestRanking_PostNoCacheControl(t *testing.T) {
	h, repo := newTestHandler()
	repo.records = []*model.BubbleGameRecord{{ID: "r1", Score: 200}}
	rec := post(h.Ranking, `{"gameMode":"normal"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Cache-Control"), "POST には Cache-Control を付けない (#2027)")
}

// #2049: raw token を付けた GET (= 無効/suspended token で anonymous に落ちた場合) は
// HasRawToken=true なので Cache-Control を付けない (upstream `!token` 判定)。
func TestRanking_GetWithRawTokenNoCacheControl(t *testing.T) {
	h, repo := newTestHandler()
	repo.records = []*model.BubbleGameRecord{{ID: "r1", Score: 200}}
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?gameMode=normal", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	// auth middleware が raw token 検出時に積む flag (#2049) を simulate する。
	c.Set("misskeyRawTokenPresent", true)
	_ = h.Ranking(c)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Cache-Control"), "raw token GET には Cache-Control を付けない (#2049)")
}

// GET で gameMode が無ければ POST 同様 400。
func TestRanking_GetMissingGameMode(t *testing.T) {
	h, _ := newTestHandler()
	rec := getRanking(h.Ranking, "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRanking_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Ranking, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

type failRankingRepo struct{ *mockBubbleGameRepo }

func (f *failRankingRepo) Ranking(_ string, _ int) ([]*model.BubbleGameRecord, error) {
	return nil, errMock
}

func TestRanking_Error(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&failRankingRepo{&mockBubbleGameRepo{}}, idGen)
	rec := post(h.Ranking, `{"gameMode":"normal"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

func TestRanking_NilUser(t *testing.T) {
	h, repo := newTestHandler()
	repo.records = []*model.BubbleGameRecord{
		{ID: "r1", Score: 100, User: nil},
	}
	rec := post(h.Ranking, `{"gameMode":"normal"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	_, hasUser := resp[0]["user"]
	assert.False(t, hasUser)
}

// #2027: score<0 は upstream の minimum:0 に従い 400 (seed parse より前に弾く)。
func TestRegister_NegativeScoreRejected(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Register, `{"score":-1,"seed":"x","gameMode":"normal","gameVersion":1}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
}
