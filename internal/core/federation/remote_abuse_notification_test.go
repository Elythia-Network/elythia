package federation_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

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

// #2868 / #3256: リモートからの通報 (AP Flag) も、ローカルの通報と同じ通知
// (通知欄 / admin stream / abuseReport system webhook) を出す。
//
// **local の report-abuse だけ通知するのは非対称。** どちらも同じ
// abuse_user_report 行として管理画面には出るので、通知が来ないことだけが
// 症状になる。#3256 までは通知欄だけをここで出しており、webhook と admin
// stream が抜けていた。
func TestProcess_FlagNotifiesModerators(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	idGenFlag, _ := id.NewGenerator("aidx")
	p.SetAbuseReportRepo(abuseRepo, idGenFlag)
	notifier := &stubCreatedNotifier{}
	p.SetAbuseReportCreatedNotifier(notifier)

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

	// webhook の本文の reporter / targetUser に使う。欠けると null になる。
	require.NotNil(t, notifier.reporters[0], "通報者 (リモートの actor) を渡す")
	require.NotNil(t, notifier.reporters[0].Host)
	assert.Equal(t, "remote.example", *notifier.reporters[0].Host)
	assert.Equal(t, r.ReporterID, notifier.reporters[0].ID)
	require.NotNil(t, notifier.targets[0], "対象を渡す")
	assert.Equal(t, "bob", notifier.targets[0].ID)
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
