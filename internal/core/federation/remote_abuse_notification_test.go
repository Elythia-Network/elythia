package federation_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

type stubRemoteInAppNotifier struct{ reports []*model.AbuseUserReport }

func (s *stubRemoteInAppNotifier) NotifyNewReport(_ context.Context, report *model.AbuseUserReport) {
	s.reports = append(s.reports, report)
}

// #2868: リモートからの通報 (AP Flag) もモデレーターの通知欄に出す。
//
// **local の report-abuse だけ通知するのは非対称。** どちらも同じ
// abuse_user_report 行として管理画面には出るので、通知が来ないことだけが
// 症状になる。
func TestProcess_FlagNotifiesModerators(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	idGenFlag, _ := id.NewGenerator("aidx")
	p.SetAbuseReportRepo(abuseRepo, idGenFlag)
	notifier := &stubRemoteInAppNotifier{}
	p.SetAbuseReportNotification(notifier)

	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	body := []byte(`{
		"type": "Flag",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob",
		"content": "abuse"
	}`)
	require.NoError(t, p.Process(body))
	require.Len(t, abuseRepo.Reports, 1)
	require.Len(t, notifier.reports, 1, "通報 1 件につき 1 回渡す (モデレーターごとの展開と絞りは notifier 側)")

	r := notifier.reports[0]
	assert.Equal(t, "bob", r.TargetUserID)
	require.NotNil(t, r.ReporterHost, "ホスト単位の絞り (#3200) に reporterHost が要る")
	assert.Equal(t, "remote.example", *r.ReporterHost)
	assert.Same(t, abuseRepo.Reports[r.ID], r, "保存済みの通報を渡す")
}

// 未配線なら通知を作らない (旧挙動)。
func TestProcess_FlagWithoutNotifierIsNoop(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	idGenFlag, _ := id.NewGenerator("aidx")
	p.SetAbuseReportRepo(abuseRepo, idGenFlag)

	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	body := []byte(`{
		"type": "Flag",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob",
		"content": "abuse"
	}`)
	require.NoError(t, p.Process(body))
	assert.Len(t, abuseRepo.Reports, 1, "通知の配線が無くても通報自体は保存する")
}
