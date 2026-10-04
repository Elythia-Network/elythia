package drive

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"time"

	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/colfit"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/misc/imagedecode"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/safehttp"
	"github.com/shiroha-a/mk/internal/safemath"
	"golang.org/x/sync/semaphore"
	"gorm.io/datatypes"

	"github.com/shiroha-a/mk/internal/core/role"
)

// Errors returned by Service.
var (
	// ErrFileNotFound is returned when the target file does not exist.
	ErrFileNotFound = errors.New("drive file not found")
	// ErrFolderNotFound is returned when the target folder does not exist.
	ErrFolderNotFound = errors.New("drive folder not found")
	// ErrParentFolderNotFound is returned when the parent folder referenced
	// by a create/update operation does not exist. Distinct from
	// ErrFolderNotFound so that drive/folders/update can map this to the
	// upstream NO_SUCH_PARENT_FOLDER code (#977).
	ErrParentFolderNotFound = errors.New("drive parent folder not found")
	// ErrAccessDenied is returned when a file/folder is owned by another user.
	ErrAccessDenied = errors.New("access denied")
	// ErrFolderNotEmpty is returned when attempting to delete a folder that
	// still has children.
	ErrFolderNotEmpty = errors.New("folder is not empty")
	// ErrUnallowedFileType is returned when an upload's MIME type is not
	// covered by the user's `uploadableFileTypes` role policy. Handler
	// translates this into upstream's UNALLOWED_FILE_TYPE error code
	// (UUID 4becd248-...) (#1028).
	ErrUnallowedFileType = errors.New("unallowed file type")
	// ErrCannotUnmarkSensitive is returned when a user with the
	// `alwaysMarkNsfw=true` role policy tries to clear `isSensitive` on
	// their own drive file. Handler translates this into the drive-specific
	// RESTRICTED_BY_ROLE response (UUID 7f59dccb-...) (#1028).
	ErrCannotUnmarkSensitive = errors.New("cannot unmark sensitive while alwaysMarkNsfw policy is active")
	// ErrMaxFileSizeExceeded is returned when the uploaded file size exceeds
	// the user's `maxFileSizeMb` role policy (#1029 PR-2). Handler maps this
	// to upstream's `MAX_FILE_SIZE_EXCEEDED` 400 response.
	ErrMaxFileSizeExceeded = errors.New("max file size exceeded")

	// ErrUndecodableImage is returned when an image cannot be decoded and
	// therefore cannot have its metadata stripped.
	//
	// **代替画像の生成は best-effort だが、この形だけは例外 (#3037 レビュー
	// 2 周目)。** webpublic が作られないと `GetPublicURL` が原本を指すので、
	// **EXIF の GPS がそのまま公開側へ出る** — この PR が別の経路で塞いだ穴と
	// 同じもの。しかも同じ理由で `NormalizeImageForDetection` も失敗するので、
	// センシティブ判定が fail-open で `false` を返す。作れないなら受け取らない。
	ErrUndecodableImage = errors.New("image cannot be decoded, so its metadata cannot be stripped")
	// ErrNoFreeSpace is returned when the user's current drive usage plus the
	// new file size exceeds `driveCapacityMb` (#1029 PR-2). Handler maps this
	// to upstream's `NO_FREE_SPACE` 400 response. remote user の場合 upstream
	// は `expireOldFile` で古い file を退去させて capacity を確保するが、
	// mk-go は remote user に本 gate を掛けずそのまま受け入れる。
	//
	// **これは未実装ではなく意図的**。mk-go はリモートメディアの実体を持たず
	// link 行 (`isLink=true` / `size=0`) しか作らないため、退去すべき実体が
	// 存在せず使用量にも乗らない。gate も LRU 退去も対象が無い
	// (docs/divergence.md §5.5、#2411)。
	ErrNoFreeSpace = errors.New("no free space")
	// ErrInvalidFileName is returned when a file rename fails
	// ValidateFileName. Handler maps this to upstream's INVALID_FILE_NAME
	// (drive/files/update は 395e7156-..., create は f449b209-...) (#1564)。
	ErrInvalidFileName = errors.New("invalid file name")
	// ErrRecursiveNesting is returned when drive/folders/update would make a
	// folder its own ancestor (self-parent or ancestor cycle)。upstream
	// folders/update の recursiveNesting (#1564)。
	ErrRecursiveNesting = errors.New("recursive folder nesting")
)

// policyNumber は [role.PolicyNumber] の別名。呼び出し箇所が多いので短く
// 参照できるようにしてある。契約と理由はそちらの doc を参照。
func policyNumber(v any) (float64, bool) { return role.PolicyNumber(v) }

// policyMegabytesは数値policyを飽和byte数へ変換する。boolは値の存在ではなく、
// gateに使える正の上限であることを表す。
func policyMegabytes(v any) (int64, bool) {
	mb, ok := policyNumber(v)
	if !ok {
		return 0, false
	}
	return safemath.MulFloat64(mb, 1024*1024), mb > 0
}

// MaxUploadBytes reports the `maxFileSizeMb` role policy for a user, in bytes.
//
// **読み切る前に見るためのもの (#3037)。** `Upload` の中にも同じ判定がある
// が、あそこに届く時点で**本体はすでに全部メモリに載っている**。既定では
// policy が 30MB なのに `config.maxFileSize` が 250MB なので、
// 「30MB しか保存できない利用者が 250MB を送り付けてメモリを確保させる」
// ことができた。handler が先にこれを引いて、超える分は読まずに落とす。
//
// ok=false は「上限なし」(policy 未設定 / 0 以下 / system file / role が
// 未配線)。判定できないときに勝手な上限を作らないための形で、`Upload` 側の
// `policyMegabytes` の ok と同じ意味。
func (s *Service) MaxUploadBytes(user *model.User) (int64, bool) {
	// system file (user == nil) と remote user は `Upload` 側でも gate の
	// 対象外なので、ここでも上限を作らない。
	if user == nil || !user.IsLocal() || s.roleChecker == nil {
		return 0, false
	}
	policies := s.roleChecker.GetUserPolicies(user.ID)
	if policies == nil {
		return 0, false
	}
	return policyMegabytes(policies["maxFileSizeMb"])
}

// ValidateFileName mirrors upstream DriveFileEntityService.validateFileName:
// the trimmed name must be non-empty, at most 200 characters, and must not
// contain a backslash, a slash, or "..". 長さは JS の String.length (UTF-16)
// に対し rune 数で近似する (upload-from-url の comment 512 判定と同方針)。
//
// **NUL と不正な UTF-8 もここで落とす (#3037)。** `drive_file.name` は
// varchar なので、そのまま INSERT / UPDATE すると PostgreSQL が SQLSTATE 22021
// でクエリごと落とし、**認証済みの利用者が 1 文字で 500 を起こせる**。
// #3022 と同じく**既存の述語に畳んで既存の 400 に落とす** — 利用者にできること
// は変わらないので、wire に新しいエラーコードを足さない。
func ValidateFileName(name string) bool {
	return strings.TrimSpace(name) != "" &&
		colfit.Fits(name, 200) &&
		!strings.Contains(name, "\\") &&
		!strings.Contains(name, "/") &&
		!strings.Contains(name, "..")
}

// StreamingPublisher receives drive file life-cycle events so that
// WebSocket subscribers (the "drive" channel) can be pushed in real time.
// パッケージ間の循環依存を避けるためinterfaceで受け取る(実装はinternal/
// stream)。eventTypeは"fileCreated"/"fileUpdated"/"fileDeleted"。
// PublishDriveEvent body は upstream publishDriveStream に揃え、fileCreated /
// fileUpdated は packed DriveFileEntity (self)、fileDeleted は file id 文字列を
// 渡す (#2002)。型が event ごとに異なるため any で受ける。
type StreamingPublisher interface {
	PublishDriveEvent(userID, eventType string, body any)
}

// FolderStreamingPublisher receives drive folder life-cycle events for the
// "drive" WebSocket channel. upstream の publishDriveStream(userID,
// 'folderCreated'|'folderUpdated', folderObj) / ('folderDeleted', folder.id)
// に対応する (#1564)。body は packed folder (created/updated) または folder
// id 文字列 (deleted)。file 系イベントを扱う StreamingPublisher とは eventType
// 名前空間が別なので interface を分けている。実装は internal/stream。
type FolderStreamingPublisher interface {
	PublishDriveFolderEvent(userID, eventType string, body any)
}

// MainStreamPublisher emits real-time events to a single target user's `main`
// WebSocket channel. Used here to deliver `driveFileCreated` so the frontend
// can reflect uploads across screens (e.g. drop-upload UI).
// 循環依存を避けるためinterfaceで受け取る(実装はinternal/stream)。
type MainStreamPublisher interface {
	PublishMainEvent(userID, eventType string, body any)
}

// ChartHook is invoked after a drive file has been uploaded or deleted
// so the chart subsystem can record the byte / count delta. パッケージ
// 間の循環依存を避けるためinterfaceで受け取る(実装はcore/chart/
// charthook)。
type ChartHook interface {
	OnFileUploaded(file *model.DriveFile)
	OnFileDeleted(file *model.DriveFile)
}

