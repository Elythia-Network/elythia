package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"time"

	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
	"gorm.io/datatypes"

	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testRedis *testutil.TestRedis
	idGen     id.Generator
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		log.Fatalf("redis setup failed: %v", err)
	}
	testRedis = tr
	idGen, _ = id.NewGenerator("aidx")
	code := m.Run()
	tr.Teardown(ctx)
	os.Exit(code)
}

func newTestHandler(t *testing.T) (*Handler, *notification.Service) {
	t.Helper()
	testRedis.FlushAll(context.Background())
	svc := notification.NewService(testRedis.Client, idGen, "")
	// テストは AfterFunc 待ちを避けるため同期 publish にする。
	svc.SetUnreadPublishDelay(0)
	return NewHandler(svc, idGen), svc
}

func newJSONRequest(t *testing.T, path, body string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func setAuth(c echo.Context, user *model.User) {
	c.Set(string(middleware.UserContextKey), user)
}

func TestShow_Empty(t *testing.T) {
	h, _ := newTestHandler(t)
	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp)
}

func TestShow_WithEntries(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)
	_, err = svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeReaction,
		NoteID: "n1", Reaction: "👍",
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{"limit":50}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 2)
	assert.Equal(t, "reaction", resp[0]["type"])
	assert.Equal(t, "n1", resp[0]["noteId"])
	assert.Equal(t, "👍", resp[0]["reaction"])
	assert.Equal(t, "bob", resp[0]["userId"])
}

