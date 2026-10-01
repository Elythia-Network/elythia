package webhook_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/core/webhook"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fake Enqueuer ---

type fakeEnqueuer struct {
	userCalls   []queue.WebhookPayload
	systemCalls []queue.WebhookPayload
	userErr     error
	systemErr   error
}

func (f *fakeEnqueuer) EnqueueDeliver(_ queue.DeliverPayload, _ ...driver.EnqueueOption) error {
	return nil
}
func (f *fakeEnqueuer) EnqueueExport(_ queue.ExportPayload) error                      { return nil }
func (f *fakeEnqueuer) EnqueueImport(_ queue.ImportPayload) error                      { return nil }
func (f *fakeEnqueuer) EnqueueWebPush(_ context.Context, _ queue.WebPushPayload) error { return nil }
func (f *fakeEnqueuer) EnqueueUserWebhook(_ context.Context, p queue.WebhookPayload) error {
	if f.userErr != nil {
		return f.userErr
	}
	f.userCalls = append(f.userCalls, p)
	return nil
}
func (f *fakeEnqueuer) EnqueueSystemWebhook(_ context.Context, p queue.WebhookPayload) error {
	if f.systemErr != nil {
		return f.systemErr
	}
	f.systemCalls = append(f.systemCalls, p)
	return nil
}
func (f *fakeEnqueuer) EnqueueInbox(_ context.Context, _ queue.InboxPayload) error { return nil }
func (f *fakeEnqueuer) EnqueuePostScheduledNote(_ queue.PostScheduledNotePayload, _ ...driver.EnqueueOption) error {
	return nil
}
func (f *fakeEnqueuer) ClearScheduledNote(_ string) error { return nil }
func (f *fakeEnqueuer) Close() error                      { return nil }

// --- fake webhook repos ---

type fakeWebhookRepo struct {
	hooks  map[string]*model.Webhook
	listEr error
}

func (r *fakeWebhookRepo) Create(_ *model.Webhook) error             { return nil }
func (r *fakeWebhookRepo) FindByID(_ string) (*model.Webhook, error) { return nil, nil }
func (r *fakeWebhookRepo) FindByIDAndUserID(_, _ string) (*model.Webhook, error) {
	return nil, nil
}
func (r *fakeWebhookRepo) ListByUserID(_ string) ([]*model.Webhook, error) {
	return nil, nil
}
func (r *fakeWebhookRepo) CountByUserID(_ string) (int64, error) { return 0, nil }
func (r *fakeWebhookRepo) ListActiveByUserID(userID string) ([]*model.Webhook, error) {
	if r.listEr != nil {
		return nil, r.listEr
	}
	out := make([]*model.Webhook, 0)
	for _, h := range r.hooks {
		if h.UserID == userID && h.Active {
			out = append(out, h)
		}
	}
	return out, nil
}
func (r *fakeWebhookRepo) Update(_ *model.Webhook) error { return nil }
func (r *fakeWebhookRepo) UpdateLatestStatus(_ string, _ time.Time, _ int) error {
	return nil
}
func (r *fakeWebhookRepo) Delete(_, _ string) error { return nil }

type fakeSystemWebhookRepo struct {
	hooks  map[string]*model.SystemWebhook
	listEr error
}

func (r *fakeSystemWebhookRepo) Create(_ *model.SystemWebhook) error             { return nil }
func (r *fakeSystemWebhookRepo) FindByID(_ string) (*model.SystemWebhook, error) { return nil, nil }
func (r *fakeSystemWebhookRepo) List() ([]*model.SystemWebhook, error)           { return nil, nil }
func (r *fakeSystemWebhookRepo) ListActive() ([]*model.SystemWebhook, error) {
	if r.listEr != nil {
		return nil, r.listEr
	}
	out := make([]*model.SystemWebhook, 0)
	for _, h := range r.hooks {
		if h.IsActive {
			out = append(out, h)
		}
	}
	return out, nil
}
func (r *fakeSystemWebhookRepo) Update(_ *model.SystemWebhook) error { return nil }
func (r *fakeSystemWebhookRepo) UpdateAdminFields(_ string, _ map[string]any) error {
	return nil
}
func (r *fakeSystemWebhookRepo) UpdateLatestStatus(_ string, _ time.Time, _ int) error {
	return nil
}
func (r *fakeSystemWebhookRepo) Delete(_ string) error { return nil }

// --- tests ---

func TestDispatchUser_NoActiveHooksIsNoop(t *testing.T) {
	enq := &fakeEnqueuer{}
	repo := &fakeWebhookRepo{hooks: map[string]*model.Webhook{}}
	svc := webhook.NewService(enq, repo, nil, "https://example.com")
	svc.DispatchUser("alice", webhook.EventNote, map[string]any{"note": "x"})
	assert.Empty(t, enq.userCalls)
}

