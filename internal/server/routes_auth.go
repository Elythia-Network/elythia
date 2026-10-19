package server

import (
	apiapp "github.com/elythia-network/elythia/internal/api/app"
	apiauth "github.com/elythia-network/elythia/internal/api/auth"
	apiinvite "github.com/elythia-network/elythia/internal/api/invite"
	apiresetpassword "github.com/elythia-network/elythia/internal/api/resetpassword"
	apisignin "github.com/elythia-network/elythia/internal/api/signin"
	apisignup "github.com/elythia-network/elythia/internal/api/signup"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerAuthRoutes registers the sign-up, sign-in, password reset,
// invitation and app authorisation routes on the /api group.
func registerAuthRoutes(api apiRoutes, signupHandler *apisignup.Handler, signinHandler *apisignin.Handler, resetHandler *apiresetpassword.Handler, authHandler *apiauth.Handler, appHandler *apiapp.Handler, inviteHandler *apiinvite.Handler, roleService *corerole.Service) {
	api.POST("/signup", signupHandler.Signup)
	// 承認制の登録 (#2554 / #2556)。認証は不要 — 本人確認は MiAuth が担う。
	// 有効になっていない構成では 503 を返す。
	api.POST("/signup-application/form-token", signupHandler.ApplicationFormToken)
	api.POST("/signup-application/apply", signupHandler.ApplicationApply)
	api.POST("/signup-application/status", signupHandler.ApplicationStatus)
	api.POST("/signup-application/register", signupHandler.ApplicationRegister)
	api.POST("/signup-pending", signupHandler.SignupPending)

	api.POST("/username/available", signupHandler.UsernameAvailable)

	api.POST("/signin", signinHandler.Signin)
	api.POST("/signin-flow", signinHandler.SigninFlow)
	api.POST("/signin-with-passkey", signinHandler.SigninWithPasskey)

	api.POST("/request-reset-password", resetHandler.RequestReset)
	api.POST("/reset-password", resetHandler.Reset)

	api.POST("/auth/session/generate", authHandler.SessionGenerate)
	api.POST("/auth/session/show", authHandler.SessionShow)
	api.POST("/auth/session/userkey", authHandler.SessionUserkey)
	api.POST("/auth/accept", authHandler.Accept, middleware.RequireAuth(), middleware.RequireSecure())

	// miauth/gen-token — アクセストークン直接生成
	api.POST("/miauth/gen-token", authHandler.GenToken, middleware.RequireAuth(), middleware.RequireSecure())

	api.POST("/miauth/:session/check", authHandler.MiAuthCheck)

	// app/* — アプリ管理API
	api.POST("/app/create", appHandler.Create)
	api.POST("/app/show", appHandler.Show)
	api.POST("/my/apps", appHandler.MyApps, middleware.RequireAuth(), middleware.RequireScope("read:account"))

	api.POST("/email-address/available", signupHandler.EmailAvailable)

	api.POST("/invite/create", inviteHandler.Create,
		middleware.RequireAuth(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanInvite), middleware.RequireScope("write:invite-codes"))

	// upstream は invite/list・delete・limit にも create と同じ
	// requiredRolePolicy:'canInvite' を付ける。canInvite 非保持ユーザーが招待コード
	// 一覧/削除/残数取得を実行できないよう gate を揃える (#1776)。
	api.POST("/invite/list", inviteHandler.List,
		middleware.RequireAuth(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanInvite), middleware.RequireScope("read:invite-codes"))
	api.POST("/invite/delete", inviteHandler.Delete,
		middleware.RequireAuth(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanInvite), middleware.RequireScope("write:invite-codes"))
	api.POST("/invite/limit", inviteHandler.Limit,
		middleware.RequireAuth(),
		middleware.RequireRolePolicy(roleService, corerole.PolicyCanInvite), middleware.RequireScope("read:invite-codes"))
}
