// Package transfer implements user data export/import in Misskey-compatible
// formats. Export types produce JSON or CSV blobs saved as DriveFiles; import
// types read DriveFile content and apply operations via the matching core
// services.
//
// フォーマットは本家Misskey (packages/backend/src/queue/processors/Export*Service.ts
// および Import*Service.ts) と一致させる。CSV 行終端は LF、JSON はインデント
// なしの UTF-8。ファイル名は `{type}-{YYYY-MM-DD-HH-mm-ss}.{ext}`。
package transfer

import (
	"context"
	"fmt"
	"time"

	"github.com/shiroha-a/mk/internal/core/drive"
	"github.com/shiroha-a/mk/internal/core/notification"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// Export types supported by Exporter.Export.
const (
	ExportNotes     = "notes"
	ExportFollowing = "following"
	ExportBlocking  = "blocking"
	ExportMuting    = "mute"
	ExportFavorites = "favorites"
	ExportUserLists = "user-lists"
	ExportAntennas  = "antennas"
	ExportClips     = "clips"
)

// Import types supported by Importer.Import.
const (
	ImportFollowing = "following"
	ImportBlocking  = "blocking"
	ImportMuting    = "muting"
	ImportUserLists = "user-lists"
	ImportAntennas  = "antennas"
)

// DriveSaver writes a byte buffer as a DriveFile for a given user. Implemented
// by *drive.Service in production.
type DriveSaver interface {
	Upload(ctx context.Context, in drive.UploadInput) (*model.DriveFile, error)
}

// DriveReader fetches a previously-uploaded DriveFile and streams its stored
// content back. Used by imports to pick up the user-provided JSON/CSV.
type DriveReader interface {
	Fetch(fileID string) (*model.DriveFile, []byte, error)
}

// NotificationDispatcher creates a notification on a user's stream.
// Implemented by *notification.Service.
type NotificationDispatcher interface {
	Create(ctx context.Context, in notification.CreateInput) (*notification.Notification, error)
}

// ErrUnsupportedType signals that the requested export/import type is not
// understood by the package.
var ErrUnsupportedType = fmt.Errorf("unsupported transfer type")

// timestampedName produces a Misskey-compatible filename of the form
// `{base}-{YYYY-MM-DD-HH-mm-ss}.{ext}` in UTC.
// 本家 is local-time based but UTC is deterministic and harmless for users.
func timestampedName(base, ext string) string {
	return fmt.Sprintf("%s-%s.%s", base, time.Now().UTC().Format("2006-01-02-15-04-05"), ext)
}

// acct formats a user as `username` (for local) or `username@host`
// (for remote), following Misskey's Acct.toString behaviour.
func acct(u *model.User) string {
	if u == nil {
		return ""
	}
	if u.Host == nil || *u.Host == "" {
		return u.Username
	}
	return u.Username + "@" + *u.Host
}

// ExporterDeps bundles the repositories and services an Exporter needs.
// 明示的な struct にすることで NewExporter の引数列が膨らみすぎるのを避ける。
type ExporterDeps struct {
	UserRepo         repository.UserRepository
	NoteRepo         repository.NoteRepository
	PollRepo         repository.PollRepository
	FollowingRepo    repository.FollowingRepository
	BlockingRepo     repository.BlockingRepository
	MutingRepo       repository.MutingRepository
	NoteFavoriteRepo repository.NoteFavoriteRepository
	AntennaRepo      repository.AntennaRepository
	ClipRepo         repository.ClipRepository
	ClipNoteRepo     repository.ClipNoteRepository
	UserListRepo     repository.UserListRepository
	Drive            DriveSaver
	Notifier         NotificationDispatcher
	// EmojiRepo / EmojiImageFetcher は custom-emojis export 用 (任意)。未配線
	// なら ExportCustomEmojis は "emoji source not configured" で失敗する。
	EmojiRepo         EmojiSource
	EmojiImageFetcher EmojiImageFetcher
	// IDGen はノート ID から作成時刻を読むのに使う (favorites / clips export の
	// makeNotesHiddenBefore 判定)。未配線だと期間設定のある作者のノートは
	// 判定不能として出力しない。
	IDGen id.Generator
}

// Exporter produces Misskey-compatible export files and saves them as
// DriveFiles owned by the requesting user.
type Exporter struct {
	deps ExporterDeps
}

// NewExporter constructs an Exporter from the given dependencies.
func NewExporter(deps ExporterDeps) *Exporter {
	return &Exporter{deps: deps}
}

// exportConfig carries per-call export options threaded via functional options.
// Currently only the export-following muting / inactive filters.
type exportConfig struct {
	excludeMuting   bool
	excludeInactive bool
}

// ExportOption configures a single Export call (functional-options pattern so
// existing callers need no signature change)。
type ExportOption func(*exportConfig)

// WithExcludeMuting excludes muted users from export-following (upstream
// excludeMuting param)。他 export type では無視される。
func WithExcludeMuting(v bool) ExportOption {
	return func(c *exportConfig) { c.excludeMuting = v }
}

// WithExcludeInactive excludes followees inactive for 90+ days from
// export-following (upstream excludeInactive param)。他 type では無視される。
func WithExcludeInactive(v bool) ExportOption {
	return func(c *exportConfig) { c.excludeInactive = v }
}

// Export runs the export pipeline for a single (userID, exportType) pair.
// On success the user is notified via NotificationDispatcher.Create with
// Extra={exportedEntity, fileId}. Errors bubble out to the queue processor.
func (e *Exporter) Export(ctx context.Context, userID, exportType string, opts ...ExportOption) (*model.DriveFile, error) {
	var cfg exportConfig
	for _, o := range opts {
		o(&cfg)
	}
	user, err := e.deps.UserRepo.FindByID(userID)
	if err != nil {
		return nil, fmt.Errorf("find user: %w", err)
	}

	var (
		body []byte
		name string
	)
	switch exportType {
	case ExportNotes:
		body, err = e.exportNotes(user.ID)
		name = timestampedName("notes", "json")
	case ExportFollowing:
		body, err = e.exportFollowing(user.ID, cfg.excludeMuting, cfg.excludeInactive)
		name = timestampedName("following", "csv")
	case ExportBlocking:
		body, err = e.exportBlocking(user.ID)
		name = timestampedName("blocking", "csv")
	case ExportMuting:
		body, err = e.exportMuting(user.ID)
		name = timestampedName("mute", "csv")
	case ExportFavorites:
		body, err = e.exportFavorites(user)
		name = timestampedName("favorites", "json")
	case ExportUserLists:
		body, err = e.exportUserLists(user.ID)
		name = timestampedName("user-lists", "csv")
	case ExportAntennas:
		body, err = e.exportAntennas(user.ID)
		name = timestampedName("antennas", "json")
	case ExportClips:
		body, err = e.exportClips(user)
		name = timestampedName("clips", "json")
	case ExportCustomEmojis:
		body, err = e.exportCustomEmojis(ctx)
		name = timestampedName("custom-emojis", "zip")
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedType, exportType)
	}
	if err != nil {
		return nil, fmt.Errorf("export %s: %w", exportType, err)
	}

	file, err := e.deps.Drive.Upload(ctx, drive.UploadInput{
		User:  user,
		Body:  body,
		Name:  name,
		Force: true,
	})
	if err != nil {
		return nil, fmt.Errorf("upload drive file: %w", err)
	}

	if e.deps.Notifier != nil {
		_, _ = e.deps.Notifier.Create(ctx, notification.CreateInput{
			NotifieeID: user.ID,
			Type:       notification.TypeExportCompleted,
			Extra: map[string]any{
				"exportedEntity": misskeyExportedEntity(exportType),
				"fileId":         file.ID,
			},
		})
	}
	return file, nil
}

// misskeyExportedEntity maps mk-go's internal export type to the value the
// exportCompleted notification's `exportedEntity` field must carry. misskey_dart
// (および upstream Misskey TS) は enum: note / antenna / blocking / clip /
// customEmoji / favorite / following / muting / userList で受けるため、mk-go の
// 複数形 / kebab-case の内部値 (notes / favorites / user-lists 等) を単数形 /
// camelCase へ変換しないと notifications 一覧が enum decode で落ちる (#1249)。
func misskeyExportedEntity(exportType string) string {
	switch exportType {
	case ExportNotes:
		return "note"
	case ExportFavorites:
		return "favorite"
	case ExportUserLists:
		return "userList"
	case ExportAntennas:
		return "antenna"
	case ExportClips:
		return "clip"
	case ExportMuting:
		return "muting"
	case ExportCustomEmojis:
		return "customEmoji"
	case ExportFollowing, ExportBlocking:
		// following / blocking は既に misskey enum と一致。
		return exportType
	default:
		return exportType
	}
}