// #1546: 明示的な includeTypes:[] は空配列を返す (= 何も含めない)。
// includeTypes 省略時は全件返ることと対比して固定する。
func TestShow_IncludeTypesEmptyReturnsEmpty(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	// includeTypes:[] → []
	c, rec := newJSONRequest(t, "/api/i/notifications", `{"limit":50,"includeTypes":[]}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp, "explicit includeTypes:[] must return []")

	// includeTypes 省略 → 全件 (対比)。
	c2, rec2 := newJSONRequest(t, "/api/i/notifications", `{"limit":50}`)
	setAuth(c2, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c2))
	var resp2 []map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	assert.Len(t, resp2, 1, "omitted includeTypes returns all")
}

// sinceDate=0 (= 1970 epoch) を渡したら全 notification が post-fetch filter
// を通過する。adapter pattern (= sinceDate → aidx prefix) が effectively に
// 動くことの positive case (#1174)。
func TestShow_SinceDate_EpochIncludesAll(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{"limit":50,"sinceDate":0}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1, "sinceDate=0 (= 1970) で全件含まれること")
}

// untilDate を未来に設定したら全 notification が post-fetch filter を通過する
// (= untilID は exclusive upper bound、未来 ms に対する aidx prefix は全 ID
// より大きいので全件含まれる)。
func TestShow_UntilDate_FutureIncludesAll(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	futureMs := time.Now().Add(time.Hour).UnixMilli()
	body := fmt.Sprintf(`{"limit":50,"untilDate":%d}`, futureMs)
	c, rec := newJSONRequest(t, "/api/i/notifications", body)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1, "untilDate=future で全件含まれること")
}

// sinceDate を未来に設定したら post-fetch filter で全件除外される。
// adapter pattern の negative case (#1174)。
func TestShow_SinceDate_FutureFiltersOut(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	futureMs := time.Now().Add(time.Hour).UnixMilli()
	body := fmt.Sprintf(`{"limit":50,"sinceDate":%d}`, futureMs)
	c, rec := newJSONRequest(t, "/api/i/notifications", body)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp, "sinceDate=future で全件 filter out されること")
}

func TestShow_InvalidJSON(t *testing.T) {
	h, _ := newTestHandler(t)
	c, rec := newJSONRequest(t, "/api/i/notifications", `{invalid`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_RedisError(t *testing.T) {
	closed := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = closed.Close()
	svc := notification.NewService(closed, idGen, "")
	h := NewHandler(svc, idGen)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// stubMainPublisher captures PublishMainEvent calls so the test can verify
// the implicit mark-as-read side effect of /api/i/notifications (#420).
type stubMainPublisher struct {
	mu     sync.Mutex
	events []stubMainEvent
}

type stubMainEvent struct {
	userID    string
	eventType string
}

func (s *stubMainPublisher) PublishMainEvent(userID, eventType string, _ any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, stubMainEvent{userID: userID, eventType: eventType})
}

func (s *stubMainPublisher) types(userID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.events))
	for _, e := range s.events {
		if e.userID == userID {
			out = append(out, e.eventType)
		}
	}
	return out
}

// 通知一覧 fetch の副作用として暗黙の mark-all-as-read が走り、main stream
// に `readAllNotifications` が publish されることを確認する (#420)。
func TestShow_DefaultMarksAllAsRead(t *testing.T) {
	h, svc := newTestHandler(t)
	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	_, err := svc.Create(context.Background(), notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Contains(t, pub.types("alice"), "readAllNotifications",
		"Show should publish readAllNotifications by default")
}

// markAsRead: false を明示した場合は副作用を発火させない。本家
// i/notifications と同じ semantics。
func TestShow_MarkAsReadFalseSkipsRead(t *testing.T) {
	h, svc := newTestHandler(t)
	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	_, err := svc.Create(context.Background(), notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{"markAsRead":false}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	assert.NotContains(t, pub.types("alice"), "readAllNotifications",
		"Show with markAsRead:false must not publish readAllNotifications")
}

func TestMarkAllAsRead_OK(t *testing.T) {
	h, svc := newTestHandler(t)
	_, err := svc.Create(context.Background(), notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/notifications/mark-all-as-read", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.MarkAllAsRead(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// POST /api/notifications/mark-all-as-read は明示的な既読操作なので、読み取り
// 位置が動かなくても readAllNotifications を再送する (#2831)。バッジのカウンタは
// クライアント側にしか無く、イベントを取りこぼした状態からの復帰手段はこれだけ。
func TestMarkAllAsRead_ForcesRepublishWhenAlreadyRead(t *testing.T) {
	h, svc := newTestHandler(t)
	_, err := svc.Create(context.Background(), notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	// Create 由来の unreadNotification を拾わないよう、通知を作ってから wire する。
	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	// 1 回目で read marker が最新まで進む。
	c, _ := newJSONRequest(t, "/api/notifications/mark-all-as-read", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.MarkAllAsRead(c))
	require.Len(t, pub.types("alice"), 1)

	c2, rec2 := newJSONRequest(t, "/api/notifications/mark-all-as-read", `{}`)
	setAuth(c2, &model.User{ID: "alice"})
	require.NoError(t, h.MarkAllAsRead(c2))
	assert.Equal(t, http.StatusNoContent, rec2.Code)
	assert.Equal(t, []string{"readAllNotifications", "readAllNotifications"}, pub.types("alice"),
		"explicit mark-all-as-read must re-publish even when the marker does not move")
}

// 暗黙既読 (通知一覧 fetch の副作用) は force を立てない。毎 fetch で再送すると
// 保留中の unreadNotification を潰してバッジが点かなくなる (#420 follow-up)。
func TestShow_ImplicitMarkAsReadDoesNotForce(t *testing.T) {
	h, svc := newTestHandler(t)
	_, err := svc.Create(context.Background(), notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	// Create 由来の unreadNotification を拾わないよう、通知を作ってから wire する。
	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	for i := 0; i < 2; i++ {
		c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
		setAuth(c, &model.User{ID: "alice"})
		require.NoError(t, h.Show(c))
		require.Equal(t, http.StatusOK, rec.Code)
	}

	assert.Equal(t, []string{"readAllNotifications"}, pub.types("alice"),
		"implicit mark-as-read must publish only while the marker actually moves")
}

func TestMarkAllAsRead_RedisError(t *testing.T) {
	closed := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = closed.Close()
	svc := notification.NewService(closed, idGen, "")
	h := NewHandler(svc, idGen)

	c, rec := newJSONRequest(t, "/api/notifications/mark-all-as-read", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.MarkAllAsRead(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestCreate_Success(t *testing.T) {
	h, _ := newTestHandler(t)
	c, rec := newJSONRequest(t, "/api/notifications/create", `{"body":"hello","header":"MyApp","icon":"https://example.com/i.png"}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Show で type 'app' + body/header/icon が surface されることを検証。
	c2, rec2 := newJSONRequest(t, "/api/i/notifications", `{"limit":50}`)
	setAuth(c2, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c2))
	require.Equal(t, http.StatusOK, rec2.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "app", resp[0]["type"])
	assert.Equal(t, "hello", resp[0]["body"])
	assert.Equal(t, "MyApp", resp[0]["header"])
	assert.Equal(t, "https://example.com/i.png", resp[0]["icon"])
	// app 通知は notifier を持たないこと。
	_, hasUserID := resp[0]["userId"]
	assert.False(t, hasUserID, "app notification must not carry a notifier userId")
}

func TestCreate_BodyOnly(t *testing.T) {
	h, _ := newTestHandler(t)
	c, rec := newJSONRequest(t, "/api/notifications/create", `{"body":"just body"}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)

	c2, rec2 := newJSONRequest(t, "/api/i/notifications", `{"limit":50}`)
	setAuth(c2, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c2))
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "just body", resp[0]["body"])
	// #1557: 'app' 通知の header / icon は required + nullable なので、未指定 +
	// token fallback 無し (accessTokenRepo 未配線) のときは present かつ null。
	hVal, hasHeader := resp[0]["header"]
	assert.True(t, hasHeader)
	assert.Nil(t, hVal)
	iVal, hasIcon := resp[0]["icon"]
	assert.True(t, hasIcon)
	assert.Nil(t, iVal)
}

// #1557 notifications/create: header/icon 未指定なら access token の name/iconUrl に
// fallback し、appAccessTokenId も記録する。
func TestCreate_TokenFallback(t *testing.T) {
	h, _ := newTestHandler(t)
	atRepo := testutil.NewMockAccessTokenRepository()
	appName, appIcon := "My App", "https://app.example/icon.png"
	atRepo.Tokens["hash1"] = &model.AccessToken{ID: "at1", Token: "rawtoken", Name: &appName, IconURL: &appIcon}
	h.SetAccessTokenRepo(atRepo)

	c, rec := newJSONRequest(t, "/api/notifications/create", `{"body":"hi"}`)
	setAuth(c, &model.User{ID: "alice"})
	c.Set(string(middleware.TokenContextKey), "rawtoken")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)

	c2, rec2 := newJSONRequest(t, "/api/i/notifications", `{"limit":50}`)
	setAuth(c2, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c2))
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "hi", resp[0]["body"])
	assert.Equal(t, appName, resp[0]["header"], "header は token.name に fallback")
	assert.Equal(t, appIcon, resp[0]["icon"], "icon は token.iconUrl に fallback")
	// appAccessTokenId は内部メタデータでレスポンスには出さない。
	_, hasATID := resp[0]["appAccessTokenId"]
	assert.False(t, hasATID)
}

// 明示 header/icon は token fallback より優先される。
func TestCreate_ExplicitHeaderIconOverridesToken(t *testing.T) {
	h, _ := newTestHandler(t)
	atRepo := testutil.NewMockAccessTokenRepository()
	appName := "My App"
	atRepo.Tokens["hash1"] = &model.AccessToken{ID: "at1", Token: "rawtoken", Name: &appName}
	h.SetAccessTokenRepo(atRepo)

	c, rec := newJSONRequest(t, "/api/notifications/create", `{"body":"hi","header":"Custom","icon":"https://x/i.png"}`)
	setAuth(c, &model.User{ID: "alice"})
	c.Set(string(middleware.TokenContextKey), "rawtoken")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)

	c2, rec2 := newJSONRequest(t, "/api/i/notifications", `{"limit":50}`)
	setAuth(c2, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c2))
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "Custom", resp[0]["header"])
	assert.Equal(t, "https://x/i.png", resp[0]["icon"])
}

func TestCreate_MissingBody(t *testing.T) {
	h, _ := newTestHandler(t)
	c, rec := newJSONRequest(t, "/api/notifications/create", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// upstream の ajv required は presence チェックなので body="" は受理される。
// mk-go も不在 (nil) のみ 400 とし、空文字は通すこと (drop-in 互換)。
func TestCreate_EmptyBodyAccepted(t *testing.T) {
	h, _ := newTestHandler(t)
	c, rec := newJSONRequest(t, "/api/notifications/create", `{"body":""}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)

	c2, rec2 := newJSONRequest(t, "/api/i/notifications", `{"limit":50}`)
	setAuth(c2, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c2))
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "app", resp[0]["type"])
	assert.Equal(t, "", resp[0]["body"])
}

