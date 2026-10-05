// Package drive provides /api/drive/* endpoints.
package drive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/notehide"
	"github.com/elythia-network/elythia/internal/api/pagination"
	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/notesfilter"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/colfit"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
	"gorm.io/datatypes"
)

// Handler handles drive-related API endpoints.
type Handler struct {
	svc          *coredrive.Service
	idGen        id.Generator
	fileRepo     repository.DriveFileRepository
	folderRepo   repository.DriveFolderRepository
	noteRepo     repository.NoteRepository
	userRepo     repository.UserRepository
	instanceRepo repository.InstanceRepository
	emojiRepo    repository.EmojiRepository
	bufReader    entity.BufferedReactionsReader
	fieldRes     *entity.NoteFieldResolver
	urlUploader  URLUploadProcessor
	// metaRepo は drive/files/create で meta.enableIpLogging を確認し、無効時に
	// requestIp / requestHeaders を保存しないために使う (#2106 S4)。nil なら保存しない
	// (= 安全側)。
	metaRepo repository.MetaRepository
}

// SetMetaRepo wires the meta repository used to honor enableIpLogging on upload.
func (h *Handler) SetMetaRepo(m repository.MetaRepository) {
	h.metaRepo = m
}

// SetURLUploader wires the processor used by /drive/files/upload-from-url to
// download a remote file in the background and notify the user via the main
// stream. Without it the endpoint degrades to a 204 no-op.
func (h *Handler) SetURLUploader(p URLUploadProcessor) {
	h.urlUploader = p
}

// SetNoteFieldResolver wires the shared resolver that fills Files /
// MyReaction / Channel on packed notes (#426)。
func (h *Handler) SetNoteFieldResolver(r *entity.NoteFieldResolver) {
	h.fieldRes = r
}

// NewHandler creates a new drive Handler.
func NewHandler(svc *coredrive.Service, idGen id.Generator) *Handler {
	return &Handler{svc: svc, idGen: idGen}
}

// SetRepos attaches repositories for drive listing endpoints.
func (h *Handler) SetRepos(fileRepo repository.DriveFileRepository, folderRepo repository.DriveFolderRepository, noteRepo repository.NoteRepository) {
	h.fileRepo = fileRepo
	h.folderRepo = folderRepo
	h.noteRepo = noteRepo
}

// SetUserRepo attaches a UserRepository so drive file responses can embed
// the owning user as `user` (TS schema: DriveFile.user: UserLite). Without
// it the `user` field is omitted.
func (h *Handler) SetUserRepo(r repository.UserRepository) {
	h.userRepo = r
}

// SetInstanceRepo attaches an InstanceRepository so drive/files/attached-notes
// populates UserLite.Instance for remote users (#277).
func (h *Handler) SetInstanceRepo(r repository.InstanceRepository) {
	h.instanceRepo = r
}

func (h *Handler) instanceLookup() entity.InstanceLookup {
	if h.instanceRepo == nil {
		return nil
	}
	return h.instanceRepo
}

// SetEmojiRepo attaches an EmojiRepository so custom emoji shortcodes in
// note text and user displayNames get resolved to URLs.
func (h *Handler) SetEmojiRepo(r repository.EmojiRepository) {
	h.emojiRepo = r
}

// SetReactionReader wires a BufferedReactionsReader so PackNote / PackNotes
// can merge in-flight buffered reaction deltas (#647)。
func (h *Handler) SetReactionReader(r entity.BufferedReactionsReader) {
	h.bufReader = r
}

func (h *Handler) reactionReader() entity.BufferedReactionsReader {
	return h.bufReader
}

func (h *Handler) emojiLookup() entity.EmojiLookup {
	if h.emojiRepo == nil {
		return nil
	}
	return h.emojiRepo
}

// upstream paramDef の maxLength 定数 (#1564)。ajv の maxLength は UTF-16
// code unit 基準だが mk-go は rune 数で近似する (多バイト文字を過剰に弾か
// ない側。サロゲートペア絵文字は upstream より長く受理される既知の差)。
const (
	// maxDriveCommentLength は drive/files/{create,update,upload-from-url}
	// の comment maxLength (= upstream DB_MAX_IMAGE_COMMENT_LENGTH)。
	maxDriveCommentLength = 512
	// maxDriveFolderNameLength は drive/folders/{create,update} の name
	// maxLength。core 側 ValidateFileName の 200 (file name) とは別の値。
	maxDriveFolderNameLength = 200
)

// driveTypePatternRe は drive/files・drive/stream の `type` param に対する
// upstream paramDef pattern `^[a-zA-Z\/\-*]+$` の移植 (#1564 review)。
// 数字を含む MIME (video/mp4 等) も upstream はこの pattern で reject する。
// SQL LIKE の metacharacter (% _) もここで弾かれるため repo 層の LIKE に
// wildcard が混入しない。
var driveTypePatternRe = regexp.MustCompile(`^[a-zA-Z/\-*]+$`)

// packDriveFileSelfSingle packs a drive file for the "self single" path used
// by drive/files/create (and the urlUploadFinished payload in url_upload.go).
//
// upstream Misskey TS は `pack(file, { self: true })` (= withUser=false /
// detail=false) を呼び、`folder=null` / `user=null` / `userId=null` を
// 返す (DriveFileEntityService.ts:191-222)。PackDriveFile は Folder / User
// を fetch しない (= nil) ので、userId の null 化だけ足す (#812 / #1564
// review で full pack→null 潰しの無駄 fetch を解消)。
//
// self list path とは異なり userId も **null にする** 点に注意 (= pack は
// detail=false で userId を返さない、packNullable とは別経路)。
func packDriveFileSelfSingle(f *model.DriveFile, idGen id.Generator) entity.DriveFileEntity {
	e := entity.PackDriveFileSelf(f, idGen)
	e.UserID = nil
	return e
}

// packDriveFileSelfList packs a drive file for the "self list" path used by
// drive/files, drive/files/find, drive/files/find-by-hash, drive/stream.
//
// upstream Misskey TS は `packMany(files, { self: true })` 経由で
// `packNullable` を呼び、`folder=null` / `user=null` / `userId=owner ID`
// を返す (DriveFileEntityService.ts:225-261, withUser/detail デフォルト
// false)。PackDriveFile がそのままこの shape (Folder/User nil, UserID 維持)
// を返す (#818 / #1564 review で full pack→null 潰しの per-file 2 query を
// 解消。limit=100 の list で最大 200 query 削減)。
//
// self single path (#812) とは異なり userId は **owner ID を維持** する
// (= packNullable は userId を常時返す)。
func (h *Handler) packDriveFileSelfList(f *model.DriveFile) entity.DriveFileEntity {
	return entity.PackDriveFileSelf(f, h.idGen)
}

// openMultipartFile opens one parsed multipart part. テスト用の差し替え口。
//
// **メモリ由来とは限らない。** echo の multipart パーサは `maxMemory` を
// 超えた分を一時ファイルへ落とすので、`Open` は実ファイルを開く。ディスクが
// 埋まっている / 一時ファイルが消された / I/O エラーのいずれでも失敗しうる。
var openMultipartFile = func(fh *multipart.FileHeader) (multipart.File, error) {
	return fh.Open()
}

