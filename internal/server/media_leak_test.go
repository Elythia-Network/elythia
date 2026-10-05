package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
)

// withMediaProxy installs a media URL context for the duration of the test.
func withMediaProxy(t *testing.T) {
	t.Helper()
	entity.SetMediaURLContext(entity.NewMediaURLContext(
		"https://local.example", "https://local.example/proxy",
		[]byte("test-secret"), false, true))
	t.Cleanup(func() { entity.SetMediaURLContext(nil) })
}

// leakUserRepo serves a single user for the avatar handler.
type leakUserRepo struct{ user *model.User }

func (r leakUserRepo) FindByUsernameLower(string, *string) (*model.User, error) {
	return r.user, nil
}

func strPtrLeak(s string) *string { return &s }

// `/avatar/@acct` の redirect 先はメディアプロキシ経由になる (#2425)。
//
// **ここが生の URL だと、mention chip を 1 つ表示するだけで閲覧者の IP と
// リファラが相手インスタンスへ渡る。** API 応答側は既に leak-safe だったのに、
// この endpoint だけ素通しだった。
func TestAvatarHandler_RemoteAvatarGoesThroughProxy(t *testing.T) {
	withMediaProxy(t)
	host := "remote.example"
	repo := leakUserRepo{user: &model.User{
		ID: "u1", Username: "bob", Host: &host,
		AvatarURL: strPtrLeak("https://remote.example/files/avatar.png"),
	}}

	e := echo.New()
	e.GET("/avatar/@:acct", avatarHandler(repo, "local.example"))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/avatar/@bob@remote.example", nil))

	require.Equal(t, http.StatusFound, rec.Code)
	loc := rec.Header().Get(echo.HeaderLocation)
	assert.True(t, strings.HasPrefix(loc, "https://local.example/proxy"),
		"リモート avatar は自オリジンのプロキシへ飛ばす (got %q)", loc)
	assert.NotContains(t, loc, "remote.example/files",
		"生のリモート URL を Location に入れない")
}

// ローカル user の avatar は書き換えない。同一オリジンなので包む意味が無く、
// 包むと無駄な往復が 1 段増える。
func TestAvatarHandler_LocalAvatarUnchanged(t *testing.T) {
	withMediaProxy(t)
	repo := leakUserRepo{user: &model.User{
		ID: "u1", Username: "alice",
		AvatarURL: strPtrLeak("https://local.example/files/avatar.png"),
	}}

	e := echo.New()
	e.GET("/avatar/@:acct", avatarHandler(repo, "local.example"))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/avatar/@alice", nil))

	require.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "https://local.example/files/avatar.png", rec.Header().Get(echo.HeaderLocation))
}

// avatar 未設定は identicon への相対 URL。プロキシに包まれてはいけない。
func TestAvatarHandler_IdenticonFallbackUnchanged(t *testing.T) {
	withMediaProxy(t)
	repo := leakUserRepo{user: &model.User{ID: "u1", Username: "alice"}}

	e := echo.New()
	e.GET("/avatar/@:acct", avatarHandler(repo, "local.example"))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/avatar/@alice", nil))

	require.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/identicon/u1", rec.Header().Get(echo.HeaderLocation))
}

// `/emoji/:path` の redirect 先もメディアプロキシ経由 (#2425)。
//
// upstream `ServerService.ts` も `${mediaProxy}/emoji.webp` へ飛ばしており、
// **生の URL を返していた mk-go の方が乖離していた。** リアクションを 1 つ表示
// するだけで閲覧者の IP が相手インスタンスへ渡っていた。
func TestEmojiRedirectHandler_RemoteEmojiGoesThroughProxy(t *testing.T) {
	withMediaProxy(t)
	repo := &stubEmojiLookup{emojis: map[string]*model.Emoji{
		"smile|remote.example": {Name: "smile", PublicURL: "https://remote.example/files/smile.png"},
	}}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "smile@remote.example.webp", "")
	require.NoError(t, h(c))

	require.Equal(t, http.StatusFound, rec.Code)
	loc := rec.Header().Get(echo.HeaderLocation)
	assert.True(t, strings.HasPrefix(loc, "https://local.example/proxy"),
		"リモート emoji は自オリジンのプロキシへ飛ばす (got %q)", loc)
	assert.NotContains(t, loc, "remote.example/files")
}

// ローカル emoji は書き換えない。
func TestEmojiRedirectHandler_LocalEmojiUnchanged(t *testing.T) {
	withMediaProxy(t)
	repo := &stubEmojiLookup{emojis: map[string]*model.Emoji{
		"smile|": {Name: "smile", PublicURL: "https://local.example/files/smile.png"},
	}}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "smile.webp", "")
	require.NoError(t, h(c))

	require.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "https://local.example/files/smile.png", rec.Header().Get(echo.HeaderLocation))
}

// context 未設定 (proxy 無効構成) では素通し。ここで空文字を返すと画像が
// 全部壊れるので、**書き換えられないなら元の URL を返す**のが正しい。
func TestMediaLeak_WithoutContextPassesThrough(t *testing.T) {
	entity.SetMediaURLContext(nil)
	assert.Equal(t, "https://remote.example/a.png",
		entity.ProxyAvatarURLString("https://remote.example/a.png"))
	assert.Equal(t, "https://remote.example/a.png",
		entity.ProxyEmojiURLString("https://remote.example/a.png"))
}
