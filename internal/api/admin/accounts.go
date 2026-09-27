package admin

import (
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/moderationlog"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/repository"
)

// AccountsDelete handles POST /api/admin/accounts/delete.
func (h *Handler) AccountsDelete(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.NoContent(http.StatusNoContent)
	}
	// root / system アカウントの削除は連合を壊すため拒否する (#parity review F1)。
	// **判定できないときは 500 に倒す** — 分からないまま不可逆な削除を通さない。
	switch protected, undetermined := h.isProtectedAccount(req.UserID); {
	case undetermined:
		return apierr.JSONInternalError(c)
	case protected:
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Cannot delete a root or system account.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
	}
	// #2230: local user は物理削除 (Soft=false)、remote user は tombstone (Soft=true)。
	user, _ := h.userRepo.FindByID(req.UserID)
	if err := h.userRepo.UpdateUser(req.UserID, map[string]any{"isSuspended": true, "isDeleted": true}); err == nil {
		// **モデレーターの判断として刻む** (#2973)。刻まないと、発信元由来の
		// 凍結が `remote` のまま残っている行では、発信元が `toot:suspended` を
		// 下ろした時点で tombstone の凍結が解除される。inbound の gate は
		// `isSuspended` しか見ないので、削除済みアカウントからの activity が
		// 再び通ることになる。
		h.recordLocalSuspensionOrigin(req.UserID)
		// 論理削除直後の auth bypass 防止 (#965)。target の全 token cache
		// entry を即時 invalidate して 30s stale window を消す。DB 更新が
		// 失敗したケースでは cache を触る理由がないので、err 成功時のみ。
		h.invalidateUserTokenCache(req.UserID)
		h.revokeUserStreams(req.UserID)
		h.logUserAction(c, moderationlog.LogDeleteAccount, req.UserID)
	}
	h.scheduleAccountCascade(req.UserID, user != nil && user.Host != nil)
	return c.NoContent(http.StatusNoContent)
}

// AccountsFindByEmail handles POST /api/admin/accounts/find-by-email.
// user_profile.email 列を検索して、紐づく user を返す。本家 Misskey の
// admin/accounts/find-by-email と同等。
func (h *Handler) AccountsFindByEmail(c echo.Context) error {
	var req struct {
		Email string `json:"email"`
	}
	if err := c.Bind(&req); err != nil || req.Email == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "email is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	profile, err := h.userRepo.FindProfileByEmail(req.Email)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("USER_NOT_FOUND", "User not found.", "cb865949-8af5-4062-a88c-ef55e8786d1d"))
	}
	user, err := h.userRepo.FindByID(profile.UserID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("USER_NOT_FOUND", "User not found.", "cb865949-8af5-4062-a88c-ef55e8786d1d"))
	}
	// upstream admin/accounts/find-by-email.ts は pack(user, null,
	// {schema:'UserDetailedNotMe'}) (includeSecrets 無し) を返すため email /
	// emailVerified / securityKeysList は含めない。packAdminUser を使うとこれら
	// includeSecrets 限定 field が漏れる (#1847、#1822 と同 class)。UserDetailed に
	// 揃えて過剰露出を防ぐ (ShowUsers と同方針)。生 model.User の内部 field
	// (inbox/sharedInbox/usernameLower) も UserDetailed では出ない。
	return c.JSON(http.StatusOK, h.packModeratorVisibleUser(user, profile))
}

