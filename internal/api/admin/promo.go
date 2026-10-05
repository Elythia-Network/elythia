package admin

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// PromoCreate handles POST /api/admin/promo/create.
func (h *Handler) PromoCreate(c echo.Context) error {
	if h.promoNoteRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req struct {
		NoteID string `json:"noteId"`
		// upstream promo/create.ts は required:['noteId','expiresAt']。欠落を 0 (=epoch)
		// で保存しないよう pointer で受け、nil を弾く (#1539)。
		ExpiresAt *int64 `json:"expiresAt"`
	}
	if err := c.Bind(&req); err != nil || req.NoteID == "" || req.ExpiresAt == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "noteId and expiresAt are required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	// noteFinder 未配線時は visibility 検証ができないため fail-closed で
	// promote を断る (#1466 / #1469 review)。production の router.go では
	// SetPromoNoteRepo の直後に SetNoteFinder が必ず呼ばれるため経路には
	// 到達しないが、設定ミス時に visibility check を黙って bypass しないよう
	// 兄弟 PR (#1467/#1468/#1460) と doctrine を揃える。
	if h.noteFinder == nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	// 対象 note の存在確認 → 公開範囲確認 → 既に promote 済みでないか確認
	note, err := h.noteFinder.FindByID(req.NoteID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を「そんなノートは無い」にしない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "ee449fbe-af2a-453b-9cae-cf2fe7c895fc"))
	}
	// 公開範囲が public 以外の note は promote できない (#1466)。upstream
	// Misskey TS の `admin/promo/create.ts` (pinned 2026.7.0) には visibility
	// check 自体が存在しないため、本実装は mk-go 独自の forward defense
	// (= 意図的な divergence) として加える。実害観点では upstream / mk-go
	// どちらも promo を表示する endpoint が未実装で latent な穴に留まるが、
	// 表示実装が将来入った瞬間に followers / specified / home note の本文が
	// 全 viewer に漏れる IDOR となるため create 段で先回りで reject する。
	// home は LTL/GTL に出ないため厳密な非公開ではないが、promo は全 viewer
	// 露出なので public のみ許可で揃える (upstream に明示根拠は無いが、
	// 漏洩面を最小化する保守側に倒す)。
	if note.Visibility != model.NoteVisibilityPublic {
		return apierr.JSONAccessDenied(c)
	}
	targetUserID := note.UserID

	exists, err := h.promoNoteRepo.Exists(req.NoteID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if exists {
		return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_PROMOTED", "The note has already promoted.", "ae427aa2-7a41-484f-a18c-2c1104051604"))
	}

	promo := &model.PromoNote{
		NoteID:    req.NoteID,
		ExpiresAt: time.UnixMilli(*req.ExpiresAt),
		UserID:    targetUserID,
	}
	if err := h.promoNoteRepo.Create(promo); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	return c.NoContent(http.StatusNoContent)
}
