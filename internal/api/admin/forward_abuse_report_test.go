package admin_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reportId は本家の paramDef で required かつ format misskey:id なので、
// 欠落・型違い・空文字・形式違反は ajv と同じ INVALID_PARAM (400) になり、
// 通報を引かず転送もしない (#3330)。
func TestForwardAbuseUserReport_InvalidReportID(t *testing.T) {
	for _, tt := range []struct {
		name   string
		body   string
		param  string
		reason string
	}{
		{"missing", `{}`, "#/required", "must have required property 'reportId'"},
		{"null", `{"reportId":null}`, "#/properties/reportId/type", "must be string"},
		{"number", `{"reportId":1}`, "#/properties/reportId/type", "must be string"},
		{"empty", `{"reportId":""}`, "#/properties/reportId/format", `must match format "misskey:id"`},
		{"bad format", `{"reportId":"r-1"}`, "#/properties/reportId/format", `must match format "misskey:id"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := setupAbuseReportHandler(t, remoteAbuseReport())
			stub := &stubAbuseForwarder{}
			h.SetAbuseForwarder(stub)
			rec := doPost(h.ForwardAbuseUserReport, tt.body, adminUser)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			var resp struct {
				Error struct {
					Code string `json:"code"`
					ID   string `json:"id"`
					Info struct {
						Param  string `json:"param"`
						Reason string `json:"reason"`
					} `json:"info"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, "INVALID_PARAM", resp.Error.Code)
			assert.Equal(t, "3d81ceae-475f-4600-b2a8-2bc116157532", resp.Error.ID)
			assert.Equal(t, tt.param, resp.Error.Info.Param)
			assert.Equal(t, tt.reason, resp.Error.Info.Reason)
			assert.Empty(t, stub.calledWith)
		})
	}
}

type stubAbuseForwarder struct {
	calledWith string
	err        error
}

func (s *stubAbuseForwarder) ForwardReport(reportID string) error {
	s.calledWith = reportID
	return s.err
}

// remoteAbuseReport is a forwardable report (remote target, not yet forwarded).
func remoteAbuseReport() *model.AbuseUserReport {
	host := "remote.example"
	return &model.AbuseUserReport{ID: "r1", TargetUserID: "u1", ReporterID: "u2", TargetUserHost: &host}
}

func TestForwardAbuseUserReport_UsesForwarderWhenWired(t *testing.T) {
	h, _ := setupAbuseReportHandler(t, remoteAbuseReport())
	stub := &stubAbuseForwarder{}
	h.SetAbuseForwarder(stub)
	rec := doPost(h.ForwardAbuseUserReport, `{"reportId":"r1"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "r1", stub.calledWith)
}

func TestForwardAbuseUserReport_ForwarderError(t *testing.T) {
	h, _ := setupAbuseReportHandler(t, remoteAbuseReport())
	h.SetAbuseForwarder(&stubAbuseForwarder{err: assertError{}})
	rec := doPost(h.ForwardAbuseUserReport, `{"reportId":"r1"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- moderation log assertion (#664) ---

func TestForwardAbuseUserReport_WritesModerationLog(t *testing.T) {
	// forwarder 未配線 fallback path で abuseRepo の DB フラグが立った時に
	// forwardAbuseReport log が書かれることを確認。target は remote (host あり)
	// でないと local-target guard で弾かれる。
	host := "remote.example"
	h, _ := setupAbuseReportHandler(t,
		&model.AbuseUserReport{ID: "r1", TargetUserID: "u1", ReporterID: "u2", TargetUserHost: &host},
	)
	repo := attachModLog(t, h)

	rec := doPost(h.ForwardAbuseUserReport, `{"reportId":"r1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "forwardAbuseReport", repo.Snapshot()[0].Type)
}

// ローカル対象 (targetUserHost == null) は forward 不可で 400 (本家は 500。意図的な差、docs/divergence.md)。
func TestForwardAbuseUserReport_RejectsLocalTarget(t *testing.T) {
	h, _ := setupAbuseReportHandler(t,
		&model.AbuseUserReport{ID: "r1", TargetUserID: "u1", ReporterID: "u2"},
	)
	stub := &stubAbuseForwarder{}
	h.SetAbuseForwarder(stub)
	rec := doPost(h.ForwardAbuseUserReport, `{"reportId":"r1"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
	assert.Empty(t, stub.calledWith)
}

// abuseRepo wired で report が存在しなければ NO_SUCH_ABUSE_REPORT (404)。
func TestForwardAbuseUserReport_NotFound(t *testing.T) {
	h, _ := setupAbuseReportHandler(t) // abuseRepo wired, report 無し
	rec := doPost(h.ForwardAbuseUserReport, `{"reportId":"ghost"}`, adminUser)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_ABUSE_REPORT")
	assert.Contains(t, rec.Body.String(), "8763e21b-d9bc-40be-acf6-54c1a6986493")
}

// 既に forwarded 済みの report は再 forward 不可で 400 (本家は 500。意図的な差、docs/divergence.md)。
func TestForwardAbuseUserReport_RejectsAlreadyForwarded(t *testing.T) {
	host := "remote.example"
	h, _ := setupAbuseReportHandler(t,
		&model.AbuseUserReport{ID: "r1", TargetUserID: "u1", ReporterID: "u2", TargetUserHost: &host, Forwarded: true},
	)
	stub := &stubAbuseForwarder{}
	h.SetAbuseForwarder(stub)
	rec := doPost(h.ForwardAbuseUserReport, `{"reportId":"r1"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
	assert.Empty(t, stub.calledWith)
}

// abuseRepo が未配線だと存在を確かめられないので、204 ではなく
// NO_SUCH_ABUSE_REPORT (404) を返す。forwarder が配線済みでも呼ばない (#3330)。
func TestForwardAbuseUserReport_NoAbuseRepoIsNotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubAbuseForwarder{}
	h.SetAbuseForwarder(stub)
	rec := doPost(h.ForwardAbuseUserReport, `{"reportId":"r1"}`, adminUser)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_ABUSE_REPORT")
	assert.Contains(t, rec.Body.String(), "8763e21b-d9bc-40be-acf6-54c1a6986493")
	assert.Empty(t, stub.calledWith)
}

// forwarder 未配線の fallback で forwarded の更新が失敗したら、204 ではなく
// INTERNAL_ERROR (500) を返し、moderation log も書かない
// (upstream AbuseReportService.forward は update の例外をそのまま投げる) (#3330)。
func TestForwardAbuseUserReport_FallbackUpdateErrorIsInternalError(t *testing.T) {
	h, _ := setupAbuseReportHandler(t)
	repo := &failingAbuseUpdateRepo{testutil.NewMockAbuseReportRepository()}
	require.NoError(t, repo.Create(remoteAbuseReport()))
	h.SetAbuseRepo(repo)
	modLogs := attachModLog(t, h)

	rec := doPost(h.ForwardAbuseUserReport, `{"reportId":"r1"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "INTERNAL_ERROR")
	assert.Never(t, func() bool { return len(modLogs.Snapshot()) > 0 }, 100*time.Millisecond, 5*time.Millisecond)
}

// fallback の成功時は forwarded=true を立てて 204 を返す。
func TestForwardAbuseUserReport_FallbackMarksForwarded(t *testing.T) {
	h, repo := setupAbuseReportHandler(t, remoteAbuseReport())
	rec := doPost(h.ForwardAbuseUserReport, `{"reportId":"r1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, repo.Reports["r1"].Forwarded)
}
