package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/emojiapplication"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/colfit"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/safehttp"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// emojiApplicationReviewer is the subset of the emoji application service the
// admin endpoints need.
type emojiApplicationReviewer interface {
	Approve(ctx context.Context, id, moderatorID string) (*model.EmojiApplication, error)
	Reject(ctx context.Context, id, moderatorID, reason string) (*model.EmojiApplication, error)
	// ResetQuota clears the user's rolling quota and returns the usage
	// *before* the reset plus the recorded row (#2962).
	//
	// **前の使用状況を返させるのが要点。** 監査ログに「何件使っていた人を
	// 戻したか」を残すためで、後から採ると必ず 0 になる。
	ResetQuota(userID, moderatorID, reason string) (emojiapplication.UserSummary, *model.EmojiApplicationQuotaReset, error)
	// UserSummary backs the moderation screen's per-user tab (#2961).
	//
	// **service 経由で採る。** 期間上限の窓はロールの policy から組むもので、
	// handler が repository を直に叩くと policy の解決を書き写すことになる
	// (作成側と数え方がずれる)。
	UserSummary(userID string) (emojiapplication.UserSummary, error)
}

// SetEmojiApplicationReviewer wires the review half of #2934.
func (h *Handler) SetEmojiApplicationReviewer(r emojiApplicationReviewer) {
	h.emojiApplicationReviewer = r
}

// SetEmojiApplicationRepo wires the read half of #2934.
func (h *Handler) SetEmojiApplicationRepo(r repository.EmojiApplicationRepository) {
	h.emojiApplicationRepo = r
}

