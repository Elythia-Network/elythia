package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// stubEmojiLookup is a focused mock — we don't need the full
// EmojiRepository interface for the redirect handler's contract.
type stubEmojiLookup struct {
	emojis map[string]*model.Emoji // key: "name|host" (host="" for local)
	err    error
}

func (s *stubEmojiLookup) FindByNameAndHost(name string, host *string) (*model.Emoji, error) {
	if s.err != nil {
		return nil, s.err
	}
	key := name + "|"
	if host != nil {
		key += *host
	}
	e, ok := s.emojis[key]
	if !ok {
		// **repository の sentinel を返す。** 汎用 error だと #2792 の
		// 「DB 障害は 500」に引っかかる。
		return nil, repository.ErrNotFound
	}
	return e, nil
}

func newEmojiTestContext(t *testing.T, path, query string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	url := "/emoji/" + path
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath("/emoji/:path")
	c.SetParamNames("path")
	c.SetParamValues(path)
	return c, rec
}

func TestEmojiRedirectHandler_LocalEmoji(t *testing.T) {
	repo := &stubEmojiLookup{emojis: map[string]*model.Emoji{
		"smile|": {Name: "smile", PublicURL: "https://cdn.example/local-smile.png"},
	}}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "smile.webp", "")
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "https://cdn.example/local-smile.png", rec.Header().Get(echo.HeaderLocation))
	assert.Equal(t, "public, max-age=86400", rec.Header().Get(echo.HeaderCacheControl))
}

func TestEmojiRedirectHandler_LocalCanonicalDotSuffix(t *testing.T) {
	// `:smile@.:` (ReactionService.resolveReaction が永続化する形) は
	// frontend が `/emoji/smile@..webp` で投げてくる。`@.` は host=NULL
	// と等価に扱う。
	repo := &stubEmojiLookup{emojis: map[string]*model.Emoji{
		"smile|": {Name: "smile", PublicURL: "https://cdn.example/local-smile.png"},
	}}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "smile@..webp", "")
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "https://cdn.example/local-smile.png", rec.Header().Get(echo.HeaderLocation))
}

func TestEmojiRedirectHandler_RemoteEmoji(t *testing.T) {
	repo := &stubEmojiLookup{emojis: map[string]*model.Emoji{
		"meow_yikes|remote.example": {
			Name:      "meow_yikes",
			PublicURL: "https://r/meow.png",
		},
	}}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "meow_yikes@remote.example.webp", "")
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "https://r/meow.png", rec.Header().Get(echo.HeaderLocation))
}

func TestEmojiRedirectHandler_PublicURLFallbackToOriginal(t *testing.T) {
	// publicUrl 空 → originalUrl にフォールバック (upstream `||` と同等)
	repo := &stubEmojiLookup{emojis: map[string]*model.Emoji{
		"x|host.example": {Name: "x", PublicURL: "", OriginalURL: "https://r/x_orig.png"},
	}}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "x@host.example.webp", "")
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "https://r/x_orig.png", rec.Header().Get(echo.HeaderLocation))
}

func TestEmojiRedirectHandler_NotFound_404(t *testing.T) {
	repo := &stubEmojiLookup{emojis: map[string]*model.Emoji{}}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "ghost.webp", "")
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestEmojiRedirectHandler_NotFound_FallbackQuery(t *testing.T) {
	// ?fallback クエリ付きの場合は static-assets の placeholder へ 302
	repo := &stubEmojiLookup{emojis: map[string]*model.Emoji{}}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "ghost.webp", "fallback=1")
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, emojiRedirectFallback, rec.Header().Get(echo.HeaderLocation))
}

func TestEmojiRedirectHandler_BadPath_404(t *testing.T) {
	repo := &stubEmojiLookup{}
	h := emojiRedirectHandler(repo)

	cases := []string{
		"foo.png",   // 拡張子違反
		"x!y.webp",  // 記号違反 (regex の charset 外)
		"foo+.webp", // 記号違反
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			c, rec := newEmojiTestContext(t, p, "")
			require.NoError(t, h(c))
			// regex で弾かれるか、guard で 4xx
			assert.Contains(t, []int{http.StatusNotFound, http.StatusBadRequest}, rec.Code,
				"unexpected status %d for path %q", rec.Code, p)
		})
	}
}