func TestCreate_NilService(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(nil, idGen)
	c, rec := newJSONRequest(t, "/api/notifications/create", `{"body":"x"}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestFlush_Success(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	// 1件作成してから flush するとストリームが消えることを確認。
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/notifications/flush", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Flush(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// handler が Flush() を呼んでいれば stream 長 0。
	// 以前は MarkAllAsRead() を呼んでいたため元通知が残ってしまっていた。
	out, err := svc.List(ctx, "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestFlush_NilService(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(nil, idGen)
	c, rec := newJSONRequest(t, "/api/notifications/flush", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Flush(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestFlush_RedisError(t *testing.T) {
	closed := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = closed.Close()
	svc := notification.NewService(closed, idGen, "")
	h := NewHandler(svc, idGen)

	c, rec := newJSONRequest(t, "/api/notifications/flush", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Flush(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestTestNotification_Success(t *testing.T) {
	h, _ := newTestHandler(t)
	c, rec := newJSONRequest(t, "/api/notifications/test-notification", `{}`)
	require.NoError(t, h.TestNotification(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// recordingTestNotifier captures OnTest calls (#1559)。
type recordingTestNotifier struct{ called []string }

func (n *recordingTestNotifier) OnTest(userID string) { n.called = append(n.called, userID) }

// #1559 [LOW] test-notification は notifier が配線されていれば OnTest を発火する。
func TestTestNotification_FiresNotifier(t *testing.T) {
	h, _ := newTestHandler(t)
	notifier := &recordingTestNotifier{}
	h.SetTestNotifier(notifier)
	c, rec := newJSONRequest(t, "/api/notifications/test-notification", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.TestNotification(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"alice"}, notifier.called)
}

func TestShow_WithUserAndNoteResolution(t *testing.T) {
	h, svc := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bobuser"}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: "public", User: &model.User{ID: "bob", Username: "bobuser"}}
	h.SetRepos(userRepo, noteRepo)
	h.SetQueryService(corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository()))

	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeMention, NoteID: "n1",
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "mention", resp[0]["type"])
	assert.NotNil(t, resp[0]["user"])
	assert.NotNil(t, resp[0]["note"])
}

// #1775: read 時の valid-notifier filter。now-muted / muted-instance / suspended
// notifier からの通知は落とし、通常 notifier と notifier 無しの system 通知は残す。
func TestShow_FiltersInvalidNotifiers(t *testing.T) {
	h, svc := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	mutingRepo := testutil.NewMockMutingRepository()

	mutedHost := "muted.example"
	userRepo.Users["good"] = &model.User{ID: "good", Username: "good"}
	userRepo.Users["mutedU"] = &model.User{ID: "mutedU", Username: "mutedu"}
	userRepo.Users["suspendedU"] = &model.User{ID: "suspendedU", Username: "susp", IsSuspended: true}
	userRepo.Users["instU"] = &model.User{ID: "instU", Username: "inst", Host: &mutedHost}
	// alice が mutedU を mute / muted.example を instance-mute している。
	require.NoError(t, mutingRepo.Create(&model.Muting{ID: "mu1", MuterID: "alice", MuteeID: "mutedU"}))
	userRepo.Profiles["alice"] = &model.UserProfile{UserID: "alice", MutedInstances: datatypes.JSON(`["muted.example"]`)}

	h.SetRepos(userRepo, noteRepo)
	h.SetMutingRepo(mutingRepo)

	ctx := context.Background()
	for _, nid := range []string{"good", "mutedU", "suspendedU", "instU"} {
		_, err := svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: nid, Type: notification.TypeReaction, Reaction: "👍"})
		require.NoError(t, err)
	}
	// notifier 無しの system 通知 (achievementEarned) は常に残る。
	_, err := svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", Type: notification.Type("achievementEarned")})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	notifiers := map[string]bool{}
	systemSeen := false
	for _, r := range resp {
		if uid, ok := r["userId"].(string); ok {
			notifiers[uid] = true
		} else {
			systemSeen = true
		}
	}
	assert.True(t, notifiers["good"], "通常 notifier は残る")
	assert.True(t, systemSeen, "notifier 無しの system 通知は残る")
	assert.False(t, notifiers["mutedU"], "mute 済 notifier は落とす")
	assert.False(t, notifiers["suspendedU"], "suspended notifier は落とす")
	assert.False(t, notifiers["instU"], "muted-instance notifier は落とす")
}

// InstanceRepo / EmojiRepo 配線時にリモート notifier の user.instance が
// streaming / REST payload に入ることを確認する (#415 InstanceTicker 欠落の
// follow-up)。
func TestShow_WithInstanceAndEmojiResolution(t *testing.T) {
	h, svc := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	instanceRepo := testutil.NewMockInstanceRepository()
	emojiRepo := testutil.NewMockEmojiRepository()

	remoteHost := "remote.example"
	themeColor := "#ff7799"
	instanceRepo.Instances[remoteHost] = &model.Instance{Host: remoteHost, ThemeColor: &themeColor}
	userRepo.Users["remoteU"] = &model.User{ID: "remoteU", Username: "remotee", Host: &remoteHost}

	h.SetRepos(userRepo, noteRepo)
	h.SetInstanceRepo(instanceRepo)
	h.SetEmojiRepo(emojiRepo)

	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "remoteU", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	user, ok := resp[0]["user"].(map[string]any)
	require.True(t, ok)
	inst, ok := user["instance"].(map[string]any)
	require.True(t, ok, "user.instance must be set on remote notifier")
	assert.Equal(t, themeColor, inst["themeColor"])
	// L3 (#1326): 実 HTTP 応答を golden Notification union に突合 (type=follow で
	// variant dispatch)。gate の union 対応 (ValidateResponse) を実経路で検証。
	shapetest.Assert(t, "Notification", resp[0])
}

func TestShow_IncludeTypesFilter(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	_, _ = svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	_, _ = svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeMention, NoteID: "n1",
	})

	c, rec := newJSONRequest(t, "/api/i/notifications", `{"includeTypes":["mention"]}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
	assert.Equal(t, "mention", resp[0]["type"])
}

func TestShow_ExcludeTypesFilter(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	_, _ = svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	_, _ = svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeMention, NoteID: "n1",
	})

	c, rec := newJSONRequest(t, "/api/i/notifications", `{"excludeTypes":["follow"]}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1)
	assert.Equal(t, "mention", resp[0]["type"])
}

func TestShow_CursorPagination(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	_, _ = svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	n2, _ := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeMention, NoteID: "n1",
	})

	// sinceId: n2より新しいものだけ (= 該当なし)
	body := `{"sinceId":"` + n2.ID + `"}`
	c, rec := newJSONRequest(t, "/api/i/notifications", body)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp)
}

// countingNotifRepos wraps the mock user / note repos to count single-row
// FindByID-style calls. After #515 the bulk path must use FindManyByIDs /
// FindManyByIDsWithUser so single-row callers should be 0.
type countingNotifUserRepo struct {
	*testutil.MockUserRepository
	findByIDCalls         int
	findManyByIDsCalls    int
	findManyByIDsCallSize int
}

func (c *countingNotifUserRepo) FindByID(id string) (*model.User, error) {
	c.findByIDCalls++
	return c.MockUserRepository.FindByID(id)
}

func (c *countingNotifUserRepo) FindManyByIDs(ids []string) ([]*model.User, error) {
	c.findManyByIDsCalls++
	c.findManyByIDsCallSize += len(ids)
	return c.MockUserRepository.FindManyByIDs(ids)
}

type countingNotifNoteRepo struct {
	*testutil.MockNoteRepository
	findByIDWithRelationsCalls int
	findManyByIDsWithUserCalls int
}

func (c *countingNotifNoteRepo) FindByIDWithRelations(id string) (*model.Note, error) {
	c.findByIDWithRelationsCalls++
	return c.MockNoteRepository.FindByIDWithRelations(id)
}

func (c *countingNotifNoteRepo) FindManyByIDsWithUser(ids []string) ([]*model.Note, error) {
	c.findManyByIDsWithUserCalls++
	return c.MockNoteRepository.FindManyByIDsWithUser(ids)
}

// 通知 5 件 (異なる notifier / note) を返すケースで、user と note が batch
// 1 回ずつだけ DB に問い合わされ、per-row FindByID 経路が使われていないこと
// を担保する (#515 N+1 解消)。
func TestShow_BatchFetchesNotifierAndNote(t *testing.T) {
	h, svc := newTestHandler(t)
	userRepo := &countingNotifUserRepo{MockUserRepository: testutil.NewMockUserRepository()}
	noteRepo := &countingNotifNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository()}
	for i := 0; i < 5; i++ {
		uid := fmt.Sprintf("notifier-%d", i)
		nid := fmt.Sprintf("note-%d", i)
		userRepo.Users[uid] = &model.User{ID: uid, Username: uid}
		noteRepo.Notes[nid] = &model.Note{ID: nid, UserID: uid, Visibility: "public",
			User: &model.User{ID: uid, Username: uid}}
	}
	h.SetRepos(userRepo, noteRepo)
	h.SetQueryService(corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository()))

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, err := svc.Create(ctx, notification.CreateInput{
			NotifieeID: "alice",
			NotifierID: fmt.Sprintf("notifier-%d", i),
			Type:       notification.TypeMention,
			NoteID:     fmt.Sprintf("note-%d", i),
		})
		require.NoError(t, err)
	}

	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 5)

	assert.Equal(t, 0, userRepo.findByIDCalls,
		"per-row userRepo.FindByID must not be called (N+1 must be eliminated)")
	assert.Equal(t, 1, userRepo.findManyByIDsCalls,
		"userRepo.FindManyByIDs should be called exactly once per request")
	assert.Equal(t, 5, userRepo.findManyByIDsCallSize,
		"all 5 notifier IDs should be coalesced into a single batch")
	assert.Equal(t, 0, noteRepo.findByIDWithRelationsCalls,
		"per-row noteRepo.FindByIDWithRelations must not be called")
	assert.Equal(t, 1, noteRepo.findManyByIDsWithUserCalls,
		"noteRepo.FindManyByIDsWithUser should be called exactly once per request")

	// Devin #516 INFO-3: batch fetch しても resolved user / note が response
	// に正しく入ることを self-contained で担保する。
	for i, item := range resp {
		assert.NotNilf(t, item["user"], "item[%d].user must be present", i)
		assert.NotNilf(t, item["note"], "item[%d].note must be present", i)
	}
}

// #1444 IDOR: B が followers note で A に reply した時、A (= B を follow して
// いない recipient) の通知一覧に**その通知行自体が出ない**ことを確認する。
// note embed が CanSeeNote で gate され、note を pack できない note-required
// 通知は #1953 で行ごと落とす扱いになった (それ以前は noteId だけの行を残していた)。
func TestShow_FollowersReply_NonFollower_RowDropped(t *testing.T) {
	h, svc := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bobuser"}
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityFollowers,
		User: &model.User{ID: "bob", Username: "bobuser"},
	}
	// alice → bob の follow 関係は設定しない (= 非フォロワー視点)
	h.SetRepos(userRepo, noteRepo)
	h.SetQueryService(corenote.NewQueryService(noteRepo, followingRepo))

	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeReply, NoteID: "n1",
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// note-required 通知はその note が pack できないと行ごと落ちる
	// (`!('noteId' in x) || packedNotes.has(x.noteId)`、#1953)。以前は embed のみ
	// nil 化して noteId だけの行を残していたが upstream 非互換だった。
	//
	// **不可視 note まで落とすのは mk-go 独自。** upstream の
	// NoteEntityService.packMany は入力と 1:1 で null を返さず、可視性は hideNote が
	// blank するだけなので行は残る。mk-go は行の存在自体を伏せる分だけ安全側に
	// 倒している (#1444 IDOR)。
	require.Len(t, resp, 0, "不可視 note の note-required 通知は行ごと drop される (#1953 / #1444)")
}

// 上記の positive counterpart: A が B を follow していれば followers reply
// 通知でも note が full で取れる (= 正当 viewer は regression しない)。
func TestShow_FollowersReply_Follower_NoteVisible(t *testing.T) {
	h, svc := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bobuser"}
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityFollowers,
		User: &model.User{ID: "bob", Username: "bobuser"},
	}
	// alice → bob の follow 関係を張る (CanSeeNote の followers branch が
	// followingRepo.Exists(alice, bob) で true を返す状況)。
	followingRepo.Followings["alice-bob"] = &model.Following{
		ID: "alice-bob", FollowerID: "alice", FolloweeID: "bob",
	}
	h.SetRepos(userRepo, noteRepo)
	h.SetQueryService(corenote.NewQueryService(noteRepo, followingRepo))

	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeReply, NoteID: "n1",
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "reply", resp[0]["type"])
	assert.NotNil(t, resp[0]["note"], "follower 視点では followers note が full で見えること")
}

// regression guard: 自分の note への reaction 通知では引き続き note が full で
// 取れる (= viewer = author の typical case が #1444 修正で壊れていないこと)。
func TestShow_OwnNoteReaction_NoteVisible(t *testing.T) {
	h, svc := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bobuser"}
	// alice 自身の followers note。reaction は bob から来る。
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityFollowers,
		User: &model.User{ID: "alice", Username: "alice"},
	}
	h.SetRepos(userRepo, noteRepo)
	h.SetQueryService(corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository()))

	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeReaction,
		NoteID: "n1", Reaction: "👍",
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "reaction", resp[0]["type"])
	assert.NotNil(t, resp[0]["note"], "viewer = author の reaction 通知では note が full で取れること")
}

// fail-closed: queryService 未配線 + noteRepo 配線という partial setup でも
// note embed が漏れないこと。production の router.go では必ず queryService が
// wire されるが、テスト / future refactor で wire 漏れが起きても #1444 IDOR
// が再発しない安全網。
func TestShow_QueryServiceNil_NoteEmbedDropped(t *testing.T) {
	h, svc := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bobuser"}
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic,
		User: &model.User{ID: "bob", Username: "bobuser"},
	}
	h.SetRepos(userRepo, noteRepo)
	// あえて SetQueryService を呼ばない。

	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeMention, NoteID: "n1",
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	_, hasNote := resp[0]["note"]
	assert.False(t, hasNote, "queryService 未配線時は fail-closed で note embed を skip すること")
	assert.Equal(t, "n1", resp[0]["noteId"], "noteId 自体は echo される")
}

// #2106 N6: i/notifications は notifier user が解決できない notifier-required 通知を drop
// する (upstream #packInternal の needsUser && !userIfNeed)。strict client (misskey_dart 等)
// は Notification.user を non-null 必須で decode するため shape 不整合通知を返さない。
func TestShow_UnresolvedNotifierDropped(t *testing.T) {
	h, svc, userRepo, _ := groupedHandler(t)
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"}
	// bob は userRepo に登録しない → 解決不能 → 通知ごと drop。
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "carol", Type: notification.TypeFollow})
	require.NoError(t, err)
	_, err = svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{"limit":50}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1, "bob (解決不能 notifier) の follow 通知は drop、carol のみ残る")
	assert.Equal(t, "carol", resp[0]["userId"])
}

// #2735: 通知に埋め込む note の `files` が解決されること。packer は Files を空
// スライスで初期化するだけなので、resolver 未配線だと fileIds はあるのに files が
// 空のまま返り、通知ページの MkNote から添付メディアが消える。
func TestShow_ResolvesNoteFiles(t *testing.T) {
	h, svc := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	driveRepo := testutil.NewMockDriveFileRepository()
	driveRepo.Files["f1"] = &model.DriveFile{
		ID: "f1", UserID: strPtr("bob"), Name: "a.png", Type: "image/png",
		URL: "https://example.com/f1",
	}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bobuser"}
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "bob", Visibility: "public",
		FileIDs: model.StringArray{"f1"},
		User:    &model.User{ID: "bob", Username: "bobuser"},
	}
	h.SetRepos(userRepo, noteRepo)
	h.SetQueryService(corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository()))
	h.SetNoteFieldResolver(entity.NewNoteFieldResolver(driveRepo, nil, nil, nil, nil, idGen))

	_, err := svc.Create(context.Background(), notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeMention, NoteID: "n1",
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	note := resp[0]["note"].(map[string]any)
	files := note["files"].([]any)
	require.Len(t, files, 1)
	assert.Equal(t, "f1", files[0].(map[string]any)["id"])
}

// #1570 との関係: hide された embed には添付が乗らず、同じレスポンス内の可視な
// top-level には乗ること (= gate が「常に空にする」で通っているのではないこと)。
//
// **順序 (resolve → hide) はここでは固定できない。** entity.HideNoteEntity は Files
// だけでなく FileIDs も空にするので、仮に hide の後に resolver を走らせても拾う ID が
// 0 件で、添付が書き戻ることは無い。順序に意味があるのは hide が消さない側の field
// (channel / myReaction) で、そちらは upstream hideNote も消さないので現行順序が parity。
func TestShow_HiddenEmbedKeepsFilesEmpty(t *testing.T) {
	h, svc := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	driveRepo := testutil.NewMockDriveFileRepository()
	driveRepo.Files["f1"] = &model.DriveFile{
		ID: "f1", UserID: strPtr("bob"), Name: "visible.png", Type: "image/png",
		URL: "https://example.com/f1",
	}
	driveRepo.Files["f2"] = &model.DriveFile{
		ID: "f2", UserID: strPtr("carol"), Name: "secret.png", Type: "image/png",
		URL: "https://example.com/f2",
	}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bobuser"}
	// carol の followers note を bob が renote し、それが alice への mention 通知の
	// note になる。alice は carol を follow していないので embed は hide される。
	renoteID := "n2"
	noteRepo.Notes["n2"] = &model.Note{
		ID: "n2", UserID: "carol", Visibility: model.NoteVisibilityFollowers,
		FileIDs: model.StringArray{"f2"},
		User:    &model.User{ID: "carol", Username: "carol"},
	}
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "bob", Visibility: "public",
		FileIDs:  model.StringArray{"f1"},
		RenoteID: &renoteID,
		Renote:   noteRepo.Notes["n2"],
		User:     &model.User{ID: "bob", Username: "bobuser"},
	}
	h.SetRepos(userRepo, noteRepo)
	h.SetQueryService(corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository()))
	h.SetNoteFieldResolver(entity.NewNoteFieldResolver(driveRepo, nil, nil, nil, nil, idGen))

	_, err := svc.Create(context.Background(), notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeMention, NoteID: "n1",
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	note := resp[0]["note"].(map[string]any)
	require.Len(t, note["files"].([]any), 1, "可視な top-level の添付は返る")
	renote, ok := note["renote"].(map[string]any)
	require.True(t, ok, "renote embed が返ること")
	assert.Equal(t, true, renote["isHidden"])
	assert.Empty(t, renote["files"], "hide された embed に添付が書き戻されないこと")
}

// #2735 の positive control: **embed 側にも** files が乗ること。hide 側のテスト
// (TestShow_HiddenEmbedKeepsFilesEmpty) は embed が空であることしか見ないので、
// resolver が embed へ再帰しなくなっても気付けない。
//
// resolver の再帰そのものは entity 側の
// TestNoteFieldResolver_ResolveFiles_EmbedsRenoteAndReply も守っている。ここの固有の
// 価値は**通知経路の hide gate が可視 embed まで潰していない**ことの確認。
func TestShow_VisibleEmbedCarriesFiles(t *testing.T) {
	h, svc := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	driveRepo := testutil.NewMockDriveFileRepository()
	driveRepo.Files["f1"] = &model.DriveFile{
		ID: "f1", UserID: strPtr("bob"), Name: "top.png", Type: "image/png",
		URL: "https://example.com/f1",
	}
	driveRepo.Files["f2"] = &model.DriveFile{
		ID: "f2", UserID: strPtr("carol"), Name: "embed.png", Type: "image/png",
		URL: "https://example.com/f2",
	}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bobuser"}
	renoteID := "n2"
	noteRepo.Notes["n2"] = &model.Note{
		ID: "n2", UserID: "carol", Visibility: "public",
		FileIDs: model.StringArray{"f2"},
		User:    &model.User{ID: "carol", Username: "carol"},
	}
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "bob", Visibility: "public",
		FileIDs:  model.StringArray{"f1"},
		RenoteID: &renoteID,
		Renote:   noteRepo.Notes["n2"],
		User:     &model.User{ID: "bob", Username: "bobuser"},
	}
	h.SetRepos(userRepo, noteRepo)
	h.SetQueryService(corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository()))
	h.SetNoteFieldResolver(entity.NewNoteFieldResolver(driveRepo, nil, nil, nil, nil, idGen))

	_, err := svc.Create(context.Background(), notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeMention, NoteID: "n1",
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	note := resp[0]["note"].(map[string]any)
	require.Len(t, note["files"].([]any), 1)
	renote, ok := note["renote"].(map[string]any)
	require.True(t, ok, "renote embed が返ること")
	files, ok := renote["files"].([]any)
	require.True(t, ok)
	require.Len(t, files, 1, "可視な embed にも添付が乗ること")
	df := files[0].(map[string]any)
	assert.Equal(t, "f2", df["id"])
}

// upstream i/notifications.ts が getNotifications の前に持つ 2 つの早期 return
// と同じく、type filter だけで結果が空と決まる fetch では既読化しない (#2835)。
//
// **既読位置が飛ぶのが問題。** MarkAllAsRead が進める先は fetch が返した行では
// なくストリームの最新エントリなので、1 件も返していないのに未読が全部消える。
func TestShow_IncludeTypesEmptyDoesNotMarkAsRead(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{"includeTypes":[]}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	// **wire 上 `[]` であること。** nil を返す変異は `var resp []map[string]any`
	// への Unmarshal では nil slice になって Empty を通ってしまう。misskey-js /
	// misskey_dart は Notification[] を non-null で受けるので、null が出ると
	// 通知ページごと落ちる。
	assert.Equal(t, "[]", strings.TrimSpace(rec.Body.String()))

	assert.NotContains(t, pub.types("alice"), "readAllNotifications",
		"explicit includeTypes:[] must not publish readAllNotifications")
	readID, err := svc.LatestReadID(ctx, "alice")
	require.NoError(t, err)
	assert.Empty(t, readID, "explicit includeTypes:[] must not advance the read marker")
}

// excludeTypes が upstream notificationTypes を全て覆う場合も同じ (#2835)。
// **obsolete type は数えない** — 覆う対象は notificationTypeList の 20 種だけで、
// pollVote / groupInvited を足さなくても早期 return する。
func TestShow_ExcludeAllTypesDoesNotMarkAsRead(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	body, err := json.Marshal(map[string]any{"excludeTypes": notificationTypeList})
	require.NoError(t, err)
	c, rec := newJSONRequest(t, "/api/i/notifications", string(body))
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]", strings.TrimSpace(rec.Body.String()))

	assert.NotContains(t, pub.types("alice"), "readAllNotifications",
		"excludeTypes covering every type must not publish readAllNotifications")
	readID, err := svc.LatestReadID(ctx, "alice")
	require.NoError(t, err)
	assert.Empty(t, readID, "excludeTypes covering every type must not advance the read marker")
}

// 1 つでも残っていれば早期 return しない = 従来どおり fetch して既読化する。
// これが無いと「excludeTypes が空でなければ常に空を返す」形の変異が素通りする。
func TestShow_ExcludeAllButOneStillMarksAsRead(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	// "follow" だけ残す。行は返るので既読化する。
	rest := make([]string, 0, len(notificationTypeList))
	for _, t2 := range notificationTypeList {
		if t2 != "follow" {
			rest = append(rest, t2)
		}
	}
	body, err := json.Marshal(map[string]any{"excludeTypes": rest})
	require.NoError(t, err)
	c, rec := newJSONRequest(t, "/api/i/notifications", string(body))
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Contains(t, pub.types("alice"), "readAllNotifications",
		"one remaining type means the query still runs and marks as read")
	readID, err := svc.LatestReadID(ctx, "alice")
	require.NoError(t, err)
	assert.NotEmpty(t, readID)
}

// enum 検証は早期 return より先 (#2835)。upstream は ajv の paramDef 検証が
// handler 本体より前に走るので、不正な type を含むリクエストは 200 [] ではなく
// 400 になる。emptyByTypeFilter を bindListRequest より前に出す変異が
// 素通りしないよう固定する。
func TestShow_InvalidTypeIsRejectedBeforeEarlyReturn(t *testing.T) {
	h, _ := newTestHandler(t)
	c, rec := newJSONRequest(t, "/api/i/notifications",
		`{"includeTypes":[],"excludeTypes":["bogus"]}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"enum validation must run before the type-filter early return")
}

