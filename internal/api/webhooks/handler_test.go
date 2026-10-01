package webhooks

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/entitycompat/shapetest"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **not-found を模す。** 汎用 error だと #2792 の「DB 障害は 500」に引っかかる。
// repository は GORM の error をそのまま返すので、テストもそれに揃える。
var errMock = repository.ErrNotFound

// errBoom is a generic failure for write paths. **not-found とは分けること** —
// 同じ値を使い回すと、将来 write path に `IsNotFound` の分岐が入ったときに
// これらのテストが黙って別の枝を通る (#2792)。
var errBoom = errors.New("db down")

type mockWebhookRepo struct {
	webhooks  map[string]*model.Webhook
	createErr error
	updateErr error
	deleteErr error
}

func newMockRepo() *mockWebhookRepo {
	return &mockWebhookRepo{webhooks: make(map[string]*model.Webhook)}
}

func (m *mockWebhookRepo) Create(w *model.Webhook) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.webhooks[w.ID] = w
	return nil
}

func (m *mockWebhookRepo) FindByID(id string) (*model.Webhook, error) {
	if w, ok := m.webhooks[id]; ok {
		return w, nil
	}
	return nil, errMock
}

func (m *mockWebhookRepo) FindByIDAndUserID(id, userID string) (*model.Webhook, error) {
	if w, ok := m.webhooks[id]; ok && w.UserID == userID {
		return w, nil
	}
	return nil, errMock
}

func (m *mockWebhookRepo) ListByUserID(userID string) ([]*model.Webhook, error) {
	var result []*model.Webhook
	for _, w := range m.webhooks {
		if w.UserID == userID {
			result = append(result, w)
		}
	}
	return result, nil
}

func (m *mockWebhookRepo) ListActiveByUserID(userID string) ([]*model.Webhook, error) {
	var result []*model.Webhook
	for _, w := range m.webhooks {
		if w.UserID == userID && w.Active {
			result = append(result, w)
		}
	}
	return result, nil
}

func (m *mockWebhookRepo) CountByUserID(userID string) (int64, error) {
	var count int64
	for _, w := range m.webhooks {
		if w.UserID == userID {
			count++
		}
	}
	return count, nil
}

func (m *mockWebhookRepo) Update(w *model.Webhook) error {
	if m.updateErr != nil {
		return m.updateErr
	}
	m.webhooks[w.ID] = w
	return nil
}

func (m *mockWebhookRepo) UpdateLatestStatus(id string, sentAt time.Time, status int) error {
	if w, ok := m.webhooks[id]; ok {
		w.LatestSentAt = &sentAt
		w.LatestStatus = &status
	}
	return nil
}

func (m *mockWebhookRepo) Delete(id, userID string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.webhooks, id)
	return nil
}

func newTestHandler() (*Handler, *mockWebhookRepo) {
	repo := newMockRepo()
	idGen, _ := id.NewGenerator("aidx")
	return NewHandler(repo, idGen), repo
}

func post(handler func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	_ = handler(c)
	return rec
}

// --- validateOnArray helper (#939) ---

func TestValidateOnArray(t *testing.T) {
	// nil / 空配列は valid (paramDef では required ではないため許容)。
	assert.True(t, validateOnArray(nil))
	assert.True(t, validateOnArray([]string{}))
	// upstream webhookEventTypes に含まれる値は全て valid。
	assert.True(t, validateOnArray([]string{"note", "follow", "reaction"}))
	assert.True(t, validateOnArray([]string{
		"mention", "unfollow", "follow", "followed",
		"note", "reply", "renote", "reaction",
	}))
	// 1 つでも enum 違反があれば invalid (途中までが valid でも全体を reject)。
	assert.False(t, validateOnArray([]string{"foo"}))
	assert.False(t, validateOnArray([]string{"note", "foo"}))
	assert.False(t, validateOnArray([]string{"note", "Note"})) // case-sensitive
}

// --- Create ---

