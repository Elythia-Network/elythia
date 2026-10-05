package wellknown

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newHandler(t *testing.T) (*Handler, *testutil.MockUserRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := user.NewService(userRepo, testutil.NewMockNoteRepository(), testutil.NewMockUserNotePiningRepository(), idGen)
	urls := activitypub.NewURLBuilder("https://example.com")
	return NewHandler(urls, svc, "example.com", "https://example.com"), userRepo
}

func newReq(t *testing.T, target string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func addUser(repo *testutil.MockUserRepository, id, username string) {
	repo.Users[id] = &model.User{ID: id, Username: username, UsernameLower: username}
}

func TestWebfinger_AcctWithHost(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "u1", "alice")

	c, rec := newReq(t, "/.well-known/webfinger?resource=acct:alice@example.com")
	require.NoError(t, h.Webfinger(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "acct:alice@example.com", resp["subject"])
}

// #1948-22: upstream WellKnownServerService は jrd/xrd を bare content-type
// (charset 無し) で返す。webfinger JRD と host-meta XRD の Content-Type を pin する。
func TestWebfinger_JRDBareContentType(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "u1", "alice")
	c, rec := newReq(t, "/.well-known/webfinger?resource=acct:alice@example.com")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, "application/jrd+json", rec.Header().Get("Content-Type"))
}

func TestHostMeta_XRDBareContentType(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, "/.well-known/host-meta")
	require.NoError(t, h.HostMeta(c))
	assert.Equal(t, "application/xrd+xml", rec.Header().Get("Content-Type"))
}

func TestWebfinger_AcctWithoutHost(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "u1", "alice")

	c, rec := newReq(t, "/.well-known/webfinger?resource=acct:alice")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1834 F3: 別ホスト acct は upstream fromAcct と同じく 422。
func TestWebfinger_AcctWrongHost(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, "/.well-known/webfinger?resource=acct:alice@other.example")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

// acct:alice@bob@charlie は host=bob(@charlie) と解釈され別ホスト扱い → 422。
func TestWebfinger_AcctMalformed(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, "/.well-known/webfinger?resource=acct:alice@bob@charlie")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

// #1834 F2: actor-URI 形式 (${url}/users/<id>) は id で解決する。
func TestWebfinger_HTTPSResource(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "u1", "alice")
	c, rec := newReq(t, "/.well-known/webfinger?resource=https://example.com/users/u1")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// http スキームは origin (https://example.com) と不一致のため actor-URI として
// マッチせず username 検索に落ちて 404 (upstream は config.url を scheme 込みで照合)。
func TestWebfinger_HTTPResource(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "u1", "alice")
	c, rec := newReq(t, "/.well-known/webfinger?resource=http://example.com/users/u1")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// 別ホストの actor-URI は /users/ prefix に一致せず username 検索に落ちて 404。
func TestWebfinger_HTTPSWrongHost(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, "/.well-known/webfinger?resource=https://other.example/users/alice")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// /users/ でも /@ でもない path は username 検索に落ちて 404。
func TestWebfinger_HTTPSWrongPath(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, "/.well-known/webfinger?resource=https://example.com/something")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestWebfinger_HTTPSInvalidURL(t *testing.T) {
	h, _ := newHandler(t)
	// invalid URL with control char
	c, rec := newReq(t, "/.well-known/webfinger?resource=https://example.com/users/alice%00")
	require.NoError(t, h.Webfinger(c))
	// パースは成功してusersの後ろにごみが付くので NotFound
	assert.NotEqual(t, http.StatusOK, rec.Code)
}