// notificationTypeEnum は 2 つの list から組み立てる。手で二重に持つと片方が
// 古いまま残り、emptyByTypeFilter の全指定判定が足りない type を無視して
// **早く空を返しすぎる**方向に壊れる。
func TestNotificationTypeEnumMatchesLists(t *testing.T) {
	// **内容をリテラルで固定する。** 件数と自己参照ループだけだと、list から
	// 1 つ落としても両辺が同時に減って通ってしまう (変異で確認)。落ちた type は
	// enum からも消えるので、正当な excludeTypes が 400 になり、同時に
	// emptyByTypeFilter の被覆集合が縮んで「早く空を返しすぎる」側に倒れる。
	//
	// **突き合わせ先は registry の KindUpstream (#2898)。** notificationTypeList
	// (= UpstreamTypeNames) と同じ集合になるので直接比較でも今は通るが、
	// **見たいのは「upstream 由来と宣言した集合」**であって被覆判定用の list では
	// ない。両者を同一視すると、被覆判定の中身を変えたときにこのテストの意味も
	// 一緒に変わる (初版で固有型を list に入れて既読位置が飛ぶ回帰を起こしたとき、
	// テストは緑のままだった)。
	var upstreamTypes []string
	for _, d := range notification.Descriptors() {
		if d.Kind == notification.KindUpstream {
			upstreamTypes = append(upstreamTypes, string(d.Type))
		}
	}
	assert.Equal(t, []string{
		"note", "follow", "mention", "reply", "renote",
		"quote", "reaction", "pollEnded", "scheduledNotePosted",
		"scheduledNotePostFailed", "receiveFollowRequest", "followRequestAccepted",
		"roleAssigned", "chatRoomInvitationReceived", "achievementEarned",
		"exportCompleted", "login", "createToken", "app", "test",
	}, upstreamTypes, "upstream types.ts の notificationTypes と一致すること")
	assert.Equal(t, []string{"pollVote", "groupInvited"}, obsoleteNotificationTypeList,
		"upstream types.ts の obsoleteNotificationTypes と一致すること")

	// notificationTypeList は upstream 由来を全て含み、かつ obsolete を含まない。
	for _, ty := range upstreamTypes {
		assert.Contains(t, notificationTypeList, ty,
			"%s is an upstream type and must be counted by emptyByTypeFilter", ty)
	}

	// enum は upstream + obsolete + mk-go 固有。全指定判定 (notificationTypeList)
	// は upstream のみ — 固有型を入れると upstream 由来のクライアントの
	// 「すべて無効」が被覆判定を外れ、既読位置が飛ぶ (#2898)。
	mkgoTypes := notification.MkGoTypeNames()
	require.NotEmpty(t, mkgoTypes)
	require.Len(t, notificationTypeEnum,
		len(notificationTypeList)+len(obsoleteNotificationTypeList)+len(mkgoTypes))
	for _, ty := range notificationTypeList {
		assert.True(t, notificationTypeEnum[ty], "%s must be in the enum", ty)
	}
	for _, ty := range obsoleteNotificationTypeList {
		assert.True(t, notificationTypeEnum[ty], "%s must be in the enum", ty)
	}
	for _, ty := range mkgoTypes {
		assert.True(t, notificationTypeEnum[ty], "%s must be in the enum", ty)
		assert.NotContains(t, notificationTypeList, ty,
			"%s は mk-go 固有なので全指定判定に入れてはいけない", ty)
	}
	// obsolete は全指定判定の対象外。
	for _, ty := range obsoleteNotificationTypeList {
		assert.NotContains(t, notificationTypeList, ty,
			"%s is obsolete and must not be counted by emptyByTypeFilter", ty)
	}
}

