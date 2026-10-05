package drive

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/elythia-network/elythia/internal/api/apierr"
	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// Error codes for the chunked upload endpoints (#2313).
//
// 分割アップロードは Misskey 本家に存在しない mk-go 独自拡張なので、upstream と
// 一致させるべき code / UUID は無い。UUID はここで発番して固定し、フロントエンド
// (#2314) がこの組で分岐できるようにする。
const (
	uuidChunkedUploadUnavailable = "0e2bd4a2-2bd4-4a1f-8d15-2f1a5a0d0c31"
	uuidChunkedUploadNotAllowed  = "7f1cba7f-6a0f-4b1e-9a2e-3f0f8c0b6d02"
	uuidTooManyUploadSessions    = "a4a3a9a0-3a1c-4b6e-8c7d-9f0a1b2c3d44"
	uuidPendingUploadLimit       = "b5e0c3e1-4f2d-4c85-9a3b-0d1e2f3a4b55"
	uuidNoSuchUploadSession      = "c6f1d4f2-5a3e-4d96-8b4c-1e2f3a4b5c66"
	uuidUploadSessionBusy        = "d70283a3-6b4f-4ea7-9c5d-2f3a4b5c6d77"
	uuidInvalidChunkIndex        = "e81394b4-7c50-4fb8-ad6e-3a4b5c6d7e88"
	uuidInvalidChunkSize         = "f924a5c5-8d61-40c9-be7f-4b5c6d7e8f99"
	uuidChunkContentMismatch     = "0a35b6d6-9e72-41da-8f80-5c6d7e8f90aa"
	uuidIncompleteUpload         = "1b46c7e7-af83-42eb-9091-6d7e8f90a1bb"
	uuidInvalidUploadSize        = "2c57d8f8-b094-43fc-a1a2-7e8f90a1b2cc"
)

// ChunkedStartRequest is the body of drive/files/create-chunked/start.
type ChunkedStartRequest struct {
	Name string `json:"name"`
	// Size is the total byte length the client intends to upload. It is
	// validated against every quota up front and bounds what append accepts;
	// the real size is re-derived from the assembled object at finish.
	Size        int64   `json:"size"`
	Comment     *string `json:"comment"`
	FolderID    *string `json:"folderId"`
	IsSensitive bool    `json:"isSensitive"`
	Force       bool    `json:"force"`
}

// ChunkedSessionRequest identifies a session for finish / abort.
type ChunkedSessionRequest struct {
	UploadID string `json:"uploadId"`
}

// FilesCreateChunkedStart handles POST /api/drive/files/create-chunked/start.
func (h *Handler) FilesCreateChunkedStart(c echo.Context) error {
	user := middleware.GetUser(c)

	var req ChunkedStartRequest
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}

	// name の扱いは drive/files/create と揃える (#1564)。分割経路だけ別の
	// 正規化をすると同じファイルでも保存名が変わってしまう。
	name := strings.TrimSpace(req.Name)
	if name == "" || name == "blob" {
		name = "untitled"
	} else if !coredrive.ValidateFileName(name) {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_FILE_NAME", "Invalid file name.", "f449b209-0c60-4e51-84d5-29486263bfd4"))
	}
	if req.Comment != nil && utf8.RuneCountInString(*req.Comment) > maxDriveCommentLength {
		return apierr.JSONInvalidParam(c)
	}

	in := coredrive.StartChunkedUploadInput{
		User:        user,
		Name:        name,
		Size:        req.Size,
		Comment:     req.Comment,
		FolderID:    req.FolderID,
		IsSensitive: req.IsSensitive,
		Force:       req.Force,
	}
	// requestIp / requestHeaders は create と同じく meta.enableIpLogging が
	// 有効なときだけ記録する (#2106 S4)。start の時点で拾って session に
	// 持たせる — finish のリクエストは同じ端末から来るとは限らない。
	if h.metaRepo != nil {
		if m, err := h.metaRepo.Fetch(); err == nil && m != nil && m.EnableIPLogging {
			in.RequestIP = requestIPFromContext(c)
			in.RequestHeaders = requestHeadersForDrive(c)
		}
	}

	sess, err := h.svc.StartChunkedUpload(c.Request().Context(), in)
	if err != nil {
		return h.chunkedError(c, err)
	}
	// totalChunks はクライアントが進捗を出すための派生値。chunkSize は
	// セッションに固定されているので、これ以降 admin が設定を変えても
	// 進行中のアップロードには影響しない。
	totalChunks := (sess.TotalSize + sess.ChunkSize - 1) / sess.ChunkSize
	return c.JSON(http.StatusOK, map[string]any{
		"uploadId":    sess.ID,
		"chunkSize":   sess.ChunkSize,
		"totalChunks": totalChunks,
		"expiresAt":   sess.ExpiresAt,
	})
}

