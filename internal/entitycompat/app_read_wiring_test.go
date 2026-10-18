package entitycompat

import (
	"fmt"
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
	assertWired(t, serverPkg,
		"api.Use(middleware.HideAppUserWithoutRead())",
		"read:accountを持たないapp tokenが、notes/showなどで本人として扱われる")

	if err := hideAppUserOrderError(serverSources(t)); err != nil {
		t.Error(err)
	}
}

// hideAppUserOrderError reports whether HideAppUserWithoutRead is mounted
// after the rate limiter and RecordClientIP, in one file.
//
// **順序はファイルの中でだけ比べられる (#3672)。** offset はファイルごとの
// 値なので、3 つが同じファイルに無ければ前後を決められない。Echo の
// middleware の順序は登録の順で、ファイルをまたぐとそれを読み取れない。
func hideAppUserOrderError(sources []serverSource) error {
	const (
		hideUse     = "api.Use(middleware.HideAppUserWithoutRead())"
		limiterUse  = "api.Use(rateLimiter.Middleware())"
		clientIPUse = "api.Use(middleware.RecordClientIP(ipLogService))"
	)
	var file, s string
	for _, f := range sources {
		body := stripGoComments(string(f.src))
		if !strings.Contains(body, hideUse) {
			continue
		}
		if file != "" {
			return fmt.Errorf("%s が %s と %s の両方にある", hideUse, file, f.rel)
		}
		file, s = f.rel, body
	}
	if file == "" {
		return fmt.Errorf("HideAppUserWithoutReadの配線が見つからない")
	}
	hide := strings.Index(s, hideUse)
	for _, c := range []struct{ use, what string }{
		{limiterUse, "rate limiter"},
		{clientIPUse, "RecordClientIP"},
	} {
		i := strings.Index(s, c.use)
		switch {
		case i == -1:
			return fmt.Errorf("%sの配線が%sに無い (HideAppUserWithoutReadと同じファイルに置く)", c.what, file)
		case hide < i:
			return fmt.Errorf("HideAppUserWithoutReadは%sより後ろに置く (%s)", c.what, file)
		}
	}
	return nil
}

// 3 つが別々のファイルにあるとき・2 つのファイルにあるときに落とすこと
// (#3672 レビュー M-1)。
func TestHideAppUserOrderErrorRequiresOneFile(t *testing.T) {
	const (
		limiter  = "api.Use(rateLimiter.Middleware())\n"
		clientIP = "api.Use(middleware.RecordClientIP(ipLogService))\n"
		hide     = "api.Use(middleware.HideAppUserWithoutRead())\n"
	)
	src := func(rel, body string) serverSource { return serverSource{rel: rel, src: []byte(body)} }

	assert.NoError(t, hideAppUserOrderError([]serverSource{src("a.go", limiter+clientIP+hide)}))
	assert.Error(t, hideAppUserOrderError([]serverSource{src("a.go", hide+limiter+clientIP)}), "順序が逆")
	assert.Error(t, hideAppUserOrderError([]serverSource{src("a.go", limiter+clientIP), src("b.go", hide)}),
		"別々のファイルにあると前後を決められない")
	assert.Error(t, hideAppUserOrderError([]serverSource{src("a.go", limiter+clientIP+hide), src("b.go", limiter+clientIP+hide)}),
		"2 つのファイルにある (どちらも順序は正しいが、どちらが効くか決まらない)")
}

// i/revoke-tokenは、どの権限のapp tokenでも自分自身を失効できる (本家と同じ)。
// AdmitAppUserが無いと、read:accountを持たないトークンは匿名になり401で失効できない。
func TestRevokeTokenAdmitsAppUser(t *testing.T) {
	routes := parseRouteRegistrations(t, serverSourcePaths(t)...)
	reg, ok := routes["i/revoke-token"]
	require.True(t, ok, "i/revoke-tokenのrouteが見つからない")
	assert.Contains(t, stripGoComments(reg), "middleware.AdmitAppUser()",
		"read:accountを持たないapp tokenが自分自身を失効できなくなる")
}
