package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Mock ---

type mockAuthSessionRepo struct {
	apps         map[string]*model.App
	accessTokens map[string]*model.AccessToken
	sessions     map[string]*model.AuthSession
	createErr    error
	listErr      error
}

func newMockRepo() *mockAuthSessionRepo {
	return &mockAuthSessionRepo{
		apps:         make(map[string]*model.App),
		accessTokens: make(map[string]*model.AccessToken),
		sessions:     make(map[string]*model.AuthSession),
	}
}

func (m *mockAuthSessionRepo) FindAppBySecret(secret string) (*model.App, error) {
	for _, a := range m.apps {
		if a.Secret == secret {
			return a, nil
		}
	}
	return nil, repository.ErrNotFound
}
func (m *mockAuthSessionRepo) CreateApp(app *model.App) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.apps[app.ID] = app
	return nil
}
func (m *mockAuthSessionRepo) CreateSession(*model.AuthSession) error { return nil }
func (m *mockAuthSessionRepo) FindSessionByToken(string) (*model.AuthSession, error) {
	return nil, assert.AnError
}
func (m *mockAuthSessionRepo) FindSessionByTokenAndAppID(string, string) (*model.AuthSession, error) {
	return nil, assert.AnError
}
func (m *mockAuthSessionRepo) UpdateSessionUserID(string, string) error { return nil }
func (m *mockAuthSessionRepo) DeleteSession(string) error               { return nil }
func (m *mockAuthSessionRepo) FindAccessTokenByAppAndUser(appID, userID string) (*model.AccessToken, error) {
	for _, t := range m.accessTokens {
		if t.AppID != nil && *t.AppID == appID && t.UserID == userID {
			return t, nil
		}
	}
	return nil, repository.ErrNotFound
}
func (m *mockAuthSessionRepo) CreateAccessToken(t *model.AccessToken) error {
	m.accessTokens[t.ID] = t
	return nil
}

