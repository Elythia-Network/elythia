package admin

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/gonecleanup"
	"github.com/shiroha-a/mk/internal/core/moderationlog"
	"github.com/shiroha-a/mk/internal/repository"
)

// GoneInstanceCleaner lists goneSuspended instances and removes the follow
// relations left with them (#3067).
type GoneInstanceCleaner interface {
	List() ([]repository.GoneInstanceSummary, error)
	Clean(host string) (gonecleanup.Result, error)
}

// SetGoneInstanceCleaner wires the gone-instance cleanup.
func (h *Handler) SetGoneInstanceCleaner(c GoneInstanceCleaner) {
	h.goneCleaner = c
}

type goneInstanceResponse struct {
	Host string `json:"host"`
	// SuspendedAt は goneSuspended になった時刻。記録が無い (TS が立てた停止など)
	// ときは null。
	SuspendedAt    *time.Time `json:"suspendedAt"`
	Followers      int64      `json:"followers"`
	Following      int64      `json:"following"`
	FollowRequests int64      `json:"followRequests"`
}

// FederationGoneInstances handles POST /api/admin/federation/gone-instances.
//
// **mk-go 独自 endpoint** (#3067)。goneSuspended のホストと、そのホストとの間に
// 残っているフォロー関係の件数を返す。upstream に対応物は無い。
func (h *Handler) FederationGoneInstances(c echo.Context) error {
	out := []goneInstanceResponse{}
	if h.goneCleaner == nil {
		return c.JSON(http.StatusOK, out)
	}
	list, err := h.goneCleaner.List()
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	for _, g := range list {
		out = append(out, goneInstanceResponse{
			Host: g.Host, SuspendedAt: g.SuspendedAt,
			Followers: g.Followers, Following: g.Following, FollowRequests: g.FollowRequests,
		})
	}
	return c.JSON(http.StatusOK, out)
}

// FederationCleanGoneInstance handles POST /api/admin/federation/clean-gone-instance.
//
// **mk-go 独自 endpoint** (#3067)。goneSuspended のホストとローカルの利用者の間の
// フォロー関係 (両方向) とフォローリクエストを消す。**取り返しがつかない**ので
// 自動では実行せず、管理者がこの口から明示的に実行する。
//
// upstream の `admin/federation/remove-all-following` と違い、**ローカルの利用者の
// フォローも消す**。相手はもう存在しないので、残しておいても意味の無い行になる。
func (h *Handler) FederationCleanGoneInstance(c echo.Context) error {
	var req struct {
		Host string `json:"host"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	host := normalizeRequestHost(req.Host)
	if host == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "host is required.", "96c56be5-a921-44f6-b914-21bce995c2eb"))
	}
	if h.goneCleaner == nil {
		return apierr.JSONInternalError(c)
	}
	res, err := h.goneCleaner.Clean(host)
	if errors.Is(err, gonecleanup.ErrInProgress) {
		return c.JSON(http.StatusBadRequest, apierr.Error("CLEANUP_IN_PROGRESS", "Cleanup of this instance is already running.", "d6238277-da77-4025-8d23-66e89a381876"))
	}
	// **途中で止まっても、消した分は記録する** (途中で戻されて ErrNotGone に
	// なった場合も含む)。取り返しがつかない変更を監査ログから落とさない。
	// 止まったときは remaining を書かない — 数えていないので 0 と書くと
	// 「全部片付いた」と読める。
	if removed := res.RemovedFollowers + res.RemovedFollowing + res.RemovedFollowRequests; removed > 0 || err == nil {
		info := map[string]any{
			"host":                  host,
			"removedFollowers":      res.RemovedFollowers,
			"removedFollowing":      res.RemovedFollowing,
			"removedFollowRequests": res.RemovedFollowRequests,
		}
		if err == nil {
			info["remaining"] = res.Remaining
		} else {
			info["failed"] = true
		}
		h.logModeration(c, moderationlog.LogCleanGoneInstance, info)
	}
	if errors.Is(err, gonecleanup.ErrNotGone) {
		return c.JSON(http.StatusBadRequest, apierr.Error("INSTANCE_NOT_GONE", "The instance is not goneSuspended.", "42db9eef-d896-4501-ac17-d468fdbd3eca"))
	}
	if err != nil {
		slog.Error("admin: clean gone instance failed", "host", host, "err", err)
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, res)
}
