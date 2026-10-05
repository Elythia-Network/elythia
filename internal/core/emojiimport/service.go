// Package emojiimport implements the Misskey-compatible "admin/emoji/
// import-zip" flow: read a previously-uploaded ZIP from Drive, extract each
// emoji image + meta.json, and register them as local custom emojis.
package emojiimport

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"regexp"
	"time"

	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/misc/colfit"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// Errors returned by Importer.
var (
	// ErrDriveFileNotFound is returned when the referenced fileId cannot be
	// loaded from Drive.
	ErrDriveFileNotFound = errors.New("drive file not found")
	// ErrUserNotFound is returned when the requesting admin user is absent.
	ErrUserNotFound = errors.New("user not found")
	// ErrInvalidZip is returned when the body cannot be parsed as a ZIP.
	ErrInvalidZip = errors.New("invalid zip archive")
	// ErrMissingMeta is returned when meta.json is not present in the archive.
	ErrMissingMeta = errors.New("meta.json missing in archive")
	// ErrMalformedMeta is returned when meta.json cannot be parsed as JSON.
	ErrMalformedMeta = errors.New("meta.json malformed")
)

// DriveReader fetches a previously-uploaded DriveFile by id and returns its
// raw bytes. 本家 Misskey の DownloadService.downloadUrl 相当。importer は file
// の URL を解決せず byte body だけを必要とする。
type DriveReader interface {
	Fetch(fileID string) (*model.DriveFile, []byte, error)
}

// Deps bundles the dependencies needed by Importer.
// DecorationCacheInvalidator drops the in-process map used to resolve
// emoji-backed avatar decorations (#2975).
//
// **zip インポートは `publishEmoji*` を通らない。** admin の絵文字 endpoint は
// どれもあの helper を経由するのでそちらで捨てているが、この経路は
// `EmojiRepo` を直に叩くので自分で捨てる必要がある。捨てないと、インポート
// 直後の絵文字は TTL のあいだ装着しても表示されず、置き換えで消えた絵文字は
// TTL のあいだプロフィールに残る (#2258 と同じ形)。
type DecorationCacheInvalidator interface {
	Invalidate()
}

type Deps struct {
	UserRepo  repository.UserRepository
	EmojiRepo repository.EmojiRepository
	Drive     DriveReader
	Uploader  *drive.Service
	IDGen     id.Generator
	// DecorationCache は未配線でも動く (TTL 分だけ反映が遅れる)。
	DecorationCache DecorationCacheInvalidator
}

// Importer runs the emoji ZIP import process.
type Importer struct {
	deps Deps
	now  func() time.Time
}

// NewImporter constructs an Importer.
func NewImporter(deps Deps) *Importer {
	return &Importer{deps: deps, now: time.Now}
}

// SetNow overrides the clock. Tests only.
func (i *Importer) SetNow(fn func() time.Time) {
	if fn != nil {
		i.now = fn
	}
}

// Result captures per-batch counters surfaced to the caller.
type Result struct {
	Total    int
	Imported int
	Skipped  int
}

// metaDoc mirrors the `meta.json` produced by Misskey's custom-emoji
// export. 本家 ExportCustomEmojisProcessorService が出力する構造を参照した。
// emoji サブオブジェクトの全フィールドを保持するが、互換性のため欠けていても
// 許容する (ゼロ値で扱う)。
type metaDoc struct {
	MetaVersion int          `json:"metaVersion"`
	Host        *string      `json:"host"`
	ExportedAt  string       `json:"exportedAt"`
	Emojis      []metaRecord `json:"emojis"`
}

type metaRecord struct {
	FileName   string    `json:"fileName"`
	Downloaded bool      `json:"downloaded"`
	Emoji      metaEmoji `json:"emoji"`
}

type metaEmoji struct {
	Name        string   `json:"name"`
	Category    *string  `json:"category"`
	Aliases     []string `json:"aliases"`
	License     *string  `json:"license"`
	IsSensitive bool     `json:"isSensitive"`
	LocalOnly   bool     `json:"localOnly"`
}

// maxImportNameLength は meta.json 中の名前の長さ上限。upstream
// ImportCustomEmojisProcessorService の `MAX_NAME_LENGTH` と同じ。
// **これだけでは列を守れない** (下記) が、桁違いに長い名前をここで落とせば、
// 画像処理と storage 書き込みが 1 件ぶん無駄に走るのを避けられる。
const maxImportNameLength = 255

