package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

type stubModeratorChecker struct {
	moderators map[string]bool
	admins     map[string]bool
	// policies[userID][policyKey]。絵文字の申請の通知 (#2987) は
	// `canManageCustomEmojis` で判定されるので、モデレーターとは別に持つ。
	policies map[string]map[string]bool
}

func (s stubModeratorChecker) IsModerator(userID string) bool     { return s.moderators[userID] }
func (s stubModeratorChecker) IsAdministrator(userID string) bool { return s.admins[userID] }

// HasRolePolicy mirrors core/role.Service: root / 管理者は短絡し、
// **モデレーターは短絡しない**。
func (s stubModeratorChecker) HasRolePolicy(userID, policyKey string) bool {
	if s.admins[userID] {
		return true
	}
	return s.policies[userID][policyKey]
}

// wireAbuseLookup は read 時の状態引き当てを配線する (#2868)。
// **未配線だと abuseReport が drop される** (fail-closed) ので、通報の通知を
// 期待するテストは必ず通す。
func wireAbuseLookup(h *Handler, resolved bool) {
	h.SetAbuseReportLookup(func(ids []string) (map[string]model.AbuseReportState, error) {
		out := make(map[string]model.AbuseReportState, len(ids))
		for _, id := range ids {
			out[id] = model.AbuseReportState{Resolved: resolved}
		}
		return out, nil
	})
}

func seedAbuseReport(t *testing.T, svc *notification.Service, notifieeID string) {
	t.Helper()
	_, err := svc.Create(context.Background(), notification.CreateInput{
		NotifieeID: notifieeID, NotifierID: "reporter", Type: notification.TypeAbuseReport,
		Extra: map[string]any{"reportId": "r1", "targetUserId": "u2", "comment": "spam"},
	})
	require.NoError(t, err)
}

func listNotifications(t *testing.T, h *Handler, userID string) []map[string]any {
	t.Helper()
	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, &model.User{ID: userID})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(rec.Body.String())), &out))
	return out
}

// #2868: モデレーターには通報の通知が見える。
func TestShow_AbuseReportVisibleToModerator(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{moderators: map[string]bool{"mod1": true}})
	wireAbuseLookup(h, false)
	seedAbuseReport(t, svc, "mod1")

	out := listNotifications(t, h, "mod1")
	require.Len(t, out, 1)
	assert.Equal(t, "abuseReport", out[0]["type"])
	assert.Equal(t, "r1", out[0]["reportId"])
}

// administrator も通す (upstream の iAmModerator と同じ扱い)。
func TestShow_AbuseReportVisibleToAdministrator(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{admins: map[string]bool{"root": true}})
	wireAbuseLookup(h, false)
	seedAbuseReport(t, svc, "root")

	out := listNotifications(t, h, "root")
	require.Len(t, out, 1)
}

// **権限を失うと見えなくなる (#2868)。** 通報本文と対象ユーザー ID が Extra に
// 入っているので、admin/abuse-user-reports が 403 になった後も通知欄から
// 読み続けられてはいけない。
func TestShow_AbuseReportHiddenAfterPrivilegeLoss(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{})
	seedAbuseReport(t, svc, "exmod")

	out := listNotifications(t, h, "exmod")
	assert.Empty(t, out, "権限を失った利用者に通報の通知を返してはいけない")
}

// checker 未配線なら返さない (fail-closed)。
func TestShow_AbuseReportHiddenWithoutChecker(t *testing.T) {
	h, svc := newTestHandler(t)
	seedAbuseReport(t, svc, "mod1")

	out := listNotifications(t, h, "mod1")
	assert.Empty(t, out, "checker 未配線では通報の通知を返してはいけない (fail-closed)")
}

// 他の型は権限に関係なく返る (filter が広すぎないこと)。
func TestShow_OtherTypesUnaffectedByModerationFilter(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{})
	_, err := svc.Create(context.Background(), notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	out := listNotifications(t, h, "alice")
	require.Len(t, out, 1)
	assert.Equal(t, "follow", out[0]["type"])
}

