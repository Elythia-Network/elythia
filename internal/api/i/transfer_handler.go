package i

import (
	"encoding/json"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/transfer"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/safemath"
	"github.com/shiroha-a/mk/internal/server/middleware"

	"github.com/shiroha-a/mk/internal/core/role"
)

// ImportFileReader reads a drive file's content synchronously. Satisfied by
// transfer.RepoBackedDriveReader。i/import-antennas が件数 enforce のために
// content を読むのに使う (#1667)。entity が core/transfer に依存しないよう
// narrow interface で受ける。
type ImportFileReader interface {
	Fetch(fileID string) (*model.DriveFile, []byte, error)
}

// AntennaCounter returns the number of antennas owned by a user. Satisfied by
// repository.AntennaRepository (#1667)。
type AntennaCounter interface {
	CountByUser(userID string) (int64, error)
}

// SetImportDriveReader wires the synchronous drive content reader used by
// i/import-antennas to count antennas before enqueue (#1667)。
func (h *Handler) SetImportDriveReader(r ImportFileReader) { h.importDriveReader = r }

// SetAntennaCounter wires the antenna counter used by i/import-antennas to
// enforce TOO_MANY_ANTENNAS (#1667)。
func (h *Handler) SetAntennaCounter(c AntennaCounter) { h.antennaCounter = c }

// TransferEnqueuer is the subset of queue.Enqueuer needed to schedule
// export/import jobs. 小さいインターフェースにすることで i/Handler のテスト
// mockが簡単になる。
type TransferEnqueuer interface {
	EnqueueExport(payload queue.ExportPayload) error
	EnqueueImport(payload queue.ImportPayload) error
}

// SetTransferEnqueuer attaches a TransferEnqueuer for i/export-* and
// i/import-* endpoints.
func (h *Handler) SetTransferEnqueuer(e TransferEnqueuer) {
	h.transferEnqueuer = e
}

// SetDriveFileRepo wires the DriveFileRepository used by i/import-* to verify
// that the supplied fileId is owned by the requesting user (#1555). Without
// this the import endpoints fail closed (NO_SUCH_FILE) to avoid reading another
// user's drive file.
func (h *Handler) SetDriveFileRepo(r repository.DriveFileRepository) {
	h.driveFileRepo = r
}

// exportHandler enqueues an export job for the authenticated user and
// returns 204 No Content on success, matching Misskey's behavior.
func (h *Handler) exportHandler(exportType string) echo.HandlerFunc {
	return func(c echo.Context) error {
		u := middleware.GetUser(c)
		if h.transferEnqueuer == nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Transfer queue not configured.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
		// export-following のみ excludeMuting / excludeInactive を受ける
		// (upstream export-following.ts paramDef)。他 type では無視される。
		var req struct {
			ExcludeMuting   bool `json:"excludeMuting"`
			ExcludeInactive bool `json:"excludeInactive"`
		}
		if err := c.Bind(&req); err != nil {
			return apierr.JSONInvalidParam(c)
		}
		if err := h.transferEnqueuer.EnqueueExport(queue.ExportPayload{
			UserID:          u.ID,
			Type:            exportType,
			ExcludeMuting:   req.ExcludeMuting,
			ExcludeInactive: req.ExcludeInactive,
		}); err != nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Failed to enqueue export.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
		return c.NoContent(http.StatusNoContent)
	}
}

// importNoSuchFileID maps an import type to upstream Misskey's per-endpoint
// noSuchFile error UUID. 各 i/import-* endpoint は固有の noSuchFile id を持つ
// ため、共通 importHandler でも importType ごとに本家 UUID を返す (#1555)。
func importNoSuchFileID(importType string) string {
	switch importType {
	case transfer.ImportBlocking:
		return "ebb53e5f-6574-9c0c-0b92-7ca6def56d7e"
	case transfer.ImportMuting:
		return "e674141e-bd2a-ba85-e616-aefb187c9c2a"
	case transfer.ImportUserLists:
		return "ea9cc34f-c415-4bc6-a6fe-28ac40357049"
	case transfer.ImportAntennas:
		return "3b71d086-c3fa-431c-b01d-ded65a777172"
	default: // ImportFollowing
		return "b98644cf-a5ac-4277-a502-0b8054a709a3"
	}
}

// importMaxFileSize is upstream の通常 import file 上限 (64KB)。
const importMaxFileSize = 64 * 1024

// importMovedMaxFileSize is the relaxed limit granted right after a confirmed
// account move (upstream import-* の 32MB)。移行直後は旧アカウントから出力した
// 一覧をまとめて取り込むため、通常の 64KB では足りない。
//
// 緩和には移行元との相互確認が要る (MoveInValidator 参照)。
const importMovedMaxFileSize = 32 * 1024 * 1024