// mailto:alice@example.com は host=example.com (自ホスト) と解釈され username
// "mailto:alice" の検索に落ちて 404 (upstream Acct.parse と同じ挙動)。
func TestWebfinger_UnknownScheme(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, "/.well-known/webfinger?resource=mailto:alice@example.com")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestWebfinger_NoResource(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, "/.well-known/webfinger")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestWebfinger_UserNotFound(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, "/.well-known/webfinger?resource=acct:ghost@example.com")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestWebfinger_DeletedUserIsNotFound(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "u1", "alice")
	repo.Users["u1"].IsDeleted = true

	c, rec := newReq(t, "/.well-known/webfinger?resource=acct:alice@example.com")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHostMeta(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, "/.well-known/host-meta")
	require.NoError(t, h.HostMeta(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "webfinger")
}

func TestNodeInfoDiscovery(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, "/.well-known/nodeinfo")
	require.NoError(t, h.NodeInfoDiscovery(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	links := resp["links"].([]any)
	// TS本家と同じく2.1と2.0の両方を返す
	assert.Len(t, links, 2)
	assert.Contains(t, rec.Body.String(), "nodeinfo/2.1")
	assert.Contains(t, rec.Body.String(), "nodeinfo/2.0")
}

func TestWebfinger_ResponseLinks(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "u1", "alice")

	c, rec := newReq(t, "/.well-known/webfinger?resource=acct:alice@example.com")
	require.NoError(t, h.Webfinger(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	links := resp["links"].([]any)
	// self, profile-page, subscribe の3リンク
	require.Len(t, links, 3)

	self := links[0].(map[string]any)
	assert.Equal(t, "self", self["rel"])
	assert.Equal(t, "application/activity+json", self["type"])

	profile := links[1].(map[string]any)
	assert.Equal(t, "http://webfinger.net/rel/profile-page", profile["rel"])
	assert.Equal(t, "https://example.com/@alice", profile["href"])

	subscribe := links[2].(map[string]any)
	assert.Equal(t, "http://ostatus.org/schema/1.0/subscribe", subscribe["rel"])
	assert.Equal(t, "https://example.com/authorize-follow?acct={uri}", subscribe["template"])

	// CORSヘッダー
	assert.Equal(t, "Accept", rec.Header().Get("Vary"))
	assert.Equal(t, "Vary", rec.Header().Get("Access-Control-Expose-Headers"))
}

func TestHostMetaJSON(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, "/.well-known/host-meta.json")
	require.NoError(t, h.HostMetaJSON(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	links := resp["links"].([]any)
	require.Len(t, links, 1)
	link := links[0].(map[string]any)
	assert.Equal(t, "lrdd", link["rel"])
	assert.Equal(t, "application/jrd+json", link["type"])
	assert.Equal(t, "https://example.com/.well-known/webfinger?resource={uri}", link["template"])
}

func TestOAuthAuthorizationServer(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, "/.well-known/oauth-authorization-server")
	require.NoError(t, h.OAuthAuthorizationServer(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "https://example.com", resp["issuer"])
	assert.Equal(t, "https://example.com/oauth/authorize", resp["authorization_endpoint"])
	assert.Equal(t, "https://example.com/oauth/token", resp["token_endpoint"])
	// scopes_supported は granular な permission kinds を広告する
	// (Mastodon 風の "read"/"write"/"follow" ではない、#1904)。
	scopes, ok := resp["scopes_supported"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, scopes)
	assert.Contains(t, scopes, "read:account")
	assert.Contains(t, scopes, "write:notes")
}

// --- #1834 F1: federation='none' で discovery が 403 ---

func newHandlerWithFederation(t *testing.T, federation string) (*Handler, *testutil.MockUserRepository) {
	h, repo := newHandler(t)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", Federation: federation}
	h.SetMetaRepo(metaRepo)
	return h, repo
}

func TestWebfinger_FederationNone403(t *testing.T) {
	h, repo := newHandlerWithFederation(t, "none")
	addUser(repo, "u1", "alice")
	c, rec := newReq(t, "/.well-known/webfinger?resource=acct:alice@example.com")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestWebfinger_FederationAllResolves(t *testing.T) {
	h, repo := newHandlerWithFederation(t, "all")
	addUser(repo, "u1", "alice")
	c, rec := newReq(t, "/.well-known/webfinger?resource=acct:alice@example.com")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestDiscovery_FederationNone403(t *testing.T) {
	h, _ := newHandlerWithFederation(t, "none")
	cases := []struct {
		name string
		fn   func(echo.Context) error
	}{
		{"host-meta", h.HostMeta},
		{"host-meta.json", h.HostMetaJSON},
		{"nodeinfo", h.NodeInfoDiscovery},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newReq(t, "/.well-known/"+tc.name)
			require.NoError(t, tc.fn(c))
			assert.Equal(t, http.StatusForbidden, rec.Code)
		})
	}
}

// metaRepo 未配線なら fail-open (403 にしない)。
func TestWebfinger_NoMetaRepoFailsOpen(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "u1", "alice")
	c, rec := newReq(t, "/.well-known/webfinger?resource=acct:alice@example.com")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// --- #1834 F5: /@username 形式・XRD negotiation・Cache-Control ---

func TestWebfinger_AtUsernameForm(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "u1", "alice")
	c, rec := newReq(t, "/.well-known/webfinger?resource=https://example.com/@alice")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "acct:alice@example.com")
}

func TestWebfinger_JRDDefault(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "u1", "alice")
	c, rec := newReq(t, "/.well-known/webfinger?resource=acct:alice@example.com")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/jrd+json")
	assert.Equal(t, "public, max-age=180", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "Accept", rec.Header().Get("Vary"))
}