// RoleChecker abstracts moderator-role + policy lookups so Show /
// Upload / Update can honour upstream Misskey's role-policy semantics
// without taking a hard dep on core/role (avoids circular imports).
//
// IsModerator is consumed by Show ("moderators can view any file") and
// Upload (moderators bypass uploadableFileTypes allowlist).
// GetUserPolicies is consumed by Upload (alwaysMarkNsfw / uploadableFileTypes)
// and Update (alwaysMarkNsfw guard against clearing isSensitive). nil
// policies map disables the gate (= test fixture). (#1028)
type RoleChecker interface {
	IsModerator(userID string) bool
	GetUserPolicies(userID string) map[string]any
}

// Service manages drive files and folders.
type Service struct {
	fileRepo            repository.DriveFileRepository
	folderRepo          repository.DriveFolderRepository
	storage             Storage
	idGen               id.Generator
	publisher           StreamingPublisher
	mainStreamPublisher MainStreamPublisher
	chartHook           ChartHook
	imageProcessor      ImageProcessor
	videoProcessor      VideoProcessor
	// Sensitive media detection
	sensitiveDetector SensitiveDetector
	sensitiveCfg      SensitiveConfig
	// officialSensitiveDetector は公式 sensitive-detector 契約 (2026.7.0) の
	// client。meta の API URL が設定されているとき優先される。
	officialSensitiveDetector *OfficialDetector
	// sensitiveSettingsProvider は meta を都度参照する live settings 源。
	sensitiveSettingsProvider func() SensitiveSettings
	// Optional moderator bypass for Show. Nil keeps the default "owner-only"
	// guard.
	roleChecker RoleChecker
	// folderPublisher emits folderCreated/folderUpdated/folderDeleted to the
	// drive channel (#1564)。nil disables folder events.
	folderPublisher FolderStreamingPublisher
	// localStorage は storedInternal=true な行の実体があるローカル FS を常に
	// 指す backend。storage が object storage に切り替わっても、切替前に
	// 保存されたファイルはこちらにある (#1414 / #2315)。
	localStorage Storage
	// 分割アップロード (#2313)。両方が配線され、かつ storage が
	// MultipartStorage を満たすときだけ機能が有効になる。
	chunkedRepo     repository.ChunkedUploadSessionRepository
	chunkedSettings func() ChunkedUploadSettings
	// objectDeleteEnqueuer は object storage の実体削除を queue に逃がす
	// (#2325)。nil なら従来どおり同期削除にフォールバックする。
	objectDeleteEnqueuer ObjectDeleteEnqueuer
	// mediaSlots は画像 / 動画処理の同時実行枠。**構築時に必ず埋める**
	// (`NewService`)。nil のまま使うと枠が無いのと同じになる。
	mediaSlots *semaphore.Weighted
	// mediaSlotCap は mediaSlots の容量。semaphore.Weighted は容量を公開
	// しないので、テストと自己検査のために持っておく。
	mediaSlotCap int
}

// defaultMediaProcessingConcurrency is how many uploads may decode at once.
//
// **枠が無いと確保量を掛け算できた。** `processImage` は寸法 / blurhash /
// サムネイル / webpublic で**順に 4 回デコードする**ため、GC が前の 1 枚を
// 返す前に次を確保しうる。認証済みの利用者が並行アップロードを投げるだけで
// それを人数分重ねられた (`semaphore` / `Acquire(` の grep が 0 件だった)。
//
// **1 枠あたりの最悪値は 1GB 級 (#3037 レビュー 2 周目で実測)。**
// drive が渡すのは `imagedecode.MaxPixels` (64MP) ではなく
// `UpstreamMaxPixels` (0x3FFF^2 = 268MP) — upstream に揃える判断で、
// 厳しくすると 102MP の実写真が webpublic を失い EXIF の GPS が公開側へ
// 出るため (`imagedecode.go` 参照)。16383x16383 の全画素ゼロ PNG は
// **764 KiB** で作れて `UpstreamMaxPixels` にちょうど一致するので上限を
// 通り抜け、8 core / 32GB の実測で `processImage` の 4 段を通すと
// **ピーク RSS 2.23 GiB / 13.6 秒**だった (デコード単体で TotalAlloc 768MiB)。
//
// **バイト予算はこの帯を縛らない。** 確保量の判定は
// `画素数 x 1 画素あたりのバイト数 > 上限画素数 x 4` なので、8bit 画像には
// 「宣言寸法の cap と同じ」判定にしかならない (16bit 画像を縛るための段)。
//
// **枠の本数はこの最悪値を前提にしていない。** 値と根拠は media proxy の枠
// (#3032) から採っており、あちらは 64MP の cap で測っている。2GB の VPS は
// `GOMAXPROCS / 2 = 1` なので、764 KiB のアップロード 1 本で RAM を使い切り
// うる。`GOMEMLIMIT` の設定はリポジトリに無い (grep 0 件)。**残存リスクとして
// `docs/divergence.md` に記録してある。**
//
// **値と根拠は media proxy の枠 (#3032) と同じ。** 同じデコーダで同じ中間
// バッファを確保するので、あちらの実測 (8 core / AVIF / c=8 で peak RSS が
// 枠 1 → 131MB、2 → 409MB、4 → 636MB、8 → 995MB、無制限 → 959MB) がそのまま
// 当てはまる。GOMAXPROCS 全部にすると無制限と区別が付かず枠を持つ意味が無い。
// 詳細は `internal/core/mediaproxy/cpulimit.go`。
func defaultMediaProcessingConcurrency() int {
	if n := runtime.GOMAXPROCS(0) / 2; n > 0 {
		return n
	}
	return 1
}

// ObjectDeleteEnqueuer queues the removal of one object storage object.
// Implemented by queue.Client (EnqueueDeleteObjectStorageFile).
type ObjectDeleteEnqueuer interface {
	EnqueueDeleteObjectStorageFile(key string) error
}

// SetObjectDeleteEnqueuer attaches the queue client used to delete object
// storage objects asynchronously. Optional — nil keeps inline deletion.
func (s *Service) SetObjectDeleteEnqueuer(e ObjectDeleteEnqueuer) {
	s.objectDeleteEnqueuer = e
}

// deleteFileObjects removes a row's primary / thumbnail / webpublic objects.
//
// upstream `DriveService.deleteFile` と同じ振り分け: ローカル FS
// (storedInternal=true) は同期削除、object storage 上の実体は queue に積む。
// 実体を持たない link 行は何もしない。
//
// 戻り値は primary object の削除結果だけ。サムネイル / webpublic の失敗で
// 行の削除を止めると、消せない実体のせいで行が永久に残ってしまう。
func (s *Service) deleteFileObjects(f *model.DriveFile) error {
	if f == nil {
		return nil
	}
	// upstream の分岐順 (`if storedInternal ... else if !isLink`) に合わせる。
	// link 行は実体を持たないので何もしない。storedInternal が立っている行は
	// link フラグに関わらずローカル FS を消す。
	if !f.StoredInternal && f.IsLink {
		return nil
	}
	del := s.objectDeleter(f)
	var primaryErr error
	if f.AccessKey != nil && *f.AccessKey != "" {
		primaryErr = del(*f.AccessKey)
	}
	for _, key := range []*string{f.ThumbnailAccessKey, f.WebpublicAccessKey} {
		if key != nil && *key != "" {
			_ = del(*key)
		}
	}
	return primaryErr
}

// objectDeleter picks the deletion strategy for f.
//
// enqueue に失敗したときは同期削除へ倒す。queue が死んでいるからといって実体を
// 取りこぼすと、追跡する手掛かりが無いまま課金され続ける孤児になる。
func (s *Service) objectDeleter(f *model.DriveFile) func(string) error {
	if !f.StoredInternal && s.objectDeleteEnqueuer != nil {
		return func(key string) error {
			err := s.objectDeleteEnqueuer.EnqueueDeleteObjectStorageFile(key)
			if err == nil {
				return nil
			}
			slog.Warn("drive: enqueue object delete failed, deleting inline", "fileId", f.ID, "err", err)
			return s.storageFor(f).Delete(key)
		}
	}
	return s.storageFor(f).Delete
}

// SetFolderStreamingPublisher attaches the drive-channel publisher for folder
// life-cycle events (#1564)。Optional — nil disables emit.
func (s *Service) SetFolderStreamingPublisher(p FolderStreamingPublisher) {
	s.folderPublisher = p
}

// publishFolderEvent is the best-effort wrapper for folder events.
func (s *Service) publishFolderEvent(userID, eventType string, body any) {
	if s.folderPublisher == nil || userID == "" {
		return
	}
	s.folderPublisher.PublishDriveFolderEvent(userID, eventType, body)
}

// SetRoleChecker wires a moderator role lookup so Show allows moderators to
// inspect any file (matching upstream Misskey's drive/files/show behaviour).
// Nil keeps the existing owner-only check.
func (s *Service) SetRoleChecker(rc RoleChecker) { s.roleChecker = rc }

// SetLocalStorage wires the always-local backend used to reach rows whose
// `storedInternal` is true while object storage is the active backend (#1414 /
// #2315). Optional: when unset those rows fall back to the primary storage,
// which is correct only while object storage is disabled.
func (s *Service) SetLocalStorage(st Storage) { s.localStorage = st }