// readMultipartFile extracts the uploaded file's bytes and original filename.
// テスト用に差し替え可能な変数として定義する。
//
// **`Open` と `ReadAll` の error を捨てない (#3037)。** 以前は `src, _ :=` /
// `body, _ :=` と書いており、
//
//   - `Open` 失敗 → `src` が nil のまま `defer src.Close()` で **nil 参照 panic**
//   - `ReadAll` の途中失敗 → **無言で切り詰められた本体**がそのまま保存され、
//     MD5 / size / MIME がその切れ端で確定する (利用者には成功として返る)
//
// の 2 つが起きた。どちらも「実用上失敗しない」を前提にしていたが、上の
// `openMultipartFile` のとおりその前提が成り立たない。
var readMultipartFile = func(c echo.Context, maxBytes int64) ([]byte, string, error) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		return nil, "", err
	}
	// **`io.ReadAll` のコピーを 1 つ減らす (#3037)。** `Upload` にも同じ判定が
	// あるが、あそこへ届く時点で本体はもう読み終わっている。既定では policy が
	// 30MB なのに `config.maxFileSize` が 250MB なので、**30MB しか保存できない
	// 利用者が 250MB のヒープを確保させられた**。
	//
	// **「読む前」ではない (レビュー 2 周目の実測)。** global な
	// `auth.Authenticate` が `multipart/form-data` のとき `c.FormValue("i")` を
	// 呼ぶので、handler へ来る時点で `ParseMultipartForm(32MiB)` は済んでいる。
	// 消えるのは `io.ReadAll` の 2 つ目のコピーだけで、**RAM 32MiB +
	// 超過分の一時ファイル書き込みは残る** (`ParseMultipartForm` は 32MiB を
	// 超えた分をディスクへ spill するので、250MB の本体なら約 218MB が
	// ディスクに書かれる)。それでも最大 250MB のヒープ確保は実際に消える。
	//
	// `maxBytes` の上限値 (`math.MaxInt64`) は `readAtMost` 側で扱う。
	// ここで比べる `fileHeader.Size` は `int64` なので、`MaxInt64` を
	// 超えることは原理的に無い。
	if maxBytes > 0 && fileHeader.Size > maxBytes {
		return nil, "", coredrive.ErrMaxFileSizeExceeded
	}
	src, err := openMultipartFile(fileHeader)
	if err != nil {
		return nil, "", fmt.Errorf("%w: open: %w", errMultipartIO, err)
	}
	defer src.Close()
	body, err := readAtMost(src, maxBytes)
	if err != nil {
		if errors.Is(err, coredrive.ErrMaxFileSizeExceeded) {
			return nil, "", err
		}
		return nil, "", fmt.Errorf("%w: read: %w", errMultipartIO, err)
	}
	return body, fileHeader.Filename, nil
}

// readAtMost reads r fully, failing with ErrMaxFileSizeExceeded past maxBytes.
//
// **`FileHeader.Size` だけに頼らない。** あれはパーサが数えた値なので、
// 数え方が変わったり part が差し替わったりすると読み込み量と食い違う。
// 上限を強制するのは実際に読むこちら側で、`Size` は「読む前に落とせる
// ときは落とす」ための早い枝。
//
// maxBytes <= 0 は上限なし (policy 未設定 / system file / remote user)。
func readAtMost(r io.Reader, maxBytes int64) ([]byte, error) {
	// **`maxBytes+1` のオーバーフローを避ける (#3037 レビュー)。**
	// `policyMegabytes` は `safemath.MulFloat64` で `MaxInt64` に飽和するので、
	// `maxFileSizeMb` に `math.MaxFloat64` (= このリポジトリが「無制限」の
	// 意味で使うイディオム) を入れると `maxBytes+1` が `MinInt64` になり、
	// `io.LimitReader` が即 EOF を返して **0 バイトの本体が error 無しで
	// 保存される**。この関数の doc が塞いだばかりの「無言で切り詰められた
	// 本体」そのもの。
	if maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return io.ReadAll(r)
	}
	// 1 バイト余分に読んで、超過を「読めてしまった」ことで判定する。
	body, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, coredrive.ErrMaxFileSizeExceeded
	}
	return body, nil
}

// errMultipartIO marks a multipart failure as **server-side**.
//
// **`c.FormFile` の失敗と分ける (#3037)。** あちらは「`file` フィールドが
// 無い」= 本当にクライアント起因なので 400 でよい。`Open` / `ReadAll` の
// 失敗はディスク満杯 / fd 枯渇 / 一時ファイル消失で、**全部サーバー側の
// 事象**。まとめて `INVALID_PARAM` にすると (a) 利用者もフロントも
// 「パラメータが不正」と読んで再試行せず、(b) 監視には 4xx しか出ないので
// 障害が見えない。#2792 と同じ判断で 5xx に倒し、ログにも残す。
var errMultipartIO = errors.New("drive: multipart io")

// uploadBoolFields are drive/files/create's boolean params in paramDef
// order (endpoints/drive/files/create.ts).
var uploadBoolFields = []string{"isSensitive", "force"}

// multipartField returns the value of the multipart text field key when
// the body carries it exactly once.
//
// 本家は multipart の field だけを params にする (handleMultipartRequest)。
// c.FormValue は URL の query も引くので使わない。同じ名前の field が 2 つ
// あると本家では値が undefined になる (@fastify/multipart が配列にし、
// `'value' in v` が偽になる) ので、未指定と同じに扱う。
func multipartField(c echo.Context, key string) (string, bool) {
	form, err := c.MultipartForm()
	if err != nil {
		return "", false
	}
	vs := form.Value[key]
	if len(vs) != 1 {
		return "", false
	}
	return vs[0], true
}

// multipartBool returns the boolean field key (false when absent).
// uploadFieldError must have accepted the request.
func multipartBool(c echo.Context, key string) bool {
	v, _ := multipartField(c, key)
	var b bool
	_ = json.Unmarshal([]byte(v), &b)
	return b
}

// uploadFieldError returns the INVALID_PARAM envelope upstream answers
// drive/files/create's boolean fields with, or nil when they are fine.
//
// 本家の call() は ajv より先に、boolean の param の文字列を JSON.parse で変換
// し、失敗すると 0b5f1631 を返す (ApiCallService.ts の "Cast non JSON
// input")。変換できても真偽値でなければ ("1" や "null")、ajv が
// `#/properties/<key>/type` で落とす。以前は "true" 以外を全て false と
// 読んでいた。
func uploadFieldError(c echo.Context) map[string]any {
	for _, key := range uploadBoolFields {
		if v, ok := multipartField(c, key); ok && !json.Valid([]byte(v)) {
			return apierr.InvalidParamCast(key, "boolean")
		}
	}
	// ajv は paramDef の properties の順 (folderId, name, comment, isSensitive,
	// force) に見て最初の違反を返す。comment の maxLength (512) は真偽値の型
	// より先に出る。
	if v, _ := multipartField(c, "comment"); v != "" && !colfit.Fits(v, maxDriveCommentLength) {
		if utf8.RuneCountInString(v) > maxDriveCommentLength {
			return apierr.InvalidParamClient("#/properties/comment/maxLength", "must NOT have more than 512 characters")
		}
		// 長さは収まるが列に入らない (NUL / 不正な UTF-8) 値。本家に同じ検査は無い。
		return apierr.InvalidParam()
	}
	for _, key := range uploadBoolFields {
		v, ok := multipartField(c, key)
		if !ok {
			continue
		}
		var parsed any
		_ = json.Unmarshal([]byte(v), &parsed)
		if _, isBool := parsed.(bool); !isBool {
			return apierr.InvalidParamClient("#/properties/"+key+"/type", "must be boolean")
		}
	}
	return nil
}

