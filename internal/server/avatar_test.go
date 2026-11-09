package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// stubAvatarLookup is a hand-rolled minimal mock for the avatar
// handler. Using a custom struct rather than testutil.MockUserRepository
// keeps this test focused on the handler's redirect contract.
type stubAvatarLookup struct {
	users map[string]*model.User // key: "username|host" (host="" for local)
	err   error
}

func (s *stubAvatarLookup) FindByUsernameLower(username string, host *string) (*model.User, error) {
	if s.err != nil {
		return nil, s.err
	}
	key := username + "|"
	if host != nil {
		key += *host
	}
	u, ok := s.users[key]
	if !ok {
		return nil, errors.New("not found")
	}
	return u, nil
}

func newAvatarTestContext(t *testing.T, acct string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/avatar/@"+acct, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath("/avatar/@:acct")
	c.SetParamNames("acct")
	c.SetParamValues(acct)
	return c, rec
}

func TestAvatarHandler_LocalUserRedirectsToAvatarURL(t *testing.T) {
	avatarURL := "https://cdn.example/avatars/alice.png"
	repo := &stubAvatarLookup{users: map[string]*model.User{
		"alice|": {ID: "u1", Username: "alice", AvatarURL: &avatarURL},
	}}
	h := avatarHandler(nil, repo, "go.example")

	c, rec := newAvatarTestContext(t, "alice")
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, avatarURL, rec.Header().Get(echo.HeaderLocation))
	assert.Equal(t, "public, max-age=86400", rec.Header().Get(echo.HeaderCacheControl))
}

func TestAvatarHandler_RemoteUserUsesHostFilter(t *testing.T) {
	avatarURL := "https://r/u.png"
	repo := &stubAvatarLookup{users: map[string]*model.User{
		"bob|remote.example": {ID: "u2", Username: "bob", AvatarURL: &avatarURL},
	}}
	h := avatarHandler(nil, repo, "go.example")

	c, rec := newAvatarTestContext(t, "bob@remote.example")
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, avatarURL, rec.Header().Get(echo.HeaderLocation))
}

func TestAvatarHandler_OwnHostTreatedAsLocal(t *testing.T) {
	// acct の host 部が自インスタンス host と一致する場合は local user
	// (host=NULL) として lookup される。
	avatarURL := "https://cdn.example/u3.png"
	repo := &stubAvatarLookup{users: map[string]*model.User{
		"carol|": {ID: "u3", Username: "carol", AvatarURL: &avatarURL},
	}}
	h := avatarHandler(nil, repo, "go.example")

	c, rec := newAvatarTestContext(t, "carol@go.example")
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, avatarURL, rec.Header().Get(echo.HeaderLocation))
}

func TestAvatarHandler_FallsBackToIdenticonWhenNoAvatar(t *testing.T) {
	repo := &stubAvatarLookup{users: map[string]*model.User{
		"dan|": {ID: "u4", Username: "dan"}, // AvatarURL == nil
	}}
	h := avatarHandler(nil, repo, "go.example")

	c, rec := newAvatarTestContext(t, "dan")
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/identicon/dan", rec.Header().Get(echo.HeaderLocation))
}

func TestAvatarHandler_UnknownUserRedirectsToStaticFallback(t *testing.T) {
	repo := &stubAvatarLookup{users: map[string]*model.User{}}
	h := avatarHandler(nil, repo, "go.example")

	c, rec := newAvatarTestContext(t, "ghost")
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, avatarStaticFallback, rec.Header().Get(echo.HeaderLocation))
}

func TestAvatarHandler_SuspendedUserHidden(t *testing.T) {
	avatarURL := "https://cdn.example/banned.png"
	repo := &stubAvatarLookup{users: map[string]*model.User{
		"banned|": {ID: "u5", Username: "banned", AvatarURL: &avatarURL, IsSuspended: true},
	}}
	h := avatarHandler(nil, repo, "go.example")

	c, rec := newAvatarTestContext(t, "banned")
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	// suspended は user-unknown へ。avatarUrl は露出させない。
	assert.Equal(t, avatarStaticFallback, rec.Header().Get(echo.HeaderLocation))
}