// storageFor picks the backend that holds f, mirroring upstream
// `DriveService.deleteFile` which branches on `file.storedInternal`.
//
// オブジェクトストレージを有効にする前に保存されたファイルはローカル FS に
// 残る。primary だけを見て消すと、S3 に存在しないキーを消しに行って空振りし、
// ローカルに孤児が残り続ける。
func (s *Service) storageFor(f *model.DriveFile) Storage {
	if f != nil && f.StoredInternal && s.localStorage != nil {
		return s.localStorage
	}
	return ResolveStorage(s.storage)
}

// ReadFileBody reads the stored bytes of f from whichever backend actually
// holds it (#2966).
//
// **HTTP で自分の公開 URL を叩かない。** SSRF ガードと衝突するうえ、非公開
// URL や内部向けの構成では取れず、DB 上の行と実体の対応も確かめられない。
// `storageFor` は `storedInternal` を見るので、オブジェクトストレージへ
// 移行する前に保存されたファイルもローカルから読める。
//
// max を超える本体は読まずにエラーにする (絵文字として複製する用途で、
// 上限なしにすると 1 リクエストで任意サイズをメモリに載せられる)。
func (s *Service) ReadFileBody(f *model.DriveFile, max int64) ([]byte, error) {
	if f == nil {
		return nil, ErrObjectNotFound
	}
	// **リンクは実体を持たない。** `isLink` の行は URL を指しているだけなので、
	// storage から読めない (#2966 の複製はこの経路を使わない前提だが、
	// 呼び出し側が増えたときに黙って空を返さないようにする)。
	if f.IsLink {
		return nil, ErrObjectNotFound
	}
	if f.AccessKey == nil || *f.AccessKey == "" {
		return nil, ErrObjectNotFound
	}
	st := s.storageFor(f)
	if st == nil {
		return nil, ErrObjectNotFound
	}
	rc, err := st.Get(*f.AccessKey)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return safehttp.ReadAllLimit(rc, max)
}

// CopyToSystemFile duplicates src as a system-owned drive file (userId /
// userHost NULL) and returns the copy (#2966).
//
// 元のファイルは読むだけで、移動も変更も削除もしない。所有権を移すと、ノートの
// 添付やプロフィールで使っているファイルが利用者の drive から突然消える。
//
// **実体はストレージから直に読む。** 自分の公開 URL を HTTP で叩くと SSRF ガードと
// 衝突し、非公開 URL の構成では取れず、DB 上の行と実体の対応も確かめられない。
// `ReadFileBody` の `storageFor` が `storedInternal` を見るので、オブジェクト
// ストレージへ移行する前に保存されたファイルもローカルから読める。
//
// max は読み出しの上限。**ここには既定値を置かない** — 用途ごとの上限を呼び出し側が
// 1 箇所で定義できるようにするため (絵文字は core/emojiapplication の
// MaxEmojiCopyBytes が唯一の定義で、申請側と承認側で食い違うと「申請はできたのに
// 承認だけが恒久的に失敗する」サイズ帯が生まれる)。
//
// 実体が無い行 (isLink / accessKey 無し / ストレージから消えた) は
// ErrObjectNotFound、上限超過は safehttp.ErrResponseTooLarge が `%w` で伝わるので、
// 呼び出し側は errors.Is で種別を分けられる。
func (s *Service) CopyToSystemFile(ctx context.Context, src *model.DriveFile, name string, sensitive bool, max int64) (*model.DriveFile, error) {
	if src == nil {
		return nil, fmt.Errorf("no source file")
	}
	body, err := s.ReadFileBody(src, max)
	if err != nil {
		return nil, fmt.Errorf("read source file: %w", err)
	}

	driveName := name
	if driveName == "" {
		driveName = src.Name
	}
	// **`User: nil` が system-owned の作り方。** 利用者に紐付けると、ロールの
	// 変更やアカウント削除で巻き込まれる。`Force` は user==nil のとき dedup が
	// 元から効かないので no-op だが、「重複しても新しいファイルを作る」契約を
	// 読み手に示すために明示する。
	df, err := s.Upload(ctx, UploadInput{
		User:  nil,
		Body:  body,
		Name:  driveName,
		Force: true,
		// **センシティブの指定は引き継ぐ。** 元ファイルに印が無くても、呼び出し側
		// (申請) が sensitive を指定していれば複製にも付ける。
		IsSensitive: src.IsSensitive || sensitive,
	})
	if err != nil {
		return nil, fmt.Errorf("upload copy to drive: %w", err)
	}
	return df, nil
}

// IsSystemOwned reports whether f belongs to the instance itself rather than to
// a local user or to a remote author.
//
// **条件は孤児 cleanup の guard (`orphanWhere`) と同じ。** あちらが
// `"userId" IS NULL AND "userHost" IS NULL` を system 所有の定義にしているので、
// ここで片方だけを見ると「cleanup からは守られないのに複製も要らない」と判定する
// 行が生まれる。リモート由来の行 (`userHost` 非 NULL) は
// `admin/drive/clean-remote-files` で消えるので system 所有ではない。
func IsSystemOwned(f *model.DriveFile) bool {
	return f != nil && f.UserID == nil && f.UserHost == nil
}

// PreferWebpublicURL returns f's webpublic URL when present, else its canonical
// URL (upstream `webpublicUrl ?? url`).
//
// **絵文字の `originalUrl` にはこれを使わない。** drive の孤児 cleanup は
// `emoji.originalUrl = drive_file.url` または `publicUrl = url` を参照保護の条件に
// しているので、`originalUrl` に webpublic だけを入れると保護が外れて消される。
func PreferWebpublicURL(f *model.DriveFile) string {
	if f == nil {
		return ""
	}
	if f.WebpublicURL != nil && *f.WebpublicURL != "" {
		return *f.WebpublicURL
	}
	return f.URL
}

// PreferWebpublicType returns f's webpublic MIME when present, else its
// canonical type (upstream `webpublicType ?? type`). Returned as a pointer so a
// non-empty value lands in emoji.type and NULL when both are empty.
func PreferWebpublicType(f *model.DriveFile) *string {
	if f == nil {
		return nil
	}
	if f.WebpublicType != nil && *f.WebpublicType != "" {
		t := *f.WebpublicType
		return &t
	}
	if f.Type != "" {
		t := f.Type
		return &t
	}
	return nil
}

// SetSensitiveDetection attaches the sensitive media detector and config.
func (s *Service) SetSensitiveDetection(detector SensitiveDetector, cfg SensitiveConfig) {
	s.sensitiveDetector = detector
	s.sensitiveCfg = cfg
}

// SensitiveSettings bundles the meta-driven sensitive detection state used
// per upload. Config は従来の検出モード/感度、Official は 2026.7.0 の公式
// sensitive-detector 接続設定。
type SensitiveSettings struct {
	Config   SensitiveConfig
	Official OfficialDetectorSettings
}

// SetSensitiveSettingsProvider wires a live meta source for sensitive
// detection settings. upstream TS は meta を都度参照するため、admin の設定
// 変更が再起動なしで反映される。provider が nil の場合は
// SetSensitiveDetection の起動時 snapshot (旧挙動) を使う。
func (s *Service) SetSensitiveSettingsProvider(p func() SensitiveSettings) {
	s.sensitiveSettingsProvider = p
}

// SetOfficialSensitiveDetector attaches the official sensitive-detector
// client (upstream 2026.7.0 protocol)。meta.sensitiveMediaDetectionApiUrl が
// 設定されているとき legacy detector より優先される。
func (s *Service) SetOfficialSensitiveDetector(d *OfficialDetector) {
	s.officialSensitiveDetector = d
}

// NewService constructs a DriveService.
func NewService(
	fileRepo repository.DriveFileRepository,
	folderRepo repository.DriveFolderRepository,
	storage Storage,
	idGen id.Generator,
) *Service {
	return &Service{
		fileRepo:     fileRepo,
		folderRepo:   folderRepo,
		storage:      storage,
		idGen:        idGen,
		mediaSlots:   semaphore.NewWeighted(int64(defaultMediaProcessingConcurrency())),
		mediaSlotCap: defaultMediaProcessingConcurrency(),
	}
}

// SetMediaProcessingConcurrency resizes the image/video processing pool.
//
// n <= 0 は既定に戻す。**枠を無くす指定は用意しない** — 無制限にすると、
// 認証済みの利用者が並行アップロードだけでプロセスを落とせる状態に戻る。
//
// **起動時専用。goroutine-safe ではない。** `mediaproxy.SetCPUConcurrency` と
// 同じ理由で、リクエストを捌いている最中に呼ぶと (a) `acquireMediaSlot` の
// 読みとの間でデータ競合になり、(b) 旧枠の保持者と新枠の容量で一瞬 2n 本が
// 同時にデコードする = **この枠が守っている不変条件そのものが破れる**。
func (s *Service) SetMediaProcessingConcurrency(n int) {
	if n <= 0 {
		n = defaultMediaProcessingConcurrency()
	}
	s.mediaSlotCap = n
	s.mediaSlots = semaphore.NewWeighted(int64(n))
}

// SetStreamingPublisher attaches a StreamingPublisher invoked best-effort
// after Upload / Update / Delete succeed.
func (s *Service) SetStreamingPublisher(p StreamingPublisher) {
	s.publisher = p
}