// FilesCreate handles POST /api/drive/files/create.
// multipart/form-data with the file under "file"; optional fields: name,
// folderId, comment, isSensitive, force.
func (h *Handler) FilesCreate(c echo.Context) error {
	user := middleware.GetUser(c)
	if e := uploadFieldError(c); e != nil {
		return c.JSON(http.StatusBadRequest, e)
	}

	// 読み切る前に上限を引く (`readMultipartFile` の doc 参照)。
	maxBytes, _ := h.svc.MaxUploadBytes(user)
	body, filename, err := readMultipartFile(c, maxBytes)
	if err != nil {
		// upstream `drive/files/create` と同じ 413。**`Upload` が返すのと
		// 同じ error** なので、読む前に落ちたか後で落ちたかで応答は変わらない。
		if errors.Is(err, coredrive.ErrMaxFileSizeExceeded) {
			return c.JSON(http.StatusRequestEntityTooLarge, apierr.Error("MAX_FILE_SIZE_EXCEEDED", "Max file size exceeded.", "b9d8c348-33f0-4673-b9a9-5d4da058977a"))
		}
		// サーバー側の I/O 障害は 500 + ログ。クライアント起因 (`file` が
		// 無い) だけ 400 のまま。
		if errors.Is(err, errMultipartIO) {
			slog.ErrorContext(c.Request().Context(), "drive: multipart read failed", "err", err)
			return apierr.JSONInternalError(c)
		}
		return apierr.JSONInvalidParam(c)
	}

	in := coredrive.UploadInput{
		User: user,
		Body: body,
	}
	// #2106 S4: requestIp / requestHeaders は meta.enableIpLogging が有効なときのみ
	// 保存する (upstream create.ts:120-121 の `enableIpLogging ? x : null`)。プライバシー
	// 設定を尊重し PII の過剰保存を防ぐ。upload-from-url は upstream 同様 gate しない (別 handler)。
	if h.metaRepo != nil {
		if m, ferr := h.metaRepo.Fetch(); ferr == nil && m != nil && m.EnableIPLogging {
			in.RequestIP = requestIPFromContext(c)
			in.RequestHeaders = requestHeadersForDrive(c)
		}
	}
	// upstream create.ts:98-108 の name 決定: ps.name ?? file.name を trim し、
	// 空 / 'blob' は null 化、それ以外は validateFileName 失敗で
	// INVALID_FILE_NAME (#1564)。
	//
	// ここで決まるのは**補正前**の名前。upstream `DriveService.addFile` と同じく、
	// 検出した MIME に基づく拡張子補正は保存側 (`core/drive` の
	// `CorrectFilename`) が行う。したがって 'untitled' に落ちた場合も、拡張子の
	// 付いた名前 (`untitled.jpg` 等) で保存される。
	//
	// multipart form の name 欄は「明示的な空文字」と「未指定」を区別する
	// (upstream `ps.name ?? file.name` は '' を nullish 扱いしないため、
	// 明示 '' は trim 後 null → 'untitled' になる) (#1564 review)。
	name := filename
	if v, ok := multipartField(c, "name"); ok {
		name = v
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "blob" {
		name = "untitled"
	} else if !coredrive.ValidateFileName(name) {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_FILE_NAME", "Invalid file name.", "f449b209-0c60-4e51-84d5-29486263bfd4"))
	}
	in.Name = name
	if v, _ := multipartField(c, "folderId"); v != "" {
		in.FolderID = &v
	}
	// comment の長さは uploadFieldError が検査済み。
	if v, _ := multipartField(c, "comment"); v != "" {
		in.Comment = &v
	}
	in.IsSensitive = multipartBool(c, "isSensitive")
	in.Force = multipartBool(c, "force")

	f, err := h.svc.Upload(c.Request().Context(), in)
	if err != nil {
		switch {
		case errors.Is(err, coredrive.ErrFolderNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FOLDER", "No such folder.", "d77545ec-1283-4b73-bbe1-e90e1da6a4e7"))
		case errors.Is(err, coredrive.ErrAccessDenied):
			return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "fe8d7103-0ea8-4ec3-814d-f8b401dc69e9"))
		case errors.Is(err, coredrive.ErrUnallowedFileType):
			// upstream drive/files/create の unallowedFileType。frontend は
			// この UUID で error mapping するので code/id 完全一致が要求される。
			return c.JSON(http.StatusBadRequest, apierr.Error("UNALLOWED_FILE_TYPE",
				"Cannot upload the file because it is an unallowed file type.",
				"4becd248-7f2c-48c4-a9f0-75edc4f9a1ea"))
		// **`ErrUndecodableImage` も 413 に寄せる (#3037 レビュー 2 周目)。**
		// 新しい wire コードを足すとフロントエンドに分岐が無く汎用の失敗になる。
		case errors.Is(err, coredrive.ErrMaxFileSizeExceeded), errors.Is(err, coredrive.ErrUndecodableImage):
			// upstream drive/files/create は httpStatusCode:413 を明示する
			return c.JSON(http.StatusRequestEntityTooLarge, apierr.Error("MAX_FILE_SIZE_EXCEEDED", "Max file size exceeded.", "b9d8c348-33f0-4673-b9a9-5d4da058977a"))
		case errors.Is(err, coredrive.ErrNoFreeSpace):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_FREE_SPACE", "No free space.", "d08dbc37-a6a9-463a-8c47-96c32ab5f064"))
		}
		// ここに来るのはストレージ書き込み失敗などの想定外エラー。ログに残さないと
		// クライアントには汎用 INTERNAL_ERROR しか出ないため、オブジェクトストレージ
		// の設定ミス (エンドポイント不正・認証失敗) を運用側から追跡できない。
		slog.ErrorContext(c.Request().Context(), "drive: upload failed",
			"userId", userIDForLog(user), "name", in.Name, "size", len(body), "err", err)
		return apierr.JSONInternalError(c)
	}
	// upstream `drive/files/create` は `pack(file, { self: true })` の
	// self single path 経路 (#812)。helper が folder / userId / user を
	// 一律 null 化する (旧 inline 版は folder の nil 化を見落としていて
	// folderId 指定 upload で drift していたが、helper 化で同時に解消)。
	return c.JSON(http.StatusOK, packDriveFileSelfSingle(f, h.idGen))
}

// FileIDRequest is the body for show/delete and similar single-file ops.
type FileIDRequest struct {
	FileID string `json:"fileId"`
}

// FilesShow handles POST /api/drive/files/show.
//
// upstream paramDef は anyOf で {fileId} または {url} を受け、url 指定時は
// url / webpublicUrl / thumbnailUrl の OR 一致で検索する (#1564)。
func (h *Handler) FilesShow(c echo.Context) error {
	user := middleware.GetUser(c)
	// fileId / url は「キー存在」で anyOf を判定する (upstream の required は
	// presence のみで、`{"url":""}` は lookup に進んで 404 NO_SUCH_FILE になる)
	// (#1564 review)。fileId は format misskey:id なので空文字は INVALID_PARAM。
	var req struct {
		FileID *string `json:"fileId"`
		URL    *string `json:"url"`
	}
	if err := c.Bind(&req); err != nil || (req.FileID == nil && req.URL == nil) {
		return apierr.JSONInvalidParam(c)
	}
	var f *model.DriveFile
	var err error
	if req.FileID != nil {
		if *req.FileID == "" {
			return apierr.JSONInvalidParam(c)
		}
		f, err = h.svc.Show(user, *req.FileID)
	} else {
		f, err = h.svc.ShowByURL(user, *req.URL)
	}
	if err != nil {
		return mapFileError(c, err, fileEndpointShow)
	}
	return c.JSON(http.StatusOK, h.packDriveFileShow(f))
}

