package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- in-memory fakes ---

type fakeStore struct {
	txns   map[string]*transaction
	grants map[string]*grant
	claims map[string]bool
	tokens map[string]string // code -> issued token id
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		txns:   map[string]*transaction{},
		grants: map[string]*grant{},
		claims: map[string]bool{},
		tokens: map[string]string{},
	}
}

func (s *fakeStore) PutTransaction(_ context.Context, id string, t *transaction) error {
	s.txns[id] = t
	return nil
}
func (s *fakeStore) TakeTransaction(_ context.Context, id string) (*transaction, error) {
	t, ok := s.txns[id]
	if !ok {
		return nil, ErrNotFound
	}
	delete(s.txns, id)
	return t, nil
}
func (s *fakeStore) PutGrant(_ context.Context, code string, g *grant) error {
	s.grants[code] = g
	return nil
}
func (s *fakeStore) GetGrant(_ context.Context, code string) (*grant, error) {
	g, ok := s.grants[code]
	if !ok {
		return nil, ErrNotFound
	}
	return g, nil
}
func (s *fakeStore) ClaimGrant(_ context.Context, code string) (bool, error) {
	if s.claims[code] {
		return false, nil
	}
	s.claims[code] = true
	return true, nil
}
func (s *fakeStore) PutGrantToken(_ context.Context, code, tokenID string) error {
	s.tokens[code] = tokenID
	return nil
}
func (s *fakeStore) GetGrantToken(_ context.Context, code string) (string, error) {
	return s.tokens[code], nil
}

type fakeUserRepo struct{ byToken map[string]*model.User }

func (r *fakeUserRepo) FindByToken(token string) (*model.User, error) {
	u, ok := r.byToken[token]
	if !ok {
		return nil, ErrNotFound
	}
	return u, nil
}

type fakeTokenStore struct {
	created []*model.AccessToken
	deleted []string
}