func TestAvatarHandler_RepoErrorFallback(t *testing.T) {
	repo := &stubAvatarLookup{err: errors.New("db down")}
	h := avatarHandler(nil, repo, "go.example")

	c, rec := newAvatarTestContext(t, "alice")
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, avatarStaticFallback, rec.Header().Get(echo.HeaderLocation))
}

func TestAvatarHandler_EmptyAcctFallback(t *testing.T) {
	repo := &stubAvatarLookup{}
	h := avatarHandler(nil, repo, "go.example")

	// Echo router strips the leading `@` via `:acct`、handler は acct ""
	// を受けたら即 fallback する。
	c, rec := newAvatarTestContext(t, "")
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, avatarStaticFallback, rec.Header().Get(echo.HeaderLocation))
}

// `parseAcct` は `users.ParseAcct` に一本化したので、テストも
// `internal/api/users/explore_test.go` の `TestParseAcct` に集約した (#2791)。
// avatar 経路は構築時に localHost を小文字化して渡す (avatar.go)。

// newAvatarStaticContext builds a request with `?static=1` (#2908).
func newAvatarStaticContext(t *testing.T, acct string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/avatar/@"+acct+"?static=1", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath("/avatar/@:acct")
	c.SetParamNames("acct")
	c.SetParamValues(acct)
	return c, rec
}

// #2908: `?static=1` はリモートアバターを静止画プロキシへ回す。
//
// **frontend がこの形で叩けるようにするのが目的。** それまで frontend は
// `getStaticImageUrl('/avatar/@u@h')` を呼んでおり、それは
// `<mediaProxy>/static.webp?url=<instance>/avatar/@u@h&static=1` になる。
// mk-go の proxy は allowlist が DB に実在する URL だけを通すので、その URL は
// 403 + `max-age=86400` になり、静止画になるどころか 1 日壊れていた。
func TestAvatarHandler_StaticGoesThroughProxy(t *testing.T) {
	pk := withMediaProxy(t)
	avatarURL := "https://remote.example/avatars/bob.gif"
	remote := "remote.example"
	repo := &stubAvatarLookup{users: map[string]*model.User{
		"bob|remote.example": {ID: "u2", Username: "bob", Host: &remote, AvatarURL: &avatarURL},
	}}
	h := avatarHandler(pk, repo, "go.example")

	c, rec := newAvatarStaticContext(t, "bob@remote.example")
	require.NoError(t, h(c))
	require.Equal(t, http.StatusFound, rec.Code)

	loc, err := neturl.Parse(rec.Header().Get(echo.HeaderLocation))
	require.NoError(t, err)
	assert.Equal(t, "https://local.example/proxy/avatar.webp", loc.Scheme+"://"+loc.Host+loc.Path,
		"media proxy の base を使うこと")
	assert.Equal(t, avatarURL, loc.Query().Get("url"))
	assert.Equal(t, "1", loc.Query().Get("static"), "static を落とすと設定が無視される")
	assert.Equal(t, "1", loc.Query().Get("avatar"), "avatar のリサイズ寸法を保つこと")
	assert.NotEmpty(t, loc.Query().Get("sig"),
		"sig が無いと Authorize が DB allowlist に落ちる (mention chip はホットパス)")
}

// **identicon は static でもプロキシへ回さない (#2908)。**
//
// PNG を生成して返すのでアニメーションせず、回す理由が無い。回すと `mediaproxy.Fetch`
// が自分自身へ HTTP で取りに行き、自分のホストが private IP に解決される構成
// (`allowedPrivateNetworks` で許していないもの) では SSRF の防御で 403 になる (相対 URL だった #4095 より前は 400 + `max-age=86400`)。
func TestAvatarHandler_StaticKeepsIdenticonDirect(t *testing.T) {
	pk := withMediaProxy(t)
	repo := &stubAvatarLookup{users: map[string]*model.User{
		"dave|": {ID: "u4", Username: "dave"},
	}}
	h := avatarHandler(pk, repo, "go.example")

	c, rec := newAvatarStaticContext(t, "dave")
	require.NoError(t, h(c))
	require.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "https://local.example/identicon/dave@local.example", rec.Header().Get(echo.HeaderLocation),
		"自分の identicon はプロキシへ回さない")
}