// EmojiApplicationList handles POST /api/admin/emoji-application/list.
func (h *Handler) EmojiApplicationList(c echo.Context) error {
	if h.emojiApplicationRepo == nil {
		return apierr.JSONInternalError(c)
	}
	var req struct {
		Filter  string `json:"filter"`
		Limit   int    `json:"limit"`
		UntilID string `json:"untilId"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	if req.Limit <= 0 || req.Limit > 100 {
		req.Limit = 30
	}
	if req.Filter == "" {
		req.Filter = repository.EmojiApplicationFilterPending
	}

	// **列に入らないカーソルは 400 (#3025)。** `untilId` はそのまま
	// `"id" < ?` の bind parameter に載るので、NUL を含むとクエリごと落ちて
	// 500 になる。他のページングと同じく `id.NormalizeCursor` を通す。
	_, untilID, cursorOK := id.NormalizeCursor("", req.UntilID, nil, nil)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	rows, err := h.emojiApplicationRepo.List(req.Filter, req.Limit, untilID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	out := make([]map[string]any, 0, len(rows))
	for i := range rows {
		out = append(out, h.packEmojiApplicationForModerator(&rows[i]))
	}
	return c.JSON(http.StatusOK, out)
}

// EmojiApplicationApprove handles POST /api/admin/emoji-application/approve.
func (h *Handler) EmojiApplicationApprove(c echo.Context) error {
	if h.emojiApplicationReviewer == nil {
		return apierr.JSONInternalError(c)
	}
	me := middleware.GetUser(c)
	if me == nil {
		return apierr.JSONInternalError(c)
	}
	var req struct {
		ApplicationID string `json:"applicationId"`
	}
	if err := c.Bind(&req); err != nil || req.ApplicationID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"INVALID_PARAM", "applicationId is required.",
			"3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	app, err := h.emojiApplicationReviewer.Approve(c.Request().Context(), req.ApplicationID, me.ID)
	if err != nil {
		return emojiApplicationReviewError(c, err)
	}
	// **`emoji` キーを必ず入れる。** 管理画面の modlog は
	// `log.info.emoji.name` を無条件に読むので (upstream の
	// `modlog.ModLog.vue`)、無いと TypeError で **その 1 件が空行になって
	// 消える** — Vue 3.5 の renderComponentRoot が render 例外を握って
	// Comment ノードを返すため、コンソールにしか出ない。
	h.logModeration(c, moderationlog.LogAddCustomEmoji, map[string]any{
		"emojiId":                 derefString(app.EmojiID),
		"emoji":                   emojiApplicationLogEmoji(app),
		"emojiApplicationId":      app.ID,
		"emojiApplicationUserId":  app.UserID,
		"emojiApplicationDecided": "approved",
	})
	return c.JSON(http.StatusOK, h.packEmojiApplicationForModerator(app))
}

// EmojiApplicationReject handles POST /api/admin/emoji-application/reject.
func (h *Handler) EmojiApplicationReject(c echo.Context) error {
	if h.emojiApplicationReviewer == nil {
		return apierr.JSONInternalError(c)
	}
	me := middleware.GetUser(c)
	if me == nil {
		return apierr.JSONInternalError(c)
	}
	var req struct {
		ApplicationID string `json:"applicationId"`
		Reason        string `json:"reason"`
	}
	if err := c.Bind(&req); err != nil || req.ApplicationID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"INVALID_PARAM", "applicationId is required.",
			"3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	app, err := h.emojiApplicationReviewer.Reject(c.Request().Context(), req.ApplicationID, me.ID, req.Reason)
	if err != nil {
		return emojiApplicationReviewError(c, err)
	}
	// **却下も監査に残す。** 何を通したかだけでなく、何を落としたかが残らないと
	// 「モデレーターが恣意的に落としている」という疑いに答える材料が無くなる。
	//
	// **type は addCustomEmoji のまま。** modlog UI はこの type を「追加」として
	// 緑 + ti-plus で描くので、却下ログも「追加」に見える (レビュー Low 3)。
	// 専用の type を足すには upstream の enum と UI 両方に手を入れることになり、
	// 追従のたびに衝突する。`emojiApplicationDecided` を info に入れてあるので
	// 区別は付く。専用 type は #2935 と合わせて検討する。
	h.logModeration(c, moderationlog.LogAddCustomEmoji, map[string]any{
		"emoji":                   emojiApplicationLogEmoji(app),
		"emojiApplicationId":      app.ID,
		"emojiApplicationUserId":  app.UserID,
		"emojiApplicationDecided": "rejected",
	})
	return c.JSON(http.StatusOK, h.packEmojiApplicationForModerator(app))
}

// emojiApplicationReviewError maps service errors onto the wire.
func emojiApplicationReviewError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, emojiapplication.ErrNotFound):
		return c.JSON(http.StatusNotFound, apierr.Error(
			"NO_SUCH_APPLICATION", "No such application.",
			"2b7e0c94-6d1a-4f3e-b5c8-0a9d3e7f1b44"))
	case errors.Is(err, emojiapplication.ErrNotPending):
		// **同時に 2 人が押した場合もここに来る。** 「もう処理済み」と返して
		// 一覧を引き直させるほうが、500 を出して何が起きたか分からないより良い。
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"ALREADY_PROCESSED", "This request was already processed.",
			"4e6f1a37-9c2b-4d80-a1f5-7b3c8e0d2a55"))
	case errors.Is(err, emojiapplication.ErrImageURLTooLong):
		// **保存先の URL が `emoji` の列に入らない (#3023)。** 直せるのは保存先の
		// 設定だけ (URL の長さは `base (+ prefix) + accessKey` で決まるので、
		// 申請者が別の画像を出し直しても同じ長さになる) だが、**操作者の入力で
		// 起きた失敗ではない**ので 500 にはしない。承認できないあいだの出口は却下。
		// `admin/emoji/*` と同じ code / id / message を返す。
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM",
			"The drive file URL is too long to store on an emoji (max 512 characters). Its length is determined by the file storage base URL and prefix.",
			"3d81ceae-475f-4600-b2a8-2bc116157532"))
	case errors.Is(err, emojiapplication.ErrTooLong):
		// **却下理由が列に入らない場合 (#3022)。** 無いと 500 に落ちる。
		// **申請側と同じ code / id を返す** — 同じ理由で断っているので、
		// 経路ごとに分岐を持たせない。
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"TOO_LONG", "A field is too long or contains an invalid character.",
			"8c5e2f60-1a4d-4b93-8e77-3d0f6a2b9c66"))
	case errors.Is(err, emojiapplication.ErrDuplicateName):
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"DUPLICATE_NAME", "Duplicate name.",
			"f7a3462c-4e6e-4069-8421-b9bd4f4c3975"))
	case errors.Is(err, emojiapplication.ErrUnsupportedFileType):
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"UNSUPPORTED_FILE_TYPE", "Unsupported file type.",
			"f7599d96-8750-af68-1633-9575d625c1a7"))
	case errors.Is(err, emojiapplication.ErrImageTooLarge):
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"EMOJI_IMAGE_TOO_LARGE", "The image is too large to register as an emoji.",
			"6b1d5f0a-3c9e-4f27-9a4d-7e2b8c1f0d64"))
	case errors.Is(err, emojiapplication.ErrImageCopyFailed):
		return c.JSON(http.StatusInternalServerError, apierr.Error(
			"INTERNAL_ERROR", "Failed to copy emoji image.",
			"c2f7a3d1-58be-4e09-bb26-0d4a9e7f3c15"))
	case errors.Is(err, emojiapplication.ErrRemoteFetchFailed):
		return c.JSON(http.StatusInternalServerError, apierr.Error(
			"INTERNAL_ERROR", "Failed to fetch emoji image.",
			"0a4e0b9e-2d7c-4d6f-8f6b-1f9c2e9b4d83"))
	case errors.Is(err, emojiapplication.ErrNoSuchRemoteEmoji):
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"NO_SUCH_EMOJI", "The remote emoji is no longer known to this instance.",
			"e2785b66-dca3-4087-9cac-b93c541cc425"))
	case errors.Is(err, emojiapplication.ErrFileGone):
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"NO_SUCH_FILE", "No such file.",
			"fc46b5a4-6b92-4c33-ac66-b806659bb5cf"))
	default:
		return apierr.JSONInternalError(c)
	}
}

// packEmojiApplicationForModerator renders a row for the review screen.
//
// 申請者向け (internal/api/emojiapplications) より項目が多い。審査に要る情報を
// 出し切るのが目的で、**同名の既存絵文字があるかどうかもここで解決する** —
// 承認を押してから DUPLICATE_NAME で落ちると、押した側にも申請者にも何も残らない。
func (h *Handler) packEmojiApplicationForModerator(app *model.EmojiApplication) map[string]any {
	out := map[string]any{
		"id":          app.ID,
		"userId":      app.UserID,
		"kind":        app.Kind,
		"status":      app.Status,
		"name":        app.Name,
		"category":    app.Category,
		"aliases":     []string(app.Aliases),
		"license":     app.License,
		"isSensitive": app.IsSensitive,
		"comment":     app.Comment,
		"createdAt":   entity.ISOMillis(app.CreatedAt),
		"processedAt": entity.ISOMillisPtr(app.ProcessedAt),
	}
	if app.RejectReason != nil {
		out["rejectReason"] = *app.RejectReason
	}
	if app.EmojiID != nil {
		out["emojiId"] = *app.EmojiID
	}
	// **drive は 1 回だけ引く (レビュー M6)。** url と fileType で別々に呼ぶと
	// 1 行あたり 2 回になり、limit=100 の一覧で 200 クエリが直列に走る。
	//
	// **「消えた」と「確認できなかった」を区別する (レビュー M1)。** DB 障害を
	// 「画像がありません」と表示すると、モデレーターが実際には存在する申請を
	// 却下しうる。nameConflict と同じく null で区別できるようにする。
	// **リモートも画像を出す (レビュー H1)。** 出さないと、モデレーターは
	// `:name:@host` の文字列だけを見て承認を押すことになる — #2934 が「大きな
	// 升目だけで判断すると本文で潰れて読めないものを通してしまう」として申請側と
	// 審査側の両方にプレビューを置いた設計の、リモート側だけが抜ける。
	// 名前だけ穏当にして中身が問題のある絵文字を通す審査回避にも直結する。
	if app.RemoteHost != nil && app.RemoteName != nil {
		out["remoteHost"] = *app.RemoteHost
		out["remoteName"] = *app.RemoteName
		// **処理済みの行でも引く (レビュー Low 3)。** 下の nameConflict は
		// `IsPending()` で絞っているが、こちらは絞らない — 画像は履歴を見る
		// ときにも要るもので、絞ると「承認済みのリモート絵文字だけ画像が
		// 出ない」という own 側との非対称が生まれる。件数は frontend が
		// limit 50 なので最大 100 クエリで、本番実測は 25-35ms (どちらも
		// index scan)。
		src, url, ft, lookupOK := h.remoteEmojiPreview(app)
		out["url"] = url
		out["fileType"] = ft
		// **承認できるかどうかもここで分かるようにする。** 審査を待つ間に
		// リモート絵文字の行が消えると承認は NO_SUCH_EMOJI で落ちる。
		//
		// **DB 障害を「消えた」に丸めない (レビュー R2-H2 / #2792)。** 丸めると
		// 障害中にモデレーターが「もう無い」と言われて**承認を操作ごと止められる**。
		// nil = 確認できなかった / false = ある / true = 消えた の 3 値にする。
		switch {
		case !lookupOK:
			out["remoteGone"] = nil
		default:
			out["remoteGone"] = src == nil
		}
		// **早期 return しない (レビュー R2-H1)。** 下の nameConflict に到達
		// しなくなり、リモートでだけ重複の警告が出なくなる — 承認を押してから
		// DUPLICATE_NAME で落ちるという、#2934 が塞いだ状態に戻る。
	} else {
		f, lookupOK := h.emojiApplicationFile(app)
		switch {
		case f != nil:
			out["url"] = f.URL
			out["fileType"] = f.Type
		case lookupOK:
			// 引けたが行が無い = 申請者が drive から消した。
			out["url"] = ""
			out["fileType"] = ""
		default:
			// 引けなかった。null で「分からない」を表す。
			out["url"] = nil
			out["fileType"] = nil
		}
	}

	// 審査中のものだけ重複を見る。処理済みの行で引いても意味が無く、
	// 一覧のたびに件数分のクエリを足すだけになる。
	if app.IsPending() && h.emojiRepo != nil {
		existing, err := h.emojiRepo.FindByNameAndHost(app.Name, nil)
		// **DB 障害を「重複なし」に丸めない** (#2792)。判定できなかったことを
		// そのまま出し、モデレーターに「確認できていない」と分かるようにする。
		switch {
		case err != nil && !repository.IsNotFound(err):
			out["nameConflict"] = nil
		case err == nil && existing != nil:
			out["nameConflict"] = map[string]any{
				"emojiId":   existing.ID,
				"createdAt": entity.ISOMillisPtr(existing.UpdatedAt),
			}
		default:
			out["nameConflict"] = false
		}
	}
	return out
}

// remoteEmojiPreview resolves the source emoji so the review screen can show it.
//
// `checkRemoteEmoji` と同じ lookup。**DB 障害と「消えた」を区別する** — 4 つ目の
// 戻り値が false なら「引けなかった」で、呼び出し側は null を出して
// 「分からない」と伝える。true かつ src が nil なら「もう無い」。
func (h *Handler) remoteEmojiPreview(app *model.EmojiApplication) (*model.Emoji, any, any, bool) {
	if h.emojiRepo == nil {
		return nil, nil, nil, false
	}
	src, err := h.emojiRepo.FindByNameAndHost(*app.RemoteName, app.RemoteHost)
	if err != nil {
		if repository.IsNotFound(err) {
			return nil, "", "", true
		}
		slog.Warn("emoji application: remote emoji lookup failed",
			"applicationId", app.ID, "host", *app.RemoteHost, "name", *app.RemoteName, "err", err)
		return nil, nil, nil, false
	}
	if src == nil {
		return nil, "", "", true
	}
	// publicUrl が空なら originalUrl に落とす (entity の packSimple と同じ)。
	url := src.PublicURL
	if url == "" {
		url = src.OriginalURL
	}
	var ft any = ""
	if src.Type != nil {
		ft = *src.Type
	}
	return src, url, ft, true
}

// emojiApplicationFile resolves the drive file for an application.
//
// 返り値の 2 つ目は「引けたかどうか」。**DB 障害を「画像が無い」に丸めない
// (#2792 の型)。** 丸めると障害中に審査画面が「画像がありません」と表示し、
// モデレーターが実際には存在する申請を却下しうる。
//
//	(f, true)   — 見つかった
//	(nil, true) — 引けたが行が無い (申請者が drive から消した)
//	(nil, false) — 引けなかった (障害)。呼び出し側は null で「分からない」を出す
func (h *Handler) emojiApplicationFile(app *model.EmojiApplication) (*model.DriveFile, bool) {
	if app.FileID == nil || h.driveFileRepo == nil {
		return nil, true
	}
	f, err := h.driveFileRepo.FindByID(*app.FileID)
	if err != nil {
		if repository.IsNotFound(err) {
			return nil, true
		}
		slog.Warn("emoji application: drive file lookup failed",
			"applicationId", app.ID, "fileId", *app.FileID, "err", err)
		return nil, false
	}
	return f, true
}

// CreateFromApplication implements emojiapplication.EmojiCreator.
//
// **EmojiAdd と同じ検証・同じ導出を通す。** 独自に emoji 行を組むと、MIME の
// allowlist や webpublic variant の優先が申請経路だけ抜け、承認が「検証を
// 迂回して絵文字を登録する方法」になる。共有しているのは
// isAllowedEmojiImageType / preferWebpublicURL / preferWebpublicType の 3 つで、
// これは EmojiAdd がそのまま使っているものと同一。
func (h *Handler) CreateFromApplication(ctx context.Context, app *model.EmojiApplication) (emojiapplication.CreatedEmoji, error) {
	if h.emojiRepo == nil || h.idGen == nil {
		return emojiapplication.CreatedEmoji{}, errors.New("admin: emoji creation is not wired")
	}
	if app.Kind == model.EmojiApplicationKindRemote {
		return h.createFromRemoteApplication(ctx, app)
	}
	if h.driveFileRepo == nil {
		return emojiapplication.CreatedEmoji{}, errors.New("admin: emoji creation is not wired")
	}
	if app.FileID == nil {
		return emojiapplication.CreatedEmoji{}, emojiapplication.ErrFileGone
	}

	f, err := h.driveFileRepo.FindByID(*app.FileID)
	if err != nil {
		if repository.IsNotFound(err) {
			// **申請から承認までの間にファイルが消えることがある。** 利用者が
			// drive から消せるので、500 ではなく「もう無い」と伝える。
			return emojiapplication.CreatedEmoji{}, emojiapplication.ErrFileGone
		}
		return emojiapplication.CreatedEmoji{}, err
	}
	// **承認側でも所有者を見る (レビュー Low 3)。** 申請時にも見ているが、
	// 検証が入る前に作られた行が残っている可能性があり、多層にしておけば
	// 「申請時の検証を落としたら承認が素通りする」形にもならない。
	if f.UserID == nil || *f.UserID != app.UserID {
		return emojiapplication.CreatedEmoji{}, emojiapplication.ErrFileGone
	}
	if !isAllowedEmojiImageType(f.Type) {
		return emojiapplication.CreatedEmoji{}, emojiapplication.ErrUnsupportedFileType
	}

	// 承認の直前にもう一度だけ重複を見る。申請の受付時にも見ているが、審査を
	// 待つ間に同じ名前が登録されうる。
	existing, dupErr := h.emojiRepo.FindByNameAndHost(app.Name, nil)
	if dupErr != nil && !repository.IsNotFound(dupErr) {
		return emojiapplication.CreatedEmoji{}, dupErr
	}
	if dupErr == nil && existing != nil {
		return emojiapplication.CreatedEmoji{}, emojiapplication.ErrDuplicateName
	}

	// **申請者のファイルを参照し続けない (#2966)。** 承認した絵文字が
	// 申請者所有の drive ファイルの URL を指したままだと、申請者がそれを
	// 消した時点で / アカウントを消した時点で表示が壊れる。リモート経路は
	// 既に system 所有で取り込んでおり、同じ機能で保存期間が非対称だった。
	//
	// **元のファイルは触らない。** 所有権を移すと、ノートの添付やプロフィールで
	// 使っているファイルが利用者の drive から突然消える。
	src := f
	systemFileID := ""
	if h.emojiImageFetcher != nil {
		copied, cerr := h.emojiImageFetcher.CopyToSystemFile(ctx, f, app.Name, app.IsSensitive)
		if cerr != nil {
			slog.WarnContext(ctx, "emoji application: drive copy failed",
				"applicationId", app.ID, "fileId", *app.FileID, "err", cerr)
			// **絵文字を作らない。** ここで元ファイルを参照して作ると、直そうと
			// しているバグをそのまま残すことになる。申請は pending のまま。
			//
			// **失敗の種類を潰さない (レビュー H2)。** すべて `ErrFileGone` に
			// すると 400「そんなファイルは無い」になり、ストレージや DB の障害が
			// **client error に化けて監視でも 5xx が立たない** (#2792)。しかも
			// モデレーターには却下すべき申請に見える。
			if errors.Is(cerr, safehttp.ErrResponseTooLarge) {
				return emojiapplication.CreatedEmoji{}, emojiapplication.ErrImageTooLarge
			}
			// **逆に潰しすぎない (2 周目レビュー M1)。** #2792 が禁じているのは
			// 「障害を not-found に丸める」ことで、その逆ではない。実体が本当に
			// 無い場合 (`storedInternal` の行だけ残ってローカルの実体を失った /
			// S3 の lifecycle で object だけ消えた / `isLink` の行) は**却下が
			// 正しい操作**なのに、500 を返すと運用側に直しようがなく理由も出ない。
			if errors.Is(cerr, drive.ErrObjectNotFound) {
				return emojiapplication.CreatedEmoji{}, emojiapplication.ErrFileGone
			}
			return emojiapplication.CreatedEmoji{}, emojiapplication.ErrImageCopyFailed
		}
		// **複製した実体の MIME を見る (レビュー M5)。** `Upload` は
		// `AnalyseFile` でバイト列から型を引き直すので、元の行の宣言と実体が
		// ずれていると allowlist 外の型が絵文字として登録される (TS 製 DB からの
		// drop-in や実体破損で起きうる)。remote 経路が同じ理由で取り込み後に
		// 見ているのに、own だけ無検査なのは非対称。
		if !isAllowedEmojiImageType(copied.Type) {
			h.deleteSystemEmojiFile(ctx, copied.ID)
			return emojiapplication.CreatedEmoji{}, emojiapplication.ErrUnsupportedFileType
		}
		src = copied
		systemFileID = copied.ID
	}
	// **URL が列に入るかを見る (#3023)。** 複製の URL は保存先が決めるので、
	// 作ってからでないと分からない。**弾いたら片付ける** (MIME の再検査と同じ形)。
	if !emojiFileURLFits(src) {
		h.deleteSystemEmojiFile(ctx, systemFileID)
		return emojiapplication.CreatedEmoji{}, emojiapplication.ErrImageURLTooLong
	}

	now := time.Now()
	e := &model.Emoji{
		ID:        h.idGen.Generate(now),
		UpdatedAt: &now,
		Name:      app.Name,
		// **`originalUrl` は複製した実体の `url` と一致させる。** drive の孤児
		// cleanup は `emoji.originalUrl = drive_file.url` または
		// `publicUrl = url` を参照保護の条件にしているので、webpublic だけを
		// 入れると保護が外れて消される。
		OriginalURL: src.URL,
		PublicURL:   preferWebpublicURL(src),
		Type:        preferWebpublicType(src),
		Category:    app.Category,
		Aliases:     model.StringArray(app.Aliases),
		License:     &app.License,
		IsSensitive: app.IsSensitive,
	}
	if err := h.emojiRepo.Create(e); err != nil {
		// **作った system ファイルを片付ける (#2966)。** 残すと誰からも
		// 参照されない孤児になる。消せなくてもログに残して cleanup に拾わせる。
		//
		// **ただし載ったかを読み直してから (#3019)。** INSERT が commit 済みで
		// ack だけ失われた場合に消すと、絵文字はあるのに画像が無い状態になり、
		// 申請は pending のままなので**もう一度承認を押しても `DUPLICATE_NAME`
		// で通らない** = 恒久的に壊れる。
		//
		// **`UpdateIfPending` の失敗 (`approvalLanded`) とは逆に倒している。**
		// 理由は消せる範囲の違い — あちらは絵文字の行ごと消せるので押し直しで
		// 作り直せるが、ここで決められるのは複製の行方だけ。
		//
		// **載っていた場合、申請は pending のまま押し直せない。** `Create` が
		// 失敗した時点で `Approve` は return するので申請は pending のまま残り、
		// そのまま押し直すと `DUPLICATE_NAME` になる。**復旧はできる** — 審査画面は
		// 衝突している絵文字の id を出す (`packEmojiApplicationForModerator` の
		// `nameConflict`) ので、それを消せば承認を押し直せる。却下に倒すと申請者の
		// 枠を消費したまま閉じることになる。
		//
		// **申請 ID をログに残す。** 500 だけでは、どの申請が手当てを要するのか
		// 運用側から分からない。**載っていない失敗 (押し直せば通る) でも出る** —
		// どちらかはこの時点では未確定で、載っていた場合だけ
		// `cleanupUnreferencedEmojiCopy` が別の warn を出す。
		slog.WarnContext(ctx, "emojiapplication: emoji write failed during approval",
			"applicationId", app.ID, "emojiId", e.ID, "systemFileId", systemFileID, "err", err)
		h.cleanupUnreferencedEmojiCopy(ctx, e.ID, systemFileID, e.OriginalURL)
		return emojiapplication.CreatedEmoji{}, err
	}
	// **EmojiAdd と同じく broadcast する (レビュー M3)。** これが無いと、承認した
	// 絵文字が全クライアントのリロードまで絵文字ピッカーに出てこない
	// (`boot/main-boot.ts` が emojiAdded を受けて addCustomEmoji する)。
	h.publishEmojiAdded(e)
	return emojiapplication.CreatedEmoji{EmojiID: e.ID, DriveFileID: systemFileID}, nil
}

// deleteSystemEmojiFile removes a system-owned drive file created during an
// emoji registration that then failed.
//
// 申請の承認 (#2966) だけでなく、`admin/emoji/copy` (#2998) /
// `admin/emoji/add` (#2999) / `admin/emoji/update` (#3014) も同じ後始末を通る。
//
// **握り潰してログに残す。** 後始末に失敗しても審査の結果は変わらないので、
// 呼び出し元のエラーを上書きしない。残ったものは drive の孤児 cleanup から
// 見つけられる状態にしておく。
func (h *Handler) deleteSystemEmojiFile(ctx context.Context, fileID string) {
	if fileID == "" || h.emojiImageFetcher == nil {
		return
	}
	if err := h.emojiImageFetcher.DeleteSystemFile(ctx, fileID); err != nil {
		slog.WarnContext(ctx, "emoji: system drive file cleanup failed",
			"driveFileId", fileID, "err", err)
	}
}

// emojiApplicationLogEmoji renders the emoji-ish payload the moderation log UI
// expects.
//
// 却下では emoji 行が存在しないので、申請の内容から同じ形を組む。**空にしない** —
// modlog UI が `log.info.emoji.name` を無条件に読むので、無いとその行が消える。
func emojiApplicationLogEmoji(app *model.EmojiApplication) map[string]any {
	return map[string]any{
		"id":       derefString(app.EmojiID),
		"name":     app.Name,
		"category": app.Category,
		"aliases":  []string(app.Aliases),
		"license":  app.License,
	}
}

// createFromRemoteApplication imports a remote emoji for an approved request.
//
// **`admin/emoji/copy` と同じ経路を通す (#2935)。** 独自に emoji を組むと、
// リモート由来の文字列を列に収める規則 (`colfit`、#2726) と drive への取り込み
// (#670 / #722) が申請経路だけ抜ける。前者が抜けると相手サーバーが決めた長い
// 文字列で SQLSTATE 22001、後者が抜けると相手が画像を消した瞬間に表示が壊れる。
//
// **承認の時点で引き直す。** 申請は host + name で持っており emoji の行 ID では
// ない — リモート絵文字の行はキャッシュに近く、審査を待つ間に消えうる。
func (h *Handler) createFromRemoteApplication(ctx context.Context, app *model.EmojiApplication) (emojiapplication.CreatedEmoji, error) {
	if app.RemoteHost == nil || app.RemoteName == nil {
		return emojiapplication.CreatedEmoji{}, emojiapplication.ErrNoSuchRemoteEmoji
	}
	src, err := h.emojiRepo.FindByNameAndHost(*app.RemoteName, app.RemoteHost)
	if err != nil {
		if repository.IsNotFound(err) {
			// 審査を待つ間に消えた。500 ではなく「もう無い」と伝える。
			return emojiapplication.CreatedEmoji{}, emojiapplication.ErrNoSuchRemoteEmoji
		}
		return emojiapplication.CreatedEmoji{}, err
	}
	if src == nil {
		return emojiapplication.CreatedEmoji{}, emojiapplication.ErrNoSuchRemoteEmoji
	}

	// 承認の直前にもう一度だけ重複を見る (own と同じ)。
	existing, dupErr := h.emojiRepo.FindByNameAndHost(app.Name, nil)
	if dupErr != nil && !repository.IsNotFound(dupErr) {
		return emojiapplication.CreatedEmoji{}, dupErr
	}
	if dupErr == nil && existing != nil {
		return emojiapplication.CreatedEmoji{}, emojiapplication.ErrDuplicateName
	}

	now := time.Now()
	copied := *src
	copied.ID = h.idGen.Generate(now)
	// **`updatedAt` は今にする。** `EmojiCopy` は src の値を引き継ぐが、
	// それだと「相手サーバーで最後に更新された時刻」がローカル絵文字の
	// 更新時刻として並ぶ。`EmojiAdd` は now を入れており、そちらが正しい。
	// (`EmojiCopy` 側も揃えるべきだが、この PR の範囲外。)
	copied.UpdatedAt = &now
	copied.Host = nil
	copied.Name = app.Name
	// **`URI` は引き継ぐ。** `EmojiCopy` と同じ挙動で、renderer が
	// `emoji.URI != nil` のとき AP の `tag.ID` にそれを使う — remote emoji の
	// 参照同一性を壊さないための保守的方針 (#1948-11)。ここだけ変えると
	// 同じ操作で結果が変わるので踏襲する (本番に該当が 11 件ある)。

	// **申請で指定された値を列に収めてから入れる (#2726 と同じ規則)。**
	// 出どころは相手サーバーなので、そのまま渡すと列を超えて 500 になる。
	// 本文は切り、alias は長すぎる要素だけ落とす (切ると別の名前になる)。
	if app.Category != nil {
		v := colfit.Text(*app.Category, emojiCategoryMaxRunes)
		copied.Category = &v
	}
	if len(app.Aliases) > 0 {
		copied.Aliases = model.StringArray(normalizeEmojiAliases(app.Aliases))
	}
	// **申請者が書いたときだけ上書きする (レビュー M1)。** リモート絵文字の
	// 43% は AP の `_misskey_license` 由来の**本物の license** を持っている
	// (#731)。申請者は `fetch-remote-meta` を叩けない (moderator 専用) ので、
	// 参照できない値を手で書かせて潰すことになる。`EmojiCopy` も
	// `req.License != nil` のときだけ上書きしている。
	if app.License != "" {
		license := colfit.Text(app.License, emojiLicenseMaxRunes)
		copied.License = &license
	}
	// isSensitive も同じ理由で、指定があったときだけ上書きする。
	if app.IsSensitive {
		copied.IsSensitive = true
	}

	// **drive へ取り込む (#670 / #722)。** URL を引き継ぐだけだと、相手が
	// 画像を消した瞬間に表示が壊れる。system 所有で作るのは、操作者個人の
	// drive に紐付けるとロール変更や削除で巻き込まれるため。
	systemFileID := ""
	if h.emojiImageFetcher != nil && src.OriginalURL != "" {
		df, ferr := h.emojiImageFetcher.FetchAndStore(ctx, src.OriginalURL, nil, src.Name)
		if ferr != nil {
			slog.WarnContext(ctx, "emoji application: drive fetch failed",
				"applicationId", app.ID, "url", src.OriginalURL, "err", ferr)
			// **取得できなかったことを伝える (レビュー Low 2)。** 生の err を
			// 返すと汎用 500 になり、審査画面では「何か問題が」としか出ない。
			return emojiapplication.CreatedEmoji{}, emojiapplication.ErrRemoteFetchFailed
		}
		systemFileID = df.ID
		// **取り込んだものの MIME を見る (レビュー M7)。** 相手が icon.url に
		// 非画像を置くと、承認でそれが絵文字として登録される。own 経路は
		// 申請時に見ているので、remote だけ無検査なのは非対称。
		// (`EmojiCopy` にも同じ穴があったが #2998 で塞いだ。)
		if !isAllowedEmojiImageType(df.Type) {
			// **取り込んだものを片付ける (#2966)。** 弾いた時点で誰からも
			// 参照されないので、残すと孤児になる。
			h.deleteSystemEmojiFile(ctx, systemFileID)
			return emojiapplication.CreatedEmoji{}, emojiapplication.ErrUnsupportedFileType
		}
		// **URL が列に入るかを見る (#3023)。**
		//
		// **ここだけ取り込みブロックの内側にある。** own 経路 (上) と
		// `admin/emoji/*` はブロックの外に置いてあるが、こちらは取り込まなかった
		// ときのフォールバックが `src` = **`emoji` 行の値**で、その `originalUrl` /
		// `publicUrl` は同じ varchar(512) なので入ることが保証されている
		// (`copied` の初期化を参照)。**揃えるつもりで外へ出さないこと** — 出すと
		// fetcher 未配線の構成で毎回この検査を通ることになり、意味は変わらないが
		// 「入るはずのものを見ている」分かりにくさだけが残る。
		if !emojiFileURLFits(df) {
			h.deleteSystemEmojiFile(ctx, systemFileID)
			return emojiapplication.CreatedEmoji{}, emojiapplication.ErrImageURLTooLong
		}
		copied.OriginalURL = df.URL
		copied.PublicURL = preferWebpublicURL(df)
		// **nil ガードは要らない。** 手前の isAllowedEmojiImageType が
		// `df.Type` の空を弾くので、preferWebpublicType が nil を返す経路に
		// 入らない (レビュー Low 1 の指摘はこの順序を見落としていた。
		// ガードを入れても到達しないデッドコードになる)。
		copied.Type = preferWebpublicType(df)
	}

	if err := h.emojiRepo.Create(&copied); err != nil {
		// **載ったかを読み直してから片付ける (#3019)。** own 経路と同じ窓で、
		// 申請が pending のまま詰むところまで同じ。
		slog.WarnContext(ctx, "emojiapplication: emoji write failed during approval",
			"applicationId", app.ID, "emojiId", copied.ID, "systemFileId", systemFileID, "err", err)
		h.cleanupUnreferencedEmojiCopy(ctx, copied.ID, systemFileID, copied.OriginalURL)
		return emojiapplication.CreatedEmoji{}, err
	}
	h.publishEmojiAdded(&copied)
	return emojiapplication.CreatedEmoji{EmojiID: copied.ID, DriveFileID: systemFileID}, nil
}

// DeleteCreatedEmoji implements emojiapplication.EmojiCreator.
//
// 承認が競合に負けたときだけ呼ばれる。**broadcast も出す** — 作成時に
// emojiAdded を流しているので、消したことも伝えないとピッカーに残る。
func (h *Handler) DeleteCreatedEmoji(ctx context.Context, created emojiapplication.CreatedEmoji) error {
	// **絵文字が消せなかったら複製も残す (2 周目レビュー L4)。** 順序は絵文字が
	// 先 — 先にファイルを消すと、絵文字だけ残って画像が出ない窓が開く。同じ
	// 理由で、絵文字の削除に失敗したときにファイルだけ消すと、その状態が
	// **窓ではなく恒久状態**になる: 申請は pending、名前は使用中なので再承認は
	// `DUPLICATE_NAME`、画像は 404。
	//
	// 残した複製が孤児になることはない — 参照している絵文字が消えるまでは
	// `orphanWhere` の保護条件に当たり、消えれば cleanup が回収する。
	if err := h.deleteCreatedEmojiRow(created.EmojiID); err != nil {
		return err
	}
	h.deleteSystemEmojiFile(ctx, created.DriveFileID)
	return nil
}

func (h *Handler) deleteCreatedEmojiRow(emojiID string) error {
	if h.emojiRepo == nil || emojiID == "" {
		return nil
	}
	e, err := h.emojiRepo.FindByID(emojiID)
	if err != nil {
		if repository.IsNotFound(err) {
			return nil
		}
		return err
	}
	if err := h.emojiRepo.Delete(emojiID); err != nil {
		return err
	}
	h.publishEmojiDeleted(e)
	return nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// EmojiApplicationRelated handles POST /api/admin/emoji-application/related (#2960).
//
// **一覧には埋め込まない。** 全行ぶん履歴を引くと N+1 になるので、申請の詳細を
// 開いたときだけ呼ぶ。
//
// **過去に却下されていたことを理由に自動拒否はしない。** ライセンスの変更・
// 画像の修正・運用方針の変更がありうるので、これは審査を補助する情報。
func (h *Handler) EmojiApplicationRelated(c echo.Context) error {
	if h.emojiApplicationRepo == nil {
		return apierr.JSONInternalError(c)
	}
	var req struct {
		ApplicationID string `json:"applicationId"`
		Limit         int    `json:"limit"`
		UntilID       string `json:"untilId"`
	}
	if err := c.Bind(&req); err != nil || req.ApplicationID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"INVALID_PARAM", "applicationId is required.", apierr.UUIDInvalidParam))
	}
	if req.Limit <= 0 || req.Limit > 100 {
		req.Limit = 10
	}

	// **列に入らない id は引く前に弾く (#3022)。** ここはサービス層を通らず
	// リポジトリを直叩きするので、`cancel` / `approve` / `reject` の guard が
	// 効かない。NUL を含む id を渡すと SELECT がその時点で落ち、`IsNotFound`
	// でもないので 500 になる。
	if !emojiapplication.FitsApplicationID(req.ApplicationID) {
		return c.JSON(http.StatusNotFound, apierr.Error(
			"NO_SUCH_APPLICATION", "No such application.",
			"3f0e6b4a-9c21-4d7e-8b35-6a1f2c9d4e08"))
	}
	app, err := h.emojiApplicationRepo.FindByID(req.ApplicationID)
	if err != nil {
		// DB 障害を not-found に丸めない (#2792)。
		if repository.IsNotFound(err) {
			return c.JSON(http.StatusNotFound, apierr.Error(
				"NO_SUCH_APPLICATION", "No such application.",
				"3f0e6b4a-9c21-4d7e-8b35-6a1f2c9d4e08"))
		}
		return apierr.JSONInternalError(c)
	}

	counts, err := h.emojiApplicationRepo.CountRelated(app)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	// **列に入らないカーソルは 400 (#3025)。** `untilId` はそのまま
	// `"id" < ?` の bind parameter に載るので、NUL を含むとクエリごと落ちて
	// 500 になる。他のページングと同じく `id.NormalizeCursor` を通す。
	_, untilID, cursorOK := id.NormalizeCursor("", req.UntilID, nil, nil)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	rows, err := h.emojiApplicationRepo.FindRelated(app, req.Limit, untilID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}

	items := make([]map[string]any, 0, len(rows))
	for i := range rows {
		// **審査側の pack を使う。** 申請者向けの pack はモデレーターや
		// 却下理由を落とすので、履歴として見る意味が無くなる。
		packed := h.packEmojiApplicationForModerator(&rows[i].EmojiApplication)
		packed["matchedBy"] = rows[i].MatchedBy()
		items = append(items, packed)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"counts": counts,
		"items":  items,
	})
}

// emojiApplicationUserStatuses is the status filter accepted by list-by-user.
//
// **未知の値を全件に倒さない (#2961)。** 絞ったつもりで全部出るほうが危険側で、
// モデレーターは「却下されたものだけ」を見ているつもりで判断する。
var emojiApplicationUserStatuses = map[string]bool{
	repository.EmojiApplicationFilterAll: true,
	model.EmojiApplicationPending:        true,
	model.EmojiApplicationApproved:       true,
	model.EmojiApplicationRejected:       true,
	model.EmojiApplicationCanceled:       true,
}

// EmojiApplicationListByUser handles POST /api/admin/emoji-application/list-by-user.
//
// ユーザーモデレーション画面の申請履歴 (#2961)。**一般ユーザー向けには公開
// しない** — 却下理由とモデレーターへの補足が載る。
func (h *Handler) EmojiApplicationListByUser(c echo.Context) error {
	if h.emojiApplicationRepo == nil {
		return apierr.JSONInternalError(c)
	}
	var req struct {
		UserID  string `json:"userId"`
		Status  string `json:"status"`
		Query   string `json:"query"`
		Limit   int    `json:"limit"`
		UntilID string `json:"untilId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"INVALID_PARAM", "userId is required.", apierr.UUIDInvalidParam))
	}
	if req.Status != "" && !emojiApplicationUserStatuses[req.Status] {
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"INVALID_PARAM", "Unknown status.", apierr.UUIDInvalidParam))
	}
	// **既存の一覧と同じクランプ。** packer が 1 行あたり drive / emoji を
	// 引くので、上限を上げると 1 リクエストで数百クエリになる。
	if req.Limit <= 0 || req.Limit > 100 {
		req.Limit = 30
	}

	// **列に入らないカーソルは 400 (#3025)。** `untilId` はそのまま
	// `"id" < ?` の bind parameter に載るので、NUL を含むとクエリごと落ちて
	// 500 になる。他のページングと同じく `id.NormalizeCursor` を通す。
	_, untilID, cursorOK := id.NormalizeCursor("", req.UntilID, nil, nil)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	rows, err := h.emojiApplicationRepo.ListByUserFiltered(req.UserID, req.Status, req.Query, req.Limit, untilID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	// **0 件でも配列で返す。** null だと frontend の追い読みが TypeError になる。
	items := make([]map[string]any, 0, len(rows))
	for i := range rows {
		items = append(items, h.packEmojiApplicationForModerator(&rows[i]))
	}
	return c.JSON(http.StatusOK, map[string]any{"items": items})
}

// EmojiApplicationUserSummary handles POST /api/admin/emoji-application/user-summary.
//
// 件数の内訳と、期間上限の使用状況 (#2961)。
func (h *Handler) EmojiApplicationUserSummary(c echo.Context) error {
	if h.emojiApplicationReviewer == nil {
		return apierr.JSONInternalError(c)
	}
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"INVALID_PARAM", "userId is required.", apierr.UUIDInvalidParam))
	}

	sum, err := h.emojiApplicationReviewer.UserSummary(req.UserID)
	if err != nil {
		// **0 件として返さない。** 障害を「申請なし」と描くと、実際には
		// 履歴のあるユーザーを何も無いものとして扱う。
		return apierr.JSONInternalError(c)
	}

	windows := make([]map[string]any, 0, len(sum.Windows))
	for _, w := range sum.Windows {
		out := map[string]any{
			"period": w.Period,
			"used":   w.Used,
			"limit":  w.Limit,
			// **無制限を 0 で表さない。** 0 を「上限 0 件 = 出せない」と読める
			// 形にすると、画面が「0 / 0」を出して枠が尽きているように見える。
			"unlimited": w.Limit <= 0,
		}
		// 満杯の窓だけ次に出せる時刻を載せる。空きがあるのに時刻が出ると
		// 「今は出せない」と読める。
		if !w.RetryAt.IsZero() {
			out["retryAt"] = entity.ISOMillis(w.RetryAt)
		}
		windows = append(windows, out)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"counts":  sum.Counts,
		"windows": windows,
		// **審査待ちの上限も出す (レビュー H1)。** 期間の窓に空きがあっても
		// これが満杯なら申請は 400 で弾かれる。出さないと画面は
		// 「24時間: 2 / 10 (空きあり)」と描き、実際には出せない人を出せると
		// 案内する — この機能が塞ごうとしている失敗形そのもの。
		"pending": map[string]any{
			"used":      sum.Pending,
			"limit":     sum.MaxPending,
			"unlimited": sum.MaxPending <= 0,
		},
		// 最後に枠を手動で戻した操作 (#2962)。未実施なら null。
		"lastReset": packQuotaReset(sum.LastReset),
	})
}

// packQuotaReset renders one manual quota reset for the moderation screen (#2962).
//
// **モデレーターにだけ出す経路なので実行者を出す。** 申請者向けの pack が
// 審査担当者を落とすのとは前提が違う (こちらは admin gate の内側)。
func packQuotaReset(row *model.EmojiApplicationQuotaReset) any {
	if row == nil {
		return nil
	}
	return map[string]any{
		"at":     entity.ISOMillis(row.CreatedAt),
		"byId":   row.ResetByID,
		"reason": row.Reason,
	}
}

// EmojiApplicationResetUserQuota handles
// POST /api/admin/emoji-application/reset-user-quota (#2962).
//
// **申請の行は触らない。** 履歴を消して枠を空けると、過去の判断 (#2960 が
// 審査の材料として出している却下理由や同じ画像かどうか) も同時に消える。
func (h *Handler) EmojiApplicationResetUserQuota(c echo.Context) error {
	if h.emojiApplicationReviewer == nil || h.userRepo == nil {
		return apierr.JSONInternalError(c)
	}
	var req struct {
		UserID string `json:"userId"`
		Reason string `json:"reason"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"INVALID_PARAM", "userId is required.", apierr.UUIDInvalidParam))
	}
	// **理由は必須。** 監査ログに残る唯一の文脈なので、空を許すと
	// 「誰かが戻した」以上のことが後から分からない。空白だけも弾く。
	if strings.TrimSpace(req.Reason) == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"INVALID_PARAM", "reason is required.", apierr.UUIDInvalidParam))
	}

	// **実在しない利用者に対して行を作らない。** 打ち間違いで残った行は
	// 誰のものでもない監査記録になる。
	target, err := h.userRepo.FindByID(req.UserID)
	if err != nil {
		// DB 障害を not-found に丸めない (#2792)。
		if repository.IsNotFound(err) {
			return c.JSON(http.StatusNotFound, apierr.Error(
				"NO_SUCH_USER", "No such user.", apierr.UUIDNoSuchUser))
		}
		return apierr.JSONInternalError(c)
	}

	// **操作者が取れないまま進めない (レビュー L1)。** 空のまま続けると
	// `resetById = ''` の行が残り、しかも `logModeration` は actor nil で
	// 黙って return するので**監査ログが 1 件も残らない**。approve / reject が
	// 採っているのと同じ扱いにする (middleware が 401 で止めるので production
	// では到達しない)。
	actor := middleware.GetUser(c)
	if actor == nil {
		return apierr.JSONInternalError(c)
	}
	actorID := actor.ID
	before, row, err := h.emojiApplicationReviewer.ResetQuota(req.UserID, actorID, req.Reason)
	if err != nil {
		if errors.Is(err, emojiapplication.ErrResetReasonRequired) {
			return c.JSON(http.StatusBadRequest, apierr.Error(
				"INVALID_PARAM", "reason is required.", apierr.UUIDInvalidParam))
		}
		// **長すぎる理由は 400 (レビュー M1)。** そのまま DB へ渡すと
		// SQLSTATE 22001 で 500 になり、画面は「もう一度お試しください」と
		// 案内するが何度やっても通らない。
		if errors.Is(err, emojiapplication.ErrTooLong) {
			return c.JSON(http.StatusBadRequest, apierr.Error(
				"INVALID_PARAM", "reason is too long or contains an invalid character.",
				apierr.UUIDInvalidParam))
		}
		return apierr.JSONInternalError(c)
	}

	// **リセット前の使用数を残す。** 後から採ると必ず 0 になり、「何件使って
	// いた人を戻したか」という記録として意味が無くなる。
	info := moderationlog.UserInfo(target)
	info["reason"] = row.Reason
	for _, w := range before.Windows {
		// **バイトで切らない (レビュー L5)。** `Period` は素の string なので、
		// 空なら panic、非 ASCII の窓名を足すと不正な UTF-8 のキーになる。
		if k := quotaUsageInfoKey(w.Period); k != "" {
			info[k] = w.Used
		}
	}
	h.logModeration(c, moderationlog.LogResetEmojiApplicationQuota, info)

	return c.JSON(http.StatusOK, map[string]any{"lastReset": packQuotaReset(row)})
}

// quotaUsageInfoKey renders the moderation log key for one window (#2962).
//
// 例: "day" -> "usedDay"。空の期間名はキーを作らない (キーが "used" に潰れて
// 窓ごとの値が上書きされる)。
func quotaUsageInfoKey(period string) string {
	r := []rune(period)
	if len(r) == 0 {
		return ""
	}
	return "used" + strings.ToUpper(string(r[0])) + string(r[1:])
}
