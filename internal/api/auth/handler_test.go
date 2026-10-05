package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/misc"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Mock Repository ---

type mockAuthSessionRepo struct {
	apps         map[string]*model.App         // secret -> app
	sessions     map[string]*model.AuthSession // token -> session
	accessTokens map[string]*model.AccessToken // appID:userID -> token
	createErr    error
}

func newMockRepo() *mockAuthSessionRepo {
	return &mockAuthSessionRepo{
		apps:         make(map[string]*model.App),
		sessions:     make(map[string]*model.AuthSession),
		accessTokens: make(map[string]*model.AccessToken),
	}
}

func (m *mockAuthSessionRepo) FindAppBySecret(secret string) (*model.App, error) {
	if app, ok := m.apps[secret]; ok {
		return app, nil
	}
	return nil, errNotFound
}

func (m *mockAuthSessionRepo) CreateApp(app *model.App) error {
	m.apps[app.Secret] = app
	return nil
}

func (m *mockAuthSessionRepo) CreateSession(session *model.AuthSession) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.sessions[session.Token] = session
	return nil
}

func (m *mockAuthSessionRepo) FindSessionByToken(token string) (*model.AuthSession, error) {
	if s, ok := m.sessions[token]; ok {
		// Appリレーションを模擬
		if s.App == nil {
			if app := m.findAppByID(s.AppID); app != nil {
				s.App = app
			}
		}
		return s, nil
	}
	return nil, errNotFound
}

func (m *mockAuthSessionRepo) FindSessionByTokenAndAppID(token, appID string) (*model.AuthSession, error) {
	if s, ok := m.sessions[token]; ok && s.AppID == appID {
		if s.App == nil {
			if app := m.findAppByID(s.AppID); app != nil {
				s.App = app
			}
		}
		// 既に User が pre-set されていれば尊重する (テストが任意の user を
		// 注入できるようにするため)。未設定のときだけ既定 user を合成する。
		if s.UserID != nil && s.User == nil {
			s.User = &model.User{ID: *s.UserID, Username: "testuser"}
		}
		return s, nil
	}
	return nil, errNotFound
}

func (m *mockAuthSessionRepo) UpdateSessionUserID(sessionID, userID string) error {
	for _, s := range m.sessions {
		if s.ID == sessionID {
			s.UserID = &userID
			return nil
		}
	}
	return errNotFound
}

func (m *mockAuthSessionRepo) DeleteSession(sessionID string) error {
	for token, s := range m.sessions {
		if s.ID == sessionID {
			delete(m.sessions, token)
			return nil
		}
	}
	return errNotFound
}

func (m *mockAuthSessionRepo) FindAccessTokenByAppAndUser(appID, userID string) (*model.AccessToken, error) {
	key := appID + ":" + userID
	if t, ok := m.accessTokens[key]; ok {
		return t, nil
	}
	return nil, errNotFound
}

func (m *mockAuthSessionRepo) CreateAccessToken(token *model.AccessToken) error {
	if m.createErr != nil {
		return m.createErr
	}
	key := ""
	if token.AppID != nil {
		key = *token.AppID + ":" + token.UserID
	}
	m.accessTokens[key] = token
	return nil
}

func (m *mockAuthSessionRepo) FindAccessTokenBySession(session string) (*model.AccessToken, error) {
	for _, t := range m.accessTokens {
		if t.Session != nil && *t.Session == session {
			return t, nil
		}
	}
	return nil, errNotFound
}

func (m *mockAuthSessionRepo) MarkAccessTokenFetched(id string) (bool, error) {
	for _, t := range m.accessTokens {
		if t.ID == id && !t.Fetched {
			t.Fetched = true
			return true, nil
		}
	}
	return false, nil
}

func (m *mockAuthSessionRepo) FindAppByID(appID string) (*model.App, error) {
	if app := m.findAppByID(appID); app != nil {
		return app, nil
	}
	return nil, errNotFound
}

