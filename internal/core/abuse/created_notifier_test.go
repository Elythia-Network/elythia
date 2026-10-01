package abuse_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/abuse"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

type cnInApp struct{ reports []*model.AbuseUserReport }

func (s *cnInApp) NotifyNewReport(_ context.Context, r *model.AbuseUserReport) {
	s.reports = append(s.reports, r)
}

type cnMods struct {
	mods []*model.User
	err  error
}

func (s cnMods) GetModerators() ([]*model.User, error) { return s.mods, s.err }

type cnAdmin struct {
	calls []struct {
		userID, eventType string
		body              any
	}
}

func (s *cnAdmin) PublishAdminEvent(userID, eventType string, body any) {
	s.calls = append(s.calls, struct {
		userID, eventType string
		body              any
	}{userID, eventType, body})
}

type cnWebhook struct {
	calls []struct {
		eventType string
		body      any
		excludes  []string
	}
}

func (s *cnWebhook) DispatchSystemExcluding(eventType string, body any, excludes []string) {
	s.calls = append(s.calls, struct {
		eventType string
		body      any
		excludes  []string
	}{eventType, body, excludes})
}

var cnT0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func cnReport(t *testing.T, idGen id.Generator) *model.AbuseUserReport {
	t.Helper()
	host := "remote.example"
	return &model.AbuseUserReport{
		ID: idGen.Generate(cnT0), TargetUserID: "bob", ReporterID: "alice",
		Comment: "spam", ReporterHost: &host,
	}
}

// 1 回の呼び出しで、通知欄・admin stream・abuseReport system webhook の 3 つを
// 出す (#3256)。ローカルの report-abuse も連合経由の Flag も、これを呼ぶ。
func TestCreatedNotifier_NotifiesEveryChannel(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	inApp := &cnInApp{}
	admin := &cnAdmin{}
	hook := &cnWebhook{}
	recipients := testutil.NewMockAbuseReportNotificationRecipientRepository()
	inactiveID, activeID := "wh_inactive", "wh_active"
	require.NoError(t, recipients.Create(&model.AbuseReportNotificationRecipient{ID: "rc1", Method: "webhook", IsActive: false, SystemWebhookID: &inactiveID}))
	require.NoError(t, recipients.Create(&model.AbuseReportNotificationRecipient{ID: "rc2", Method: "webhook", IsActive: true, SystemWebhookID: &activeID}))
	// 方法が email の通知先は、無効でも webhook の除外に入れない (指す webhook が無い)。
	emailHookID := "wh_email"
	require.NoError(t, recipients.Create(&model.AbuseReportNotificationRecipient{ID: "rc3", Method: "email", IsActive: false, SystemWebhookID: &emailHookID}))
	n := abuse.NewCreatedNotifier(inApp, cnMods{mods: []*model.User{{ID: "mod1"}, {ID: "mod2"}}}, admin)
	instName := "Remote"
	lookups := abuse.UserLookups{Instances: wpInstances{rows: []*model.Instance{{Host: "remote.example", Name: &instName}}}}
	n.SetWebhook(hook, recipients, lookups, idGen)

	report := cnReport(t, idGen)
	host := "remote.example"
	reporter := &model.User{ID: "alice", Username: "alice", Host: &host}
	target := &model.User{ID: "bob", Username: "bob"}
	n.NotifyCreated(context.Background(), report, reporter, target)

	// 通知欄 (連打の絞りは InAppNotifier の責務なので 1 回渡すだけ)。
	require.Len(t, inApp.reports, 1)
	assert.Same(t, report, inApp.reports[0])

	// admin stream: モデレーター全員に、本家と同じ 4 つだけを送る。
	require.Len(t, admin.calls, 2)
	assert.ElementsMatch(t, []string{"mod1", "mod2"}, []string{admin.calls[0].userID, admin.calls[1].userID})
	assert.Equal(t, "newAbuseUserReport", admin.calls[0].eventType)
	assert.Equal(t, map[string]any{"id": report.ID, "targetUserId": "bob", "reporterId": "alice", "comment": "spam"}, admin.calls[0].body)

	// system webhook: 無効にした通知先が指す webhook は送らない。
	require.Len(t, hook.calls, 1)
	assert.Equal(t, "abuseReport", hook.calls[0].eventType)
	assert.Equal(t, []string{inactiveID}, hook.calls[0].excludes)
	// 本文の形は WebhookPayload のテストで固定する。ここでは渡した利用者が
	// 載ることだけ見る。
	raw, err := json.Marshal(hook.calls[0].body)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	assert.Equal(t, report.ID, body["id"])
	assert.Equal(t, "remote.example", body["reporterHost"])
	assert.Equal(t, "alice", body["reporter"].(map[string]any)["username"])
	// SetWebhook で渡した lookups を本文に使う。Flag の通報者は必ずリモートなので、
	// ここが抜けると Flag 経由の通報だけ instance が付かない。
	assert.Equal(t, "Remote", body["reporter"].(map[string]any)["instance"].(map[string]any)["name"])
	assert.Equal(t, "bob", body["targetUser"].(map[string]any)["username"])
	assert.Nil(t, body["assignee"])
}

// 経路ごとに独立している。片方が未配線でも、残りは出る。
func TestCreatedNotifier_ChannelsAreIndependent(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	report := cnReport(t, idGen)

	// webhook だけ配線した (admin stream と通知欄が無い)。
	hook := &cnWebhook{}
	n := abuse.NewCreatedNotifier(nil, nil, nil)
	n.SetWebhook(hook, nil, abuse.UserLookups{}, idGen)
	n.NotifyCreated(context.Background(), report, nil, nil)
	require.Len(t, hook.calls, 1, "通知欄と admin stream が無くても webhook は出す")
	body := hook.calls[0].body.(map[string]any)
	assert.Nil(t, body["reporter"], "引けなかった利用者は null")
	assert.Nil(t, hook.calls[0].excludes, "通知先を読めなければ除外せずに送る")

	// モデレーターの一覧が取れなくても、通知欄と webhook は出す。
	inApp := &cnInApp{}
	admin := &cnAdmin{}
	hook2 := &cnWebhook{}
	n2 := abuse.NewCreatedNotifier(inApp, cnMods{err: errors.New("db down")}, admin)
	n2.SetWebhook(hook2, nil, abuse.UserLookups{}, idGen)
	n2.NotifyCreated(context.Background(), report, nil, nil)
	assert.Empty(t, admin.calls)
	assert.Len(t, inApp.reports, 1)
	assert.Len(t, hook2.calls, 1)

	// webhook が未配線なら何もしない (通知欄は出す)。
	inApp3 := &cnInApp{}
	abuse.NewCreatedNotifier(inApp3, nil, nil).NotifyCreated(context.Background(), report, nil, nil)
	assert.Len(t, inApp3.reports, 1)

	// nil の notifier / 通報は何もしない。
	var nilN *abuse.CreatedNotifier
	nilN.NotifyCreated(context.Background(), report, nil, nil)
	n.NotifyCreated(context.Background(), nil, nil, nil)
	assert.Len(t, hook.calls, 1)
}
