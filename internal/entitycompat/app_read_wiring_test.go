package entitycompat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// read:accountを持たないapp tokenを、kindも資格情報の要求も無いendpointで匿名に
// 落とすmiddlewareは、routerの1行で効く。その行が落ちてもbuildも他のテストも
// 緑のままなので、ここで固定する。
//
// **rate limiterとIPの記録より後ろに置く。** 前に置くと、狭いapp tokenの要求が
// 利用者ではなくIPで数えられる。
func TestHideAppUserWithoutReadIsWired(t *testing.T) {
	assertWired(t, routerGo,
		"api.Use(middleware.HideAppUserWithoutRead())",
		"read:accountを持たないapp tokenが、notes/showなどで本人として扱われる")

	src, err := os.ReadFile(filepath.Join(repoRoot(t), routerGo))
	require.NoError(t, err)
	s := stripGoComments(string(src))
	hide := strings.Index(s, "api.Use(middleware.HideAppUserWithoutRead())")
	limiter := strings.Index(s, "api.Use(rateLimiter.Middleware())")
	clientIP := strings.Index(s, "api.Use(middleware.RecordClientIP(ipLogService))")
	require.NotEqual(t, -1, limiter, "rate limiterの配線が見つからない")
	require.NotEqual(t, -1, clientIP, "RecordClientIPの配線が見つからない")
	assert.Greater(t, hide, limiter, "HideAppUserWithoutReadはrate limiterより後ろに置く")
	assert.Greater(t, hide, clientIP, "HideAppUserWithoutReadはRecordClientIPより後ろに置く")
}

// i/revoke-tokenは、どの権限のapp tokenでも自分自身を失効できる (本家と同じ)。
// AdmitAppUserが無いと、read:accountを持たないトークンは匿名になり401で失効できない。
func TestRevokeTokenAdmitsAppUser(t *testing.T) {
	routes := parseRouteRegistrations(t, filepath.Join(repoRoot(t), routerGo))
	reg, ok := routes["i/revoke-token"]
	require.True(t, ok, "i/revoke-tokenのrouteが見つからない")
	assert.Contains(t, stripGoComments(reg), "middleware.AdmitAppUser()",
		"read:accountを持たないapp tokenが自分自身を失効できなくなる")
}
