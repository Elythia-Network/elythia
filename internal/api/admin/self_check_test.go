package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/selfcheck"
)

type stubSelfCheck struct {
	report selfcheck.Report
	called bool
}

func (s *stubSelfCheck) RunSelfCheck(_ context.Context) selfcheck.Report {
	s.called = true
	return s.report
}

func TestSelfCheck_ReturnsReport(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubSelfCheck{report: selfcheck.Report{
		OK: false,
		Results: []selfcheck.Result{
			{Name: "webfinger", Status: selfcheck.StatusFail, Detail: "status 404", Hint: "転送設定を確認する"},
		},
	}}
	h.SetSelfCheckRunner(stub)

	rec := doPost(h.SelfCheck, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, stub.called)

	var got selfcheck.Report
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.False(t, got.OK)
	require.Len(t, got.Results, 1)
	// hint が API に出ることが本機能の価値。落とさない。
	assert.Equal(t, "転送設定を確認する", got.Results[0].Hint)
}

// 未配線なら空の結果。エラーにすると管理画面が壊れて見えるが、実際には機能が
// 無効なだけ。
func TestSelfCheck_NoRunnerReturnsEmpty(t *testing.T) {
	h, _, _, _ := newTestHandler(t)

	rec := doPost(h.SelfCheck, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var got selfcheck.Report
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.True(t, got.OK)
	assert.Empty(t, got.Results)
}

// 宛先はリクエストから指定できない。検査用 client は SSRF ガードを通さないので、
// 外から宛先を与えられる口があると SSRF になる。
func TestSelfCheck_IgnoresRequestSuppliedTarget(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubSelfCheck{report: selfcheck.Report{OK: true}}
	h.SetSelfCheckRunner(stub)

	rec := doPost(h.SelfCheck, `{"url":"http://169.254.169.254/latest/meta-data/"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	// runner は引数を取らない。body に何を入れても宛先は変わらない。
	assert.True(t, stub.called)
}