// 他のインスタンスの identicon は向こうの生成物なので、静止画にするならプロキシへ回す。
// 自分の identicon を外す判定 (#4095) が、/identicon/ の付く URL をすべて外していないこと。
func TestAvatarHandler_StaticProxiesRemoteIdenticon(t *testing.T) {
	pk := withMediaProxy(t)
	host := "remote.example"
	avatarURL := "https://remote.example/identicon/erin@remote.example"
	repo := &stubAvatarLookup{users: map[string]*model.User{
		"erin|remote.example": {ID: "u6", Username: "erin", Host: &host, AvatarURL: &avatarURL},
	}}
	h := avatarHandler(pk, repo, "local.example")

	c, rec := newAvatarStaticContext(t, "erin@remote.example")
	require.NoError(t, h(c))
	require.Equal(t, http.StatusFound, rec.Code)
	loc := rec.Header().Get(echo.HeaderLocation)
	assert.True(t, strings.HasPrefix(loc, "https://local.example/proxy/"), "got %q", loc)
	assert.Contains(t, loc, "static=1")
}

// /avatar の fallback は、API の avatarUrl と同じ URL にする。種が違うと別の絵になる (#4095)。
func TestAvatarHandler_FallbackMatchesAPIAvatarURL(t *testing.T) {
	pk := withMediaProxy(t)
	user := &model.User{ID: "u7", Username: "Frank"}
	repo := &stubAvatarLookup{users: map[string]*model.User{"frank|": user}}
	h := avatarHandler(pk, repo, "local.example")

	c, rec := newAvatarTestContext(t, "frank")
	require.NoError(t, h(c))
	require.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, pk.IdenticonURL(user), rec.Header().Get(echo.HeaderLocation))
	assert.Equal(t, "https://local.example/identicon/frank@local.example", rec.Header().Get(echo.HeaderLocation))
}

// **同一オリジンのアバターもプロキシへ回す (#2908)。**
//
// ローカルの drive アバターも GIF / APNG / animated WebP になりうるので、
// リモートに限ると静止画設定 (disableShowingAnimatedImages / dataSaver.avatar) が
// ローカルユーザーにだけ効かない。allowlist は関係ない — `StaticAvatarProxyURL` は
// 必ず `sig` を付け、`Authorize` は allowlist より先に HMAC を見る。
func TestAvatarHandler_StaticProxiesSameOriginAvatar(t *testing.T) {
	pk := withMediaProxy(t)
	avatarURL := "https://local.example/files/abc.gif"
	repo := &stubAvatarLookup{users: map[string]*model.User{
		"carol|": {ID: "u5", Username: "carol", AvatarURL: &avatarURL},
	}}
	h := avatarHandler(pk, repo, "local.example")

	c, rec := newAvatarStaticContext(t, "carol")
	require.NoError(t, h(c))
	require.Equal(t, http.StatusFound, rec.Code)

	loc, err := neturl.Parse(rec.Header().Get(echo.HeaderLocation))
	require.NoError(t, err)
	assert.Equal(t, "https://local.example/proxy/avatar.webp", loc.Scheme+"://"+loc.Host+loc.Path,
		"同一オリジンでも静止画にするにはプロキシを通す必要がある")
	assert.Equal(t, avatarURL, loc.Query().Get("url"))
	assert.Equal(t, "1", loc.Query().Get("static"))
	assert.NotEmpty(t, loc.Query().Get("sig"),
		"sig が無いと Authorize が DB allowlist に落ちる")
}

// static が無ければ従来どおり (回帰していないこと)。
func TestAvatarHandler_WithoutStaticUnchanged(t *testing.T) {
	pk := withMediaProxy(t)
	avatarURL := "https://remote.example/avatars/bob.gif"
	remote := "remote.example"
	repo := &stubAvatarLookup{users: map[string]*model.User{
		"bob|remote.example": {ID: "u2", Username: "bob", Host: &remote, AvatarURL: &avatarURL},
	}}
	h := avatarHandler(pk, repo, "go.example")

	c, rec := newAvatarTestContext(t, "bob@remote.example")
	require.NoError(t, h(c))
	require.Equal(t, http.StatusFound, rec.Code)

	loc, err := neturl.Parse(rec.Header().Get(echo.HeaderLocation))
	require.NoError(t, err)
	assert.Equal(t, "", loc.Query().Get("static"), "static を付けないこと")
	assert.Equal(t, "1", loc.Query().Get("avatar"))
}