// SetMainStreamPublisher attaches a publisher used to emit `driveFileCreated`
// on the uploader's main channel. Optional — nil disables main-stream emit
// but the drive-channel `fileCreated` event remains unaffected.
func (s *Service) SetMainStreamPublisher(p MainStreamPublisher) {
	s.mainStreamPublisher = p
}

// SetChartHook attaches a ChartHook invoked best-effort after a drive
// file has been uploaded or deleted.
func (s *Service) SetChartHook(h ChartHook) {
	s.chartHook = h
}

// SetImageProcessor attaches an ImageProcessor for thumbnail/webpublic
// generation during upload.
func (s *Service) SetImageProcessor(p ImageProcessor) {
	s.imageProcessor = p
}

// SetVideoProcessor attaches a VideoProcessor for video thumbnail
// extraction during upload.
func (s *Service) SetVideoProcessor(p VideoProcessor) {
	s.videoProcessor = p
}

// publishEvent is a tiny best-effort wrapper around publisher.PublishDriveEvent.
func (s *Service) publishEvent(userID, eventType string, body any) {
	if s.publisher == nil || userID == "" {
		return
	}
	s.publisher.PublishDriveEvent(userID, eventType, body)
}

// UploadInput is the parameter set for Service.Upload.
type UploadInput struct {
	User        *model.User
	Body        []byte
	Name        string
	Comment     *string
	FolderID    *string
	IsSensitive bool
	Force       bool // if true, do not deduplicate by md5 hash
	// RequestIP / RequestHeaders はモデレーション目的で drive_file 行に
	// 記録される (upstream DriveService と同じく admin/drive/show-file の
	// IP タブから参照される、#1148)。handler 層が c.RealIP() / c.Request().
	// Header から構築して渡す。system file (User==nil) や upload-from-url
	// の経路では空のままで OK (= row column は nil / "{}" のまま)。
	RequestIP      *string
	RequestHeaders datatypes.JSON
	// PreStored marks the body as already present in storage, so Upload skips
	// its own Put and adopts the given key/URL for the main object. Set by the
	// chunked upload path (#2313), where the object was assembled by the
	// storage backend's multipart API.
	//
	// 呼び出し側の責務: dedup で既存 file が返ったときや Upload がエラーを
	// 返したときは、pre-stored object を消すこと。Upload は自分で Put して
	// いない object の後始末を、fileRepo.Create 失敗の rollback 以外では
	// 行わない。
	PreStored *PreStoredObject
}

// PreStoredObject identifies an object that already exists in storage.
type PreStoredObject struct {
	AccessKey string
	URL       string
}

// Upload writes a file to storage and creates a drive_file row. もし同じmd5の
// ファイルが既に存在し Force=false なら、既存レコードを返す (deduplication)。
//
// in.User == nil は「system 所有 drive file」を表現する (Misskey TS の
// driveService.uploadFromUrl({user: null, ...}) と等価)。system file は
// 特定 user に紐付かないため md5 dedup / folder 所有権チェック / sensitive
// 検出 / stream publish はいずれも skip する。custom emoji のリモート→
// local コピー (#670) など、誰のものでもないがインスタンスが管理する
// アセット用。
//
// ctx は upload リクエストの context (#749)。sensitive media detection の
// 外部 detector 呼び出しでこの ctx を伝播するため、caller は client が
// 切断したら detector もキャンセルされることを期待できる。
func (s *Service) Upload(ctx context.Context, in UploadInput) (*model.DriveFile, error) {
	// in.Body は []byte なので bytes.Reader 経由のAnalyseFileは失敗しない。
	// AnalyseFile自体は io.Reader を取るがエラー経路はここでは到達しない。
	info, _ := AnalyseFile(bytes.NewReader(in.Body))

	// role policy gate (#1028)。system file (in.User == nil) は policy 対象外。
	// upstream Misskey TS DriveService と同様、moderator は uploadableFileTypes
	// allowlist を bypass し、それ以外の user は allowlist match を要求する。
	// alwaysMarkNsfw policy が true なら IsSensitive を強制 true にし、
	// sensitive detection は実行しても無意味なので skip フラグを立てる。
	var rolePolicyForceSensitive bool
	if in.User != nil && s.roleChecker != nil {
		isModerator := s.roleChecker.IsModerator(in.User.ID)
		policies := s.roleChecker.GetUserPolicies(in.User.ID)
		if !isModerator && policies != nil {
			if !mimeAllowedByPolicy(info.MimeType, policies["uploadableFileTypes"]) {
				return nil, ErrUnallowedFileType
			}
		}
		if policies != nil {
			if v, ok := policies["alwaysMarkNsfw"].(bool); ok && v {
				rolePolicyForceSensitive = true
				in.IsSensitive = true
			}
		}
		// maxFileSizeMb / driveCapacityMb gate (#1029 PR-2)。upstream Misskey TS
		// `DriveService.addFile` は **local user のみ** local capacity / size を
		// 強制し、remote user は expireOldFile で逃げ道を作る。mk-go も remote は
		// skip するが、理由が違う: リモートメディアの実体を持たない設計なので
		// gate すべき容量そのものが発生しない (link 行は `size=0`)。expireOldFile
		// 相当を足さないのも同じ理由で、意図的な差分 (docs/divergence.md §5.5)。
		if in.User.IsLocal() && policies != nil {
			if maxBytes, ok := policyMegabytes(policies["maxFileSizeMb"]); ok {
				if int64(len(info.Body)) > maxBytes {
					return nil, ErrMaxFileSizeExceeded
				}
			}
			if capacityBytes, ok := policyMegabytes(policies["driveCapacityMb"]); ok {
				// UsageByUser の DB error を握り潰すと usage=0 として gate を
				// pass してしまい driveCapacityMb 制限が事実上効かなくなる。
				// production の transient DB error 時に黙って upload を許可する
				// のは避けたいので明示的に internal error として伝播する。
				usage, err := s.fileRepo.UsageByUser(in.User.ID)
				if err != nil {
					return nil, fmt.Errorf("calc drive usage: %w", err)
				}
				if safemath.SumExceedsInt64(capacityBytes, usage, int64(len(info.Body))) {
					return nil, ErrNoFreeSpace
				}
			}
		}
	}

	if in.User != nil && !in.Force {
		if existing, err := s.fileRepo.FindByMD5(in.User.ID, info.MD5); err == nil {
			return existing, nil
		}
	}

	// 宛先 folder の所有権チェック (system file は folder を持たない前提なので skip)。
	// 他人所有 folder も「不在」として ErrFolderNotFound に統一する: missing と
	// others' で response が変わると folder 存在 oracle になる。upstream addFile は
	// 両者とも folder-not-found error (→ 500) で区別しない。mk-go は missing を
	// 既に NO_SUCH_FOLDER(404) で返すため others' も揃えて oracle を閉じる (#1908)。
	if in.User != nil && in.FolderID != nil {
		folder, err := s.folderRepo.FindByID(*in.FolderID)
		if err != nil {
			// **DB 障害を not-found に丸めない** (#2799)。
			if !repository.IsNotFound(err) {
				return nil, err
			}
			return nil, ErrFolderNotFound
		}
		if folder.UserID == nil || *folder.UserID != in.User.ID {
			return nil, ErrFolderNotFound
		}
	}

	// backend は meta から動的に解決される (#2315)。本体・サムネイル・
	// webpublic の書き込みと storedInternal の決定が途中の設定変更で
	// ちぐはぐにならないよう、ここで一度だけ固定する。
	//
	// PreStored (#2313) の場合も同じ backend を使う。分割アップロードは
	// MultipartStorage を要求するので backend は必ず object storage 側であり、
	// storedInternal は false になる。
	// **メタデータを落とせない画像は受け取らない (#3037 レビュー 2 周目)。**
	//
	// 代替画像の生成は best-effort だが、`webpublic` が作られないと
	// `GetPublicURL` が原本へ落ちるので、**EXIF の GPS がそのまま公開側へ出る**
	// — この PR が `entity/drive.go` 側で塞いだ穴と同じもの。
	// `SandboxedDecoderMaxBytes` は #3037 が新しく入れた上限なので、**それが
	// 原因で落ちる入力だけ**を「作れないなら受け取らない」に倒す (元から
	// デコードできない形式の扱いは変えない)。
	//
	// 条件に `hasStrippableMetadata` を入れてあるので、落とすものが無い画像は
	// 従来どおり通る (その場合は原本を出しても漏れない)。
	//
	// **AVIF だけは別に見る。** `hasStrippableMetadata` は AVIF に false を
	// 返す — 「AVIF は寸法に関わらず webpublic を作る枝が別にある」ことが
	// 前提だが、**デコードできなければその枝も動かない**。AVIF の原本は
	// Mastodon / MS Edge が表示できないので、通すと壊れた添付になる。
	//
	// **`backend.Put` より前に置く (#3037 レビュー 3 周目)。** 後ろに置くと
	// 本体だけがストレージに残り、`drive_file` 行が無いので
	// `UsageByUser` にも `DeleteOrphans` (行ベース) にも乗らない =
	// **恒久的にリークする**。実測で拒否 1 回あたり 34,603,050 バイトが
	// 残った。分割アップロードは `discardChunkedObject` が消すので
	// 残らないが、`drive/files/create` と `upload-from-url` は残る。
	if isMimeImage(info.MimeType) && imagedecode.ExceedsSandboxedDecoderSize(info.Body) &&
		(hasStrippableMetadata(info.Body, info.MimeType) || info.MimeType == "image/avif") {
		return nil, ErrUndecodableImage
	}

	backend := ResolveStorage(s.storage)
	storedInternal := StorageIsLocal(backend)

	var accessKey, url string
	if in.PreStored != nil {
		// chunked upload (#2313) は既に multipart で object を組み終えている。
		// ここで Put し直すと本体をもう一度丸ごと転送することになるので skip する。
		// 以降の gate (dedup / folder 所有権) を通らなかった場合の後始末は
		// 呼び出し側が行う。
		accessKey = in.PreStored.AccessKey
		url = in.PreStored.URL
	} else {
		var err error
		accessKey, err = newAccessKey()
		if err != nil {
			return nil, err
		}
		url, err = backend.Put(accessKey, bytes.NewReader(info.Body))
		if err != nil {
			return nil, err
		}
	}

	// 画像/動画処理 (best-effort)
	var thumbnail, webpublic *ProcessedImage
	var blurhash *string
	var properties datatypes.JSON

	alts := s.generateAlts(ctx, in.Body, info.MimeType)
	if alts != nil {
		thumbnail = alts.thumbnail
		webpublic = alts.webpublic
		blurhash = alts.blurhash
		properties = alts.properties
	}

	// サムネイル/webpublic を storage に保存
	var thumbnailURL, webpublicURL *string
	var thumbnailAccessKey, webpublicAccessKey *string
	var webpublicType *string

	if thumbnail != nil {
		tKey, err := newAccessKey()
		if err == nil {
			tURL, err := backend.Put(tKey, bytes.NewReader(thumbnail.Data))
			if err == nil {
				thumbnailURL = &tURL
				thumbnailAccessKey = &tKey
			}
		}
	}
	if webpublic != nil {
		wKey, err := newAccessKey()
		if err == nil {
			wURL, err := backend.Put(wKey, bytes.NewReader(webpublic.Data))
			if err == nil {
				webpublicURL = &wURL
				webpublicAccessKey = &wKey
				webpublicType = &webpublic.MimeType
			}
		}
	}

	now := time.Now()
	fileID := s.idGen.Generate(now)
	var userIDPtr *string
	var userHostPtr *string
	if in.User != nil {
		uid := in.User.ID
		userIDPtr = &uid
		userHostPtr = in.User.Host
	}
	f := &model.DriveFile{
		ID:       fileID,
		UserID:   userIDPtr,
		UserHost: userHostPtr,
		MD5:      info.MD5,
		// 検出した形式と食い違う拡張子は upstream と同じく足して補正する
		// (`Belmond.png` に JPEG を上げたら `Belmond.png.jpg`)。
		Name:       CorrectFilename(in.Name, ExtensionForMIME(info.MimeType)),
		Type:       info.MimeType,
		Size:       info.Size,
		Comment:    in.Comment,
		Blurhash:   blurhash,
		Properties: properties,
		// upstream DriveService.save は useObjectStorage で true/false を
		// 出し分ける。ここを true 固定にしていたため、オブジェクトストレージに
		// 保存したファイルまで「ローカル保存」と記録され、`/files/:accessKey`
		// がローカルを見に行って 404 になっていた (#2315)。
		StoredInternal:     storedInternal,
		URL:                url,
		ThumbnailURL:       thumbnailURL,
		WebpublicURL:       webpublicURL,
		WebpublicType:      webpublicType,
		AccessKey:          &accessKey,
		ThumbnailAccessKey: thumbnailAccessKey,
		WebpublicAccessKey: webpublicAccessKey,
		FolderID:           in.FolderID,
		IsSensitive:        in.IsSensitive,
		RequestIP:          in.RequestIP,
		RequestHeaders:     in.RequestHeaders,
	}

	// Sensitive media detection フック (system file は user 紐付きが無いので skip)
	// alwaysMarkNsfw role policy が true の user は既に IsSensitive=true で
	// 確定しているので detect の意味がない、skip して負荷を減らす (#1028)。
	if !f.IsSensitive && !rolePolicyForceSensitive && in.User != nil {
		f.IsSensitive = s.detectSensitive(ctx, in.User, in.Body, info.MimeType)
	}

	if err := s.fileRepo.Create(f); err != nil {
		// ロールバックとして storage を削除する
		_ = backend.Delete(accessKey)
		if thumbnailAccessKey != nil {
			_ = backend.Delete(*thumbnailAccessKey)
		}
		if webpublicAccessKey != nil {
			_ = backend.Delete(*webpublicAccessKey)
		}
		return nil, err
	}
	// system file (in.User == nil) は誰の UI にも反映する必要が無いので
	// stream publish 系を skip する。publishEvent / mainStreamPublisher は
	// それぞれ userID を必要とする。
	if in.User != nil {
		// upstream は packedFile (self) を drive channel に送る (#2002)。
		s.publishEvent(in.User.ID, "fileCreated", entity.PackDriveFileSelf(f, s.idGen))
		// Misskey本家DriveService.addFileはdrive topicに加えてmainにも
		// driveFileCreatedをemitしてUploader自身のUI(ドロップアップロード等)
		// を即時反映させる(TS: DriveService.ts:665)。
		if s.mainStreamPublisher != nil {
			s.mainStreamPublisher.PublishMainEvent(in.User.ID, "driveFileCreated", entity.PackDriveFileSelf(f, s.idGen))
		}
	}
	// chartHook は user nil でも発火させる: bytes / count は instance-wide
	// stats なので custom emoji 等の system file もインスタンスストレージの
	// 一部として計上したい (uploader UI 反映の publishEvent とは異なる扱い)。
	if s.chartHook != nil {
		s.chartHook.OnFileUploaded(f)
	}
	return f, nil
}