func TestDispatchUser_FiltersByEvent(t *testing.T) {
	enq := &fakeEnqueuer{}
	repo := &fakeWebhookRepo{hooks: map[string]*model.Webhook{
		"h1": {ID: "h1", UserID: "alice", Active: true, URL: "u", On: model.StringArray{"note"}},
		"h2": {ID: "h2", UserID: "alice", Active: true, URL: "u", On: model.StringArray{"reaction"}},
	}}
	svc := webhook.NewService(enq, repo, nil, "https://example.com")
	svc.DispatchUser("alice", webhook.EventNote, map[string]any{"note": "x"})
	require.Len(t, enq.userCalls, 1)
	assert.Equal(t, "h1", enq.userCalls[0].WebhookID)

	var env webhook.Envelope
	require.NoError(t, json.Unmarshal(enq.userCalls[0].Body, &env))
	assert.Equal(t, "https://example.com", env.Server)
	assert.Equal(t, "alice", env.UserID)
	assert.Equal(t, "note", env.Type)
	assert.NotZero(t, env.CreatedAt)
	assert.NotEmpty(t, env.EventID)
}

func TestDispatchUser_InactiveSkipped(t *testing.T) {
	enq := &fakeEnqueuer{}
	repo := &fakeWebhookRepo{hooks: map[string]*model.Webhook{
		"h1": {ID: "h1", UserID: "alice", Active: false, URL: "u", On: model.StringArray{"note"}},
	}}
	svc := webhook.NewService(enq, repo, nil, "https://example.com")
	svc.DispatchUser("alice", webhook.EventNote, map[string]any{"note": "x"})
	assert.Empty(t, enq.userCalls)
}

func TestDispatchUser_RepoError(t *testing.T) {
	enq := &fakeEnqueuer{}
	repo := &fakeWebhookRepo{listEr: errors.New("db boom")}
	svc := webhook.NewService(enq, repo, nil, "https://example.com")
	svc.DispatchUser("alice", webhook.EventNote, nil)
	assert.Empty(t, enq.userCalls)
}

func TestDispatchUser_EnqueueError(t *testing.T) {
	enq := &fakeEnqueuer{userErr: errors.New("enq boom")}
	repo := &fakeWebhookRepo{hooks: map[string]*model.Webhook{
		"h1": {ID: "h1", UserID: "alice", Active: true, URL: "u", On: model.StringArray{"note"}},
	}}
	svc := webhook.NewService(enq, repo, nil, "https://example.com")
	svc.DispatchUser("alice", webhook.EventNote, nil) // should log but not panic
}

func TestDispatchUser_NilSafe(t *testing.T) {
	var svc *webhook.Service
	svc.DispatchUser("alice", "note", nil) // must not panic
	svc = webhook.NewService(nil, nil, nil, "")
	svc.DispatchUser("alice", "note", nil)
}

func TestDispatchSystem_Works(t *testing.T) {
	enq := &fakeEnqueuer{}
	repo := &fakeSystemWebhookRepo{hooks: map[string]*model.SystemWebhook{
		"h1": {ID: "h1", IsActive: true, On: model.StringArray{"userCreated"}, URL: "u"},
	}}
	svc := webhook.NewService(enq, nil, repo, "https://example.com")
	svc.DispatchSystem(webhook.SystemEventUserCreated, map[string]any{"id": "u1"})
	require.Len(t, enq.systemCalls, 1)
	assert.Equal(t, "h1", enq.systemCalls[0].WebhookID)
	assert.Empty(t, enq.systemCalls[0].UserID)
}

func TestDispatchSystem_FiltersByEvent(t *testing.T) {
	enq := &fakeEnqueuer{}
	repo := &fakeSystemWebhookRepo{hooks: map[string]*model.SystemWebhook{
		"h1": {ID: "h1", IsActive: true, On: model.StringArray{"abuseReport"}},
	}}
	svc := webhook.NewService(enq, nil, repo, "https://example.com")
	svc.DispatchSystem(webhook.SystemEventUserCreated, nil)
	assert.Empty(t, enq.systemCalls)
}

func TestDispatchSystem_InactiveSkipped(t *testing.T) {
	enq := &fakeEnqueuer{}
	repo := &fakeSystemWebhookRepo{hooks: map[string]*model.SystemWebhook{
		"h1": {ID: "h1", IsActive: false, On: model.StringArray{"userCreated"}},
	}}
	svc := webhook.NewService(enq, nil, repo, "https://example.com")
	svc.DispatchSystem(webhook.SystemEventUserCreated, nil)
	assert.Empty(t, enq.systemCalls)
}

