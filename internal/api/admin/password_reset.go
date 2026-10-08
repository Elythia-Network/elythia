package admin

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/misc"
	"github.com/elythia-network/elythia/internal/misc/password"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// ResetPassword handles POST /api/admin/reset-password.
//
// upstream reset-password.ts は常に secureRndstr(8) で 8 文字の英数字パスワードを
// 生成し、その場で対象ユーザーの password を更新して {"password": "..."} を返す
// (res schema は minLength/maxLength=8)。標準管理 UI (admin-user.vue) は返却を
// `const {password} = ...` で分割代入して表示するため、mk-go もこれに揃える (#1825)。
//
// 旧 #186 の verified-email 経路 ({"sent": true} を返し reset link を送る) は標準 UI で
// password が undefined になり drop-in を壊すため撤去した。ユーザー自身が行う
// パスワードリセット (/request-reset-password → /reset-password) は別経路で存続する。
func (h *Handler) ResetPassword(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "userId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// upstream reset-password.ts は findOneBy({id}) で対象を引き、user==null なら
	// 'user not found' を throw する。mk-go は他の admin user-lookup (#1542 の roles
	// assign 等) と揃えて NO_SUCH_USER を返す (#1862)。これが無いと存在しない userId
	// でも UpdateProfile の 0 行 UPDATE が no-op で成功し、実際には設定されていない
	// 偽の password を 200 で返してしまう。userRepo 未配線時は guard を skip
	// (production では常に配線済み)。
	var target *model.User
	if h.userRepo != nil {
		user, err := h.userRepo.FindByID(req.UserID)
		// **DB 障害を not-found に丸めない** (#2792)。管理者によるパスワード
		// リセットで障害を 400 にすると、対象が居ないのか DB が落ちているのか
		// 区別できない。
		if err != nil && !repository.IsNotFound(err) {
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		if err != nil || user == nil {
			// #2106 L2: upstream reset-password.ts:26 固有の UUID に揃える。
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "ccafc7fe-5074-4edd-9dc0-8ef9ef6a701d"))
		}
		// **プラグインが管理するアカウントにはパスワードを作らない (#3468)。**
		// この endpoint は新しいパスワードを応答に載せて返すので、通すとそのまま
		// サインインできる相手になる。管理者が相手でも拒否する。
		if user.IsPluginManaged() {
			return c.JSON(http.StatusBadRequest, apierr.PluginManagedAccount())
		}
		// upstream 2026.7.0 (fork merge): moderator が administrator の password を
		// リセットできた improper authorization の修正。旧 root-only guard
		// (CANNOT_RESET_PASSWORD_OF_ROOT_USER) は廃止され、「対象が administrator
		// かつ実行者 != 対象」を ACCESS_DENIED で弾く。IsAdministrator は root を
		// 含むため root 保護は維持される。
		// **system アカウントと他のモデレーターも塞ぐ (#3037)。** この
		// endpoint は新しいパスワードを応答に載せて返すので、対象として
		// 選べる相手はそのままサインインできる相手になる。
		denied, undetermined := h.credentialTakeoverDenied(c, user)
		if undetermined {
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		if denied {
			return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "cda8f8ce-89a6-4f92-8055-33bbe0c1464d"))
		}
		target = user
	}
	// upstream secureRndstr(8) = 英数字 8 文字。
	newPass := misc.SecureRandomString(8, misc.AlphanumericChars)
	hash, err := password.Hash(newPass)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if h.userRepo != nil {
		if err := h.userRepo.UpdateProfile(req.UserID, map[string]any{"password": hash}); err != nil {
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
	}
	// 存在チェックで既に target を引いているので二度引きしない (logUserActionWithUser)。
	h.logUserActionWithUser(c, moderationlog.LogResetPassword, target)
	return c.JSON(http.StatusOK, map[string]any{"password": newPass})
}
