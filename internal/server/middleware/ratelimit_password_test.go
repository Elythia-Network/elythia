package middleware

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDefaultEndpointLimits_TokenHolderCannotExhaustRecoveryEndpoints guards
// the endpoints that verify the current password against route limits.
//
// limiter は route の RequireAuth / RequireSecure より前に走り、成否に関係なく
// user bucket を消費する。ここに上限を置くと、被害者の token を持つだけの
// 第三者が `i/regenerate-token` (token 漏洩時の唯一の対処) を使い切れる。
// 総当たりは handler 側の passwordguard が照合失敗だけを数えて止める。
func TestDefaultEndpointLimits_TokenHolderCannotExhaustRecoveryEndpoints(t *testing.T) {
	paths := []string{
		"/api/i/change-password",
		"/api/i/delete-account",
		"/api/i/regenerate-token",
		"/api/i/2fa/register",
		"/api/i/2fa/unregister",
		"/api/i/2fa/register-key",
		"/api/i/2fa/key-done",
		"/api/i/2fa/remove-key",
	}
	user := &model.User{ID: "01hpasswordbruteforce00000"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			_, ok := DefaultEndpointLimits[strings.TrimPrefix(path, "/api/")]
			assert.False(t, ok, "%s に route の上限がある。token を持つ第三者が使い切れる", path)

			// store が「尽きた」を返しても limiter はこの endpoint を数えない。
			store := &mockLimitStore{results: []mockResult{
				{Info: LimitInfo{Remaining: 0, ResetMs: time.Now().Add(time.Hour).UnixMilli()}},
			}}
			rl := NewRateLimiter(store, true, DefaultEndpointLimits)
			e, h := setupEcho(rl)
			rec := doRequest(e, rl.Middleware(), h, path, user)
			assert.NotEqual(t, http.StatusTooManyRequests, rec.Code)
			assert.Empty(t, store.calls, "%s で bucket を消費している", path)
		})
	}
}

// TestDefaultEndpointLimits_RestoreStatusIsLimitedPerClient checks the limit
// on the restore status, which the server does not authenticate (#3463 (6c)):
// an anonymous request is counted on the client's IP bucket.
func TestDefaultEndpointLimits_RestoreStatusIsLimitedPerClient(t *testing.T) {
	const path = "/api/admin/backup/restore/status"
	limit, ok := DefaultEndpointLimits[strings.TrimPrefix(path, "/api/")]
	if assert.True(t, ok) {
		// 画面の読み直し (2 秒に 1 回) は止めない。
		assert.GreaterOrEqual(t, float64(limit.Max)/limit.Duration.Seconds(), 0.5)
	}
	store := &mockLimitStore{results: []mockResult{
		{Info: LimitInfo{Remaining: 0, ResetMs: time.Now().Add(time.Minute).UnixMilli()}},
	}}
	rl := NewRateLimiter(store, true, DefaultEndpointLimits)
	e, h := setupEcho(rl)
	rec := doRequest(e, rl.Middleware(), h, path, nil)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	if assert.Len(t, store.calls, 1) {
		assert.Contains(t, store.calls[0].Key, ipBucketPrefix, "an anonymous request is counted per client")
	}
}

// TestAllowIP_SharesTheBucketWithMiddleware checks that AllowIP counts on the
// same key as Middleware does for an anonymous request.
func TestAllowIP_SharesTheBucketWithMiddleware(t *testing.T) {
	const ep = "admin/backup/restore/status"
	store := &mockLimitStore{}
	rl := NewRateLimiter(store, true, DefaultEndpointLimits)
	e, h := setupEcho(rl)
	doRequest(e, rl.Middleware(), h, "/api/"+ep, nil)
	ok, _ := rl.AllowIP(context.Background(), ep, "192.168.1.100")
	assert.True(t, ok)
	if assert.Len(t, store.calls, 2) {
		assert.Equal(t, store.calls[0].Key, store.calls[1].Key)
	}
	store.results, store.idx = []mockResult{{Info: LimitInfo{Remaining: 0, ResetMs: time.Now().Add(30 * time.Second).UnixMilli()}}}, 0
	ok, retry := rl.AllowIP(context.Background(), ep, "192.168.1.100")
	assert.False(t, ok)
	assert.Positive(t, retry)
	ok, _ = rl.AllowIP(context.Background(), "admin/backup/restore/confirm", "192.168.1.100")
	assert.True(t, ok, "an endpoint without a limit")
}