// packDriveFileShow is the drive/files/show pack: upstream は pack(file,
// {detail:true, withUser:true, self:true}) で、埋め込み folder を detail mode
// (foldersCount / filesCount / parent 付き)、user を UserLite で pack する
// (#1564)。folder / user は各 1 回だけ fetch する (full pack 経由の二重
// fetch を解消、#1564 review)。
func (h *Handler) packDriveFileShow(f *model.DriveFile) entity.DriveFileEntity {
	e := entity.PackDriveFileSelf(f, h.idGen)
	if h.folderRepo != nil && f.FolderID != nil {
		if fo, err := h.folderRepo.FindByID(*f.FolderID); err == nil {
			d := h.packDriveFolderDetail(fo)
			e.Folder = &d
		}
	}
	if h.userRepo != nil && f.UserID != nil {
		if u, err := h.userRepo.FindByID(*f.UserID); err == nil {
			lite := entity.PackUserLite(u)
			e.User = &lite
		}
	}
	return e
}

// FilesUpdateRequest is the body for drive/files/update.
type FilesUpdateRequest struct {
	FileID string  `json:"fileId"`
	Name   *string `json:"name"`
	// upstream update.ts paramDef は comment が nullable。JSON の `comment:null`
	// (= 明示クリア) と省略を区別するため json.RawMessage で受ける (#1769)。
	Comment json.RawMessage `json:"comment"`
	// folderId は upstream paramDef が nullable。`folderId: null` (= ルートへ
	// 移動) と省略を区別するため RawMessage で受ける。旧実装は *string で
	// 区別できず、mk-go 独自の unsetFolder フラグを足していた。
	FolderID    json.RawMessage `json:"folderId"`
	IsSensitive *bool           `json:"isSensitive"`
}