// includeTypes が obsolete type だけのときは「filter 無し = 全件」になる (#2837)。
//
// upstream は早期 return の後に obsolete を除去し、除去後に空になった includeTypes は
// `includeTypes && length > 0` の分岐に入らないので filter が掛からない。
// mk-go は除去せず渡していたため全件落ちて `[]` を返していた。
//
// **`includeTypes:[]` とは結果が逆になる。** あちらは明示的に「何も含めない」で
// 空配列。順序 (早期 return が先、obsolete 除去が後) がこの差を作る。
func TestShow_IncludeTypesObsoleteOnlyReturnsAll(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{"includeTypes":["pollVote"]}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1, "obsolete-only includeTypes means no filter, so all rows come back")
	assert.Equal(t, "follow", resp[0]["type"])
}

// obsolete と実在 type の混在では、実在 type だけが残って filter として効く。
// これが無いと「obsolete を含むなら filter を丸ごと捨てる」形の変異が素通りする。
func TestShow_IncludeTypesObsoleteMixedStillFilters(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)
	_, err = svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeMention, NoteID: "n1",
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications",
		`{"includeTypes":["follow","pollVote"]}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1, "the non-obsolete half must still filter")
	assert.Equal(t, "follow", resp[0]["type"])
}

// excludeTypes 側も除去する。obsolete を混ぜても実在 type の除外は効き続ける。
func TestShow_ExcludeTypesObsoleteMixedStillFilters(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)
	_, err = svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeMention, NoteID: "n1",
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications",
		`{"excludeTypes":["follow","pollVote"]}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "mention", resp[0]["type"])
}