func TestEmojiRedirectHandler_BadAtChunks_400(t *testing.T) {
	// regex は `[a-zA-Z0-9\-_@\.]+?\.webp$` なので "a@b@c.webp" は通る。
	// path split 後に chunks > 2 で 400 を返す。
	repo := &stubEmojiLookup{}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "a@b@c.webp", "")
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// **DB 障害は 404 ではなく 500** (#2792)。以前はここが
// `TestEmojiRedirectHandler_RepoErrorTreatedAsNotFound` で「db down でも 404」を
// 固定していたが、それだと接続断が「そんな絵文字は無い」に化けたまま気付けない。
func TestEmojiRedirectHandler_DBFailureIs500(t *testing.T) {
	repo := &stubEmojiLookup{err: errors.New("db down")}
	h := emojiRedirectHandler(repo)

	// **`e.ServeHTTP` を通す。** handler の戻り値だけを見ると、echo が実際に
	// 書き出す status と header が確かめられない。ここでは 500 に 1 日の
	// キャッシュが載らないことまで固定したい。
	e := echo.New()
	e.GET("/emoji/:path", h)
	req := httptest.NewRequest(http.MethodGet, "/emoji/smile.webp", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	// **500 をキャッシュさせない** (#2792)。上流で `public, max-age=86400` を
	// 張っているので、打ち消さないと復旧後もその絵文字だけ壊れて見える。
	assert.Equal(t, "no-store", rec.Header().Get(echo.HeaderCacheControl))
}

// not-found は従来どおり 404。
func TestEmojiRedirectHandler_NotFoundIs404(t *testing.T) {
	repo := &stubEmojiLookup{err: repository.ErrNotFound}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "smile.webp", "")
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestEmojiRedirectHandler_EmptyURLs_404(t *testing.T) {
	// publicUrl も originalUrl も空の row は redirect 先が無いので 404。
	repo := &stubEmojiLookup{emojis: map[string]*model.Emoji{
		"empty|": {Name: "empty", PublicURL: "", OriginalURL: ""},
	}}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "empty.webp", "")
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// upstream (`ServerService.ts` の /emoji/:path) が付けている CSP を揃える (#2404)。
//
// **この応答は redirect なので CSP は実際には効かない** (ブラウザは最終的な応答の
// header を適用する)。upstream も redirect の前に付けており同じ状態で、header 集合を
// 一致させるためのもの。効果を期待して置いているわけではない。
func TestEmojiRedirectHandler_SetsAssetCSP(t *testing.T) {
	repo := &stubEmojiLookup{emojis: map[string]*model.Emoji{
		"smile|": {Name: "smile", PublicURL: "https://cdn.example/local-smile.png"},
	}}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "smile.webp", "")
	require.NoError(t, h(c))
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "default-src 'none'; style-src 'unsafe-inline'",
		rec.Header().Get("Content-Security-Policy"))
}

// #2905: `?static=1` は raw へ 302 せず media proxy へ回す。
//
// **利用者の「アニメーション画像を再生しない」設定がここで落ちていた。**
// frontend の getStaticImageUrl は `/emoji/` を見つけると searchParams を足すだけ
// なので、raw へ 302 するとクエリごと失われて設定が無視される。
// context 未配線なら raw へ 302 する (fallback)。
//
// 本番では必ず配線されるので、これは「配線が無いときに壊れない」ことの確認。
// 署名付きプロキシ URL になることは StaticIsSigned が固定する。
func TestEmojiRedirectHandler_StaticWithoutContextFallsBackToRaw(t *testing.T) {
	repo := &stubEmojiLookup{emojis: map[string]*model.Emoji{
		"smile|": {Name: "smile", PublicURL: "https://cdn.example/anim.gif"},
	}}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "smile.webp", "static=1")
	require.NoError(t, h(c))
	require.Equal(t, http.StatusFound, rec.Code)

	assert.Equal(t, "https://cdn.example/anim.gif", rec.Header().Get(echo.HeaderLocation),
		"context 未配線では raw へ 302 する")
}