// FilesCreateChunkedAppend handles POST /api/drive/files/create-chunked/append.
//
// multipart/form-data で uploadId / index / chunk を受ける。create と同じ形に
// 揃えているので、フロントエンドは既存の XHR 送信をほぼそのまま使える。
func (h *Handler) FilesCreateChunkedAppend(c echo.Context) error {
	user := middleware.GetUser(c)

	uploadID := c.FormValue("uploadId")
	if uploadID == "" {
		return apierr.JSONInvalidParam(c)
	}
	index, err := strconv.Atoi(c.FormValue("index"))
	if err != nil || index < 0 {
		return apierr.JSONInvalidParam(c)
	}
	// **セッションの上限を読む前に引く (#3037 レビュー)。** `AppendChunk` は
	// `size > sess.ChunkSize` を拒否するが、そこへ届く時点でチャンクは全部
	// メモリに載っている。body limit はこの経路で 33MiB なので、既定 10MiB の
	// セッションでも 1 リクエストあたり 33MiB を確保させられていた。
	maxChunk, _ := h.svc.SessionChunkSize(user, uploadID)
	chunk, err := readMultipartChunk(c, maxChunk)
	if err != nil {
		if errors.Is(err, coredrive.ErrInvalidChunkSize) {
			return h.chunkedError(c, err)
		}
		return apierr.JSONInvalidParam(c)
	}

	res, err := h.svc.AppendChunk(c.Request().Context(), user, uploadID, index, chunk)
	if err != nil {
		return h.chunkedError(c, err)
	}
	return c.JSON(http.StatusOK, res)
}

// FilesCreateChunkedFinish handles POST /api/drive/files/create-chunked/finish.
// レスポンスは drive/files/create と同一 shape (pack(self)) にする。
func (h *Handler) FilesCreateChunkedFinish(c echo.Context) error {
	user := middleware.GetUser(c)

	var req ChunkedSessionRequest
	if err := c.Bind(&req); err != nil || req.UploadID == "" {
		return apierr.JSONInvalidParam(c)
	}
	f, err := h.svc.FinishChunkedUpload(c.Request().Context(), user, req.UploadID)
	if err != nil {
		return h.chunkedError(c, err)
	}
	return c.JSON(http.StatusOK, packDriveFileSelfSingle(f, h.idGen))
}

