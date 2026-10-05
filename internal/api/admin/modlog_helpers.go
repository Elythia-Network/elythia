package admin

import (
	"log/slog"

	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// logModeration is the generic moderation log writer. Callers build the
// info map themselves to match the type-specific schema expected by the
// frontend (suspend → {userId, userUsername, userHost}, updateRole →
// {roleId, before, after}, etc.).
//
// fire-and-forget: no error returned. All failure modes — unwired
// service, missing actor (RequireModerator misconfiguration) — are
// silently dropped or logged at warn level so audit logging never
// blocks or fails the moderation action itself.
//
// Callers should invoke this only AFTER the underlying moderation
// action has succeeded so the log reflects observed state changes
// rather than attempts.
func (h *Handler) logModeration(c echo.Context, t moderationlog.LogType, info map[string]any) {
	if h.modLogService == nil {
		return
	}
	ctx := c.Request().Context()
	actor := middleware.GetUser(c)
	if actor == nil {
		// RequireModerator middleware should guarantee an actor; if we
		// hit this branch in production something is misconfigured and
		// we want it visible rather than silently dropped.
		slog.WarnContext(ctx, "moderation log: skipping — actor missing in moderator-only handler",
			"type", t)
		return
	}
	h.modLogService.Log(ctx, actor.ID, t, info)
}

// logModerationMany batches N moderation_log writes into a single goroutine
// + single multi-row INSERT. Used by bulk admin operations (#671) to avoid
// fanning out N goroutines + N round-trips when the operator deletes / edits
// hundreds of items at once.
//
// Same fire-and-forget semantics as logModeration. Empty entries / unwired
// service / missing actor all degrade silently. Callers should invoke this
// only AFTER the underlying bulk action has succeeded.
func (h *Handler) logModerationMany(c echo.Context, entries []moderationlog.Entry) {
	if h.modLogService == nil || len(entries) == 0 {
		return
	}
	ctx := c.Request().Context()
	actor := middleware.GetUser(c)
	if actor == nil {
		slog.WarnContext(ctx, "moderation log: skipping batch — actor missing in moderator-only handler",
			"count", len(entries))
		return
	}
	h.modLogService.LogMany(ctx, actor.ID, entries)
}

// logUserAction records a moderation_log entry for an action that
// targets a single user, looking up the user by ID. The {userId,
// userUsername, userHost} payload is built from a fresh user lookup,
// matching the Misskey TS frontend schema for user-targeted log types
// (suspend, unsuspend, resetPassword, deleteAccount, unsetUserAvatar,
// unsetUserBanner).
//
// Convenience wrapper around logModeration. For user-targeted log
// types that need extra fields (updateUserNote with before/after,
// assignRole with role info, etc.), build the info map yourself and
// call logModeration directly. When the caller already has the
// *model.User in scope (e.g. SuspendUser called FindByID before
// flipping the flag), prefer logUserActionWithUser to avoid a second
// DB lookup.
func (h *Handler) logUserAction(c echo.Context, t moderationlog.LogType, targetUserID string) {
	if h.userRepo == nil {
		return
	}
	target, err := h.userRepo.FindByID(targetUserID)
	if err != nil || target == nil {
		// Rare race (user deleted concurrently with the moderation
		// action) or callers passing a bogus id — debug-level so
		// production noise stays low while still leaving a trail when
		// debugging missing log entries.
		slog.DebugContext(c.Request().Context(), "moderation log: target user not found",
			"type", t, "targetUserId", targetUserID)
		return
	}
	h.logUserActionWithUser(c, t, target)
}

// logUserActionWithUser is the same as logUserAction but takes the
// already-looked-up user object, skipping the redundant FindByID.
// Use this from handlers that load the target user up-front for their
// own logic (e.g. existence check) so we don't pay for two SELECTs.
func (h *Handler) logUserActionWithUser(c echo.Context, t moderationlog.LogType, target *model.User) {
	if target == nil {
		return
	}
	h.logModeration(c, t, moderationlog.UserInfo(target))
}

// logRoleAssignment records a moderation_log entry for assignRole /
// unassignRole. Combines UserInfo with role identifier/name; the
// frontend's modlog.ModLog.vue branch reads {userId, userUsername,
// userHost, roleId, roleName} for these types. roleService lookup
// failures are tolerated (best-effort): the user info is logged even
// if the role snapshot is unavailable.
func (h *Handler) logRoleAssignment(c echo.Context, t moderationlog.LogType, targetUserID, roleID string) {
	if h.userRepo == nil {
		return
	}
	target, err := h.userRepo.FindByID(targetUserID)
	if err != nil || target == nil {
		return
	}
	info := moderationlog.UserInfo(target)
	info["roleId"] = roleID
	if h.roleService != nil {
		if r, err := h.roleService.Show(roleID); err == nil && r != nil {
			info["roleName"] = r.Name
		}
	}
	h.logModeration(c, t, info)
}