// **通報者をミュートしていても通報は届く (#2868)。** notifier は通報者なので、
// 通常の valid-notifier filter に掛けるとモデレーターがミュートした相手からの
// 通報が通知欄に一切現れなくなる。
func TestShow_AbuseReportSurvivesNotifierMute(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{moderators: map[string]bool{"mod1": true}})
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	mutingRepo := testutil.NewMockMutingRepository()
	userRepo.Users["reporter"] = &model.User{ID: "reporter", Username: "reporter"}
	require.NoError(t, mutingRepo.Create(&model.Muting{ID: "mu1", MuterID: "mod1", MuteeID: "reporter"}))
	h.SetRepos(userRepo, noteRepo)
	h.SetMutingRepo(mutingRepo)
	wireAbuseLookup(h, false)
	seedAbuseReport(t, svc, "mod1")

	out := listNotifications(t, h, "mod1")
	require.Len(t, out, 1, "ミュートした相手からの通報も通知欄に出す")
}

// **対処済みの通報は resolved=true で返る (#2868)。** 通知は作成時点の状態しか
// 持たないので、他のモデレーターが対処しても通知欄が「未対応」のまま残ると、
// 同じ通報に二重で当たることになる。
func TestShow_AbuseReportCarriesResolvedState(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{moderators: map[string]bool{"mod1": true}})
	wireAbuseLookup(h, true)
	seedAbuseReport(t, svc, "mod1")

	out := listNotifications(t, h, "mod1")
	require.Len(t, out, 1)
	assert.Equal(t, true, out[0]["resolved"], "対処済みが read 時に反映されること")
}

// 通報が削除されていたら通知ごと落とす。
func TestShow_AbuseReportDroppedWhenReportGone(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{moderators: map[string]bool{"mod1": true}})
	h.SetAbuseReportLookup(func([]string) (map[string]model.AbuseReportState, error) {
		return map[string]model.AbuseReportState{}, nil
	})
	seedAbuseReport(t, svc, "mod1")

	out := listNotifications(t, h, "mod1")
	assert.Empty(t, out, "削除済みの通報を指す通知は返さない")
}

// **1 ページで 1 回しか引かない (#2868)。** 通知一覧はページあたり最大 100 件で、
// 1 件ずつ引くとリクエストごとに数百 SELECT が直列に走る。
func TestShow_AbuseReportStatesAreFetchedInOneBatch(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{moderators: map[string]bool{"mod1": true}})

	var calls int
	var askedIDs []string
	h.SetAbuseReportLookup(func(ids []string) (map[string]model.AbuseReportState, error) {
		calls++
		askedIDs = append(askedIDs, ids...)
		out := make(map[string]model.AbuseReportState, len(ids))
		for _, id := range ids {
			out[id] = model.AbuseReportState{}
		}
		return out, nil
	})

	ctx := context.Background()
	for _, rid := range []string{"r1", "r2", "r3"} {
		_, err := svc.Create(ctx, notification.CreateInput{
			NotifieeID: "mod1", NotifierID: "reporter", Type: notification.TypeAbuseReport,
			Extra: map[string]any{"reportId": rid, "targetUserId": "u2"},
		})
		require.NoError(t, err)
	}

	out := listNotifications(t, h, "mod1")
	require.Len(t, out, 3)
	require.Equal(t, 1, calls, "通報の状態はページごとに 1 回だけ引くこと")
	require.ElementsMatch(t, []string{"r1", "r2", "r3"}, askedIDs)
}

// abuseReport が 1 件も無いページでは引かない。
func TestShow_NoAbuseReportMeansNoStateQuery(t *testing.T) {
	h, svc := newTestHandler(t)
	var calls int
	h.SetAbuseReportLookup(func(ids []string) (map[string]model.AbuseReportState, error) {
		calls++
		return map[string]model.AbuseReportState{}, nil
	})
	_, err := svc.Create(context.Background(), notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	listNotifications(t, h, "alice")
	require.Zero(t, calls, "abuseReport が無いページで通報を引いている")
}

// **DB 障害を「削除済み」に丸めない (#2792 / #2868)。** 丸めると一時的な接続断で
// 通報の通知だけが消え、しかも既読位置は進むので未読の合図が失われる。
func TestShow_AbuseReportStateErrorIs500(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{moderators: map[string]bool{"mod1": true}})
	h.SetAbuseReportLookup(func([]string) (map[string]model.AbuseReportState, error) {
		return nil, assertAnError{}
	})
	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)
	seedAbuseReport(t, svc, "mod1")

	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, &model.User{ID: "mod1"})
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code, "DB 障害は 500 にする")
	assert.NotContains(t, pub.types("mod1"), "readAllNotifications",
		"エラーで返したのに既読化してはいけない")
}

type assertAnError struct{}

func (assertAnError) Error() string { return "boom" }
