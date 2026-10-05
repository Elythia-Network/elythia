package roles

import (
	"errors"
	"net/http"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// AssignmentShow handles POST /api/roles/assignment-show.
//
// **見るのは role_assignment 行だけで、conditional role の condFormula は評価しない**
// (#2633)。conditional role は行を持たないので、条件を満たしている viewer にも
// assigned=false を返す。role.target を返しているのは呼び出し側がこれを判別する
// ためで、「一貫性のため」に GetUserRoles 相当の effective 判定へ差し替えないこと
// — conditional の評価は user ごとに全 role を fetch して formula を回すので、この
// endpoint の存在理由である「(userId, roleId) の unique index を 1 回引くだけ」が
// 失われる。effective 判定が要るなら #2608 (プラグイン向け effective-policy
// provider) 側で扱う。
func (h *Handler) AssignmentShow(c echo.Context) error {
	var req struct {
		RoleID string `json:"roleId"`
	}
	if err := c.Bind(&req); err != nil || req.RoleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roleId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	viewerID := ""
	if viewer := middleware.GetUser(c); viewer != nil {
		viewerID = viewer.ID
	}
	// private roleの存在を未付与userへ明かさないため、self側はroleより先に
	// assignmentを確認する。admin側のrole-first lookupとは意図的に非対称。
	assignment, err := h.roleService.GetUserAssign(viewerID, req.RoleID, time.Now())
	if err != nil {
		return apierr.JSONInternalError(c)
	}

	r, err := h.roleService.Show(req.RoleID)
	if err != nil {
		if errors.Is(err, corerole.ErrRoleNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "4a3b21e8-9c54-4d7a-b1e2-8f6c9d0e5a71"))
		}
		return apierr.JSONInternalError(c)
	}

	assigned := assignment != nil
	// private かつ未付与なら role の存在ごと隠す (existence oracle 対策)。
	// **private な conditional role は条件を満たす viewer にも NO_SUCH_ROLE になる**
	// — assignment 行が無いためで、上の doc comment の制約がここで最も強く出る。
	// 秘匿を緩めて解くのは oracle を戻すので不可 (#2633)。
	if !r.IsPublic && !assigned {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "4a3b21e8-9c54-4d7a-b1e2-8f6c9d0e5a71"))
	}
	return c.JSON(http.StatusOK, entity.PackRoleAssignmentLookup(assignment, r))
}