// detectSensitive runs sensitive media detection based on meta config.
// ベストエフォート: detector 未設定やエラー時は false を返す。
//
// ctx は upload リクエストの context を伝播する (#749)。client が早期に
// 切断したら detector 呼び出しもキャンセルされる。
func (s *Service) detectSensitive(ctx context.Context, user *model.User, body []byte, mime string) bool {
	settings := SensitiveSettings{Config: s.sensitiveCfg}
	if s.sensitiveSettingsProvider != nil {
		settings = s.sensitiveSettingsProvider()
	}
	cfg := settings.Config

	// mediaSilencedHosts: リモートユーザーのホストが silenced ならば無条件 sensitive
	if user.Host != nil && IsSilencedHost(*user.Host, cfg.SilencedHosts) {
		return true
	}

	// 公式 sensitive-detector (2026.7.0) は meta の API URL 設定時のみ有効。
	officialEnabled := s.officialSensitiveDetector != nil && strings.TrimSpace(settings.Official.APIURL) != ""

	if !cfg.SetFlagAutomatically || (s.sensitiveDetector == nil && !officialEnabled) {
		return false
	}

	isLocal := user.Host == nil || *user.Host == ""
	if !ShouldDetect(cfg, isLocal) {
		return false
	}

	if officialEnabled {
		return s.detectSensitiveOfficial(ctx, body, mime, cfg, settings.Official)
	}

	// 動画チェック: enableForVideos が false なら動画はスキップ
	if IsVideoMIME(mime) && !cfg.EnableForVideos {
		return false
	}

	score, err := s.sensitiveDetector.Detect(ctx, body, mime)
	if err != nil {
		// detector が落ちていても upload 自体は通す best-effort 実装だが、
		// silently 飲み込むと「自動付与が動いていない」ことに operator が
		// 気付けない (#750)。warn ログだけ残す。body 内容は出さない (privacy)。
		slog.Warn("drive: sensitive detector failed", "mime", mime, "err", err)
		return false
	}

	threshold := SensitivityThreshold(cfg.Sensitivity)
	return score >= threshold
}

