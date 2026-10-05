package admin

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/pagination"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/core/role"
)

// driveCleanupBatchSize はバルク削除の 1 バッチ件数。upstream の cursor 巡回
// (8 件) より大きめにして round-trip を減らす。
const driveCleanupBatchSize = 100

// driveCleanupMaxBatches は無限ループ防止の安全弁 (= 約 100 万件)。
const driveCleanupMaxBatches = 10000

// DriveCleanRemoteFiles handles POST /api/admin/drive/clean-remote-files.
//
// upstream CleanRemoteFilesProcessorService はキャッシュ済みリモートファイル
// (userHost IS NOT NULL AND isLink=false) を物理オブジェクトごと削除する。
// endpoint 自体は job を 1 本積んで即座に返し、実際のバッチ削除は
// objectStorage queue の worker が回す (#2325)。
//
// queue 未配線時のみ、従来どおり handler 内で同期削除する。storage backend も
// 未配線なら DB 行のみ削除 (条件は修正済 = isLink=false)。
func (h *Handler) DriveCleanRemoteFiles(c echo.Context) error {
	if h.driveFileRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	if h.objectStorageEnqueuer != nil {
		if err := h.objectStorageEnqueuer.EnqueueCleanRemoteFiles(); err != nil {
			slog.ErrorContext(c.Request().Context(), "drive cleanup: enqueue cleanRemoteFiles failed", "err", err)
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		return c.NoContent(http.StatusNoContent)
	}
	if h.storageDeleter == nil {
		// storage 未配線: DB 行を link に倒すだけ (実体はそもそも無い)。
		_, _ = h.driveFileRepo.ExpireRemoteCache()
		return c.NoContent(http.StatusNoContent)
	}
	// **行は消さず link に倒す** (#3102)。upstream と同じ動作で、消すと
	// `note.fileIds` の指す先が無くなり過去の投稿から添付が黙って消える。
	if err := h.cleanupFilesBatched(c, h.driveFileRepo.ListRemoteCache, h.driveFileRepo.ExpireByIDs); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	return c.NoContent(http.StatusNoContent)
}

// deleteFilesBatched lists files via list() in batches, deletes each file's
// object-storage objects, then deletes the DB rows, until none remain. Because
// the rows are deleted each round, list() naturally returns the next batch
// (driveCleanupMaxBatches caps the loop against a non-shrinking repo).
//
// storage object 削除は best-effort (失敗しても DB 行は消す)。同期ループで
// upstream のような job 再試行ができないため、storage 失敗で DB 削除を止めると
// 同じ行を re-list し続けてしまう。失敗は slog で観測可能にし orphan を検知できる
// ようにする。list / DeleteByIDs (= DB) の失敗は hard error として返し、handler が
// 500 を返す (DB-only 経路の 500 と整合)。
func (h *Handler) deleteFilesBatched(c echo.Context, list func(limit int) ([]*model.DriveFile, error)) error {
	return h.cleanupFilesBatched(c, list, h.driveFileRepo.DeleteByIDs)
}

// cleanupFilesBatched is deleteFilesBatched with the terminal DB operation
// parameterised: `finish` either deletes the rows or turns them into link rows.
//
// **リモートキャッシュの掃除だけ link に倒す** (#3102)。利用者のファイルを消す
// ほう (`delete-all-files-of-a-user`) は今までどおり行ごと消す — あちらは
// 「その利用者のファイルを消す」が目的で、行を残す理由が無い。
//
// **`finish` が行を消さない場合、list が同じ行を返し続けない形であること。**
// link に倒すと `isLink = false` の条件から外れるので `ListRemoteCache` は
// 次のバッチを返す (消す場合と同じ)。
func (h *Handler) cleanupFilesBatched(c echo.Context, list func(limit int) ([]*model.DriveFile, error), finish func(ids []string) (int64, error)) error {
	for i := 0; i < driveCleanupMaxBatches; i++ {
		files, err := list(driveCleanupBatchSize)
		if err != nil {
			slog.ErrorContext(c.Request().Context(), "drive cleanup: list failed", "err", err)
			return err
		}
		if len(files) == 0 {
			return nil
		}
		ids := make([]string, 0, len(files))
		for _, f := range files {
			h.deleteFileStorageObjects(c, f)
			ids = append(ids, f.ID)
		}
		if _, err := finish(ids); err != nil {
			slog.ErrorContext(c.Request().Context(), "drive cleanup: finish failed", "err", err)
			return err
		}
		if len(files) < driveCleanupBatchSize {
			return nil
		}
	}
	return nil
}

// deleteFileStorageObjects removes a file's primary / thumbnail / webpublic
// objects from object storage. best-effort: 欠損 object はエラーにならない
// (Storage.Delete の契約) が、それ以外の失敗は slog.Warn で残す (DB 行は消すので
// orphan を後追いできるように)。upstream は thumbnailUrl/webpublicUrl の有無で
// gate するが、mk-go は access key の有無で判定する (best-effort なので余分な key
// への Delete は no-op)。
//
// object storage 有効化より前に保存された storedInternal=true な行はローカル FS に
// 実体があるので、primary (= object storage) に消しに行っても空振りしてローカルに
// 孤児が残る。行の storedInternal を見て backend を選ぶ (#1414 の二系統 storage /
// #2315)。localStorageDeleter 未配線なら従来どおり primary に倒す。
func (h *Handler) deleteFileStorageObjects(c echo.Context, f *model.DriveFile) {
	if h.storageDeleter == nil || f == nil {
		return
	}
	// object storage 上の実体は queue に逃がす (#2325)。upstream の cleanup も
	// ファイルごとに deleteFile job を積む。ローカル FS の行 (object storage
	// 有効化より前に保存されたもの) は upstream 同様ここで同期削除する。
	queued := !f.StoredInternal && h.objectStorageEnqueuer != nil
	deleter := h.storageDeleter
	if f.StoredInternal && h.localStorageDeleter != nil {
		deleter = h.localStorageDeleter
	}
	for _, key := range []*string{f.AccessKey, f.ThumbnailAccessKey, f.WebpublicAccessKey} {
		if key == nil || *key == "" {
			continue
		}
		if queued {
			err := h.objectStorageEnqueuer.EnqueueDeleteObjectStorageFile(*key)
			if err == nil {
				continue
			}
			// queue が死んでいても実体は消す。取りこぼすと追跡不能な孤児になる。
			slog.WarnContext(c.Request().Context(), "drive cleanup: enqueue object delete failed, deleting inline",
				"fileId", f.ID, "err", err)
		}
		if err := deleter.Delete(*key); err != nil {
			slog.WarnContext(c.Request().Context(), "drive cleanup: storage delete failed (object may be orphaned)",
				"fileId", f.ID, "err", err)
		}
	}
}

// DriveCleanup handles POST /api/admin/drive/cleanup.
//
// upstream の cleanup は orphan file (userId IS NULL) を 1 件ずつ
// driveService.deleteFile で消すので object storage の実体も削除される。
// storage backend が配線されていれば list→object storage 削除→DB 行削除を
// バッチで回す。未配線時は DB 行のみ削除する。DriveCleanRemoteFiles と同じ
// 二系統構造 (#1724)。
//
// **対象行の選び方は両経路で同じ。** どちらも DriveFileRepository の
// DeleteOrphans / ListOrphans が返す集合で、選択条件は同 interface の doc を
// 参照。
func (h *Handler) DriveCleanup(c echo.Context) error {
	if h.driveFileRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	if h.storageDeleter == nil {
		_, _ = h.driveFileRepo.DeleteOrphans()
		return c.NoContent(http.StatusNoContent)
	}
	if err := h.deleteFilesBatched(c, h.driveFileRepo.ListOrphans); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	return c.NoContent(http.StatusNoContent)
}

// SystemUserIDToken は `/api/admin/drive/files` の `userId` パラメータで
// 「local な system 所有 (userId IS NULL かつ userHost IS NULL) の drive
// file」を一覧する特殊値。custom emoji の copy / import zip 経路で蓄積される
// system file (#670 で導入) を admin UI から可視化する経路 (#686)。
// `@` 接頭辞は実 user ID と衝突しない (aidx ID は英数字のみ) ため安全。
//
// **リモートの owner 無し行は含まない (#2753)。** それらは origin=remote を
// 渡せば通常の一覧に出る。
const SystemUserIDToken = "@system"

// DriveFiles handles POST /api/admin/drive/files.
func (h *Handler) DriveFiles(c echo.Context) error {
	if h.driveFileRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	var req struct {
		Limit     *int   `json:"limit"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
		UserID    string `json:"userId"`
		Origin    string `json:"origin"`
		Host      string `json:"host"`
		Hostname  string `json:"hostname"`
		Type      string `json:"type"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	// userId に @system が指定されたら system file 専用 listing を返す。
	// origin / host filter は**無視する** (upstream も userId 指定時は読まない)。
	// この一覧は local (userHost IS NULL) に限定してあるので、origin=remote を
	// 渡しても local 行が返る点に注意 (#2753)。type と pagination のみを
	// 取り回す。
	if req.UserID == SystemUserIDToken {
		files, err := h.driveFileRepo.ListSystemFiles(req.Type, untilID, sinceID, limit)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		out := make([]entity.DriveFileEntity, 0, len(files))
		for _, f := range files {
			out = append(out, h.packDriveFileAdmin(f))
		}
		return c.JSON(http.StatusOK, out)
	}
	switch req.Origin {
	case "combined", "local", "remote":
	case "":
		// upstream files.ts paramDef は origin default='local' (#1545)。
		// userId 未指定時は userHost IS NULL のローカルのみ返す。
		req.Origin = "local"
	default:
		req.Origin = "combined"
	}
	// upstream は host も hostname もどちらも受けて同じ意味で扱う。
	// frontend (admin/instance-info) が hostname で投げるため互換上両方サポート。
	host := req.Host
	if host == "" {
		host = req.Hostname
	}
	files, err := h.driveFileRepo.ListForAdmin(req.UserID, req.Origin, host, req.Type, untilID, sinceID, limit)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	out := make([]entity.DriveFileEntity, 0, len(files))
	for _, f := range files {
		out = append(out, h.packDriveFileAdmin(f))
	}
	return c.JSON(http.StatusOK, out)
}

// packDriveFileAdmin packs a drive file and embeds the owning user as
// nested `user` when userRepo is wired. Folder is left nil because the
// admin handler does not currently wire a DriveFolderRepository.
func (h *Handler) packDriveFileAdmin(f *model.DriveFile) entity.DriveFileEntity {
	// user repoが未設定なら user は nil fallback。folder は admin handler
	// に folderRepoがないため常に nil (必要になれば拡張)。
	var user *model.User
	if h.userRepo != nil && f.UserID != nil {
		if u, err := h.userRepo.FindByID(*f.UserID); err == nil {
			user = u
		}
	}
	// admin moderation view は upstream pack({self:true}) 相当で raw properties を
	// 保持する (orientation を strip しない、#1948-14)。
	return entity.PackDriveFileWithRelationsSelf(f, h.idGen, nil, user)
}

// DriveShowFile handles POST /api/admin/drive/show-file.
// Accepts either a fileId or a url as identifier (Misskey 本家互換)。
// レスポンス shape は upstream admin/drive/show-file.ts の custom 28-field
// 形に揃え、`requestIp` / `requestHeaders` を含む (frontend admin-file の
// IP タブで参照、それ以外は PackDriveFile + admin only field の合成、#1148)。
func (h *Handler) DriveShowFile(c echo.Context) error {
	var req struct {
		FileID string `json:"fileId"`
		URL    string `json:"url"`
	}
	if err := c.Bind(&req); err != nil || (req.FileID == "" && req.URL == "") {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "fileId or url is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.driveFileRepo == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "No such file.", "caf3ca38-c6e5-472e-a30c-b05377dcc240"))
	}
	viewer := middleware.GetUser(c)
	if req.FileID != "" {
		file, err := h.driveFileRepo.FindByID(req.FileID)
		if err != nil && !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		if err != nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "No such file.", "caf3ca38-c6e5-472e-a30c-b05377dcc240"))
		}
		return c.JSON(http.StatusOK, h.packAdminDriveShowFile(file, viewer))
	}
	// url 指定時は adminDB を使って url / thumbnailUrl / webpublicUrl いずれか
	// に一致する 1 件を引く。 driveFileRepo には該当 API が無いため raw query で。
	if h.adminDB == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "No such file.", "caf3ca38-c6e5-472e-a30c-b05377dcc240"))
	}
	var file model.DriveFile
	if err := h.adminDB.Where(
		`"url" = ? OR "thumbnailUrl" = ? OR "webpublicUrl" = ?`,
		req.URL, req.URL, req.URL,
	).First(&file).Error; err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "No such file.", "caf3ca38-c6e5-472e-a30c-b05377dcc240"))
	}
	return c.JSON(http.StatusOK, h.packAdminDriveShowFile(&file, viewer))
}