// validFileName matches Misskey's ImportCustomEmojisProcessorService
// `FILE_NAME_PATTERN` (2026.9.0 で厳格化された)。旧 upstream の
// `^[a-zA-Z0-9_]+?([a-zA-Z0-9.]+)?$` は `a..png` や `a.` を通していた。
var validFileName = regexp.MustCompile(`^[a-zA-Z0-9_]+(\.[a-zA-Z0-9]+)*$`)

// validEmojiName matches Misskey's `EMOJI_NAME_PATTERN`.
var validEmojiName = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// validImportName は upstream の isValidName 相当 (長さ + パターン)。
//
// **`maxImportNameLength` は upstream から移した 255。** upstream のコメントが
// 書いているとおり**ファイルシステムのファイル名長**の上限で、列幅ではない。
// `emoji.name` は varchar(128) と狭いので、絵文字名には `emojiNameMaxRunes` を
// 別に掛ける (#3021)。
//
// **`fileName` 側は塞ぎきれていない。** `Upload` は `CorrectFilename` で拡張子を
// 足すので、255 バイトの名前は `drive_file.name` varchar(256) を超えて 22001 に
// なる (実測で 259 バイト)。**破壊的ではない** — 取り込みが先なので既存の絵文字は
// 残り、そのレコードが skip されるだけ。
//
// **`drive/files/create` は塞がっている** — `ValidateFileName` が 200 rune で
// 止めるので、拡張子を足しても列に収まる。`drive.Service.Upload` 自体は名前を
// 見ないので、**長さの決まらない名前が届くのは `drive/files/upload-from-url`
// (`path.Base` をそのまま渡す) とここの 2 つ** (数え方: 非テストの `Upload`
// 呼び出し 7 箇所から、`ValidateFileName` を通るもの・生成した名前を渡すもの・
// 呼び出し元が絵文字名の規則で 128 rune に縛るもの (`CopyToSystemFile` 経由) を
// 除いた残り)。別 issue。
func validImportName(v string, pattern *regexp.Regexp) bool {
	return len(v) <= maxImportNameLength && pattern.MatchString(v)
}

// 列幅は migration/000001_initial の `emoji` テーブル定義に対応する (#3021)。
// 変えるときは DDL と揃えること — DDL 側の実値は
// `internal/repository/emoji_column_limits_test.go` が `information_schema` と
// 突き合わせ、こちらの定数は `column_fit_test.go` の at-limit ケースが固定する
// (あちらは独立した数字で入力を作るので、定数だけ動かすと落ちる)。
//
// **`internal/api/admin` が同じ値を別に持っている。** あちらは API のリクエストを、
// こちらは `meta.json` を見るので、共有せずに DDL と突き合わせる形で揃えてある。
const (
	emojiNameMaxRunes     = 128
	emojiCategoryMaxRunes = 128
	emojiAliasMaxRunes    = 128
	emojiLicenseMaxRunes  = 1024
	// `emoji.originalUrl` / `publicUrl` (#3023)。`drive_file.url` は varchar(1024)
	// と広いので、保存先次第で超えうる。
	emojiURLMaxRunes = 512
)

// fitsColumns reports whether the record's body values can be stored, and
// returns the aliases with unstorable elements dropped (#3021)。
//
// **本文はレコードごと skip、alias は要素ごと落とす。** zip は複数レコードの一括
// 処理なので、1 件のために全体を落とさない。alias を切ると別の名前になって
// リアクションの照合に使えないので、その要素だけ落とす (`admin/emoji/*` と同じ規則)。
//
// **NUL も落とす。** 長さに関わらず列に入らない (本番の pgx extended protocol では
// SQLSTATE 22021) ので、通すと同じ書き込みに乗っている他の列まで巻き添えになる。
func fitsColumns(e metaEmoji) ([]string, string, bool) {
	if !colfit.Fits(e.Name, emojiNameMaxRunes) {
		return nil, "emoji name", false
	}
	if e.Category != nil && !colfit.Fits(*e.Category, emojiCategoryMaxRunes) {
		return nil, "category", false
	}
	if e.License != nil && !colfit.Fits(*e.License, emojiLicenseMaxRunes) {
		return nil, "license", false
	}
	out := make([]string, 0, len(e.Aliases))
	for _, a := range e.Aliases {
		a = colfit.ToStorable(a)
		if a == "" || !colfit.Fits(a, emojiAliasMaxRunes) {
			continue
		}
		out = append(out, a)
	}
	return out, "", true
}

