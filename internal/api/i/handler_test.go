package i

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/core/notification"
	coreuser "github.com/shiroha-a/mk/internal/core/user"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/entitycompat/shapetest"
	"github.com/shiroha-a/mk/internal/misc/id"
	miscsmtp "github.com/shiroha-a/mk/internal/misc/smtp"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

var errStub = errors.New("stub error")

// failingPiningRepo lets us trigger non-domain errors from PinNote.
type failingPiningRepo struct {
	*testutil.MockUserNotePiningRepository
}

func (f *failingPiningRepo) CountByUser(_ string) (int, error) { return 0, errStub }

// failingPiningDeleteRepo lets us trigger non-domain errors from UnpinNote
// while still allowing FindByPair to succeed (so the service reaches Delete).
type failingPiningDeleteRepo struct {
	*testutil.MockUserNotePiningRepository
}

func (f *failingPiningDeleteRepo) Delete(_ *model.UserNotePining) error {
	return errStub
}

func newHandlerWithFailingUnpinDelete(t *testing.T) (*Handler, *testutil.MockUserNotePiningRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	mock := testutil.NewMockUserNotePiningRepository()
	piningRepo := &failingPiningDeleteRepo{MockUserNotePiningRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	return NewHandler(svc, idGen), mock
}

func newHandlerWithFailingPiningCount(t *testing.T) (*Handler, *testutil.MockUserRepository, *testutil.MockNoteRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := &failingPiningRepo{MockUserNotePiningRepository: testutil.NewMockUserNotePiningRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	return NewHandler(svc, idGen), userRepo, noteRepo
}

// failingUserRepoForUpdate forces user.UpdateUser to fail.
type failingUserRepoForUpdate struct {
	*testutil.MockUserRepository
}

func (f *failingUserRepoForUpdate) UpdateUser(_ string, _ map[string]any) error { return errStub }

func newHandlerWithFailingUpdate(t *testing.T) (*Handler, *testutil.MockUserRepository) {
	t.Helper()
	mockUR := testutil.NewMockUserRepository()
	mockUR.Users["user1"] = &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	userRepo := &failingUserRepoForUpdate{MockUserRepository: mockUR}
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	return NewHandler(svc, idGen), mockUR
}

// post is a small helper to invoke a handler with an authenticated request.
func post(h echo.HandlerFunc, body string, me *model.User) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if me != nil {
		c.Set(string(middleware.UserContextKey), me)
	}
	_ = h(c)
	return rec
}

func newTestHandler(t *testing.T) (*Handler, *testutil.MockUserRepository, *testutil.MockNoteRepository, *testutil.MockUserNotePiningRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	h := NewHandler(svc, idGen)
	return h, userRepo, noteRepo, piningRepo
}

func TestMe_Success(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)

	name := "Test User"
	user := &model.User{
		ID:                "user1",
		Username:          "testuser",
		Name:              &name,
		FollowersCount:    10,
		FollowingCount:    20,
		NotesCount:        100,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
		ChatScope:         "mutual",
	}

	email := "test@example.com"
	userRepo.Profiles["user1"] = &model.UserProfile{
		UserID:              "user1",
		Email:               &email,
		EmailVerified:       true,
		TwoFactorEnabled:    false,
		AutoAcceptFollowed:  true,
		NoCrawle:            false,
		PreventAiLearning:   true,
		Fields:              datatypes.JSON([]byte("[]")),
		FollowersVisibility: model.FollowingVisibilityPublic,
		FollowingVisibility: model.FollowingVisibilityPublic,
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/i", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), user)
	// native session token を配線して isSecure=true にする (#1824)。これが無いと
	// email / securityKeysList は app token 扱いで省かれる。
	nativeTok := "native-session-token"
	user.Token = &nativeTok
	c.Set(string(middleware.TokenContextKey), nativeTok)

	err := h.Me(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, "user1", resp["id"])
	assert.Equal(t, "testuser", resp["username"])
	assert.Equal(t, "Test User", resp["name"])
	assert.Equal(t, float64(10), resp["followersCount"])
	assert.Equal(t, float64(20), resp["followingCount"])
	assert.Equal(t, float64(100), resp["notesCount"])

	// Private fields
	assert.Equal(t, "test@example.com", resp["email"])
	assert.Equal(t, true, resp["emailVerified"])
	assert.Equal(t, true, resp["autoAcceptFollowed"])
	assert.Equal(t, false, resp["twoFactorEnabled"])
	assert.Equal(t, true, resp["preventAiLearning"])

	// Hardcoded fields
	assert.Equal(t, false, resp["hasUnreadNotification"])
	assert.Equal(t, false, resp["hasPendingReceivedFollowRequest"])

	// Phase 4.5c 互換性フィールド
	assert.Equal(t, false, resp["isAdmin"])
	assert.Equal(t, false, resp["isModerator"])
	assert.Equal(t, false, resp["isDeleted"])
	assert.NotNil(t, resp["pinnedNoteIds"])
	assert.NotNil(t, resp["pinnedNotes"])
	assert.Nil(t, resp["pinnedPageId"])
	assert.NotNil(t, resp["policies"])
	assert.NotNil(t, resp["roles"])
	assert.NotNil(t, resp["achievements"])
	assert.NotNil(t, resp["unreadAnnouncements"])
	assert.Equal(t, false, resp["publicReactions"]) // profile has default false
	// C3 追加フィールド
	assert.NotNil(t, resp["avatarUrl"]) // identicon URL 自動生成
	assert.Equal(t, false, resp["hasUnreadChatMessages"])
	assert.Equal(t, "public", resp["followersVisibility"])
	assert.Equal(t, "public", resp["followingVisibility"])
	assert.Equal(t, "mutual", resp["chatScope"])
	assert.Equal(t, true, resp["canChat"])
	assert.NotNil(t, resp["verifiedLinks"])
	assert.NotNil(t, resp["securityKeysList"])
	assert.NotNil(t, resp["mutingNotificationTypes"])
	assert.Equal(t, false, resp["securityKeys"])
	assert.Nil(t, resp["movedTo"])
	assert.Nil(t, resp["alsoKnownAs"])
}

// #1824: OAuth/MiAuth の app access token (request token != user.Token) で /api/i を
// 呼んだとき、email / emailVerified / securityKeysList を返さない (includeSecrets
// 限定)。twoFactorEnabled 等の MeDetailed field は引き続き出す。
func TestMe_AppTokenOmitsSecrets(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	nativeTok := "native-login-token"
	user := &model.User{
		ID: "user1", Username: "testuser", Token: &nativeTok,
		AvatarDecorations: datatypes.JSON([]byte("[]")), ChatScope: "mutual",
	}
	require.NoError(t, userRepo.Create(user))
	email := "secret@example.com"
	userRepo.Profiles["user1"] = &model.UserProfile{
		UserID: "user1", Email: &email, EmailVerified: true,
		TwoFactorEnabled: true, Fields: datatypes.JSON([]byte("[]")),
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/i", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), user)
	// app access token: request token は user.Token (native) と一致しない。
	c.Set(string(middleware.TokenContextKey), "app-access-token-xyz")

	require.NoError(t, h.Me(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// secrets は省かれる。
	_, hasEmail := resp["email"]
	assert.False(t, hasEmail, "app token に email を漏らさない")
	_, hasEmailVerified := resp["emailVerified"]
	assert.False(t, hasEmailVerified, "app token に emailVerified を漏らさない")
	_, hasSecurityKeysList := resp["securityKeysList"]
	assert.False(t, hasSecurityKeysList, "app token に securityKeysList を漏らさない")
	// MeDetailed の非 secret field は引き続き出る。
	assert.Equal(t, "user1", resp["id"])
	assert.Equal(t, true, resp["twoFactorEnabled"])
}

// TestMe_CanChatFromRolePolicy: #988 — Me handler が canChat を role policy
// 由来で返すこと (旧 hardcode `resp["canChat"] = true` を撤去後の regression
// guard)。CanChatLookup を wire して false を返すと self-view でも false。
func TestMe_CanChatFromRolePolicy(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)

	t.Cleanup(func() { entity.SetCanChatLookup(nil) })
	entity.SetCanChatLookup(&stubMeCanChatLookup{result: false})

	user := &model.User{
		ID:                "user-canchat",
		Username:          "blocked",
		ChatScope:         "all", // 旧実装ではこの値で true 固定だった
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	userRepo.Profiles["user-canchat"] = &model.UserProfile{
		UserID: "user-canchat",
		Fields: datatypes.JSON([]byte("[]")),
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/i", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), user)

	require.NoError(t, h.Me(c))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["canChat"])
}

type stubMeCanChatLookup struct{ result bool }

func (s *stubMeCanChatLookup) LookupCanChat(_ string) (bool, bool) {
	return s.result, true
}

// TestMe_PreservesMeDetailedFields: #971 — Me handler を PackMeDetailed base
// に refactor した後も、MeDetailed の 11 self-view field + notification 3
// field がすべて response に乗っていることを確認する regression guard。
func TestMe_PreservesMeDetailedFields(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)

	user := &model.User{
		ID:                "user2",
		Username:          "merefactor",
		IsExplorable:      true,
		IsDeleted:         false,
		HideOnlineStatus:  true,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	userRepo.Profiles["user2"] = &model.UserProfile{
		UserID:                    "user2",
		NoCrawle:                  true,
		PreventAiLearning:         false,
		AutoSensitive:             true,
		CarefulBot:                true,
		AutoAcceptFollowed:        true,
		AlwaysMarkNsfw:            true,
		ReceiveAnnouncementEmail:  false,
		InjectFeaturedNote:        false,
		EmailNotificationTypes:    datatypes.JSON([]byte(`["mention","reply"]`)),
		NotificationRecieveConfig: datatypes.JSON([]byte(`{"mention":{"type":"following"}}`)),
		Fields:                    datatypes.JSON([]byte("[]")),
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/i", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), user)

	require.NoError(t, h.Me(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// 11 MeDetailed self-view field
	assert.Equal(t, true, resp["isExplorable"])
	assert.Equal(t, false, resp["isDeleted"])
	assert.Equal(t, true, resp["hideOnlineStatus"])
	assert.Equal(t, true, resp["noCrawle"])
	assert.Equal(t, false, resp["preventAiLearning"])
	assert.Equal(t, true, resp["autoSensitive"])
	assert.Equal(t, true, resp["carefulBot"])
	assert.Equal(t, true, resp["autoAcceptFollowed"])
	assert.Equal(t, true, resp["alwaysMarkNsfw"])
	assert.Equal(t, false, resp["receiveAnnouncementEmail"])
	assert.Equal(t, false, resp["injectFeaturedNote"])

	// #985 notification 3 field (profile JSON column 由来)
	emailTypes, ok := resp["emailNotificationTypes"].([]any)
	require.True(t, ok)
	assert.Equal(t, []any{"mention", "reply"}, emailTypes)
	mutingTypes, ok := resp["mutingNotificationTypes"].([]any)
	require.True(t, ok)
	assert.Empty(t, mutingTypes)
	notifConf, ok := resp["notificationRecieveConfig"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, notifConf, "mention")
}

func TestMe_LoggedInDays(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)

	user := &model.User{
		ID:                "user1",
		Username:          "testuser",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
		ChatScope:         "mutual",
	}

	userRepo.Profiles["user1"] = &model.UserProfile{
		UserID:              "user1",
		Fields:              datatypes.JSON([]byte("[]")),
		LoggedInDates:       model.StringArray{"2026-04-01", "2026-04-02", "2026-04-03"},
		FollowersVisibility: model.FollowingVisibilityPublic,
		FollowingVisibility: model.FollowingVisibilityPublic,
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/i", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), user)

	err := h.Me(c)
	require.NoError(t, err)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// #1767: Me が当日 (YYYY/M/D) を loggedInDates に追記するので 3 -> 4。
	assert.Equal(t, float64(4), resp["loggedInDays"])
	require.Len(t, userRepo.Profiles["user1"].LoggedInDates, 4)

	// 2 回目の呼び出しでは当日が既に存在するので増えない (冪等)。
	rec2 := httptest.NewRecorder()
	c2 := e.NewContext(httptest.NewRequest(http.MethodPost, "/api/i", nil), rec2)
	c2.Set(string(middleware.UserContextKey), user)
	require.NoError(t, h.Me(c2))
	var resp2 map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	assert.Equal(t, float64(4), resp2["loggedInDays"], "当日は二重追記しない")
}

func TestMe_BackupCodesStock(t *testing.T) {
	tests := []struct {
		name     string
		enabled  bool
		codes    model.StringArray
		expected string
	}{
		{"disabled 2FA", false, nil, "none"},
		{"enabled but no codes", true, model.StringArray{}, "none"},
		{"enabled with partial codes", true, model.StringArray{"code1", "code2"}, "partial"},
		{"enabled with full codes", true, model.StringArray{"c1", "c2", "c3", "c4", "c5"}, "full"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, userRepo, _, _ := newTestHandler(t)

			user := &model.User{
				ID:                "user1",
				Username:          "testuser",
				AvatarDecorations: datatypes.JSON([]byte("[]")),
				ChatScope:         "mutual",
			}

			userRepo.Profiles["user1"] = &model.UserProfile{
				UserID:                "user1",
				Fields:                datatypes.JSON([]byte("[]")),
				TwoFactorEnabled:      tt.enabled,
				TwoFactorBackupSecret: tt.codes,
				FollowersVisibility:   model.FollowingVisibilityPublic,
				FollowingVisibility:   model.FollowingVisibilityPublic,
			}

			e := echo.New()
			req := httptest.NewRequest(http.MethodPost, "/api/i", nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.Set(string(middleware.UserContextKey), user)

			err := h.Me(c)
			require.NoError(t, err)

			var resp map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, tt.expected, resp["twoFactorBackupCodesStock"])
		})
	}
}

func TestJsonbArray(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		expected any
	}{
		{"nil input", nil, []any{}},
		{"empty input", []byte{}, []any{}},
		{"valid array", []byte(`[{"name":"test"}]`), []any{map[string]any{"name": "test"}}},
		{"invalid JSON", []byte("not json"), []any{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := jsonbArray(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestMe_PackUserDetailedFieldsFlowThrough: #987 review で removed
// した explicit override (avatarId / bannerId / chatScope) が
// PackUserDetailed → MeDetailed embed 経由でも正しく resp に乗ること。
func TestMe_PackUserDetailedFieldsFlowThrough(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)

	avatarID := "av-flow"
	bannerID := "bn-flow"
	user := &model.User{
		ID:                "user-flow",
		Username:          "flowthrough",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
		ChatScope:         "followers",
		AvatarID:          &avatarID,
		BannerID:          &bannerID,
	}
	userRepo.Profiles["user-flow"] = &model.UserProfile{
		UserID: "user-flow",
		Fields: datatypes.JSON([]byte("[]")),
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/i", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), user)

	require.NoError(t, h.Me(c))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// 3 field とも explicit override 削除後も MeDetailed embed 経由で乗る
	assert.Equal(t, "av-flow", resp["avatarId"])
	assert.Equal(t, "bn-flow", resp["bannerId"])
	assert.Equal(t, "followers", resp["chatScope"])
}

func TestMe_AvatarAndBannerIDs(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)

	avatarID := "avatar123"
	bannerID := "banner456"
	user := &model.User{
		ID:                "user1",
		Username:          "testuser",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
		ChatScope:         "mutual",
		AvatarID:          &avatarID,
		BannerID:          &bannerID,
	}

	userRepo.Profiles["user1"] = &model.UserProfile{
		UserID:              "user1",
		Fields:              datatypes.JSON([]byte("[]")),
		FollowersVisibility: model.FollowingVisibilityPublic,
		FollowingVisibility: model.FollowingVisibilityPublic,
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/i", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), user)
	// native session token を配線して isSecure=true にする (#1824)。
	nativeTok := "native-session-token"
	user.Token = &nativeTok
	c.Set(string(middleware.TokenContextKey), nativeTok)

	err := h.Me(c)
	require.NoError(t, err)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "avatar123", resp["avatarId"])
	assert.Equal(t, "banner456", resp["bannerId"])
	assert.NotNil(t, resp["securityKeysList"])
}

// stubRoleProvider implements i.RoleProvider for testing.
type stubRoleProvider struct {
	admin     bool
	moderator bool
	silenced  bool
	roles     []*model.Role
	policies  map[string]any
}

func (s *stubRoleProvider) IsAdministrator(_ string) bool { return s.admin }
func (s *stubRoleProvider) IsModerator(_ string) bool     { return s.moderator }
func (s *stubRoleProvider) IsSilenced(_ string) bool      { return s.silenced }
func (s *stubRoleProvider) GetUserRoles(_ string) ([]*model.Role, error) {
	return s.roles, nil
}
func (s *stubRoleProvider) GetUserPolicies(_ string) map[string]any {
	if s.policies != nil {
		return s.policies
	}
	return map[string]any{}
}

// HasRolePolicy mirrors core/role.Service: admin / moderator は常に true、
// それ以外は policies map から bool 値を取り出す。未配線 / 非 bool は false。
func (s *stubRoleProvider) HasRolePolicy(_, policyKey string) bool {
	if s.admin || s.moderator {
		return true
	}
	if s.policies == nil {
		return false
	}
	v, ok := s.policies[policyKey].(bool)
	return ok && v
}

func TestMe_WithRoleProvider(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetRoleProvider(&stubRoleProvider{
		admin:     true,
		moderator: true,
		roles: []*model.Role{
			{ID: "r1", Name: "Admin", IsAdministrator: true, IsPublic: true, DisplayOrder: 10},
			// 非公開ロールは本人にも出さない (#3240)。権限 (isAdmin 等) には効く。
			{ID: "r2", Name: "internal-tag", IsAdministrator: true, IsPublic: false, DisplayOrder: 20},
			{ID: "r3", Name: "Supporter", IsPublic: true, DisplayOrder: 30},
		},
		policies: map[string]any{"gtlAvailable": true, "driveCapacityMb": 500},
	})

	user := &model.User{
		ID:                "user1",
		Username:          "admin",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/i", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), user)

	err := h.Me(c)
	require.NoError(t, err)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["isAdmin"])
	assert.Equal(t, true, resp["isModerator"])

	roles := resp["roles"].([]any)
	require.Len(t, roles, 2, "the non-public role is not shown")
	// 表示順 (displayOrder) の大きい順 (本家と同じ)。
	assert.Equal(t, "Supporter", roles[0].(map[string]any)["name"])
	assert.Equal(t, "Admin", roles[1].(map[string]any)["name"])

	policies := resp["policies"].(map[string]any)
	assert.Equal(t, float64(500), policies["driveCapacityMb"])
}

// 他の経路 (users/show 等) で自分を表示したときも、非公開ロールを出さない (#3240)。
// EnrichSelf は呼び出し元が絞った roles を上書きするので、ここで絞っていないと
// 呼び出し元の絞り込みが失われる。
func TestEnrichSelf_HidesPrivateRoles(t *testing.T) {
	h := &Handler{roleProvider: &stubRoleProvider{roles: []*model.Role{
		{ID: "pub", Name: "public-role", IsPublic: true},
		{ID: "priv", Name: "internal-tag", IsPublic: false},
	}}}
	resp := map[string]any{"roles": []any{map[string]any{"id": "pub"}}}
	h.EnrichSelf(context.Background(), &model.User{ID: "u1"}, &model.UserProfile{UserID: "u1"}, resp)
	roles := resp["roles"].([]any)
	require.Len(t, roles, 1)
	assert.Equal(t, "pub", roles[0].(map[string]any)["id"])
}

func TestMe_CreatedAtFromValidID(t *testing.T) {
	h, _, _, _ := newTestHandler(t)

	// AIDXで生成した有効なIDを使う
	idGen, _ := id.NewGenerator("aidx")
	validID := idGen.Generate(javaTime())

	user := &model.User{
		ID:                validID,
		Username:          "validid",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/i", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), user)

	err := h.Me(c)
	require.NoError(t, err)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// createdAt は有効なIDから復元される
	createdAt, ok := resp["createdAt"].(string)
	require.True(t, ok, "createdAt should be a string")
	assert.Contains(t, createdAt, "T") // ISO8601 format
}

func javaTime() time.Time {
	return time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
}

func TestMe_NoProfile(t *testing.T) {
	h, _, _, _ := newTestHandler(t)

	user := &model.User{
		ID:                "user1",
		Username:          "noprofile",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/i", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), user)

	err := h.Me(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, "user1", resp["id"])
	assert.Equal(t, "noprofile", resp["username"])
	// profileがない場合、private fieldsはレスポンスに含まれない
	assert.Nil(t, resp["email"])
	// ただしhardcoded fieldsは含まれる
	assert.Equal(t, false, resp["hasUnreadNotification"])
}

// upstream の MeDetailed schema には clientData / room が無い (clientData は
// user_profile の列としてだけ残っていて pack されず、room は機能ごと削除済)。
// mk-go も列は保持するが応答には出さない。
func TestMe_DoesNotExposeClientDataOrRoom(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)

	userRepo.Profiles["user1"] = &model.UserProfile{
		UserID:     "user1",
		ClientData: datatypes.JSON([]byte(`{"theme":"dark"}`)),
		Room:       datatypes.JSON([]byte(`{"furnitures":[{"id":"chair1"}]}`)),
		Fields:     datatypes.JSON([]byte("[]")),
	}

	user := &model.User{
		ID:                "user1",
		Username:          "cdroom",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/i", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), user)

	require.NoError(t, h.Me(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.NotContains(t, resp, "clientData")
	assert.NotContains(t, resp, "room")
}

func TestUpdate_AvatarID_RestrictedByRole(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["u1"] = user

	// policies map で canUpdateBioMedia=false を設定 (admin/moderator フラグ
	// なし → HasRolePolicy が false を返す stub)。
	h.SetRoleProvider(&stubRoleProvider{
		policies: map[string]any{"canUpdateBioMedia": false},
	})

	rec := post(h.Update, `{"avatarId":"file1"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "policy=false で 403")
	assert.Contains(t, rec.Body.String(), "RESTRICTED_BY_ROLE")
	assert.Contains(t, rec.Body.String(), "8feff0ba-5ab5-585b-31f4-4df816663fad")
}

func TestUpdate_AvatarID_AllowedWhenPolicyTrue(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["u1"] = user

	h.SetRoleProvider(&stubRoleProvider{
		policies: map[string]any{"canUpdateBioMedia": true},
	})

	rec := post(h.Update, `{"avatarId":"file1"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code, "policy=true なら通常成功")
}

func TestUpdate_AvatarID_NullClearAllowedRegardlessOfPolicy(t *testing.T) {
	// avatarId=null (= 解除) は policy gate の対象外 (= 自分の avatar 解除は
	// いつでも可能、設定 (新規 set) のみ制限する upstream 互換挙動)。
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["u1"] = user
	h.SetRoleProvider(&stubRoleProvider{
		policies: map[string]any{"canUpdateBioMedia": false},
	})

	rec := post(h.Update, `{"avatarId":null}`, user)
	assert.Equal(t, http.StatusOK, rec.Code, "null clear は gate 対象外")
}

// --- i/update paramDef validation (#1918) ---

func updateUser(repo *testutil.MockUserRepository) *model.User {
	u := &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["u1"] = u
	return u
}

func TestUpdate_NameTooLong(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := updateUser(repo)
	rec := post(h.Update, `{"name":"`+strings.Repeat("a", 51)+`"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
}

func TestUpdate_DescriptionTooLong(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := updateUser(repo)
	rec := post(h.Update, `{"description":"`+strings.Repeat("d", 1501)+`"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_FollowedMessageTooLong(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := updateUser(repo)
	rec := post(h.Update, `{"followedMessage":"`+strings.Repeat("m", 257)+`"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_LocationTooLong(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := updateUser(repo)
	rec := post(h.Update, `{"location":"`+strings.Repeat("l", 51)+`"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 境界: 50 字ちょうどは通る、51 字は弾く (#1918)。
func TestUpdate_NameBoundary(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := updateUser(repo)
	rec := post(h.Update, `{"name":"`+strings.Repeat("a", 50)+`"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code, "50 字ちょうどは valid")
	rec = post(h.Update, `{"name":"`+strings.Repeat("a", 51)+`"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "51 字は INVALID_PARAM")
}

func TestUpdate_BirthdayBadPattern(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := updateUser(repo)
	rec := post(h.Update, `{"birthday":"1999/01/01"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_BirthdayValid(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := updateUser(repo)
	rec := post(h.Update, `{"birthday":"1999-01-01"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestUpdate_LangInvalid(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := updateUser(repo)
	rec := post(h.Update, `{"lang":"klingon"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_LangValid(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := updateUser(repo)
	rec := post(h.Update, `{"lang":"ja-JP"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// 空文字 / null はクリア要求として検証対象外 (#1918)。
func TestUpdate_EmptyValuesPassValidation(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := updateUser(repo)
	rec := post(h.Update, `{"name":"","description":"","birthday":"","lang":""}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// updateWithToken は i/update を UserContextKey + TokenContextKey 付きで叩く。
func updateWithToken(h *Handler, body string, user *model.User, token string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/i/update", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), user)
	c.Set(string(middleware.TokenContextKey), token)
	_ = h.Update(c)
	return rec
}

// #1919: native session (request token == users.token) で i/update を呼ぶと
// email / emailVerified / securityKeysList を含む (includeSecrets)。
func TestUpdate_NativeSessionIncludesSecrets(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	nativeTok := "native-login-token"
	user := &model.User{ID: "user1", Username: "testuser", Token: &nativeTok, AvatarDecorations: datatypes.JSON([]byte("[]")), ChatScope: "mutual"}
	require.NoError(t, userRepo.Create(user))
	email := "secret@example.com"
	// 2FA 有効 + key 登録済 → securityKeysList が format されて返る (#1921 gate を通る)。
	userRepo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Email: &email, EmailVerified: true, TwoFactorEnabled: true, SecurityKeysAvailable: true, Fields: datatypes.JSON([]byte("[]"))}
	skRepo := newInMemSKRepo()
	require.NoError(t, skRepo.Create(&model.UserSecurityKey{ID: "key1", UserID: "user1", Name: "yubikey", PublicKey: "pk"}))
	h.SetWebAuthn(nil, skRepo)

	rec := updateWithToken(h, `{"name":"newname"}`, user, nativeTok)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "secret@example.com", resp["email"])
	assert.Equal(t, true, resp["emailVerified"])
	list, ok := resp["securityKeysList"].([]any)
	require.True(t, ok, "native session は securityKeysList を array で含む")
	require.Len(t, list, 1)
	key := list[0].(map[string]any)
	assert.Equal(t, "key1", key["id"])
	assert.Equal(t, "yubikey", key["name"])
	assert.Contains(t, key, "lastUsed")
	// securityKeys bool も 2FA 有効 + key 有りなので true。
	assert.Equal(t, true, resp["securityKeys"])
}

// #1921: native session でも 2FA 無効なら securityKeysList は [] (repo を引かない)、
// securityKeys bool も SecurityKeysAvailable に関わらず false。
func TestUpdate_SecurityKeysGatedByTwoFactorDisabled(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	nativeTok := "native-login-token"
	user := &model.User{ID: "user1", Username: "testuser", Token: &nativeTok, AvatarDecorations: datatypes.JSON([]byte("[]")), ChatScope: "mutual"}
	require.NoError(t, userRepo.Create(user))
	// 2FA 無効だが key 登録 + SecurityKeysAvailable=true (stale flag) の状況。
	userRepo.Profiles["user1"] = &model.UserProfile{UserID: "user1", TwoFactorEnabled: false, SecurityKeysAvailable: true, Fields: datatypes.JSON([]byte("[]"))}
	skRepo := newInMemSKRepo()
	require.NoError(t, skRepo.Create(&model.UserSecurityKey{ID: "key1", UserID: "user1", Name: "yubikey", PublicKey: "pk"}))
	h.SetWebAuthn(nil, skRepo)

	rec := updateWithToken(h, `{"name":"newname"}`, user, nativeTok)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	list, ok := resp["securityKeysList"].([]any)
	require.True(t, ok, "securityKeysList は存在する (includeSecrets)")
	assert.Empty(t, list, "2FA 無効なら securityKeysList は [] (key があっても)")
	assert.Equal(t, false, resp["securityKeys"], "2FA 無効なら securityKeys は false")
}

// #1919: app access token (request token != users.token) で i/update を呼ぶと
// email / emailVerified / securityKeysList を出さない (over-leak 防止、Me と整合)。
func TestUpdate_AppTokenOmitsSecrets(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	nativeTok := "native-login-token"
	user := &model.User{ID: "user1", Username: "testuser", Token: &nativeTok, AvatarDecorations: datatypes.JSON([]byte("[]")), ChatScope: "mutual"}
	require.NoError(t, userRepo.Create(user))
	email := "secret@example.com"
	userRepo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Email: &email, EmailVerified: true, Fields: datatypes.JSON([]byte("[]"))}

	rec := updateWithToken(h, `{"name":"newname"}`, user, "app-access-token-xyz")
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	_, hasEmail := resp["email"]
	assert.False(t, hasEmail, "app token に email を漏らさない")
	_, hasEmailVerified := resp["emailVerified"]
	assert.False(t, hasEmailVerified)
	_, hasSKL := resp["securityKeysList"]
	assert.False(t, hasSKL, "app token に securityKeysList を漏らさない")
}

func TestUpdate_BannerID_RestrictedByRole(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["u1"] = user
	h.SetRoleProvider(&stubRoleProvider{
		policies: map[string]any{"canUpdateBioMedia": false},
	})

	rec := post(h.Update, `{"bannerId":"file2"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "RESTRICTED_BY_ROLE")
}

// roleProvider 未配線時は gate を skip (= 旧挙動互換)。
func TestUpdate_AvatarID_NoRoleProviderSkipsGate(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["u1"] = user
	// SetRoleProvider を呼ばない
	rec := post(h.Update, `{"avatarId":"file1"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// admin / moderator は policy override で常に通る (HasRolePolicy 仕様)。
func TestUpdate_AvatarID_AdminBypass(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["u1"] = user
	h.SetRoleProvider(&stubRoleProvider{
		admin:    true,
		policies: map[string]any{"canUpdateBioMedia": false},
	})

	rec := post(h.Update, `{"avatarId":"file1"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code, "admin は policy=false でもバイパス")
}

// #1028: i/update で alwaysMarkNsfw role policy=true の user が profile flag
// を変更しようとしたら restrictedByRole (= 403)。
func TestUpdate_AlwaysMarkNsfw_RestrictedByRole(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["u1"] = user
	h.SetRoleProvider(&stubRoleProvider{
		policies: map[string]any{"alwaysMarkNsfw": true},
	})

	rec := post(h.Update, `{"alwaysMarkNsfw":false}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "policy=true で 403")
	assert.Contains(t, rec.Body.String(), "RESTRICTED_BY_ROLE")
	assert.Contains(t, rec.Body.String(), "8feff0ba-5ab5-585b-31f4-4df816663fad")
}

// policy=true でも true 設定 (= NSFW 維持) は意味的に重複でも reject される。
// upstream は `typeof ps.alwaysMarkNsfw === 'boolean'` 条件で gate するので
// true/false どちらの代入も等しく rejected (admin 強制 marker への user 介入
// を一切禁じる)。
func TestUpdate_AlwaysMarkNsfw_RestrictedEvenForTrue(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["u1"] = user
	h.SetRoleProvider(&stubRoleProvider{
		policies: map[string]any{"alwaysMarkNsfw": true},
	})

	rec := post(h.Update, `{"alwaysMarkNsfw":true}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_AlwaysMarkNsfw_AllowedWhenPolicyFalse(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["u1"] = user
	h.SetRoleProvider(&stubRoleProvider{
		policies: map[string]any{"alwaysMarkNsfw": false},
	})

	rec := post(h.Update, `{"alwaysMarkNsfw":true}`, user)
	assert.Equal(t, http.StatusOK, rec.Code, "policy=false なら通常成功")
}

// admin / moderator は alwaysMarkNsfw policy gate を bypass する。
func TestUpdate_AlwaysMarkNsfw_AdminBypass(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["u1"] = user
	h.SetRoleProvider(&stubRoleProvider{
		admin:    true,
		policies: map[string]any{"alwaysMarkNsfw": true},
	})

	rec := post(h.Update, `{"alwaysMarkNsfw":false}`, user)
	assert.Equal(t, http.StatusOK, rec.Code, "admin は policy=true でもバイパス")
}

// req.alwaysMarkNsfw 省略時は policy 無関係に通る (= 他 field 更新を妨げない)。
func TestUpdate_AlwaysMarkNsfw_OmittedSkipsGate(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["u1"] = user
	h.SetRoleProvider(&stubRoleProvider{
		policies: map[string]any{"alwaysMarkNsfw": true},
	})

	rec := post(h.Update, `{"name":"new name"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code, "省略時は gate 通過")
}

func TestUpdate_Success(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user

	rec := post(h.Update, `{"name": "New Name", "description": "hi", "location": "Tokyo", "birthday": "1990-01-01", "lang": "ja", "isLocked": true, "isBot": true, "isCat": true, "isExplorable": false, "hideOnlineStatus": true, "alwaysMarkNsfw": true, "autoSensitive": true, "noCrawle": true, "preventAiLearning": true}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #968: i/update の response は MeDetailed shape (= isExplorable / noCrawle /
// preventAiLearning など self-view-only field を含む) を返すこと。
// フロントの updateCurrentAccountPartial が key 名で merge するため、
// 欠損があると保存後も session が stale なまま残る regression を防ぐ。
func TestUpdate_ResponseContainsMeDetailedFields(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		IsExplorable:      true,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{
		UserID: "user1",
		Fields: datatypes.JSON([]byte("[]")),
	}

	rec := post(h.Update, `{"isExplorable":false,"noCrawle":true,"preventAiLearning":true,"hideOnlineStatus":true}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	// MeDetailed 拡張の全 11 field が key として response shape に含まれること。
	// UserDetailed shape に戻る regression を「key 存在」で素通ししないように、
	// 更新を要求した 4 field については値が **新値** を返していることも確認する。
	assert.Equal(t, false, body["isExplorable"], "MeDetailed field isExplorable は update 後の新値を返す")
	assert.Equal(t, true, body["noCrawle"])
	assert.Equal(t, true, body["preventAiLearning"])
	assert.Equal(t, true, body["hideOnlineStatus"])
	// 残り field は key 存在のみ verify (値は他 test でカバー済み)。
	assert.Contains(t, body, "isDeleted")
	assert.Contains(t, body, "alwaysMarkNsfw")
	assert.Contains(t, body, "autoSensitive")
	assert.Contains(t, body, "carefulBot")
	assert.Contains(t, body, "autoAcceptFollowed")
	assert.Contains(t, body, "receiveAnnouncementEmail")
	assert.Contains(t, body, "injectFeaturedNote")
	// MeDetailed が UserDetailed を embed していることの健全性 check。
	// 将来うっかり embed を外すと「Me 拡張だけ」の壊れた shape になるので
	// UserLite (id/username) と UserDetailed (isLocked) 由来 field の存在を確認。
	assert.Contains(t, body, "id")
	assert.Contains(t, body, "username")
	assert.Contains(t, body, "isLocked")
}

func TestUpdate_FollowedMessageAndPublicReactions(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}

	rec := post(h.Update, `{"followedMessage":"thanks!","publicReactions":false}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	// profile had fields applied
	p := repo.Profiles["user1"]
	require.NotNil(t, p.FollowedMessage)
	assert.Equal(t, "thanks!", *p.FollowedMessage)
	assert.False(t, p.PublicReactions)
}

// #972: i/update が autoAcceptFollowed / carefulBot / injectFeaturedNote /
// receiveAnnouncementEmail を受け付けて profile に persist することを確認
// する。これらは upstream Misskey TS i/update.ts:178-188 paramDef に存在
// するが、mk-go では UpdateRequest struct から抜けていて silent drop される
// drift があった。response 側 (PackMeDetailed #969) には含まれていたので
// 「読めるが書けない」asymmetric drift を 4 field 同時に解消する。
func TestUpdate_ProfileBoolFields_972(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	// 既定値とは逆向きに設定して、reset 期待値が「未設定の Go zero」と
	// 区別できる初期 state を作る。
	repo.Profiles["user1"] = &model.UserProfile{
		UserID:                   "user1",
		Fields:                   datatypes.JSON([]byte("[]")),
		AutoAcceptFollowed:       false,
		CarefulBot:               false,
		InjectFeaturedNote:       true,
		ReceiveAnnouncementEmail: true,
	}

	rec := post(h.Update, `{"autoAcceptFollowed":true,"carefulBot":true,"injectFeaturedNote":false,"receiveAnnouncementEmail":false}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)

	p := repo.Profiles["user1"]
	assert.True(t, p.AutoAcceptFollowed, "autoAcceptFollowed が persist される")
	assert.True(t, p.CarefulBot, "carefulBot が persist される")
	assert.False(t, p.InjectFeaturedNote, "injectFeaturedNote が persist される")
	assert.False(t, p.ReceiveAnnouncementEmail, "receiveAnnouncementEmail が persist される")

	// response body も MeDetailed shape (#969) で 4 field を新値で返す。
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, true, body["autoAcceptFollowed"])
	assert.Equal(t, true, body["carefulBot"])
	assert.Equal(t, false, body["injectFeaturedNote"])
	assert.Equal(t, false, body["receiveAnnouncementEmail"])
}

// #972: 4 field を omit した update は対応 profile field を変えない (= nil
// pointer は no-op) ことを確認する。partial-update セマンティクスの担保。
func TestUpdate_ProfileBoolFields_OmittedIsNoop(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{
		UserID:                   "user1",
		Fields:                   datatypes.JSON([]byte("[]")),
		AutoAcceptFollowed:       true,
		CarefulBot:               true,
		InjectFeaturedNote:       false,
		ReceiveAnnouncementEmail: false,
	}

	// 別 field のみ更新する request — 4 field は不変であるべき。
	rec := post(h.Update, `{"isLocked":true}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)

	p := repo.Profiles["user1"]
	assert.True(t, p.AutoAcceptFollowed, "omit した autoAcceptFollowed は不変")
	assert.True(t, p.CarefulBot, "omit した carefulBot は不変")
	assert.False(t, p.InjectFeaturedNote, "omit した injectFeaturedNote は不変")
	assert.False(t, p.ReceiveAnnouncementEmail, "omit した receiveAnnouncementEmail は不変")
}

func TestUpdate_InvalidJSON(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1"}
	rec := post(h.Update, `{invalid`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// #692: chatScope を i/update で受け付け、user テーブルに反映する。
func TestUpdate_ChatScope_Persisted(t *testing.T) {
	cases := []string{"everyone", "followers", "following", "mutual", "none"}
	for _, scope := range cases {
		t.Run(scope, func(t *testing.T) {
			h, repo, _, _ := newTestHandler(t)
			user := &model.User{ID: "user1", Username: "user1", ChatScope: "mutual", AvatarDecorations: datatypes.JSON([]byte("[]"))}
			repo.Users["user1"] = user
			rec := post(h.Update, `{"chatScope":"`+scope+`"}`, user)
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, scope, repo.Users["user1"].ChatScope)
		})
	}
}

func TestUpdate_ChatScope_InvalidValue(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "user1", ChatScope: "mutual", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user
	rec := post(h.Update, `{"chatScope":"NEVER_VALID"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "mutual", repo.Users["user1"].ChatScope, "不正値で update されないこと")
}

func TestUpdate_ChatScope_OmittedIsNoop(t *testing.T) {
	// JSON に chatScope が含まれない update は ChatScope を変更しない。
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "user1", ChatScope: "followers", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user
	rec := post(h.Update, `{"name":"new"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "followers", repo.Users["user1"].ChatScope)
}

// #1546: followingVisibility / followersVisibility を i/update で受け付け、
// user_profile の対応列に persist して response にも反映する。
func TestUpdate_FollowVisibility_Persisted(t *testing.T) {
	cases := []string{"public", "followers", "private"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			h, repo, _, _ := newTestHandler(t)
			user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
			repo.Users["user1"] = user
			repo.Profiles["user1"] = &model.UserProfile{
				UserID:              "user1",
				Fields:              datatypes.JSON([]byte("[]")),
				FollowingVisibility: model.FollowingVisibilityPublic,
				FollowersVisibility: model.FollowingVisibilityPublic,
			}

			rec := post(h.Update, `{"followingVisibility":"`+v+`","followersVisibility":"`+v+`"}`, user)
			assert.Equal(t, http.StatusOK, rec.Code)
			p := repo.Profiles["user1"]
			assert.Equal(t, model.FollowingVisibility(v), p.FollowingVisibility)
			assert.Equal(t, model.FollowingVisibility(v), p.FollowersVisibility)

			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, v, body["followingVisibility"])
			assert.Equal(t, v, body["followersVisibility"])
		})
	}
}

// 片方だけ指定したときに、指定した側だけ更新され他方は不変であること。
func TestUpdate_FollowVisibility_PartialUpdate(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{
		UserID:              "user1",
		Fields:              datatypes.JSON([]byte("[]")),
		FollowingVisibility: model.FollowingVisibilityPublic,
		FollowersVisibility: model.FollowingVisibilityPublic,
	}

	rec := post(h.Update, `{"followersVisibility":"private"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	p := repo.Profiles["user1"]
	assert.Equal(t, model.FollowingVisibilityPrivate, p.FollowersVisibility)
	assert.Equal(t, model.FollowingVisibilityPublic, p.FollowingVisibility, "followingVisibility は不変であること")
}

// enum 外の値は fail-close で INVALID_PARAM を返し、列を更新しないこと。
func TestUpdate_FollowVisibility_InvalidValue(t *testing.T) {
	for _, field := range []string{"followingVisibility", "followersVisibility"} {
		t.Run(field, func(t *testing.T) {
			h, repo, _, _ := newTestHandler(t)
			user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
			repo.Users["user1"] = user
			repo.Profiles["user1"] = &model.UserProfile{
				UserID:              "user1",
				Fields:              datatypes.JSON([]byte("[]")),
				FollowingVisibility: model.FollowingVisibilityFollowers,
				FollowersVisibility: model.FollowingVisibilityFollowers,
			}

			rec := post(h.Update, `{"`+field+`":"NEVER_VALID"}`, user)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			p := repo.Profiles["user1"]
			assert.Equal(t, model.FollowingVisibilityFollowers, p.FollowingVisibility, "不正値で update されないこと")
			assert.Equal(t, model.FollowingVisibilityFollowers, p.FollowersVisibility, "不正値で update されないこと")
		})
	}
}

// JSON に含まれない update は visibility 列を変更しないこと。
func TestUpdate_FollowVisibility_OmittedIsNoop(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{
		UserID:              "user1",
		Fields:              datatypes.JSON([]byte("[]")),
		FollowingVisibility: model.FollowingVisibilityFollowers,
		FollowersVisibility: model.FollowingVisibilityPrivate,
	}

	rec := post(h.Update, `{"name":"new"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	p := repo.Profiles["user1"]
	assert.Equal(t, model.FollowingVisibilityFollowers, p.FollowingVisibility)
	assert.Equal(t, model.FollowingVisibilityPrivate, p.FollowersVisibility)
}

// #956: i/update で fields を accept して user_profile.fields に persist する。
// trim と空 entry 排除は core/user 側で行う。本 test は handler 経由で
// fields が core まで届くことを確認する。
func TestUpdate_FieldsPersisted(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}

	rec := post(h.Update, `{"fields":[{"name":"  blog ","value":" https://example.com "},{"name":"","value":"empty"},{"name":"x","value":"y"}]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	p := repo.Profiles["user1"]
	require.NotNil(t, p)
	assert.JSONEq(t,
		`[{"name":"blog","value":"https://example.com"},{"name":"x","value":"y"}]`,
		string(p.Fields),
	)
}

// --- #1546 i/update 残り params ---

func newProfileUser(repo *testutil.MockUserRepository) *model.User {
	user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1"}
	return user
}

func TestUpdate_MutedInstancesPersisted(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := newProfileUser(repo)
	rec := post(h.Update, `{"mutedInstances":["evil.example","bad.example"]}`, user)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `["evil.example","bad.example"]`, string(repo.Profiles["user1"].MutedInstances))
}

func TestUpdate_EmailNotificationTypesPersisted(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := newProfileUser(repo)
	rec := post(h.Update, `{"emailNotificationTypes":["mention","reply"]}`, user)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `["mention","reply"]`, string(repo.Profiles["user1"].EmailNotificationTypes))
}

func TestUpdate_NotificationRecieveConfigPersisted(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := newProfileUser(repo)
	rec := post(h.Update, `{"notificationRecieveConfig":{"mention":{"type":"following"}}}`, user)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"mention":{"type":"following"}}`, string(repo.Profiles["user1"].NotificationRecieveConfig))
}

func TestUpdate_RequireSigninToViewContentsPersisted(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := newProfileUser(repo)
	rec := post(h.Update, `{"requireSigninToViewContents":true}`, user)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, repo.Users["user1"].RequireSigninToViewContents)
}

func TestUpdate_MakeNotesBeforeSetAndClear(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := newProfileUser(repo)
	// set
	rec := post(h.Update, `{"makeNotesFollowersOnlyBefore":1700000000,"makeNotesHiddenBefore":1700000001}`, user)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, repo.Users["user1"].MakeNotesFollowersOnlyBefore)
	assert.Equal(t, 1700000000, *repo.Users["user1"].MakeNotesFollowersOnlyBefore)
	require.NotNil(t, repo.Users["user1"].MakeNotesHiddenBefore)
	assert.Equal(t, 1700000001, *repo.Users["user1"].MakeNotesHiddenBefore)
	// clear via null
	rec = post(h.Update, `{"makeNotesFollowersOnlyBefore":null}`, user)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Nil(t, repo.Users["user1"].MakeNotesFollowersOnlyBefore)
	// hidden unchanged (omitted)
	require.NotNil(t, repo.Users["user1"].MakeNotesHiddenBefore)
}

func TestUpdate_PinnedPageOwned(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := newProfileUser(repo)
	pageRepo := testutil.NewMockPageRepository()
	pageRepo.Pages["pg1"] = &model.Page{ID: "pg1", UserID: "user1"}
	h.SetPageRepo(pageRepo)
	rec := post(h.Update, `{"pinnedPageId":"pg1"}`, user)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, repo.Profiles["user1"].PinnedPageID)
	assert.Equal(t, "pg1", *repo.Profiles["user1"].PinnedPageID)
}

func TestUpdate_PinnedPageNotOwned(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := newProfileUser(repo)
	pageRepo := testutil.NewMockPageRepository()
	pageRepo.Pages["pg1"] = &model.Page{ID: "pg1", UserID: "someoneelse"}
	h.SetPageRepo(pageRepo)
	rec := post(h.Update, `{"pinnedPageId":"pg1"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "8e01b590-7eb9-431b-a239-860e086c408e")
}

func TestUpdate_PinnedPageNullClears(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := newProfileUser(repo)
	pinned := "pgOld"
	repo.Profiles["user1"].PinnedPageID = &pinned
	rec := post(h.Update, `{"pinnedPageId":null}`, user)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Nil(t, repo.Profiles["user1"].PinnedPageID)
}

// 空配列 fields=[] を投げると現状を `[]` で上書き (= clear)。nil との挙動
// 差別化を handler 経路で確認する。
func TestUpdate_FieldsEmptyClears(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{
		UserID: "user1",
		Fields: datatypes.JSON([]byte(`[{"name":"x","value":"y"}]`)),
	}

	rec := post(h.Update, `{"fields":[]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[]`, string(repo.Profiles["user1"].Fields))
}

// #960: Update が成功した後に auth middleware の tokenCache を invalidate
// する。同じ token の次 request で stale な user object が返るのを防ぐ。
// invalidator が wire されており request context に token がある場合のみ
// invalidate を呼ぶ — どちらが欠けても panic / 不正呼び出しにならない。
func TestUpdate_InvalidatesTokenCacheOnSuccess(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user

	inv := &stubTokenInvalidator{}
	h.SetAuthInvalidator(inv)

	// middleware が context に詰める想定の auth token を直接 set する。
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"updated"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), user)
	c.Set(string(middleware.TokenContextKey), "the-current-request-token")

	require.NoError(t, h.Update(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{"the-current-request-token"}, inv.calls,
		"成功した i/update は本 request の token を invalidate するべき")
}

// /api/i は middleware が渡す user (tokenCache 由来、30 秒 TTL) ではなく
// repository から取り直した最新の行を返す。フォローや投稿でカウントが変わって
// も反映されないと、プロフィール表示が最大 30 秒古いままになる。
func TestMe_ReturnsFreshUserNotCachedOne(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	// middleware が渡す (古い) user。
	stale := &model.User{ID: "user1", Username: "user1", FollowingCount: 0, NotesCount: 0, AvatarDecorations: datatypes.JSON([]byte("[]"))}
	// repository 側は更新済み。
	repo.Users["user1"] = &model.User{ID: "user1", Username: "user1", FollowingCount: 3, NotesCount: 7, AvatarDecorations: datatypes.JSON([]byte("[]"))}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), stale)

	require.NoError(t, h.Me(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.EqualValues(t, 3, resp["followingCount"], "repository の最新値を返すこと")
	assert.EqualValues(t, 7, resp["notesCount"])
}

// i/update の応答は MeDetailed をそのまま返すので policies / roles を含む。
// フロントは応答を updateCurrentAccountPartial で `$i` にマージするため、
// 欠けているとプロフィール更新のたびに role 由来の権限が消える。
//
// meUpdated (ストリーミング) は逆に policies を含めてはいけない (#1240)。
// 両者が同じ helper を共有しているので、取り違えの回帰ガードとして固定する。
func TestUpdate_ResponseIncludesPolicies(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"updated"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), user)

	require.NoError(t, h.Update(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Contains(t, resp, "policies", "i/update は policies を返すこと")
	assert.NotEmpty(t, resp["policies"], "policies が空だとフロントの $i を clobber する")
	assert.Contains(t, resp, "roles")
	assert.Contains(t, resp, "isAdmin")
	assert.Contains(t, resp, "isModerator")
}

func TestUpdate_NoInvalidatorIsNoop(t *testing.T) {
	// invalidator が wire されていないとき (production では必ず wire するが
	// 単体 test や router 配線忘れ時) は invalidate skip して 200 を返す。
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user

	rec := post(h.Update, `{"name":"updated"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestUpdate_NoTokenInContextIsNoop(t *testing.T) {
	// invalidator は wire 済みだが context に token が無いケース (= 認証
	// pipeline が新旧混在している過渡期や unit test 直叩き)。invalidate
	// 呼び出しは skip し、handler は 200 で完了する。
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user

	inv := &stubTokenInvalidator{}
	h.SetAuthInvalidator(inv)

	rec := post(h.Update, `{"name":"updated"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, inv.calls, "token が context に無いとき invalidate は呼ばれない")
}

// upstream paramDef は maxItems 16。それ超えは INVALID_PARAM (400)。
func TestUpdate_FieldsTooMany(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}

	// 17 entries
	items := ""
	for i := 0; i < 17; i++ {
		if i > 0 {
			items += ","
		}
		items += `{"name":"a","value":"b"}`
	}
	rec := post(h.Update, `{"fields":[`+items+`]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// 失敗したので fields は不変 ([] のまま)
	assert.JSONEq(t, `[]`, string(repo.Profiles["user1"].Fields))
}

func TestUpdate_UserNotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	user := &model.User{ID: "ghost"}
	rec := post(h.Update, `{}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_InternalError(t *testing.T) {
	h, _ := newHandlerWithFailingUpdate(t)
	user := &model.User{ID: "user1"}
	rec := post(h.Update, `{"isLocked": true}`, user)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestUpdate_RoomAccepted(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}

	rec := post(h.Update, `{"room":{"furnitures":[{"id":"bed1"}],"seed":42}}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)

	// profile 側に反映されていること。
	got := repo.Profiles["user1"]
	require.NotNil(t, got)
	assert.JSONEq(t, `{"furnitures":[{"id":"bed1"}],"seed":42}`, string(got.Room))
}

func TestUpdate_RoomMalformedOuterJSONRejected(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user

	// 親 JSON が壊れていれば c.Bind が失敗するため 400。
	rec := post(h.Update, `{"room":{"broken":`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_ProhibitedWordRejected(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user

	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", ProhibitedWordsForNameOfUser: []string{"Spam", "foo"}}
	h.SetMetaRepo(metaRepo)

	// "ContainsSpamString" は "Spam" を含むのでブロック (case-insensitive)。
	// upstream は YOUR_NAME_CONTAINS_PROHIBITED_WORDS / 422 (#1546)。
	rec := post(h.Update, `{"name":"ContainsSPAMString"}`, user)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "YOUR_NAME_CONTAINS_PROHIBITED_WORDS")
	assert.Contains(t, rec.Body.String(), "0b3f9f6a-2f4d-4b1f-9fb4-49d3a2fd7191")
}

func TestUpdate_ProhibitedWordAllowsClear(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user

	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", ProhibitedWordsForNameOfUser: []string{"spam"}}
	h.SetMetaRepo(metaRepo)

	// 空文字 (クリア) は検査対象外 — ユーザーが表示名を外せなくなるのを避ける。
	rec := post(h.Update, `{"name":""}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestUpdate_ProhibitedWordNoMetaSkipped(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user

	// metaRepo 未設定なら検査をスキップ。禁止ワードらしき値でも通る。
	rec := post(h.Update, `{"name":"spammer"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestUpdate_ProhibitedWordEmptyListSkipped(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user

	metaRepo := testutil.NewMockMetaRepository()
	// 空要素だけが入っているケース: 空文字比較で全 name がヒットしないことを検査する。
	metaRepo.Meta = &model.Meta{ID: "x", ProhibitedWordsForNameOfUser: []string{"", "   "}}
	h.SetMetaRepo(metaRepo)

	rec := post(h.Update, `{"name":"anything"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #787 ワードミュート (mutedWords / hardMutedWords) の persist。

// stubHardMutePublisher records published userIDs so tests can assert
// whether i/update fired the wordmute reload event (#791).
type stubHardMutePublisher struct {
	published []string
}

func (s *stubHardMutePublisher) PublishHardMuteReload(userID string) {
	s.published = append(s.published, userID)
}

// #791: hardMutedWords 変更時に publish が走ること。
func TestUpdate_HardMutedWordsTriggersReload(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	pub := &stubHardMutePublisher{}
	h.SetHardMutePublisher(pub)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}

	rec := post(h.Update, `{"hardMutedWords":["spoiler"]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"user1"}, pub.published)
}

// soft mute (mutedWords) の変更だけでは publish しないこと (= reload 不要)。
func TestUpdate_SoftMutedWordsDoesNotTriggerReload(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	pub := &stubHardMutePublisher{}
	h.SetHardMutePublisher(pub)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}

	rec := post(h.Update, `{"mutedWords":[["foo"]]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, pub.published, "soft mute changes must not trigger reload")
}

// publisher 未配線時 (= production の wire 失敗 / test default) は publish 試行
// しないこと (= panic / nil deref しない)。
func TestUpdate_HardMutedWordsNoPublisherIsNoOp(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	// SetHardMutePublisher は呼ばない
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}

	rec := post(h.Update, `{"hardMutedWords":["x"]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestUpdate_MutedWordsAccepted(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}

	rec := post(h.Update, `{"mutedWords":[["foo"],["bar","baz"]],"hardMutedWords":["spoiler"]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)

	got := repo.Profiles["user1"]
	require.NotNil(t, got)
	assert.JSONEq(t, `[["foo"],["bar","baz"]]`, string(got.MutedWords))
	assert.JSONEq(t, `["spoiler"]`, string(got.HardMutedWords))
}

func TestUpdate_MutedWordsEmptyArrayClears(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{
		UserID:         "user1",
		MutedWords:     datatypes.JSON([]byte(`[["pre-existing"]]`)),
		HardMutedWords: datatypes.JSON([]byte(`["pre-existing"]`)),
		Fields:         datatypes.JSON([]byte("[]")),
	}

	rec := post(h.Update, `{"mutedWords":[],"hardMutedWords":[]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)

	got := repo.Profiles["user1"]
	assert.JSONEq(t, `[]`, string(got.MutedWords))
	assert.JSONEq(t, `[]`, string(got.HardMutedWords))
}

func TestUpdate_MutedWordsOmittedLeavesUnchanged(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{
		UserID:     "user1",
		MutedWords: datatypes.JSON([]byte(`[["keep"]]`)),
		Fields:     datatypes.JSON([]byte("[]")),
	}

	rec := post(h.Update, `{"name":"renamed"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)

	got := repo.Profiles["user1"]
	assert.JSONEq(t, `[["keep"]]`, string(got.MutedWords))
}

func TestUpdate_MutedWordsNonArrayRejected(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user

	// 文字列やオブジェクトは reject (frontend 不正実装の防御)。
	rec := post(h.Update, `{"mutedWords":"foo"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	rec2 := post(h.Update, `{"hardMutedWords":{"oops":true}}`, user)
	assert.Equal(t, http.StatusBadRequest, rec2.Code)
}

func TestUpdate_MutedWordsExplicitNullIsOmit(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{
		UserID:     "user1",
		MutedWords: datatypes.JSON([]byte(`[["keep"]]`)),
		Fields:     datatypes.JSON([]byte("[]")),
	}

	// upstream paramDef は nullable: false だが、frontend 不具合で null が
	// 来たケースは clear ではなく omit と同義に倒す (誤クリア事故防止)。
	rec := post(h.Update, `{"mutedWords":null}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)

	got := repo.Profiles["user1"]
	assert.JSONEq(t, `[["keep"]]`, string(got.MutedWords))
}

func TestUpdate_RoomOmittedLeavesUnchanged(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{
		UserID: "user1",
		Room:   datatypes.JSON([]byte(`{"keep":true}`)),
		Fields: datatypes.JSON([]byte("[]")),
	}

	rec := post(h.Update, `{"name":"renamed"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)

	got := repo.Profiles["user1"]
	assert.JSONEq(t, `{"keep":true}`, string(got.Room))
}

// avatarDecorations の検証 (#521)。全 case で stubRoleProvider と
// MockAvatarDecorationRepository を組み合わせて catalog / role / policy を
// 注入する。
func TestUpdate_AvatarDecorations_Success(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}

	deco := testutil.NewMockAvatarDecorationRepository()
	deco.Decorations["dec1"] = &model.AvatarDecoration{ID: "dec1", Name: "Hat", URL: "https://e/x.png"}
	h.SetAvatarDecorationRepo(deco)
	h.SetRoleProvider(&stubRoleProvider{policies: map[string]any{"avatarDecorationLimit": 1}})

	rec := post(h.Update, `{"avatarDecorations":[{"id":"dec1","angle":0.5,"flipH":true,"offsetX":0.1,"offsetY":-0.2}]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)

	// 永続化された JSON が正規化済みであること。
	got := string(repo.Users["user1"].AvatarDecorations)
	assert.JSONEq(t, `[{"id":"dec1","angle":0.5,"flipH":true,"offsetX":0.1,"offsetY":-0.2}]`, got)
}

func TestUpdate_AvatarDecorations_EmptyArrayClears(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte(`[{"id":"dec1","angle":0,"flipH":false,"offsetX":0,"offsetY":0}]`)),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}

	h.SetRoleProvider(&stubRoleProvider{policies: map[string]any{"avatarDecorationLimit": 1}})

	rec := post(h.Update, `{"avatarDecorations":[]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)

	assert.JSONEq(t, `[]`, string(repo.Users["user1"].AvatarDecorations))
}

func TestUpdate_AvatarDecorations_DefaultsAppliedWhenFieldsOmitted(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}

	deco := testutil.NewMockAvatarDecorationRepository()
	deco.Decorations["dec1"] = &model.AvatarDecoration{ID: "dec1"}
	h.SetAvatarDecorationRepo(deco)
	h.SetRoleProvider(&stubRoleProvider{policies: map[string]any{"avatarDecorationLimit": 1}})

	// id だけ指定 — 残り全フィールドは 0 / false で埋まる。
	rec := post(h.Update, `{"avatarDecorations":[{"id":"dec1"}]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[{"id":"dec1","angle":0,"flipH":false,"offsetX":0,"offsetY":0}]`, string(repo.Users["user1"].AvatarDecorations))
}

func TestUpdate_AvatarDecorations_ExceedsLimit(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	deco := testutil.NewMockAvatarDecorationRepository()
	deco.Decorations["dec1"] = &model.AvatarDecoration{ID: "dec1"}
	deco.Decorations["dec2"] = &model.AvatarDecoration{ID: "dec2"}
	h.SetAvatarDecorationRepo(deco)
	h.SetRoleProvider(&stubRoleProvider{policies: map[string]any{"avatarDecorationLimit": 1}})

	rec := post(h.Update, `{"avatarDecorations":[{"id":"dec1"},{"id":"dec2"}]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "TOO_MANY_AVATAR_DECORATIONS")
}

func TestUpdate_AvatarDecorations_UnknownIDRejected(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	deco := testutil.NewMockAvatarDecorationRepository()
	h.SetAvatarDecorationRepo(deco)
	h.SetRoleProvider(&stubRoleProvider{policies: map[string]any{"avatarDecorationLimit": 1}})

	rec := post(h.Update, `{"avatarDecorations":[{"id":"missing"}]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_AVATAR_DECORATION")
}

func TestUpdate_AvatarDecorations_RestrictedByRole(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	deco := testutil.NewMockAvatarDecorationRepository()
	deco.Decorations["dec1"] = &model.AvatarDecoration{ID: "dec1", RoleIDs: model.StringArray{"role-vip"}}
	h.SetAvatarDecorationRepo(deco)
	// ユーザーは role-other のみ所持 → role-vip 必須の dec1 は弾かれる。
	h.SetRoleProvider(&stubRoleProvider{
		roles:    []*model.Role{{ID: "role-other"}},
		policies: map[string]any{"avatarDecorationLimit": 1},
	})

	rec := post(h.Update, `{"avatarDecorations":[{"id":"dec1"}]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "RESTRICTED_BY_ROLE")
}

func TestUpdate_AvatarDecorations_RoleAllowed(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}
	deco := testutil.NewMockAvatarDecorationRepository()
	deco.Decorations["dec1"] = &model.AvatarDecoration{ID: "dec1", RoleIDs: model.StringArray{"role-vip"}}
	h.SetAvatarDecorationRepo(deco)
	h.SetRoleProvider(&stubRoleProvider{
		roles:    []*model.Role{{ID: "role-vip"}, {ID: "role-other"}},
		policies: map[string]any{"avatarDecorationLimit": 1},
	})

	rec := post(h.Update, `{"avatarDecorations":[{"id":"dec1"}]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestUpdate_AvatarDecorations_EmptyIDRejected(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user
	h.SetRoleProvider(&stubRoleProvider{policies: map[string]any{"avatarDecorationLimit": 5}})

	rec := post(h.Update, `{"avatarDecorations":[{"id":""}]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdate_AvatarDecorations_OmittedLeavesUnchanged(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	prev := datatypes.JSON([]byte(`[{"id":"dec1","angle":0,"flipH":false,"offsetX":0,"offsetY":0}]`))
	user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: prev}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}

	rec := post(h.Update, `{"name":"x"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, string(prev), string(repo.Users["user1"].AvatarDecorations))
}

func TestUpdate_AvatarDecorations_NoRepoSkipsCatalogCheck(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}
	h.SetRoleProvider(&stubRoleProvider{policies: map[string]any{"avatarDecorationLimit": 1}})

	// AvatarDecorationRepo 未配線 → catalog 検証 skip。任意 id を受け付ける fallback。
	rec := post(h.Update, `{"avatarDecorations":[{"id":"anything"}]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// role.GetUserPolicies は jsonb override を json.Unmarshal で any に
// 展開するため、数値は float64 で入ってくる。int 限定 assertion だと
// override が silent fallback して既定 limit=1 のままになる回帰を防ぐ。
func TestUpdate_AvatarDecorations_RoleOverrideFloatLimitAccepted(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}
	deco := testutil.NewMockAvatarDecorationRepository()
	deco.Decorations["a"] = &model.AvatarDecoration{ID: "a"}
	deco.Decorations["b"] = &model.AvatarDecoration{ID: "b"}
	deco.Decorations["c"] = &model.AvatarDecoration{ID: "c"}
	h.SetAvatarDecorationRepo(deco)
	// jsonb override 経由を模倣して float64 を渡す。
	h.SetRoleProvider(&stubRoleProvider{policies: map[string]any{"avatarDecorationLimit": float64(3)}})

	rec := post(h.Update, `{"avatarDecorations":[{"id":"a"},{"id":"b"},{"id":"c"}]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestUpdate_AvatarDecorations_NoRoleProviderUsesDefaultLimit(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user

	// roleProvider 未配線 → default limit=1。2 件渡すと 400。
	rec := post(h.Update, `{"avatarDecorations":[{"id":"a"},{"id":"b"}]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Pin ---

func TestPin_Success(t *testing.T) {
	h, repo, noteRepo, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "user1"}

	rec := post(h.Pin, `{"noteId": "n1"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// fakePinHook records OnLocalPinChanged calls for #2024 wiring tests.
type fakePinHook struct {
	calls []struct {
		userID, noteID string
		isAddition     bool
	}
}

func (f *fakePinHook) OnLocalPinChanged(userID, noteID string, isAddition bool) {
	f.calls = append(f.calls, struct {
		userID, noteID string
		isAddition     bool
	}{userID, noteID, isAddition})
}

// #2024: Pin/Unpin 成功時に PinDeliveryHook が (userID, noteID, isAddition) で呼ばれ、
// 失敗時は呼ばれない。
func TestPin_InvokesDeliveryHook(t *testing.T) {
	h, repo, noteRepo, _ := newTestHandler(t)
	hook := &fakePinHook{}
	h.SetPinDeliveryHook(hook)
	user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "user1"}

	require.Equal(t, http.StatusOK, post(h.Pin, `{"noteId":"n1"}`, user).Code)
	require.Len(t, hook.calls, 1)
	assert.Equal(t, "user1", hook.calls[0].userID)
	assert.Equal(t, "n1", hook.calls[0].noteID)
	assert.True(t, hook.calls[0].isAddition, "pin は isAddition=true (#2024)")

	require.Equal(t, http.StatusOK, post(h.Unpin, `{"noteId":"n1"}`, user).Code)
	require.Len(t, hook.calls, 2)
	assert.False(t, hook.calls[1].isAddition, "unpin は isAddition=false (#2024)")

	// pin 失敗 (note 不在) では hook を呼ばない。
	require.Equal(t, http.StatusBadRequest, post(h.Pin, `{"noteId":"ghost"}`, user).Code)
	assert.Len(t, hook.calls, 2, "pin 失敗時は hook を呼ばない (#2024)")
}

func TestPin_NoteNotFound(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user

	rec := post(h.Pin, `{"noteId": "ghost"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPin_AlreadyPinned(t *testing.T) {
	h, repo, noteRepo, _ := newTestHandler(t)
	user := &model.User{ID: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "user1"}
	post(h.Pin, `{"noteId": "n1"}`, user)

	rec := post(h.Pin, `{"noteId": "n1"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPin_LimitExceeded(t *testing.T) {
	h, repo, noteRepo, _ := newTestHandler(t)
	user := &model.User{ID: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user
	for i := 1; i <= coreuser.MaxPinnedNotes; i++ {
		nid := "n" + string(rune('0'+i))
		noteRepo.Notes[nid] = &model.Note{ID: nid, UserID: "user1"}
		post(h.Pin, `{"noteId": "`+nid+`"}`, user)
	}
	noteRepo.Notes["nx"] = &model.Note{ID: "nx", UserID: "user1"}

	rec := post(h.Pin, `{"noteId": "nx"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPin_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1"}
	rec := post(h.Pin, `{}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPin_InternalError(t *testing.T) {
	h, repo, noteRepo := newHandlerWithFailingPiningCount(t)
	user := &model.User{ID: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "user1"}

	rec := post(h.Pin, `{"noteId": "n1"}`, user)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Unpin ---

func TestUnpin_Success(t *testing.T) {
	h, repo, noteRepo, _ := newTestHandler(t)
	user := &model.User{ID: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "user1"}
	post(h.Pin, `{"noteId": "n1"}`, user)

	rec := post(h.Unpin, `{"noteId": "n1"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestUnpin_NotFound(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user

	rec := post(h.Unpin, `{"noteId": "n1"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUnpin_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1"}
	rec := post(h.Unpin, `{}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUnpin_InternalError(t *testing.T) {
	h, mock := newHandlerWithFailingUnpinDelete(t)
	mock.Pinings["p1"] = &model.UserNotePining{ID: "p1", UserID: "user1", NoteID: "n1"}
	user := &model.User{ID: "user1"}

	rec := post(h.Unpin, `{"noteId": "n1"}`, user)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Registry endpoints ---

func newHandlerWithRegistry(t *testing.T) (*Handler, *testutil.MockRegistryRepository) {
	t.Helper()
	h, _, _, _ := newTestHandler(t)
	regRepo := testutil.NewMockRegistryRepository()
	h.SetRegistryRepo(regRepo)
	return h, regRepo
}

func TestRegistrySet_Success(t *testing.T) {
	h, regRepo := newHandlerWithRegistry(t)
	user := &model.User{ID: "u1"}
	rec := post(h.RegistrySet, `{"key":"theme","value":"dark"}`, user)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, regRepo.Items, 1)
}

func TestRegistrySet_InvalidParam(t *testing.T) {
	h, _ := newHandlerWithRegistry(t)
	rec := post(h.RegistrySet, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRegistrySet_PublishesRegistryUpdated_WhenDomainNil(t *testing.T) {
	h, _ := newHandlerWithRegistry(t)
	pub := &stubIMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)

	rec := post(h.RegistrySet, `{"key":"theme","value":"dark","scope":["client"]}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Len(t, pub.calls, 1)
	assert.Equal(t, "u1", pub.calls[0].userID)
	assert.Equal(t, "registryUpdated", pub.calls[0].eventType)
	body, ok := pub.calls[0].body.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "theme", body["key"])
	// scope は []string で渡される
	scope, ok := body["scope"].([]string)
	require.True(t, ok)
	assert.Equal(t, []string{"client"}, scope)
	// value は json.RawMessage (生バイトを frontend にそのまま渡すため)
	val, ok := body["value"].(json.RawMessage)
	require.True(t, ok)
	assert.Equal(t, `"dark"`, string(val))
}

func TestRegistrySet_SkipsPublishWhenDomainSpecified(t *testing.T) {
	h, _ := newHandlerWithRegistry(t)
	pub := &stubIMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)

	// domain指定時はTS本家同様emitしない(サードパーティアプリ領域)
	rec := post(h.RegistrySet, `{"key":"k","value":"v","domain":"app.example"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, pub.calls)
}

func TestRegistrySet_NoPublisher_NoEmit(t *testing.T) {
	h, _ := newHandlerWithRegistry(t)
	// publisher 未設定でも通常動作すること
	rec := post(h.RegistrySet, `{"key":"k","value":"v"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestRegistryGet_Success(t *testing.T) {
	h, _ := newHandlerWithRegistry(t)
	user := &model.User{ID: "u1"}
	post(h.RegistrySet, `{"key":"lang","value":"ja"}`, user)
	rec := post(h.RegistryGet, `{"key":"lang"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRegistryGet_NotFound(t *testing.T) {
	h, _ := newHandlerWithRegistry(t)
	rec := post(h.RegistryGet, `{"key":"ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRegistryGet_InvalidParam(t *testing.T) {
	h, _ := newHandlerWithRegistry(t)
	rec := post(h.RegistryGet, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRegistryGetAll_Success(t *testing.T) {
	h, _ := newHandlerWithRegistry(t)
	user := &model.User{ID: "u1"}
	post(h.RegistrySet, `{"key":"k1","value":1}`, user)
	post(h.RegistrySet, `{"key":"k2","value":2}`, user)
	rec := post(h.RegistryGetAll, `{}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRegistryGetAll_InvalidJSON(t *testing.T) {
	h, _ := newHandlerWithRegistry(t)
	rec := post(h.RegistryGetAll, `invalid`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRegistryKeysWithType_Success(t *testing.T) {
	h, _ := newHandlerWithRegistry(t)
	user := &model.User{ID: "u1"}
	post(h.RegistrySet, `{"key":"theme","value":"dark"}`, user)
	rec := post(h.RegistryKeysWithType, `{}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRegistryKeysWithType_InvalidJSON(t *testing.T) {
	h, _ := newHandlerWithRegistry(t)
	rec := post(h.RegistryKeysWithType, `invalid`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRegistryRemove_Success(t *testing.T) {
	h, regRepo := newHandlerWithRegistry(t)
	user := &model.User{ID: "u1"}
	post(h.RegistrySet, `{"key":"temp","value":true}`, user)
	assert.Len(t, regRepo.Items, 1)
	rec := post(h.RegistryRemove, `{"key":"temp"}`, user)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, regRepo.Items)
}

type failingSetRegistryRepo struct {
	*testutil.MockRegistryRepository
}

func (f *failingSetRegistryRepo) Set(_ *model.RegistryItem) error { return errStub }

type failingGetAllRegistryRepo struct {
	*testutil.MockRegistryRepository
}

func (f *failingGetAllRegistryRepo) GetAll(_ string, _ []string, _ *string) ([]*model.RegistryItem, error) {
	return nil, errStub
}

func (f *failingGetAllRegistryRepo) KeysWithType(_ string, _ []string, _ *string) (map[string]string, error) {
	return nil, errStub
}

type failingRemoveRegistryRepo struct {
	*testutil.MockRegistryRepository
}

func (f *failingRemoveRegistryRepo) Remove(_ string, _ string, _ []string, _ *string) error {
	return errStub
}

func TestRegistrySet_Error(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetRegistryRepo(&failingSetRegistryRepo{testutil.NewMockRegistryRepository()})
	rec := post(h.RegistrySet, `{"key":"k","value":1}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRegistryGetAll_Error(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetRegistryRepo(&failingGetAllRegistryRepo{testutil.NewMockRegistryRepository()})
	rec := post(h.RegistryGetAll, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRegistryKeysWithType_Error(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetRegistryRepo(&failingGetAllRegistryRepo{testutil.NewMockRegistryRepository()})
	rec := post(h.RegistryKeysWithType, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRegistryRemove_Error(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetRegistryRepo(&failingRemoveRegistryRepo{testutil.NewMockRegistryRepository()})
	rec := post(h.RegistryRemove, `{"key":"k"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRegistryRemove_InvalidParam(t *testing.T) {
	h, _ := newHandlerWithRegistry(t)
	rec := post(h.RegistryRemove, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Handler setters ---

func TestSetServerURL(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetServerURL("https://example.com")
}

func TestSetEmailSender(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetEmailSender(func(_ string, _ miscsmtp.Message) {})
}

func TestSetEmailValidationClient(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetEmailValidationClient(&http.Client{})
}

// --- Phase 7-2 (#244): 未読系フィールド実装 ---

// stubUnreadNotification is a local UnreadNotificationSource for tests.
type stubUnreadNotification struct {
	count        int64
	hasTypes     bool
	hasSpecified bool
	gotTypesArgs []notification.Type
}

func (s *stubUnreadNotification) UnreadSummary(_ context.Context, _ string, types []notification.Type) (notification.UnreadSummary, error) {
	s.gotTypesArgs = types
	return notification.UnreadSummary{
		TotalCount:       s.count,
		HasMentions:      s.hasTypes,
		HasSpecifiedNote: s.hasSpecified,
	}, nil
}

type stubAnnouncementRepo struct {
	rows []*model.Announcement
}

func (s *stubAnnouncementRepo) UnreadForUser(_ string) ([]*model.Announcement, error) {
	return s.rows, nil
}

type stubChatRepo struct{ n int64 }

func (s *stubChatRepo) CountUnread(_ string) (int64, error) { return s.n, nil }

// runMe invokes h.Me with a minimal user + profile seeded and returns the
// decoded response.
func runMe(t *testing.T, h *Handler, userRepo *testutil.MockUserRepository, userID string) map[string]any {
	t.Helper()
	u := &model.User{
		ID: userID, Username: "u", AvatarDecorations: datatypes.JSON([]byte("[]")),
		ChatScope: "mutual",
	}
	userRepo.Profiles[userID] = &model.UserProfile{
		UserID: userID, Fields: datatypes.JSON([]byte("[]")),
	}
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/i", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), u)
	require.NoError(t, h.Me(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func TestMe_UnreadNotification_Populated(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	stub := &stubUnreadNotification{count: 3, hasTypes: true}
	h.SetNotificationService(stub)

	resp := runMe(t, h, userRepo, "u1")
	assert.Equal(t, float64(3), resp["unreadNotificationsCount"])
	assert.Equal(t, true, resp["hasUnreadNotification"])
	assert.Equal(t, true, resp["hasUnreadMentions"])
	assert.Equal(t, []notification.Type{notification.TypeMention, notification.TypeReply}, stub.gotTypesArgs)
}

func TestMe_UnreadNotification_ZeroCount(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	h.SetNotificationService(&stubUnreadNotification{count: 0, hasTypes: false})

	resp := runMe(t, h, userRepo, "u1")
	assert.Equal(t, float64(0), resp["unreadNotificationsCount"])
	assert.Equal(t, false, resp["hasUnreadNotification"])
	assert.Equal(t, false, resp["hasUnreadMentions"])
}

func TestMe_PendingFollowRequest(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	frRepo := testutil.NewMockFollowRequestRepository()
	require.NoError(t, frRepo.Create(&model.FollowRequest{ID: "fr1", FollowerID: "other", FolloweeID: "u1"}))
	h.SetFollowRequestRepo(frRepo)

	resp := runMe(t, h, userRepo, "u1")
	assert.Equal(t, true, resp["hasPendingReceivedFollowRequest"])
}

func TestMe_UnreadAnnouncement(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	// 実 aidx ID を使うことで PackAnnouncement の createdAt が ParseTime で
	// 復元され、非null の timestamp 文字列になることを保証する。
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	annID := gen.Generate(time.Now())
	h.SetAnnouncementRepo(&stubAnnouncementRepo{
		rows: []*model.Announcement{{ID: annID, Title: "hello", Text: "world"}},
	})

	resp := runMe(t, h, userRepo, "u1")
	assert.Equal(t, true, resp["hasUnreadAnnouncement"])
	arr := resp["unreadAnnouncements"].([]any)
	assert.Len(t, arr, 1)
	first := arr[0].(map[string]any)
	assert.Equal(t, annID, first["id"])
	assert.Equal(t, "hello", first["title"])
	assert.Equal(t, "world", first["text"])
	// createdAt は misskey_dart の AnnouncementsResponse が非null String として
	// cast するため、キーが存在し空でないことを必須とする (#1224 回帰防止)。
	createdAt, ok := first["createdAt"].(string)
	assert.True(t, ok, "createdAt must be a non-null string")
	assert.NotEmpty(t, createdAt)
}

func TestMe_UnreadAnnouncement_SkipsNil(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	annID := gen.Generate(time.Now())
	// nil entry が混じっても [null] を出力せず除外されることを保証する。
	h.SetAnnouncementRepo(&stubAnnouncementRepo{
		rows: []*model.Announcement{nil, {ID: annID, Title: "hello", Text: "world"}},
	})

	resp := runMe(t, h, userRepo, "u1")
	arr := resp["unreadAnnouncements"].([]any)
	assert.Len(t, arr, 1)
	assert.NotNil(t, arr[0])
	first := arr[0].(map[string]any)
	assert.Equal(t, annID, first["id"])
}

func TestMe_UnreadAnnouncement_Empty(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	h.SetAnnouncementRepo(&stubAnnouncementRepo{rows: nil})

	resp := runMe(t, h, userRepo, "u1")
	assert.Equal(t, false, resp["hasUnreadAnnouncement"])
	arr := resp["unreadAnnouncements"].([]any)
	assert.Empty(t, arr)
}

func TestMe_UnreadChatMessages(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	h.SetChatRepo(&stubChatRepo{n: 5})

	resp := runMe(t, h, userRepo, "u1")
	assert.Equal(t, true, resp["hasUnreadChatMessages"])
}

func TestMe_UnreadFieldsDefaults(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	resp := runMe(t, h, userRepo, "u1")
	assert.Equal(t, float64(0), resp["unreadNotificationsCount"])
	assert.Equal(t, false, resp["hasUnreadNotification"])
	assert.Equal(t, false, resp["hasUnreadMentions"])
	assert.Equal(t, false, resp["hasPendingReceivedFollowRequest"])
	assert.Equal(t, false, resp["hasUnreadAnnouncement"])
	assert.Equal(t, false, resp["hasUnreadChatMessages"])
	assert.Equal(t, false, resp["hasUnreadAntenna"])
	assert.Equal(t, false, resp["hasUnreadChannel"])
	assert.Equal(t, false, resp["hasUnreadSpecifiedNotes"])
}

// stubUnreadSource is a minimal AntennaUnread / ChannelUnreadSource stub.
type stubUnreadSource struct{ has bool }

func (s *stubUnreadSource) HasAnyByUser(_ string) (bool, error) { return s.has, nil }

func TestMe_UnreadAntennaAndChannel_Populated(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	h.SetAntennaUnreadRepo(&stubUnreadSource{has: true})
	h.SetChannelUnreadRepo(&stubUnreadSource{has: true})
	resp := runMe(t, h, userRepo, "u1")
	assert.Equal(t, true, resp["hasUnreadAntenna"])
	assert.Equal(t, true, resp["hasUnreadChannel"])
}

func TestMe_HasUnreadSpecifiedNotes_Populated(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	h.SetNotificationService(&stubUnreadNotification{hasSpecified: true})
	resp := runMe(t, h, userRepo, "u1")
	assert.Equal(t, true, resp["hasUnreadSpecifiedNotes"])
}

// --- Phase 7-3 (#245): pinnedNotes / pinnedPage ---

func TestMe_PinnedNoteIDs_Populated(t *testing.T) {
	h, userRepo, noteRepo, _ := newTestHandler(t)

	piningRepo := testutil.NewMockUserNotePiningRepository()
	require.NoError(t, piningRepo.Create(&model.UserNotePining{ID: "p1", UserID: "u1", NoteID: "note_a"}))
	require.NoError(t, piningRepo.Create(&model.UserNotePining{ID: "p2", UserID: "u1", NoteID: "note_b"}))
	h.SetPiningRepo(piningRepo)

	// noteRepo に 2 件の note を挿入 (FindManyByIDsWithUser の戻り対象)
	txtA := "a"
	noteRepo.Notes["note_a"] = &model.Note{ID: "note_a", UserID: "u1", Text: &txtA, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
	txtB := "b"
	noteRepo.Notes["note_b"] = &model.Note{ID: "note_b", UserID: "u1", Text: &txtB, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
	h.SetNoteRepo(noteRepo)

	resp := runMe(t, h, userRepo, "u1")

	ids, ok := resp["pinnedNoteIds"].([]any)
	require.True(t, ok)
	assert.Len(t, ids, 2)

	notes, ok := resp["pinnedNotes"].([]any)
	require.True(t, ok)
	assert.Len(t, notes, 2)
}

func TestMe_PinnedNoteIDs_EmptyDefault(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	// 依存 未wire → default empty
	resp := runMe(t, h, userRepo, "u1")
	ids, ok := resp["pinnedNoteIds"].([]any)
	require.True(t, ok)
	assert.Empty(t, ids)
	notes, _ := resp["pinnedNotes"].([]any)
	assert.Empty(t, notes)
}

// stubPageRepo to avoid DB.
type stubPageRepo struct {
	page *model.Page
	err  error
}

func (s *stubPageRepo) Create(*model.Page) error                              { return nil }
func (s *stubPageRepo) FindByID(id string) (*model.Page, error)               { return s.page, s.err }
func (s *stubPageRepo) FindManyByIDs([]string) ([]*model.Page, error)         { return nil, nil }
func (s *stubPageRepo) FindByUserAndName(string, string) (*model.Page, error) { return nil, nil }
func (s *stubPageRepo) UpdateFields(string, map[string]any) error             { return nil }
func (s *stubPageRepo) Delete(*model.Page) error                              { return nil }
func (s *stubPageRepo) ListByUser(string, string, string, int, int) ([]*model.Page, error) {
	return nil, nil
}
func (s *stubPageRepo) ListPublicByUser(string, string, string, int, int) ([]*model.Page, error) {
	return nil, nil
}
func (s *stubPageRepo) ListFeatured(string, string, int, int) ([]*model.Page, error) { return nil, nil }
func (s *stubPageRepo) IncrementCount(string, string, int) error                     { return nil }

func TestMe_PinnedPage_Populated(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)

	pageID := "page_123"
	userRepo.Profiles["u1"] = &model.UserProfile{
		UserID:       "u1",
		Fields:       datatypes.JSON([]byte("[]")),
		PinnedPageID: &pageID,
	}

	h.SetPageRepo(&stubPageRepo{page: &model.Page{ID: pageID, Title: "pinned", UserID: "u1"}})

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/i", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), &model.User{
		ID: "u1", Username: "u", AvatarDecorations: datatypes.JSON([]byte("[]")),
		ChatScope: "mutual",
	})
	require.NoError(t, h.Me(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, pageID, resp["pinnedPageId"])
	pg, ok := resp["pinnedPage"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "pinned", pg["title"])
	// golden Page は user 必須。pinnedPage に owner=u を渡す修正 (#1266 fu) を
	// /api/i 側でも回帰 guard する (user が present で id が self と一致)。
	pgUser, ok := pg["user"].(map[string]any)
	require.True(t, ok, "pinnedPage.user must be present (golden Page requires user)")
	assert.Equal(t, "u1", pgUser["id"])

	// L3 (#1270): 実 /api/i レスポンス map を golden User-family schema に突合する。
	// struct ではなく handler が PackMeDetailed + fillUnreadFields + override で
	// 実際に組み立てた map を検証するので、populate 漏れも検出できる。
	shapetest.Assert(t, "UserLite", resp)
	shapetest.Assert(t, "UserDetailedNotMeOnly", resp)
	shapetest.Assert(t, "MeDetailedOnly", resp)
}

func TestMe_PinnedPage_NoPinnedPageID(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	resp := runMe(t, h, userRepo, "u1")
	assert.Nil(t, resp["pinnedPageId"])
	assert.Nil(t, resp["pinnedPage"])
}

// #739: 多くの setter (SetNoteFieldResolver / SetInstanceRepo / SetEmojiRepo /
// SetReactionReader) は wire のみで lookup の non-nil 分岐は別パスで踏む。
// ここでは構築 + setter 呼び出しのみで panic しないことを担保する。
func TestSetters_NotPanic(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetNoteFieldResolver(nil)
	h.SetInstanceRepo(testutil.NewMockInstanceRepository())
	h.SetEmojiRepo(testutil.NewMockEmojiRepository())
	h.SetReactionReader(stubBufferedReactions{})
}

// stubBufferedReactions implements entity.BufferedReactionsReader as a no-op
// for setter wiring tests (#739)。
type stubBufferedReactions struct{}

func (stubBufferedReactions) GetBufferedMany(_ context.Context, _ []string) (map[string]map[string]int64, error) {
	return map[string]map[string]int64{}, nil
}

// #739: verifyURL は serverURL fallback と path 結合の単純関数だが UpdateEmail
// 経路で email 送信が起きないと cover されない。直接呼んで両分岐 (serverURL
// 未設定 / 設定済み) を担保する。
func TestVerifyURL(t *testing.T) {
	h := &Handler{}
	// serverURL 未設定 → https://localhost フォールバック
	assert.Equal(t, "https://localhost/verify-email/abc", h.verifyURL("abc"))

	h2 := &Handler{serverURL: "https://example.com"}
	assert.Equal(t, "https://example.com/verify-email/abc", h2.verifyURL("abc"))
}

// #2094: /api/i は moderator viewer (= self が moderator) のとき moderationNote を
// emit する (空なら "")。非 moderator では field を出さない (upstream の undefined 相当)。
func TestMe_ModerationNote_ModeratorGated(t *testing.T) {
	mn := "keep an eye on this account"
	callMe := func(t *testing.T, h *Handler, user *model.User) map[string]any {
		t.Helper()
		e := echo.New()
		req := httptest.NewRequest(http.MethodPost, "/api/i", nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.Set(string(middleware.UserContextKey), user)
		require.NoError(t, h.Me(c))
		require.Equal(t, http.StatusOK, rec.Code)
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp
	}

	t.Run("moderator sees moderationNote", func(t *testing.T) {
		h, userRepo, _, _ := newTestHandler(t)
		h.SetRoleProvider(&stubRoleProvider{moderator: true})
		user := &model.User{ID: "u1", Username: "u1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
		userRepo.Users["u1"] = user
		userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", ModerationNote: &mn, Fields: datatypes.JSON([]byte("[]"))}
		resp := callMe(t, h, user)
		assert.Equal(t, mn, resp["moderationNote"])
	})

	t.Run("non-moderator omits moderationNote", func(t *testing.T) {
		h, userRepo, _, _ := newTestHandler(t)
		h.SetRoleProvider(&stubRoleProvider{moderator: false})
		user := &model.User{ID: "u2", Username: "u2", AvatarDecorations: datatypes.JSON([]byte("[]"))}
		userRepo.Users["u2"] = user
		userRepo.Profiles["u2"] = &model.UserProfile{UserID: "u2", ModerationNote: &mn, Fields: datatypes.JSON([]byte("[]"))}
		resp := callMe(t, h, user)
		_, present := resp["moderationNote"]
		assert.False(t, present, "non-moderator must not receive moderationNote")
	})
}

// #2106 N1: i/update のレスポンス (MeDetailed) は moderator/admin に対し isAdmin/isModerator
// を実値で返す (Me handler と同様 roleProvider から override)。
func TestUpdate_ReturnsRoleFlags(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["u1"] = user
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Fields: datatypes.JSON([]byte("[]"))}
	h.SetRoleProvider(&stubRoleProvider{admin: true, moderator: true})

	rec := post(h.Update, `{"name":"updated"}`, user)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["isAdmin"], "admin の i/update は isAdmin=true を返す")
	assert.Equal(t, true, resp["isModerator"])
}

// **registry の `key` / `domain` も列に入らない値を弾く (#3025)。** scope だけ
// 検証していたので、`key = ?` / `domain = ?` の bind parameter に NUL が乗って
// 500 になっていた (`i/registry/*` は**任意の認証ユーザー**が叩ける)。
func TestRegistryRejectsUnstorableKeyAndDomain(t *testing.T) {
	user := &model.User{ID: "u1"}
	for _, tt := range []struct {
		name string
		call func(h *Handler) func(echo.Context) error
		body string
	}{
		{"set の key", func(h *Handler) func(echo.Context) error { return h.RegistrySet }, `{"key":"a\u0000b","value":"x"}`},
		{"set の domain", func(h *Handler) func(echo.Context) error { return h.RegistrySet }, `{"key":"theme","value":"x","domain":"a\u0000b"}`},
		{"get-all の domain", func(h *Handler) func(echo.Context) error { return h.RegistryGetAll }, `{"domain":"a\u0000b"}`},
		{"keys の domain", func(h *Handler) func(echo.Context) error { return h.RegistryKeys }, `{"domain":"a\u0000b"}`},
		{"remove の key", func(h *Handler) func(echo.Context) error { return h.RegistryRemove }, `{"key":"a\u0000b"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := newHandlerWithRegistry(t)
			rec := post(tt.call(h), tt.body, user)
			assert.Equal(t, http.StatusBadRequest, rec.Code,
				"列に入らない値を SQL へ流している: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
		})
	}
}

// **jsonb 列へそのまま入る field を弾く (#3037)。** PostgreSQL は NUL
// エスケープを SQLSTATE 22P05 でクエリごと落とすので、引く前に弾かないと
// **任意の認証ユーザーが 500 を起こせる**。
func TestUpdate_UnstorableJSONBFieldsAre400(t *testing.T) {
	esc := `\u0000`
	for name, body := range map[string]string{
		"room":                      `{"room":{"a":"x` + esc + `y"}}`,
		"mutedWords":                `[["x` + esc + `y"]]`,
		"notificationRecieveConfig": `{"notificationRecieveConfig":{"x` + esc + `y":{"type":"all"}}}`,
		"mutedInstances":            `{"mutedInstances":["x` + esc + `y"]}`,
		"emailNotificationTypes":    `{"emailNotificationTypes":["x` + esc + `y"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			payload := body
			if name == "mutedWords" {
				payload = `{"mutedWords":` + body + `}`
			}
			h, repo, _, _ := newTestHandler(t)
			user := updateUser(repo)
			rec := post(h.Update, payload, user)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "列に入らない jsonb を受け入れている")
			assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
		})
	}
}

// **普通の値は通ったまま。** これが無いと「常に拒否する」実装でも上が通る。
func TestUpdate_OrdinaryJSONBFieldsStillPass(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := updateUser(repo)
	rec := post(h.Update, `{"room":{"あ":"絵文字"},"mutedInstances":["example.com"]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// **`fields` も jsonb 列へ入る (#3037)。**
//
// ここはプレーンな `string` なので NUL のエスケープが実 NUL バイトとして
// decode され、`core/user` が `json.Marshal` で戻したものが jsonb へ渡って
// SQLSTATE 22P05 で落ちる。`json.RawMessage` の一覧とは経路が違うので、
// 手で持つ一覧から漏れていた (敵対的レビューで実測)。
func TestUpdate_UnstorableFieldsAre400(t *testing.T) {
	esc := `\u0` + `000`
	for name, body := range map[string]string{
		"name に NUL":  `{"fields":[{"name":"a` + esc + `b","value":"x"}]}`,
		"value に NUL": `{"fields":[{"name":"a","value":"x` + esc + `y"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			h, repo, _, _ := newTestHandler(t)
			user := updateUser(repo)
			rec := post(h.Update, body, user)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "列に入らない fields を受け入れている")
			assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
		})
	}
}

// **普通の fields は通ったまま。** これが無いと「常に拒否する」実装でも上が通る。
func TestUpdate_OrdinaryFieldsStillPass(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := updateUser(repo)
	rec := post(h.Update, `{"fields":[{"name":"サイト","value":"https://example.com"}]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// ロールのアイコンは entity (meUpdated の経路) と同じく media proxy を通す。
// 通さないと $i.roles[].iconUrl が /api/i とストリーミングで食い違う。
func TestRolePayload_ProxiesRoleIcon(t *testing.T) {
	entity.SetMediaURLContext(entity.NewMediaURLContext(
		"https://mk.test", "https://mk.test/proxy", []byte("role-icon-secret"), false, true))
	defer entity.SetMediaURLContext(nil)
	icon := "https://cdn.example.test/role.png"
	h := &Handler{roleProvider: &stubRoleProvider{roles: []*model.Role{
		{ID: "pub", Name: "public-role", IsPublic: true, IconURL: &icon},
	}}}
	_, _, _, roles := h.rolePayload("u1")
	require.Len(t, roles, 1)
	got := roles[0].(map[string]any)["iconUrl"].(*string)
	require.NotNil(t, got)
	assert.Equal(t, *entity.ProxyMediaURLPtr(&icon), *got)
	assert.NotEqual(t, icon, *got, "a remote icon goes through the proxy")
}
