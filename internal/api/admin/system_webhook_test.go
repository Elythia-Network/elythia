package admin_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/entitycompat/shapetest"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- SystemWebhook -----------------------------------------------------------

func TestSystemWebhookCreate_Success(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockSystemWebhookRepository()
	h.SetSystemWebhookRepo(repo)

	rec := doPost(h.SystemWebhookCreate,
		`{"name":"hook1","url":"https://example.com/hook","secret":"s","on":["abuseReport"],"isActive":true}`,
		adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)

	var got model.SystemWebhook
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "hook1", got.Name)
	assert.Equal(t, "https://example.com/hook", got.URL)
	assert.True(t, got.IsActive)
	assert.Contains(t, repo.Webhooks, got.ID)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	shapetest.Assert(t, "SystemWebhook", raw) // L3 (#1294)
}

func TestSystemWebhookCreate_MissingFields(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetSystemWebhookRepo(testutil.NewMockSystemWebhookRepository())
	rec := doPost(h.SystemWebhookCreate, `{"name":""}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// upstream create.ts required:[isActive,name,on,url]。on / isActive 欠落は 400 (#1542)。
func TestSystemWebhookCreate_RequiredOnIsActive(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetSystemWebhookRepo(testutil.NewMockSystemWebhookRepository())
	// on 欠落
	assert.Equal(t, http.StatusBadRequest,
		doPost(h.SystemWebhookCreate, `{"name":"x","url":"https://x","isActive":true}`, adminUser).Code)
	// isActive 欠落
	assert.Equal(t, http.StatusBadRequest,
		doPost(h.SystemWebhookCreate, `{"name":"x","url":"https://x","on":["abuseReport"]}`, adminUser).Code)
}

// on は systemWebhookEventTypes enum のみ受理。未知値は 400 (#1542)。
func TestSystemWebhookCreate_InvalidEventType(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetSystemWebhookRepo(testutil.NewMockSystemWebhookRepository())
	rec := doPost(h.SystemWebhookCreate,
		`{"name":"x","url":"https://x","on":["bogusEvent"],"isActive":true}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// update も on の enum を検証する (#1542)。
func TestSystemWebhookUpdate_InvalidEventType(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockSystemWebhookRepository()
	require.NoError(t, repo.Create(&model.SystemWebhook{ID: "w1", Name: "x", URL: "https://x"}))
	h.SetSystemWebhookRepo(repo)
	rec := doPost(h.SystemWebhookUpdate, `{"id":"w1","on":["bogusEvent"]}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSystemWebhookCreate_RepoError(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockSystemWebhookRepository()
	repo.CreateErr = assertError{}
	h.SetSystemWebhookRepo(repo)
	rec := doPost(h.SystemWebhookCreate, `{"name":"x","url":"https://x","on":["abuseReport"],"isActive":true}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestSystemWebhookList_ReturnsRows(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockSystemWebhookRepository()
	require.NoError(t, repo.Create(&model.SystemWebhook{ID: "w1", Name: "a", URL: "u", IsActive: true}))
	require.NoError(t, repo.Create(&model.SystemWebhook{ID: "w2", Name: "b", URL: "u", IsActive: true}))
	h.SetSystemWebhookRepo(repo)

	rec := doPost(h.SystemWebhookList, `{}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []model.SystemWebhook
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Len(t, rows, 2)
	// 並び順は id DESC
	assert.Equal(t, "w2", rows[0].ID)
	assert.Equal(t, "w1", rows[1].ID)
}

// #1948-10: SystemWebhook の updatedAt / latestSentAt は upstream toISOString()
// (.000Z / null) で返す。raw time.Time の RFC3339Nano だと wire-byte が乖離する。
func TestSystemWebhookShow_DateFormat(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockSystemWebhookRepository()
	sent := time.Date(2026, 6, 21, 1, 2, 3, 456_000_000, time.UTC)
	// w1: 秒ちょうど (RFC3339Nano なら .000 を落とす) updatedAt + latestSentAt set。
	require.NoError(t, repo.Create(&model.SystemWebhook{
		ID: "w1", Name: "hook", URL: "u", IsActive: true,
		UpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), LatestSentAt: &sent,
	}))
	// w2: latestSentAt nil。
	require.NoError(t, repo.Create(&model.SystemWebhook{
		ID: "w2", Name: "hook2", URL: "u", IsActive: true,
		UpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}))
	h.SetSystemWebhookRepo(repo)

	rec := doPost(h.SystemWebhookShow, `{"id":"w1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "2026-01-02T03:04:05.000Z", got["updatedAt"], "updatedAt は .000Z 形式 (#1948-10)")
	assert.Equal(t, "2026-06-21T01:02:03.456Z", got["latestSentAt"], "latestSentAt は .000Z 形式 (#1948-10)")
	// createdAt は upstream SystemWebhook pack に無い (model にも無い) → 露出しない。
	_, hasCreatedAt := got["createdAt"]
	assert.False(t, hasCreatedAt, "createdAt は SystemWebhook shape に含まれない")

	rec = doPost(h.SystemWebhookShow, `{"id":"w2"}`, adminUser)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Nil(t, got["latestSentAt"], "latestSentAt nil は null (#1948-10)")
}

func TestSystemWebhookShow_FoundAndNotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockSystemWebhookRepository()
	require.NoError(t, repo.Create(&model.SystemWebhook{ID: "w1", Name: "hook"}))
	h.SetSystemWebhookRepo(repo)

	rec := doPost(h.SystemWebhookShow, `{"id":"w1"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)

	rec2 := doPost(h.SystemWebhookShow, `{"id":"missing"}`, adminUser)
	assert.Equal(t, http.StatusNotFound, rec2.Code)
	// 本家 show.ts の noSuchSystemWebhook (#3262)。
	assert.Contains(t, rec2.Body.String(), "NO_SUCH_SYSTEM_WEBHOOK")
	assert.Contains(t, rec2.Body.String(), "38dd1ffe-04b4-6ff5-d8ba-4e6a6ae22c9d")

	// id は本家では必須。
	assert.Equal(t, http.StatusBadRequest, doPost(h.SystemWebhookShow, `{}`, adminUser).Code)
}

func TestSystemWebhookDelete_Removes(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockSystemWebhookRepository()
	require.NoError(t, repo.Create(&model.SystemWebhook{ID: "w1"}))
	h.SetSystemWebhookRepo(repo)

	rec := doPost(h.SystemWebhookDelete, `{"id":"w1"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotContains(t, repo.Webhooks, "w1")
}

func TestSystemWebhookUpdate_PartialFields(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockSystemWebhookRepository()
	require.NoError(t, repo.Create(&model.SystemWebhook{
		ID: "w1", Name: "old", URL: "https://old", IsActive: true,
	}))
	h.SetSystemWebhookRepo(repo)

	rec := doPost(h.SystemWebhookUpdate,
		`{"id":"w1","name":"new","isActive":false}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "new", repo.Webhooks["w1"].Name)
	assert.Equal(t, "https://old", repo.Webhooks["w1"].URL)
	assert.False(t, repo.Webhooks["w1"].IsActive)
}

func TestSystemWebhookUpdate_PreservesDeliveryStatus(t *testing.T) {
	// 配送 processor が latestSentAt/latestStatus を書き込んだ後でも
	// admin の Update がそれらを踏まないことを確認する。
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockSystemWebhookRepository()
	require.NoError(t, repo.Create(&model.SystemWebhook{
		ID: "w1", Name: "orig", URL: "https://o", IsActive: true,
	}))
	sentAt := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, repo.UpdateLatestStatus("w1", sentAt, 202))
	h.SetSystemWebhookRepo(repo)

	rec := doPost(h.SystemWebhookUpdate,
		`{"id":"w1","name":"renamed"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	w := repo.Webhooks["w1"]
	assert.Equal(t, "renamed", w.Name)
	require.NotNil(t, w.LatestStatus)
	assert.Equal(t, 202, *w.LatestStatus)
}

func TestSystemWebhookUpdate_NotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetSystemWebhookRepo(testutil.NewMockSystemWebhookRepository())
	rec := doPost(h.SystemWebhookUpdate, `{"id":"missing","name":"x"}`, adminUser)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSystemWebhookUpdate_MissingID(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetSystemWebhookRepo(testutil.NewMockSystemWebhookRepository())
	rec := doPost(h.SystemWebhookUpdate, `{}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSystemWebhookTest_WithRepo(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockSystemWebhookRepository()
	require.NoError(t, repo.Create(&model.SystemWebhook{ID: "w1", URL: "https://127.0.0.1:1"}))
	h.SetSystemWebhookRepo(repo)
	disp := &stubSystemWebhookDispatcher{}
	h.SetSystemWebhookDispatcher(disp)

	// webhookId 空は 400 (required, #1542)。
	assert.Equal(t, http.StatusBadRequest, doPost(h.SystemWebhookTest, `{}`, adminUser).Code)
	// 存在しない webhookId は NO_SUCH_WEBHOOK (400)。
	rec := doPost(h.SystemWebhookTest, `{"webhookId":"missing","type":"userCreated"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_WEBHOOK")
	// 正常系は 204 + dispatcher 呼び出し。
	assert.Equal(t, http.StatusNoContent,
		doPost(h.SystemWebhookTest, `{"webhookId":"w1","type":"abuseReport"}`, adminUser).Code)
	require.Len(t, disp.testCalls, 1)
	assert.Equal(t, "w1", disp.testCalls[0].webhookID)
	assert.Equal(t, "abuseReport", disp.testCalls[0].eventType)
}

// override.url / override.secret を受け取り dispatcher にそのまま渡す (#1542)。
func TestSystemWebhookTest_Override(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockSystemWebhookRepository()
	require.NoError(t, repo.Create(&model.SystemWebhook{ID: "w1", URL: "https://saved.example", Secret: "saved"}))
	h.SetSystemWebhookRepo(repo)
	disp := &stubSystemWebhookDispatcher{}
	h.SetSystemWebhookDispatcher(disp)

	rec := doPost(h.SystemWebhookTest,
		`{"webhookId":"w1","type":"userCreated","override":{"url":"https://override.example","secret":"ovsecret"}}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, disp.testCalls, 1)
	assert.Equal(t, "https://override.example", disp.testCalls[0].overrideURL)
	assert.Equal(t, "ovsecret", disp.testCalls[0].overrideSecret)
}

// --- thin nil-repo smoke tests ---
//
// nil systemWebhookRepo 経路 (newTestHandler は wire しない) で expected
// status を返すことを担保。詳細テストは repo を wire して実挙動を検証する
// ので、本群は nil 分岐の coverage 補完。

func TestSystemWebhookCreate(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.SystemWebhookCreate, `{}`, adminUser).Code)
}

func TestSystemWebhookDelete(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.SystemWebhookDelete, `{}`, adminUser).Code)
}

func TestSystemWebhookList(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusOK, doPost(h.SystemWebhookList, `{}`, adminUser).Code)
}

func TestSystemWebhookShow(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNotFound, doPost(h.SystemWebhookShow, `{}`, adminUser).Code)
}

func TestSystemWebhookTest(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// 空 webhookId は 400 (required, #1542)。
	assert.Equal(t, http.StatusBadRequest, doPost(h.SystemWebhookTest, `{}`, adminUser).Code)
	// nil repo + webhookId 指定は 500 (lookup 不能)。
	assert.Equal(t, http.StatusInternalServerError, doPost(h.SystemWebhookTest, `{"webhookId":"x","type":"userCreated"}`, adminUser).Code)
}

func TestSystemWebhookUpdate(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.SystemWebhookUpdate, `{}`, adminUser).Code)
}

// --- moderation log assertions (#665) ---

func TestSystemWebhookCreate_WritesModerationLog(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetSystemWebhookRepo(testutil.NewMockSystemWebhookRepository())
	repo := attachModLog(t, h)

	rec := doPost(h.SystemWebhookCreate, `{"name":"hook","url":"https://x","on":["abuseReport"],"isActive":true}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "createSystemWebhook", repo.Snapshot()[0].Type)
}

func TestSystemWebhookUpdate_WritesModerationLog(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	whRepo := testutil.NewMockSystemWebhookRepository()
	require.NoError(t, whRepo.Create(&model.SystemWebhook{ID: "w1", Name: "old", URL: "https://x"}))
	h.SetSystemWebhookRepo(whRepo)
	repo := attachModLog(t, h)

	rec := doPost(h.SystemWebhookUpdate, `{"id":"w1","name":"new"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	logs := repo.Snapshot()
	assert.Equal(t, "updateSystemWebhook", logs[0].Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	require.NotNil(t, info["before"])
	require.NotNil(t, info["after"])
}

func TestSystemWebhookDelete_WritesModerationLog(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	whRepo := testutil.NewMockSystemWebhookRepository()
	require.NoError(t, whRepo.Create(&model.SystemWebhook{ID: "w1", Name: "doomed", URL: "https://x"}))
	h.SetSystemWebhookRepo(whRepo)
	repo := attachModLog(t, h)

	rec := doPost(h.SystemWebhookDelete, `{"id":"w1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "deleteSystemWebhook", repo.Snapshot()[0].Type)
}

// --- #3262 ---

// type は本家では必須で、決まった種類だけを受ける。
func TestSystemWebhookTest_TypeIsRequired(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockSystemWebhookRepository()
	require.NoError(t, repo.Create(&model.SystemWebhook{ID: "w1", URL: "https://saved.example"}))
	h.SetSystemWebhookRepo(repo)
	disp := &stubSystemWebhookDispatcher{}
	h.SetSystemWebhookDispatcher(disp)

	for _, body := range []string{`{"webhookId":"w1"}`, `{"webhookId":"w1","type":""}`, `{"webhookId":"w1","type":"follow"}`} {
		assert.Equal(t, http.StatusBadRequest, doPost(h.SystemWebhookTest, body, adminUser).Code, body)
	}
	assert.Empty(t, disp.testCalls, "弾いた要求では送らない")
}

// override は保存済みの webhook に重ねる。片方だけ指定したら、もう片方は保存済みの値。
func TestSystemWebhookTest_OverrideMergesWithSaved(t *testing.T) {
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
			h, _, _, _ := newTestHandler(t)
			repo := testutil.NewMockSystemWebhookRepository()
			require.NoError(t, repo.Create(&model.SystemWebhook{ID: "w1", URL: "https://saved.example", Secret: "saved"}))
			h.SetSystemWebhookRepo(repo)
			disp := &stubSystemWebhookDispatcher{}
			h.SetSystemWebhookDispatcher(disp)

			rec := doPost(h.SystemWebhookTest, `{"webhookId":"w1","type":"userCreated"`+tt.override+`}`, adminUser)
			require.Equal(t, http.StatusNoContent, rec.Code)
			require.Len(t, disp.testCalls, 1)
			assert.Equal(t, tt.wantURL, disp.testCalls[0].overrideURL)
			assert.Equal(t, tt.wantSecret, disp.testCalls[0].overrideSecret)
		})
	}
}

func testWebhookBody(t *testing.T, eventType string) map[string]any {
	t.Helper()
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockSystemWebhookRepository()
	require.NoError(t, repo.Create(&model.SystemWebhook{ID: "w1", URL: "https://saved.example"}))
	h.SetSystemWebhookRepo(repo)
	disp := &stubSystemWebhookDispatcher{}
	h.SetSystemWebhookDispatcher(disp)
	rec := doPost(h.SystemWebhookTest, `{"webhookId":"w1","type":"`+eventType+`"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, disp.testCalls, 1)
	raw, err := json.Marshal(disp.testCalls[0].body)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// badgeRoleLookup returns a public badge role for every user, so a dummy user
// packed by the real packer would carry a badge.
type badgeRoleLookup struct{}

func (badgeRoleLookup) LookupUserRoles(string) []*model.Role {
	return []*model.Role{{ID: "r1", Name: "badge", AsBadge: true, IsPublic: true}}
}

// テスト送信の本文は本家 WebhookTestService と同じ形 (本配送と同じ組み立て)。
func TestSystemWebhookTest_DummyBodies(t *testing.T) {
	// ダミーの利用者にロールが当たっても、本家と同じくバッジは出さない。
	entity.SetUserRolesLookup(badgeRoleLookup{})
	t.Cleanup(func() { entity.SetUserRolesLookup(nil) })

	report := testWebhookBody(t, "abuseReport")
	for _, k := range []string{"id", "createdAt", "targetUserId", "targetUser", "targetUserHost", "reporterId", "reporter", "reporterHost", "assigneeId", "assignee", "resolved", "forwarded", "comment", "moderationNote", "resolvedAs"} {
		assert.Contains(t, report, k)
	}
	assert.Len(t, report, 15)
	assert.Equal(t, "dummy-user-1", report["targetUserId"])
	assert.Equal(t, "dummy1", report["targetUser"].(map[string]any)["username"])
	assert.Equal(t, "dummy-user-2", report["reporterId"])
	assert.Equal(t, "dummy2", report["reporter"].(map[string]any)["username"])
	// 本家 toPackedUserLite と同じく、アバターは空・active・バッジ無し。
	for _, k := range []string{"reporter", "targetUser"} {
		u := report[k].(map[string]any)
		assert.Equal(t, "", u["avatarUrl"], k)
		assert.Equal(t, "active", u["onlineStatus"], k)
		assert.Equal(t, []any{}, u["badgeRoles"], k)
	}
	assert.Equal(t, false, report["resolved"])
	assert.Nil(t, report["assignee"])
	assert.NotEmpty(t, report["createdAt"])

	resolved := testWebhookBody(t, "abuseReportResolved")
	assert.Equal(t, true, resolved["resolved"])
	assert.Equal(t, "dummy-user-3", resolved["assigneeId"])
	assignee := resolved["assignee"].(map[string]any)
	assert.Equal(t, "dummy3", assignee["username"])
	assert.Equal(t, "", assignee["avatarUrl"])
	assert.Equal(t, "active", assignee["onlineStatus"])
	assert.Equal(t, []any{}, assignee["badgeRoles"])

	user := testWebhookBody(t, "userCreated")
	assert.Equal(t, "dummy-user-1", user["id"])
	assert.Equal(t, "dummy1", user["username"])
	assert.Equal(t, "DummyUser1", user["name"])
	assert.NotContains(t, user, "followersCount", "UserLite で送る")
	assert.Equal(t, "", user["avatarUrl"])
	assert.Equal(t, "active", user["onlineStatus"])
	assert.Equal(t, []any{}, user["badgeRoles"])

	assert.Equal(t, map[string]any{"remainingTime": map[string]any{
		"time": float64(100000), "asDays": float64(1), "asHours": float64(24),
	}}, testWebhookBody(t, "inactiveModeratorsWarning"))
	assert.Equal(t, map[string]any{}, testWebhookBody(t, "inactiveModeratorsInvitationOnlyChanged"))
}

// list は本家と同じく isActive と on で絞る。on は「指定した種類をすべて含む」。
func TestSystemWebhookList_Filters(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockSystemWebhookRepository()
	require.NoError(t, repo.Create(&model.SystemWebhook{ID: "w1", IsActive: true, On: model.StringArray{"abuseReport", "userCreated"}}))
	require.NoError(t, repo.Create(&model.SystemWebhook{ID: "w2", IsActive: false, On: model.StringArray{"abuseReport"}}))
	require.NoError(t, repo.Create(&model.SystemWebhook{ID: "w3", IsActive: true, On: model.StringArray{"userCreated"}}))
	h.SetSystemWebhookRepo(repo)

	ids := func(body string) []string {
		rec := doPost(h.SystemWebhookList, body, adminUser)
		require.Equal(t, http.StatusOK, rec.Code, body)
		var rows []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
		out := []string{}
		for _, r := range rows {
			out = append(out, r["id"].(string))
		}
		return out
	}
	assert.ElementsMatch(t, []string{"w1", "w2", "w3"}, ids(`{}`))
	assert.ElementsMatch(t, []string{"w1", "w3"}, ids(`{"isActive":true}`))
	assert.ElementsMatch(t, []string{"w2"}, ids(`{"isActive":false}`))
	assert.ElementsMatch(t, []string{"w1", "w2"}, ids(`{"on":["abuseReport"]}`))
	assert.ElementsMatch(t, []string{"w1"}, ids(`{"on":["abuseReport","userCreated"]}`))
	assert.ElementsMatch(t, []string{"w1"}, ids(`{"isActive":true,"on":["abuseReport"]}`))
	assert.Equal(t, http.StatusBadRequest, doPost(h.SystemWebhookList, `{"on":["follow"]}`, adminUser).Code)
}

// 存在しない webhook の update / delete は NO_SUCH_SYSTEM_WEBHOOK (本家は 500)。
func TestSystemWebhook_UpdateAndDeleteMissing(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetSystemWebhookRepo(testutil.NewMockSystemWebhookRepository())

	for _, run := range []func() *httptest.ResponseRecorder{
		func() *httptest.ResponseRecorder {
			return doPost(h.SystemWebhookUpdate, `{"id":"missing","name":"x"}`, adminUser)
		},
		func() *httptest.ResponseRecorder { return doPost(h.SystemWebhookDelete, `{"id":"missing"}`, adminUser) },
	} {
		rec := run()
		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.Contains(t, rec.Body.String(), "NO_SUCH_SYSTEM_WEBHOOK")
	}
	assert.Equal(t, http.StatusBadRequest, doPost(h.SystemWebhookDelete, `{}`, adminUser).Code, "id は必須")
	// 本家はパラメータをすべて検査してから引くので、不正な on は存在の確認より先に 400。
	assert.Equal(t, http.StatusBadRequest, doPost(h.SystemWebhookUpdate, `{"id":"missing","on":["follow"]}`, adminUser).Code)
}

// 本家 create.ts / update.ts の長さの制限。検査しないと DB で弾かれて 500 になる。
func TestSystemWebhook_FieldLengths(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	for _, tt := range []struct {
		name string
		body string
		want int
	}{
		{"empty name", `"name":"","url":"https://x"`, http.StatusBadRequest},
		{"name 255", `"name":"` + long(255) + `","url":"https://x"`, http.StatusOK},
		{"name 256", `"name":"` + long(256) + `","url":"https://x"`, http.StatusBadRequest},
		{"empty url", `"name":"n","url":""`, http.StatusBadRequest},
		{"url 1024", `"name":"n","url":"` + long(1024) + `"`, http.StatusOK},
		{"url 1025", `"name":"n","url":"` + long(1025) + `"`, http.StatusBadRequest},
		{"secret 1024", `"name":"n","url":"https://x","secret":"` + long(1024) + `"`, http.StatusOK},
		{"secret 1025", `"name":"n","url":"https://x","secret":"` + long(1025) + `"`, http.StatusBadRequest},
		// 長さはコードポイントで数える (本家の ajv と varchar)。バイトで数えると落ちる。
		{"multibyte name 255", `"name":"` + strings.Repeat("あ", 255) + `","url":"https://x"`, http.StatusOK},
		{"multibyte url 1024", `"name":"n","url":"` + strings.Repeat("あ", 1024) + `"`, http.StatusOK},
		{"multibyte secret 1024", `"name":"n","url":"https://x","secret":"` + strings.Repeat("あ", 1024) + `"`, http.StatusOK},
		// NUL は列に入らない (DB で 500 になる)。
		{"nul name", `"name":"a\u0000b","url":"https://x"`, http.StatusBadRequest},
		{"nul url", `"name":"n","url":"https://x\u0000"`, http.StatusBadRequest},
		{"nul secret", `"name":"n","url":"https://x","secret":"\u0000"`, http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, _, _, _ := newTestHandler(t)
			repo := testutil.NewMockSystemWebhookRepository()
			require.NoError(t, repo.Create(&model.SystemWebhook{ID: "w1", Name: "n", URL: "https://x"}))
			h.SetSystemWebhookRepo(repo)
			create := doPost(h.SystemWebhookCreate, `{`+tt.body+`,"on":[],"isActive":true}`, adminUser)
			assert.Equal(t, tt.want, create.Code, "create")
			update := doPost(h.SystemWebhookUpdate, `{"id":"w1",`+tt.body+`}`, adminUser)
			assert.Equal(t, tt.want, update.Code, "update")
		})
	}
}