// truncateForLog は upstream の同名ヘルパー相当。**上限を移植したなら log 側も要る** —
// meta.json は 64MiB まで許すので、1 レコードに数十 MiB の名前を入れればそのまま
// 1 行のログとして出てしまう。
func truncateForLog(v string) string {
	if len(v) <= maxImportNameLength {
		return v
	}
	return v[:maxImportNameLength] + "..."
}

// Run executes the import for the given user/file. Per-item errors are logged
// and counted as Skipped so that a single malformed entry does not abort the
// batch. 本家と同様「meta.json が壊れていれば job 全体失敗、個別エントリは
// スキップ」の方針。
func (i *Importer) Run(ctx context.Context, userID, fileID string) (*Result, error) {
	if i.deps.Drive == nil || i.deps.Uploader == nil || i.deps.EmojiRepo == nil || i.deps.UserRepo == nil || i.deps.IDGen == nil {
		return nil, errors.New("emoji importer not fully configured")
	}
	user, err := i.deps.UserRepo.FindByID(userID)
	// **DB 障害を not-found に丸めない** (#2799)。`emoji_import` processor は
	// `ErrUserNotFound` で `ErrSkipRetry` を付けるので、丸めると**瞬断中に走った
	// ジョブが retry されず恒久破棄される**。
	if err != nil && !repository.IsNotFound(err) {
		return nil, err
	}
	if err != nil || user == nil {
		return nil, fmt.Errorf("%w: %s", ErrUserNotFound, userID)
	}

	_, body, err := i.deps.Drive.Fetch(fileID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDriveFileNotFound, err)
	}

	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidZip, err)
	}

	// fileName → *zip.File の索引を作っておくと、meta.json の record.fileName から
	// ZIP 内のエントリを O(1) で引ける。
	index := make(map[string]*zip.File, len(zr.File))
	var metaFile *zip.File
	for _, f := range zr.File {
		// ディレクトリエントリ (末尾 `/`) は対象外。`path.Base("meta.json/")` が
		// `meta.json` になるため、ここで弾かないと空のディレクトリを meta.json
		// として採用して ErrMalformedMeta になる。
		if f.FileInfo().IsDir() {
			continue
		}
		base := path.Base(f.Name)
		if base == "meta.json" {
			metaFile = f
			continue
		}
		index[base] = f
	}
	if metaFile == nil {
		return nil, ErrMissingMeta
	}

	metaBytes, err := readZipEntry(metaFile, maxMetaJSONBytes)
	if err != nil {
		return nil, fmt.Errorf("read meta.json: %w", err)
	}
	var meta metaDoc
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedMeta, err)
	}

	result := &Result{Total: len(meta.Emojis)}
	for _, record := range meta.Emojis {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if !record.Downloaded {
			result.Skipped++
			continue
		}
		if !validImportName(record.FileName, validFileName) {
			slog.Warn("emoji import: invalid filename", "filename", truncateForLog(record.FileName))
			result.Skipped++
			continue
		}
		if !validImportName(record.Emoji.Name, validEmojiName) {
			slog.Warn("emoji import: invalid emoji name", "name", truncateForLog(record.Emoji.Name))
			result.Skipped++
			continue
		}
		// **列に入らない値はここで落とす (#3021)。** 名前の検査と並べるのは、
		// ここが**何も読まず何も書かないうち**だから。`replaceEmoji` まで持って
		// いくと、弾いたレコードごとに孤児の drive ファイルが残り、消した行を戻す
		// 経路 (それ自体が失敗しうる) にも頼ることになる。
		// **テストが固定しているのは「弾いたら drive に何も作らない」まで** —
		// zip の展開を省くこと自体は観測できない (`maxImportNameLength` が避けて
		// いるのと同じ無駄なので、位置は前のほうがよい)。
		aliases, badField, ok := fitsColumns(record.Emoji)
		if !ok {
			slog.Warn("emoji import: value does not fit the column",
				"name", truncateForLog(record.Emoji.Name), "field", badField)
			result.Skipped++
			continue
		}

		entry := index[record.FileName]
		if entry == nil {
			slog.Warn("emoji import: missing zip entry", "filename", truncateForLog(record.FileName))
			result.Skipped++
			continue
		}
		imgBody, err := readZipEntry(entry, maxEmojiImageBytes)
		if err != nil {
			slog.Warn("emoji import: read entry failed", "filename", truncateForLog(record.FileName), "err", err)
			result.Skipped++
			continue
		}

		if err := i.replaceEmoji(ctx, record, imgBody, aliases); err != nil {
			slog.Warn("emoji import: replace failed", "name", truncateForLog(record.Emoji.Name), "err", err)
			result.Skipped++
			continue
		}
		result.Imported++
	}
	return result, nil
}

