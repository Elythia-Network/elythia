package entity

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/notification"
)

func abuseNotification() *notification.Notification {
	return &notification.Notification{
		ID: "n1", Type: notification.TypeAbuseReport, NotifierID: "reporter",
		Extra: map[string]any{"reportId": "r1", "targetUserId": "u2"},
	}
}

// #2868: 未対応の通報は resolved=false で返る。
func TestPackNotification_AbuseReportUnresolved(t *testing.T) {
	out := PackNotification(abuseNotification(), nil, nil, nil, nil, nil,
		WithAbuseReportLookup(func(id string) (AbuseReportStatus, bool) {
			require.Equal(t, "r1", id)
			return AbuseReportStatus{}, true
		}))
	require.NotNil(t, out)
	require.Equal(t, false, out["resolved"])
	require.NotContains(t, out, "resolvedAs")
	require.NotContains(t, out, "assigneeId")
}

// **対処済みは read 時に分かる (#2868)。** 通知は作成時点の状態しか持たないので、
// 他のモデレーターが対処しても通知欄が「未対応」のまま残ってしまう。
func TestPackNotification_AbuseReportResolved(t *testing.T) {
	out := PackNotification(abuseNotification(), nil, nil, nil, nil, nil,
		WithAbuseReportLookup(func(string) (AbuseReportStatus, bool) {
			return AbuseReportStatus{Resolved: true, ResolvedAs: "accept", AssigneeID: "mod1"}, true
		}))
	require.NotNil(t, out)
	require.Equal(t, true, out["resolved"])
	require.Equal(t, "accept", out["resolvedAs"])
	require.Equal(t, "mod1", out["assigneeId"])
}

// 通報が消えていたら通知ごと drop する (roleAssigned と同じ形)。
func TestPackNotification_AbuseReportDeletedIsDropped(t *testing.T) {
	out := PackNotification(abuseNotification(), nil, nil, nil, nil, nil,
		WithAbuseReportLookup(func(string) (AbuseReportStatus, bool) {
			return AbuseReportStatus{}, false
		}))
	require.Nil(t, out, "削除済みの通報を指す通知は返さない")
}

// lookup 未配線でも drop する (fail-closed)。
//
// 状態を出せないまま「未対応」に見せると、対処済みの通報に別のモデレーターが
// 二重で当たる。
func TestPackNotification_AbuseReportWithoutLookupIsDropped(t *testing.T) {
	require.Nil(t, PackNotification(abuseNotification(), nil, nil, nil, nil, nil))
}

// reportId / targetUserId は raw で返す (frontend がボタンの遷移先に使う)。
// roleId / invitationId が packed entity に解決されて消えるのとは扱いが違う。
func TestPackNotification_AbuseReportKeepsReportID(t *testing.T) {
	out := PackNotification(abuseNotification(), nil, nil, nil, nil, nil,
		WithAbuseReportLookup(func(string) (AbuseReportStatus, bool) {
			return AbuseReportStatus{}, true
		}))
	require.NotNil(t, out)
	// **reportId は残す。** frontend が「通報を確認」ボタンの遷移先に使う。
	require.Equal(t, "r1", out["reportId"])
	require.Equal(t, "u2", out["targetUserId"])
}

// #2868 以前に積まれた通知は Extra に通報本文を持つ。read 時に落とす。
//
// **通知に本文を出さない方針を既存分にも及ぼす。** Redis stream は MaxPerUser で
// 回転するまで残るので、書き込み側を直しただけでは古い通知から本文が出続ける。
func TestPackNotification_AbuseReportDropsLegacyComment(t *testing.T) {
	n := abuseNotification()
	n.Extra["comment"] = "【違反カテゴリ】スパム\n【詳細】通報本文"

	out := PackNotification(n, nil, nil, nil, nil, nil,
		WithAbuseReportLookup(func(string) (AbuseReportStatus, bool) {
			return AbuseReportStatus{}, true
		}))
	require.NotNil(t, out)
	require.NotContains(t, out, "comment", "永続化済みの通報本文を surface している")
	require.Equal(t, "r1", out["reportId"], "他の Extra は残ること")
}

// 他の型の comment は落とさない (filter が広すぎないこと)。
func TestPackNotification_OtherTypesKeepComment(t *testing.T) {
	n := &notification.Notification{
		ID: "n2", Type: notification.TypeApp, Extra: map[string]any{"comment": "keep me"},
	}
	out := PackNotification(n, nil, nil, nil, nil, nil)
	require.NotNil(t, out)
	require.Equal(t, "keep me", out["comment"])
}

// #3200: まとめた通知から後続の通報に気付けるよう、未対応の件数を出す。
func TestPackNotification_AbuseReportUnresolvedCount(t *testing.T) {
	n := int64(7)
	out := PackNotification(abuseNotification(), nil, nil, nil, nil, nil,
		WithAbuseReportLookup(func(string) (AbuseReportStatus, bool) {
			return AbuseReportStatus{Resolved: true, UnresolvedCount: &n}, true
		}))
	require.NotNil(t, out)
	require.Equal(t, int64(7), out["unresolvedCount"])

	zero := int64(0)
	out = PackNotification(abuseNotification(), nil, nil, nil, nil, nil,
		WithAbuseReportLookup(func(string) (AbuseReportStatus, bool) {
			return AbuseReportStatus{UnresolvedCount: &zero}, true
		}))
	require.Equal(t, int64(0), out["unresolvedCount"], "0 件も件数として出す")
}

// 件数が分からないときは出さない。0 を出すと「未対応なし」と読めてしまう。
func TestPackNotification_AbuseReportUnknownCountIsOmitted(t *testing.T) {
	out := PackNotification(abuseNotification(), nil, nil, nil, nil, nil,
		WithAbuseReportLookup(func(string) (AbuseReportStatus, bool) {
			return AbuseReportStatus{}, true
		}))
	require.NotNil(t, out)
	require.NotContains(t, out, "unresolvedCount")
}