// decodeNullableID maps the 3 JSON states of a nullable id field onto the core
// layer's `**string`. ok=false は「キー省略 = 不変」、返り値 nil は
// 「null = 親を外す」、非 nil は「その id へ移動」。
func decodeNullableID(raw json.RawMessage) (*string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	if string(raw) == "null" {
		return nil, true
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	return &v, true
}

// FilesUpdate handles POST /api/drive/files/update.
func (h *Handler) FilesUpdate(c echo.Context) error {
	user := middleware.GetUser(c)
	var req FilesUpdateRequest
	if err := c.Bind(&req); err != nil || req.FileID == "" {
		return apierr.JSONInvalidParam(c)
	}
	in := coredrive.UpdateInput{
		Name:        req.Name,
		IsSensitive: req.IsSensitive,
	}
	if req.Comment != nil {
		if string(req.Comment) == "null" {
			// 明示 null → comment をクリア (upstream は comment:null で消せる、#1769)。
			var nilStr *string
			in.Comment = &nilStr
		} else {
			var comment string
			if err := json.Unmarshal(req.Comment, &comment); err != nil {
				return apierr.JSONInvalidParam(c)
			}
			// upstream update.ts paramDef は comment maxLength=512 (#1564)。
			if !colfit.Fits(comment, maxDriveCommentLength) {
				return apierr.JSONInvalidParam(c)
			}
			cp := &comment
			in.Comment = &cp
		}
	}
	if fid, ok := decodeNullableID(req.FolderID); ok {
		in.FolderID = &fid
	}

	f, err := h.svc.Update(user, req.FileID, in)
	if err != nil {
		return mapFileError(c, err, fileEndpointUpdate)
	}
	// upstream は updateFile 後 `pack(file.id, { self: true })` を返す
	// (= self single shape、DriveService.ts)。本実装も #812 の create と
	// 同じ self single helper を使う (folder / userId / user 全 null)。
	return c.JSON(http.StatusOK, packDriveFileSelfSingle(f, h.idGen))
}

// FilesDelete handles POST /api/drive/files/delete.
func (h *Handler) FilesDelete(c echo.Context) error {
	user := middleware.GetUser(c)
	var req FileIDRequest
	if err := c.Bind(&req); err != nil || req.FileID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if err := h.svc.Delete(user, req.FileID); err != nil {
		return mapFileError(c, err, fileEndpointDelete)
	}
	return c.NoContent(http.StatusNoContent)
}

// FilesFindByHashRequest is the body for drive/files/find-by-hash.
type FilesFindByHashRequest struct {
	MD5 string `json:"md5"`
}

// FilesFindByHash handles POST /api/drive/files/find-by-hash.
func (h *Handler) FilesFindByHash(c echo.Context) error {
	user := middleware.GetUser(c)
	var req FilesFindByHashRequest
	// upstream find-by-hash の paramDef は md5 {type:'string'} で空文字も valid
	// (required は presence のみ)。空 md5 は findBy で 0 件一致し 200 + [] を返す。
	// INVALID_PARAM で弾かない (#1831)。
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	files, err := h.svc.FindByHash(user, req.MD5)
	if err != nil {
		// FindByHash は #1564 以降 not-found を空 slice で返すため、ここに
		// 来るのは repo error のみ (= upstream も 500 相当)。
		return apierr.JSONInternalError(c)
	}
	// upstream は findBy({md5, userId}) の一致全件を `packMany(files,
	// { self: true })` で返す (#818 / #1564 で最新 1 件 → 全件に修正)。
	out := make([]entity.DriveFileEntity, 0, len(files))
	for _, f := range files {
		out = append(out, h.packDriveFileSelfList(f))
	}
	return c.JSON(http.StatusOK, out)
}

// FoldersCreateRequest is the body for drive/folders/create.
type FoldersCreateRequest struct {
	// upstream paramDef は name に default:'Untitled' を持つ。default は key 省略時
	// のみ適用され、明示的な空文字 "" はそのまま保持される (minLength 無し)。absent と
	// explicit "" を区別するため *string にする (#1948-16)。
	Name     *string `json:"name"`
	ParentID *string `json:"parentId"`
}

// FoldersCreate handles POST /api/drive/folders/create.
func (h *Handler) FoldersCreate(c echo.Context) error {
	user := middleware.GetUser(c)
	var req FoldersCreateRequest
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	// name 省略 (nil) のときだけ default 'Untitled' を適用する。explicit "" は
	// upstream paramDef (minLength 無し) と同じくそのまま保持する (#1948-16)。
	name := "Untitled"
	if req.Name != nil {
		name = *req.Name
	}
	// upstream folders/create paramDef は name maxLength=200 (#1564)。
	if !colfit.Fits(name, maxDriveFolderNameLength) {
		return apierr.JSONInvalidParam(c)
	}
	f, err := h.svc.CreateFolder(user, name, req.ParentID)
	if err != nil {
		return mapFolderError(c, err, folderEndpointCreate)
	}
	return c.JSON(http.StatusOK, entity.PackDriveFolder(f, h.idGen))
}

// FolderIDRequest is the body for show/delete folder ops.
type FolderIDRequest struct {
	FolderID string `json:"folderId"`
}

// FoldersShow handles POST /api/drive/folders/show.
//
// upstream Misskey TS は `pack(folder, { detail: true })` で foldersCount /
// filesCount / parent (recursive) を埋めて返す。本実装も同 shape に揃える
// (#845)。default mode (= create / update / find) は影響なし。
func (h *Handler) FoldersShow(c echo.Context) error {
	user := middleware.GetUser(c)
	var req FolderIDRequest
	if err := c.Bind(&req); err != nil || req.FolderID == "" {
		return apierr.JSONInvalidParam(c)
	}
	f, err := h.svc.ShowFolder(user, req.FolderID)
	if err != nil {
		return mapFolderError(c, err, folderEndpointShow)
	}
	return c.JSON(http.StatusOK, h.packDriveFolderDetail(f))
}

// packDriveFolderDetail packs a folder in upstream "detail" mode for
// drive/folders/show. Adds foldersCount / filesCount / parent (recursive
// detail pack) to the default 4 field. count は best-effort で取れない
// ときは 0 を返す (= upstream も Promise が reject されると 0 相当)。
//
// repository が wired されていない (= 旧 test handler) と count は 0、
// parent は nil。これは production runtime には影響しない (= router で
// 必ず wired される)。
func (h *Handler) packDriveFolderDetail(f *model.DriveFolder) entity.DriveFolderEntity {
	return h.packDriveFolderDetailGuarded(f, map[string]struct{}{})
}

// packDriveFolderDetailGuarded is the recursion body of packDriveFolderDetail.
// visited は parent 連鎖の循環 data guard (#1564 review): 旧 mk-go は folder
// 循環を作れてしまっていた (folders/update の RECURSIVE_NESTING 検証は #1564
// で追加) ため、既存の循環 data で parent 再帰が無限 loop → stack overflow
// でプロセスごと落ちるのを防ぐ。既出 folder に当たったら連鎖を打ち切る。
func (h *Handler) packDriveFolderDetailGuarded(f *model.DriveFolder, visited map[string]struct{}) entity.DriveFolderEntity {
	visited[f.ID] = struct{}{}
	e := entity.PackDriveFolder(f, h.idGen)
	foldersCount := 0
	filesCount := 0
	if h.folderRepo != nil {
		if n, err := h.folderRepo.CountChildFolders(f.ID); err != nil {
			// silent 0 を返すと真の DB error と空 state が区別できないので
			// 運用 debug 用に Warn で記録する。frontend には response が
			// 0 で返るので影響なし。
			slog.Warn("packDriveFolderDetail: CountChildFolders failed", "folderID", f.ID, "err", err)
		} else {
			foldersCount = n
		}
	}
	if h.fileRepo != nil {
		if n, err := h.fileRepo.CountByFolder(f.ID); err != nil {
			slog.Warn("packDriveFolderDetail: CountByFolder failed", "folderID", f.ID, "err", err)
		} else {
			filesCount = n
		}
	}
	e.FoldersCount = &foldersCount
	e.FilesCount = &filesCount
	if f.ParentID != nil && h.folderRepo != nil {
		if _, seen := visited[*f.ParentID]; seen {
			slog.Warn("packDriveFolderDetail: folder parent cycle detected, truncating", "folderID", f.ID, "parentID", *f.ParentID)
		} else if parent, err := h.folderRepo.FindByID(*f.ParentID); err == nil {
			parentPacked := h.packDriveFolderDetailGuarded(parent, visited)
			e.Parent = &parentPacked
		} else {
			// parent FindByID error は recursion 切断の原因。同上で Warn。
			slog.Warn("packDriveFolderDetail: parent FindByID failed", "folderID", f.ID, "parentID", *f.ParentID, "err", err)
		}
	}
	return e
}

// FoldersUpdateRequest is the body for drive/folders/update.
type FoldersUpdateRequest struct {
	FolderID string  `json:"folderId"`
	Name     *string `json:"name"`
	// parentId も files/update の folderId と同じく nullable。
	ParentID json.RawMessage `json:"parentId"`
}

// FoldersUpdate handles POST /api/drive/folders/update.
func (h *Handler) FoldersUpdate(c echo.Context) error {
	user := middleware.GetUser(c)
	var req FoldersUpdateRequest
	if err := c.Bind(&req); err != nil || req.FolderID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// upstream folders/update paramDef は name maxLength=200 (#1564)。
	if req.Name != nil && !colfit.Fits(*req.Name, maxDriveFolderNameLength) {
		return apierr.JSONInvalidParam(c)
	}
	in := coredrive.UpdateFolderInput{Name: req.Name}
	if pid, ok := decodeNullableID(req.ParentID); ok {
		in.ParentID = &pid
	}
	f, err := h.svc.UpdateFolder(user, req.FolderID, in)
	if err != nil {
		return mapFolderError(c, err, folderEndpointUpdate)
	}
	return c.JSON(http.StatusOK, entity.PackDriveFolder(f, h.idGen))
}

// FoldersDelete handles POST /api/drive/folders/delete.
func (h *Handler) FoldersDelete(c echo.Context) error {
	user := middleware.GetUser(c)
	var req FolderIDRequest
	if err := c.Bind(&req); err != nil || req.FolderID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if err := h.svc.DeleteFolder(user, req.FolderID); err != nil {
		return mapFolderError(c, err, folderEndpointDelete)
	}
	return c.NoContent(http.StatusNoContent)
}

// helpers

// fileEndpoint identifies which drive/files endpoint produced a file error, so
// mapFileError can return the upstream-canonical per-endpoint UUID. Misskey TS
// uses a distinct UUID per endpoint for NO_SUCH_FILE / ACCESS_DENIED (#1336)。
type fileEndpoint int

const (
	fileEndpointShow fileEndpoint = iota
	fileEndpointUpdate
	fileEndpointDelete
)

func mapFileError(c echo.Context, err error, ep fileEndpoint) error {
	switch {
	case errors.Is(err, coredrive.ErrFileNotFound):
		var id string
		switch ep {
		case fileEndpointShow:
			id = "067bc436-2718-4795-b0fb-ecbe43949e31"
		case fileEndpointUpdate:
			id = "e7778c7e-3af9-49cd-9690-6dbc3e6c972d"
		case fileEndpointDelete:
			id = "908939ec-e52b-4458-b395-1025195cea58"
		default:
			panic(fmt.Sprintf("mapFileError: unknown fileEndpoint %d", ep))
		}
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "No such file.", id))
	case errors.Is(err, coredrive.ErrFolderNotFound):
		// folder 不在は drive/files/update の parent 指定経路でのみ発生する。
		id := "1069098f-c281-440f-b085-f9932edbe091"
		if ep == fileEndpointUpdate {
			id = "ea8fb7a5-af77-4a08-b608-c0218176cd73"
		}
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FOLDER", "No such folder.", id))
	case errors.Is(err, coredrive.ErrAccessDenied):
		var id string
		switch ep {
		case fileEndpointShow:
			id = "25b73c73-68b1-41d0-bad1-381cfdf6579f"
		case fileEndpointUpdate:
			id = "01a53b27-82fc-445b-a0c1-b558465a8ed2"
		case fileEndpointDelete:
			id = "5eb8d909-2540-4970-90b8-dd6f86088121"
		default:
			panic(fmt.Sprintf("mapFileError: unknown fileEndpoint %d", ep))
		}
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", id))
	case errors.Is(err, coredrive.ErrInvalidFileName):
		// upstream drive/files/update の invalidFileName (#1564)。create は
		// handler 内で別 UUID (f449b209-...) を直接返すためここには来ない。
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_FILE_NAME", "Invalid file name.", "395e7156-f9f0-475e-af89-53c3c23080c2"))
	case errors.Is(err, coredrive.ErrCannotUnmarkSensitive):
		// upstream drive/files/update の restrictedByRole は i/update と別 UUID
		// (7f59dccb-...)。endpoint 単位で frontend が i18n 引きするので、
		// upstream の per-endpoint UUID をそのまま使う (#1028)。
		return c.JSON(http.StatusBadRequest, apierr.Error("RESTRICTED_BY_ROLE",
			"This feature is restricted by your role.",
			"7f59dccb-f465-75ab-5cf4-3ce44e3282f7"))
	}
	return apierr.JSONInternalError(c)
}