func (m *mockAuthSessionRepo) ListAppsByUserID(userID string, limit, offset int) ([]*model.App, error) {
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

func (m *mockAuthSessionRepo) findAppByID(appID string) *model.App {
	for _, app := range m.apps {
		if app.ID == appID {
			return app
		}
	}
	return nil
}

// **not-found を模す。** 汎用 error だと #2792 の「DB 障害は 500」に引っかかる。
// repository は GORM の error をそのまま返すので、テストもそれに揃える。
var errNotFound = repository.ErrNotFound

// errBoom is a generic failure for write paths. **not-found とは分けること** —
// 同じ値を使い回すと、将来 write path に `IsNotFound` の分岐が入ったときに
// これらのテストが黙って別の枝を通る (#2792)。
var errBoom = errors.New("db down")

func newTestHandler() (*Handler, *mockAuthSessionRepo) {
	repo := newMockRepo()
	cfg := &config.Config{URL: "http://localhost:3000"}
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(repo, cfg, idGen)
	return h, repo
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

// --- SessionGenerate ---

func TestSessionGenerate_Success(t *testing.T) {
	h, repo := newTestHandler()
	repo.apps["secret123"] = &model.App{ID: "app1", Secret: "secret123", Name: "TestApp", Permission: model.StringArray{"read:account"}}

	rec := post(h.SessionGenerate, `{"appSecret":"secret123"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["token"])
	assert.Contains(t, resp["url"], "http://localhost:3000/auth/")
}

func TestSessionGenerate_NoSuchApp(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.SessionGenerate, `{"appSecret":"ghost"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSessionGenerate_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.SessionGenerate, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSessionGenerate_CreateError(t *testing.T) {
	h, repo := newTestHandler()
	repo.apps["s1"] = &model.App{ID: "a1", Secret: "s1"}
	repo.createErr = errBoom
	rec := post(h.SessionGenerate, `{"appSecret":"s1"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- SessionShow ---

func TestSessionShow_Success(t *testing.T) {
	h, repo := newTestHandler()
	repo.apps["s1"] = &model.App{ID: "a1", Secret: "s1", Name: "App", Permission: model.StringArray{"read:account"}}
	repo.sessions["tok1"] = &model.AuthSession{ID: "sess1", Token: "tok1", AppID: "a1"}

	rec := post(h.SessionShow, `{"token":"tok1"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "sess1", resp["id"])
	assert.NotNil(t, resp["app"])
}

func TestSessionShow_NotFound(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.SessionShow, `{"token":"ghost"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSessionShow_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.SessionShow, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Accept ---

func TestAccept_Success(t *testing.T) {
	h, repo := newTestHandler()
	repo.apps["s1"] = &model.App{ID: "a1", Secret: "s1", Permission: model.StringArray{"read:account"}}
	repo.sessions["tok1"] = &model.AuthSession{ID: "sess1", Token: "tok1", AppID: "a1"}
	user := &model.User{ID: "u1"}

	rec := post(h.Accept, `{"token":"tok1"}`, user)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	// アクセストークンが作成され、セッションにユーザーIDが設定されたか確認
	assert.Len(t, repo.accessTokens, 1)
	assert.NotNil(t, repo.sessions["tok1"].UserID)
}

func TestAccept_ExistingToken(t *testing.T) {
	h, repo := newTestHandler()
	repo.apps["s1"] = &model.App{ID: "a1", Secret: "s1", Permission: model.StringArray{}}
	repo.sessions["tok1"] = &model.AuthSession{ID: "sess1", Token: "tok1", AppID: "a1"}
	appID := "a1"
	repo.accessTokens["a1:u1"] = &model.AccessToken{ID: "at1", AppID: &appID, UserID: "u1", Token: "existing"}

	rec := post(h.Accept, `{"token":"tok1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	// 既存トークンがあるので新しく作られない
	assert.Len(t, repo.accessTokens, 1)
}

func TestAccept_SessionNotFound(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Accept, `{"token":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAccept_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Accept, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

type failUpdateRepo struct {
	*mockAuthSessionRepo
}

func (f *failUpdateRepo) UpdateSessionUserID(_, _ string) error { return errBoom }

func TestAccept_UpdateSessionError(t *testing.T) {
	base := newMockRepo()
	base.apps["s1"] = &model.App{ID: "a1", Secret: "s1", Permission: model.StringArray{}}
	appID := "a1"
	base.accessTokens["a1:u1"] = &model.AccessToken{ID: "at1", AppID: &appID, UserID: "u1"}
	base.sessions["tok1"] = &model.AuthSession{ID: "sess1", Token: "tok1", AppID: "a1"}

	cfg := &config.Config{URL: "http://localhost:3000"}
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&failUpdateRepo{base}, cfg, idGen)

	rec := post(h.Accept, `{"token":"tok1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestAccept_CreateTokenError(t *testing.T) {
	h, repo := newTestHandler()
	repo.apps["s1"] = &model.App{ID: "a1", Secret: "s1", Permission: model.StringArray{}}
	repo.sessions["tok1"] = &model.AuthSession{ID: "sess1", Token: "tok1", AppID: "a1"}
	repo.createErr = errBoom

	rec := post(h.Accept, `{"token":"tok1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- SessionUserkey ---

func TestSessionUserkey_Success(t *testing.T) {
	h, repo := newTestHandler()
	repo.apps["s1"] = &model.App{ID: "a1", Secret: "s1"}
	userID := "u1"
	desc := "hello"
	user := &model.User{ID: "u1", Username: "alice", Name: nil}
	repo.sessions["tok1"] = &model.AuthSession{ID: "sess1", Token: "tok1", AppID: "a1", UserID: &userID, User: user}
	appID := "a1"
	repo.accessTokens["a1:u1"] = &model.AccessToken{ID: "at1", Token: "mytoken", AppID: &appID, UserID: "u1"}

	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u1"] = user
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Description: &desc}
	h.SetUserRepo(userRepo)

	rec := post(h.SessionUserkey, `{"appSecret":"s1","token":"tok1"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "mytoken", resp["accessToken"])
	// user must be packed as UserDetailedNotMe, not the old 4-field stub:
	// UserDetailed-only fields (createdAt / isLocked / description /
	// publicReactions) prove the full schema is used (#1557).
	userObj, ok := resp["user"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "u1", userObj["id"])
	assert.Equal(t, "alice", userObj["username"])
	assert.Contains(t, userObj, "createdAt")
	assert.Contains(t, userObj, "isLocked")
	assert.Contains(t, userObj, "publicReactions")
	assert.Equal(t, "hello", userObj["description"])
	// セッション削除されたか
	assert.Empty(t, repo.sessions)
}

// TestSessionUserkey_NoUserRepo verifies the user is still packed with the
// UserDetailed schema (sans profile fields) when userRepo is not wired,
// degrading gracefully rather than reverting to the 4-field stub.
func TestSessionUserkey_NoUserRepo(t *testing.T) {
	h, repo := newTestHandler()
	repo.apps["s1"] = &model.App{ID: "a1", Secret: "s1"}
	userID := "u1"
	repo.sessions["tok1"] = &model.AuthSession{ID: "sess1", Token: "tok1", AppID: "a1", UserID: &userID, User: &model.User{ID: "u1", Username: "alice"}}
	appID := "a1"
	repo.accessTokens["a1:u1"] = &model.AccessToken{ID: "at1", Token: "mytoken", AppID: &appID, UserID: "u1"}

	rec := post(h.SessionUserkey, `{"appSecret":"s1","token":"tok1"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	userObj, ok := resp["user"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "u1", userObj["id"])
	assert.Contains(t, userObj, "isLocked")
}

func TestSessionUserkey_NoSuchApp(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.SessionUserkey, `{"appSecret":"ghost","token":"tok1"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSessionUserkey_NoSuchSession(t *testing.T) {
	h, repo := newTestHandler()
	repo.apps["s1"] = &model.App{ID: "a1", Secret: "s1"}
	rec := post(h.SessionUserkey, `{"appSecret":"s1","token":"ghost"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSessionUserkey_PendingSession(t *testing.T) {
	h, repo := newTestHandler()
	repo.apps["s1"] = &model.App{ID: "a1", Secret: "s1"}
	repo.sessions["tok1"] = &model.AuthSession{ID: "sess1", Token: "tok1", AppID: "a1", UserID: nil}

	rec := post(h.SessionUserkey, `{"appSecret":"s1","token":"tok1"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSessionUserkey_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.SessionUserkey, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSessionUserkey_TokenNotFound(t *testing.T) {
	h, repo := newTestHandler()
	repo.apps["s1"] = &model.App{ID: "a1", Secret: "s1"}
	userID := "u1"
	repo.sessions["tok1"] = &model.AuthSession{ID: "sess1", Token: "tok1", AppID: "a1", UserID: &userID}
	// accessTokensにエントリなし

	rec := post(h.SessionUserkey, `{"appSecret":"s1","token":"tok1"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Helper functions ---

func TestSecureRandomHex(t *testing.T) {
	s := misc.SecureRandomHex(32)
	assert.Len(t, s, 32)
	// 2回呼ぶと異なる値
	s2 := misc.SecureRandomHex(32)
	assert.NotEqual(t, s, s2)
}

func TestSha256Hex(t *testing.T) {
	h := sha256Hex("hello")
	assert.Len(t, h, 64)
	// 同じ入力 → 同じ出力
	assert.Equal(t, h, sha256Hex("hello"))
}

func TestPackSession_NilApp(t *testing.T) {
	s := &model.AuthSession{ID: "s1", Token: "t1"}
	result := packSession(s, nil)
	assert.Equal(t, "s1", result["id"])
	_, hasApp := result["app"]
	assert.False(t, hasApp)
}

// #1557 app.isAuthorized は non-nil 渡しのときだけ含める。
func TestPackSession_AppIsAuthorized(t *testing.T) {
	s := &model.AuthSession{ID: "s1", Token: "t1", App: &model.App{ID: "a1", Name: "App"}}

	// isAuthorized nil (me==null) → 省略
	app := packSession(s, nil)["app"].(map[string]any)
	_, has := app["isAuthorized"]
	assert.False(t, has)

	// isAuthorized non-nil → 値が出る
	yes := true
	app2 := packSession(s, &yes)["app"].(map[string]any)
	assert.Equal(t, true, app2["isAuthorized"])
}

func TestPackUserDetailed_NilUser(t *testing.T) {
	h, _ := newTestHandler()
	result := h.packUserDetailed(nil, "u1")
	m, ok := result.(map[string]any)
	require.True(t, ok)
	assert.Empty(t, m)
}

// --- GenToken ---

func TestGenToken_Success(t *testing.T) {
	h, repo := newTestHandler()
	user := &model.User{ID: "u1", Username: "testuser"}

	rec := post(h.GenToken, `{"permission":["read:account","write:notes"]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["token"])

	// DB上にアクセストークンが作成されている
	assert.Len(t, repo.accessTokens, 1)
	for _, at := range repo.accessTokens {
		assert.Equal(t, "u1", at.UserID)
		assert.Nil(t, at.AppID)
		assert.Equal(t, resp["token"], at.Token)
		// SHA-256ハッシュ化の検証: HashはTokenと異なり、sha256Hexの結果と一致する
		assert.NotEqual(t, at.Token, at.Hash, "Hash should differ from raw Token")
		assert.Equal(t, sha256Hex(at.Token), at.Hash, "Hash should be SHA-256 of Token")
	}
}

// chanTokenNotifier captures OnCreateToken asynchronously (#1559)。
type chanTokenNotifier struct{ ch chan string }

func (n *chanTokenNotifier) OnCreateToken(userID string) { n.ch <- userID }

// #1559 [LOW] gen-token 成功時に createToken 通知が発火する。
func TestGenToken_FiresTokenNotifier(t *testing.T) {
	h, _ := newTestHandler()
	user := &model.User{ID: "u1", Username: "testuser"}
	notifier := &chanTokenNotifier{ch: make(chan string, 1)}
	h.SetTokenNotifier(notifier)

	rec := post(h.GenToken, `{"permission":["read:account"]}`, user)
	require.Equal(t, http.StatusOK, rec.Code)

	select {
	case uid := <-notifier.ch:
		assert.Equal(t, "u1", uid)
	case <-time.After(2 * time.Second):
		t.Fatal("token notifier was not fired")
	}
}

func TestGenToken_WithOptionalFields(t *testing.T) {
	h, repo := newTestHandler()
	user := &model.User{ID: "u1", Username: "testuser"}

	rec := post(h.GenToken, `{"permission":["read:account"],"name":"MyApp","description":"desc","iconUrl":"https://example.com/icon.png","session":"sess1"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)

	assert.Len(t, repo.accessTokens, 1)
	for _, at := range repo.accessTokens {
		assert.NotNil(t, at.Name)
		assert.Equal(t, "MyApp", *at.Name)
		assert.NotNil(t, at.Description)
		assert.Equal(t, "desc", *at.Description)
		assert.NotNil(t, at.IconURL)
		assert.Equal(t, "https://example.com/icon.png", *at.IconURL)
		assert.NotNil(t, at.Session)
		assert.Equal(t, "sess1", *at.Session)
	}
}

func TestGenToken_MissingPermission(t *testing.T) {
	h, _ := newTestHandler()
	user := &model.User{ID: "u1", Username: "testuser"}

	rec := post(h.GenToken, `{}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestGenToken_CreateError(t *testing.T) {
	h, repo := newTestHandler()
	repo.createErr = errBoom
	user := &model.User{ID: "u1", Username: "testuser"}

	rec := post(h.GenToken, `{"permission":["read:account"]}`, user)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- MiAuthCheck (#1224) ---

func postMiAuthCheck(h *Handler, session string, userRepo *testutil.MockUserRepository) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("session")
	c.SetParamValues(session)
	if userRepo != nil {
		h.SetUserRepo(userRepo)
	}
	_ = h.MiAuthCheck(c)
	return rec
}

func TestMiAuthCheck_ReturnsTokenAndUser(t *testing.T) {
	h, repo := newTestHandler()
	sess := "sess-abc"
	repo.accessTokens["k"] = &model.AccessToken{ID: "at1", Token: "secret-token", UserID: "u1", Session: &sess}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	rec := postMiAuthCheck(h, sess, userRepo)
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, true, out["ok"])
	assert.Equal(t, "secret-token", out["token"])
	user, ok := out["user"].(map[string]any)
	require.True(t, ok, "user must be present and non-null")
	assert.Equal(t, "u1", user["id"])
	// one-time: token は fetched 済になる。
	assert.True(t, repo.accessTokens["k"].Fetched)
}

func TestMiAuthCheck_UnknownSession(t *testing.T) {
	h, _ := newTestHandler()
	rec := postMiAuthCheck(h, "ghost", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, false, out["ok"])
	_, hasToken := out["token"]
	assert.False(t, hasToken)
}

func TestMiAuthCheck_AlreadyFetched(t *testing.T) {
	h, repo := newTestHandler()
	sess := "sess-used"
	repo.accessTokens["k"] = &model.AccessToken{ID: "at1", Token: "t", UserID: "u1", Session: &sess, Fetched: true}
	rec := postMiAuthCheck(h, sess, nil)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, false, out["ok"], "already-fetched token must not be returned again")
}

func TestMiAuthCheck_EmptySession(t *testing.T) {
	h, _ := newTestHandler()
	rec := postMiAuthCheck(h, "", nil)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, false, out["ok"])
}

// failMarkRepo は MarkAccessTokenFetched が遷移に負ける (race の敗者 / DB
// エラー) ケースを再現するためのスタブ。
type failMarkRepo struct {
	*mockAuthSessionRepo
}

func (f *failMarkRepo) MarkAccessTokenFetched(string) (bool, error) { return false, errNotFound }

func TestMiAuthCheck_LosesFetchRace(t *testing.T) {
	base := newMockRepo()
	sess := "sess-race"
	base.accessTokens["k"] = &model.AccessToken{ID: "at1", Token: "t", UserID: "u1", Session: &sess}
	cfg := &config.Config{URL: "http://localhost:3000"}
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&failMarkRepo{base}, cfg, idGen)

	rec := postMiAuthCheck(h, sess, nil)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	// 遷移に負けたリクエストは token を払い出さない。**DB エラー側は見ていない** —
	// この stub は not-found を返す (#2792 で sentinel を分けた)。
	assert.Equal(t, false, out["ok"])
	_, hasToken := out["token"]
	assert.False(t, hasToken)
}

func TestMiAuthCheck_NoUserRepoOmitsUser(t *testing.T) {
	h, repo := newTestHandler()
	sess := "sess-nouser"
	repo.accessTokens["k"] = &model.AccessToken{ID: "at1", Token: "t", UserID: "u1", Session: &sess}
	// userRepo 未配線 → token は返るが user は省略 (null ではなく欠落)。
	rec := postMiAuthCheck(h, sess, nil)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, true, out["ok"])
	assert.Equal(t, "t", out["token"])
	_, hasUser := out["user"]
	assert.False(t, hasUser)
}