// stripObsoleteTypes の単体。nil / 空 / 全除去 / 部分除去 / 除去なしを固定する。
func TestStripObsoleteTypes(t *testing.T) {
	assert.Nil(t, stripObsoleteTypes(nil))
	// 明示的な空はそのまま (非 nil の長さ 0)。全除去は nil。**この 2 つを混ぜない。**
	// 前者は「何も含めない」、後者は「filter 無し」で意味が逆。
	explicitEmpty := stripObsoleteTypes([]string{})
	assert.NotNil(t, explicitEmpty, "explicit [] must stay non-nil")
	assert.Empty(t, explicitEmpty)
	assert.Nil(t, stripObsoleteTypes([]string{"pollVote", "groupInvited"}),
		"fully stripped means no filter, which must be nil")
	assert.Equal(t, []string{"follow"}, stripObsoleteTypes([]string{"follow", "pollVote"}))
	assert.Equal(t, []string{"follow", "mention"},
		stripObsoleteTypes([]string{"follow", "mention"}))
}

// excludeTypes が obsolete だけなら除去後に nil になり、filter が掛からない (#2837)。
//
// **obsolete type の行を実際に積んで確かめる。** 混在ケース
// (TestShow_ExcludeTypesObsoleteMixedStillFilters) は該当行が無いので strip の
// 有無で結果が変わらず、exclude 側の適用行を消す変異が素通りしていた。
//
// pollVote は notificationTypeList の外にある唯一の type で、現在は producer が
// 無い (#690 で無効化) が、それ以前に積まれた行はストリームに残りうる。
func TestShow_ExcludeTypesObsoleteOnlyDoesNotFilter(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypePollVote, NoteID: "n1",
	})
	require.NoError(t, err)
	_, err = svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications", `{"excludeTypes":["pollVote"]}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 2,
		"obsolete-only excludeTypes is stripped to nil, so nothing is filtered out")
}