func (s *fakeTokenStore) Create(t *model.AccessToken) error {
	s.created = append(s.created, t)
	return nil
}
func (s *fakeTokenStore) FindByID(id string) (*model.AccessToken, error) {
	for _, t := range s.created {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, nil
}
func (s *fakeTokenStore) DeleteByID(id string) error {
	s.deleted = append(s.deleted, id)
	return nil
}

// fakeInvalidator records raw tokens dropped from the auth cache.
type fakeInvalidator struct{ tokens []string }

func (f *fakeInvalidator) InvalidateToken(t string) { f.tokens = append(f.tokens, t) }

type fakeIDGen struct{ n int }

func (g *fakeIDGen) Generate(_ time.Time) string {
	g.n++
	return "tok" + string(rune('0'+g.n))
}

// testHarness wires a Handler against a discovery server that advertises the
// given redirect_uri.
type testHarness struct {
	h        *Handler
	store    *fakeStore
	users    *fakeUserRepo
	tokens   *fakeTokenStore
	clientID string
	redirect string
	consent  *ConsentMeta // captured by the renderer
}

func newHarness(t *testing.T) *testHarness {
	t.Helper()
	// client discovery server: JSON metadata listing the redirect_uri.
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"client_id": "` + srvURL + `/",
			"client_uri": "` + srvURL + `/",
			"client_name": "Test App",
			"redirect_uris": ["` + srvURL + `/cb"]
		}`))
	}))
	t.Cleanup(srv.Close)
	srvURL = srv.URL

	store := newFakeStore()
	users := &fakeUserRepo{byToken: map[string]*model.User{}}
	tokens := &fakeTokenStore{}
	hn := &testHarness{store: store, users: users, tokens: tokens, clientID: srv.URL + "/", redirect: srv.URL + "/cb"}
	hn.h = NewHandler(store, srv.Client(), users, tokens, &fakeIDGen{}, "https://mk.example", true,
		func(c echo.Context, m ConsentMeta) error {
			hn.consent = &m
			return c.HTML(http.StatusOK, "<html>consent</html>")
		})
	return hn
}

func (hn *testHarness) authorize(t *testing.T, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	_ = hn.h.Authorize(e.NewContext(req, rec))
	return rec
}

func (hn *testHarness) post(t *testing.T, handler echo.HandlerFunc, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	rec := httptest.NewRecorder()
	_ = handler(e.NewContext(req, rec))
	return rec
}

func validAuthorizeQuery(hn *testHarness, challenge string) url.Values {
	return url.Values{
		"response_type":         {"code"},
		"client_id":             {hn.clientID},
		"redirect_uri":          {hn.redirect},
		"scope":                 {"read:account write:notes"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"st-123"},
	}
}

// --- Authorize ---

func TestAuthorize_RendersConsent(t *testing.T) {
	hn := newHarness(t)
	rec := hn.authorize(t, validAuthorizeQuery(hn, challengeFor("verifier-abc")))

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, hn.consent)
	assert.Equal(t, "Test App", hn.consent.ClientName)
	assert.Equal(t, "read:account write:notes", hn.consent.Scope)
	assert.NotEmpty(t, hn.consent.TransactionID)
	// transaction が格納されている。
	_, ok := hn.store.txns[hn.consent.TransactionID]
	assert.True(t, ok)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

func TestAuthorize_InvalidClientID(t *testing.T) {
	hn := newHarness(t)
	q := validAuthorizeQuery(hn, challengeFor("v"))
	q.Set("client_id", "not-a-url")
	rec := hn.authorize(t, q)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Nil(t, hn.consent)
}

func TestAuthorize_RedirectURINotRegistered(t *testing.T) {
	hn := newHarness(t)
	q := validAuthorizeQuery(hn, challengeFor("v"))
	q.Set("redirect_uri", "https://evil.example/cb")
	rec := hn.authorize(t, q)
	// discovery list に無い redirect_uri → 直接 400 (redirect しない)。
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Nil(t, hn.consent)
}

func TestAuthorize_MissingPKCE(t *testing.T) {
	hn := newHarness(t)
	q := validAuthorizeQuery(hn, "")
	q.Del("code_challenge")
	rec := hn.authorize(t, q)
	// redirect_uri は validated 済みなので error redirect (302)。
	assert.Equal(t, http.StatusFound, rec.Code)
	loc := rec.Header().Get("Location")
	assert.Contains(t, loc, "error=invalid_request")
}

func TestAuthorize_NonS256Challenge(t *testing.T) {
	hn := newHarness(t)
	q := validAuthorizeQuery(hn, "plainchallenge")
	q.Set("code_challenge_method", "plain")
	rec := hn.authorize(t, q)
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Contains(t, rec.Header().Get("Location"), "error=invalid_request")
}

func TestAuthorize_EmptyScope(t *testing.T) {
	hn := newHarness(t)
	q := validAuthorizeQuery(hn, challengeFor("v"))
	q.Del("scope")
	rec := hn.authorize(t, q)
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Contains(t, rec.Header().Get("Location"), "error=invalid_scope")
}

// TestAuthorize_AllUnknownScope: 既知 kinds に 1 つも該当しない scope は
// invalid_scope (#1904)。
func TestAuthorize_AllUnknownScope(t *testing.T) {
	hn := newHarness(t)
	q := validAuthorizeQuery(hn, challengeFor("v"))
	q.Set("scope", "bogus another:unknown")
	rec := hn.authorize(t, q)
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Contains(t, rec.Header().Get("Location"), "error=invalid_scope")
	assert.Nil(t, hn.consent)
}

// TestAuthorize_FiltersUnknownScopes: 未知 scope は除去され、既知 kinds のみが
// consent / token に渡る (#1904)。
func TestAuthorize_FiltersUnknownScopes(t *testing.T) {
	hn := newHarness(t)
	q := validAuthorizeQuery(hn, challengeFor("v"))
	q.Set("scope", "read:account bogus write:notes read:*")
	rec := hn.authorize(t, q)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, hn.consent)
	assert.Equal(t, "read:account write:notes", hn.consent.Scope)
	// 格納された txn の scopes も filter 済み。
	txn := hn.store.txns[hn.consent.TransactionID]
	require.NotNil(t, txn)
	assert.Equal(t, []string{"read:account", "write:notes"}, txn.Scopes)
}

// --- Decision ---

func TestDecision_IssuesCode(t *testing.T) {
	hn := newHarness(t)
	hn.users.byToken["native-tok"] = &model.User{ID: "u1"}
	hn.store.txns["txn1"] = &transaction{
		ClientID: hn.clientID, RedirectURI: hn.redirect, Scopes: []string{"read:account"},
		CodeChallenge: challengeFor("v"), State: "st-123",
	}
	rec := hn.post(t, hn.h.Decision, url.Values{
		"transaction_id": {"txn1"}, "login_token": {"native-tok"},
	})
	require.Equal(t, http.StatusFound, rec.Code)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	code := loc.Query().Get("code")
	require.NotEmpty(t, code)
	assert.Equal(t, "st-123", loc.Query().Get("state"))
	assert.Equal(t, "https://mk.example", loc.Query().Get("iss"))
	g := hn.store.grants[code]
	require.NotNil(t, g)
	assert.Equal(t, "u1", g.UserID)
}

func TestDecision_Cancel(t *testing.T) {
	hn := newHarness(t)
	hn.store.txns["txn1"] = &transaction{ClientID: hn.clientID, RedirectURI: hn.redirect, State: "st"}
	rec := hn.post(t, hn.h.Decision, url.Values{"transaction_id": {"txn1"}, "cancel": {"1"}})
	require.Equal(t, http.StatusFound, rec.Code)
	assert.Contains(t, rec.Header().Get("Location"), "error=access_denied")
}

// 不明な login_token も redirect せず直接返す (upstream の
// #findUserByLoginToken は InvalidRequestError を投げる = 400)。
func TestDecision_BadLoginToken(t *testing.T) {
	hn := newHarness(t)
	hn.store.txns["txn1"] = &transaction{ClientID: hn.clientID, RedirectURI: hn.redirect}
	rec := hn.post(t, hn.h.Decision, url.Values{"transaction_id": {"txn1"}, "login_token": {"bogus"}})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, rec.Header().Get("Location"), "redirect させないこと")
}

// 期限切れ / 未知の transaction は access_denied (403)。transaction_id 欠落の
// invalid_request (400) とは種別が違う。
func TestDecision_ExpiredTransaction(t *testing.T) {
	hn := newHarness(t)
	rec := hn.post(t, hn.h.Decision, url.Values{"transaction_id": {"gone"}, "login_token": {"x"}})
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// --- Token ---

func seedGrant(hn *testHarness, code string) {
	hn.store.grants[code] = &grant{
		ClientID: hn.clientID, UserID: "u1", RedirectURI: hn.redirect,
		CodeChallenge: challengeFor("the-verifier"), Scopes: []string{"read:account", "write:notes"},
	}
}

func validTokenForm(hn *testHarness, code string) url.Values {
	return url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {hn.clientID},
		"redirect_uri":  {hn.redirect},
		"code_verifier": {"the-verifier"},
	}
}

func TestToken_Success(t *testing.T) {
	hn := newHarness(t)
	seedGrant(hn, "code1")
	rec := hn.post(t, hn.h.Token, validTokenForm(hn, "code1"))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["access_token"])
	assert.Equal(t, "Bearer", resp["token_type"])
	assert.Equal(t, "read:account write:notes", resp["scope"])

	require.Len(t, hn.tokens.created, 1)
	tok := hn.tokens.created[0]
	assert.Equal(t, "u1", tok.UserID)
	assert.Equal(t, tok.Token, tok.Hash, "OAuth token: hash == token")
	require.NotNil(t, tok.Name)
	assert.Equal(t, hn.clientID, *tok.Name)
	assert.ElementsMatch(t, []string{"read:account", "write:notes"}, []string(tok.Permission))
	assert.Nil(t, tok.AppID, "OAuth token has no registered app")
}

// TestToken_JSONBody confirms the token endpoint also accepts an
// application/json body (upstream bodyParser.json parity, #1899).
func TestToken_JSONBody(t *testing.T) {
	hn := newHarness(t)
	seedGrant(hn, "code1")
	e := echo.New()
	jsonBody := `{"grant_type":"authorization_code","code":"code1","client_id":"` + hn.clientID +
		`","redirect_uri":"` + hn.redirect + `","code_verifier":"the-verifier"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(jsonBody))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	_ = hn.h.Token(e.NewContext(req, rec))

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["access_token"])
	require.Len(t, hn.tokens.created, 1)
}

func TestToken_ReplayRevokesAndRejects(t *testing.T) {
	hn := newHarness(t)
	inv := &fakeInvalidator{}
	hn.h.SetAuthInvalidator(inv)
	seedGrant(hn, "code1")
	// 1 回目: 成功。
	rec1 := hn.post(t, hn.h.Token, validTokenForm(hn, "code1"))
	require.Equal(t, http.StatusOK, rec1.Code)
	require.Len(t, hn.tokens.created, 1)
	issuedID := hn.tokens.created[0].ID
	issuedToken := hn.tokens.created[0].Token

	// 2 回目 (replay): invalid_grant + 既発行 token を revoke。
	rec2 := hn.post(t, hn.h.Token, validTokenForm(hn, "code1"))
	assert.Equal(t, http.StatusBadRequest, rec2.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp))
	assert.Equal(t, "invalid_grant", resp["error"])
	assert.Contains(t, hn.tokens.deleted, issuedID, "replay は発行済み token を revoke する")
	// DB から消すだけでは auth cache (30s TTL) が残るので、raw token も落とす。
	assert.Contains(t, inv.tokens, issuedToken, "replay は auth cache も無効化する")
}

type fakeStreamRevoker struct {
	calls [][2]string
}

func (f *fakeStreamRevoker) RevokeAccessTokenStreams(userID, tokenID string) {
	f.calls = append(f.calls, [2]string{userID, tokenID})
}

// replay で失効させた token で既に張られた WebSocket も閉じる。
func TestToken_ReplayRevokesStreams(t *testing.T) {
	hn := newHarness(t)
	rev := &fakeStreamRevoker{}
	hn.h.SetStreamRevoker(rev)
	assert.True(t, hn.h.HasStreamRevoker())
	seedGrant(hn, "code1")
	rec1 := hn.post(t, hn.h.Token, validTokenForm(hn, "code1"))
	require.Equal(t, http.StatusOK, rec1.Code)
	assert.Empty(t, rev.calls, "正常な発行では閉じない")
	issuedID := hn.tokens.created[0].ID

	rec2 := hn.post(t, hn.h.Token, validTokenForm(hn, "code1"))
	assert.Equal(t, http.StatusBadRequest, rec2.Code)
	assert.Equal(t, [][2]string{{"u1", issuedID}}, rev.calls)
}

func TestToken_WrongVerifier(t *testing.T) {
	hn := newHarness(t)
	seedGrant(hn, "code1")
	form := validTokenForm(hn, "code1")
	form.Set("code_verifier", "wrong")
	rec := hn.post(t, hn.h.Token, form)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, hn.tokens.created)
}

func TestToken_RedirectURIMismatch(t *testing.T) {
	hn := newHarness(t)
	seedGrant(hn, "code1")
	form := validTokenForm(hn, "code1")
	form.Set("redirect_uri", "https://evil.example/cb")
	rec := hn.post(t, hn.h.Token, form)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, hn.tokens.created)
}

func TestToken_ClientIDMismatch(t *testing.T) {
	hn := newHarness(t)
	seedGrant(hn, "code1")
	form := validTokenForm(hn, "code1")
	form.Set("client_id", "https://other.example/")
	rec := hn.post(t, hn.h.Token, form)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, hn.tokens.created)
}

func TestToken_UnsupportedGrantType(t *testing.T) {
	hn := newHarness(t)
	seedGrant(hn, "code1")
	form := validTokenForm(hn, "code1")
	form.Set("grant_type", "client_credentials")
	rec := hn.post(t, hn.h.Token, form)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "unsupported_grant_type", resp["error"])
}

func TestToken_UnknownCode(t *testing.T) {
	hn := newHarness(t)
	rec := hn.post(t, hn.h.Token, validTokenForm(hn, "nope"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "invalid_grant", resp["error"])
}

// --- End-to-end ---

func TestEndToEnd_AuthorizeDecisionToken(t *testing.T) {
	hn := newHarness(t)
	hn.users.byToken["native-tok"] = &model.User{ID: "u1"}
	verifier := "e2e-verifier-1234567890"

	// 1. authorize → consent meta + txn。
	rec := hn.authorize(t, validAuthorizeQuery(hn, challengeFor(verifier)))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, hn.consent)
	txnID := hn.consent.TransactionID

	// 2. decision → code。
	drec := hn.post(t, hn.h.Decision, url.Values{"transaction_id": {txnID}, "login_token": {"native-tok"}})
	require.Equal(t, http.StatusFound, drec.Code)
	loc, _ := url.Parse(drec.Header().Get("Location"))
	code := loc.Query().Get("code")
	require.NotEmpty(t, code)

	// 3. token → access token。
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"client_id": {hn.clientID}, "redirect_uri": {hn.redirect}, "code_verifier": {verifier},
	}
	trec := hn.post(t, hn.h.Token, form)
	require.Equal(t, http.StatusOK, trec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(trec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["access_token"])
	require.Len(t, hn.tokens.created, 1)
	assert.Equal(t, "u1", hn.tokens.created[0].UserID)
}

// flakyStore wraps fakeStore and forces a non-NotFound error from one method
// to exercise the handlers' server_error branches.
type flakyStore struct {
	*fakeStore
	failPutTxn   bool
	failPutGrant bool
	failGetGrant bool
	failClaim    bool
}

func (s *flakyStore) PutTransaction(ctx context.Context, id string, t *transaction) error {
	if s.failPutTxn {
		return assertErr
	}
	return s.fakeStore.PutTransaction(ctx, id, t)
}
func (s *flakyStore) PutGrant(ctx context.Context, code string, g *grant) error {
	if s.failPutGrant {
		return assertErr
	}
	return s.fakeStore.PutGrant(ctx, code, g)
}
func (s *flakyStore) GetGrant(ctx context.Context, code string) (*grant, error) {
	if s.failGetGrant {
		return nil, assertErr
	}
	return s.fakeStore.GetGrant(ctx, code)
}
func (s *flakyStore) ClaimGrant(ctx context.Context, code string) (bool, error) {
	if s.failClaim {
		return false, assertErr
	}
	return s.fakeStore.ClaimGrant(ctx, code)
}

var assertErr = errStore("boom")

type errStore string

func (e errStore) Error() string { return string(e) }

func TestAuthorize_DiscoveryFailure(t *testing.T) {
	// discovery server が 404 → client metadata 取得失敗 → 直接 400。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	h := NewHandler(newFakeStore(), srv.Client(), &fakeUserRepo{}, &fakeTokenStore{}, &fakeIDGen{}, "https://mk.example", true,
		func(c echo.Context, _ ConsentMeta) error { return c.HTML(http.StatusOK, "x") })
	e := echo.New()
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {srv.URL + "/"}, "redirect_uri": {srv.URL + "/cb"},
		"scope": {"read:account"}, "code_challenge": {challengeFor("v")}, "code_challenge_method": {"S256"},
	}
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	_ = h.Authorize(e.NewContext(req, rec))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAuthorize_StoreError(t *testing.T) {
	hn := newHarness(t)
	hn.h.store = &flakyStore{fakeStore: hn.store, failPutTxn: true}
	rec := hn.authorize(t, validAuthorizeQuery(hn, challengeFor("v")))
	// redirect_uri は validated 済みなので server_error redirect。
	require.Equal(t, http.StatusFound, rec.Code)
	assert.Contains(t, rec.Header().Get("Location"), "error=server_error")
}

func TestDecision_PutGrantError(t *testing.T) {
	hn := newHarness(t)
	hn.users.byToken["native-tok"] = &model.User{ID: "u1"}
	hn.store.txns["txn1"] = &transaction{ClientID: hn.clientID, RedirectURI: hn.redirect, Scopes: []string{"read:account"}, CodeChallenge: challengeFor("v")}
	hn.h.store = &flakyStore{fakeStore: hn.store, failPutGrant: true}
	rec := hn.post(t, hn.h.Decision, url.Values{"transaction_id": {"txn1"}, "login_token": {"native-tok"}})
	require.Equal(t, http.StatusFound, rec.Code)
	assert.Contains(t, rec.Header().Get("Location"), "error=server_error")
}

func TestToken_GrantLookupError(t *testing.T) {
	hn := newHarness(t)
	seedGrant(hn, "code1")
	hn.h.store = &flakyStore{fakeStore: hn.store, failGetGrant: true}
	rec := hn.post(t, hn.h.Token, validTokenForm(hn, "code1"))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestToken_ClaimError(t *testing.T) {
	hn := newHarness(t)
	seedGrant(hn, "code1")
	hn.h.store = &flakyStore{fakeStore: hn.store, failClaim: true}
	rec := hn.post(t, hn.h.Token, validTokenForm(hn, "code1"))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestToken_InvalidClientIDForm(t *testing.T) {
	hn := newHarness(t)
	seedGrant(hn, "code1")
	form := validTokenForm(hn, "code1")
	form.Set("client_id", "not-a-url") // canonicalClientID → "" → mismatch
	rec := hn.post(t, hn.h.Token, form)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, hn.tokens.created)
}

func TestNotFound(t *testing.T) {
	hn := newHarness(t)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/oauth/whatever", nil)
	rec := httptest.NewRecorder()
	_ = hn.h.NotFound(e.NewContext(req, rec))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "UNKNOWN_OAUTH_ENDPOINT", errObj["code"])
}

// #2079: response_type が code 以外 / 欠落のとき、client_id 検証より前に 501
// unsupported_response_type を直接返す (redirect しない、upstream OAuth2ProviderService)。
func TestAuthorize_UnsupportedResponseType(t *testing.T) {
	hn := newHarness(t)
	for _, rt := range []string{"token", "", "id_token"} {
		q := validAuthorizeQuery(hn, challengeFor("verifier-abc"))
		q.Set("response_type", rt)
		if rt == "" {
			q.Del("response_type")
		}
		rec := hn.authorize(t, q)
		require.Equal(t, http.StatusNotImplemented, rec.Code, "response_type=%q は 501", rt)
		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "unsupported_response_type", body["error"], "response_type=%q", rt)
		// redirect していないこと (consent も発行されない)。
		assert.Empty(t, rec.Header().Get("Location"))
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	}
	// code は従来通り consent。
	rec := hn.authorize(t, validAuthorizeQuery(hn, challengeFor("verifier-abc")))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #2106 L21: 空 login_token は lookup 前に弾く。
//
// upstream は decision endpoint のエラーを「Misskey 側の問題でクライアントには
// 関係ない」として redirect せず直接返す (oauth.ts: "Do not use indirect error
// here")。旧実装は redirect していたので 400 direct に揃えた。
func TestDecision_EmptyLoginToken(t *testing.T) {
	hn := newHarness(t)
	hn.store.txns["txn1"] = &transaction{ClientID: hn.clientID, RedirectURI: hn.redirect}
	rec := hn.post(t, hn.h.Decision, url.Values{"transaction_id": {"txn1"}, "login_token": {""}})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, rec.Header().Get("Location"), "redirect させないこと")
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "invalid_request", body["error"])
}

// transaction_id 欠落は invalid_request (400)、無効 / 期限切れは
// access_denied (403)。upstream が種別を分けているのに合わせる。
func TestDecision_TransactionErrors(t *testing.T) {
	hn := newHarness(t)

	rec := hn.post(t, hn.h.Decision, url.Values{"login_token": {"tok"}})
	require.Equal(t, http.StatusBadRequest, rec.Code, "transaction_id 欠落は 400")
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "invalid_request", body["error"])

	rec = hn.post(t, hn.h.Decision, url.Values{"transaction_id": {"nope"}, "login_token": {"tok"}})
	require.Equal(t, http.StatusForbidden, rec.Code, "無効な transaction_id は 403")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "access_denied", body["error"])
	assert.Empty(t, rec.Header().Get("Location"))
}
