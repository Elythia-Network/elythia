package users_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/model"
)

// stubInAppNotifier records the reports report-abuse hands to the notifier.
type stubInAppNotifier struct {
	reports []*model.AbuseUserReport
}

func (s *stubInAppNotifier) NotifyNewReport(_ context.Context, report *model.AbuseUserReport) {
	s.reports = append(s.reports, report)
}

// #2868: 通報はモデレーターの通知欄にも残る。誰に何件作るか・連打をどう絞るか
// (#3200) は core/abuse.InAppNotifier の責務で、handler は保存した通報を渡す。
//
// **admin stream (#1549) では足りない。** あちらはその瞬間に管理画面を開いて
// いる人にしか届かない。
func TestReportAbuse_HandsReportToInAppNotifier(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	notifier := &stubInAppNotifier{}
	h.SetAbuseReportFanout(stubModeratorLister{mods: []*model.User{{ID: "mod1"}, {ID: "mod2"}}}, &stubAbuseNotifier{})
	h.SetAbuseReportInAppNotifier(notifier)

	rec := postExtra(h.ReportAbuse, `{"userId":"u2","comment":"spam"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, notifier.reports, 1, "通報 1 件につき 1 回渡す (モデレーターごとの展開は notifier 側)")

	r := notifier.reports[0]
	assert.NotEmpty(t, r.ID, "保存済みの通報を渡す")
	assert.Equal(t, "u1", r.ReporterID)
	assert.Equal(t, "u2", r.TargetUserID)
}

// admin stream 未配線でも in-app 通知だけは作る。
//
// 早期 return の条件を admin stream の配線に掛けると、片方だけ配線した構成で
// 無言で no-op になる。
func TestReportAbuse_InAppNotificationWithoutAdminStream(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	notifier := &stubInAppNotifier{}
	h.SetAbuseReportInAppNotifier(notifier)

	rec := postExtra(h.ReportAbuse, `{"userId":"u2","comment":"spam"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, notifier.reports, 1)
}

// 未配線なら通知を作らない (旧挙動)。
func TestReportAbuse_NoInAppNotifierIsNoop(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	adminNotifier := &stubAbuseNotifier{}
	h.SetAbuseReportFanout(stubModeratorLister{mods: []*model.User{{ID: "mod1"}}}, adminNotifier)

	rec := postExtra(h.ReportAbuse, `{"userId":"u2","comment":"spam"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, adminNotifier.calls, 1, "admin stream 側は従来どおり動く")
}
