package notifications

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// #3200: 通報の通知はまとめているので、未対応の件数を載せて後続に気付けるようにする。
// 件数はインスタンス全体の値なので、ページで 1 回だけ引く。
func TestShow_AbuseReportCarriesUnresolvedCount(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{moderators: map[string]bool{"mod1": true}})
	wireAbuseLookup(h, true)
	calls := 0
	h.SetAbuseReportUnresolvedCounter(func() (int64, error) {
		calls++
		return 3, nil
	})
	seedAbuseReport(t, svc, "mod1")
	seedAbuseReport(t, svc, "mod1")

	out := listNotifications(t, h, "mod1")
	require.Len(t, out, 2)
	for _, n := range out {
		assert.Equal(t, true, n["resolved"])
		assert.EqualValues(t, 3, n["unresolvedCount"], "この通報が対処済みでも未対応が残っていることを出す")
	}
	assert.Equal(t, 1, calls, "件数はページで 1 回だけ引く")
}

// 未配線なら件数を出さない。0 を出すと「未対応なし」と読めてしまう。
func TestShow_AbuseReportWithoutCounterOmitsCount(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{moderators: map[string]bool{"mod1": true}})
	wireAbuseLookup(h, false)
	seedAbuseReport(t, svc, "mod1")

	out := listNotifications(t, h, "mod1")
	require.Len(t, out, 1)
	assert.NotContains(t, out[0], "unresolvedCount")
}

// 件数の取得失敗も 500 にする (#2792)。状態の lookup と同じ扱い。
func TestShow_AbuseReportCountErrorIs500(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{moderators: map[string]bool{"mod1": true}})
	wireAbuseLookup(h, false)
	h.SetAbuseReportUnresolvedCounter(func() (int64, error) { return 0, assertAnError{} })
	seedAbuseReport(t, svc, "mod1")

	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, &model.User{ID: "mod1"})
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// 通報の通知が無いページでは件数を引かない。
func TestShow_NoAbuseReportSkipsCount(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{moderators: map[string]bool{"mod1": true}})
	wireAbuseLookup(h, false)
	h.SetAbuseReportUnresolvedCounter(func() (int64, error) {
		t.Fatal("must not count without abuseReport notifications")
		return 0, nil
	})
	assert.Empty(t, listNotifications(t, h, "mod1"))
}
