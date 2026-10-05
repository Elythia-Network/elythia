package admin

import (
	"errors"
	"net/http"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// RolesAssignmentShow handles POST /api/admin/roles/assignment-show.
//
// **見るのは role_assignment 行だけで、conditional role の condFormula は評価しない**
// (#2633)。conditional role は行を持たないので、条件を満たしている user にも
// assigned=false を返す。既存の admin/roles/users も ListByRole で同じテーブルを
// 引くので挙動は揃っている。呼び出し側が判別できるよう role.target を返す。
// effective 判定へ差し替えないこと (理由は roles.AssignmentShow の doc comment)。
func (h *Handler) RolesAssignmentShow(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
		RoleID string `json:"roleId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" || req.RoleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "userId and roleId are required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	// moderator向けread endpointなのでroleを先に確認する。private roleを未付与userへ
	// 隠すself側とは意図的に非対称で、編集gateはここへ適用しない。
	r, err := h.roleService.Show(req.RoleID)
	if err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "2c7a9e1f-6d84-4f3b-9a0c-5e2b8d1f7a34"))
		}
		return apierr.JSONInternalError(c)
	}
	if h.userRepo != nil {
		if _, err := h.userRepo.FindByID(req.UserID); err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "1b5c8d2e-3f4a-4b6c-8d9e-0f1a2b3c4d5e"))
			}
			return apierr.JSONInternalError(c)
		}
	}

	assignment, err := h.roleService.GetUserAssign(req.UserID, req.RoleID, time.Now())
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, entity.PackRoleAssignmentLookup(assignment, r))
}