// RequireIPRateLimit は `/api` の外のルート (`GET /url`) に IP の bucket を掛けること。
func TestRequireIPRateLimit_URLPreview(t *testing.T) {
	store := &mockLimitStore{}
	rl := NewRateLimiter(store, true, DefaultEndpointLimits)
	e, h := setupEcho(rl)
	mw := RequireIPRateLimit(rl, URLPreviewRateLimitKey)

	rec := doRequest(e, mw, h, "/url", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	if assert.Len(t, store.calls, 1, "`/url` の要求を数えていない") {
		assert.Contains(t, store.calls[0].Key, URLPreviewRateLimitKey)
	}

	store.results, store.idx = []mockResult{{Info: LimitInfo{Remaining: 0, ResetMs: time.Now().Add(time.Minute).UnixMilli()}}}, 0
	rec = doRequest(e, mw, h, "/url", nil)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))

	// 上限の表に載っていること (載っていないと AllowIP は素通しする)。
	limit, ok := DefaultEndpointLimits[URLPreviewRateLimitKey]
	require.True(t, ok)
	assert.Positive(t, limit.Max)

	// limiter が無い構成 (テスト用) では素通し。
	rec = doRequest(e, RequireIPRateLimit(nil, URLPreviewRateLimitKey), h, "/url", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// `GET /oauth/authorize` は匿名で外部への取得と Redis への保存を起こすので、
// 接続元ごとの上限が表に載っていること。
func TestRequireIPRateLimit_OAuthAuthorize(t *testing.T) {
	limit, ok := DefaultEndpointLimits[OAuthAuthorizeRateLimitKey]
	require.True(t, ok)
	assert.Equal(t, 10*time.Minute, limit.Duration)
	assert.Equal(t, 60, limit.Max)

	store := &mockLimitStore{}
	rl := NewRateLimiter(store, true, DefaultEndpointLimits)
	e, h := setupEcho(rl)
	rec := doRequest(e, RequireIPRateLimit(rl, OAuthAuthorizeRateLimitKey), h, "/oauth/authorize", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	if assert.Len(t, store.calls, 1, "`/oauth/authorize` の要求を数えていない") {
		assert.Contains(t, store.calls[0].Key, OAuthAuthorizeRateLimitKey)
	}
}

// プラグインのルートには、表に無くても既定の上限が掛かること。peer の受け口は除く。
func TestMiddleware_PluginRoutesGetDefaultLimit(t *testing.T) {
	store := &mockLimitStore{}
	rl := NewRateLimiter(store, true, DefaultEndpointLimits)
	e, h := setupEcho(rl)

	rec := doRequest(e, rl.Middleware(), h, "/api/plugin/status/me/set", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, store.calls, 1, "プラグインのルートを数えていない")
	assert.Contains(t, store.calls[0].Key, "plugin/status/me/set")

	store.results, store.idx = []mockResult{{Info: LimitInfo{Remaining: 0, ResetMs: time.Now().Add(time.Minute).UnixMilli()}}}, 0
	rec = doRequest(e, rl.Middleware(), h, "/api/plugin/status/me/set", nil)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)

	// peer の受け口は専用の上限があるので数えない。
	store.calls, store.results, store.idx = nil, nil, 0
	rec = doRequest(e, rl.Middleware(), h, "/api/plugin/status/_peer", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, store.calls)

	// 表が無い (disableEndpointRateLimits) なら掛けない。
	store2 := &mockLimitStore{}
	rl2 := NewRateLimiter(store2, true, nil)
	e2, h2 := setupEcho(rl2)
	rec = doRequest(e2, rl2.Middleware(), h2, "/api/plugin/status/me/set", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, store2.calls)
}
