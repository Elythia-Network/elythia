package users_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

// stubCreatedNotifier records what report-abuse hands to the notifier.
type stubCreatedNotifier struct {
	reports   []*model.AbuseUserReport
	reporters []*model.User
	targets   []*model.User
}

func (s *stubCreatedNotifier) NotifyCreated(_ context.Context, report *model.AbuseUserReport, reporter, target *model.User) {
	s.reports = append(s.reports, report)
	s.reporters = append(s.reporters, reporter)
	s.targets = append(s.targets, target)
}

// 通報の通知 (通知欄 #2868 / admin stream #1549 / abuseReport system webhook
// #1542) は core/abuse.CreatedNotifier の責務で、handler は保存した通報と
// 通報者・対象を渡す。連合経由の Flag も同じものを呼ぶ (#3256)。
func TestReportAbuse_HandsReportToCreatedNotifier(t *testing.T) {
	h, userRepo, _ := newExtraHandler(t)
	h.SetUserRepo(userRepo)
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "target"}
	notifier := &stubCreatedNotifier{}
	h.SetAbuseReportCreatedNotifier(notifier)

	me := &model.User{ID: "u1"}
	rec := postExtra(h.ReportAbuse, `{"userId":"u2","comment":"spam"}`, me)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, notifier.reports, 1, "通報 1 件につき 1 回渡す")

	r := notifier.reports[0]
	assert.NotEmpty(t, r.ID, "保存済みの通報を渡す")
	assert.Equal(t, "u1", r.ReporterID)
	assert.Equal(t, "u2", r.TargetUserID)
	assert.Equal(t, "spam", r.Comment)
	// webhook の本文の reporter / targetUser に使う。欠けると null になる。
	assert.Same(t, me, notifier.reporters[0])
	require.NotNil(t, notifier.targets[0])
	assert.Equal(t, "u2", notifier.targets[0].ID)
}

// 未配線でも通報自体は成功する。
func TestReportAbuse_NoCreatedNotifierStillSucceeds(t *testing.T) {
	h, userRepo, _ := newExtraHandler(t)
	h.SetUserRepo(userRepo)
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "target"}
	rec := postExtra(h.ReportAbuse, `{"userId":"u2","comment":"spam"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// failingAbuseRepo fails every Create.
type failingAbuseRepo struct {
	*testutil.MockAbuseReportRepository
}

func (failingAbuseRepo) Create(*model.AbuseUserReport) error { return errors.New("db down") }

// 通報を保存できなかったら通知しない (通知が指す通報が無い)。
func TestReportAbuse_UnsavedReportIsNotNotified(t *testing.T) {
	h, userRepo, _ := newExtraHandler(t)
	h.SetUserRepo(userRepo)
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "target"}
	h.SetAbuseRepo(failingAbuseRepo{testutil.NewMockAbuseReportRepository()})
	notifier := &stubCreatedNotifier{}
	h.SetAbuseReportCreatedNotifier(notifier)

	rec := postExtra(h.ReportAbuse, `{"userId":"u2","comment":"spam"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, notifier.reports)
}

// 弾いた通報 (自分を通報) も通知しない。
func TestReportAbuse_RejectedReportIsNotNotified(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	notifier := &stubCreatedNotifier{}
	h.SetAbuseReportCreatedNotifier(notifier)
	rec := postExtra(h.ReportAbuse, `{"userId":"u1","comment":"spam"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "自分を通報")
	assert.Empty(t, notifier.reports)
}