// folderEndpoint identifies which drive/folders endpoint produced a folder
// error, so that mapFolderError can return the upstream-canonical UUID for
// NO_SUCH_FOLDER. Misskey TS uses a distinct UUID per endpoint (#977):
//   - create: 53326628-a00d-40a6-a3cd-8975105c0f95 (parent not found)
//   - show:   d74ab9eb-bb09-4bba-bf24-fb58f761e1e9
//   - update: f7974dac-2c0d-4a27-926e-23583b28e98e (target not found)
//   - delete: 1069098f-c281-440f-b085-f9932edbe091
type folderEndpoint int

const (
	folderEndpointCreate folderEndpoint = iota
	folderEndpointShow
	folderEndpointUpdate
	folderEndpointDelete
)

func mapFolderError(c echo.Context, err error, ep folderEndpoint) error {
	switch {
	case errors.Is(err, coredrive.ErrFolderNotFound):
		var id string
		switch ep {
		case folderEndpointCreate:
			id = "53326628-a00d-40a6-a3cd-8975105c0f95"
		case folderEndpointShow:
			id = "d74ab9eb-bb09-4bba-bf24-fb58f761e1e9"
		case folderEndpointUpdate:
			id = "f7974dac-2c0d-4a27-926e-23583b28e98e"
		case folderEndpointDelete:
			id = "1069098f-c281-440f-b085-f9932edbe091"
		default:
			// 将来 endpoint を追加して switch case を漏らすと UUID drift で
			// frontend / spec が壊れるので fail-fast する。
			panic(fmt.Sprintf("mapFolderError: unknown folderEndpoint %d", ep))
		}
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FOLDER", "No such folder.", id))
	case errors.Is(err, coredrive.ErrParentFolderNotFound):
		// folders/update のみ NO_SUCH_PARENT_FOLDER を区別する。
		// folders/create の parent 不在は upstream も NO_SUCH_FOLDER 扱い
		// (ErrParentFolderNotFound はそもそも CreateFolder では返らない)。
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_PARENT_FOLDER", "No such parent folder.", "ce104e3a-faaf-49d5-b459-10ff0cbbcaa1"))
	case errors.Is(err, coredrive.ErrAccessDenied):
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "fe8d7103-0ea8-4ec3-814d-f8b401dc69e9"))
	case errors.Is(err, coredrive.ErrFolderNotEmpty):
		return c.JSON(http.StatusBadRequest, apierr.Error("HAS_CHILD_FILES_OR_FOLDERS", "Folder is not empty.", "b0fc8a17-963c-405d-bfbc-859a487295e1"))
	case errors.Is(err, coredrive.ErrRecursiveNesting):
		// upstream folders/update の recursiveNesting (#1564)。id はダッシュ
		// 無しの upstream 原文 (i/2fa の typo UUID と同じく矯正しない)。
		return c.JSON(http.StatusBadRequest, apierr.Error("RECURSIVE_NESTING", "It can not be structured like nesting folders recursively.", "dbeb024837894013aed44279f9199740"))
	}
	return apierr.JSONInternalError(c)
}

// --- Drive listing endpoints ---

// Usage handles POST /api/drive — returns usage summary.
func (h *Handler) Usage(c echo.Context) error {
	user := middleware.GetUser(c)
	var usage int64
	if h.fileRepo != nil {
		usage, _ = h.fileRepo.UsageByUser(user.ID)
	}
	// upstream drive.ts:49-53 は driveCapacityMb role policy から capacity を
	// 算出する (1024*1024*driveCapacityMb)。以前は 1GiB ハードコードで policy /
	// role 上書きを無視していた (#1769)。
	return c.JSON(http.StatusOK, map[string]any{
		"capacity": h.svc.Capacity(user.ID),
		"usage":    usage,
	})
}

// FilesList handles POST /api/drive/files — lists user's files.
// emptyFolderIDToNil normalizes an empty-string folderId pointer to nil so the
// repository treats it as "root" (folderId IS NULL) instead of filtering on the
// literal empty string. #2106 L14: upstream は falsy folderId を root 扱いし、
// 空文字 "" は misskey:id format 違反として扱う。mk-go は "" を root に正規化する。
func emptyFolderIDToNil(s *string) *string {
	if s != nil && *s == "" {
		return nil
	}
	return s
}