// **media proxy を配線した状態で sig が付くこと (#2905)。**
//
// `/proxy` を手で組むと sig が付かず、Authorize が HMAC ではなく DB allowlist の
// 4 テーブル UNION に落ちる。`/emoji/:path` はリアクションアイコンのホットパス
// なので毎リクエスト DB を引くことになる。**DB の瞬断が 403 + max-age=86400 で
// 1 日キャッシュされる**という更に悪い形は #3036 で潰したが
// (503 + `no-store` になった)、DB を引く回数そのものは署名を付けないと減らない。
//
// **context を配線しないと検出できない。** 未配線だと raw へ 302 するので、
// この欠陥はテストから完全に不可視だった。
func TestEmojiRedirectHandler_StaticIsSigned(t *testing.T) {
	withMediaProxy(t)
	repo := &stubEmojiLookup{emojis: map[string]*model.Emoji{
		"smile|": {Name: "smile", PublicURL: "https://cdn.example/anim.gif"},
	}}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "smile.webp", "static=1")
	require.NoError(t, h(c))
	require.Equal(t, http.StatusFound, rec.Code)

	loc, err := neturl.Parse(rec.Header().Get(echo.HeaderLocation))
	require.NoError(t, err)
	assert.Equal(t, "https://local.example/proxy/emoji.webp", loc.Scheme+"://"+loc.Host+loc.Path,
		"media proxy の base を使うこと (/proxy をハードコードしない)")
	assert.NotEmpty(t, loc.Query().Get("sig"),
		"sig が無いと Authorize が DB allowlist に落ちる (ホットパスで毎回 DB)")
	assert.Equal(t, "1", loc.Query().Get("static"))
	assert.Equal(t, "1", loc.Query().Get("emoji"))
	assert.Equal(t, "https://cdn.example/anim.gif", loc.Query().Get("url"))
}

// static が無ければ従来どおり raw へ 302 する (回帰していないこと)。
//
// **context 未配線での挙動。** 配線済みなら ProxyEmojiURLString が働いて
// 署名付きプロキシ URL になる (media_leak_test.go が固定している)。
func TestEmojiRedirectHandler_WithoutStaticKeepsDirectRedirect(t *testing.T) {
	repo := &stubEmojiLookup{emojis: map[string]*model.Emoji{
		"smile|": {Name: "smile", PublicURL: "https://cdn.example/anim.gif"},
	}}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "smile.webp", "")
	require.NoError(t, h(c))
	require.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "https://cdn.example/anim.gif", rec.Header().Get(echo.HeaderLocation))
}

// **`?badge=1` は badge モードへ回す (#2909)。**
//
// Service Worker の `create-notification.ts` がリアクションのプッシュ通知で
// `/emoji/<name>.webp?badge=1` を組み立てる。分岐が無いと badge が無視されて通常の
// 枝に落ち、96x96 の silhouette PNG ではなく**カラーの絵文字画像**が 200 で返る
// ので、SW のエラー処理を素通りして静かに違うものが出る。
func TestEmojiRedirectHandler_BadgeGoesThroughProxy(t *testing.T) {
	withMediaProxy(t)
	repo := &stubEmojiLookup{emojis: map[string]*model.Emoji{
		"smile|": {Name: "smile", PublicURL: "https://cdn.example/anim.gif"},
	}}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "smile.webp", "badge=1")
	require.NoError(t, h(c))
	require.Equal(t, http.StatusFound, rec.Code)

	loc, err := neturl.Parse(rec.Header().Get(echo.HeaderLocation))
	require.NoError(t, err)
	// upstream (`ServerService.ts`) は badge の枝だけ `emoji.png` を出す。
	// 実際に返るのも PNG (mediaproxy.processBadge) なので中身とも一致する。
	assert.Equal(t, "https://local.example/proxy/emoji.png", loc.Scheme+"://"+loc.Host+loc.Path,
		"media proxy の base と upstream のファイル名を使うこと")
	assert.Equal(t, "1", loc.Query().Get("badge"),
		"badge を落とすと proxy が emoji モードに落ちる")
	assert.Equal(t, "https://cdn.example/anim.gif", loc.Query().Get("url"))
	assert.NotEmpty(t, loc.Query().Get("sig"),
		"sig が無いと Authorize が DB allowlist に落ちる (#2905 と同じ罠)")
	assert.Empty(t, loc.Query().Get("emoji"),
		"upstream は badge の枝で emoji=1 を付けない (parseMode が emoji を先に見るので mode が奪われる)")
}