// Uncompressed size caps for zip entries, matching upstream Misskey's
// `src/misc/zip.ts` (2026.9.0).
//
// **var にしてあるのはテストから下げるため。** const のままだと、上限が
// 呼び出し側に配線されていることを検証するのに 64MiB の fixture が要る。
var (
	maxMetaJSONBytes   int64 = 64 << 20 // 64MiB
	maxEmojiImageBytes int64 = 32 << 20 // 32MiB
)

// ErrZipEntryTooLarge is returned when a zip entry's uncompressed size exceeds
// the cap for its kind.
var ErrZipEntryTooLarge = errors.New("zip entry exceeds the uncompressed size limit")

// readZipEntry reads an entry body into memory, refusing anything whose
// uncompressed size exceeds maxBytes.
//
// **ヘッダ値と実際に読んだバイト数の両方で見る。** 旧実装は `io.ReadAll` で
// 上限なしに読んでおり、コメントも「絵文字画像は高々数百KB なので全量展開で
// 問題ない」としていたが、**中身を作るのは攻撃者**という前提が抜けていた。
// deflate のゼロ埋めは 1000:1 を超える圧縮率が出るので、Drive の上限に収まる
// ZIP から巨大な単一 allocation を起こせる。
//
// ヘッダ (`UncompressedSize64`) が主。実バイト数の打ち切りは**保険**で、
// Go の `archive/zip` は宣言サイズと実データの不一致を読み出しの時点で
// `zip: not a valid zip file` として弾くので、現状ここには到達しない
// (実測: サイズ欄を偽装した zip は 0 バイトも読めずに失敗する)。
// stdlib の挙動に依存しない形にしておくために両方置く。upstream の zip.ts も
// ヘッダ値と実書き出しバイト数の 2 段構えにしている。
func readZipEntry(f *zip.File, maxBytes int64) ([]byte, error) {
	if f.UncompressedSize64 > uint64(maxBytes) {
		return nil, ErrZipEntryTooLarge
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrZipEntryTooLarge
	}
	return data, nil
}