func (m *mockAuthSessionRepo) FindAccessTokenBySession(session string) (*model.AccessToken, error) {
	for _, t := range m.accessTokens {
		if t.Session != nil && *t.Session == session {
			return t, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (m *mockAuthSessionRepo) MarkAccessTokenFetched(id string) (bool, error) {
	if t, ok := m.accessTokens[id]; ok && !t.Fetched {
		t.Fetched = true
		return true, nil
	}
	return false, nil
}

func (m *mockAuthSessionRepo) FindAppByID(id string) (*model.App, error) {
	a, ok := m.apps[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return a, nil
}

func (m *mockAuthSessionRepo) ListAppsByUserID(userID string, limit, offset int) ([]*model.App, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	var apps []*model.App
	for _, a := range m.apps {
		if a.UserID != nil && *a.UserID == userID {
			apps = append(apps, a)
		}
	}
	if offset >= len(apps) {
		return []*model.App{}, nil
	}
	apps = apps[offset:]
	if len(apps) > limit {
		apps = apps[:limit]
	}
	return apps, nil
}

func newTestHandler() (*Handler, *mockAuthSessionRepo) {
	idGen, _ := id.NewGenerator("aidx")
	repo := newMockRepo()
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

// postWithToken is post but also wires the raw auth token onto the context so
// handlers that distinguish native session vs app access token (app/show
// secret gate, #1829) can be exercised.
func postWithToken(handler func(echo.Context) error, body string, user *model.User, token string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	c.Set(string(middleware.TokenContextKey), token)
	_ = handler(c)
	return rec
}

// --- Create ---

func TestCreate_Success(t *testing.T) {
	h, repo := newTestHandler()

	rec := post(h.Create, `{"name":"MyApp","description":"desc","permission":["read:account"]}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "MyApp", resp["name"])
	assert.NotEmpty(t, resp["secret"])
	assert.NotEmpty(t, resp["id"])
	assert.Len(t, repo.apps, 1)
	shapetest.Assert(t, "App", resp) // L3 (#1270)
}

func TestCreate_WithUser(t *testing.T) {
	h, repo := newTestHandler()
	user := &model.User{ID: "u1"}

	rec := post(h.Create, `{"name":"App","description":"d","permission":["account/read","read:account"]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)

	for _, a := range repo.apps {
		require.NotNil(t, a.UserID)
		assert.Equal(t, "u1", *a.UserID)
		// #1557 permission は正規化 + de-dup される (account/read と read:account は同一)。
		assert.Equal(t, []string{"read:account"}, []string(a.Permission))
	}

	// #1557 app/create は upstream が pack(app, null) するため、authenticated
	// でも isAuthorized を含めない。
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	_, hasAuth := resp["isAuthorized"]
	assert.False(t, hasAuth, "app/create は isAuthorized を省略する")
}

func TestCreate_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()

	rec := post(h.Create, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	rec = post(h.Create, `{"name":"x"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_DBError(t *testing.T) {
	h, repo := newTestHandler()
	repo.createErr = assert.AnError

	rec := post(h.Create, `{"name":"App","description":"d","permission":["read:account"]}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Show ---

func TestShow_Success(t *testing.T) {
	h, repo := newTestHandler()
	repo.apps["app1"] = &model.App{ID: "app1", Name: "MyApp", Secret: "s1", Permission: model.StringArray{"read:account"}}

	rec := post(h.Show, `{"appId":"app1"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "MyApp", resp["name"])
	// 認証なし→secret非公開
	_, hasSecret := resp["secret"]
	assert.False(t, hasSecret)
	// 認証なし (me==null) → isAuthorized は省略される (#1557)。
	_, hasAuth := resp["isAuthorized"]
	assert.False(t, hasAuth)
}

// #1557 app/show: 認証 viewer が access_token を持つ → isAuthorized true、
// 持たない → false。me!=null では field を含める。
func TestShow_IsAuthorized(t *testing.T) {
	h, repo := newTestHandler()
	repo.apps["app1"] = &model.App{ID: "app1", Name: "MyApp", Permission: model.StringArray{"read:account"}}

	// token 無し viewer → isAuthorized false (present)
	rec := post(h.Show, `{"appId":"app1"}`, &model.User{ID: "viewer1"})
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["isAuthorized"])

	// token あり viewer → isAuthorized true
	appID := "app1"
	repo.accessTokens["t1"] = &model.AccessToken{ID: "t1", AppID: &appID, UserID: "viewer2"}
	rec = post(h.Show, `{"appId":"app1"}`, &model.User{ID: "viewer2"})
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["isAuthorized"])
}

// TestShow_WithOwner: owner が native session (raw token == users.token) で
// 叩くと secret を返す (upstream isSecure = token==null)。
func TestShow_WithOwner(t *testing.T) {
	h, repo := newTestHandler()
	uid := "u1"
	repo.apps["app1"] = &model.App{ID: "app1", Name: "MyApp", Secret: "s1", UserID: &uid, Permission: model.StringArray{"read:account"}}

	nativeTok := "native-session-token"
	rec := postWithToken(h.Show, `{"appId":"app1"}`, &model.User{ID: "u1", Token: &nativeTok}, nativeTok)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "s1", resp["secret"])
}

// TestShow_OwnerViaAppTokenNoSecret: owner であっても app access token
// (raw token != users.token) で叩くと secret を返さない。secret を app token に
// 渡す privilege escalation の防止 (#1829, upstream isSecure = token==null)。
func TestShow_OwnerViaAppTokenNoSecret(t *testing.T) {
	h, repo := newTestHandler()
	uid := "u1"
	repo.apps["app1"] = &model.App{ID: "app1", Name: "MyApp", Secret: "s1", UserID: &uid, Permission: model.StringArray{"read:account"}}

	nativeTok := "native-session-token"
	// raw token は app access token (native token とは別) を渡す。
	rec := postWithToken(h.Show, `{"appId":"app1"}`, &model.User{ID: "u1", Token: &nativeTok}, "app-access-token")
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	_, hasSecret := resp["secret"]
	assert.False(t, hasSecret, "app access token (owner) には secret を返さない")
	// name 等の public field は返る。
	assert.Equal(t, "MyApp", resp["name"])
}

func TestShow_NotFound(t *testing.T) {
	h, _ := newTestHandler()

	rec := post(h.Show, `{"appId":"ghost"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()

	rec := post(h.Show, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- MyApps ---

func TestMyApps_Success(t *testing.T) {
	h, repo := newTestHandler()
	uid := "u1"
	repo.apps["a1"] = &model.App{ID: "a1", Name: "App1", Secret: "s1", UserID: &uid, Permission: model.StringArray{"read:account"}}
	repo.apps["a2"] = &model.App{ID: "a2", Name: "App2", Secret: "s2", UserID: &uid, Permission: model.StringArray{"read:account"}}

	rec := post(h.MyApps, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 2)
	// my/apps は secret を返さない (upstream parity、#1900)。secret 取得は
	// app/show (secure session) 経由のみ。public field (name 等) は返る。
	for _, a := range resp {
		_, hasSecret := a["secret"]
		assert.False(t, hasSecret, "my/apps は secret を含めない")
		assert.NotEmpty(t, a["name"])
	}
}

func TestMyApps_Empty(t *testing.T) {
	h, _ := newTestHandler()

	rec := post(h.MyApps, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 0)
}

func TestMyApps_WithLimit(t *testing.T) {
	h, repo := newTestHandler()
	uid := "u1"
	for i := 0; i < 5; i++ {
		appID := "a" + string(rune('0'+i))
		repo.apps[appID] = &model.App{ID: appID, Name: "App", Secret: "s", UserID: &uid, Permission: model.StringArray{"read:account"}}
	}

	rec := post(h.MyApps, `{"limit":2}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.LessOrEqual(t, len(resp), 2)
}

func TestMyApps_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	// 不正なJSON
	rec := post(h.MyApps, `{invalid`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMyApps_LimitClamp(t *testing.T) {
	h, _ := newTestHandler()

	// limit < 1 → 1にクランプ
	rec := post(h.MyApps, `{"limit":0}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)

	// limit > 100 → 100にクランプ
	rec = post(h.MyApps, `{"limit":999}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestMyApps_WithOffset(t *testing.T) {
	h, repo := newTestHandler()
	uid := "u1"
	for i := range 5 {
		appID := "a" + string(rune('0'+i))
		repo.apps[appID] = &model.App{ID: appID, Name: "App", Secret: "s", UserID: &uid, Permission: model.StringArray{"read:account"}}
	}

	rec := post(h.MyApps, `{"offset":3}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.LessOrEqual(t, len(resp), 2)
}

func TestMyApps_DBError(t *testing.T) {
	h, repo := newTestHandler()
	repo.listErr = assert.AnError
	rec := post(h.MyApps, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestSecureRandomHex(t *testing.T) {
	s := misc.SecureRandomHex(32)
	assert.Len(t, s, 32)
	s2 := misc.SecureRandomHex(32)
	assert.NotEqual(t, s, s2)
}

func TestPackApp_NoSecret(t *testing.T) {
	a := &model.App{ID: "a1", Name: "App", Permission: model.StringArray{"read:account"}}
	result := packApp(a, false, nil)
	_, has := result["secret"]
	assert.False(t, has)
	// isAuthorized は nil 渡し (me==null 相当) で省略される (#1557)。
	_, hasAuth := result["isAuthorized"]
	assert.False(t, hasAuth)
}

func TestPackApp_WithSecret(t *testing.T) {
	a := &model.App{ID: "a1", Name: "App", Secret: "s123", Permission: model.StringArray{"read:account"}}
	result := packApp(a, true, nil)
	assert.Equal(t, "s123", result["secret"])
}

// #1557 isAuthorized は non-nil 渡しで値が出る。
func TestPackApp_IsAuthorized(t *testing.T) {
	a := &model.App{ID: "a1", Name: "App", Permission: model.StringArray{"read:account"}}
	yes := true
	assert.Equal(t, true, packApp(a, false, &yes)["isAuthorized"])
	no := false
	assert.Equal(t, false, packApp(a, false, &no)["isAuthorized"])
}

// #1557 legacy permission の正規化 + de-dup。
func TestNormalizeAppPermissions(t *testing.T) {
	got := normalizeAppPermissions([]string{"account/read", "notes-write", "read:account", "read:drive", "read:drive"})
	// account/read -> read:account (既存と重複し dedup)、notes-write -> write:notes
	assert.Equal(t, []string{"read:account", "write:notes", "read:drive"}, []string(got))
}