func TestDispatchSystem_RepoError(t *testing.T) {
	enq := &fakeEnqueuer{}
	repo := &fakeSystemWebhookRepo{listEr: errors.New("db boom")}
	svc := webhook.NewService(enq, nil, repo, "https://example.com")
	svc.DispatchSystem(webhook.SystemEventUserCreated, nil)
	assert.Empty(t, enq.systemCalls)
}

func TestDispatchSystem_EnqueueError(t *testing.T) {
	enq := &fakeEnqueuer{systemErr: errors.New("enq boom")}
	repo := &fakeSystemWebhookRepo{hooks: map[string]*model.SystemWebhook{
		"h1": {ID: "h1", IsActive: true, On: model.StringArray{"userCreated"}},
	}}
	svc := webhook.NewService(enq, nil, repo, "https://example.com")
	svc.DispatchSystem(webhook.SystemEventUserCreated, nil)
}

func TestDispatchSystem_NilSafe(t *testing.T) {
	var svc *webhook.Service
	svc.DispatchSystem("userCreated", nil)
	svc = webhook.NewService(nil, nil, nil, "")
	svc.DispatchSystem("userCreated", nil)
}

// DispatchSystemExcluding は excludes に含まれる webhook ID へは配送しない
// (本家 enqueueSystemWebhook opts.excludes 相当、#1723)。
func TestDispatchSystemExcluding_SkipsExcluded(t *testing.T) {
	enq := &fakeEnqueuer{}
	repo := &fakeSystemWebhookRepo{hooks: map[string]*model.SystemWebhook{
		"h1": {ID: "h1", IsActive: true, On: model.StringArray{"abuseReportResolved"}},
		"h2": {ID: "h2", IsActive: true, On: model.StringArray{"abuseReportResolved"}},
	}}
	svc := webhook.NewService(enq, nil, repo, "https://example.com")
	svc.DispatchSystemExcluding(webhook.SystemEventAbuseReportResolved, map[string]any{"id": "r1"}, []string{"h2"})
	require.Len(t, enq.systemCalls, 1)
	assert.Equal(t, "h1", enq.systemCalls[0].WebhookID, "excludes の h2 は配送されない")
}

// DispatchSystemTest は webhookID 1 件に test 配送を enqueue し、override を
// payload に載せる (#1542)。
func TestDispatchSystemTest_EnqueuesWithOverride(t *testing.T) {
	enq := &fakeEnqueuer{}
	svc := webhook.NewService(enq, nil, nil, "https://example.com")
	svc.DispatchSystemTest("w1", webhook.SystemEventAbuseReport, map[string]any{"id": "r1"}, "https://override.example", "ovsecret")
	require.Len(t, enq.systemCalls, 1)
	assert.Equal(t, "w1", enq.systemCalls[0].WebhookID)
	assert.Equal(t, "https://override.example", enq.systemCalls[0].OverrideURL)
	assert.Equal(t, "ovsecret", enq.systemCalls[0].OverrideSecret)
	assert.Empty(t, enq.systemCalls[0].UserID, "system webhook は UserID を持たない")
	assert.True(t, enq.systemCalls[0].SingleAttempt, "テスト送信は本家と同じく再試行しない (#3262)")
}

// 利用者の webhook のテスト送信も、本家と同じく再試行しない (#3278)。
func TestDispatchUserTest_SingleAttempt(t *testing.T) {
	enq := &fakeEnqueuer{}
	svc := webhook.NewService(enq, nil, nil, "https://example.com")
	svc.DispatchUserTest("w1", "u1", webhook.EventNote, map[string]any{}, "", "")
	require.Len(t, enq.userCalls, 1)
	assert.True(t, enq.userCalls[0].SingleAttempt)
}

func TestDispatchSystemTest_NilSafe(t *testing.T) {
	var svc *webhook.Service
	svc.DispatchSystemTest("w1", "abuseReport", nil, "", "")
	svc = webhook.NewService(nil, nil, nil, "")
	svc.DispatchSystemTest("w1", "abuseReport", nil, "", "")
}

// excludes が nil の場合は DispatchSystem と同じ全 active 配送。
func TestDispatchSystemExcluding_NilExcludesDeliversAll(t *testing.T) {
	enq := &fakeEnqueuer{}
	repo := &fakeSystemWebhookRepo{hooks: map[string]*model.SystemWebhook{
		"h1": {ID: "h1", IsActive: true, On: model.StringArray{"abuseReportResolved"}},
		"h2": {ID: "h2", IsActive: true, On: model.StringArray{"abuseReportResolved"}},
	}}
	svc := webhook.NewService(enq, nil, repo, "https://example.com")
	svc.DispatchSystemExcluding(webhook.SystemEventAbuseReportResolved, nil, nil)
	assert.Len(t, enq.systemCalls, 2)
}