// packAdminDriveShowFile builds the upstream-compatible admin/drive/show-file
// response shape.
//
// `requestIp` は viewer が moderator **かつ `canSearchIpHistory` を持つ**
// ときだけ含める。**upstream より厳しい** (あちらは moderator であれば返す) —
// mk-go は IP の閲覧を role policy で絞る仕組みを持っており、`admin/ip/*` が
// それを要求するのにここだけ素通しだと迂回路になる (`docs/divergence.md`)。
// `requestHeaders` は上記に加えて owner が moderator でない場合のみ含める
// (upstream: モデレーターの個人情報を他のモデレーターから守る制限)。
//
// 両 field とも `nil` で omit せず明示 null を emit する (upstream の
// `optional: false, nullable: true` schema 通り)。
func (h *Handler) packAdminDriveShowFile(f *model.DriveFile, viewer *model.User) map[string]any {
	// リモートファイルの url/thumbnailUrl/webpublicUrl は外部 URL なので、admin が
	// drive 一覧を開いただけで moderator の IP が連合先へ漏洩する (issue #1529)。
	// browser が img src として取得するこれら 3 つは、通常の drive pack と同じ
	// media proxy 書き換え (image 限定・thumbnail は static mode・remote のみ) を
	// 通すことで一貫させる。uri/accessKey 等は moderation 用の raw メタなのでそのまま。
	// admin moderation view は self:true 相当。ここでは packed から url/thumbnailUrl/
	// webpublicUrl のみ使い、properties は後段の driveFileProperties が raw 返却するため
	// self の有無は実効的に URL のみに影響する。一貫性のため Self 変種を使う (#1948-14)。
	packed := entity.PackDriveFileSelf(f, h.idGen)
	fileURL := packed.URL
	thumbURL := packed.ThumbnailURL
	webpublicURL := packed.WebpublicURL
	resp := map[string]any{
		"id":                 f.ID,
		"userId":             f.UserID,
		"userHost":           f.UserHost,
		"isLink":             f.IsLink,
		"maybePorn":          f.MaybePorn,
		"maybeSensitive":     f.MaybeSensitive,
		"isSensitive":        f.IsSensitive,
		"folderId":           f.FolderID,
		"src":                f.Src,
		"uri":                f.URI,
		"webpublicAccessKey": f.WebpublicAccessKey,
		"thumbnailAccessKey": f.ThumbnailAccessKey,
		"accessKey":          f.AccessKey,
		"webpublicType":      f.WebpublicType,
		"webpublicUrl":       webpublicURL,
		"thumbnailUrl":       thumbURL,
		"url":                fileURL,
		"storedInternal":     f.StoredInternal,
		"properties":         driveFileProperties(f.Properties),
		"blurhash":           f.Blurhash,
		"comment":            f.Comment,
		"size":               f.Size,
		"type":               f.Type,
		"name":               f.Name,
		"md5":                f.MD5,
		"createdAt":          driveFileCreatedAtOrNil(h.idGen, f.ID),
		"requestIp":          nil,
		"requestHeaders":     nil,
	}
	// gating: 通常 admin route は moderator/admin gate 済だが、防御的 check。
	viewerIsModerator := viewer != nil && h.roleService != nil && h.roleService.IsModerator(viewer.ID)
	// **IP を出してよいのは `canSearchIpHistory` を持つ相手だけ。**
	//
	// #3114 が `admin/show-user` の signin IP に入れたのと同じ条件。あちらだけ
	// 絞ると、同じ種類の情報 (ローカル利用者の接続元 IP) を素のモデレーター
	// 権限だけで全件返す口がここに残り、policy が門として成立しない。
	// `admin/drive/files` に userId を渡してファイル ID を列挙すれば、
	// アップロード元 IP の履歴がそのまま取れる。
	//
	// `HasRolePolicy` は管理者を短絡するので、既定 (policy false) の構成では
	// 管理者だけが通る = `admin/ip/*` と同じ条件になる。
	// **roleService 未配線なら伏せる側へ倒す** (fail-closed)。
	showIPs := viewerIsModerator && h.roleService != nil && viewer != nil &&
		h.roleService.HasRolePolicy(viewer.ID, role.PolicyCanSearchIPHistory)
	if showIPs {
		resp["requestIp"] = f.RequestIP
		// owner が moderator のときは headers を隠す (upstream 仕様、
		// モデレーター同士の互いの個人情報保護)。
		//
		// **判定できないときは隠す側に倒す (#3037 レビュー 2 周目)。**
		// `IsModerator` は判定できないときに false を返すので、素で使うと
		// `ListByUser` が一時的に失敗する窓で他のモデレーターの
		// `requestHeaders` が出る。隠して困るのは表示が減ることだけ。
		ownerIsModerator := true
		if f.UserID == nil || *f.UserID == "" {
			ownerIsModerator = false
		} else if h.roleService != nil {
			if _, mod, err := h.roleService.RolePrivileges(*f.UserID); err == nil {
				ownerIsModerator = mod
			}
		} else {
			ownerIsModerator = false
		}
		if !ownerIsModerator {
			// **ヘッダにも接続元 IP が載る。** 本番構成の nginx は
			// `x-real-ip` / `x-forwarded-for` を必ず付けるので、
			// `requestIp` だけ絞ってもここから同じ値が読める。
			resp["requestHeaders"] = driveFileRequestHeaders(f.RequestHeaders)
		}
	}
	return resp
}

// driveFileCreatedAtOrNil renders the aidx-derived createdAt ISO string,
// returning nil on parse failure (= shape becomes `"createdAt": null` rather
// than emitting a malformed string).
func driveFileCreatedAtOrNil(idGen id.Generator, aidx string) any {
	if s, err := aidxCreatedAtString(idGen, aidx); err == nil {
		return s
	}
	return nil
}

// driveFileProperties unmarshals the jsonb properties column to map for the
// admin endpoint response. Falls back to empty map on parse error.
func driveFileProperties(raw datatypes.JSON) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// driveFileRequestHeaders unmarshals the jsonb requestHeaders column to a
// map<string,string> shape that frontend admin-file.root.vue iterates with
// v-for="(v, k) in info.requestHeaders". Falls back to nil if column is
// empty / unparseable so frontend's `v-if` guard hides the section.
func driveFileRequestHeaders(raw datatypes.JSON) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