// replaceEmoji uploads the image to Drive as a new DriveFile, then swaps the
// same-named local emoji for a fresh row. Run still resolves the requesting user
// up front to fail-fast on missing accounts, but the user is *not* propagated
// here because the resulting drive file is system-owned (see Upload comment
// below for #670 rationale).
//
// **消すのは「置き換える中身が用意できてから」(#3021)。** 旧実装は先に削除して
// いたので、画像の取り込みでも `Create` でも失敗すると**元の絵文字が戻らない**まま
// job が終わっていた。取り込みを先に済ませ、削除と作成を隣に置き、`Create` が
// 失敗したら消した行を戻す。
func (i *Importer) replaceEmoji(ctx context.Context, record metaRecord, body []byte, aliases []string) error {
	// **捨てるのは defer 1 本にする。** delete と create の 2 箇所に書くと、
	// 片方を消しても「この関数はどこかで捨てている」という静的な検査を
	// 素通りする (実測)。**何も書かずに return する経路もある** (取り込みの失敗、
	// 既存行の lookup 失敗、削除に失敗して元の行が残っているとき、#3021 で増えた)
	// が、倒す向きは「余分に捨てる」なので安全側。
	defer i.invalidateDecorationCache()

	// emoji import zip で展開された画像は upstream Misskey TS と同じく
	// system 所有 drive file (User: nil) として保存する (#670)。custom emoji
	// はインスタンス管理アセットであり、import を実行した admin 個人の drive
	// に紐付けると ロール変更 / アカウント削除 で巻き込まれて表示が壊れる。
	// User: nil の経路では drive.Service.Upload が md5 dedup を skip するため
	// Force: true は実質 no-op だが、明示性のため残す。
	uploaded, err := i.deps.Uploader.Upload(ctx, drive.UploadInput{
		Body:  body,
		Name:  record.FileName,
		Force: true,
	})
	if err != nil {
		return fmt.Errorf("upload emoji image: %w", err)
	}

	publicURL := uploaded.URL
	if uploaded.WebpublicURL != nil && *uploaded.WebpublicURL != "" {
		publicURL = *uploaded.WebpublicURL
	}
	// **URL が列に入るかを見る (#3023)。** `drive_file.url` は varchar(1024) だが
	// `emoji.originalUrl` / `publicUrl` は varchar(512)。保存先の URL は
	// `objectStorageBaseUrl` + prefix + `accessKey` で決まるので、長い prefix の
	// 構成では超える。通すと `Create` が 22001 で落ち、**その時点で元の絵文字は
	// まだ消していない**とはいえ復元経路に頼ることになる。
	//
	// **取り込んだ実体は消さない。** この時点では絵文字の行に一切触っていないので
	// 未参照なのは確実だが、この関数は upload を消す経路を持たない (#3021 と同じ形)。
	// **100 件の zip が全滅すれば 100 個の孤児が残り**、回収は手動の
	// `admin/drive/cleanup` だけ。
	//
	// **ここまで来ないことが多い** — `Put` が返す URL は `base (+ prefix) + accessKey`
	// (32 桁の hex 固定) なので `url` / `thumbnailUrl` / `webpublicUrl` の長さは必ず
	// 同じで、`drive_file` 側の後ろ 2 つは varchar(512)。つまりサムネイルを作る画像
	// (decode できるものはすべて) は上の `Upload` が先に落ちる。同じ理由で
	// `publicURL` 側の判定も現状は冗長だが、導出を変えたときに素通りさせないために
	// 両方見る。
	//
	// **数えるのは rune。** `colfit.Fits` は PostgreSQL の varchar(n) と同じ
	// 「n 文字」で見る (byte ではない)。NUL も入らないので、長さが収まっていても
	// 落ちることがある。
	if !colfit.Fits(uploaded.URL, emojiURLMaxRunes) || !colfit.Fits(publicURL, emojiURLMaxRunes) {
		return fmt.Errorf(
			"drive file url does not fit emoji.originalUrl / publicUrl (max %d runes; url=%d, publicUrl=%d)",
			emojiURLMaxRunes, len([]rune(uploaded.URL)), len([]rune(publicURL)))
	}
	fileType := uploaded.Type
	if uploaded.WebpublicType != nil && *uploaded.WebpublicType != "" {
		fileType = *uploaded.WebpublicType
	}

	now := i.now()
	// 列に入らない要素は `fitsColumns` が落とし済み (#3021)。
	// 不変条件 (#722): emoji.originalUrl は drive_file.url と一致させる。
	// `DriveFileRepository.DeleteOrphans` の cleanup guard は
	// `NOT EXISTS (emoji.originalUrl = drive_file.url ...)` で system 所有
	// emoji 画像を保護しているので、別 URL (webpublic 等) を originalUrl に
	// 入れると cleanup で消える。
	emoji := &model.Emoji{
		ID:          i.deps.IDGen.Generate(now),
		UpdatedAt:   &now,
		Name:        record.Emoji.Name,
		Host:        nil,
		Category:    record.Emoji.Category,
		OriginalURL: uploaded.URL,
		PublicURL:   publicURL,
		Type:        &fileType,
		Aliases:     model.StringArray(aliases),
		License:     record.Emoji.License,
		IsSensitive: record.Emoji.IsSensitive,
		LocalOnly:   record.Emoji.LocalOnly,
	}
	// 既存 local 絵文字 (同名) は上書きのために削除する。
	// 本家 Misskey では emojisRepository.delete({ name, host: IsNull() }) 相当。
	//
	// **DB 障害を「同名なし」に丸めない。** 丸めると、接続断のあいだ同名の行が
	// 2 つできる (local emoji は `host IS NULL` なので一意制約が効かない)。
	var replaced *model.Emoji
	existing, err := i.deps.EmojiRepo.FindByNameAndHost(record.Emoji.Name, nil)
	if err != nil && !repository.IsNotFound(err) {
		return fmt.Errorf("look up existing emoji: %w", err)
	}
	if err == nil && existing != nil {
		if derr := i.deps.EmojiRepo.Delete(existing.ID); derr != nil {
			// **消えたかを読み直す (#3021 のレビュー H2)。** COMMIT の後・ack の前に
			// 接続が切れると、**消えているのにエラーが返る**。そのまま skip すると
			// 元の絵文字が消えたまま新しい行も作られず、この issue が直そうとしている
			// 被害そのものになる。
			//
			// **まだ残っている / 読み直せないなら作らない。** 作ると同名の行が 2 つ
			// 残り、どちらが引かれるか決まらない (local emoji は `host IS NULL` なので
			// 一意制約が効かない)。
			//
			// **読み直せない枝だけは「元の絵文字が残る」と断言できない** — 削除が
			// 載っていた可能性があり、その場合は消えたままになる (戻す相手も無い)。
			// 作る側と逆に倒しているのは、ここで作ると**確実に**同名 2 行を作りうる
			// から。残るのは warn 1 行なので、そこから追うことになる。
			if gone, rerr := i.deps.EmojiRepo.FindByID(existing.ID); rerr == nil && gone != nil {
				return fmt.Errorf("delete existing emoji: %w", derr)
			} else if rerr != nil && !repository.IsNotFound(rerr) {
				return fmt.Errorf("delete existing emoji: %w (read back: %w)", derr, rerr)
			}
		}
		replaced = existing
	}

	if err := i.deps.EmojiRepo.Create(emoji); err != nil {
		// **消した行を戻す (#3021)。** ここまで来ると元の絵文字は消えているので、
		// 戻さないと「import に失敗したら手元の絵文字も消えた」になる。元の行が
		// 指していた drive ファイルは触っていないので、行を入れ直せば表示も戻る。
		//
		// **戻せなかったらログに残す。** 握り潰すと、消えた絵文字がどれだったか
		// 追う手がかりが無くなる。
		if replaced != nil && i.shouldRestoreReplaced(emoji) {
			if rerr := i.deps.EmojiRepo.Create(replaced); rerr != nil {
				slog.Error("emoji import: failed to restore the replaced emoji",
					"name", truncateForLog(replaced.Name), "emojiId", replaced.ID, "err", rerr)
			}
		}
		return fmt.Errorf("create emoji row: %w", err)
	}
	return nil
}