func (h *Handler) FilesList(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Limit     *int    `json:"limit"`
		SinceID   string  `json:"sinceId"`
		UntilID   string  `json:"untilId"`
		SinceDate *int64  `json:"sinceDate"`
		UntilDate *int64  `json:"untilDate"`
		FolderID  *string `json:"folderId"`
		Type      string  `json:"type"`
		Sort      string  `json:"sort"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	// upstream paramDef の sort は enum、type は pattern (#1564)。違反は
	// ajv が INVALID_PARAM で弾くので同じく reject する。
	if !isValidDriveSort(req.Sort) {
		return apierr.JSONInvalidParam(c)
	}
	if req.Type != "" && !driveTypePatternRe.MatchString(req.Type) {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	if h.fileRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	files, err := h.fileRepo.ListByUser(user.ID, emptyFolderIDToNil(req.FolderID), false, req.Type, req.Sort, untilID, sinceID, limit)
	if err != nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// upstream files.ts は packMany(files, {detail:false, self:true}) =
	// folder/user null, userId は owner ID (#1564)。
	out := make([]entity.DriveFileEntity, 0, len(files))
	for _, f := range files {
		out = append(out, h.packDriveFileSelfList(f))
	}
	return c.JSON(http.StatusOK, out)
}

// isValidDriveSort reports whether v matches upstream drive/files.ts
// `sort: enum ['+createdAt','-createdAt','+name','-name','+size','-size', null]`.
func isValidDriveSort(v string) bool {
	switch v {
	case "", "+createdAt", "-createdAt", "+name", "-name", "+size", "-size":
		return true
	}
	return false
}

// FilesFind handles POST /api/drive/files/find — search by name.
func (h *Handler) FilesFind(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Name     string  `json:"name"`
		FolderID *string `json:"folderId"`
	}
	// upstream files/find の paramDef は name {type:'string'} で空文字も valid。
	// 空 name は findBy で 0 件一致し 200 + [] を返す (#1831)。
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	if h.fileRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	files, err := h.fileRepo.FindByName(user.ID, req.Name, emptyFolderIDToNil(req.FolderID))
	if err != nil {
		return c.JSON(http.StatusOK, []any{})
	}
	out := make([]entity.DriveFileEntity, 0, len(files))
	for _, f := range files {
		// upstream `packMany(files, { self: true })` 経路 (#818)。
		out = append(out, h.packDriveFileSelfList(f))
	}
	return c.JSON(http.StatusOK, out)
}

// FilesCheckExistence handles POST /api/drive/files/check-existence.
func (h *Handler) FilesCheckExistence(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		MD5 string `json:"md5"`
	}
	if err := c.Bind(&req); err != nil || req.MD5 == "" {
		return apierr.JSONInvalidParam(c)
	}
	if h.fileRepo == nil {
		return c.JSON(http.StatusOK, false)
	}
	exists, _ := h.fileRepo.ExistsByMD5(user.ID, req.MD5)
	return c.JSON(http.StatusOK, exists)
}

// FilesAttachedNotes handles POST /api/drive/files/attached-notes.
//
// frontend Paginator (cursor mode) は { sinceId / untilId / limit } を投げて
// くる。upstream Misskey TS と同じく cursor で keyset pagination する。
// 過去はページング非対応で同じ note を返し続ける無限スクロール bug が
// 出ていた (#488)。
//
// 旧実装は fileId を viewer 所有か検証せず直に `noteRepo.ListByFileID` を
// 叩いていたため、認証 viewer が他人 owner の fileID を投げて当該 file を
// attach した followers / specified note を本文ごと列挙できる IDOR が
// 成立していた (#1470)。upstream TS と同じく viewer 所有 file のみ許可し、
// 不存在 / owner mismatch / system file (userId IS NULL) はいずれも
// NO_SUCH_FILE で隠蔽する。`fileRepo` 未配線も同じく fail-closed。
//
// owner check 通過後の note は upstream `notes/create` の `checkFileIDs`
// が「file は author 所有」を強制するため必然的に viewer 自身の note で
// あり、CanSeeNote は本人 short-circuit で常に true (= 追加の
// FilterVisible が無くても leak しない)。
func (h *Handler) FilesAttachedNotes(c echo.Context) error {
	viewer := middleware.GetUser(c)
	var req struct {
		FileID    string `json:"fileId"`
		Limit     *int   `json:"limit"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
	}
	if err := c.Bind(&req); err != nil || req.FileID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// upstream Misskey TS の attached-notes.ts と同じ semantics: viewer 所有
	// file のみ許可 (#1470 IDOR fix)。moderator は upstream と同じく任意
	// user の file を照会できる (#1564、userId filter を外す)。upstream 専用
	// UUID `c118ece3-2e4b-4296-99d1-51756e32d232` を使い、汎用 NoSuchFile
	// (UUIDNoSuchFile) と区別する。
	if h.fileRepo == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "No such file.", "c118ece3-2e4b-4296-99d1-51756e32d232"))
	}
	// owner 判定を先に行い、moderator role lookup は非 owner のときだけ払う
	// (lazy 評価、#1564 review)。moderator 判定は core 側の RoleChecker
	// (svc.IsModerator) に一本化し、wiring を二重化しない。
	f, err := h.fileRepo.FindByID(req.FileID)
	viewerIsOwner := err == nil && f.UserID != nil && *f.UserID == viewer.ID
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil || (!viewerIsOwner && !h.svc.IsModerator(viewer.ID)) {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "No such file.", "c118ece3-2e4b-4296-99d1-51756e32d232"))
	}
	// fileIds配列にfileIDを含むノートを検索 (PostgreSQL配列演算子)
	if h.noteRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	notes, err := h.noteRepo.ListByFileID(req.FileID, sinceID, untilID, limit)
	if err != nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// moderator が他人 file を照会する経路 (#1564) では note が viewer 自身の
	// ものとは限らないため、可視性 filter を通す (followingRepo=nil の
	// fail-closed: followers note は author 本人以外に出さない)。TS は
	// pack 側の hide で本文を null 化するが、mk-go は drop で安全側に倒す。
	// owner 経路は従来どおり filter 不要 (notes/create の checkFileIDs により
	// 必然的に viewer 自身の note のみ)。
	if !viewerIsOwner {
		notes = notesfilter.FilterVisible(viewer, notes, nil)
	}
	notes = notesfilter.ApplyHardMute(h.userRepo, viewer, notes)
	out := entity.PackNotes(c.Request().Context(), notes, h.idGen, h.instanceLookup(), h.emojiLookup(), h.reactionReader())
	h.fieldRes.Apply(out, viewer)
	notehide.HideEmbeds(viewer, out)
	return c.JSON(http.StatusOK, out)
}

// FilesUploadFromURL handles POST /api/drive/files/upload-from-url.
//
// upstream Misskey TS drive/files/upload-from-url と同 semantics: URL を指定して
// サーバーに非同期でファイルを download させる。endpoint は即 204 を返し、
// download / 保存はバックグラウンドで実行され、完了後に user の main stream へ
// `urlUploadFinished` ({marker, file}) を publish する (#1217)。
func (h *Handler) FilesUploadFromURL(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		URL         string  `json:"url"`
		FolderID    *string `json:"folderId"`
		IsSensitive bool    `json:"isSensitive"`
		Comment     *string `json:"comment"`
		Marker      *string `json:"marker"`
		Force       bool    `json:"force"`
	}
	if err := c.Bind(&req); err != nil || req.URL == "" {
		return apierr.JSONInvalidParam(c)
	}
	// upstream paramDef は comment maxLength=512。ajv の maxLength は文字数
	// (code unit) 基準なので byte 長ではなく rune 数で判定する (多バイト文字を
	// 過剰に弾かないため)。超過は INVALID_PARAM。
	if req.Comment != nil && !colfit.Fits(*req.Comment, maxDriveCommentLength) {
		return apierr.JSONInvalidParam(c)
	}
	// uploader 未配線 (= 単体テスト等) は upstream と同じ空レスポンスで返す。
	if h.urlUploader == nil || user == nil {
		return c.NoContent(http.StatusNoContent)
	}
	// upstream は fire-and-forget。request context は応答後に cancel されるため
	// download はバックグラウンド context で継続する。
	in := URLUploadInput{
		User:        user,
		URL:         req.URL,
		FolderID:    req.FolderID,
		Comment:     req.Comment,
		Marker:      req.Marker,
		IsSensitive: req.IsSensitive,
		Force:       req.Force,
		RequestIP:   requestIPValue(c),
	}
	go h.urlUploader.Process(context.Background(), in)
	return c.NoContent(http.StatusNoContent)
}

