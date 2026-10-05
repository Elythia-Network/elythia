package following

import (
	"errors"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// Invalidate handles POST /api/following/invalidate.
//
// 認証ユーザーが自分のフォロワーのうち userId 指定の相手を強制的に外す。
// Misskey 本家互換で RequireAuth レベル (moderator 不要) のため、嫌がらせ
// フォロワーに対して本人がいつでも解除できる。
func (h *Handler) Invalidate(c echo.Context) error {
	me := middleware.GetUser(c)
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// upstream invalidate.ts は self check → getUser → Following row 検査の
	// 順で FOLLOWER_IS_YOURSELF / NO_SUCH_USER / NOT_FOLLOWING を返し、成功時
	// は packed follower (= 外された相手) を返す (#1562)。
	if req.UserID == me.ID {
		return c.JSON(http.StatusBadRequest, apierr.Error("FOLLOWER_IS_YOURSELF", "Follower is yourself.", "07dc03b9-03da-422d-885b-438313707662"))
	}
	bundle, err := h.userService.ShowByID(req.UserID)
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "b77e6ae6-a3e5-40da-9cc8-c240115479cc"))
	}
	if err := h.followingService.Unfollow(req.UserID, me.ID); err != nil {
		if errors.Is(err, corefollowing.ErrNotFollowing) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NOT_FOLLOWING", "The other use is not following you.", "918faac3-074f-41ae-9c43-ed5d2946770d"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, entity.PackUserLite(bundle.User))
}

// UpdateFollow handles POST /api/following/update.
//
// notify ("normal" / "none") / withReplies を呼び出しユーザーの指定 followee
// に対して更新する。
//
// upstream Misskey TS は `ps.notify === 'none' ? null : ps.notify` の三項で
// "none" を SQL NULL に変換する仕様 (#17385)。これがないと `following/list`
// の `notification=true` filter (= `notify IS NOT NULL`) が機能不全になる
// (mk-go では旧来 "none" 文字列がそのまま入って null 扱いされなかった)。
func (h *Handler) UpdateFollow(c echo.Context) error {
	me := middleware.GetUser(c)
	var req struct {
		UserID      string  `json:"userId"`
		Notify      *string `json:"notify"`
		WithReplies *bool   `json:"withReplies"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// upstream update.ts は self check → getUser → Following row 検査の順で
	// FOLLOWEE_IS_YOURSELF / NO_SUCH_USER / NOT_FOLLOWING を返し、成功時は
	// packed follower (= 自分) を返す (#1562)。
	if req.UserID == me.ID {
		return c.JSON(http.StatusBadRequest, apierr.Error("FOLLOWEE_IS_YOURSELF", "Followee is yourself.", "4c4cbaf9-962a-463b-8418-a5e365dbf2eb"))
	}
	if _, err := h.userService.ShowByID(req.UserID); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "14318698-f67e-492a-99da-5353a5ac52be"))
	}
	if following, err := h.followingService.IsFollowing(me.ID, req.UserID); err != nil {
		return apierr.JSONInternalError(c)
	} else if !following {
		return c.JSON(http.StatusBadRequest, apierr.Error("NOT_FOLLOWING", "You are not following that user.", "b8dc75cf-1cb5-46c9-b14b-5f1ffbd782c9"))
	}
	fields := map[string]any{}
	if req.Notify != nil {
		// upstream update.ts paramDef は notify:{enum:['normal','none']}。enum 外を
		// verbatim 保存すると DB 列が壊れるので ajv 同様 400 で弾く (#2027)。
		if !validNotify(*req.Notify) {
			return apierr.JSONInvalidParam(c)
		}
		fields["notify"] = normalizeNotify(*req.Notify)
	}
	if req.WithReplies != nil {
		fields["withReplies"] = *req.WithReplies
	}
	if err := h.followingService.UpdateRelation(me.ID, req.UserID, fields); err != nil {
		return apierr.JSONInternalError(c)
	}
	meBundle, err := h.userService.ShowByID(me.ID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, entity.PackUserLite(meBundle.User))
}

// validNotify reports whether v is one of upstream's `notify` enum values
// ('normal' / 'none')。enum 外は ajv で 400 になるので caller で弾く (#2027)。
func validNotify(v string) bool {
	return v == "normal" || v == "none"
}

// normalizeNotify converts the upstream `notify` enum ("normal" / "none") to
// the value stored in the DB column. "none" → nil (SQL NULL) so the
// `following/list` notification=true filter (= `notify IS NOT NULL`) correctly
// excludes users with notifications turned off (upstream Misskey TS 2026.5.2
// #17385 互換)。"normal" / other → そのまま文字列として保存。
func normalizeNotify(v string) any {
	if v == "none" {
		return nil
	}
	return v
}

// UpdateFollowAll handles POST /api/following/update-all.
//
// 呼び出しユーザーの全フォロー関係に対して同じ partial update を適用する。
// 旅行期間中 notify を一括で無効にする等の運用に使う。
func (h *Handler) UpdateFollowAll(c echo.Context) error {
	me := middleware.GetUser(c)
	var req struct {
		Notify      *string `json:"notify"`
		WithReplies *bool   `json:"withReplies"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	fields := map[string]any{}
	if req.Notify != nil {
		// upstream update-all.ts も notify:{enum:['normal','none']} (#2027)。
		if !validNotify(*req.Notify) {
			return apierr.JSONInvalidParam(c)
		}
		// upstream #17385 互換: notify="none" は SQL NULL に正規化する。
		fields["notify"] = normalizeNotify(*req.Notify)
	}
	if req.WithReplies != nil {
		fields["withReplies"] = *req.WithReplies
	}
	if len(fields) == 0 {
		return c.NoContent(http.StatusNoContent)
	}
	if err := h.followingService.UpdateAllByFollower(me.ID, fields); err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}
