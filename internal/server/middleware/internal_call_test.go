package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

type recordingObserver struct{ calls []string }

func (r *recordingObserver) Record(userID, ip string) {
	r.calls = append(r.calls, userID+"@"+ip)
}

func TestIsInternalCall(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/api/i", nil)
	require.False(t, IsInternalCall(req.Context()), "素のリクエストは内部呼び出しではない")

	marked := MarkInternalCall(req)
	require.True(t, IsInternalCall(marked.Context()))
	require.False(t, IsInternalCall(req.Context()), "元のリクエストは変わらないこと")

	// nil を渡して落ちないことがこのアサーションの主題。
	//nolint:staticcheck // SA1012: nil Context を渡す挙動そのものを固定している
	require.False(t, IsInternalCall(nil), "nil context でも落ちないこと")
}

// **プロセス内の呼び出しでは IP を記録しないこと。**
//
// プラグインの `AsUser` は実在しない `127.0.0.1` を名乗る。記録すると
// `admin/ip/related-accounts` が、そのプラグインを使った全員を互いの関連候補
// として最上位に並べる (観測時刻が常に「今」なので時間減衰の重みが最大)。
func TestRecordClientIP_SkipsInternalCalls(t *testing.T) {
	t.Parallel()

	run := func(internal bool) []string {
		obs := &recordingObserver{}
		e := echo.New()
		req := httptest.NewRequest(http.MethodPost, "/api/i", nil)
		req.RemoteAddr = "127.0.0.1:0"
		if internal {
			req = MarkInternalCall(req)
		}
		c := e.NewContext(req, httptest.NewRecorder())
		c.Set(string(UserContextKey), &model.User{ID: "u1"})
		h := RecordClientIP(obs)(func(echo.Context) error { return nil })
		_ = h(c)
		return obs.calls
	}

	require.Empty(t, run(true), "内部呼び出しは記録しないこと")
	require.Equal(t, []string{"u1@127.0.0.1"}, run(false),
		"通常のリクエストは従来どおり記録すること")
}