// detectSensitiveOfficial runs the upstream 2026.7.0 detection pipeline:
// 画像は 299x299 PNG に正規化して単発判定、動画/APNG はフレーム抽出して
// batch 判定 + 割合集約。すべての失敗は fail-open (非センシティブ)。
func (s *Service) detectSensitiveOfficial(ctx context.Context, body []byte, mime string, cfg SensitiveConfig, official OfficialDetectorSettings) bool {
	threshold := OfficialSensitivityThreshold(cfg.Sensitivity)

	// upstream は `image/apng` を動画側 (FFmpeg フレーム抽出) に流す。mk-go の
	// mime 検出は APNG を image/vnd.mozilla.apng と報告するため両方受ける。
	isAPNG := mime == "image/apng" || mime == "image/vnd.mozilla.apng"
	if (IsVideoMIME(mime) || isAPNG) && cfg.EnableForVideos {
		extractor, ok := s.videoProcessor.(DetectionFrameExtractor)
		if !ok {
			slog.Warn("drive: official sensitive detection for videos requires FFmpeg video processor", "mime", mime)
			return false
		}
		frames, err := extractor.ExtractDetectionFrames(ctx, body)
		if err != nil || len(frames) == 0 {
			if err != nil {
				slog.Warn("drive: detection frame extraction failed", "mime", mime, "err", err)
			}
			return false
		}
		predictions := s.officialSensitiveDetector.DetectMany(ctx, frames, official)
		// 判定に成功したフレームのみ集約する。0 件なら false (upstream の
		// results.length > 0 ガード = 全動画センシティブ化バグの回避)。
		var judged []bool
		for _, preds := range predictions {
			if preds == nil {
				continue
			}
			sensitive, _ := JudgePredictions(preds, threshold)
			judged = append(judged, sensitive)
		}
		return AggregateFrameJudgements(judged, threshold)
	}
	if IsVideoMIME(mime) {
		// 動画で enableForVideos=false は判定しない (upstream analyzeVideo)。
		return false
	}
	if !isMimeImage(mime) {
		// upstream の isMimeImage('sharp-convertible-image-with-bmp') guard 相当。
		return false
	}

	// **ここも枠を取る (#3037)。** `NormalizeImageForDetection` は
	// `decodeImage` (= `DecodeWithPixelCap(body, UpstreamMaxPixels)`) +
	// `imaging.Fill` で、`processImage` と**同じ cap・同じ大きさ**の中間
	// バッファを確保する。枠の外に置くと、並行アップロードのぶんだけ
	// 無制限に積み上がって「同時にデコードする本数を縛る」という枠の不変条件が
	// 成立しない。
	release, slotOK := s.acquireMediaSlot(ctx)
	if !slotOK {
		slog.Warn("drive: センシティブ判定の枠を待っている間に中断されました", "mime", mime)
		return false
	}
	defer release()

	normalized, err := NormalizeImageForDetection(body, mime)
	if err != nil {
		slog.Warn("drive: image normalization for detection failed", "mime", mime, "err", err)
		return false
	}
	predictions := s.officialSensitiveDetector.DetectMany(ctx, [][]byte{normalized}, official)
	if len(predictions) == 0 || predictions[0] == nil {
		return false
	}
	sensitive, _ := JudgePredictions(predictions[0], threshold)
	return sensitive
}

// generateAltsResult holds the results of image/video processing.
type generateAltsResult struct {
	thumbnail  *ProcessedImage
	webpublic  *ProcessedImage
	blurhash   *string
	properties datatypes.JSON
}

// generateAlts runs image or video processing on the uploaded file.
// processor が nil の場合や処理失敗時は nil を返す (best-effort)。
//
// **同時実行枠を取ってから走る。** デコードは 1 枚で数百 MB を確保しうるので、
// 並行アップロードで掛け算されるとプロセスごと落ちる (Go の大確保失敗は
// `throw("out of memory")` で recover できない)。枠が空くまで待つのは、
// アップロード自体を失敗させるより背圧として素直なため。
func (s *Service) generateAlts(ctx context.Context, body []byte, mimeType string) *generateAltsResult {
	image := isMimeImage(mimeType) && s.imageProcessor != nil
	video := isMimeVideo(mimeType) && s.videoProcessor != nil
	if !image && !video {
		return nil
	}
	if video {
		// **ffmpeg は枠の外で回す。** 別プロセスなので Go ヒープの中間バッファを
		// 1 つも抱えない一方、ffmpeg 自身は 1 本で何十秒も回りうるので、枠の
		// 内側に入れると**動画 1 本がその間ずっと枠を占有する**。既定枠は
		// `GOMAXPROCS / 2` (2 core の VPS では 1) なので、大きい動画 1 本で
		// 画像アップロードのサムネイル生成が全部止まる。
		//
		// **#3037 で ffmpeg 側に ctx と timeout を入れた**ので「何分でも
		// 居座る」ことは無くなったが、枠の外に置く判断は変えていない
		// (timeout は 60 秒で、枠 1 つがその間塞がるのは変わらない)。
		//
		// media proxy の枠も同じ理由で「囲むのは decode/resize/encode だけ」と
		// 決めてある (`internal/core/mediaproxy/cpulimit.go`)。
		thumb, _ := s.videoProcessor.GenerateThumbnail(ctx, body, mimeType)
		if thumb != nil {
			return &generateAltsResult{thumbnail: thumb}
		}
		return nil
	}

	release, ok := s.acquireMediaSlot(ctx)
	if !ok {
		// **ctx が切れたときだけここへ来る。** 呼び出し元が諦めている状態なので
		// 代替画像を作っても捨てられる。best-effort の契約どおり nil を返す。
		slog.Warn("drive: メディア処理の枠を待っている間に中断されました", "mimeType", mimeType)
		return nil
	}
	defer release()
	return s.processImage(body, mimeType)
}

// acquireMediaSlot takes one processing slot, returning the release func.
//
// ok=false は ctx が切れた場合だけ。**待ちに固有の上限は置かない** — 待って
// いるのはアップロード中のリクエストで、その ctx は利用者が接続を切れば切れる
// ので、待ち時間はそこで頭打ちになる。media proxy と違って途中で 503 に倒さ
// ないのは、代替画像を作れなかったアップロードは**やり直しが効かない**ため
// (ファイル自体は保存済みで、サムネイルだけが恒久的に欠ける)。
//
// **取った semaphore をクロージャに捕まえる。** `SetMediaProcessingConcurrency`
// は `s.mediaSlots` を差し替えるので、解放時にフィールドを読み直すと acquire
// したのと別の Weighted へ Release が入り `semaphore: released more than held`
// で **panic してプロセスが落ちる** (`mediaproxy.acquireCPU` と同じ理由)。
//
// 枠が未配線 (テストが Service を構造体リテラルで組んだ場合など) なら
// 素通しする。`NewService` は必ず張るので production では起きない
// (`TestMediaProcessingConcurrency_Defaults` が固定)。
func (s *Service) acquireMediaSlot(ctx context.Context) (func(), bool) {
	slots := s.mediaSlots
	if slots == nil {
		return func() {}, true
	}
	if err := slots.Acquire(ctx, 1); err != nil {
		return nil, false
	}
	var released bool
	return func() {
		if released {
			return
		}
		released = true
		slots.Release(1)
	}, true
}

// processImage runs all image processing steps (best-effort).
func (s *Service) processImage(body []byte, mimeType string) *generateAltsResult {
	result := &generateAltsResult{}

	// 寸法取得
	w, h, err := s.imageProcessor.GetDimensions(body, mimeType)
	if err == nil && (w > 0 || h > 0) {
		props := map[string]int{"width": w, "height": h}
		if data, err := json.Marshal(props); err == nil {
			result.properties = datatypes.JSON(data)
		}
	}

	// BlurHash
	if hash, err := s.imageProcessor.CalculateBlurhash(body, mimeType); err == nil && hash != "" {
		result.blurhash = &hash
	}

	// サムネイル
	result.thumbnail, _ = s.imageProcessor.GenerateThumbnail(body, mimeType)

	// Webpublic
	result.webpublic, _ = s.imageProcessor.GenerateWebpublic(body, mimeType)

	return result
}

// Show returns the file with id. Owners can read their own files; moderators
// (when a roleChecker is wired) can read any file. Matches upstream Misskey
// drive/files/show.ts semantics so admin moderation surfaces and remote-media
// detail views work without 403.
//
// 注意: write 経路 (Update / Delete) では moderator bypass を意図的に
// 適用しない。他ユーザーの file への moderator 経由の write/delete を
// 許してしまう privilege escalation を避けるため、それらは
// findOwnedFile() を使って owner-only ガードする (#499)。moderator が
// 他ユーザーの file を編集 / 削除する正規経路は admin/drive/* 側に
// 別途存在する。
func (s *Service) Show(user *model.User, id string) (*model.DriveFile, error) {
	if user == nil {
		return nil, errors.New("user is required")
	}
	f, err := s.fileRepo.FindByID(id)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrFileNotFound
	}
	return s.authorizeFileRead(user, f)
}

// ShowByURL is the url-keyed variant of Show used by drive/files/show's
// anyOf {url} param (#1564)。url / webpublicUrl / thumbnailUrl のいずれか一致
// で検索し、permission は Show と同じ owner or moderator。
func (s *Service) ShowByURL(user *model.User, url string) (*model.DriveFile, error) {
	if user == nil {
		return nil, errors.New("user is required")
	}
	f, err := s.fileRepo.FindByAnyURL(url)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrFileNotFound
	}
	return s.authorizeFileRead(user, f)
}

// authorizeFileRead is the shared read guard for Show / ShowByURL: the owner
// always may read; otherwise a moderator may (lazy 評価で owner 経路は role
// lookup を払わない)。read 認可規則を 1 箇所に集約して fileId / url 経路の
// drift を防ぐ (#1564 review)。
func (s *Service) authorizeFileRead(user *model.User, f *model.DriveFile) (*model.DriveFile, error) {
	if f.UserID != nil && *f.UserID == user.ID {
		return f, nil
	}
	if s.IsModerator(user.ID) {
		return f, nil
	}
	return nil, ErrAccessDenied
}

// IsModerator reports whether the user holds a moderator role via the wired
// RoleChecker (false when none is wired). handler 層 (attached-notes の
// moderator bypass、#1564) が参照する。
func (s *Service) IsModerator(userID string) bool {
	return s.roleChecker != nil && s.roleChecker.IsModerator(userID)
}

