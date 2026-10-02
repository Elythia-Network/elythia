package admin_test

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

// abuseReportColumnKeys is the key set of upstream MiAbuseUserReport when it is
// loaded without relations (models/AbuseUserReport.ts). The moderation log
// `report` field must carry exactly these keys (#3267).
var abuseReportColumnKeys = []string{
	"assigneeId", "comment", "forwarded", "id", "moderationNote",
	"reporterHost", "reporterId", "resolved", "resolvedAs",
	"targetUserHost", "targetUserId",
}

// seedAbuseReportWithUsers returns a report whose relations are populated the
// way AbuseReportRepository.FindByID preloads them.
func seedAbuseReportWithUsers() *model.AbuseUserReport {
	host := "remote.example"
	assignee := "mod1"
	return &model.AbuseUserReport{
		ID:             "r1",
		TargetUserID:   "tgt1",
		ReporterID:     "rep1",
		AssigneeID:     &assignee,
		TargetUserHost: &host,
		ModerationNote: "old",
		TargetUser:     &model.User{ID: "tgt1", Username: "target"},
		Reporter:       &model.User{ID: "rep1", Username: "reporter"},
		Assignee:       &model.User{ID: "mod1", Username: "moderator"},
	}
}

// waitReportLog waits for the single moderation log and returns its `report`.
func waitReportLog(t *testing.T, logs *testutil.MockModerationLogRepository, wantType string) map[string]any {
	t.Helper()
	require.Eventually(t, func() bool { return len(logs.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	log := logs.Snapshot()[0]
	require.Equal(t, wantType, log.Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(log.Info, &info))
	report, ok := info["report"].(map[string]any)
	require.True(t, ok, "report must be an object: %v", info["report"])
	keys := make([]string, 0, len(report))
	for k := range report {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assert.Equal(t, abuseReportColumnKeys, keys, "report must carry only the abuse_user_report columns")
	return report
}

func TestResolveAbuseReport_LogReportHasOnlyColumns(t *testing.T) {
	h, repo := setupAbuseReportHandler(t, seedAbuseReportWithUsers())
	logs := attachModLog(t, h)

	rec := doPost(h.ResolveAbuseReport, `{"reportId":"r1","resolvedAs":"accept"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	report := waitReportLog(t, logs, "resolveAbuseReport")
	assert.Equal(t, "r1", report["id"])
	assert.Equal(t, false, report["resolved"], "report は更新前の行")
	// 写しを取っただけで、通報の行の関連は消さない (Webhook が使う)。
	assert.NotNil(t, repo.Reports["r1"].TargetUser)
}

func TestForwardAbuseUserReport_LogReportHasOnlyColumns(t *testing.T) {
	t.Run("fallback", func(t *testing.T) {
		h, _ := setupAbuseReportHandler(t, seedAbuseReportWithUsers())
		logs := attachModLog(t, h)

		rec := doPost(h.ForwardAbuseUserReport, `{"reportId":"r1"}`, adminUser)
		require.Equal(t, http.StatusNoContent, rec.Code)
		report := waitReportLog(t, logs, "forwardAbuseReport")
		assert.Equal(t, false, report["forwarded"], "report は更新前の行")
	})
	t.Run("forwarder", func(t *testing.T) {
		h, _ := setupAbuseReportHandler(t, seedAbuseReportWithUsers())
		h.SetAbuseForwarder(&stubAbuseForwarder{})
		logs := attachModLog(t, h)

		rec := doPost(h.ForwardAbuseUserReport, `{"reportId":"r1"}`, adminUser)
		require.Equal(t, http.StatusNoContent, rec.Code)
		report := waitReportLog(t, logs, "forwardAbuseReport")
		assert.Equal(t, "r1", report["id"])
	})
}

func TestUpdateAbuseUserReport_LogReportHasOnlyColumns(t *testing.T) {
	h, _ := setupAbuseReportHandler(t, seedAbuseReportWithUsers())
	logs := attachModLog(t, h)

	rec := doPost(h.UpdateAbuseUserReport, `{"reportId":"r1","moderationNote":"new"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	report := waitReportLog(t, logs, "updateAbuseReportNote")
	assert.Equal(t, "old", report["moderationNote"], "report は更新前の行")
}