// shouldRestoreReplaced reports whether the row deleted for this import has to
// be put back after a failed Create (#3021)。
//
// **エラーが返っても INSERT が載っていることがある。** COMMIT の後・ack の前に
// 接続が切れる窓で、載っているのに戻すと**同名の行が 2 つ**残る (local emoji は
// `host IS NULL` なので一意制約が効かず、どちらが引かれるかは id 順で決まる)。
//
// **読み直せないときは戻す。** 倒す向きが #3019 と逆なのは、残る状態が違うため —
// あちらで残るのは「参照されない複製」(孤児 cleanup が回収する) だが、こちらで
// 残らないのは**運用者が手で入れた絵文字**で、戻さなければ自動では復旧しない。
// 重複のほうは管理画面の一覧に 2 件出るので気付けて、片方を消せば済む。
func (i *Importer) shouldRestoreReplaced(created *model.Emoji) bool {
	landed, err := i.deps.EmojiRepo.FindByID(created.ID)
	if err == nil && landed != nil {
		// エラーは返ったが INSERT は載っていた。戻すと同名の行が 2 つになる。
		slog.Warn("emoji import: the emoji write landed despite the error; not restoring",
			"name", truncateForLog(created.Name), "emojiId", created.ID)
		return false
	}
	if err != nil && !repository.IsNotFound(err) {
		slog.Error("emoji import: cannot tell whether the emoji write landed; restoring anyway",
			"name", truncateForLog(created.Name), "emojiId", created.ID, "err", err)
	}
	return true
}

// invalidateDecorationCache is a nil-safe helper (unit tests leave it unwired).
func (i *Importer) invalidateDecorationCache() {
	if i.deps.DecorationCache == nil {
		return
	}
	i.deps.DecorationCache.Invalidate()
}
