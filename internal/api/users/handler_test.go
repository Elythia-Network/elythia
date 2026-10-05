package users

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func newTestHandler(t *testing.T) (*Handler, *testutil.MockUserRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	fRepo := testutil.NewMockFollowingRepository()
	frRepo := testutil.NewMockFollowRequestRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	fSvc := corefollowing.NewService(userRepo, fRepo, frRepo, idGen)
	h := NewHandler(svc, fSvc, noteRepo, idGen)
	// followingRepo + followRequestRepo を service と共有する形で wire
	// しておく (#1144 で users/{followers,following} の relation flag +
	// pending request flag の batch lookup が handler 直配線の repo 経由に
	// なったため)。explicit setup が要る test では Set*Repo で上書きする。
	h.SetFollowingRepo(fRepo)
	h.SetFollowRequestRepo(frRepo)
	return h, userRepo
}

func addTestUser(repo *testutil.MockUserRepository) *model.User {
	name := "Test User"
	user := &model.User{
		ID:                "user1",
		Username:          "testuser",
		UsernameLower:     "testuser",
		Name:              &name,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	return user
}

// postStub invokes a handler with the given JSON body, optionally setting an
// authenticated user on the context, and returns the recorded response. Shared
// by per-handler test files (achievements_test.go, content_lists_test.go,
// recommendation_test.go, lists_test.go).
func postStub(handler func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
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

func TestShow_ByUserID(t *testing.T) {
	h, userRepo := newTestHandler(t)
	addTestUser(userRepo)

	body := `{"userId": "user1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := h.Show(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "user1", resp["id"])
	assert.Equal(t, "testuser", resp["username"])
	assert.Equal(t, "Test User", resp["name"])
	// L3 (#1270): users/show の実レスポンスを golden User-family schema に突合する。
	shapetest.Assert(t, "UserLite", resp)
	shapetest.Assert(t, "UserDetailedNotMeOnly", resp)
}

// stubChartHook captures users handler chart fires.
type stubChartHook struct {
	calls []struct {
		ownerID, viewerID, visitor string
	}
}

func (s *stubChartHook) OnUserShow(ownerID, viewerID, visitor string) {
	s.calls = append(s.calls, struct {
		ownerID, viewerID, visitor string
	}{ownerID, viewerID, visitor})
}

func TestShow_FiresChartHookAuthenticated(t *testing.T) {
	h, userRepo := newTestHandler(t)
	addTestUser(userRepo)
	hook := &stubChartHook{}
	h.SetChartHook(hook)

	body := `{"userId": "user1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set("misskeyUser", &model.User{ID: "viewer1"})

	require.NoError(t, h.Show(c))
	require.Len(t, hook.calls, 1)
	assert.Equal(t, "user1", hook.calls[0].ownerID)
	assert.Equal(t, "viewer1", hook.calls[0].viewerID)
	assert.Empty(t, hook.calls[0].visitor)
}

func TestShow_FiresChartHookAnonymous(t *testing.T) {
	h, userRepo := newTestHandler(t)
	addTestUser(userRepo)
	hook := &stubChartHook{}
	h.SetChartHook(hook)

	body := `{"userId": "user1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	require.NoError(t, h.Show(c))
	require.Len(t, hook.calls, 1)
	assert.Equal(t, "user1", hook.calls[0].ownerID)
	assert.Equal(t, "", hook.calls[0].viewerID)
	assert.NotEmpty(t, hook.calls[0].visitor) // RemoteAddr is set by httptest
}

func TestShow_ByUsername(t *testing.T) {
	h, userRepo := newTestHandler(t)
	addTestUser(userRepo)

	body := `{"username": "testuser"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := h.Show(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "user1", resp["id"])
}

func TestShow_ByUsernameWithHost(t *testing.T) {
	h, userRepo := newTestHandler(t)

	host := "remote.example.com"
	user := &model.User{
		ID:                "user2",
		Username:          "remoteuser",
		UsernameLower:     "remoteuser",
		Host:              &host,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	userRepo.Users["user2"] = user

	body := `{"username": "remoteuser", "host": "remote.example.com"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := h.Show(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "user2", resp["id"])
}

func TestShow_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)

	body := `{"userId": "nonexistent"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := h.Show(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestShow_DeletedUserIsNotFound(t *testing.T) {
	h, userRepo := newTestHandler(t)
	user := addTestUser(userRepo)
	user.IsDeleted = true

	rec := postStub(h.Show, `{"userId":"user1"}`, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	rec = postStub(h.Show, `{"userIds":["user1"]}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[]`, rec.Body.String())
}

func TestShow_DeletedLocalUserIsVisibleToModerator(t *testing.T) {
	h, userRepo := newTestHandler(t)
	user := addTestUser(userRepo)
	user.IsDeleted = true
	user.IsSuspended = true
	h.SetModeratorChecker(visibilityModStub{modID: "u_mod"})
	moderator := &model.User{ID: "u_mod"}

	rec := postStub(h.Show, `{"userId":"user1"}`, moderator)
	assert.Equal(t, http.StatusOK, rec.Code)
	var single map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &single))
	assert.Equal(t, "user1", single["id"])

	rec = postStub(h.Show, `{"userIds":["user1"]}`, moderator)
	assert.Equal(t, http.StatusOK, rec.Code)
	var bulk []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &bulk))
	require.Len(t, bulk, 1)
	assert.Equal(t, "user1", bulk[0]["id"])
}

func TestShow_DeletedRemoteUserKeepsUpstreamVisibility(t *testing.T) {
	h, userRepo := newTestHandler(t)
	host := "remote.example"
	userRepo.Users["remote1"] = &model.User{
		ID:                "remote1",
		Username:          "alice",
		UsernameLower:     "alice",
		Host:              &host,
		IsDeleted:         true,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	h.SetModeratorChecker(visibilityModStub{modID: "u_mod"})
	viewer := &model.User{ID: "u_plain"}

	rec := postStub(h.Show, `{"userId":"remote1"}`, viewer)
	assert.Equal(t, http.StatusOK, rec.Code)
	var single map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &single))
	assert.Equal(t, "remote1", single["id"])

	rec = postStub(h.Show, `{"userIds":["remote1"]}`, viewer)
	assert.Equal(t, http.StatusOK, rec.Code)
	var bulk []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &bulk))
	require.Len(t, bulk, 1)
	assert.Equal(t, "remote1", bulk[0]["id"])
}

// stubRemoteResolver is a test double for coreuser.RemoteUserResolver.
type stubRemoteResolver struct {
	user *model.User
	err  error
}

func (s *stubRemoteResolver) ResolveByUsernameHost(_, _ string) (*model.User, error) {
	return s.user, s.err
}

func newTestHandlerWithRemoteResolver(t *testing.T, resolver coreuser.RemoteUserResolver) (*Handler, *testutil.MockUserRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	fRepo := testutil.NewMockFollowingRepository()
	frRepo := testutil.NewMockFollowRequestRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	svc.SetRemoteUserResolver(resolver)
	fSvc := corefollowing.NewService(userRepo, fRepo, frRepo, idGen)
	return NewHandler(svc, fSvc, noteRepo, idGen), userRepo
}

func TestShow_ByUsernameWithHost_RemoteResolveSucceeds(t *testing.T) {
	// webfinger + ResolveActor が成功し、返された remote user がそのまま
	// UserDetailed として返されることを確認する。
	host := "remote.example"
	remoteUser := &model.User{
		ID:                "uR",
		Username:          "remote",
		UsernameLower:     "remote",
		Host:              &host,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	h, _ := newTestHandlerWithRemoteResolver(t, &stubRemoteResolver{user: remoteUser})

	body := `{"username":"remote","host":"remote.example"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "uR", resp["id"])
}

func TestShow_ByUsernameWithHost_RemoteResolveFails(t *testing.T) {
	// resolver が error を返す場合、FAILED_TO_RESOLVE_REMOTE_USER がレスポンス
	// として返ることを確認する。HTTP ステータス / JSON の code & id を検証。
	h, _ := newTestHandlerWithRemoteResolver(t, &stubRemoteResolver{err: assert.AnError})

	body := `{"username":"ghost","host":"remote.example"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	require.NoError(t, h.Show(c))
	// upstream users/show は FAILED_TO_RESOLVE_REMOTE_USER に kind:'server' を
	// 指定するため HTTP 500
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj, ok := resp["error"].(map[string]any)
	require.True(t, ok, "response must have error field")
	assert.Equal(t, "FAILED_TO_RESOLVE_REMOTE_USER", errObj["code"])
	// apierr.UUIDFailedToResolveRemoteUser と一致すること。
	assert.Equal(t, "ef7b9be4-9cba-4e6f-ab41-90ed171c7d3c", errObj["id"])
}

func TestShow_MissingParams(t *testing.T) {
	h, _ := newTestHandler(t)

	body := `{}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := h.Show(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_WithProfile(t *testing.T) {
	h, userRepo := newTestHandler(t)
	addTestUser(userRepo)

	desc := "Hello, I'm a test user"
	location := "Tokyo"
	userRepo.Profiles["user1"] = &model.UserProfile{
		UserID:      "user1",
		Description: &desc,
		Location:    &location,
		Fields:      datatypes.JSON([]byte("[]")),
	}

	body := `{"userId": "user1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := h.Show(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "Hello, I'm a test user", resp["description"])
	assert.Equal(t, "Tokyo", resp["location"])
}

func TestShow_InvalidJSON(t *testing.T) {
	h, _ := newTestHandler(t)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader("{invalid"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := h.Show(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_UsernameNotFound(t *testing.T) {
	h, _ := newTestHandler(t)

	body := `{"username": "nonexistent"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := h.Show(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestShow_SelfViewReturnsMeDetailed: #970 — `/api/users/show` で viewer===target
// のとき MeDetailed 拡張 field (isExplorable / noCrawle / emailNotificationTypes
// 等) を merge して返すこと。
func TestShow_SelfViewReturnsMeDetailed(t *testing.T) {
	h, userRepo := newTestHandler(t)

	self := &model.User{
		ID:                "self1",
		Username:          "selfuser",
		IsExplorable:      true,
		IsDeleted:         false,
		HideOnlineStatus:  true,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	userRepo.Users["self1"] = self
	userRepo.Profiles["self1"] = &model.UserProfile{
		UserID:                    "self1",
		NoCrawle:                  true,
		PreventAiLearning:         false,
		AlwaysMarkNsfw:            true,
		EmailNotificationTypes:    datatypes.JSON([]byte(`["mention"]`)),
		NotificationRecieveConfig: datatypes.JSON([]byte(`{"mention":{"type":"following"}}`)),
		Fields:                    datatypes.JSON([]byte("[]")),
	}

	body := `{"userId":"self1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), self)

	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// MeDetailed-only field が出ること
	assert.Equal(t, true, resp["isExplorable"])
	assert.Equal(t, false, resp["isDeleted"])
	assert.Equal(t, true, resp["hideOnlineStatus"])
	assert.Equal(t, true, resp["noCrawle"])
	assert.Equal(t, false, resp["preventAiLearning"])
	assert.Equal(t, true, resp["alwaysMarkNsfw"])
	// #985 notification 3 field (profile JSON column 由来)
	emailTypes, ok := resp["emailNotificationTypes"].([]any)
	require.True(t, ok)
	require.Len(t, emailTypes, 1)
	assert.Equal(t, "mention", emailTypes[0])
	notifConf, ok := resp["notificationRecieveConfig"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, notifConf, "mention")
	// 通常 UserDetailed field も残る
	assert.Equal(t, "self1", resp["id"])
	assert.Equal(t, "selfuser", resp["username"])
	// self-view では relation を計算しないため isFollowing / isFollowed は
	// null ではなく省略されること。null だと misskey_dart の
	// UserDetailedNotMeWithRelations が bool cast で落ちる (#1228)。
	_, hasIsFollowing := resp["isFollowing"]
	assert.False(t, hasIsFollowing, "isFollowing must be omitted (not null) for self-view")
	_, hasIsFollowed := resp["isFollowed"]
	assert.False(t, hasIsFollowed, "isFollowed must be omitted (not null) for self-view")
	// misskey_dart の MeDetailed.fromJson が非null bool として cast する field は
	// self-view でも必ず bool 値で出ること (#1237)。欠落 (null) だと落ちる。
	for _, key := range []string{
		"twoFactorEnabled", "usePasswordLessLogin", "securityKeys",
		"isModerator", "isAdmin",
		"hasUnreadNotification", "hasUnreadMentions", "hasUnreadAnnouncement",
		"hasUnreadAntenna", "hasUnreadChannel", "hasUnreadSpecifiedNotes",
		"hasPendingReceivedFollowRequest",
	} {
		v, ok := resp[key]
		assert.True(t, ok, "%s must be present for self-view MeDetailed", key)
		_, isBool := v.(bool)
		assert.True(t, isBool, "%s must be a non-null bool, got %T", key, v)
	}
	// misskey_dart が非null List として cast する field (#1240)。null だと落ちる。
	for _, key := range []string{"mutedWords", "mutedInstances", "achievements"} {
		v, ok := resp[key]
		assert.True(t, ok, "%s must be present for self-view MeDetailed", key)
		_, isArr := v.([]any)
		assert.True(t, isArr, "%s must be a non-null array, got %T", key, v)
	}
	// loggedInDays は非null num、policies は非null Map (#1240)。
	_, isNum := resp["loggedInDays"].(float64)
	assert.True(t, isNum, "loggedInDays must be a non-null number, got %T", resp["loggedInDays"])
	policies, isMap := resp["policies"].(map[string]any)
	assert.True(t, isMap, "policies must be a non-null object, got %T", resp["policies"])
	assert.NotEmpty(t, policies, "policies must carry default role policy keys")
}

// TestShow_NonSelfViewExcludesMeDetailed: viewer != target なら MeDetailed
// 拡張 field は出さない (privacy boundary)。
func TestShow_NonSelfViewExcludesMeDetailed(t *testing.T) {
	h, userRepo := newTestHandler(t)

	target := &model.User{
		ID:                "target1",
		Username:          "target",
		IsExplorable:      true,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	userRepo.Users["target1"] = target
	userRepo.Profiles["target1"] = &model.UserProfile{
		UserID:                 "target1",
		NoCrawle:               true,
		EmailNotificationTypes: datatypes.JSON([]byte(`["mention"]`)),
		Fields:                 datatypes.JSON([]byte("[]")),
	}
	viewer := &model.User{ID: "viewer1", Username: "viewer"}

	body := `{"userId":"target1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), viewer)

	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// MeDetailed-only field が漏れていないこと
	_, hasIsExplorable := resp["isExplorable"]
	assert.False(t, hasIsExplorable, "isExplorable must not leak for non-self view")
	_, hasNoCrawle := resp["noCrawle"]
	assert.False(t, hasNoCrawle, "noCrawle must not leak for non-self view")
	_, hasEmailNotif := resp["emailNotificationTypes"]
	assert.False(t, hasEmailNotif, "emailNotificationTypes must not leak for non-self view")
}

// TestShow_AnonymousViewExcludesMeDetailed: viewer == nil でも UserDetailed
// shape (= 公開 field のみ) を返す。
func TestShow_AnonymousViewExcludesMeDetailed(t *testing.T) {
	h, userRepo := newTestHandler(t)
	target := &model.User{
		ID:                "target1",
		Username:          "target",
		IsExplorable:      true,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	userRepo.Users["target1"] = target
	userRepo.Profiles["target1"] = &model.UserProfile{
		UserID: "target1",
		Fields: datatypes.JSON([]byte("[]")),
	}

	body := `{"userId":"target1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	// viewer は context に set しない (= anonymous)

	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	_, hasIsExplorable := resp["isExplorable"]
	assert.False(t, hasIsExplorable)
}

func TestShow_ViewerDependentFields(t *testing.T) {
	h, userRepo := newTestHandler(t)

	// ターゲットユーザー
	target := &model.User{
		ID:                "target1",
		Username:          "target",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	userRepo.Users["target1"] = target

	// viewer
	viewer := &model.User{ID: "viewer1", Username: "viewer"}

	// blockingリポジトリをセット (viewerがtargetをブロック)
	blockingRepo := testutil.NewMockBlockingRepository()
	blockingRepo.Blockings["b1"] = &model.Blocking{ID: "b1", BlockerID: "viewer1", BlockeeID: "target1"}
	h.SetBlockingRepo(blockingRepo)

	// mutingリポジトリをセット
	mutingRepo := testutil.NewMockMutingRepository()
	h.SetMutingRepo(mutingRepo)

	// followRequestリポジトリをセット
	followRequestRepo := testutil.NewMockFollowRequestRepository()
	h.SetFollowRequestRepo(followRequestRepo)

	body := `{"userId": "target1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), viewer)

	err := h.Show(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["isBlocking"])
	assert.Equal(t, false, resp["isBlocked"])
	assert.Equal(t, false, resp["isMuted"])
}

func TestShow_ViewerMemo(t *testing.T) {
	h, userRepo := newTestHandler(t)

	target := &model.User{
		ID:                "target1",
		Username:          "target",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	userRepo.Users["target1"] = target

	viewer := &model.User{ID: "viewer1", Username: "viewer"}

	// memoリポジトリをセット
	memoRepo := testutil.NewMockUserMemoRepository()
	memoRepo.Memos["viewer1:target1"] = &model.UserMemo{
		UserID:       "viewer1",
		TargetUserID: "target1",
		Memo:         "important person",
	}
	h.SetMemoRepo(memoRepo)

	// followingリポジトリをセット (notify/withRepliesのテスト用)
	followingRepo := testutil.NewMockFollowingRepository()
	notify := "normal"
	followingRepo.Followings["f1"] = &model.Following{
		ID:          "f1",
		FollowerID:  "viewer1",
		FolloweeID:  "target1",
		Notify:      &notify,
		WithReplies: true,
	}
	h.SetFollowingRepo(followingRepo)

	// renoteMutingリポジトリをセット
	renoteMutingRepo := testutil.NewMockRenoteMutingRepository()
	h.SetRenoteMutingRepo(renoteMutingRepo)

	body := `{"userId": "target1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), viewer)

	err := h.Show(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "important person", resp["memo"])
	assert.Equal(t, "normal", resp["notify"])
	assert.Equal(t, true, resp["withReplies"])
	assert.Equal(t, true, resp["isFollowing"])
	assert.Equal(t, false, resp["isRenoteMuted"])
}

func TestShow_RemoteUserInstance(t *testing.T) {
	h, userRepo := newTestHandler(t)

	host := "remote.example.com"
	target := &model.User{
		ID:                "remote1",
		Username:          "remoteuser",
		Host:              &host,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	userRepo.Users["remote1"] = target

	// instanceリポジトリをセット
	instanceRepo := testutil.NewMockInstanceRepository()
	instName := "Remote Instance"
	softwareName := "misskey"
	instanceRepo.Instances["remote.example.com"] = &model.Instance{
		ID:               "inst1",
		Host:             "remote.example.com",
		Name:             &instName,
		SoftwareName:     &softwareName,
		FirstRetrievedAt: time.Now(),
	}
	h.SetInstanceRepo(instanceRepo)

	body := `{"userId": "remote1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := h.Show(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	inst := resp["instance"].(map[string]any)
	assert.Equal(t, "Remote Instance", inst["name"])
	assert.Equal(t, "misskey", inst["softwareName"])
}

// --- post is a small helper that exercises a handler with an optional body ---

func post(h echo.HandlerFunc, body string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = h(c)
	return rec
}

// --- Search ---

func TestSearch_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)

	rec := post(h.Search, `{"query": "test"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Len(t, out, 1)
	// users/search は PackUserDetailed (UserLite & UserDetailedNotMeOnly) の配列。
	shapetest.Assert(t, "UserLite", out[0])              // L3 (#1314)
	shapetest.Assert(t, "UserDetailedNotMeOnly", out[0]) // L3 (#1314)
}

// #1939: users/search は isSuspended ユーザーを除外する。
func TestSearch_ExcludesSuspendedUser(t *testing.T) {
	h, repo := newTestHandler(t)
	name := "Test Suspended"
	repo.Users["sus1"] = &model.User{ID: "sus1", Username: "testsuspended", UsernameLower: "testsuspended", Name: &name, IsSuspended: true, AvatarDecorations: datatypes.JSON([]byte("[]"))}
	rec := post(h.Search, `{"query":"testsuspended"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Empty(t, out, "suspended user は users/search に出ない")
}

// #1939: users/search は display name 部分一致で hit する (username 不一致でも)。
func TestSearch_MatchesDisplayName(t *testing.T) {
	h, repo := newTestHandler(t)
	name := "Zqx Display Name"
	repo.Users["dn1"] = &model.User{ID: "dn1", Username: "randomhandle", UsernameLower: "randomhandle", Name: &name, AvatarDecorations: datatypes.JSON([]byte("[]"))}
	rec := post(h.Search, `{"query":"zqx display"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1, "display name 部分一致で hit")
	assert.Equal(t, "dn1", out[0]["id"])
}

// users/search bulk path is now batch (#517). Per-row FindProfileByUserID
// must not be called and the new batch FindProfilesByUserIDs is invoked once
// with all matched user IDs.
type countingSearchUserRepo struct {
	*testutil.MockUserRepository
	findProfileByUserIDCalls   int
	findProfilesByUserIDsCalls int
	findProfilesByUserIDsSize  int
}

func (c *countingSearchUserRepo) FindProfileByUserID(id string) (*model.UserProfile, error) {
	c.findProfileByUserIDCalls++
	return c.MockUserRepository.FindProfileByUserID(id)
}

func (c *countingSearchUserRepo) FindProfilesByUserIDs(ids []string) ([]*model.UserProfile, error) {
	c.findProfilesByUserIDsCalls++
	c.findProfilesByUserIDsSize += len(ids)
	return c.MockUserRepository.FindProfilesByUserIDs(ids)
}

func TestSearch_BatchFetchesProfiles(t *testing.T) {
	repo := &countingSearchUserRepo{MockUserRepository: testutil.NewMockUserRepository()}
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	fRepo := testutil.NewMockFollowingRepository()
	frRepo := testutil.NewMockFollowRequestRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(repo, noteRepo, piningRepo, idGen)
	fSvc := corefollowing.NewService(repo, fRepo, frRepo, idGen)
	h := NewHandler(svc, fSvc, noteRepo, idGen)

	for i := 0; i < 5; i++ {
		uid := fmt.Sprintf("searchuser-%d", i)
		repo.Users[uid] = &model.User{
			ID: uid, Username: "matchu" + fmt.Sprintf("%d", i),
			UsernameLower:     "matchu" + fmt.Sprintf("%d", i),
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		}
		desc := "d" + fmt.Sprintf("%d", i)
		repo.Profiles[uid] = &model.UserProfile{UserID: uid, Description: &desc}
	}

	rec := post(h.Search, `{"query":"matchu","limit":10}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 5)

	assert.Equal(t, 0, repo.findProfileByUserIDCalls,
		"per-row FindProfileByUserID must not be called (N+1 must be eliminated)")
	assert.Equal(t, 1, repo.findProfilesByUserIDsCalls,
		"FindProfilesByUserIDs should be called exactly once per request")
	assert.Equal(t, 5, repo.findProfilesByUserIDsSize,
		"all 5 user IDs should be coalesced into a single batch")
}

func TestSearch_LimitZeroRejected(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)

	// upstream は ajv で minimum:1 を強制するので limit:0 は 400。
	rec := post(h.Search, `{"query": "test", "limit": 0}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSearch_InvalidJSON(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := post(h.Search, `{invalid`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// origin enum validation (#763): upstream paramDef enum と一致しない値は
// 400 で reject。空 / local / remote / combined は OK。
func TestSearch_OriginEnumValidation(t *testing.T) {
	h, _ := newTestHandler(t)
	tt := []struct {
		origin string
		want   int
	}{
		{"", http.StatusOK},              // default → combined
		{"local", http.StatusOK},         // upstream enum
		{"remote", http.StatusOK},        // upstream enum
		{"combined", http.StatusOK},      // upstream enum
		{"all", http.StatusBadRequest},   // typo
		{"LOCAL", http.StatusBadRequest}, // case mismatch (upstream は lower-case 厳格)
		{"none", http.StatusBadRequest},  // 別 enum 由来
		{"foo", http.StatusBadRequest},   // garbage
	}
	for _, tc := range tt {
		t.Run(tc.origin, func(t *testing.T) {
			body := `{"query":"test","origin":"` + tc.origin + `"}`
			rec := post(h.Search, body)
			assert.Equal(t, tc.want, rec.Code, "origin=%q expected %d", tc.origin, tc.want)
		})
	}
}

// --- Notes ---

func TestNotes_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	// Insert a note via the noteRepo embedded in handler
	idGen, _ := id.NewGenerator("aidx")
	noteRepo := h.noteRepo.(*testutil.MockNoteRepository)
	noteID := idGen.Generate(time.Now())
	text := "hello"
	noteRepo.Notes[noteID] = &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}

	rec := post(h.Notes, `{"userId": "user1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Len(t, out, 1)
}

// #1766: viewer が target 本人を mute していても、target のプロフィール note は
// mute filter から除外して返す (upstream notes.ts の excludeUserFromMute)。
func TestNotes_ExcludesTargetFromMute(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo) // "user1" を target として追加
	noteRepo := h.noteRepo.(*testutil.MockNoteRepository)
	text := "from target"
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "user1", Text: &text,
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	// viewer1 が target (user1) を mute している。
	mutingRepo := testutil.NewMockMutingRepository()
	require.NoError(t, mutingRepo.Create(&model.Muting{ID: "m1", MuterID: "viewer1", MuteeID: "user1"}))
	h.SetMutingRepo(mutingRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"userId":"user1"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), &model.User{ID: "viewer1"})

	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Len(t, out, 1, "mute した相手本人のプロフィール note は除外されない")
}

// --- #1021: withFiles / withReplies / withRenotes / withChannelNotes filter ---

// 4 種類のノート (text のみ / file 添付あり / reply / pure renote / channel) を
// 同 user に seed し、各 filter で期待数が返ることを確認する。upstream
// `users/notes` paramDef のデフォルトは withFiles=false / withReplies=false /
// withRenotes=true / withChannelNotes=false (#1547)。
func seedNotesForFilter(t *testing.T, repo *testutil.MockNoteRepository) {
	t.Helper()
	text := "plain text"
	repo.Notes["nf_plain"] = &model.Note{
		ID: "nf_plain", UserID: "user1", Text: &text,
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	repo.Notes["nf_file"] = &model.Note{
		ID: "nf_file", UserID: "user1", Text: &text, FileIDs: []string{"f1"},
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	parentID := "nf_plain"
	repo.Notes["nf_reply"] = &model.Note{
		ID: "nf_reply", UserID: "user1", Text: &text, ReplyID: &parentID,
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	renoteID := "external_target"
	repo.Notes["nf_renote"] = &model.Note{
		ID: "nf_renote", UserID: "user1", RenoteID: &renoteID,
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	channelID := "ch1"
	repo.Notes["nf_channel"] = &model.Note{
		ID: "nf_channel", UserID: "user1", Text: &text, ChannelID: &channelID,
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
}

func TestNotes_DefaultFilters(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	noteRepo := h.noteRepo.(*testutil.MockNoteRepository)
	seedNotesForFilter(t, noteRepo)
	rec := post(h.Notes, `{"userId": "user1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	// default: withReplies=false / withChannelNotes=false なので reply と
	// channel が除外され、plain / file / renote の 3 件が残る (#1547)。
	assert.Len(t, out, 3, "default: withReplies=false で reply を、withChannelNotes=false で channel を除外")
	for _, n := range out {
		assert.NotEqual(t, "nf_reply", n["id"], "default で reply が除外される")
		assert.NotEqual(t, "nf_channel", n["id"], "default で channel が除外される")
	}
}

func TestNotes_WithFiles(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	noteRepo := h.noteRepo.(*testutil.MockNoteRepository)
	seedNotesForFilter(t, noteRepo)
	rec := post(h.Notes, `{"userId": "user1", "withFiles": true}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1, "file 添付ノートのみ")
	assert.Equal(t, "nf_file", out[0]["id"])
}

func TestNotes_WithoutReplies(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	noteRepo := h.noteRepo.(*testutil.MockNoteRepository)
	seedNotesForFilter(t, noteRepo)
	rec := post(h.Notes, `{"userId": "user1", "withReplies": false}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	for _, n := range out {
		assert.NotEqual(t, "nf_reply", n["id"], "reply が除外される")
	}
}

// withReplies を明示的に true にすると default (false) を上書きして reply が
// 含まれることを確認する (#1547)。
func TestNotes_WithReplies(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	noteRepo := h.noteRepo.(*testutil.MockNoteRepository)
	seedNotesForFilter(t, noteRepo)
	rec := post(h.Notes, `{"userId": "user1", "withReplies": true}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	var hasReply bool
	for _, n := range out {
		if n["id"] == "nf_reply" {
			hasReply = true
		}
	}
	assert.True(t, hasReply, "withReplies=true で reply が含まれる")
}

// upstream notes.ts:93: withReplies && withFiles の同時指定は
// BOTH_WITH_REPLIES_AND_WITH_FILES (91c8cb9f-...) を 400 で返す (#1547)。
func TestNotes_BothWithRepliesAndWithFiles(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	noteRepo := h.noteRepo.(*testutil.MockNoteRepository)
	seedNotesForFilter(t, noteRepo)
	rec := post(h.Notes, `{"userId": "user1", "withReplies": true, "withFiles": true}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errObj, ok := body["error"].(map[string]any)
	require.True(t, ok, "error object present")
	assert.Equal(t, "BOTH_WITH_REPLIES_AND_WITH_FILES", errObj["code"])
	assert.Equal(t, "91c8cb9f-36ed-46e7-9ca2-7df96ed6e222", errObj["id"])
}

func TestNotes_WithoutRenotes(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	noteRepo := h.noteRepo.(*testutil.MockNoteRepository)
	seedNotesForFilter(t, noteRepo)
	rec := post(h.Notes, `{"userId": "user1", "withRenotes": false}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	for _, n := range out {
		assert.NotEqual(t, "nf_renote", n["id"], "pure renote が除外される")
	}
}

func TestNotes_WithChannelNotes(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	noteRepo := h.noteRepo.(*testutil.MockNoteRepository)
	seedNotesForFilter(t, noteRepo)
	rec := post(h.Notes, `{"userId": "user1", "withChannelNotes": true}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	var hasChannel bool
	for _, n := range out {
		if n["id"] == "nf_channel" {
			hasChannel = true
		}
	}
	assert.True(t, hasChannel, "withChannelNotes=true で channel 投稿が含まれる")
}

// followers / specified visibility のノートは閲覧権限のない viewer に対して
// 除外されることを guard する。
func TestNotes_AnonymousExcludesNonPublicVisibility(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	noteRepo := h.noteRepo.(*testutil.MockNoteRepository)
	text := "secret"
	noteRepo.Notes["nv_pub"] = &model.Note{
		ID: "nv_pub", UserID: "user1", Text: &text,
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	noteRepo.Notes["nv_fol"] = &model.Note{
		ID: "nv_fol", UserID: "user1", Text: &text,
		Visibility: model.NoteVisibilityFollowers, Reactions: datatypes.JSON([]byte("{}")),
	}
	noteRepo.Notes["nv_spec"] = &model.Note{
		ID: "nv_spec", UserID: "user1", Text: &text,
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: []string{"other"},
		Reactions:      datatypes.JSON([]byte("{}")),
	}

	rec := post(h.Notes, `{"userId":"user1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	ids := map[string]bool{}
	for _, n := range out {
		ids[n["id"].(string)] = true
	}
	assert.True(t, ids["nv_pub"], "public は anonymous で見える")
	assert.False(t, ids["nv_fol"], "followers は anonymous に漏らさない")
	assert.False(t, ids["nv_spec"], "specified は対象外 viewer に漏らさない")
}

// follower 関係にある viewer は followers visibility のノートを閲覧可能。
func TestNotes_FollowerSeesFollowersVisibility(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	noteRepo := h.noteRepo.(*testutil.MockNoteRepository)
	text := "for-followers"
	noteRepo.Notes["nv_fol2"] = &model.Note{
		ID: "nv_fol2", UserID: "user1", Text: &text,
		Visibility: model.NoteVisibilityFollowers, Reactions: datatypes.JSON([]byte("{}")),
	}
	// visibility は repository 側で push down されるため、mock note repo の
	// Following map に follow 関係を持たせる (handler は viewerID を渡すだけ)。
	noteRepo.Following = map[string][]string{"viewer": {"user1"}}

	rec := postStub(h.Notes, `{"userId":"user1"}`, &model.User{ID: "viewer"})
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.Equal(t, "nv_fol2", out[0]["id"])
}

func TestNotes_LimitOutOfRangeRejected(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	// upstream は ajv で maximum を強制するので範囲外は 400。
	rec := post(h.Notes, `{"userId": "user1", "limit": 9999}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #2106 L9: 存在しない userId は upstream notes.ts 同様 200 [] を返す (404 でない)。
func TestNotes_UserNotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := post(h.Notes, `{"userId": "ghost"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[]`, rec.Body.String())
}

func TestNotes_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := post(h.Notes, `{}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Followers / Following ---

func TestFollowers_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	// Add a follower relationship
	repo.Users["follower1"] = &model.User{
		ID:                "follower1",
		Username:          "follower1",
		UsernameLower:     "follower1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	fSvc := h.followingService
	_, err := fSvc.Follow("follower1", "user1", corefollowing.FollowOptions{})
	require.NoError(t, err)

	rec := post(h.Followers, `{"userId": "user1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Len(t, out, 1)
	// misskey_dart の Following.fromJson は createdAt / followerId / followeeId を
	// 非null必須とする (#1243)。
	createdAt, ok := out[0]["createdAt"].(string)
	assert.True(t, ok, "createdAt must be a non-null string")
	assert.NotEmpty(t, createdAt)
	assert.Equal(t, "follower1", out[0]["followerId"])
	assert.Equal(t, "user1", out[0]["followeeId"])
	shapetest.Assert(t, "Following", out[0]) // L3 (#1330)
}

// TestFollowers_PopulatesIsFollowedFromViewer guards #1144: when a viewer
// (auth user) requests users/followers of a profile, each follower row's
// embedded `follower` UserDetailed must carry `isFollowed` from the
// viewer's perspective (= "does this follower follow me?"). frontend
// MkUserInfo renders the "follows you" label based on this flag.
func TestFollowers_PopulatesIsFollowedFromViewer(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo) // user1 = target profile
	// viewer = bob, follower = alice
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob",
		AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["alice"] = &model.User{ID: "alice", Username: "alice", UsernameLower: "alice",
		AvatarDecorations: datatypes.JSON([]byte("[]"))}
	// alice follows user1 (= alice 表示される follower)、
	// 加えて alice follows bob (viewer) → isFollowed = true 期待。
	fSvc := h.followingService
	_, err := fSvc.Follow("alice", "user1", corefollowing.FollowOptions{})
	require.NoError(t, err)
	_, err = fSvc.Follow("alice", "bob", corefollowing.FollowOptions{})
	require.NoError(t, err)
	// 本番と同じく関係の repo をすべて配線する (未配線の関係は、一覧共通の
	// packer が項目ごと出さない)。
	h.SetBlockingRepo(testutil.NewMockBlockingRepository())
	h.SetMutingRepo(testutil.NewMockMutingRepository())
	h.SetRenoteMutingRepo(testutil.NewMockRenoteMutingRepository())

	rec := postStub(h.Followers, `{"userId":"user1"}`, repo.Users["bob"])
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	follower, ok := out[0]["follower"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, follower["isFollowed"], "alice follows viewer(bob) → isFollowed=true")
	// bob は alice を follow していない → isFollowing=false。
	assert.Equal(t, false, follower["isFollowing"])
	// #1249: isFollowing が present のため misskey_dart は WithRelations variant を
	// 選び、isBlocking/isBlocked/isMuted/isRenoteMuted も非null bool として cast
	// する。関係が無ければ false で、全て present であること。
	for _, key := range []string{"isBlocking", "isBlocked", "isMuted", "isRenoteMuted", "hasPendingFollowRequestFromYou", "hasPendingFollowRequestToYou"} {
		v, present := follower[key]
		assert.True(t, present, "%s must be present (WithRelations variant)", key)
		_, isBool := v.(bool)
		assert.True(t, isBool, "%s must be a non-null bool, got %T", key, v)
	}
}

// TestFollowers_PopulatesPendingFollowRequest guards #1144 #2: locked
// account からの pending follow request 状態でも MkFollowButton が正しく
// "follow request pending" 表示にできるよう、
// `hasPendingFollowRequestFromYou` / `hasPendingFollowRequestToYou` を
// embed UserDetailed に埋める。
func TestFollowers_PopulatesPendingFollowRequest(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo) // user1 = target profile
	locked := true
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob",
		IsLocked: locked, AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["alice"] = &model.User{ID: "alice", Username: "alice", UsernameLower: "alice",
		IsLocked: locked, AvatarDecorations: datatypes.JSON([]byte("[]"))}
	fSvc := h.followingService
	// alice follows user1 → alice が表示される follower。
	_, err := fSvc.Follow("alice", "user1", corefollowing.FollowOptions{})
	require.NoError(t, err)
	// viewer(bob) が alice (locked) に follow request 送信中 →
	// alice.hasPendingFollowRequestFromYou=true 期待。
	_, err = fSvc.Follow("bob", "alice", corefollowing.FollowOptions{})
	require.NoError(t, err) // locked なので pending request になる

	rec := postStub(h.Followers, `{"userId":"user1"}`, repo.Users["bob"])
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	follower, ok := out[0]["follower"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, follower["hasPendingFollowRequestFromYou"],
		"viewer(bob) → alice (locked) に pending request → fromYou=true")
	assert.Equal(t, false, follower["hasPendingFollowRequestToYou"])
}

// fakeRemoteStatsFetcher is a stub for users.RemoteStatsFetcher returning
// canned stats per (host, username) pair. Used by remote stats override
// regression tests (#1146).
type fakeRemoteStatsFetcher struct {
	stats map[string]*RemoteUserStatsView // key = host + "|" + username
	calls int
}

func (f *fakeRemoteStatsFetcher) Fetch(_ context.Context, host, username string) *RemoteUserStatsView {
	f.calls++
	return f.stats[host+"|"+username]
}

// TestFollowers_AppliesRemoteStatsOverride guards #1146: remote user の
// notesCount / followersCount / followingCount は origin instance の
// /api/users/show から取得した値で上書きされる (RemoteStatsFetcher 経由)。
// 旧来は Show 経路だけで適用されていて、list 経路 (followers/following) では
// ローカル観測値のままだった。
func TestFollowers_AppliesRemoteStatsOverride(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo) // user1 = local target profile
	remoteHost := "remote.example"
	repo.Users["remote-alice"] = &model.User{
		ID:                "remote-alice",
		Username:          "alice",
		UsernameLower:     "alice",
		Host:              &remoteHost,
		NotesCount:        5,  // ローカル観測値 (= 古い)
		FollowersCount:    10, // ローカル観測値
		FollowingCount:    3,  // ローカル観測値
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	// profile 無しの利用者はカウントを伏せる (#3330) ので、実際の行と同じく profile を置く。
	repo.Profiles["remote-alice"] = &model.UserProfile{UserID: "remote-alice"}
	// remote-alice が user1 を follow → list に出る。
	fSvc := h.followingService
	_, err := fSvc.Follow("remote-alice", "user1", corefollowing.FollowOptions{})
	require.NoError(t, err)
	// fetcher は remote-alice に対して大きい本物の値を返す。
	h.SetRemoteStatsFetcher(&fakeRemoteStatsFetcher{
		stats: map[string]*RemoteUserStatsView{
			"remote.example|alice": {NotesCount: 500, FollowersCount: 1000, FollowingCount: 250},
		},
	})

	rec := post(h.Followers, `{"userId":"user1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	follower, ok := out[0]["follower"].(map[string]any)
	require.True(t, ok)
	// 上書き後の remote 実値が反映されていることを assert (= 旧 5/10/3 では
	// なく 500/1000/250)。
	assert.Equal(t, float64(500), follower["notesCount"])
	assert.Equal(t, float64(1000), follower["followersCount"])
	assert.Equal(t, float64(250), follower["followingCount"])
}

// TestFollowing_AppliesRemoteStatsOverride covers the symmetric path for
// /api/users/following — same helper is used so symmetry is expected, but
// explicit regression guard against prefix routing or pack-side regressions.
func TestFollowing_AppliesRemoteStatsOverride(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo) // user1 = local target profile
	remoteHost := "remote.example"
	repo.Users["remote-charlie"] = &model.User{
		ID:                "remote-charlie",
		Username:          "charlie",
		UsernameLower:     "charlie",
		Host:              &remoteHost,
		NotesCount:        1,
		FollowersCount:    2,
		FollowingCount:    3,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	// profile 無しの利用者はカウントを伏せる (#3330) ので、実際の行と同じく profile を置く。
	repo.Profiles["remote-charlie"] = &model.UserProfile{UserID: "remote-charlie"}
	fSvc := h.followingService
	// user1 follows charlie → charlie が followee として list される。
	_, err := fSvc.Follow("user1", "remote-charlie", corefollowing.FollowOptions{})
	require.NoError(t, err)
	h.SetRemoteStatsFetcher(&fakeRemoteStatsFetcher{
		stats: map[string]*RemoteUserStatsView{
			"remote.example|charlie": {NotesCount: 9000, FollowersCount: 8000, FollowingCount: 7000},
		},
	})

	rec := post(h.Following, `{"userId":"user1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	followee, ok := out[0]["followee"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(9000), followee["notesCount"])
	assert.Equal(t, float64(8000), followee["followersCount"])
	assert.Equal(t, float64(7000), followee["followingCount"])
}

// TestFollowers_RemoteStatsOverride_FallsBackOnFetchError: fetcher が nil
// (= 取得失敗 / 未登録) を返した remote user は local 観測値を維持して
// silent fallback する (= upstream Show 経路と同 pattern)。
func TestFollowers_RemoteStatsOverride_FallsBackOnFetchError(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	remoteHost := "remote.example"
	repo.Users["remote-dora"] = &model.User{
		ID:                "remote-dora",
		Username:          "dora",
		UsernameLower:     "dora",
		Host:              &remoteHost,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	// profile 無しの利用者はカウントを伏せる (#3330) ので、実際の行と同じく profile を置く。
	repo.Profiles["remote-dora"] = &model.UserProfile{UserID: "remote-dora"}
	fSvc := h.followingService
	_, err := fSvc.Follow("remote-dora", "user1", corefollowing.FollowOptions{})
	require.NoError(t, err)
	// fSvc.Follow が user.FollowingCount を auto-increment するので、
	// test の baseline は Follow 後に固定 (= override しない事実だけ確認)。
	repo.Users["remote-dora"].NotesCount = 77
	repo.Users["remote-dora"].FollowersCount = 99
	repo.Users["remote-dora"].FollowingCount = 11
	// fetcher は dora 用の stats を登録しない → Fetch が nil 返却 → fallback。
	h.SetRemoteStatsFetcher(&fakeRemoteStatsFetcher{stats: map[string]*RemoteUserStatsView{}})

	rec := post(h.Followers, `{"userId":"user1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	follower, ok := out[0]["follower"].(map[string]any)
	require.True(t, ok)
	// fetch 失敗時は local 観測値そのまま (override しない)。
	assert.Equal(t, float64(77), follower["notesCount"], "fetcher nil → local count fallback")
	assert.Equal(t, float64(99), follower["followersCount"])
	assert.Equal(t, float64(11), follower["followingCount"])
}

// TestFollowers_RemoteStatsOverride_SkipsLocalUser: local user (Host==nil) は
// fetcher 経路を skip し、ローカル観測値をそのまま使う。HTTP request 削減。
func TestFollowers_RemoteStatsOverride_SkipsLocalUser(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo) // user1 = local target
	repo.Users["local-bob"] = &model.User{
		ID:                "local-bob",
		Username:          "bob",
		UsernameLower:     "bob",
		NotesCount:        42,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	fSvc := h.followingService
	_, err := fSvc.Follow("local-bob", "user1", corefollowing.FollowOptions{})
	require.NoError(t, err)
	fetcher := &fakeRemoteStatsFetcher{stats: map[string]*RemoteUserStatsView{}}
	h.SetRemoteStatsFetcher(fetcher)

	rec := post(h.Followers, `{"userId":"user1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 0, fetcher.calls, "local user (Host==nil) は fetcher を呼ばない")
}

// TestFollowing_PopulatesIsFollowedFromViewer covers the symmetric case
// for /api/users/following — each `followee` UserDetailed gets viewer's
// relation flags.
func TestFollowing_PopulatesIsFollowedFromViewer(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo) // user1 = target profile
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob",
		AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["charlie"] = &model.User{ID: "charlie", Username: "charlie", UsernameLower: "charlie",
		AvatarDecorations: datatypes.JSON([]byte("[]"))}
	fSvc := h.followingService
	// user1 follows charlie → charlie が followee として list される。
	_, err := fSvc.Follow("user1", "charlie", corefollowing.FollowOptions{})
	require.NoError(t, err)
	// viewer(bob) follows charlie → isFollowing=true、charlie は bob を
	// follow していない → isFollowed=false。
	_, err = fSvc.Follow("bob", "charlie", corefollowing.FollowOptions{})
	require.NoError(t, err)

	rec := postStub(h.Following, `{"userId":"user1"}`, repo.Users["bob"])
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	followee, ok := out[0]["followee"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, followee["isFollowing"], "viewer(bob) follows charlie → isFollowing=true")
	assert.Equal(t, false, followee["isFollowed"])
}

// users/followers が follower user の取得を per-row ShowByID で叩いて
// いた N+1 を ShowManyByIDs 1 batch に置換した (#300 2-3)。
// 5 follower 検証で per-row FindByID + FindProfileByUserID が 0 回、
// FindManyByIDs / FindProfilesByUserIDs が 1 回ずつだけ呼ばれることを担保。
type countingFollowerUserRepo struct {
	*testutil.MockUserRepository
	findByIDCalls            int
	findProfileByUserIDCalls int
	findManyByIDsCalls       int
	findProfilesByUserIDs    int
	findManyByIDsCallSize    int
}

func (c *countingFollowerUserRepo) FindByID(id string) (*model.User, error) {
	c.findByIDCalls++
	return c.MockUserRepository.FindByID(id)
}

func (c *countingFollowerUserRepo) FindProfileByUserID(id string) (*model.UserProfile, error) {
	c.findProfileByUserIDCalls++
	return c.MockUserRepository.FindProfileByUserID(id)
}

func (c *countingFollowerUserRepo) FindManyByIDs(ids []string) ([]*model.User, error) {
	c.findManyByIDsCalls++
	c.findManyByIDsCallSize += len(ids)
	return c.MockUserRepository.FindManyByIDs(ids)
}

func (c *countingFollowerUserRepo) FindProfilesByUserIDs(ids []string) ([]*model.UserProfile, error) {
	c.findProfilesByUserIDs++
	return c.MockUserRepository.FindProfilesByUserIDs(ids)
}

func TestFollowers_BatchFetchesUsers(t *testing.T) {
	repo := &countingFollowerUserRepo{MockUserRepository: testutil.NewMockUserRepository()}
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	fRepo := testutil.NewMockFollowingRepository()
	frRepo := testutil.NewMockFollowRequestRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(repo, noteRepo, piningRepo, idGen)
	fSvc := corefollowing.NewService(repo, fRepo, frRepo, idGen)
	h := NewHandler(svc, fSvc, noteRepo, idGen)

	// target user (followee)
	repo.Users["target"] = &model.User{
		ID: "target", Username: "target", UsernameLower: "target",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	for i := 0; i < 5; i++ {
		uid := fmt.Sprintf("rel-f-%d", i)
		repo.Users[uid] = &model.User{
			ID: uid, Username: uid, UsernameLower: uid,
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		}
		_, err := fSvc.Follow(uid, "target", corefollowing.FollowOptions{})
		require.NoError(t, err)
	}

	// `followingService.Follow` が内部で repo.FindByID 経由でユーザー存在
	// 確認するため、handler 直前に call counter をリセットして listRelations
	// 経由の query 数だけを観測する。
	repo.findByIDCalls = 0
	repo.findProfileByUserIDCalls = 0
	repo.findManyByIDsCalls = 0
	repo.findManyByIDsCallSize = 0
	repo.findProfilesByUserIDs = 0

	rec := post(h.Followers, `{"userId":"target","limit":10}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 5)

	// listRelations の冒頭で ShowByID(req.UserID) を呼んで target 存在確認
	// するため、FindByID が 1 回走る。FindProfileByUserID は (1) target 存在
	// 確認 + (2) #1461 の followersVisibility / followingVisibility gate で
	// 2 回走るが、いずれも target single 行の O(1) lookup なので follower
	// 件数とは独立。重要なのは follower 5 件の解決で per-row 経路に落ちて
	// いないこと = listRelations の N+1 が解消されていること。
	assert.Equal(t, 1, repo.findByIDCalls,
		"only target existence check should call FindByID; per-row must use batch")
	assert.Equal(t, 2, repo.findProfileByUserIDCalls,
		"target existence check + visibility gate (#1461) should call FindProfileByUserID twice; per-row must use batch")
	assert.Equal(t, 1, repo.findManyByIDsCalls,
		"FindManyByIDs should be called exactly once per request for the 5 followers")
	assert.Equal(t, 5, repo.findManyByIDsCallSize,
		"all 5 follower IDs should be coalesced into a single batch")
	assert.Equal(t, 1, repo.findProfilesByUserIDs,
		"FindProfilesByUserIDs should be called exactly once per request")
}

func TestFollowers_LimitOutOfRangeRejected(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	// upstream は ajv で maximum を強制するので範囲外は 400。
	rec := post(h.Followers, `{"userId": "user1", "limit": 9999}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFollowers_UserNotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := post(h.Followers, `{"userId": "ghost"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestFollowers_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := post(h.Followers, `{}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFollowing_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo)
	repo.Users["followee1"] = &model.User{
		ID:                "followee1",
		Username:          "followee1",
		UsernameLower:     "followee1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	_, err := h.followingService.Follow("user1", "followee1", corefollowing.FollowOptions{})
	require.NoError(t, err)

	rec := post(h.Following, `{"userId": "user1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Len(t, out, 1)
	shapetest.Assert(t, "Following", out[0]) // L3 (#1330)
}

func TestFollowing_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := post(h.Following, `{}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- followersVisibility / followingVisibility gate (#1461) ---
//
// upstream Misskey TS の users/followers.ts (line 115-135) /
// users/following.ts (line 123-143) と挙動を揃え、profile の
// followersVisibility / followingVisibility 設定で viewer を gate する。
// public はそのまま 200、private は self 以外を 403、followers は viewer
// が target を follow しているときだけ通す。moderator は bypass。
// drop-in 互換のため 403 body の error.id は endpoint 別 UUID を assert する。

const (
	uuidUsersFollowersForbidden = "3c6a84db-d619-26af-ca14-06232a21df8a"
	uuidUsersFollowingForbidden = "f6cdb0df-c19f-ec5c-7dbb-0ba84a1f92ba"
)

// visibilityModStub は #1461 visibility gate test 用の ModeratorChecker 実装。
// handler_extra_test.go の stubModeratorChecker と同等 (あちらは users_test
// package のため白箱 test 側からは参照できないので、内側 package 用に別途
// 定義する)。
type visibilityModStub struct {
	modID   string
	adminID string
}

func (s visibilityModStub) IsModerator(userID string) bool     { return userID == s.modID }
func (s visibilityModStub) IsAdministrator(userID string) bool { return userID == s.adminID }

// setupRelationVisibilityFixture wires target user (user1) with the given
// followers/following visibility and adds a stand-in follower (alice) so the
// follower-list returns a non-empty payload when the gate is open.
func setupRelationVisibilityFixture(t *testing.T, followersVis, followingVis model.FollowingVisibility) (*Handler, *testutil.MockUserRepository) {
	t.Helper()
	h, repo := newTestHandler(t)
	addTestUser(repo) // user1 = target
	repo.Profiles["user1"] = &model.UserProfile{
		UserID:              "user1",
		FollowersVisibility: followersVis,
		FollowingVisibility: followingVis,
	}
	repo.Users["alice"] = &model.User{
		ID: "alice", Username: "alice", UsernameLower: "alice",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	_, err := h.followingService.Follow("alice", "user1", corefollowing.FollowOptions{})
	require.NoError(t, err)
	// followee 方向もそろえて users/following でも非空 payload になるようにする
	repo.Users["carol"] = &model.User{
		ID: "carol", Username: "carol", UsernameLower: "carol",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	_, err = h.followingService.Follow("user1", "carol", corefollowing.FollowOptions{})
	require.NoError(t, err)
	return h, repo
}

func assertForbiddenWithUUID(t *testing.T, rec *httptest.ResponseRecorder, wantUUID string) {
	t.Helper()
	// upstream は API エラーを kind:'client' = 400 で返す。403 ではない。
	require.Equal(t, http.StatusBadRequest, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errObj, ok := body["error"].(map[string]any)
	require.True(t, ok, "error field must be a map")
	assert.Equal(t, "FORBIDDEN", errObj["code"])
	assert.Equal(t, wantUUID, errObj["id"])
}

// public は anonymous viewer でも従来どおり 200 を返す (regression guard)。
func TestFollowers_VisibilityPublic_Anonymous(t *testing.T) {
	h, _ := setupRelationVisibilityFixture(t,
		model.FollowingVisibilityPublic, model.FollowingVisibilityPublic)
	rec := post(h.Followers, `{"userId":"user1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFollowing_VisibilityPublic_Anonymous(t *testing.T) {
	h, _ := setupRelationVisibilityFixture(t,
		model.FollowingVisibilityPublic, model.FollowingVisibilityPublic)
	rec := post(h.Following, `{"userId":"user1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// private は self だけ通す。他 viewer / anonymous は 403。
func TestFollowers_VisibilityPrivate_Self(t *testing.T) {
	h, repo := setupRelationVisibilityFixture(t,
		model.FollowingVisibilityPrivate, model.FollowingVisibilityPublic)
	rec := postStub(h.Followers, `{"userId":"user1"}`, repo.Users["user1"])
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFollowers_VisibilityPrivate_OtherViewer_Forbidden(t *testing.T) {
	h, _ := setupRelationVisibilityFixture(t,
		model.FollowingVisibilityPrivate, model.FollowingVisibilityPublic)
	rec := postStub(h.Followers, `{"userId":"user1"}`, &model.User{ID: "bob"})
	assertForbiddenWithUUID(t, rec, uuidUsersFollowersForbidden)
}

func TestFollowers_VisibilityPrivate_Anonymous_Forbidden(t *testing.T) {
	h, _ := setupRelationVisibilityFixture(t,
		model.FollowingVisibilityPrivate, model.FollowingVisibilityPublic)
	rec := post(h.Followers, `{"userId":"user1"}`)
	assertForbiddenWithUUID(t, rec, uuidUsersFollowersForbidden)
}

func TestFollowing_VisibilityPrivate_Self(t *testing.T) {
	h, repo := setupRelationVisibilityFixture(t,
		model.FollowingVisibilityPublic, model.FollowingVisibilityPrivate)
	rec := postStub(h.Following, `{"userId":"user1"}`, repo.Users["user1"])
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFollowing_VisibilityPrivate_OtherViewer_Forbidden(t *testing.T) {
	h, _ := setupRelationVisibilityFixture(t,
		model.FollowingVisibilityPublic, model.FollowingVisibilityPrivate)
	rec := postStub(h.Following, `{"userId":"user1"}`, &model.User{ID: "bob"})
	assertForbiddenWithUUID(t, rec, uuidUsersFollowingForbidden)
}

func TestFollowing_VisibilityPrivate_Anonymous_Forbidden(t *testing.T) {
	h, _ := setupRelationVisibilityFixture(t,
		model.FollowingVisibilityPublic, model.FollowingVisibilityPrivate)
	rec := post(h.Following, `{"userId":"user1"}`)
	assertForbiddenWithUUID(t, rec, uuidUsersFollowingForbidden)
}

// followers は follower viewer / self だけ通し、それ以外 (非 follower /
// anonymous) は 403。
func TestFollowers_VisibilityFollowers_FollowerViewer(t *testing.T) {
	h, repo := setupRelationVisibilityFixture(t,
		model.FollowingVisibilityFollowers, model.FollowingVisibilityPublic)
	// bob が target (user1) を follow している → 通る。
	repo.Users["bob"] = &model.User{
		ID: "bob", Username: "bob", UsernameLower: "bob",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	_, err := h.followingService.Follow("bob", "user1", corefollowing.FollowOptions{})
	require.NoError(t, err)
	rec := postStub(h.Followers, `{"userId":"user1"}`, repo.Users["bob"])
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFollowers_VisibilityFollowers_NonFollower_Forbidden(t *testing.T) {
	h, _ := setupRelationVisibilityFixture(t,
		model.FollowingVisibilityFollowers, model.FollowingVisibilityPublic)
	rec := postStub(h.Followers, `{"userId":"user1"}`, &model.User{ID: "bob"})
	assertForbiddenWithUUID(t, rec, uuidUsersFollowersForbidden)
}

func TestFollowers_VisibilityFollowers_Anonymous_Forbidden(t *testing.T) {
	h, _ := setupRelationVisibilityFixture(t,
		model.FollowingVisibilityFollowers, model.FollowingVisibilityPublic)
	rec := post(h.Followers, `{"userId":"user1"}`)
	assertForbiddenWithUUID(t, rec, uuidUsersFollowersForbidden)
}

func TestFollowers_VisibilityFollowers_Self(t *testing.T) {
	h, repo := setupRelationVisibilityFixture(t,
		model.FollowingVisibilityFollowers, model.FollowingVisibilityPublic)
	rec := postStub(h.Followers, `{"userId":"user1"}`, repo.Users["user1"])
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFollowing_VisibilityFollowers_FollowerViewer(t *testing.T) {
	h, repo := setupRelationVisibilityFixture(t,
		model.FollowingVisibilityPublic, model.FollowingVisibilityFollowers)
	repo.Users["bob"] = &model.User{
		ID: "bob", Username: "bob", UsernameLower: "bob",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	_, err := h.followingService.Follow("bob", "user1", corefollowing.FollowOptions{})
	require.NoError(t, err)
	rec := postStub(h.Following, `{"userId":"user1"}`, repo.Users["bob"])
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFollowing_VisibilityFollowers_NonFollower_Forbidden(t *testing.T) {
	h, _ := setupRelationVisibilityFixture(t,
		model.FollowingVisibilityPublic, model.FollowingVisibilityFollowers)
	rec := postStub(h.Following, `{"userId":"user1"}`, &model.User{ID: "bob"})
	assertForbiddenWithUUID(t, rec, uuidUsersFollowingForbidden)
}

func TestFollowing_VisibilityFollowers_Anonymous_Forbidden(t *testing.T) {
	h, _ := setupRelationVisibilityFixture(t,
		model.FollowingVisibilityPublic, model.FollowingVisibilityFollowers)
	rec := post(h.Following, `{"userId":"user1"}`)
	assertForbiddenWithUUID(t, rec, uuidUsersFollowingForbidden)
}

// moderator は private/followers のいずれも bypass する (upstream
// roleService.isModerator(me) 経路)。
func TestFollowers_VisibilityPrivate_ModeratorBypass(t *testing.T) {
	h, _ := setupRelationVisibilityFixture(t,
		model.FollowingVisibilityPrivate, model.FollowingVisibilityPublic)
	h.SetModeratorChecker(visibilityModStub{modID: "u_mod"})
	rec := postStub(h.Followers, `{"userId":"user1"}`, &model.User{ID: "u_mod"})
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFollowing_VisibilityPrivate_ModeratorBypass(t *testing.T) {
	h, _ := setupRelationVisibilityFixture(t,
		model.FollowingVisibilityPublic, model.FollowingVisibilityPrivate)
	h.SetModeratorChecker(visibilityModStub{modID: "u_mod"})
	rec := postStub(h.Following, `{"userId":"user1"}`, &model.User{ID: "u_mod"})
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestShow_BulkUserIDs(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["u2"] = &model.User{ID: "u2", Username: "bob", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	rec := post(h.Show, `{"userIds":["u1","u2","ghost"]}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Len(t, out, 2)
}

func TestShow_BulkUserIDs_Empty(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := post(h.Show, `{"userIds":[]}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

// users/show bulk path は ShowManyByIDs に切り替えた (#503) ので、入力順が
// レスポンスにも保持されることを担保する。
func TestShow_BulkUserIDs_PreservesOrder(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["u2"] = &model.User{ID: "u2", Username: "bob", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["u3"] = &model.User{ID: "u3", Username: "carol", AvatarDecorations: datatypes.JSON([]byte("[]"))}

	rec := post(h.Show, `{"userIds":["u3","ghost","u1","u2"]}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 3)
	assert.Equal(t, "u3", out[0]["id"])
	assert.Equal(t, "u1", out[1]["id"])
	assert.Equal(t, "u2", out[2]["id"])
}

// 100 件を超えた userIds は先頭 100 件に切り捨てる動作を担保する (TS 互換)。
func TestShow_BulkUserIDs_Truncates100(t *testing.T) {
	h, repo := newTestHandler(t)
	ids := make([]string, 0, 105)
	for i := 0; i < 105; i++ {
		uid := fmt.Sprintf("ub%03d", i)
		repo.Users[uid] = &model.User{ID: uid, Username: uid, AvatarDecorations: datatypes.JSON([]byte("[]"))}
		ids = append(ids, fmt.Sprintf("%q", uid))
	}
	body := `{"userIds":[` + strings.Join(ids, ",") + `]}`
	rec := post(h.Show, body)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Len(t, out, 100, "userIds は 100 件で切り捨てられるはず")
}

// addSuspendedBulkFixture wires alice (alive), bob (suspended), carol (alive).
func addSuspendedBulkFixture(repo *testutil.MockUserRepository) {
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["u2"] = &model.User{ID: "u2", Username: "bob", IsSuspended: true, AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["u3"] = &model.User{ID: "u3", Username: "carol", AvatarDecorations: datatypes.JSON([]byte("[]"))}
}

// upstream show.ts:136-141: 非 moderator (匿名含む) のバルクモードでは suspended
// user を結果から除外する。
func TestShow_BulkUserIDs_ExcludesSuspendedForNonModerator(t *testing.T) {
	h, repo := newTestHandler(t)
	addSuspendedBulkFixture(repo)
	rec := post(h.Show, `{"userIds":["u1","u2","u3"]}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 2, "suspended user は非 moderator に除外されるはず")
	assert.Equal(t, "u1", out[0]["id"])
	assert.Equal(t, "u3", out[1]["id"])
}

// 認証済みだが非 moderator の viewer も suspended user は除外される。
func TestShow_BulkUserIDs_ExcludesSuspendedForAuthedNonModerator(t *testing.T) {
	h, repo := newTestHandler(t)
	addSuspendedBulkFixture(repo)
	h.SetModeratorChecker(visibilityModStub{modID: "u_mod"})
	rec := postStub(h.Show, `{"userIds":["u1","u2","u3"]}`, &model.User{ID: "u_plain"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 2)
	assert.Equal(t, "u1", out[0]["id"])
	assert.Equal(t, "u3", out[1]["id"])
}

// moderator viewer は suspended user も含めて全件返す。
func TestShow_BulkUserIDs_ModeratorSeesSuspended(t *testing.T) {
	h, repo := newTestHandler(t)
	addSuspendedBulkFixture(repo)
	h.SetModeratorChecker(visibilityModStub{modID: "u_mod"})
	rec := postStub(h.Show, `{"userIds":["u1","u2","u3"]}`, &model.User{ID: "u_mod"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 3, "moderator は suspended user も閲覧できるはず")
	assert.Equal(t, "u1", out[0]["id"])
	assert.Equal(t, "u2", out[1]["id"])
	assert.Equal(t, "u3", out[2]["id"])
}

// upstream show.ts:173-175: 単体モードで suspended user は非 moderator に
// NO_SUCH_USER(4362f8dc...) を返す。
func TestShow_SingleSuspended_NotFoundForNonModerator(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Users["sus"] = &model.User{ID: "sus", Username: "sus", UsernameLower: "sus", IsSuspended: true, AvatarDecorations: datatypes.JSON([]byte("[]"))}
	rec := post(h.Show, `{"userId":"sus"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj, ok := resp["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "NO_SUCH_USER", errObj["code"])
	assert.Equal(t, "4362f8dc-731f-4ad8-a694-be5a88922a24", errObj["id"])
}

// username 指定でも同様に suspended user は非 moderator に隠す。
func TestShow_SingleSuspendedByUsername_NotFoundForNonModerator(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Users["sus"] = &model.User{ID: "sus", Username: "sus", UsernameLower: "sus", IsSuspended: true, AvatarDecorations: datatypes.JSON([]byte("[]"))}
	rec := post(h.Show, `{"username":"sus"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj, ok := resp["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "NO_SUCH_USER", errObj["code"])
}

// moderator viewer は単体モードでも suspended user を閲覧できる。
func TestShow_SingleSuspended_ModeratorSees(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Users["sus"] = &model.User{ID: "sus", Username: "sus", UsernameLower: "sus", IsSuspended: true, AvatarDecorations: datatypes.JSON([]byte("[]"))}
	h.SetModeratorChecker(visibilityModStub{modID: "u_mod"})
	rec := postStub(h.Show, `{"userId":"sus"}`, &model.User{ID: "u_mod"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "sus", resp["id"])
}

// #1781: moderator が他ユーザーを users/show で見ると twoFactorEnabled /
// usePasswordLessLogin / securityKeys が emit される (upstream
// `isDetailed && (isMe || iAmModerator)` ブロック)。非 moderator には omit。
func TestShow_Single_ModeratorGetsSecurityFields(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Users["target"] = &model.User{ID: "target", Username: "target", UsernameLower: "target", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Profiles["target"] = &model.UserProfile{
		UserID:                "target",
		TwoFactorEnabled:      true,
		UsePasswordLessLogin:  false,
		SecurityKeysAvailable: true,
	}
	h.SetModeratorChecker(visibilityModStub{modID: "u_mod"})

	t.Run("moderator viewer", func(t *testing.T) {
		rec := postStub(h.Show, `{"userId":"target"}`, &model.User{ID: "u_mod"})
		assert.Equal(t, http.StatusOK, rec.Code)
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, true, resp["twoFactorEnabled"])
		assert.Equal(t, false, resp["usePasswordLessLogin"])
		assert.Equal(t, true, resp["securityKeys"])
	})

	t.Run("non-moderator viewer omits the trio", func(t *testing.T) {
		rec := postStub(h.Show, `{"userId":"target"}`, &model.User{ID: "u_plain"})
		assert.Equal(t, http.StatusOK, rec.Code)
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		_, hasTFA := resp["twoFactorEnabled"]
		assert.False(t, hasTFA, "非 moderator には twoFactorEnabled を出さない")
		_, hasSK := resp["securityKeys"]
		assert.False(t, hasSK, "非 moderator には securityKeys を出さない")
	})
}

// --- Internal error paths via failing repos ---

type failingNoteRepo struct {
	*testutil.MockNoteRepository
}

func (f *failingNoteRepo) ListByUserID(_ string, _, _ string, _ int) ([]*model.Note, error) {
	return nil, assertErr
}

func (f *failingNoteRepo) ListByUserIDFiltered(_, _, _, _ string, _ int, _, _, _, _ bool) ([]*model.Note, error) {
	return nil, assertErr
}

type failingUserRepo struct {
	*testutil.MockUserRepository
}

func (f *failingUserRepo) SearchUsers(_, _ string, _, _ int, _ string) ([]*model.User, error) {
	return nil, assertErr
}

type failingFollowingRepo struct {
	*testutil.MockFollowingRepository
}

func (f *failingFollowingRepo) ListFollowers(_ string, _, _ int) ([]*model.Following, error) {
	return nil, assertErr
}

func (f *failingFollowingRepo) ListFollowing(_ string, _, _ int) ([]*model.Following, error) {
	return nil, assertErr
}

// **カーソル版も落とす。** users/followers と users/following はこちらを呼ぶので、
// offset 版だけ override しても embed した mock が成功を返し、500 経路の
// テストが黙って素通りする (#2711)。
func (f *failingFollowingRepo) ListFollowersWithCursor(_, _, _ string, _, _ int) ([]*model.Following, error) {
	return nil, assertErr
}

func (f *failingFollowingRepo) ListFollowingWithCursor(_, _, _ string, _, _ int) ([]*model.Following, error) {
	return nil, assertErr
}

var assertErr = &simpleErr{"stub"}

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }

func newHandlerWithFailingNoteRepo(t *testing.T) *Handler {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := &failingNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository()}
	piningRepo := testutil.NewMockUserNotePiningRepository()
	fRepo := testutil.NewMockFollowingRepository()
	frRepo := testutil.NewMockFollowRequestRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	fSvc := corefollowing.NewService(userRepo, fRepo, frRepo, idGen)
	addTestUser(userRepo)
	return NewHandler(svc, fSvc, noteRepo, idGen)
}

func newHandlerWithFailingSearch(t *testing.T) *Handler {
	t.Helper()
	mockUR := testutil.NewMockUserRepository()
	addTestUser(mockUR)
	userRepo := &failingUserRepo{MockUserRepository: mockUR}
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	fRepo := testutil.NewMockFollowingRepository()
	frRepo := testutil.NewMockFollowRequestRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	fSvc := corefollowing.NewService(userRepo, fRepo, frRepo, idGen)
	return NewHandler(svc, fSvc, noteRepo, idGen)
}

func newHandlerWithFailingFollowing(t *testing.T) *Handler {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	addTestUser(userRepo)
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	fRepo := &failingFollowingRepo{MockFollowingRepository: testutil.NewMockFollowingRepository()}
	frRepo := testutil.NewMockFollowRequestRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	fSvc := corefollowing.NewService(userRepo, fRepo, frRepo, idGen)
	return NewHandler(svc, fSvc, noteRepo, idGen)
}

func TestSearch_InternalError(t *testing.T) {
	h := newHandlerWithFailingSearch(t)
	rec := post(h.Search, `{"query": "test"}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestNotes_InternalError(t *testing.T) {
	h := newHandlerWithFailingNoteRepo(t)
	rec := post(h.Notes, `{"userId": "user1"}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestFollowers_InternalError(t *testing.T) {
	h := newHandlerWithFailingFollowing(t)
	rec := post(h.Followers, `{"userId": "user1"}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestFollowing_InternalError(t *testing.T) {
	h := newHandlerWithFailingFollowing(t)
	rec := post(h.Following, `{"userId": "user1"}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Phase 7-3 (#245): pinnedNotes / pinnedPage on users/show ---

type stubPageRepoForPin struct {
	page *model.Page
}

func (s *stubPageRepoForPin) Create(*model.Page) error                              { return nil }
func (s *stubPageRepoForPin) FindByID(string) (*model.Page, error)                  { return s.page, nil }
func (s *stubPageRepoForPin) FindManyByIDs([]string) ([]*model.Page, error)         { return nil, nil }
func (s *stubPageRepoForPin) FindByUserAndName(string, string) (*model.Page, error) { return nil, nil }
func (s *stubPageRepoForPin) UpdateFields(string, map[string]any) error             { return nil }
func (s *stubPageRepoForPin) Delete(*model.Page) error                              { return nil }
func (s *stubPageRepoForPin) ListByUser(string, string, string, int, int) ([]*model.Page, error) {
	return nil, nil
}
func (s *stubPageRepoForPin) ListPublicByUser(string, string, string, int, int) ([]*model.Page, error) {
	return nil, nil
}
func (s *stubPageRepoForPin) ListFeatured(string, string, int, int) ([]*model.Page, error) {
	return nil, nil
}
func (s *stubPageRepoForPin) IncrementCount(string, string, int) error { return nil }

func TestShow_PinnedNotes_Populated(t *testing.T) {
	h, userRepo := newTestHandler(t)
	addTestUser(userRepo)

	piningRepo := testutil.NewMockUserNotePiningRepository()
	require.NoError(t, piningRepo.Create(&model.UserNotePining{ID: "p1", UserID: "user1", NoteID: "note_a"}))
	h.SetPiningRepo(piningRepo)

	// note 本体は h.noteRepo (newTestHandler 内で MockNoteRepo) に入れる
	nr, _ := h.noteRepo.(*testutil.MockNoteRepository)
	require.NotNil(t, nr)
	txt := "pinned!"
	nr.Notes["note_a"] = &model.Note{ID: "note_a", UserID: "user1", Text: &txt, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}

	body := `{"userId": "user1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Show(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ids, ok := resp["pinnedNoteIds"].([]any)
	require.True(t, ok)
	assert.Len(t, ids, 1)
	assert.Equal(t, "note_a", ids[0])
	notes, ok := resp["pinnedNotes"].([]any)
	require.True(t, ok)
	assert.Len(t, notes, 1)
}

// pinnedNotes に followers / specified visibility の note が含まれる場合、
// 閲覧権限のない viewer (anonymous) には pinnedNotes 本体から除外されること
// を guard する。pinnedNoteIds は visibility に関係なく出る upstream 挙動を
// 維持しつつ、pack 済み note 本体だけ落とす (#1418 review)。
func TestShow_PinnedNotes_ExcludesNonVisibleFromBody(t *testing.T) {
	h, userRepo := newTestHandler(t)
	addTestUser(userRepo)

	piningRepo := testutil.NewMockUserNotePiningRepository()
	require.NoError(t, piningRepo.Create(&model.UserNotePining{ID: "pp_pub", UserID: "user1", NoteID: "pn_pub"}))
	require.NoError(t, piningRepo.Create(&model.UserNotePining{ID: "pp_fol", UserID: "user1", NoteID: "pn_fol"}))
	h.SetPiningRepo(piningRepo)

	nr, _ := h.noteRepo.(*testutil.MockNoteRepository)
	require.NotNil(t, nr)
	txt := "pinned"
	nr.Notes["pn_pub"] = &model.Note{ID: "pn_pub", UserID: "user1", Text: &txt, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
	nr.Notes["pn_fol"] = &model.Note{ID: "pn_fol", UserID: "user1", Text: &txt, Visibility: model.NoteVisibilityFollowers, Reactions: datatypes.JSON([]byte("{}"))}

	body := `{"userId": "user1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Show(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	notes, ok := resp["pinnedNotes"].([]any)
	require.True(t, ok)
	require.Len(t, notes, 1, "followers の pinned note は anonymous の pinnedNotes から除外される")
	first := notes[0].(map[string]any)
	assert.Equal(t, "pn_pub", first["id"])
}

func TestShow_PinnedNotes_PublicationExceptionMatrix(t *testing.T) {
	h, userRepo := newTestHandler(t)
	author := addTestUser(userRepo)
	hidden := 0
	author.MakeNotesHiddenBefore = &hidden

	piningRepo := testutil.NewMockUserNotePiningRepository()
	pin := &model.UserNotePining{ID: "pin", UserID: author.ID, NoteID: "note"}
	require.NoError(t, piningRepo.Create(pin))
	h.SetPiningRepo(piningRepo)

	nr := h.noteRepo.(*testutil.MockNoteRepository)
	text := "intentionally published"
	n := &model.Note{ID: "note", UserID: author.ID, User: author, Text: &text, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
	nr.Notes[n.ID] = n

	request := func(viewer *model.User) map[string]any {
		rec := postStub(h.Show, `{"userId":"user1"}`, viewer)
		require.Equal(t, http.StatusOK, rec.Code)
		var response map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
		return response
	}
	firstPinned := func(response map[string]any) map[string]any {
		notes, ok := response["pinnedNotes"].([]any)
		require.True(t, ok)
		require.Len(t, notes, 1)
		return notes[0].(map[string]any)
	}

	assert.Equal(t, text, firstPinned(request(nil))["text"], "anonymous public pin bypasses time lockdown")

	author.RequireSigninToViewContents = true
	assert.Nil(t, firstPinned(request(nil))["text"], "anonymous sign-in requirement is never bypassed")
	assert.Equal(t, text, firstPinned(request(&model.User{ID: "viewer"}))["text"], "authenticated public pin bypasses author lockdown")

	n.Visibility = model.NoteVisibilityFollowers
	response := request(&model.User{ID: "viewer"})
	notes, _ := response["pinnedNotes"].([]any)
	assert.Empty(t, notes, "login alone must not bypass followers visibility")
}

func TestShow_PinnedNotes_ForeignAuthorDoesNotGetPublicationException(t *testing.T) {
	h, userRepo := newTestHandler(t)
	profileOwner := addTestUser(userRepo)
	hidden := 0
	foreignAuthor := &model.User{ID: "other", Username: "other", MakeNotesHiddenBefore: &hidden}
	userRepo.Users[foreignAuthor.ID] = foreignAuthor

	piningRepo := testutil.NewMockUserNotePiningRepository()
	require.NoError(t, piningRepo.Create(&model.UserNotePining{
		ID: "legacy-pin", UserID: profileOwner.ID, NoteID: "foreign-note",
	}))
	h.SetPiningRepo(piningRepo)

	text := "not published by this profile owner"
	nr := h.noteRepo.(*testutil.MockNoteRepository)
	nr.Notes["foreign-note"] = &model.Note{
		ID: "foreign-note", UserID: foreignAuthor.ID, User: foreignAuthor,
		Text: &text, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}

	rec := postStub(h.Show, `{"userId":"user1"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var response map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	notes, ok := response["pinnedNotes"].([]any)
	require.True(t, ok)
	require.Len(t, notes, 1)
	got := notes[0].(map[string]any)
	assert.Nil(t, got["text"], "a legacy pin row owned by another user must not bypass the note author's lockdown")
}

func TestShow_PinnedPage_Populated(t *testing.T) {
	h, userRepo := newTestHandler(t)
	addTestUser(userRepo)

	pageID := "pg_1"
	userRepo.Profiles["user1"] = &model.UserProfile{
		UserID:       "user1",
		Fields:       datatypes.JSON([]byte("[]")),
		PinnedPageID: &pageID,
	}
	h.SetPageRepo(&stubPageRepoForPin{page: &model.Page{ID: pageID, Title: "my page", UserID: "user1"}})

	body := `{"userId": "user1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Show(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, pageID, resp["pinnedPageId"])
	page, ok := resp["pinnedPage"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "my page", page["title"])
	// golden Page は user 必須。pinnedPage に owner を渡すよう修正 (#1266 fu) した
	// ので user (UserLite) が present で、UserDetailed shape に戻る regression を防ぐ。
	pageUser, ok := page["user"].(map[string]any)
	require.True(t, ok, "pinnedPage.user must be present (golden Page requires user)")
	assert.Equal(t, "user1", pageUser["id"])
}

func TestShow_PinnedFields_Defaults(t *testing.T) {
	h, userRepo := newTestHandler(t)
	addTestUser(userRepo)

	body := `{"userId": "user1"}`
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/show", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Show(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// pining未wireでもリストは存在 (空)
	ids, _ := resp["pinnedNoteIds"].([]any)
	assert.Empty(t, ids)
	assert.Nil(t, resp["pinnedPageId"])
	assert.Nil(t, resp["pinnedPage"])
}

// --- Show with isFollowing ---

func TestShow_IsFollowing(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON("[]")}
	repo.Users["u2"] = &model.User{ID: "u2", Username: "bob", AvatarDecorations: datatypes.JSON("[]")}
	fRepo := h.followingRepo
	_ = fRepo // followingRepoがセットされていない場合のテスト

	rec := postStub(h.Show, `{"userId":"u2"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
}

// #739: SetEmojiRepo / SetReactionReader / SetNoteFieldResolver の setter を
// 配線して非 nil 経路の lookup を踏む。populateUserEmojis も Show 経由で
// 実行されるよう emoji 行を仕込む。
func TestSetters_WireOptionalDeps(t *testing.T) {
	h, repo := newTestHandler(t)
	remoteHost := "remote.example"
	repo.Users["u1"] = &model.User{
		ID: "u1", Username: "alice", UsernameLower: "alice",
		Emojis:            []string{"smile"},
		AvatarDecorations: datatypes.JSON("[]"),
	}
	repo.Users["u2"] = &model.User{
		ID: "u2", Username: "bob", UsernameLower: "bob", Host: &remoteHost,
		Emojis:            []string{"smile"},
		AvatarDecorations: datatypes.JSON("[]"),
	}

	emojiRepo := testutil.NewMockEmojiRepository()
	require.NoError(t, emojiRepo.Create(&model.Emoji{
		ID: "e1", Name: "smile", PublicURL: "https://x/smile.png",
	}))
	require.NoError(t, emojiRepo.Create(&model.Emoji{
		ID: "e2", Name: "smile", Host: &remoteHost, PublicURL: "https://remote.example/smile.png",
	}))
	h.SetEmojiRepo(emojiRepo)
	h.SetInstanceRepo(testutil.NewMockInstanceRepository())
	h.SetReactionReader(stubBufferedReactions{})
	h.SetNoteFieldResolver(nil) // Apply は r==nil で no-op (#739)

	show := func(id string) map[string]any {
		rec := postStub(h.Show, `{"userId":"`+id+`"}`, &model.User{ID: "u1"})
		require.Equal(t, http.StatusOK, rec.Code)
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		emojis, _ := resp["emojis"].(map[string]any)
		return emojis
	}
	// populateUserEmojis がリモートの利用者の emoji を URL に解決して emojis map に出すこと
	remote := show("u2")
	require.NotNil(t, remote, "emojis should be populated when emojiRepo is wired")
	assert.Equal(t, "https://remote.example/smile.png", remote["smile"])
	// ローカルの利用者は本家と同じく解決しない (#3270)。
	assert.Empty(t, show("u1"))
}

// stubBufferedReactions implements entity.BufferedReactionsReader as a no-op
// for setter wiring tests (#739)。
type stubBufferedReactions struct{}

func (stubBufferedReactions) GetBufferedMany(_ context.Context, _ []string) (map[string]map[string]int64, error) {
	return map[string]map[string]int64{}, nil
}

// #2106 N3: users/followers の embed user に viewer 視点の block/mute 実値を埋める
// (旧実装は EnsureRelationFlags の best-effort false に倒していた)。
func TestFollowers_PopulatesBlockMute(t *testing.T) {
	h, repo := newTestHandler(t)
	addTestUser(repo) // user1 = target profile
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["alice"] = &model.User{ID: "alice", Username: "alice", UsernameLower: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	// alice follows user1 → alice が表示される follower。
	_, err := h.followingService.Follow("alice", "user1", corefollowing.FollowOptions{})
	require.NoError(t, err)
	// viewer(bob) が alice を block + mute。
	blockRepo := testutil.NewMockBlockingRepository()
	require.NoError(t, blockRepo.Create(&model.Blocking{ID: "bl1", BlockerID: "bob", BlockeeID: "alice"}))
	h.SetBlockingRepo(blockRepo)
	muteRepo := testutil.NewMockMutingRepository()
	require.NoError(t, muteRepo.Create(&model.Muting{ID: "mu1", MuterID: "bob", MuteeID: "alice"}))
	h.SetMutingRepo(muteRepo)
	h.SetRenoteMutingRepo(testutil.NewMockRenoteMutingRepository())

	rec := postStub(h.Followers, `{"userId":"user1"}`, repo.Users["bob"])
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 1)
	follower := out[0]["follower"].(map[string]any)
	assert.Equal(t, true, follower["isBlocking"], "viewer が block している follower は isBlocking=true")
	assert.Equal(t, true, follower["isMuted"], "viewer が mute している follower は isMuted=true")
	assert.Equal(t, false, follower["isBlocked"])
	assert.Equal(t, false, follower["isRenoteMuted"])
}

// #2106 L10: username の前後空白を trim して lookup する (upstream show.ts と同じ)。
func TestShow_ByUsernameTrimmed(t *testing.T) {
	h, userRepo := newTestHandler(t)
	addTestUser(userRepo)
	rec := post(h.Show, `{"username": "  testuser  "}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "user1", resp["id"], "前後空白を trim して testuser に解決")
}

// **非公開のピン留め Page の本文を返さないこと。**
//
// `pages/show` は拒否するのに `users/show` は `content` / `script` を渡して
// いた。同じ関数の少し上ではピン留めノートに可視性ゲートを掛けている。
func TestPinnedPageVisibleTo(t *testing.T) {
	t.Parallel()

	owner := &model.User{ID: "owner"}
	stranger := &model.User{ID: "stranger"}

	pub := &model.Page{ID: "p1", UserID: "owner", Visibility: model.PageVisibilityPublic}
	require.True(t, pinnedPageVisibleTo(pub, nil), "public は未認証にも見せる")

	hidden := &model.Page{ID: "p2", UserID: "owner", Visibility: model.PageVisibilityFollowers}
	require.False(t, pinnedPageVisibleTo(hidden, nil), "非公開は未認証に見せない")
	require.False(t, pinnedPageVisibleTo(hidden, stranger), "非公開は他人に見せない")
	require.True(t, pinnedPageVisibleTo(hidden, owner), "本人には見せる")

	require.False(t, pinnedPageVisibleTo(nil, owner))

	// visibility 未設定は既定 (public) 扱い。
	def := &model.Page{ID: "p3", UserID: "owner"}
	require.True(t, pinnedPageVisibleTo(def, nil))
}
