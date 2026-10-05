package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
)

// stubRoleChecker は RequireModerator / RequireAdmin 用の RoleChecker stub。
// 本テストは role gate ではなく scope gate (RequireScope) の配線を検証する
// のが目的なので、role は常に許可してスコープ判定だけを残す。
type stubRoleChecker struct{}

func (stubRoleChecker) IsAdministrator(string) bool { return true }
func (stubRoleChecker) IsModerator(string) bool     { return true }

// TestRequireScopeWiring は #1607 の中核契約を end-to-end で検証する。
// 既存の auth_test.go は AuthScope を context に直接 set して RequireScope 単体
// を見ているだけだが、ここでは router.go と同一の middleware 合成
// (グローバル auth.Authenticate + 各 route の RequireAuth/RequireModerator +
// RequireScope("<kind>")) を実 HTTP request で通し、access_token の permission
// から scope が解決され kind が強制されるまでの経路全体を担保する。
//
// 代表 route は scope family ごとに 1 本ずつ選ぶ (write:notes / write:account /
// write:drive / write:chat / read:account(public) / write:admin:*)。router.go の
// 該当行とミドルウェア構成を一致させること。
func TestRequireScopeWiring(t *testing.T) {
	role := stubRoleChecker{}

	// 代表 route 表。mws は router.go の該当行と同じ順序・構成にする
	// (RequireScope は role/auth gate の後)。
	cases := []struct {
		name       string
		path       string // /api を含むフルパス
		kind       string
		mws        []echo.MiddlewareFunc
		requireCre bool // RequireAuth を伴う (= anonymous は 401)
	}{
		{
			name:       "notes/create write:notes",
			path:       "/api/notes/create",
			kind:       "write:notes",
			mws:        []echo.MiddlewareFunc{middleware.RequireAuth(), middleware.RequireScope("write:notes")},
			requireCre: true,
		},
		{
			name:       "i/update write:account",
			path:       "/api/i/update",
			kind:       "write:account",
			mws:        []echo.MiddlewareFunc{middleware.RequireAuth(), middleware.RequireScope("write:account")},
			requireCre: true,
		},
		{
			name:       "drive/files/create write:drive",
			path:       "/api/drive/files/create",
			kind:       "write:drive",
			mws:        []echo.MiddlewareFunc{middleware.RequireAuth(), middleware.RequireScope("write:drive")},
			requireCre: true,
		},
		{
			name:       "chat/messages/create-to-user write:chat",
			path:       "/api/chat/messages/create-to-user",
			kind:       "write:chat",
			mws:        []echo.MiddlewareFunc{middleware.RequireAuth(), middleware.RequireScope("write:chat")},
			requireCre: true,
		},
		{
			// clips/show は requireCredential:false の公開 read だが kind を持つ。
			// RequireAuth は付かず、anonymous は素通し / app token は scope 検査。
			name:       "clips/show read:account (public)",
			path:       "/api/clips/show",
			kind:       "read:account",
			mws:        []echo.MiddlewareFunc{middleware.RequireScope("read:account")},
			requireCre: false,
		},
		{
			name:       "admin/suspend-user write:admin:suspend-user",
			path:       "/api/admin/suspend-user",
			kind:       "write:admin:suspend-user",
			mws:        []echo.MiddlewareFunc{middleware.RequireModerator(role), middleware.RequireScope("write:admin:suspend-user")},
			requireCre: true,
		},
	}

	const (
		nativeTok   = "native-login-token"
		narrowTok   = "app-token-narrow"   // permission: read:account のみ
		broadTok    = "app-token-broad"    // permission: 全代表 kind を保有
		emptyAppTok = "app-token-no-scope" // permission: [] (app だが scope 無し)
	)

	// broad app token が保有する scope 集合 (= 全代表 kind + read:account)。
	broadScopes := model.StringArray{
		"read:account", "write:notes", "write:account", "write:drive",
		"write:chat", "write:admin:suspend-user",
	}

	newEnv := func() (*echo.Echo, func(path, token string) *httptest.ResponseRecorder) {
		userRepo := testutil.NewMockUserRepository()
		tokenRepo := testutil.NewMockAccessTokenRepository()

		appUser := &model.User{ID: "app-user"}
		nativeUser := &model.User{ID: "native-user"}

		// native login token → full access (IsApp=false)。
		userRepo.Tokens[nativeTok] = nativeUser
		// app access token (narrow / broad / empty)。
		tokenRepo.Tokens[narrowTok] = &model.AccessToken{Token: narrowTok, Permission: model.StringArray{"read:account"}, User: appUser}
		tokenRepo.Tokens[broadTok] = &model.AccessToken{Token: broadTok, Permission: broadScopes, User: appUser}
		tokenRepo.Tokens[emptyAppTok] = &model.AccessToken{Token: emptyAppTok, Permission: model.StringArray{}, User: appUser}

		auth := middleware.NewAuthMiddleware(userRepo, tokenRepo)

		e := echo.New()
		// router.go と同様 auth.Authenticate() をグローバル前段に置く。
		e.Use(auth.Authenticate())
		api := e.Group("/api")
		// router.go と同様 JSONBodyParse を api group の先頭に置く (#1609)。
		api.Use(middleware.JSONBodyParse())
		for _, tc := range cases {
			// /api prefix を group が付けるので relative path で登録。
			api.POST(tc.path[len("/api"):], func(c echo.Context) error {
				return c.NoContent(http.StatusOK)
			}, tc.mws...)
		}

		do := func(path, token string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, path, nil)
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			return rec
		}
		return e, do
	}

	_, do := newEnv()

	assertPermissionDenied := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusForbidden, rec.Code)
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		errObj, ok := resp["error"].(map[string]any)
		require.True(t, ok, "error envelope を持つ")
		assert.Equal(t, "PERMISSION_DENIED", errObj["code"])
		assert.Equal(t, "1370e5b7-d4eb-4566-bb1d-7748ee6a1838", errObj["id"])
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 1. native login token → full access で常に通過 (200)。
			assert.Equal(t, http.StatusOK, do(tc.path, nativeTok).Code, "native token は通過")

			// 2. broad app token (kind を保有) → 通過 (200)。
			assert.Equal(t, http.StatusOK, do(tc.path, broadTok).Code, "kind を持つ app token は通過")

			// 3. narrow app token (read:account のみ保有)。
			narrowRec := do(tc.path, narrowTok)
			if tc.kind == "read:account" {
				assert.Equal(t, http.StatusOK, narrowRec.Code, "read:account 保有なので clips/show は通過")
			} else {
				assertPermissionDenied(t, narrowRec)
			}

			// 4. scope 空の app token → 必ず 403 (read:account route でも保有しない)。
			assertPermissionDenied(t, do(tc.path, emptyAppTok))

			// 5. anonymous: RequireAuth を伴う route は 401、公開 route は 200。
			anonRec := do(tc.path, "")
			if tc.requireCre {
				assert.Equal(t, http.StatusUnauthorized, anonRec.Code, "credential 必須 route は anonymous を 401")
			} else {
				assert.Equal(t, http.StatusOK, anonRec.Code, "公開 route は anonymous を通過")
			}
		})
	}
}
