package stream

import (
	"testing"

	"github.com/stretchr/testify/require"

	corenotification "github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// WebSocket 側の pack が abuseReport の lookup を実際に渡すこと (#2868)。
//
// **router の setter を固定するだけでは足りない。** `SetAbuseReportLookup` は
// 呼ばれていても、`Pack` の中で `entity.WithAbuseReportLookup(...)` を渡さなければ
// fail-closed で realtime の通報通知が全滅する。setter だけを見る wiring gate は
// その 1 行の削除を検出できない (実測で緑のまま通った)。
//
// REST 側 (internal/api/notifications) は既存の read テストが同じ形を固定して
// いるので、ここは publisher の経路だけを見る。
func TestNotificationPublisher_PassesAbuseReportLookup(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	p := &NotificationPublisher{}
	p.SetRepos(&stubNotifUserRepo{user: &model.User{ID: "reporter", Username: "reporter"}}, nil, idGen)

	n := &corenotification.Notification{
		ID: "n1", Type: corenotification.TypeAbuseReport, NotifierID: "reporter",
		Extra: map[string]any{"reportId": "r1", "targetUserId": "u2"},
	}

	// lookup 未配線なら drop される (fail-closed)。
	require.Nil(t, p.Pack("mod1", n), "lookup 未配線で abuseReport を返している")

	var asked string
	p.SetAbuseReportLookup(func(reportID string) (entity.AbuseReportStatus, bool) {
		asked = reportID
		return entity.AbuseReportStatus{Resolved: true, ResolvedAs: "accept"}, true
	})

	packed, ok := p.Pack("mod1", n).(map[string]any)
	require.True(t, ok, "lookup を配線しても abuseReport が返らない")
	require.Equal(t, "r1", asked, "Pack が lookup に reportId を渡していない")
	require.Equal(t, true, packed["resolved"], "read 時の状態が pack されていない")
	require.Equal(t, "accept", packed["resolvedAs"])
}

// 通報が削除済みなら realtime でも drop する。
func TestNotificationPublisher_DropsDeletedAbuseReport(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	p := &NotificationPublisher{}
	p.SetRepos(&stubNotifUserRepo{user: &model.User{ID: "reporter", Username: "reporter"}}, nil, idGen)
	p.SetAbuseReportLookup(func(string) (entity.AbuseReportStatus, bool) {
		return entity.AbuseReportStatus{}, false
	})

	require.Nil(t, p.Pack("mod1", &corenotification.Notification{
		ID: "n1", Type: corenotification.TypeAbuseReport,
		Extra: map[string]any{"reportId": "gone"},
	}))
}