// importEmptyFileID maps an import type to upstream's per-endpoint emptyFile
// error UUID (#1555)。
func importEmptyFileID(importType string) string {
	switch importType {
	case transfer.ImportBlocking:
		return "6f3a4dcc-f060-a707-4950-806fbdbe60d6"
	case transfer.ImportMuting:
		return "d2f12af1-e7b4-feac-86a3-519548f2728e"
	case transfer.ImportUserLists:
		return "99efe367-ce6e-4d44-93f8-5fae7b040356"
	case transfer.ImportAntennas:
		return "7f60115d-8d93-4b0f-bd0e-3815dcbb389f"
	default: // ImportFollowing
		return "31a1b42c-06f7-42ae-8a38-a661c5c9f691"
	}
}

// importTooBigFileID maps an import type to upstream's per-endpoint tooBigFile
// error UUID。import-antennas は upstream に tooBigFile check が無いため ""
// (= size 上限 check を skip)。
func importTooBigFileID(importType string) string {
	switch importType {
	case transfer.ImportBlocking:
		return "b7fbf0b1-aeef-3b21-29ef-fadd4cb72ccf"
	case transfer.ImportMuting:
		return "9b4ada6d-d7f7-0472-0713-4f558bd1ec9c"
	case transfer.ImportUserLists:
		return "ae6e7a22-971b-4b52-b2be-fc0b9b121fe9"
	case transfer.ImportAntennas:
		return "" // upstream import-antennas は tooBigFile を持たない
	default: // ImportFollowing
		return "dee9d4ed-ad07-43ed-8b34-b2856398bc60"
	}
}

// validateImportRequest validates the fileId param, file ownership, and size
// for an import-* endpoint. On any failure it writes the error response and
// returns ok=false (callers then `return nil`, mirroring requireWebAuthn)。
// 成功時は認証 user / 解決済 DriveFile / job-level withReplies を返す。
//
// upstream Misskey の import-* は driveFilesRepository.findOneBy({id, userId:
// me.id}) で所有 file のみ許可し、他人 / 不在 / system file (userId NULL) は
// NO_SUCH_FILE で弾く。所有権検証を欠くと cross-user file read が成立するため
// driveFileRepo 未配線時は fail-closed (#1555)。空 file は EMPTY_FILE、上限超過は
// TOO_BIG_FILE (import-antennas は upstream に tooBigFile が無いので skip)。
func (h *Handler) validateImportRequest(c echo.Context, importType string) (*model.User, *model.DriveFile, bool, bool) {
	u := middleware.GetUser(c)
	var req struct {
		FileID      string `json:"fileId"`
		WithReplies *bool  `json:"withReplies"`
	}
	if err := c.Bind(&req); err != nil || req.FileID == "" {
		_ = c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "fileId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
		return nil, nil, false, false
	}
	if h.driveFileRepo == nil {
		_ = c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "No such file.", importNoSuchFileID(importType)))
		return nil, nil, false, false
	}
	file, err := h.driveFileRepo.FindByID(req.FileID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。この関数は
		// (user, file, ok, ok) を返すので、応答は自分で書いて false を返す。
		_ = c.JSON(http.StatusInternalServerError, apierr.InternalError())
		return nil, nil, false, false
	}
	if err != nil || file == nil || file.UserID == nil || *file.UserID != u.ID {
		_ = c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "No such file.", importNoSuchFileID(importType)))
		return nil, nil, false, false
	}
	if file.Size == 0 {
		_ = c.JSON(http.StatusBadRequest, apierr.Error("EMPTY_FILE", "That file is empty.", importEmptyFileID(importType)))
		return nil, nil, false, false
	}
	if tooBigID := importTooBigFileID(importType); tooBigID != "" && file.Size > h.importSizeLimit(u) {
		_ = c.JSON(http.StatusBadRequest, apierr.Error("TOO_BIG_FILE", "That file is too big.", tooBigID))
		return nil, nil, false, false
	}
	withReplies := false
	if req.WithReplies != nil {
		withReplies = *req.WithReplies
	}
	return u, file, withReplies, true
}

// importHandler enqueues an import job using a DriveFile uploaded by the
// user. フロントは fileId を渡してくる。本家互換。
func (h *Handler) importHandler(importType string) echo.HandlerFunc {
	return func(c echo.Context) error {
		u, file, withReplies, ok := h.validateImportRequest(c, importType)
		if !ok {
			return nil
		}
		if h.transferEnqueuer == nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Transfer queue not configured.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
		// withReplies は import-following のみが使う job-level fallback (CSV row が
		// per-line withReplies を省略したときの default、upstream
		// `withReplies ?? job.data.withReplies`)。他 type では Importer 側で無視。
		if err := h.transferEnqueuer.EnqueueImport(queue.ImportPayload{
			UserID:      u.ID,
			Type:        importType,
			FileID:      file.ID,
			WithReplies: withReplies,
		}); err != nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Failed to enqueue import.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
		return c.NoContent(http.StatusNoContent)
	}
}