// findOwnedFile is the strict owner-only equivalent of Show, used by write
// paths (Update / Delete). It never honours the moderator bypass so that
// drive write/delete operations can never escalate via moderator role.
func (s *Service) findOwnedFile(user *model.User, id string) (*model.DriveFile, error) {
	if user == nil {
		return nil, errors.New("user is required")
	}
	f, err := s.fileRepo.FindByID(id)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrFileNotFound
	}
	if f.UserID == nil || *f.UserID != user.ID {
		return nil, ErrAccessDenied
	}
	return f, nil
}

// FindByHash returns every file of the user with the given md5 hash.
// upstream drive/files/find-by-hash は findBy({md5, userId}) で一致全件を
// 配列で返す (#1564 で最新 1 件 → 全件に修正)。一致 0 件は空 slice。
func (s *Service) FindByHash(user *model.User, md5 string) ([]*model.DriveFile, error) {
	if user == nil {
		return nil, errors.New("user is required")
	}
	return s.fileRepo.FindAllByMD5(user.ID, md5)
}

// Capacity returns the user's drive capacity in bytes derived from the
// driveCapacityMb role policy (1024*1024*driveCapacityMb), mirroring upstream
// drive.ts (`capacity: 1024 * 1024 * policies.driveCapacityMb`). Falls back to
// the default 100 MiB when no role checker is wired (#1769)。
func (s *Service) Capacity(userID string) int64 {
	mb := 100.0 // DefaultPolicies driveCapacityMb
	if s.roleChecker != nil {
		// **policyNumber を通す。** 素の `.(int)` だと小数の policy で
		// 型アサーションに失敗し、既定の 100MB を返してしまう。gate 側は
		// 小数を尊重するので、表示だけが実際の上限とずれる (#2611)。
		if v, ok := policyNumber(s.roleChecker.GetUserPolicies(userID)["driveCapacityMb"]); ok {
			mb = v
		}
	}
	return safemath.MulFloat64(mb, 1024*1024)
}

// UpdateInput holds the editable fields of a drive file.
type UpdateInput struct {
	Name        *string
	Comment     **string
	FolderID    **string
	IsSensitive *bool
}

// Update applies the non-nil fields to a drive file owned by the user.
// Owner-only: moderator が他ユーザーの file を編集できないよう
// findOwnedFile() を使う (#499)。
//
// upstream drive/files/update.ts は `!isModerator && file.userId !== me.id` の
// ときのみ ACCESS_DENIED とし、moderator は他人の file を編集できる (markSensitive
// の moderationLog も残す)。mk-go はこの moderator write bypass を**意図的に
// 排除**している (#499 で privilege-escalation として hardening 済)。moderator が
// 他ユーザーの file を moderation する正規経路は /admin/drive/* で、本 endpoint は
// owner-only を維持する。これは upstream からの deliberate deviation (#1948-16)。
func (s *Service) Update(user *model.User, id string, in UpdateInput) (*model.DriveFile, error) {
	f, err := s.findOwnedFile(user, id)
	if err != nil {
		return nil, err
	}
	fields := map[string]any{}
	if in.Name != nil {
		// upstream DriveService.updateFile は values.name に validateFileName
		// を実行し、失敗時 InvalidFileNameError を投げる (#1564)。
		if !ValidateFileName(*in.Name) {
			return nil, ErrInvalidFileName
		}
		fields["name"] = *in.Name
	}
	if in.Comment != nil {
		fields["comment"] = *in.Comment
	}
	if in.FolderID != nil {
		// 宛先 folder の所有権チェック。upstream files/update は宛先 folder を
		// findOneBy({id, userId}) で引き、他人所有も「不在」として
		// NoSuchFolderError(ea8fb7a5) を返す。owner mismatch を ACCESS_DENIED に
		// すると folder 存在 oracle になるため ErrFolderNotFound に統一する (#1908)。
		if *in.FolderID != nil {
			folder, err := s.folderRepo.FindByID(**in.FolderID)
			if err != nil {
				// **DB 障害を not-found に丸めない** (#2799)。
				if !repository.IsNotFound(err) {
					return nil, err
				}
				return nil, ErrFolderNotFound
			}
			if folder.UserID == nil || *folder.UserID != user.ID {
				return nil, ErrFolderNotFound
			}
		}
		fields["folderId"] = *in.FolderID
	}
	if in.IsSensitive != nil {
		// alwaysMarkNsfw role policy が true の user は isSensitive を false
		// に変更できない (upstream DriveService.updateFile と同 logic、#1028)。
		// 自分自身の policy なので file.userId == user.ID 前提で role を見る。
		//
		// moderator bypass はしない (= 上の findOwnedFile() が moderator にも
		// owner-only を強制する設計と整合)。admin が自分にも alwaysMarkNsfw を
		// 持つ場合、自分の file も例外なく sensitive 維持する (= self-restriction、
		// upstream は file.userId 起点で policies を見る semantics で同挙動)。
		// 「自分の role を override したい admin は role 編集経由で剥がす」が
		// 公式の脱出経路。i/update の admin bypass とは目的が異なる: i/update
		// は profile field 設定の意思表明への gate、drive Update は実 file 属性
		// の操作。
		if !*in.IsSensitive && f.IsSensitive && s.roleChecker != nil {
			policies := s.roleChecker.GetUserPolicies(user.ID)
			if policies != nil {
				if v, ok := policies["alwaysMarkNsfw"].(bool); ok && v {
					return nil, ErrCannotUnmarkSensitive
				}
			}
		}
		fields["isSensitive"] = *in.IsSensitive
	}
	if err := s.fileRepo.Update(f.ID, fields); err != nil {
		return nil, err
	}
	updated, err := s.fileRepo.FindByID(f.ID)
	if err != nil {
		return nil, err
	}
	s.publishEvent(user.ID, "fileUpdated", entity.PackDriveFileSelf(updated, s.idGen))
	return updated, nil
}

// Delete removes a file from storage and the database.
// Owner-only: moderator が他ユーザーの file を削除できないよう
// findOwnedFile() を使う (#499)。
func (s *Service) Delete(user *model.User, id string) error {
	f, err := s.findOwnedFile(user, id)
	if err != nil {
		return err
	}
	if err := s.deleteFileObjects(f); err != nil {
		return err
	}
	if err := s.fileRepo.Delete(f); err != nil {
		return err
	}
	// upstream は fileDeleted に file.id (文字列) だけを送る (#2002)。
	s.publishEvent(user.ID, "fileDeleted", f.ID)
	if s.chartHook != nil {
		s.chartHook.OnFileDeleted(f)
	}
	return nil
}

// DeleteSystemFile removes a system-owned file (userId IS NULL) by id (#2966).
//
// **所有者チェックを通せないので別の口にする。** `Delete` は
// `findOwnedFile` が user を要求するが、絵文字の実体として作るファイルは
// 誰のものでもない。代わりに**「利用者のファイルではないこと」を確かめる** —
// id を取り違えたときに利用者のファイルを消さないため。
//
// 承認が途中で失敗したときの後始末に使う。存在しない id は成功として扱う
// (二重に呼ばれても壊れない)。
func (s *Service) DeleteSystemFile(id string) error {
	if id == "" {
		return nil
	}
	f, err := s.fileRepo.FindByID(id)
	if err != nil {
		if repository.IsNotFound(err) {
			return nil
		}
		return err
	}
	if f.UserID != nil || f.UserHost != nil {
		// **利用者のファイルは消さない。** 呼び出し側が id を取り違えたときに
		// 申請者のファイルを消すのが最悪の壊れ方なので、ここで止める。
		//
		// **`userHost` も見る (レビュー M1)。** `userId IS NULL` かつ
		// `userHost` 付きの行は、著者をまだ materialize していないリモートの
		// 添付ファイル (#2717) で、**表示中の note が参照している**。
		// `orphanWhere` (`internal/repository/drive_file.go`) も同じ理由で
		// `"userHost" IS NULL` を要求しており、そちらより緩いと守る範囲が
		// 食い違う。
		return ErrAccessDenied
	}
	if err := s.deleteFileObjects(f); err != nil {
		return err
	}
	if err := s.fileRepo.Delete(f); err != nil {
		return err
	}
	if s.chartHook != nil {
		s.chartHook.OnFileDeleted(f)
	}
	// **イベントは出さない。** `publishEvent` は利用者の stream 宛てで、
	// system ファイルには宛先が無い。
	return nil
}

// DeleteAllByHost physically deletes every drive file uploaded from the given
// remote host and decrements drive usage charts, then bulk-deletes the rows.
// Used by admin/federation/delete-all-files (upstream iterates driveService
// .deleteFile per file). Unlike Delete it performs no owner check (an admin
// purges another instance's files). Storage deletes are best-effort so a single
// missing object does not abort the purge; the chart hook decrements usage per
// file. Returns the number of rows deleted (#1772)。
func (s *Service) DeleteAllByHost(host string) (int64, error) {
	if host == "" {
		return 0, nil
	}
	files, err := s.fileRepo.ListByHost(host)
	if err != nil {
		return 0, err
	}
	for _, f := range files {
		if derr := s.deleteFileObjects(f); derr != nil {
			slog.Warn("drive: delete-all-by-host storage delete failed", "host", host, "fileId", f.ID, "err", derr)
		}
		// usedRemote / instanceChart 等の使用量を per-file で減算する。
		if s.chartHook != nil {
			s.chartHook.OnFileDeleted(f)
		}
	}
	return s.fileRepo.DeleteByHost(host)
}

