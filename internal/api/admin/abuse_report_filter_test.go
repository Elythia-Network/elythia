package admin_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// #2868: reportId 指定でその 1 件だけを返す。通報の通知から該当通報へ飛ぶため。
func TestAbuseReports_ByReportID(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	abuseRepo.Reports["r1"] = &model.AbuseUserReport{ID: "r1", Comment: "spam"}
	abuseRepo.Reports["r2"] = &model.AbuseUserReport{ID: "r2", Comment: "other"}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.AbuseReports, `{"reportId":"r1"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "r1", resp[0]["id"])
	assert.Equal(t, "spam", resp[0]["comment"])
}

// **解決済みでも返す (#2868)。** state の既定は unresolved なので、絞りを
// 適用すると「他のモデレーターが先に解決した通報」に通知から飛べなくなる。
func TestAbuseReports_ByReportIDIgnoresStateFilter(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	abuseRepo.Reports["r1"] = &model.AbuseUserReport{ID: "r1", Comment: "spam", Resolved: true}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.AbuseReports, `{"reportId":"r1","state":"unresolved"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1, "reportId 指定時は state の絞りを無視する")
	assert.Equal(t, true, resp[0]["resolved"])
}

// 存在しない ID は空配列 (404 にしない — 一覧 endpoint なので)。
func TestAbuseReports_ByReportIDNotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.AbuseReports, `{"reportId":"missing"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp)
}

// reportId 未指定なら従来どおり一覧を返す (additive であることの確認)。
func TestAbuseReports_WithoutReportIDListsAll(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	abuseRepo.Reports["r1"] = &model.AbuseUserReport{ID: "r1", Comment: "spam"}
	abuseRepo.Reports["r2"] = &model.AbuseUserReport{ID: "r2", Comment: "other"}
	h.SetAbuseRepo(abuseRepo)

	rec := doPost(h.AbuseReports, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 2)
}