// antennaImportWouldExceedLimit reports whether importing the antennas in file
// would push the user at/over their antennaLimit, mirroring upstream
// import-antennas.ts (`currentAntennasCount + antennas.length >= antennaLimit`)。
// 依存 (driveReader / antennaCounter / roleProvider) 未配線、content download
// 失敗、JSON parse 失敗、count 失敗時はいずれも false を返し、limit check を
// skip して worker に委ねる (degrade)。所有権 / EMPTY は呼び出し前に検証済み。
func (h *Handler) antennaImportWouldExceedLimit(userID, fileID string) bool {
	if h.importDriveReader == nil || h.antennaCounter == nil || h.roleProvider == nil {
		return false
	}
	limit, ok := role.PolicyNumber(h.roleProvider.GetUserPolicies(userID)["antennaLimit"])
	if !ok || limit < 0 {
		return false
	}
	_, body, err := h.importDriveReader.Fetch(fileID)
	if err != nil {
		return false
	}
	// 件数のみ必要なので各 entry は raw のまま len を取る。
	var entries []json.RawMessage
	if json.Unmarshal(body, &entries) != nil {
		return false
	}
	current, err := h.antennaCounter.CountByUser(userID)
	if err != nil {
		return false
	}
	return float64(safemath.AddInt64(current, int64(len(entries)))) >= limit
}

// Export handler factory methods — one per endpoint for router binding.
func (h *Handler) ExportNotes(c echo.Context) error {
	return h.exportHandler(transfer.ExportNotes)(c)
}
func (h *Handler) ExportFollowing(c echo.Context) error {
	return h.exportHandler(transfer.ExportFollowing)(c)
}
func (h *Handler) ExportBlocking(c echo.Context) error {
	return h.exportHandler(transfer.ExportBlocking)(c)
}
func (h *Handler) ExportMute(c echo.Context) error {
	return h.exportHandler(transfer.ExportMuting)(c)
}
func (h *Handler) ExportFavorites(c echo.Context) error {
	return h.exportHandler(transfer.ExportFavorites)(c)
}
func (h *Handler) ExportUserLists(c echo.Context) error {
	return h.exportHandler(transfer.ExportUserLists)(c)
}
func (h *Handler) ExportAntennas(c echo.Context) error {
	return h.exportHandler(transfer.ExportAntennas)(c)
}
func (h *Handler) ExportClips(c echo.Context) error {
	return h.exportHandler(transfer.ExportClips)(c)
}

// Import handler factory methods.
func (h *Handler) ImportFollowing(c echo.Context) error {
	return h.importHandler(transfer.ImportFollowing)(c)
}
func (h *Handler) ImportBlocking(c echo.Context) error {
	return h.importHandler(transfer.ImportBlocking)(c)
}
func (h *Handler) ImportMuting(c echo.Context) error {
	return h.importHandler(transfer.ImportMuting)(c)
}
func (h *Handler) ImportUserLists(c echo.Context) error {
	return h.importHandler(transfer.ImportUserLists)(c)
}

// ImportAntennas は他の import-* と異なり、upstream import-antennas.ts に倣って
// enqueue 前に同期で TOO_MANY_ANTENNAS を enforce する (#1667)。upstream は
// file を download+parse して antenna 件数を数え、現件数 + import 件数 >=
// antennaLimit なら弾く。
//
// なお upstream の NO_SUCH_USER (usersRepository.exists 失敗) は防御的 check で、
// mk-go では RequireAuth が user を load 済みのため到達不能。dead code を避けて
// 実装しない (golden には id があるが emit しないのは drift gate 上問題ない)。
func (h *Handler) ImportAntennas(c echo.Context) error {
	u, file, _, ok := h.validateImportRequest(c, transfer.ImportAntennas)
	if !ok {
		return nil
	}
	if h.antennaImportWouldExceedLimit(u.ID, file.ID) {
		return c.JSON(http.StatusBadRequest, apierr.Error("TOO_MANY_ANTENNAS", "You cannot create antenna any more.", "600917d4-a4cb-4cc5-8ba8-7ac8ea3c7779"))
	}
	if h.transferEnqueuer == nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Transfer queue not configured.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	if err := h.transferEnqueuer.EnqueueImport(queue.ImportPayload{
		UserID: u.ID,
		Type:   transfer.ImportAntennas,
		FileID: file.ID,
	}); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Failed to enqueue import.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	return c.NoContent(http.StatusNoContent)
}

// importSizeLimit returns the bulk-import size limit for u, relaxed when they
// are the destination of a recently confirmed account move (#2415)。
//
// validator 未配線なら通常上限に倒す。緩和は「相互確認が取れた移行の直後」
// だけに与えるものなので、判定できないときは絞る側が安全。
func (h *Handler) importSizeLimit(u *model.User) int {
	if h.moveInValidator != nil && h.moveInValidator.HasRecentConfirmedMoveIn(u) {
		return importMovedMaxFileSize
	}
	return importMaxFileSize
}
