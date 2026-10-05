package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/server/middleware"
)

// restrictedProbePaths は応答を実際に確かめる対象。**`noCORSPaths` を回さない**
// — map を縮めるとループ回数が減って素通りするため (レビュー H-1)。AP 側は id が
// 可変なので代表値を 1 つ置く。
var restrictedProbePaths = []string{
	"/api/users/following",
	"/api/users/followers",
	"/users/al9hjjx4b0ey0002/following",
	"/users/al9hjjx4b0ey0002/followers",
	"/api/signin",
	"/api/signin-flow",
	"/api/signin-with-passkey",
	"/api/signup",
	"/api/signup-pending",
	"/api/reset-password",
	"/api/request-reset-password",
}

// corsProbe runs the global CORS middleware over a stub handler and returns the
// recorded response. DB も Redis も要らない (gzip_test.go と同じ形)。
func corsProbe(t *testing.T, method, path, origin string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	e.Use(corsMiddleware())
	e.Any("/*", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	req := httptest.NewRequest(method, path, nil)
	if origin != "" {
		req.Header.Set(echo.HeaderOrigin, origin)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// TestCORS_RestrictedPathsAreExactly pins the set of protected paths.
//
// **これが無いと防御を丸ごと消してもテストが緑になる (レビュー H-1)。** 他の
// テストは `noCORSPaths` を回す形なので、**map を空にするとループが 0 周する
// だけで全部 pass する**ことを実測された。集合そのものを期待値として持つ。
//
// 守る対象を増減させたらここも更新すること。
func TestCORS_RestrictedPathsAreExactly(t *testing.T) {
	cases := []struct {
		path       string
		restricted bool
	}{
		// API 側 (フロントエンドが使う経路)
		{"/api/users/following", true},
		{"/api/users/followers", true},
		// 資格情報を扱う経路。応答を越境で読ませると、ユーザー名の存在判定・
		// パスワード正誤・2FA 有無のオラクルと、成功時のトークン奪取が
		// **訪問者の IP に分散したまま**成立する。
		{"/api/signin", true},
		{"/api/signin-flow", true},
		{"/api/signin-with-passkey", true},
		{"/api/signup", true},
		{"/api/signup-pending", true},
		{"/api/reset-password", true},
		{"/api/request-reset-password", true},
		// **兄弟は巻き込まない。**
		{"/api/signup-application/apply", false},
		{"/api/username/available", false},
		// AP のコレクション。id は可変 (レビュー M-1)
		{"/users/al9hjjx4b0ey0002/following", true},
		{"/users/al9hjjx4b0ey0002/followers", true},
		// **兄弟は巻き込まない。** `/api/users/` へ前方一致で広げる事故を落とす
		// (`api.POST("/users/...")` は 31 本ある)。
		{"/api/users/show", false},
		{"/api/users/notes", false},
		{"/api/users/search", false},
		{"/api/users/lists/list", false},
		{"/api/users/relation", false},
		{"/api/notes/create", false},
		// AP 側も同様
		{"/users/al9hjjx4b0ey0002", false},
		{"/users/al9hjjx4b0ey0002/outbox", false},
		{"/users/al9hjjx4b0ey0002/collections/featured", false},
	}
	for _, c := range cases {
		require.Equalf(t, c.restricted, corsRestrictedPath(c.path),
			"%s の扱いが想定と違う", c.path)
	}
}

// **兄弟のエンドポイントが巻き込まれていないことを、実際の応答でも見る。**
// 上の表は述語だけを見ているので、配線側で広がった場合に備える。
func TestCORS_SiblingUsersEndpointsUnchanged(t *testing.T) {
	for _, path := range []string{"/api/users/show", "/api/users/notes", "/api/users/lists/list"} {
		rec := corsProbe(t, http.MethodPost, path, "https://example.com")
		require.Equalf(t, "*", rec.Header().Get(echo.HeaderAccessControlAllowOrigin),
			"%s が巻き込まれている。除外が広がりすぎ", path)
	}
}

// TestCORS_RelationListsRejectCrossOriginBrowsers asserts that the public
// relation lists do not advertise CORS (#2953).
//
// **越境のブラウザからフォロー一覧を一括で抜くのを止めるのが目的。** 対象は
// POST + JSON なのでプリフライトが必須で、`Access-Control-Allow-Origin` が
// 無ければブラウザは実リクエストに到達しない。
func TestCORS_RelationListsRejectCrossOriginBrowsers(t *testing.T) {
	for _, path := range restrictedProbePaths {
		t.Run(path, func(t *testing.T) {
			pre := corsProbe(t, http.MethodOptions, path, "https://example.com")
			require.Emptyf(t, pre.Header().Get(echo.HeaderAccessControlAllowOrigin),
				"%s のプリフライトが CORS を許可している。越境のブラウザから一括取得できる", path)
			// **拒否はしない (204)。** Origin を付けてくる非ブラウザの
			// クライアントをステータスで壊さないため。
			require.Equal(t, http.StatusNoContent, pre.Code,
				"プリフライトは 204 で打ち切る。通すと catchall が 200+{} を返して警告ログを吐く")

			post := corsProbe(t, http.MethodPost, path, "https://example.com")
			require.Emptyf(t, post.Header().Get(echo.HeaderAccessControlAllowOrigin),
				"%s の実リクエストが CORS を許可している", path)
			require.Equal(t, http.StatusOK, post.Code,
				"リクエスト自体は拒否しない。ヘッダを出さないだけ")
		})
	}
}

// **同一オリジンと非ブラウザは無影響。**
//
// ブラウザは同一オリジンの応答に CORS 検査を適用しないので、ヘッダが無くても
// 同梱のフロントエンドは動く。Origin を持たないクライアント (ネイティブアプリ /
// CLI / 連合) は応答ヘッダを見ない。
func TestCORS_RelationListsStillServeRequests(t *testing.T) {
	for _, path := range restrictedProbePaths {
		for _, origin := range []string{"", "https://example.com"} {
			rec := corsProbe(t, http.MethodPost, path, origin)
			require.Equalf(t, http.StatusOK, rec.Code,
				"%s (origin=%q) が拒否された。ヘッダを出さないだけにすること", path, origin)
		}
	}
}

// **`Vary: Origin` を落とさない。**
//
// **キャッシュ汚染の防止ではない (レビュー L-3)。** これらのパスは変更後どの
// Origin でも同じ応答を返すし、`/api` 側は POST なので共有キャッシュに乗らない。
// 変更前と同じヘッダ構成を保つためのもので、echo の CORS を迂回すると落ちる
// (echo は Skipper の後で `Vary` を付ける)。
func TestCORS_RelationListsKeepVaryOrigin(t *testing.T) {
	for _, path := range restrictedProbePaths {
		rec := corsProbe(t, http.MethodPost, path, "https://example.com")
		require.Containsf(t, rec.Header().Values(echo.HeaderVary), echo.HeaderOrigin,
			"%s の応答から Vary: Origin が落ちている", path)
	}
}

// **他の API は従来どおり。** 除外を広げすぎると、サードパーティの Web
// クライアントを想定外に壊す。
func TestCORS_OtherEndpointsUnchanged(t *testing.T) {
	rec := corsProbe(t, http.MethodPost, "/api/notes/create", "https://example.com")
	require.Equal(t, "*", rec.Header().Get(echo.HeaderAccessControlAllowOrigin),
		"除外が広がりすぎている。対象は noCORSPaths に列挙したものだけ")
}

// **CORS の除外とレート制限は同じ endpoint を対象にする (#2953)。**
//
// 片方だけ足すと、「越境は止めたが速度は無制限」または「速度は絞ったが越境の
// ブラウザから取り放題」という中途半端な状態になる。対になっていることを固定
// しておく。
func TestCORS_NoCORSPathsMatchRateLimitedEndpoints(t *testing.T) {
	for path := range noCORSPaths {
		key := path[len("/api/"):]
		_, ok := middleware.DefaultEndpointLimits[key]
		require.Truef(t, ok,
			"%s は CORS を絞っているのにレート制限が無い。ratelimit_defs.go にも足すこと", path)
	}
}