// CreateFolder creates a new drive folder owned by user.
func (s *Service) CreateFolder(user *model.User, name string, parentID *string) (*model.DriveFolder, error) {
	if user == nil {
		return nil, errors.New("user is required")
	}
	if parentID != nil {
		parent, err := s.folderRepo.FindByID(*parentID)
		if err != nil {
			// **DB 障害を not-found に丸めない** (#2799)。
			if !repository.IsNotFound(err) {
				return nil, err
			}
			return nil, ErrFolderNotFound
		}
		// upstream folders/create は parent を findOneBy({id, userId: me.id}) で
		// 引き、他人所有 parent も「不在」として NO_SUCH_FOLDER を返す (#1831)。
		if parent.UserID == nil || *parent.UserID != user.ID {
			return nil, ErrFolderNotFound
		}
	}
	userID := user.ID
	f := &model.DriveFolder{
		ID:       s.idGen.Generate(time.Now()),
		Name:     name,
		UserID:   &userID,
		ParentID: parentID,
	}
	if err := s.folderRepo.Create(f); err != nil {
		return nil, err
	}
	// upstream folders/create は publishDriveStream(me.id, 'folderCreated',
	// folderObj) を発火する (#1564)。body は packed folder。
	s.publishFolderEvent(user.ID, "folderCreated", entity.PackDriveFolder(f, s.idGen))
	return f, nil
}

// ShowFolder returns the folder with id, ensuring the user owns it.
func (s *Service) ShowFolder(user *model.User, id string) (*model.DriveFolder, error) {
	if user == nil {
		return nil, errors.New("user is required")
	}
	f, err := s.folderRepo.FindByID(id)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrFolderNotFound
	}
	// upstream folders/{show,update,delete} は findOneBy({id, userId: me.id}) で
	// 他人所有 folder を「不在」と区別せず NO_SUCH_FOLDER(404) を返す。owner
	// mismatch を ACCESS_DENIED(403) にすると folder ID 存在 oracle になる (mild
	// IDOR)。ErrFolderNotFound に統一して 404 に揃える (#1831)。
	if f.UserID == nil || *f.UserID != user.ID {
		return nil, ErrFolderNotFound
	}
	return f, nil
}

// UpdateFolderInput holds the editable fields of a drive folder.
type UpdateFolderInput struct {
	Name     *string
	ParentID **string
}

// UpdateFolder applies the non-nil fields to a folder owned by user.
func (s *Service) UpdateFolder(user *model.User, id string, in UpdateFolderInput) (*model.DriveFolder, error) {
	f, err := s.ShowFolder(user, id)
	if err != nil {
		return nil, err
	}
	fields := map[string]any{}
	if in.Name != nil {
		fields["name"] = *in.Name
	}
	if in.ParentID != nil {
		if *in.ParentID != nil {
			// upstream folders/update: parentId === folder.id は即
			// recursiveNesting (#1564)。
			if **in.ParentID == f.ID {
				return nil, ErrRecursiveNesting
			}
			parent, err := s.folderRepo.FindByID(**in.ParentID)
			if err != nil {
				// **DB 障害を not-found に丸めない** (#2799)。
				if !repository.IsNotFound(err) {
					return nil, err
				}
				// folders/update では parent が未存在のときに upstream は
				// NO_SUCH_PARENT_FOLDER (#977) を返すので、target 不在の
				// ErrFolderNotFound と区別する。
				return nil, ErrParentFolderNotFound
			}
			// upstream folders/update は parent を findOneBy({id, userId: me.id})
			// で引き、他人所有 parent も「不在」として NO_SUCH_PARENT_FOLDER を
			// 返す (#1831、update.ts の noSuchParentFolder)。
			if parent.UserID == nil || *parent.UserID != user.ID {
				return nil, ErrParentFolderNotFound
			}
			// upstream checkCircle: 新 parent の祖先 chain に folder 自身が
			// いたら循環 (#1564)。mk-go は過去に循環を作れてしまっていたため、
			// 既存の循環 data でも無限 loop しないよう visited set で打ち切る。
			if ok, err := s.folderAncestorContains(parent.ParentID, f.ID); err != nil {
				return nil, err
			} else if ok {
				return nil, ErrRecursiveNesting
			}
		}
		fields["parentId"] = *in.ParentID
	}
	if err := s.folderRepo.Update(f.ID, fields); err != nil {
		return nil, err
	}
	updated, err := s.folderRepo.FindByID(f.ID)
	if err != nil {
		return nil, err
	}
	// upstream folders/update は publishDriveStream(me.id, 'folderUpdated',
	// folderObj) を発火する (#1564)。
	s.publishFolderEvent(user.ID, "folderUpdated", entity.PackDriveFolder(updated, s.idGen))
	return updated, nil
}

// folderAncestorContains walks the parent chain starting at startParentID and
// reports whether targetID appears in it (upstream folders/update の
// checkCircle 相当)。walk 中の FindByID 失敗は「循環なし」に潰さず error と
// して伝播する: 一時的な DB 障害で循環チェックを skip して循環 parentId を
// commit してしまうと、read 経路の parent 再帰が壊れる (upstream の
// findOneByOrFail も request ごと失敗させる)。なお DeleteFolder は子持ち
// folder を消せないため dangling parentId は通常発生しない。
func (s *Service) folderAncestorContains(startParentID *string, targetID string) (bool, error) {
	visited := map[string]struct{}{}
	cur := startParentID
	for cur != nil {
		if *cur == targetID {
			return true, nil
		}
		// 既存の循環 data に当たっても無限 loop しない。
		if _, seen := visited[*cur]; seen {
			return false, nil
		}
		visited[*cur] = struct{}{}
		anc, err := s.folderRepo.FindByID(*cur)
		if err != nil {
			return false, fmt.Errorf("walk folder ancestors: %w", err)
		}
		cur = anc.ParentID
	}
	return false, nil
}

// DeleteFolder removes an empty folder owned by user.
func (s *Service) DeleteFolder(user *model.User, id string) error {
	f, err := s.ShowFolder(user, id)
	if err != nil {
		return err
	}
	hasChildren, err := s.folderRepo.HasChildren(f.ID)
	if err != nil {
		return err
	}
	if hasChildren {
		return ErrFolderNotEmpty
	}
	if err := s.folderRepo.Delete(f); err != nil {
		return err
	}
	// upstream folders/delete は publishDriveStream(me.id, 'folderDeleted',
	// folder.id) を発火する (#1564)。body は packed folder ではなく id 文字列。
	s.publishFolderEvent(user.ID, "folderDeleted", f.ID)
	return nil
}

// randReader is the source of randomness for newAccessKey. Tests override
// this to exercise the error path.
var randReader io.Reader = rand.Reader

// newAccessKey returns a random 32-character hex string used as the storage
// access key (and the URL path component for LocalStorage).
func newAccessKey() (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(randReader, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// mimeAllowedByPolicy reports whether mime matches any pattern in the
// `uploadableFileTypes` role policy. raw は GetUserPolicies map から取り出した
// any 値で、想定は `[]string` または `[]any` (JSON unmarshal 経由)。型不一致や
// 空 / nil は **deny** ではなく upstream 互換のため **allow** に倒す (= policy
// 未設定 / 未集約のときは制限を加えない fail-soft)。
//
// パターン:
//   - "*" / "*/*" → 全許可
//   - "image/*" など末尾 "/*" → prefix match (= "image/" で始まる)
//   - それ以外 → 完全一致
func mimeAllowedByPolicy(mime string, raw any) bool {
	patterns := normalizeMimePatterns(raw)
	// 取り出せなかった (= policy 未集約) ケースは fail-soft で許可。
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		switch {
		case p == "*" || p == "*/*":
			return true
		case strings.HasSuffix(p, "/*"):
			if strings.HasPrefix(mime, p[:len(p)-1]) {
				return true
			}
		default:
			if mime == p {
				return true
			}
		}
	}
	return false
}

// normalizeMimePatterns coerces the `uploadableFileTypes` policy value into
// a []string. Accepts []string (DefaultPolicies), []any (JSON unmarshal),
// or nil. 各 entry は trim され、空文字は除外する (upstream の `type.trim()
// === ” continue` と同 logic)。
func normalizeMimePatterns(raw any) []string {
	switch v := raw.(type) {
	case nil:
		return nil
	case []string:
		out := make([]string, 0, len(v))
		for _, s := range v {
			s = strings.TrimSpace(s)
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				s = strings.TrimSpace(s)
				if s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	default:
		return nil
	}
}

// HasRoleChecker reports whether the role checker was wired.
//
// **1 つの nil で 4 つの制限が同時に外れる** — MIME 許可リスト、NSFW 強制、
// ファイルサイズ上限、容量上限。起動時検査に使う (#2683)。
func (s *Service) HasRoleChecker() bool { return s.roleChecker != nil }