func TestCreate_Success(t *testing.T) {
	h, repo := newTestHandler()
	rec := post(h.Create, `{"name":"test","url":"https://example.com/hook","on":["note"]}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, repo.webhooks, 1)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "test", resp["name"])
	shapetest.Assert(t, "UserWebhook", resp) // L3 (#1270)
}

func TestCreate_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Create, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_InvalidOnEnum(t *testing.T) {
	// upstream paramDef.on.items.enum: webhookEventTypes (#939)。
	h, _ := newTestHandler()
	rec := post(h.Create, `{"name":"test","url":"https://example.com","on":["note","unknownEvent"]}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreate_Error(t *testing.T) {
	h, repo := newTestHandler()
	repo.createErr = errBoom
	rec := post(h.Create, `{"name":"test","url":"https://example.com","on":["note"]}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- webhookLimit role policy gate (#1029) ---

type stubRolePolicyProvider struct {
	policies map[string]any
}

func (s *stubRolePolicyProvider) GetUserPolicies(_ string) map[string]any { return s.policies }

func TestCreate_WebhookLimitExceeded(t *testing.T) {
	h, repo := newTestHandler()
	// 既に 2 webhook 保有
	repo.webhooks["w1"] = &model.Webhook{ID: "w1", UserID: "u1", On: model.StringArray{}}
	repo.webhooks["w2"] = &model.Webhook{ID: "w2", UserID: "u1", On: model.StringArray{}}
	h.SetRolePolicyProvider(&stubRolePolicyProvider{policies: map[string]any{
		"webhookLimit": 2,
	}})
	rec := post(h.Create, `{"name":"test","url":"https://example.com","on":["note"]}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "TOO_MANY_WEBHOOKS")
	assert.Contains(t, rec.Body.String(), "87a9bb19-111e-4e37-81d3-a3e7426453b0")
}

func TestCreate_WebhookLimit_PassesUnderLimit(t *testing.T) {
	h, _ := newTestHandler()
	h.SetRolePolicyProvider(&stubRolePolicyProvider{policies: map[string]any{
		"webhookLimit": 10,
	}})
	rec := post(h.Create, `{"name":"test","url":"https://example.com","on":["note"]}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code, "limit 内なら通常成功")
}

// failCountRepo は CountByUserID だけ error を返す stub。webhookLimit gate
// で count 取得が失敗した場合に 500 を返すパスを検証する。
type failCountRepo struct{ *mockWebhookRepo }

func (f *failCountRepo) CountByUserID(_ string) (int64, error) { return 0, errBoom }

func TestCreate_WebhookLimit_CountError(t *testing.T) {
	repo := newMockRepo()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&failCountRepo{repo}, idGen)
	h.SetRolePolicyProvider(&stubRolePolicyProvider{policies: map[string]any{
		"webhookLimit": 5,
	}})
	rec := post(h.Create, `{"name":"test","url":"https://example.com","on":["note"]}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- List ---

func TestList_Success(t *testing.T) {
	h, repo := newTestHandler()
	repo.webhooks["w1"] = &model.Webhook{ID: "w1", UserID: "u1", Name: "hook1", On: model.StringArray{"note"}}
	rec := post(h.List, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
}

func TestList_Empty(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.List, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1948-10: packWebhook の latestSentAt は upstream toISOString() (.000Z / null)。
// raw *time.Time の RFC3339Nano だと wire-byte が乖離する。
func TestList_LatestSentAtFormat(t *testing.T) {
	h, repo := newTestHandler()
	sent := time.Date(2026, 6, 21, 1, 2, 3, 456_000_000, time.UTC)
	repo.webhooks["w1"] = &model.Webhook{ID: "w1", UserID: "u1", Name: "hook1", On: model.StringArray{"note"}, LatestSentAt: &sent}
	repo.webhooks["w2"] = &model.Webhook{ID: "w2", UserID: "u1", Name: "hook2", On: model.StringArray{}}
	rec := post(h.List, `{}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	byID := map[string]map[string]any{}
	for _, e := range resp {
		byID[e["id"].(string)] = e
	}
	assert.Equal(t, "2026-06-21T01:02:03.456Z", byID["w1"]["latestSentAt"], "latestSentAt は .000Z (#1948-10)")
	assert.Nil(t, byID["w2"]["latestSentAt"], "nil latestSentAt は null (#1948-10)")
}

type failListRepo struct{ *mockWebhookRepo }

func (f *failListRepo) ListByUserID(_ string) ([]*model.Webhook, error) { return nil, errBoom }

func TestList_Error(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&failListRepo{newMockRepo()}, idGen)
	rec := post(h.List, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Show ---

func TestShow_Success(t *testing.T) {
	h, repo := newTestHandler()
	repo.webhooks["w1"] = &model.Webhook{ID: "w1", UserID: "u1", Name: "hook1", On: model.StringArray{}}
	rec := post(h.Show, `{"webhookId":"w1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	shapetest.Assert(t, "UserWebhook", resp) // L3 (#1320)
}

func TestShow_NotFound(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Show, `{"webhookId":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Show, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Update ---

func TestUpdate_Success(t *testing.T) {
	// upstream Misskey TS の i/webhooks/update は body 無しの 204 を返すので
	// drop-in 互換のため mk-go 側も 204 + body 無しに揃える (#936)。
	h, repo := newTestHandler()
	repo.webhooks["w1"] = &model.Webhook{ID: "w1", UserID: "u1", Name: "old", URL: "https://old.example", On: model.StringArray{}}
	rec := post(h.Update, `{"webhookId":"w1","name":"new","url":"https://new.example","on":["follow"],"active":false}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
	// repo に反映されていることを確認 (caller は別途 i/webhooks/show で取り直す前提)
	assert.Equal(t, "new", repo.webhooks["w1"].Name)
	assert.False(t, repo.webhooks["w1"].Active)
}

func TestUpdate_Partial(t *testing.T) {
	h, repo := newTestHandler()
	secret := "oldsecret"
	repo.webhooks["w1"] = &model.Webhook{ID: "w1", UserID: "u1", Name: "name", URL: "https://example.com", Secret: secret, On: model.StringArray{}}
	newSecret := "newsecret"
	rec := post(h.Update, `{"webhookId":"w1","secret":"`+newSecret+`"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, newSecret, repo.webhooks["w1"].Secret)
}

// #1948-15: secret の absent/null/value を区別する。
//
//	absent  → 維持
//	null    → "" reset (upstream: secret===null ? '')
//	"x"     → 設定
func TestUpdate_SecretNullAbsentValue(t *testing.T) {
	h, repo := newTestHandler()
	repo.webhooks["w1"] = &model.Webhook{ID: "w1", UserID: "u1", Name: "n", URL: "https://e", Secret: "old", On: model.StringArray{}}

	// absent → 維持。
	rec := post(h.Update, `{"webhookId":"w1","name":"x"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "old", repo.webhooks["w1"].Secret, "secret 省略は維持 (#1948-15)")

	// null → "" reset。
	rec = post(h.Update, `{"webhookId":"w1","secret":null}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "", repo.webhooks["w1"].Secret, "secret:null は空文字 reset (#1948-15)")

	// "v" → 設定。
	rec = post(h.Update, `{"webhookId":"w1","secret":"v"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "v", repo.webhooks["w1"].Secret)
}

func TestUpdate_NotFound(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Update, `{"webhookId":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Update, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_InvalidOnEnum(t *testing.T) {
	// upstream paramDef.on.items.enum: webhookEventTypes (#939)。
	h, repo := newTestHandler()
	repo.webhooks["w1"] = &model.Webhook{ID: "w1", UserID: "u1", On: model.StringArray{}}
	rec := post(h.Update, `{"webhookId":"w1","on":["note","unknownEvent"]}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_Error(t *testing.T) {
	h, repo := newTestHandler()
	repo.webhooks["w1"] = &model.Webhook{ID: "w1", UserID: "u1", On: model.StringArray{}}
	repo.updateErr = errBoom
	rec := post(h.Update, `{"webhookId":"w1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Delete ---

func TestDelete_Success(t *testing.T) {
	h, repo := newTestHandler()
	repo.webhooks["w1"] = &model.Webhook{ID: "w1", UserID: "u1"}
	rec := post(h.Delete, `{"webhookId":"w1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, repo.webhooks)
}

func TestDelete_NotFound(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Delete, `{"webhookId":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDelete_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Delete, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDelete_Error(t *testing.T) {
	h, repo := newTestHandler()
	repo.webhooks["w1"] = &model.Webhook{ID: "w1", UserID: "u1"}
	repo.deleteErr = errBoom
	rec := post(h.Delete, `{"webhookId":"w1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Test ---

// stubDispatcher captures DispatchUserTest invocations for assertions.
type stubDispatcher struct {
	calls []stubDispatchCall
}

type stubDispatchCall struct {
	webhookID      string
	userID         string
	eventType      string
	body           any
	overrideURL    string
	overrideSecret string
}

func (s *stubDispatcher) DispatchUserTest(webhookID, userID, eventType string, body any, overrideURL, overrideSecret string) {
	s.calls = append(s.calls, stubDispatchCall{webhookID, userID, eventType, body, overrideURL, overrideSecret})
}

func TestTest_Success(t *testing.T) {
	h, repo := newTestHandler()
	repo.webhooks["w1"] = &model.Webhook{ID: "w1", UserID: "u1"}
	disp := &stubDispatcher{}
	h.SetDispatcher(disp)

	rec := post(h.Test, `{"webhookId":"w1","type":"note"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, disp.calls, 1)
	assert.Equal(t, "w1", disp.calls[0].webhookID)
	assert.Equal(t, "u1", disp.calls[0].userID)
	assert.Equal(t, "note", disp.calls[0].eventType)
	// #1546: dummy body は type に応じた shape ({note: ...})。{test:true} ではない。
	body, ok := disp.calls[0].body.(map[string]any)
	require.True(t, ok)
	assert.Contains(t, body, "note")
	assert.NotContains(t, body, "test")
	// override 未指定なら空。
	assert.Empty(t, disp.calls[0].overrideURL)
}

// override は保存済みの webhook に重ねる。片方だけ指定したら、もう片方は
// 保存済みの値 (本家の {...webhook, ...override}、#3278)。
func TestTest_OverrideMergesWithSaved(t *testing.T) {
	for _, tt := range []struct {
		name, override, wantURL, wantSecret string
	}{
		{"none", ``, "", ""},
		{"empty object", `,"override":{}`, "", ""},
		{"url only", `,"override":{"url":"https://override.example"}`, "https://override.example", "saved"},
		{"secret only", `,"override":{"secret":"ov"}`, "https://saved.example", "ov"},
		{"empty secret", `,"override":{"secret":""}`, "https://saved.example", ""},
		{"empty url", `,"override":{"url":"","secret":"ov"}`, "https://saved.example", "ov"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, repo := newTestHandler()
			repo.webhooks["w1"] = &model.Webhook{ID: "w1", UserID: "u1", URL: "https://saved.example", Secret: "saved"}
			disp := &stubDispatcher{}
			h.SetDispatcher(disp)

			rec := post(h.Test, `{"webhookId":"w1","type":"note"`+tt.override+`}`, &model.User{ID: "u1"})
			require.Equal(t, http.StatusNoContent, rec.Code)
			require.Len(t, disp.calls, 1)
			assert.Equal(t, tt.wantURL, disp.calls[0].overrideURL)
			assert.Equal(t, tt.wantSecret, disp.calls[0].overrideSecret)
		})
	}
}

// #1546: override 指定で別 url/secret へ送る。
func TestTest_Override(t *testing.T) {
	h, repo := newTestHandler()
	repo.webhooks["w1"] = &model.Webhook{ID: "w1", UserID: "u1"}
	disp := &stubDispatcher{}
	h.SetDispatcher(disp)

	rec := post(h.Test, `{"webhookId":"w1","type":"follow","override":{"url":"https://test.example/hook","secret":"sek"}}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, disp.calls, 1)
	assert.Equal(t, "https://test.example/hook", disp.calls[0].overrideURL)
	assert.Equal(t, "sek", disp.calls[0].overrideSecret)
	// follow event の dummy body は {user: ...}。
	body, _ := disp.calls[0].body.(map[string]any)
	assert.Contains(t, body, "user")
}

func TestTest_TypeRequired(t *testing.T) {
	// upstream paramDef は webhookId + type 両方を required にしている (#937)。
	h, repo := newTestHandler()
	repo.webhooks["w1"] = &model.Webhook{ID: "w1", UserID: "u1"}
	rec := post(h.Test, `{"webhookId":"w1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTest_InvalidType(t *testing.T) {
	// upstream paramDef は type に webhookEventTypes enum を強制 (#937)。
	h, repo := newTestHandler()
	repo.webhooks["w1"] = &model.Webhook{ID: "w1", UserID: "u1"}
	rec := post(h.Test, `{"webhookId":"w1","type":"unknownEvent"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTest_NoDispatcherStillReturns204(t *testing.T) {
	h, repo := newTestHandler()
	repo.webhooks["w1"] = &model.Webhook{ID: "w1", UserID: "u1"}
	rec := post(h.Test, `{"webhookId":"w1","type":"note"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestTest_NotFound(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Test, `{"webhookId":"ghost","type":"note"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTest_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Test, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #2027: on 欠落は 400、空配列 [] は present 扱いで許容。
func TestCreate_OnRequired(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Create, `{"name":"t","url":"https://e.example"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "on 欠落は 400 (#2027)")
	rec = post(h.Create, `{"name":"t","url":"https://e.example","on":[]}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code, "空 on は許容")
	assert.Contains(t, rec.Body.String(), `"on":[]`, "response on は [] (null でない、#2027)")
}