// collectNotificationsWithDropped の fail-closed guard を直接固定する (#2837)。
//
// Show / Grouped は emptyByTypeFilter で先に抜けるので**この guard には到達
// しない**。直接呼ぶ経路が増えたときのための防御なので、endpoint 経由の
// テストでは守れない。ここだけは中を直接叩く。
//
// stripObsoleteTypes が全除去で nil を返すのは、除去後の「filter 無し」を
// ここへ「何も含めない」として渡さないため。両者の区別がこの guard の前提。
func TestCollectNotificationsWithDropped_ExplicitEmptyIncludeTypesReturnsNothing(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	limit := 10
	c, _ := newJSONRequest(t, "/api/i/notifications", `{}`)
	user := &model.User{ID: "alice"}

	// 明示的な空 = 何も含めない。
	all, _, _, _, err := h.collectNotificationsWithDropped(c, user,
		ListRequest{Limit: &limit, IncludeTypes: []string{}}, false)
	require.NoError(t, err)
	assert.Empty(t, all, "explicit includeTypes:[] must select nothing")

	// nil = 全 type を通す (対比)。
	all, _, _, _, err = h.collectNotificationsWithDropped(c, user,
		ListRequest{Limit: &limit, IncludeTypes: nil}, false)
	require.NoError(t, err)
	assert.Len(t, all, 1, "nil includeTypes must not filter")
}