// **badge は static より優先する (#2909)。**
//
// upstream の if/else が badge を先に取り、badge の枝では `static` を一切見ない。
// badge は 96x96 の silhouette PNG 固定なので、アニメーションの有無を渡しても
// 結果が変わらない。
func TestEmojiRedirectHandler_BadgeWinsOverStatic(t *testing.T) {
	withMediaProxy(t)
	repo := &stubEmojiLookup{emojis: map[string]*model.Emoji{
		"smile|": {Name: "smile", PublicURL: "https://cdn.example/anim.gif"},
	}}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "smile.webp", "badge=1&static=1")
	require.NoError(t, h(c))
	require.Equal(t, http.StatusFound, rec.Code)

	loc, err := neturl.Parse(rec.Header().Get(echo.HeaderLocation))
	require.NoError(t, err)
	assert.Equal(t, "/proxy/emoji.png", loc.Path, "badge の枝を通ること")
	assert.Equal(t, "1", loc.Query().Get("badge"))
	assert.Empty(t, loc.Query().Get("static"),
		"upstream は badge の枝で static を見ない")
}

// **値は問わない (存在で見る)。** upstream は fastify の `'badge' in query` なので
// `?badge=` (空値) でも badge になる。static 側の判定と揃える。
func TestEmojiRedirectHandler_BadgeWithoutValue(t *testing.T) {
	withMediaProxy(t)
	repo := &stubEmojiLookup{emojis: map[string]*model.Emoji{
		"smile|": {Name: "smile", PublicURL: "https://cdn.example/anim.gif"},
	}}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "smile.webp", "badge")
	require.NoError(t, h(c))
	require.Equal(t, http.StatusFound, rec.Code)
	loc, err := neturl.Parse(rec.Header().Get(echo.HeaderLocation))
	require.NoError(t, err)
	assert.Equal(t, "/proxy/emoji.png", loc.Path)
}

// context 未配線なら従来どおり raw へ 302 する (static と同じ扱い)。
func TestEmojiRedirectHandler_BadgeWithoutContextFallsBackToRaw(t *testing.T) {
	repo := &stubEmojiLookup{emojis: map[string]*model.Emoji{
		"smile|": {Name: "smile", PublicURL: "https://cdn.example/anim.gif"},
	}}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "smile.webp", "badge=1")
	require.NoError(t, h(c))
	require.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "https://cdn.example/anim.gif", rec.Header().Get(echo.HeaderLocation))
}

// **ローカル絵文字 (同一オリジン) でも badge へ回す (#2909)。**
//
// リアクションのプッシュ通知でローカル絵文字は主要ケース。ところが隣の
// `ProxyEmojiURLString` は `isRemoteOrigin` で分岐して同一オリジンを wrap しない
// ので、「同じファイルの他の関数に揃える」形で badge にも同じ gate を足すと、
// **ローカル絵文字だけ #2909 の症状が戻る**。リモートの URL しか使わないテスト
// だとその変異が素通りするため、ここで同一オリジンを固定する。
func TestEmojiRedirectHandler_BadgeProxiesLocalEmoji(t *testing.T) {
	withMediaProxy(t)
	repo := &stubEmojiLookup{emojis: map[string]*model.Emoji{
		// ローカル絵文字の publicUrl は自インスタンスの drive URL。
		"smile|": {Name: "smile", PublicURL: "https://local.example/files/abc.png"},
	}}
	h := emojiRedirectHandler(repo)

	c, rec := newEmojiTestContext(t, "smile.webp", "badge=1")
	require.NoError(t, h(c))
	require.Equal(t, http.StatusFound, rec.Code)

	loc, err := neturl.Parse(rec.Header().Get(echo.HeaderLocation))
	require.NoError(t, err)
	assert.Equal(t, "/proxy/emoji.png", loc.Path,
		"同一オリジンでも badge にするにはプロキシを通す必要がある")
	assert.Equal(t, "1", loc.Query().Get("badge"))
	assert.Equal(t, "https://local.example/files/abc.png", loc.Query().Get("url"))
	assert.NotEmpty(t, loc.Query().Get("sig"))
}