func TestWebfinger_XRDContentNegotiation(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "u1", "alice")
	c, rec := newReq(t, "/.well-known/webfinger?resource=acct:alice@example.com")
	c.Request().Header.Set("Accept", "application/xrd+xml")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/xrd+xml")
	body := rec.Body.String()
	assert.Contains(t, body, "<XRD")
	assert.Contains(t, body, "acct:alice@example.com")
	assert.Contains(t, body, "application/activity+json")
}

// jrd を明示的に要求したら XML ではなく JSON。
func TestWebfinger_JRDExplicit(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "u1", "alice")
	c, rec := newReq(t, "/.well-known/webfinger?resource=acct:alice@example.com")
	c.Request().Header.Set("Accept", "application/jrd+json")
	require.NoError(t, h.Webfinger(c))
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/jrd+json")
}

// meta fetch が error なら fail-open (403 にしない)。MockMetaRepository は Meta が
// nil のとき Fetch が error を返す。
func TestWebfinger_FetchErrorFailsOpen(t *testing.T) {
	h, repo := newHandler(t)
	addUser(repo, "u1", "alice")
	metaRepo := testutil.NewMockMetaRepository() // Meta=nil -> Fetch errors
	h.SetMetaRepo(metaRepo)
	c, rec := newReq(t, "/.well-known/webfinger?resource=acct:alice@example.com")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1924 M1: suspended local user は upstream isSuspended:false gate と同じく 404。
func TestWebfinger_SuspendedUserNotFound(t *testing.T) {
	h, repo := newHandler(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", UsernameLower: "alice", IsSuspended: true}
	c, rec := newReq(t, "/.well-known/webfinger?resource=acct:alice@example.com")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// actor-URI の id 部分に余分な path があれば 400。
func TestWebfinger_ActorURIExtraPath(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, "/.well-known/webfinger?resource=https://example.com/users/u1/extra")
	require.NoError(t, h.Webfinger(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestWantsXRD(t *testing.T) {
	cases := []struct {
		accept string
		want   bool
	}{
		{"", false},
		{"application/jrd+json", false},
		{"application/json", false}, // jrd の sibling は match しない -> JRD (else 分岐)
		{"application/xrd+xml", true},
		{"*/*", false},                                       // tie -> jrd 優先
		{"application/xrd+xml, application/jrd+json", false}, // 両 q=1 tie -> jrd
		{"application/xrd+xml;q=0.9, application/jrd+json;q=0.5", true}, // xrd 優勢
		{"application/jrd+json;q=0.9, application/xrd+xml;q=0.5", false},
		{"application/json, application/xrd+xml", true}, // xrd だけが match
		{"text/html", false},
	}
	for _, tc := range cases {
		assert.Equalf(t, tc.want, wantsXRD(tc.accept), "Accept=%q", tc.accept)
	}
}

func TestParseQ(t *testing.T) {
	cases := []struct {
		s    string
		want float64
	}{
		{"", 1.0}, {"1", 1.0}, {"1.0", 1.0}, {"0", 0.0}, {"0.0", 0.0},
		{"0.5", 0.5}, {"0.9", 0.9}, {"0.25", 0.25}, {"bad", 0.0},
		{"2", 1.0}, {"1.5", 1.0}, // 範囲外は 1.0 にクランプ
	}
	for _, tc := range cases {
		assert.InDeltaf(t, tc.want, parseQ(tc.s), 0.0001, "q=%q", tc.s)
	}
}

// upstream WellKnownServerService は preflight に `Accept` だけを広告する。
func TestPreflight(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodOptions, "/.well-known/webfinger", nil)
	rec := httptest.NewRecorder()
	h := NewHandler(nil, nil, "https://example.com", "example.com")
	require.NoError(t, h.Preflight(e.NewContext(req, rec)))

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "Accept", rec.Header().Get("Access-Control-Allow-Headers"))
	assert.Equal(t, "GET, OPTIONS", rec.Header().Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "Vary", rec.Header().Get("Access-Control-Expose-Headers"))
}

// --- upstream PR 17901: W3C "A Well-Known URL for Changing Passwords" ---

// パスワードマネージャは 302 の Location しか見ないので、絶対 URL であることを
// 固定する (upstream は config.url 基準の絶対 URL を返す)。
func TestChangePassword_RedirectsToSettingsSecurity(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, "/.well-known/change-password")
	require.NoError(t, h.ChangePassword(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "https://example.com/settings/security", rec.Header().Get("Location"))
	// **global CORS middleware は `/.well-known/` を Skipper で除外している**ので、
	// handler が付けないとヘッダがゼロになる。upstream は plugin scope の onRequest
	// hook で 4 つとも付く。
	assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "GET, OPTIONS", rec.Header().Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "Accept", rec.Header().Get("Access-Control-Allow-Headers"))
	assert.Equal(t, "Vary", rec.Header().Get("Access-Control-Expose-Headers"))
}

// federation='none' でも 302 を返す。upstream が 403 にするのは host-meta /
// host-meta.json / nodeinfo / webfinger の 4 本だけで、パスワード変更導線は
// 連合の可否と無関係。
func TestChangePassword_NotGatedByFederation(t *testing.T) {
	h, _ := newHandlerWithFederation(t, "none")
	c, rec := newReq(t, "/.well-known/change-password")
	require.NoError(t, h.ChangePassword(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "https://example.com/settings/security", rec.Header().Get("Location"))
}

// /.well-known/nodeinfo は内容が origin だけから決まる静的文書なので、
// 未認証 crawler の 1 hit ごとに origin まで届かせない。同ファイルの
// WebFinger が `public, max-age=180` を返すのに、ここだけ Cache-Control が
// 無かった。
func TestNodeInfoDiscovery_CacheControl(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newReq(t, "/.well-known/nodeinfo")
	require.NoError(t, h.NodeInfoDiscovery(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "public, max-age=600", rec.Header().Get("Cache-Control"))
}

// 403 (federation=none) には Cache-Control を付けない。付けると連合を
// 有効に戻したあとも中継が 403 を配り続ける。
func TestNodeInfoDiscovery_NoCacheControlWhenForbidden(t *testing.T) {
	h, _ := newHandlerWithFederation(t, "none")
	c, rec := newReq(t, "/.well-known/nodeinfo")
	require.NoError(t, h.NodeInfoDiscovery(c))
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Empty(t, rec.Header().Get("Cache-Control"))
}
