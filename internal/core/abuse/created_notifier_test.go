package abuse_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/abuse"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
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

type cnMail struct {
	mu   sync.Mutex
	sent []cnSent
}

type cnSent struct{ to, subject, body string }

func (m *cnMail) send(to, subject, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, cnSent{to, subject, body})
}

func (m *cnMail) snapshot() []cnSent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]cnSent(nil), m.sent...)
}

type cnMeta struct{ meta *model.Meta }

func (s cnMeta) Fetch() (*model.Meta, error) { return s.meta, nil }

// 本家 notifyMail と同じく、有効なメール方式の通知先のうち、モデレーターで
// メールアドレスを確認済みの利用者と、meta.email へ送る (#3265)。
func TestCreatedNotifier_SendsMailToRecipients(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	ptr := func(s string) *string { return &s }

	recipients := testutil.NewMockAbuseReportNotificationRecipientRepository()
	for _, r := range []*model.AbuseReportNotificationRecipient{
		{ID: "ok", Method: "email", IsActive: true, UserID: ptr("mod_ok")},
		{ID: "inactive", Method: "email", IsActive: false, UserID: ptr("mod_inactive")},
		{ID: "unverified", Method: "email", IsActive: true, UserID: ptr("mod_unverified")},
		{ID: "notmod", Method: "email", IsActive: true, UserID: ptr("not_mod")},
		{ID: "noemail", Method: "email", IsActive: true, UserID: ptr("mod_noemail")},
		{ID: "hook", Method: "webhook", IsActive: true, SystemWebhookID: ptr("wh1")},
		// webhook 方式の通知先には、利用者が入っていてもメールを送らない。
		{ID: "hookuser", Method: "webhook", IsActive: true, UserID: ptr("mod_hook"), SystemWebhookID: ptr("wh2")},
	} {
		require.NoError(t, recipients.Create(r))
	}
	users := testutil.NewMockUserRepository()
	for id, p := range map[string]*model.UserProfile{
		"mod_ok":         {UserID: "mod_ok", Email: ptr("ok@example.test"), EmailVerified: true},
		"mod_inactive":   {UserID: "mod_inactive", Email: ptr("inactive@example.test"), EmailVerified: true},
		"mod_unverified": {UserID: "mod_unverified", Email: ptr("unverified@example.test")},
		"not_mod":        {UserID: "not_mod", Email: ptr("notmod@example.test"), EmailVerified: true},
		"mod_noemail":    {UserID: "mod_noemail", EmailVerified: true},
		"mod_hook":       {UserID: "mod_hook", Email: ptr("hook@example.test"), EmailVerified: true},
	} {
		users.Profiles[id] = p
	}
	mods := cnMods{mods: []*model.User{{ID: "mod_ok"}, {ID: "mod_inactive"}, {ID: "mod_unverified"}, {ID: "mod_noemail"}, {ID: "mod_hook"}}}
	mail := &cnMail{}

	n := abuse.NewCreatedNotifier(nil, mods, nil)
	n.SetMail(mail.send, recipients, users, cnMeta{meta: &model.Meta{Email: ptr("instance@example.test")}})
	report := cnReport(t, idGen)
	n.NotifyCreated(context.Background(), report, nil, nil)

	require.Eventually(t, func() bool { return len(mail.snapshot()) == 2 }, time.Second, 5*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	sent := mail.snapshot()
	require.Len(t, sent, 2, "宛先は確認済みのモデレーター 1 人と meta.email だけ")
	var tos []string
	for _, s := range sent {
		tos = append(tos, s.to)
		assert.Equal(t, "New Abuse Report", s.subject, "本家と同じ件名")
		assert.Equal(t, "spam", s.body, "本文は通報のコメント")
	}
	assert.ElementsMatch(t, []string{"ok@example.test", "instance@example.test"}, tos)
}

// 通知先も meta.email も無ければ送らない。メールを配線しなければ何もしない。
func TestCreatedNotifier_NoMailWithoutAddresses(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	mail := &cnMail{}
	n := abuse.NewCreatedNotifier(nil, cnMods{}, nil)
	n.SetMail(mail.send, testutil.NewMockAbuseReportNotificationRecipientRepository(), testutil.NewMockUserRepository(), cnMeta{meta: &model.Meta{}})
	n.NotifyCreated(context.Background(), cnReport(t, idGen), nil, nil)
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, mail.snapshot())
}

// 送信が遅くても NotifyCreated は待たない (通報の API や inbox を止めない)。
func TestCreatedNotifier_MailDoesNotBlock(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{}, 1)
	blocking := func(string, string, string) {
		started <- struct{}{}
		<-release
	}
	email := "instance@example.test"
	n := abuse.NewCreatedNotifier(nil, nil, nil)
	n.SetMail(blocking, nil, nil, cnMeta{meta: &model.Meta{Email: &email}})

	done := make(chan struct{})
	go func() {
		n.NotifyCreated(context.Background(), cnReport(t, idGen), nil, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("NotifyCreated がメールの送信を待っている")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("メールを送っていない")
	}
}

// 空のメールアドレスには送らない (通知先の利用者も meta.email も)。
func TestCreatedNotifier_SkipsEmptyAddresses(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	empty := ""
	uid := "mod_empty"
	recipients := testutil.NewMockAbuseReportNotificationRecipientRepository()
	require.NoError(t, recipients.Create(&model.AbuseReportNotificationRecipient{ID: "r", Method: "email", IsActive: true, UserID: &uid}))
	users := testutil.NewMockUserRepository()
	users.Profiles[uid] = &model.UserProfile{UserID: uid, Email: &empty, EmailVerified: true}
	mail := &cnMail{}
	n := abuse.NewCreatedNotifier(nil, cnMods{mods: []*model.User{{ID: uid}}}, nil)
	n.SetMail(mail.send, recipients, users, cnMeta{meta: &model.Meta{Email: &empty}})
	n.NotifyCreated(context.Background(), cnReport(t, idGen), nil, nil)
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, mail.snapshot())
}

// SetMail は webhook の除外に使う通知先 (SetWebhook) を書き換えない。
func TestCreatedNotifier_SetMailKeepsWebhookRecipients(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	inactive := "wh_inactive"
	hookRecipients := testutil.NewMockAbuseReportNotificationRecipientRepository()
	require.NoError(t, hookRecipients.Create(&model.AbuseReportNotificationRecipient{ID: "h", Method: "webhook", IsActive: false, SystemWebhookID: &inactive}))
	hook := &cnWebhook{}
	n := abuse.NewCreatedNotifier(nil, nil, nil)
	n.SetWebhook(hook, hookRecipients, abuse.UserLookups{}, idGen)
	n.SetMail((&cnMail{}).send, testutil.NewMockAbuseReportNotificationRecipientRepository(), testutil.NewMockUserRepository(), cnMeta{meta: &model.Meta{}})

	n.NotifyCreated(context.Background(), cnReport(t, idGen), nil, nil)
	require.Len(t, hook.calls, 1)
	assert.Equal(t, []string{inactive}, hook.calls[0].excludes, "webhook の除外は SetWebhook で渡した通知先から作る")
}