// DeleteAccount handles POST /api/admin/delete-account. AccountsDelete と
// 機能的には同じだが、本家 Misskey が両 endpoint を持つので互換性のため
// 別 handler として保持する。
func (h *Handler) DeleteAccount(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.NoContent(http.StatusNoContent)
	}
	// root / system アカウントの削除は連合を壊すため拒否する (#parity review F1)。
	// **判定できないときは 500 に倒す** — 分からないまま不可逆な削除を通さない。
	switch protected, undetermined := h.isProtectedAccount(req.UserID); {
	case undetermined:
		return apierr.JSONInternalError(c)
	case protected:
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Cannot delete a root or system account.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
	}
	// AP Delete(actor) 配信のため、更新前に user を控える (#1759)。
	user, _ := h.userRepo.FindByID(req.UserID)
	if err := h.userRepo.UpdateUser(req.UserID, map[string]any{"isSuspended": true, "isDeleted": true}); err == nil {
		// **モデレーターの判断として刻む** (#2973)。刻まないと、発信元由来の
		// 凍結が `remote` のまま残っている行では、発信元が `toot:suspended` を
		// 下ろした時点で tombstone の凍結が解除される。inbound の gate は
		// `isSuspended` しか見ないので、削除済みアカウントからの activity が
		// 再び通ることになる。
		h.recordLocalSuspensionOrigin(req.UserID)
		// AccountsDelete と同じ。target の全 token cache entry を即時
		// invalidate (#965)。
		h.invalidateUserTokenCache(req.UserID)
		h.revokeUserStreams(req.UserID)
		h.logUserAction(c, moderationlog.LogDeleteAccount, req.UserID)
	}
	// upstream DeleteAccountService: local user は物理削除 job の前に全 sharedInbox
	// へ Delete(actor) を配信する (#1759)。deliver は cascade より先に enqueue され、
	// cascade も note purge を先に行うため実運用では署名鍵が生存した状態で配信される
	// (別 queue ゆえ厳密な順序保証は無い best-effort、upstream も同構造)。
	if user != nil && h.userModerationFed != nil {
		h.userModerationFed.OnUserDeleted(user)
	}
	// #2230: local user (host==nil) は user 行を物理削除 (Soft=false)、remote user は再連合での
	// 復活を防ぐため tombstone として残す (Soft=true)。upstream DeleteAccountService の
	// isLocalUser 分岐に対応する。
	soft := user != nil && user.Host != nil
	h.scheduleAccountCascade(req.UserID, soft)
	return c.NoContent(http.StatusNoContent)
}

// isProtectedAccount reports whether the given user ID is an account that must
// never be deleted: the instance root account or a local system account
// (instance.actor / relay.actor / proxy.actor 等)。upstream DeleteAccountService
// の `meta.rootUserId === user.id` (root) と `user.host === null &&
// username.includes('.')` (system account) ガードに対応する (#parity review F1)。
// Returns false when the user cannot be resolved.
func (h *Handler) isProtectedAccount(userID string) (protected bool, undetermined bool) {
	if h.userRepo == nil || userID == "" {
		return false, false
	}
	// **判定できないときは「分からない」を返す (#2792 / #3037)。**
	//
	// かつては meta / user の lookup 失敗を黙って握り潰して false を返して
	// いた。本番の root は `isRoot = false` (列を足した migration より後に
	// 作られていない) なので **meta が唯一の判定材料**で、そこを読めない窓では
	// root の保護が発火しない。通ると `user` 行が削除され、`meta.rootUserId` が
	// 消えた ID を指したまま残って API 経由で復旧できなくなる。
	//
	// 同じ不変条件を守る `i/delete-account` / `targetIsRoot` /
	// `suspend-user` / `show-user` は #3037 で「判定できない → 500」に直って
	// おり、**admin の削除経路だけが取り残されていた**。
	if h.metaRepo != nil {
		meta, err := h.metaRepo.Fetch()
		if err != nil {
			slog.Error("admin: cannot determine whether the target is root", "userId", userID, "err", err)
			return false, true
		}
		if meta != nil && meta.RootUserID != nil && *meta.RootUserID == userID {
			return true, false
		}
	}
	u, err := h.userRepo.FindByID(userID)
	if err != nil {
		if repository.IsNotFound(err) {
			// 居ないものは保護対象でもない。
			return false, false
		}
		slog.Error("admin: cannot look up the delete target", "userId", userID, "err", err)
		return false, true
	}
	if u == nil {
		return false, false
	}
	if u.IsRoot {
		return true, false
	}
	return isSystemAccountUser(u), false
}

// scheduleAccountCascade queues the background cascade deletion. Errors
// from the enqueuer are logged but never surfaced — the admin flag flip
// is the user-visible source of truth, so a failed enqueue only delays
// the cleanup until the next manual retry.
func (h *Handler) scheduleAccountCascade(userID string, soft bool) {
	if h.deleteAccountEnqueuer == nil || userID == "" {
		return
	}
	if err := h.deleteAccountEnqueuer.EnqueueDeleteAccount(queue.DeleteAccountPayload{UserID: userID, Soft: soft}); err != nil {
		slog.Warn("admin: enqueue delete-account failed", "userId", userID, "err", err)
	}
}