// FilesCreateChunkedAbort handles POST /api/drive/files/create-chunked/abort.
func (h *Handler) FilesCreateChunkedAbort(c echo.Context) error {
	user := middleware.GetUser(c)

	var req ChunkedSessionRequest
	if err := c.Bind(&req); err != nil || req.UploadID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if err := h.svc.AbortChunkedUpload(c.Request().Context(), user, req.UploadID); err != nil {
		return h.chunkedError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// readMultipartChunk extracts the chunk bytes from the "chunk" form field.
// テスト用に差し替え可能にするのは readMultipartFile と同じ理由。
var readMultipartChunk = func(c echo.Context, maxBytes int64) ([]byte, error) {
	fileHeader, err := c.FormFile("chunk")
	if err != nil {
		return nil, err
	}
	// 申告が上限を超えていれば開かない (`readMultipartFile` と同じ形)。
	if maxBytes > 0 && fileHeader.Size > maxBytes {
		return nil, coredrive.ErrInvalidChunkSize
	}
	// `openMultipartFile` を通す (`readMultipartFile` と同じ差し替え口)。
	src, err := openMultipartFile(fileHeader)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	body, err := readAtMost(src, maxBytes)
	if err != nil {
		if errors.Is(err, coredrive.ErrMaxFileSizeExceeded) {
			// **チャンクの上限超過は `ErrInvalidChunkSize`。**
			// `AppendChunk` が同じ条件で返すエラーに揃える。
			return nil, coredrive.ErrInvalidChunkSize
		}
		return nil, err
	}
	return body, nil
}

// chunkedError maps core errors to Misskey-shaped responses.
//
// セッションの不存在・他人所有・期限切れはすべて NO_SUCH_UPLOAD_SESSION に
// 畳む。区別すると他人のセッション ID の存在を確かめる oracle になる。
func (h *Handler) chunkedError(c echo.Context, err error) error {
	// index 不一致は期待値を info で返す。クライアントは応答の消失と送信の
	// 失敗を区別できないため、これが無いと再開位置を決められない。
	var idxErr *coredrive.ChunkIndexError
	if errors.As(err, &idxErr) {
		body := apierr.Error("INVALID_CHUNK_INDEX", "Unexpected chunk index.", uuidInvalidChunkIndex)
		body["error"].(map[string]any)["info"] = map[string]any{"expected": idxErr.Expected}
		return c.JSON(http.StatusConflict, body)
	}

	switch {
	case errors.Is(err, coredrive.ErrChunkedUploadUnavailable):
		return c.JSON(http.StatusBadRequest, apierr.Error("CHUNKED_UPLOAD_UNAVAILABLE",
			"Chunked upload is not available on this instance.", uuidChunkedUploadUnavailable))
	case errors.Is(err, coredrive.ErrChunkedUploadNotAllowed):
		return c.JSON(http.StatusForbidden, apierr.ErrorWithKind("CHUNKED_UPLOAD_NOT_ALLOWED",
			"You are not allowed to use chunked upload.", uuidChunkedUploadNotAllowed, apierr.KindPermission))
	case errors.Is(err, coredrive.ErrTooManyUploadSessions):
		return c.JSON(http.StatusTooManyRequests, apierr.Error("TOO_MANY_UPLOAD_SESSIONS",
			"Too many concurrent upload sessions.", uuidTooManyUploadSessions))
	case errors.Is(err, coredrive.ErrPendingUploadLimitExceeded):
		return c.JSON(http.StatusTooManyRequests, apierr.Error("PENDING_UPLOAD_LIMIT_EXCEEDED",
			"Too many bytes pending in unfinished uploads.", uuidPendingUploadLimit))
	case errors.Is(err, coredrive.ErrUploadSessionNotFound):
		return c.JSON(http.StatusNotFound, apierr.Error("NO_SUCH_UPLOAD_SESSION",
			"No such upload session.", uuidNoSuchUploadSession))
	case errors.Is(err, coredrive.ErrUploadSessionBusy):
		return c.JSON(http.StatusConflict, apierr.Error("UPLOAD_SESSION_BUSY",
			"The upload session is being modified by another request.", uuidUploadSessionBusy))
	case errors.Is(err, coredrive.ErrInvalidChunkSize):
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_CHUNK_SIZE",
			"Invalid chunk size.", uuidInvalidChunkSize))
	case errors.Is(err, coredrive.ErrChunkContentMismatch):
		return c.JSON(http.StatusConflict, apierr.Error("CHUNK_CONTENT_MISMATCH",
			"The re-sent chunk differs from the one already stored.", uuidChunkContentMismatch))
	case errors.Is(err, coredrive.ErrIncompleteUpload):
		return c.JSON(http.StatusBadRequest, apierr.Error("INCOMPLETE_UPLOAD",
			"The upload is not complete.", uuidIncompleteUpload))
	case errors.Is(err, coredrive.ErrInvalidUploadSize):
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_UPLOAD_SIZE",
			"Invalid upload size.", uuidInvalidUploadSize))

	// 以降は drive/files/create と同じ code / UUID を返す。同じ制約に当たった
	// ときに経路で error が変わるとフロントエンドが分岐を二重に持つことになる。
	case errors.Is(err, coredrive.ErrFolderNotFound):
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FOLDER", "No such folder.", "d77545ec-1283-4b73-bbe1-e90e1da6a4e7"))
	case errors.Is(err, coredrive.ErrUnallowedFileType):
		return c.JSON(http.StatusBadRequest, apierr.Error("UNALLOWED_FILE_TYPE",
			"Cannot upload the file because it is an unallowed file type.",
			"4becd248-7f2c-48c4-a9f0-75edc4f9a1ea"))
	// **`ErrUndecodableImage` も 413 に寄せる (#3037 レビュー 2 周目)。**
	// 新しい wire コードを足すとフロントエンドに分岐が無く汎用の失敗になる。
	// 実際「大きすぎて安全に処理できない」なので意味も近い。
	case errors.Is(err, coredrive.ErrMaxFileSizeExceeded), errors.Is(err, coredrive.ErrUndecodableImage):
		return c.JSON(http.StatusRequestEntityTooLarge, apierr.Error("MAX_FILE_SIZE_EXCEEDED", "Max file size exceeded.", "b9d8c348-33f0-4673-b9a9-5d4da058977a"))
	case errors.Is(err, coredrive.ErrNoFreeSpace):
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_FREE_SPACE", "No free space.", "d08dbc37-a6a9-463a-8c47-96c32ab5f064"))
	case errors.Is(err, coredrive.ErrAccessDenied):
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "fe8d7103-0ea8-4ec3-814d-f8b401dc69e9"))
	}
	return apierr.JSONInternalError(c)
}