// FilesMoveBulk handles POST /api/drive/files/move-bulk.
func (h *Handler) FilesMoveBulk(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		FileIDs  []string `json:"fileIds"`
		FolderID *string  `json:"folderId"`
	}
	if err := c.Bind(&req); err != nil || len(req.FileIDs) == 0 {
		return apierr.JSONInvalidParam(c)
	}
	// upstream move-bulk.ts paramDef は fileIds に minItems:1 / maxItems:100 /
	// uniqueItems:true を課す。100 超過・重複は INVALID_PARAM で reject する (#1948-16)。
	if len(req.FileIDs) > 100 {
		return apierr.JSONInvalidParam(c)
	}
	seen := make(map[string]struct{}, len(req.FileIDs))
	for _, id := range req.FileIDs {
		if _, dup := seen[id]; dup {
			return apierr.JSONInvalidParam(c)
		}
		seen[id] = struct{}{}
	}
	// 空文字 folderId は root(null) と同義に正規化する (upstream の
	// `folderId ?? null` 相当、#parity review DRV-2)。
	if req.FolderID != nil && *req.FolderID == "" {
		req.FolderID = nil
	}
	// 宛先 folder が指定されている場合は呼出ユーザー所有であることを検証する
	// (他人の folder へ移動できる経路を塞ぐ)。root (folderId=null) は検証不要。
	// folderRepo 未配線時は検証不能なので fail-closed で弾く (#parity review DRV-1)。
	if req.FolderID != nil {
		if h.folderRepo == nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FOLDER", "No such folder.", "d77545ec-1283-4b73-bbe1-e90e1da6a4e7"))
		}
		folder, err := h.folderRepo.FindByID(*req.FolderID)
		if err != nil && !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return apierr.JSONInternalError(c)
		}
		if err != nil || folder.UserID == nil || *folder.UserID != user.ID {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FOLDER", "No such folder.", "d77545ec-1283-4b73-bbe1-e90e1da6a4e7"))
		}
	}
	if h.fileRepo != nil {
		// userId scope で IDOR を防ぐ (他人の DriveFile は移動されない)。
		_ = h.fileRepo.UpdateBulkFolder(user.ID, req.FileIDs, req.FolderID)
	}
	return c.NoContent(http.StatusNoContent)
}

// Stream handles POST /api/drive/stream — lists all files (unfoldered).
func (h *Handler) Stream(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Limit     *int   `json:"limit"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
		Type      string `json:"type"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	// upstream stream.ts paramDef の type pattern (#1564)。
	if req.Type != "" && !driveTypePatternRe.MatchString(req.Type) {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	if h.fileRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	// upstream stream.ts は folder 条件を付けず全 folder 横断 (#1564)。
	files, err := h.fileRepo.ListByUser(user.ID, nil, true, req.Type, "", untilID, sinceID, limit)
	if err != nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// upstream stream.ts も packMany(files, {detail:false, self:true}) (#1564)。
	out := make([]entity.DriveFileEntity, 0, len(files))
	for _, f := range files {
		out = append(out, h.packDriveFileSelfList(f))
	}
	return c.JSON(http.StatusOK, out)
}

// FoldersList handles POST /api/drive/folders — lists user's folders.
func (h *Handler) FoldersList(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Limit     *int    `json:"limit"`
		SinceID   string  `json:"sinceId"`
		UntilID   string  `json:"untilId"`
		SinceDate *int64  `json:"sinceDate"`
		UntilDate *int64  `json:"untilDate"`
		FolderID  *string `json:"folderId"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	if h.folderRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	folders, err := h.folderRepo.ListByUser(user.ID, emptyFolderIDToNil(req.FolderID), untilID, sinceID, limit)
	if err != nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// upstream folders.ts は pack(folder) を返すため createdAt を含む
	// (#1564 で手組み 3 field map から修正)。
	out := make([]entity.DriveFolderEntity, 0, len(folders))
	for _, f := range folders {
		out = append(out, entity.PackDriveFolder(f, h.idGen))
	}
	return c.JSON(http.StatusOK, out)
}

// FoldersFind handles POST /api/drive/folders/find — search by name.
func (h *Handler) FoldersFind(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Name     string  `json:"name"`
		ParentID *string `json:"parentId"`
	}
	// upstream folders/find の paramDef は name {type:'string'} で空文字も valid。
	// 空 name は findBy で 0 件一致し 200 + [] を返す (#1831)。
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	if h.folderRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	folders, err := h.folderRepo.FindByName(user.ID, req.Name, req.ParentID)
	if err != nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// upstream folders/find.ts も pack(folder) を返すため createdAt を含む
	// (#1564)。
	out := make([]entity.DriveFolderEntity, 0, len(folders))
	for _, f := range folders {
		out = append(out, entity.PackDriveFolder(f, h.idGen))
	}
	return c.JSON(http.StatusOK, out)
}

// requestIPFromContext extracts the client IP for drive upload provenance
// tracking (admin/drive/show-file の IP タブで参照、#1148)。echo の RealIP
// は X-Forwarded-For / X-Real-IP を考慮した resolved IP を返すので、nginx
// 等の reverse proxy 配下でも本物の client IP を取得できる。空文字なら
// nil を返して `requestIp` column を NULL のままにする。
// requestIPValue is requestIPFromContext as a plain string ("" = 記録しない)。
func requestIPValue(c echo.Context) string {
	if ip := requestIPFromContext(c); ip != nil {
		return *ip
	}
	return ""
}

func requestIPFromContext(c echo.Context) *string {
	// **プロセス内 API 呼び出しの IP は記録しない。** `pluginCaller.Call` は
	// `RemoteAddr` に `127.0.0.1:0` を置く (IP を見る middleware が解釈に
	// 困らないようにするため) が、その IP は実在しない。記録すると、
	// モデレーターが `admin/drive/show-file` で見る値が実際の取得元と無関係な
	// `127.0.0.1` になる (#3130 の `user_ip` / レート制限と同じ汚染)。
	if middleware.IsInternalCall(c.Request().Context()) {
		return nil
	}
	ip := c.RealIP()
	if ip == "" {
		return nil
	}
	return &ip
}

// sensitiveRequestHeaders は drive_file.requestHeaders に保存しない header
// 名 (小文字 canonical) の deny-list。mk-go 独自 hardening として、upstream
// Misskey TS が全 header を生のまま保存する設計から divergence する (#1148)。
//
// 根拠: drive_file.requestHeaders は admin の moderation 用途で参照される
// (admin/drive/show-file の IP タブ) が、`Authorization` 等の credential
// header を保存すると DB dump 流出時に user の Bearer token / session
// cookie が芋づる式に漏れる。moderation 価値より credential 流出 risk が
// 上回るので保存時点で除外する。upstream で同 hardening を入れる PR は
// shiroha-a/misskey-ts fork 経由で中長期検討。
var sensitiveRequestHeaders = map[string]struct{}{
	"authorization":       {},
	"cookie":              {},
	"set-cookie":          {},
	"x-api-key":           {},
	"api-key":             {},
	"proxy-authorization": {},
}

// requestHeadersForDrive snapshots the inbound HTTP request headers into a
// jsonb blob stored on drive_file.requestHeaders (#1148)。`sensitiveRequest
// Headers` deny-list で credential 系を除外。`map[string]string` で保存する
// (慣習として 1 要素 header が大半、複数値の場合は最初を採用)。空 header
// set なら nil を返して列を NULL のままにする。
func requestHeadersForDrive(c echo.Context) datatypes.JSON {
	req := c.Request()
	if req == nil || len(req.Header) == 0 {
		return nil
	}
	out := make(map[string]string, len(req.Header))
	for k, vs := range req.Header {
		// HTTP header 名は case-insensitive なので canonical な小文字で
		// deny-list と比較する。Go の http.Header は MIME canonical
		// (= "Authorization" 等の Title-Case) で正規化済なので、こちらは
		// 単純に ToLower で揃える。
		if _, deny := sensitiveRequestHeaders[strings.ToLower(k)]; deny {
			continue
		}
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	if len(out) == 0 {
		return nil
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return datatypes.JSON(raw)
}

// userIDForLog returns a loggable user id, tolerating a nil user (system paths).
func userIDForLog(u *model.User) string {
	if u == nil {
		return ""
	}
	return u.ID
}
