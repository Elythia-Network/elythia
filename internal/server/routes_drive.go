package server

import (
	apichat "github.com/elythia-network/elythia/internal/api/chat"
	"github.com/elythia-network/elythia/internal/api/drive"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// registerDriveRoutes registers the drive routes on the /api group.
func registerDriveRoutes(api apiRoutes, driveHandler *drive.Handler, chatHandler *apichat.Handler) {
	api.POST("/drive", driveHandler.Usage, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	api.POST("/drive/files", driveHandler.FilesList, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	api.POST("/drive/files/create", driveHandler.FilesCreate, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:drive"))
	// 分割アップロード (#2313)。mk-go 独自拡張。保護は create と同一
	// (RequireAuth + RequireNotMoved + write:drive) — append は
	// BodyLimitByPath の 1MiB 制限から外れるため、未認証で到達できる経路を
	// 増やさないことが前提になっている。
	api.POST("/drive/files/create-chunked/start", driveHandler.FilesCreateChunkedStart, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:drive"))
	api.POST("/drive/files/create-chunked/append", driveHandler.FilesCreateChunkedAppend, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:drive"))
	api.POST("/drive/files/create-chunked/finish", driveHandler.FilesCreateChunkedFinish, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:drive"))
	api.POST("/drive/files/create-chunked/abort", driveHandler.FilesCreateChunkedAbort, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:drive"))
	api.POST("/drive/files/show", driveHandler.FilesShow, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	api.POST("/drive/files/update", driveHandler.FilesUpdate, middleware.RequireAuth(), middleware.RequireScope("write:drive"))
	api.POST("/drive/files/delete", driveHandler.FilesDelete, middleware.RequireAuth(), middleware.RequireScope("write:drive"))
	api.POST("/drive/files/find-by-hash", driveHandler.FilesFindByHash, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	api.POST("/drive/folders/create", driveHandler.FoldersCreate, middleware.RequireAuth(), middleware.RequireScope("write:drive"))
	api.POST("/drive/folders/show", driveHandler.FoldersShow, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	api.POST("/drive/folders/update", driveHandler.FoldersUpdate, middleware.RequireAuth(), middleware.RequireScope("write:drive"))
	api.POST("/drive/folders/delete", driveHandler.FoldersDelete, middleware.RequireAuth(), middleware.RequireScope("write:drive"))
	api.POST("/drive/folders", driveHandler.FoldersList, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	api.POST("/drive/folders/find", driveHandler.FoldersFind, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	api.POST("/drive/files/find", driveHandler.FilesFind, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	api.POST("/drive/files/check-existence", driveHandler.FilesCheckExistence, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	api.POST("/drive/files/attached-notes", driveHandler.FilesAttachedNotes, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
	// drive/files/attached-chat-messages は chat message を pack するため chat
	// handler 側で処理する (route 登録は chatHandler 構築後、#1218)。
	api.POST("/drive/files/upload-from-url", driveHandler.FilesUploadFromURL, middleware.RequireAuth(), middleware.RequireNotMoved(), middleware.RequireScope("write:drive"))
	api.POST("/drive/files/move-bulk", driveHandler.FilesMoveBulk, middleware.RequireAuth(), middleware.RequireScope("write:drive"))
	api.POST("/drive/stream", driveHandler.Stream, middleware.RequireAuth(), middleware.RequireScope("read:drive"))

	api.POST("/drive/files/attached-chat-messages", chatHandler.AttachedChatMessages, middleware.RequireAuth(), middleware.RequireScope("read:drive"))
}
