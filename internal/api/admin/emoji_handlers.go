package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/jsnum"
	"github.com/elythia-network/elythia/internal/api/pagination"
	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/emojiapplication"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/safehttp"
	"github.com/labstack/echo/v4"
)

// --- Emoji Admin endpoints ---

// SetEmojiRepo attaches the emoji repository.
func (h *Handler) SetEmojiRepo(r repository.EmojiRepository) { h.emojiRepo = r }

// BroadcastPublisher emits an instance-wide stream event to every connected
// client. Implemented by *stream.BroadcastPublisher (#2046)。
type BroadcastPublisher interface {
	PublishBroadcast(eventType string, body any)
}

// SetBroadcastPublisher wires the broadcast stream publisher used to emit
// emojiAdded / emojiUpdated / emojiDeleted (#2046)。nil disables emission.
func (h *Handler) SetBroadcastPublisher(p BroadcastPublisher) { h.broadcastPub = p }

// publishEmojiAdded emits emojiAdded `{emoji}` (single packed emoji).
func (h *Handler) publishEmojiAdded(e *model.Emoji) {
	h.invalidateEmojiDecorationCache()
	if h.broadcastPub == nil {
		return
	}
	h.broadcastPub.PublishBroadcast("emojiAdded", map[string]any{"emoji": entity.PackEmojiDetailed(e)})
}

// publishEmojiUpdated emits emojiUpdated `{emojis:[...]}` (packed array).
func (h *Handler) publishEmojiUpdated(emojis ...*model.Emoji) {
	h.invalidateEmojiDecorationCache()
	if h.broadcastPub == nil || len(emojis) == 0 {
		return
	}
	packed := make([]any, len(emojis))
	for i, e := range emojis {
		packed[i] = entity.PackEmojiDetailed(e)
	}
	h.broadcastPub.PublishBroadcast("emojiUpdated", map[string]any{"emojis": packed})
}

// publishEmojiDeleted emits emojiDeleted `{emojis:[...]}` (packed array).
func (h *Handler) publishEmojiDeleted(emojis ...*model.Emoji) {
	h.invalidateEmojiDecorationCache()
	if h.broadcastPub == nil || len(emojis) == 0 {
		return
	}
	packed := make([]any, len(emojis))
	for i, e := range emojis {
		packed[i] = entity.PackEmojiDetailed(e)
	}
	h.broadcastPub.PublishBroadcast("emojiDeleted", map[string]any{"emojis": packed})
}

// publishEmojiUpdatedByIDs re-fetches the given emoji ids and emits a single
// emojiUpdated event (upstream bulk ops の packDetailedMany(ids) 相当、#2046)。
func (h *Handler) publishEmojiUpdatedByIDs(ids []string) {
	h.invalidateEmojiDecorationCache()
	if h.broadcastPub == nil || h.emojiRepo == nil || len(ids) == 0 {
		return
	}
	rows, err := h.emojiRepo.FindManyByIDs(ids)
	if err != nil || len(rows) == 0 {
		return
	}
	h.publishEmojiUpdated(rows...)
}

// EmojiAdd handles POST /api/admin/emoji/add.
//
// upstream Misskey TS と同じく `fileId` を必須とし、drive_file から URL を
// resolve して保存する。以前は `fileId` 無しで `url` だけを保存する独自の経路
// (#867) もあったが、同梱の frontend は使っておらず、本家の paramDef が拒否する
// 入力なので #3865 で消した。
//
// 画像は**system 所有の drive ファイルへ複製してから**参照する (#2999)。
// upstream は操作者のファイルをそのまま指すので意図的な乖離
// (docs/divergence/security.md §7)。
func (h *Handler) EmojiAdd(c echo.Context) error {
	var req struct {
		Name        string   `json:"name"`
		FileID      string   `json:"fileId"`
		Category    *string  `json:"category"`
		Aliases     []string `json:"aliases"`
		License     *string  `json:"license"`
		IsSensitive bool     `json:"isSensitive"`
		LocalOnly   bool     `json:"localOnly"`
		RoleIDs     []string `json:"roleIdsThatCanBeUsedThisEmojiAsReaction"`
	}
	if err := c.Bind(&req); err != nil || req.Name == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "name is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// upstream paramDef は name に `^[a-zA-Z0-9_]+$` を強制する。
	//
	// **長さも見る (#2998)。** upstream の paramDef に `maxLength` は無いので、
	// 128 文字を超える名前は `emoji.name` varchar(128) に入らず SQLSTATE 22001 が
	// 生のまま 500 になる。**利用者の入力で 5xx を立てない**のは申請経路
	// (`emojiapplication.service`) が既に採っている判断で、`admin/emoji/copy` も
	// #2998 で同じ形にした。ここだけ残すと同じ入力が endpoint 次第で 400 と 500 に
	// 分かれる。
	if !emojiNamePattern.MatchString(req.Name) || utf8.RuneCountInString(req.Name) > emojiNameMaxRunes {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid emoji name.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// **`idGen` もここで見る (#2999)。** `Generate` は複製を作った**後**に呼ぶので、
	// nil のまま到達すると panic し、誰からも参照されない複製が残る。
	// `CreateFromApplication` が同じ理由で冒頭に置いている。
	if h.emojiRepo == nil || h.idGen == nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// fileId 経路は drive_file を resolve する (NO_SUCH_FILE)。filetype 検証は
	// upstream の error 順序 (noSuchFile → duplicate → unsupportedFileType) に
	// 合わせ、duplicate チェックの後で行う。
	var driveFile *model.DriveFile
	if req.FileID != "" && h.driveFileRepo != nil {
		f, err := h.driveFileRepo.FindByID(req.FileID)
		if err != nil && !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return apierr.JSONInternalError(c)
		}
		if err != nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "No such file.", "fc46b5a4-6b92-4c33-ac66-b806659bb5cf"))
		}
		driveFile = f
	}
	if driveFile == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "fileId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// 同名のローカル emoji が既に存在する場合は DUPLICATE_NAME (upstream 互換)。
	// **重複チェックを DB 障害で skip しない** (#2792)。
	existing, dupErr := h.emojiRepo.FindByNameAndHost(req.Name, nil)
	if dupErr != nil && !repository.IsNotFound(dupErr) {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if dupErr == nil && existing != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("DUPLICATE_NAME", "Duplicate name.", "f7a3462c-4e6e-4069-8421-b9bd4f4c3975"))
	}
	// **name 以外の列も見る (#3018)。** `category` varchar(128) /
	// `license` varchar(1024) を超える値はここを抜けると `Create` で
	// SQLSTATE 22001 になり、**利用者の入力で 5xx が立つ**。名前だけ長さを見て
	// いたのは #2998 の片手落ちで、`admin/emoji/copy` は 4 列すべて見ている。
	//
	// **upstream が宣言する error より後ろに置く。** `add.ts` の
	// `noSuchFile` / `duplicateName` を先に返さないと、2 つ問題があるリクエストで
	// drop-in クライアントの分岐が変わる (`EmojiUpdate` も同じ理由で絵文字の解決より
	// 後ろに置いてある)。
	//
	// **`unsupportedFileType` より前になるのは、MIME の検査が複製と同じ所にあるから。**
	// 後ろへ出すと複製の後ろになり、400 のたびに誰からも参照されない system 所有の
	// 複製が残る (#2999 / #3014)。どちらも 400 の client error なので、この 2 つの
	// 順序が入れ替わってもクライアントから見た意味は変わらない。
	if !emojiBodyFits(req.Category, emojiCategoryMaxRunes) {
		return emojiValueTooLong(c, "category")
	}
	if !emojiBodyFits(req.License, emojiLicenseMaxRunes) {
		return emojiValueTooLong(c, "license")
	}
	// 配列も**複製より前**に確定させる。全部落ちたら弾く (#3018)。
	aliases, ok := fitEmojiAliases(req.Aliases)
	if !ok {
		return emojiValueTooLong(c, "aliases")
	}
	roleIDs, ok := fitEmojiRoleIDs(req.RoleIDs)
	if !ok {
		return emojiValueTooLong(c, "roleIdsThatCanBeUsedThisEmojiAsReaction")
	}
	// drive 画像なら MIME を allowlist で検証し、webpublic variant を優先して
	// originalUrl/publicUrl/type を導出する (upstream CustomEmojiService.add)。
	var url, publicURL string
	var fileType *string
	systemFileID := ""
	if !isAllowedEmojiImageType(driveFile.Type) {
		return c.JSON(http.StatusBadRequest, apierr.Error("UNSUPPORTED_FILE_TYPE", "Unsupported file type.", "f7599d96-8750-af68-1633-9575d625c1a7"))
	}
	// **操作者の drive ファイルを参照し続けない (#2999)。** `fileId` で渡るのは
	// ふつうモデレーター自身が直前に上げたファイルで、本人が drive から消せば
	// 絵文字の画像が壊れる (ロールの変更や退会でも同じ)。申請の承認は #2966、
	// リモートの複製と zip の取り込みは #670 で既に system 所有にしてある。
	// **`admin/emoji/update` の `fileId` も #3014 で同じ形にした** — 差し替えた
	// 先も複製してから参照するので、REST の経路はこれで揃っている。
	//
	// **元のファイルは触らない** — ノートの添付やプロフィールで使っている
	// 可能性があり、所有権を移すと利用者の drive から突然消える。
	//
	// **HTTP は経由しない** (`CopyToSystemFile`)。自分の公開 URL を叩くのは
	// SSRF ガードと衝突し、非公開 URL の構成では取れない (#2966 と同じ判断)。
	//
	// **複製は検証をすべて通した後。** 先に複製すると、重複名や MIME で弾く
	// 分まで実体を作って捨てることになる (upstream の `copy.ts` がその形で、
	// `DUPLICATE_NAME` のたびに孤児を残す)。
	src := driveFile
	if h.emojiImageFetcher != nil && !coredrive.IsSystemOwned(driveFile) {
		// **読む前に行の `size` で断る (レビュー M3/L7)。** 複製は実体を丸ごと
		// メモリへ読むので、上限超過を読み切ってから 400 にするのは無駄。申請経路も
		// `emojiapplication.checkFile` が同じ規則を申請の時点で掛けている。
		//
		// **発火するのは role policy の `maxFileSizeMb` を 32 MiB より上へ
		// 設定した (ことがある) 構成だけ** — 既定は 30 MB なので upload できた
		// 時点で必ず上限内だが、一度上げて入れた行は上限を戻しても残る。
		// **一括登録の同時実行 (同梱 frontend は最大 100 件を一度に投げる) は
		// これでは軽くならない** — 1 件あたりのピークは上限のままで、rate limit か
		// queue 化が要る (別 issue)。
		//
		// **`size` は行に書いてあるだけで実体とずれうる** (TS 由来の行や手で直した
		// 行)。**過小申告は読み出し側の `safehttp.ErrResponseTooLarge` が捕まえる**
		// が、**過大申告はここで確定する** (実体を読まずに 400 になる)。
		if int64(driveFile.Size) > MaxEmojiCopyBytes {
			return c.JSON(http.StatusBadRequest, apierr.Error(
				"EMOJI_IMAGE_TOO_LARGE", "The image is too large to register as an emoji.",
				"6b1d5f0a-3c9e-4f27-9a4d-7e2b8c1f0d64"))
		}
		ctx := c.Request().Context()
		copied, cerr := h.emojiImageFetcher.CopyToSystemFile(ctx, driveFile, req.Name, req.IsSensitive)
		if cerr == nil && copied == nil {
			// 実装の契約違反。ここで気付かないと下の `src` 参照で panic する。
			cerr = errors.New("copy returned no file")
		}
		if cerr != nil {
			slog.WarnContext(ctx, "emoji add: drive copy failed",
				"fileId", req.FileID, "name", req.Name, "err", cerr)
			// **絵文字を作らない。** 元ファイルを参照して作ると、直そうと
			// している依存をそのまま残すことになる。
			return emojiCopyFailureResponse(c, cerr, "fc46b5a4-6b92-4c33-ac66-b806659bb5cf")
		}
		// **複製した実体の MIME を見る (#2966 と同じ理由)。** `Upload` は
		// `AnalyseFile` でバイト列から型を引き直すので、行の宣言と実体が
		// ずれていると allowlist 外の型が絵文字として登録される。
		// **弾いたら片付ける** — その時点で誰からも参照されない。
		if !isAllowedEmojiImageType(copied.Type) {
			h.deleteSystemEmojiFile(ctx, copied.ID)
			return c.JSON(http.StatusBadRequest, apierr.Error("UNSUPPORTED_FILE_TYPE", "Unsupported file type.", "f7599d96-8750-af68-1633-9575d625c1a7"))
		}
		src = copied
		systemFileID = copied.ID
	}
	// **URL が列に入るかを見る (#3023)。** 複製の URL は保存先が決めるので、
	// 作ってからでないと分からない。**弾いたら片付ける** — その時点で誰からも
	// 参照されない (MIME の再検査と同じ形)。
	if !emojiFileURLFits(src) {
		h.deleteSystemEmojiFile(c.Request().Context(), systemFileID)
		return emojiFileURLTooLong(c)
	}
	// 不変条件 (#722): `emoji.originalUrl` は必ず `drive_file.url` と一致
	// させる。`DriveFileRepository.DeleteOrphans` の guard が
	// `NOT EXISTS (emoji.originalUrl = drive_file.url ...)` で system 所有の
	// 絵文字画像を保護しているので、webpublic を入れると guard が外れる。
	url = src.URL
	publicURL = preferWebpublicURL(src)
	fileType = preferWebpublicType(src)
	now := time.Now()
	e := &model.Emoji{
		ID:          h.idGen.Generate(now),
		UpdatedAt:   &now,
		Name:        req.Name,
		OriginalURL: url,
		PublicURL:   publicURL,
		Type:        fileType,
		Category:    req.Category,
		// 列に入らない alias は要素ごと落とす (#3018)。
		Aliases:     model.StringArray(aliases),
		License:     req.License,
		IsSensitive: req.IsSensitive,
		LocalOnly:   req.LocalOnly,
		// 列に入らない role id は落とす (#3018)。`aliases` と同じ varchar(128)[]。
		RoleIDsThatCanBeUsedThisEmojiAsReaction: model.StringArray(roleIDs),
	}
	if err := h.emojiRepo.Create(e); err != nil {
		// **取り込んだものを片付ける (#2999)。** ここまで来た複製は誰からも
		// 参照されないので、残すと孤児になる。`DeleteOrphans` は
		// `admin/drive/cleanup` からしか走らないので、掃除するまで実体を食う。
		//
		// **ただし載ったかを読み直してから (#3019)。** INSERT が commit 済みで
		// ack だけ失われた場合に消すと、**名前が使用中のまま画像だけ無い**状態に
		// なり、同じ名前で登録し直しても `DUPLICATE_NAME` で弾かれる。
		h.cleanupUnreferencedEmojiCopy(c.Request().Context(), e.ID, systemFileID, e.OriginalURL)
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	h.logModeration(c, moderationlog.LogAddCustomEmoji, map[string]any{
		"emojiId": e.ID,
		"emoji":   e,
	})
	// local emoji の追加を broadcast stream へ流し emoji picker を live-refresh (#2046)。
	h.publishEmojiAdded(e)
	// upstream は EmojiDetailed (packDetailed) を返す。raw model.Emoji を返すと
	// `url` が欠落し originalUrl/publicUrl/uri/type 等の内部 field が漏れる。
	return c.JSON(http.StatusOK, entity.PackEmojiDetailed(e))
}

// emojiCopyFailureResponse maps a CopyToSystemFile failure onto the wire.
//
// **`EMOJI_IMAGE_TOO_LARGE` と 500 は承認経路 (`emojiApplicationReviewError`) と
// 同じ error id を返す。** 同じ複製が同じ理由で失敗したのに endpoint ごとに id が
// 違うと、クライアントは経路ごとに分岐を持つことになる。**`NO_SUCH_FILE` だけは
// 例外**で、upstream が endpoint ごとに別の id を宣言しているため呼び出し元から
// 渡す (下記)。
//
// **種別を潰さない (#2792)。** 全部 500 にすると、選び直せば済むもの
// (実体がもう無い / 大きすぎる) が運用側に直しようのない 5xx になる。逆に全部
// 400 にすると、ストレージや DB の障害が client error に化けて監視でも 5xx が
// 立たない。
//
// **`NO_SUCH_FILE` の id だけは呼び出し元から渡す。** upstream は endpoint ごとに
// 別の id を宣言しており (`add.ts` は `fc46b5a4…`、`update.ts` は `14fb9fd9…`)、
// 共有すると**同じ endpoint が同じ code に 2 つの id を返す**ことになる
// (`update` は drive 行が無いときに自分の id を返すので)。`error.id` で分岐する
// クライアントは片方を取りこぼす。
func emojiCopyFailureResponse(c echo.Context, err error, noSuchFileID string) error {
	switch {
	case errors.Is(err, coredrive.ErrObjectNotFound):
		// 行はあるのに実体が無い (`isLink` / `accessKey` 無し / ストレージから
		// 消えた)。別のファイルを選べば済むので 400。
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"NO_SUCH_FILE", "No such file.", noSuchFileID))
	case errors.Is(err, safehttp.ErrResponseTooLarge):
		// 複製の上限 (32 MiB) を超えている。drive が受け取る上限 (role policy の
		// `maxFileSizeMb`) はこれより上へ設定できるので、**upload はできたのに
		// 絵文字にはできない**帯がありうる。
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"EMOJI_IMAGE_TOO_LARGE", "The image is too large to register as an emoji.",
			"6b1d5f0a-3c9e-4f27-9a4d-7e2b8c1f0d64"))
	}
	return c.JSON(http.StatusInternalServerError, apierr.Error(
		"INTERNAL_ERROR", "Failed to copy emoji image.",
		"c2f7a3d1-58be-4e09-bb26-0d4a9e7f3c15"))
}

// emojiNamePattern mirrors upstream admin/emoji/add の paramDef name pattern。
var emojiNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// allowedEmojiImageTypes mirrors upstream FILE_TYPE_IMAGE (const.ts)。
// image/svg+xml は XSS 理由で意図的に除外する。prefix 判定 ("image/") では
// svg や任意 subtype を通してしまうため、明示 allowlist で完全一致判定する。
// **集合は core/emojiapplication が持つ。** 申請側 (checkFile) と承認側が別々に
// 持つと、片方が prefix 判定になった瞬間に「申請は通るのに承認で落ちる」形に
// なる (レビュー R3)。
func isAllowedEmojiImageTypeShared(mime string) bool {
	return emojiapplication.IsAllowedImageType(mime)
}

// isAllowedEmojiImageType reports whether a drive file MIME may back a custom
// emoji. Empty type is rejected (upstream FILE_TYPE_IMAGE.includes("")===false)。
func isAllowedEmojiImageType(mime string) bool {
	return isAllowedEmojiImageTypeShared(mime)
}

// preferWebpublicURL returns the drive file's webpublic URL when present,
// else its canonical URL (upstream `webpublicUrl ?? url`).
//
// **実体は core/drive が持つ。** 既存データを直すバッチ (#2990) が同じ導出を
// するので、api 層に閉じたままだと再実装になる。
func preferWebpublicURL(f *model.DriveFile) string {
	return coredrive.PreferWebpublicURL(f)
}

// preferWebpublicType returns the drive file's webpublic MIME when present,
// else its canonical type (upstream `webpublicType ?? type`). Returned as a
// pointer so a non-empty value lands in emoji.type (NULL when both empty).
func preferWebpublicType(f *model.DriveFile) *string {
	return coredrive.PreferWebpublicType(f)
}

// EmojiUpdate handles POST /api/admin/emoji/update.
//
// Misskey TS の admin/emoji/update は name/category/aliases に加え license/
// isSensitive/localOnly も受け付ける。フロントの編集ダイアログがこれら全部
// 送信するため Request struct を Misskey 互換に拡張する (#650 問題 2)。
//
// `fileId` 経路は**画像を system 所有の drive ファイルへ複製してから**参照する
// (#3014)。upstream は渡された drive ファイルの URL をそのまま入れるので、意図的な
// 乖離 (docs/divergence/security.md §7)。`admin/emoji/add` の #2999 と同じ形。
//
// Aliases は []string なので nil (省略) と [] (空配列) が型で区別できない。
// Misskey TS 側も optional `?` で undefined と空配列を区別しないため、
// nil != 空配列でない場合に "aliases を全削除" として扱う。実装上は
// `req.Aliases != nil` で「フロントが aliases フィールドを明示送信した」
// と判定しており、空配列送信なら全削除、フィールド欠落なら現状維持。
func (h *Handler) EmojiUpdate(c echo.Context) error {
	var req struct {
		// 本家はanyOfの{name}の枝で通った入力でも、idの値をそのまま
		// CustomEmojiService.updateへ渡し、`data.id ? getEmojiById : getEmojiByName`
		// で真偽値として見る(#3857)。JavaScriptで偽になる値(null・空文字・0・false)
		// ならnameで引き、文字列でない真の値({"id":123,"name":"hello"}など)は当たる
		// 絵文字が無いのでNO_SUCH_EMOJIになる。以前はstringで受けていたので、文字列で
		// ないidをBindのエラーで400にしていた。
		ID          json.RawMessage `json:"id"`
		Name        *string         `json:"name"`
		FileID      *string         `json:"fileId"`
		Category    *string         `json:"category"`
		Aliases     []string        `json:"aliases"`
		License     *string         `json:"license"`
		IsSensitive *bool           `json:"isSensitive"`
		LocalOnly   *bool           `json:"localOnly"`
		RoleIDs     []string        `json:"roleIdsThatCanBeUsedThisEmojiAsReaction"`
	}
	// upstream update.ts paramDef は allOf+anyOf で {id} または {name} の
	// どちらか必須。id 無し name 指定時は local emoji を name で解決する (#1543)。
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "id or name is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	reqID, idIsString := rawJSONString(req.ID)
	idNotString := !idIsString && jsTruthy(req.ID)
	if reqID == "" && !idNotString && (req.Name == nil || *req.Name == "") {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "id or name is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.emojiRepo == nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// upstream update.ts:91-95 は emoji 解決より先に fileId の driveFile を検証し、
	// null なら NO_SUCH_FILE を返す (#1772。emoji 不在 AND fileId 不正が同時のとき
	// upstream は NO_SUCH_FILE を優先する)。ここで解決した file を後段の URL 差し替え
	// でも再利用する。
	var emojiFile *model.DriveFile
	if req.FileID != nil && *req.FileID != "" {
		if h.driveFileRepo == nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
		f, ferr := h.driveFileRepo.FindByID(*req.FileID)
		if ferr != nil && !repository.IsNotFound(ferr) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return apierr.JSONInternalError(c)
		}
		if ferr != nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "No such file.", "14fb9fd9-0731-4e2f-aeb9-f09e4740333d"))
		}
		emojiFile = f
	}
	// before snapshot for moderation log (also used to gate NO_SUCH_EMOJI when
	// the row is missing — UpdateFields の RowsAffected==0 経路は repo 側で
	// ErrRecordNotFound に昇格済み (#650 問題 2)).
	// id 指定があれば id 解決、無ければ name で local emoji を解決する。upstream
	// CustomEmojiService.update は `data.id ? getEmojiById : getEmojiByName` (#1543)。
	var before *model.Emoji
	var err error
	switch {
	case idNotString:
		// 文字列でない id に当たる絵文字は無い。
		err = repository.ErrNotFound
	case reqID != "":
		before, err = h.emojiRepo.FindByID(reqID)
	default:
		before, err = h.emojiRepo.FindByNameAndHost(*req.Name, nil)
	}
	if err != nil || before == nil {
		// #729: 再現報告のため request 側 id と原因 (find vs update path) を
		// log に残す。frontend 側で stale id を握っているのか、repo 側で
		// 何か起きているのか切り分けに使う。
		slog.WarnContext(c.Request().Context(), "EmojiUpdate: NO_SUCH_EMOJI on lookup",
			"id", reqID, "rawId", string(req.ID), "err", err)
		return c.JSON(http.StatusNotFound, apierr.NoSuchEmoji())
	}
	// 以降の UpdateFields / moderation log は解決済み id を使う。
	reqID = before.ID
	// name 変更判定用に旧 name を UpdateFields 前に控える (#2046 broadcast 用。
	// GORM では before は別 struct だが、in-place 更新する実装に依らず安全側)。
	oldName := before.Name
	fields := map[string]any{}
	if req.Name != nil {
		// リネーム時は同名 local emoji の重複を弾く (SAME_NAME_EMOJI_EXISTS)。
		if *req.Name != before.Name {
			// **新しい名前に pattern を掛ける。これは upstream より厳しい。**
			// upstream の paramDef は `anyOf[{id 必須}, {name 必須 + pattern}]`
			// なので、**`id` を渡すと name の pattern は検査されない** —
			// frontend の通常の呼び方がまさにそれ。結果として `:foo bar:` の
			// ような名前が保存でき、AP で broadcast される
			// (`docs/divergence.md` に意図的乖離として記載)。
			//
			// **名前が変わらないときは掛けない。** frontend は名前を編集して
			// いなくても `name` を必ず送るので、掛けると**既に非準拠な名前で
			// 保存されている絵文字のカテゴリやライセンスが編集できなくなる**。
			// そういう行は **TS から引き継いだ DB に残りうる** — upstream の
			// `admin/emoji/update` は `id` を渡す経路で pattern を掛けないので、
			// 非準拠な名前のローカル絵文字が作れてしまう。**AP 経路は根拠にならない**
			// (作るのは remote 行だけで、ここが守るのはローカル)。`EmojiCopy` は
			// #2998 で塞いだので、mk-go が新しく作る経路はもう無い。塞ぎたいのは
			// 「不正な名前を新しく入れる」ことで、既に保存済み・broadcast 済みの
			// 名前を保存し直すのを拒む価値は無い。
			//
			// **長さも見る (#3018)。** pattern は文字種しか縛らないので、129 文字の
			// ASCII 名は素通りして `UpdateFields` が SQLSTATE 22001 で落ちる。
			// `admin/emoji/add` と `copy` は既に同じ値 (`emojiNameMaxRunes`) を見ており、
			// **名前は切らずに弾く** — 切ると別の絵文字になる。
			if !emojiNamePattern.MatchString(*req.Name) || utf8.RuneCountInString(*req.Name) > emojiNameMaxRunes {
				return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid emoji name.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
			}
			// **重複チェックを DB 障害で skip しない** (#2792)。
			dup, derr := h.emojiRepo.FindByNameAndHost(*req.Name, nil)
			if derr != nil && !repository.IsNotFound(derr) {
				return c.JSON(http.StatusInternalServerError, apierr.InternalError())
			}
			if derr == nil && dup != nil && dup.ID != reqID {
				return c.JSON(http.StatusBadRequest, apierr.Error("SAME_NAME_EMOJI_EXISTS", "Emoji with the same name already exists.", "7180fe9d-1ee3-bff9-647d-fe9896d2ffb8"))
			}
		}
		fields["name"] = *req.Name
	}
	// **列に入らない値を複製より前に弾く (#3018)。** `category` varchar(128) /
	// `license` varchar(1024) を超える値はここを抜けると `UpdateFields` が
	// SQLSTATE 22001 で落ち、**利用者の入力で 5xx が立つ**。#3014 より前は
	// `UpdateFields` のエラーを種別を問わず 404 に丸めていたので「そんな絵文字は
	// 無い」に化けていた。
	//
	// **複製より前**でなければならない — 後ろに置くと、400 のたびに誰からも
	// 参照されない system 所有の複製が残る (#2999 / #3014 と同じ理由)。
	// upstream の error 順序 (NO_SUCH_FILE → NO_SUCH_EMOJI) は変えないので、
	// 絵文字の解決より後に置いてある。
	if !emojiBodyFits(req.Category, emojiCategoryMaxRunes) {
		return emojiValueTooLong(c, "category")
	}
	if !emojiBodyFits(req.License, emojiLicenseMaxRunes) {
		return emojiValueTooLong(c, "license")
	}
	// 配列も**複製より前**に確定させる。全部落ちたら弾く (#3018) — 空配列を書くのは
	// 「全消去」なので、落ちた結果を 204 で返すと既存の値を黙って消すことになる。
	aliases, ok := fitEmojiAliases(req.Aliases)
	if !ok {
		return emojiValueTooLong(c, "aliases")
	}
	roleIDs, ok := fitEmojiRoleIDs(req.RoleIDs)
	if !ok {
		return emojiValueTooLong(c, "roleIdsThatCanBeUsedThisEmojiAsReaction")
	}
	// fileId 指定時は drive の画像で URL を差し替える (upstream 互換)。file は
	// emoji 解決前に検証済み (emojiFile)、NO_SUCH_FILE はそこで返している (#1772)。
	systemFileID := ""
	systemFileURL := ""
	if emojiFile != nil {
		f := emojiFile
		if !isAllowedEmojiImageType(f.Type) {
			return c.JSON(http.StatusBadRequest, apierr.Error("UNSUPPORTED_FILE_TYPE", "Unsupported file type.", "f7599d96-8750-af68-1633-9575d625c1a7"))
		}
		// **差し替えた先も system 所有にする (#3014)。** ここに来る `fileId` は
		// ふつうモデレーター自身が直前に上げたファイルで、本人が drive から消せば
		// 絵文字の画像が壊れる (ロールの変更や退会でも同じ)。`admin/emoji/add` は
		// #2999、申請の承認は #2966、リモートの複製と zip の取り込みは #670 で
		// 既に system 所有にしてあり、**REST の経路ではここが最後の 1 つ**だった。
		// 回避策も無い — 画像を選ぶ画面が叩く `drive/files` は呼び出し元自身の
		// ファイルしか返さないので、「system 所有のファイルを選ぶ」ことはできない。
		//
		// **差し替える前に指していたファイルは消さない。** 元が既に system 所有なら
		// 複製しない規則 (#2999) がある以上、同じファイルを別の絵文字が指している
		// 可能性があり、`EmojiRepository` に `originalUrl` の完全一致引きは無く
		// (`ListV2` が持つのは `publicUrl` の部分一致だけ)、足しても「引いた後・
		// 消す前」のレースは残る。`admin/emoji/delete` も絵文字の行だけ消して
		// drive ファイルは残す (`admin/drive/cleanup` が回収する) ので、ここだけ
		// 消すと経路ごとに後始末の意味が変わる。孤児 guard (`orphanWhere`) は
		// `emoji.originalUrl = drive_file.url` または `publicUrl = url` で参照を
		// 判定するので、**参照が外れたものだけ**が回収対象になり、共有中のものは
		// 守られる。元が利用者
		// 所有ならそもそも触ってはいけない (ノートの添付やプロフィールで使っている
		// 可能性がある)。代償は「cleanup を回すまで実体を食う」ことだけで、これは
		// `admin/emoji/delete` が既に持っている性質と同じ。
		//
		// 検証をすべて通した後に複製する / 複製した実体の MIME をもう一度見る /
		// 行の `size` で先に断る、はどれも `EmojiAdd` と同じ理由 (#2999)。
		src := f
		if h.emojiImageFetcher != nil && !coredrive.IsSystemOwned(f) {
			// **読む前に行の `size` で断る。** 複製は実体を丸ごとメモリへ読むので、
			// 上限超過を読み切ってから 400 にするのは無駄。過小申告は読み出し側の
			// `safehttp.ErrResponseTooLarge` が捕まえる。
			if int64(f.Size) > MaxEmojiCopyBytes {
				return c.JSON(http.StatusBadRequest, apierr.Error(
					"EMOJI_IMAGE_TOO_LARGE", "The image is too large to register as an emoji.",
					"6b1d5f0a-3c9e-4f27-9a4d-7e2b8c1f0d64"))
			}
			// **複製に渡すのは更新後の値。** 同じリクエストで `name` /
			// `isSensitive` を変えていればそちらが載るので、省略されたときだけ
			// 現在の絵文字の値を使う。
			copyName := before.Name
			if req.Name != nil {
				copyName = *req.Name
			}
			copySensitive := before.IsSensitive
			if req.IsSensitive != nil {
				copySensitive = *req.IsSensitive
			}
			ctx := c.Request().Context()
			copied, cerr := h.emojiImageFetcher.CopyToSystemFile(ctx, f, copyName, copySensitive)
			if cerr == nil && copied == nil {
				// 実装の契約違反。ここで気付かないと下の `src` 参照で panic する。
				cerr = errors.New("copy returned no file")
			}
			if cerr != nil {
				slog.WarnContext(ctx, "emoji update: drive copy failed",
					"emojiId", reqID, "fileId", *req.FileID, "err", cerr)
				// **絵文字を書き換えない。** 元ファイルを指して更新すると、直そうと
				// している依存をそのまま作ることになる。差し替え前の画像は生きて
				// いるので、失敗しても表示は壊れない。
				// **`NO_SUCH_FILE` は `update.ts` の id で返す** — 上の drive 行が
				// 無いときと同じ endpoint・同じ code なので、id を分けると
				// クライアントが片方を取りこぼす。
				return emojiCopyFailureResponse(c, cerr, "14fb9fd9-0731-4e2f-aeb9-f09e4740333d")
			}
			// **複製した実体の MIME を見る (#2966 と同じ理由)。** `Upload` は
			// `AnalyseFile` でバイト列から型を引き直すので、行の宣言と実体が
			// ずれていると allowlist 外の型が絵文字として登録される。
			// **弾いたら片付ける** — その時点で誰からも参照されない。
			if !isAllowedEmojiImageType(copied.Type) {
				h.deleteSystemEmojiFile(ctx, copied.ID)
				return c.JSON(http.StatusBadRequest, apierr.Error("UNSUPPORTED_FILE_TYPE", "Unsupported file type.", "f7599d96-8750-af68-1633-9575d625c1a7"))
			}
			src = copied
			systemFileID = copied.ID
			systemFileURL = copied.URL
		}
		// **URL が列に入るかを見る (#3023)。** 弾いたら複製を片付ける。
		if !emojiFileURLFits(src) {
			h.deleteSystemEmojiFile(c.Request().Context(), systemFileID)
			return emojiFileURLTooLong(c)
		}
		// upstream update.ts: originalUrl=url, publicUrl=webpublicUrl??url,
		// fileType=webpublicType??type。EmojiAdd / EmojiCopy と同ロジック。
		//
		// 不変条件 (#722): `originalUrl` は必ず `drive_file.url` と一致させる。
		// 孤児 cleanup の guard が `NOT EXISTS (emoji.originalUrl = drive_file.url
		// ...)` で system 所有の絵文字画像を保護しているので、webpublic を入れると
		// guard が外れる。
		fields["originalUrl"] = src.URL
		fields["publicUrl"] = preferWebpublicURL(src)
		if t := preferWebpublicType(src); t != nil {
			fields["type"] = *t
		}
	}
	if req.Category != nil {
		fields["category"] = *req.Category
	}
	if req.Aliases != nil {
		// `[]string` を直接 GORM に渡すと PostgreSQL の varchar[] 列に対して
		// NULL として serialize される (#729)。`model.StringArray` で wrap する
		// と空 slice 含めて `'{}'` PostgreSQL array リテラルに正しく変換さ
		// れる。Aliases 列は NOT NULL DEFAULT '{}' なので NULL 書き込みは
		// 即制約違反でエラーになっていた。
		// 正規化は上で済ませてある。**nil にはしない** — `req.Aliases != nil` が
		// 「明示送信した」の判定なので、nil に戻すと「省略」に化ける。
		fields["aliases"] = model.StringArray(aliases)
	}
	if req.License != nil {
		fields["license"] = *req.License
	}
	if req.IsSensitive != nil {
		fields["isSensitive"] = *req.IsSensitive
	}
	if req.LocalOnly != nil {
		fields["localOnly"] = *req.LocalOnly
	}
	// リアクション利用可能ロールの更新 (upstream の roleIdsThatCanBeUsedThisEmojiAsReaction)。
	if req.RoleIDs != nil {
		fields["roleIdsThatCanBeUsedThisEmojiAsReaction"] = model.StringArray(roleIDs)
	}
	if len(fields) == 0 {
		// 何も変更しないリクエストは log を書かずに 204 で返す。
		return c.NoContent(http.StatusNoContent)
	}
	if err := h.emojiRepo.UpdateFields(reqID, fields); err != nil {
		if !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない (#2792)。** 接続断が「そんな絵文字は
			// 無い」に化けると、クライアントからは区別できず監視でも 5xx が立たない。
			slog.ErrorContext(c.Request().Context(), "EmojiUpdate: UpdateFields failed",
				"id", reqID, "fields", fieldKeys(fields), "systemFileId", systemFileID, "err", err)
			h.cleanupUnreferencedEmojiCopy(c.Request().Context(), reqID, systemFileID, systemFileURL)
			return apierr.JSONInternalError(c)
		}
		// #729: FindByID 直後の RowsAffected==0 はほぼ起こらない (concurrent
		// delete 等のレース) ので診断 log で気付けるようにする。
		slog.WarnContext(c.Request().Context(), "EmojiUpdate: NO_SUCH_EMOJI on UpdateFields",
			"id", reqID, "fields", fieldKeys(fields), "err", err)
		// **載らなかった複製を片付ける (#3014)。** 行がもう無い (= `RowsAffected==0`
		// の昇格を含む) ので更新は載っておらず、絵文字は差し替え前の URL を指した
		// まま。作った複製は誰からも参照されない孤児として残る。
		h.deleteSystemEmojiFile(c.Request().Context(), systemFileID)
		return c.JSON(http.StatusNotFound, apierr.NoSuchEmoji())
	}
	after, err := h.emojiRepo.FindByID(reqID)
	if err != nil {
		// ここに到達するのは UpdateFields 直後の row 消失 (race) のみ。
		// 操作自体は成功しているので 204 を返す。
		return c.NoContent(http.StatusNoContent)
	}
	h.logModeration(c, moderationlog.LogUpdateCustomEmoji, map[string]any{
		"emojiId": reqID,
		"before":  before,
		"after":   after,
	})
	// broadcast stream へ流す (#2046)。upstream は name 変更時のみ
	// emojiDeleted(old)+emojiAdded(new)、それ以外は emojiUpdated(after)。
	if req.Name != nil && *req.Name != oldName {
		h.publishEmojiDeleted(before)
		h.publishEmojiAdded(after)
	} else {
		h.publishEmojiUpdated(after)
	}
	return c.NoContent(http.StatusNoContent)
}

// cleanupUnreferencedEmojiCopy deletes a system-owned copy unless the emoji row
// ended up referencing it (#3014 / #3019).
//
// **エラーが返っても書き込みが載っていることがある。** PostgreSQL は COMMIT を送った
// 後・ack が届く前に接続が切れると、サーバー側は commit 済みなのにクライアントは
// エラーを受け取る。載っているのに複製を消すと**絵文字が存在しないファイルを指す** —
// 差し替え (`update`) なら差し替え前の画像はもう参照されておらず戻らないし、
// 登録 (`add` / 承認) なら**名前が使用中のまま画像だけ無い**状態になり、同じ名前で
// 登録し直しても `DUPLICATE_NAME` で弾かれる (承認では申請が pending のまま残り、
// もう一度承認を押しても同じ理由で通らない)。
//
// **逆に、確実に載っていない失敗では孤児になる。** 列幅超過 (SQLSTATE 22001) や
// 一意制約違反はそれで、`admin/drive/cleanup` は手動でしか走らないので掃除するまで
// 実体を食う。
//
// **読み直して分ける。** 後始末バッチ (#2990) の `finishFailedUpdate` と同じ
// 3 分岐で、**読み直せないときは残す** — 参照されている複製を消すほうが、
// 参照されない複製を残すより悪い (後者は孤児 cleanup が回収する)。
//
// **承認の `approvalLanded` (#2966) は逆に倒している (読めなければ消す)。**
// 理由は「復旧できるか」ではなく**消せる範囲が違う**こと。あちらの後始末
// (`DeleteCreatedEmoji`) は**絵文字の行ごと**消せるので、消せば承認を押し直して
// 作り直せる (`TestApproveCleansUpWhenReadBackFails` がその判断を固定している)。
// こちらが決められるのは**複製の行方だけ**で、絵文字の行には触らない — 選べるのは
// 「画像が生きた絵文字」か「画像だけ 404 の絵文字」で、後者を選ぶ理由が無い。
//
// **「残せば復旧できる」ではない。** 承認経路では `Create` が失敗した時点で
// `Approve` が return するので申請は pending のまま残り、**そのまま押し直すと
// 複製を残しても消しても `DUPLICATE_NAME`**。差がつくのは画像が生きているか
// どうかだけ。
//
// **復旧はできる。** 審査画面は衝突している絵文字の id を出す
// (`packEmojiApplicationForModerator` の `nameConflict`) ので、それを
// `admin/emoji/delete` で消せば承認を押し直せる。**却下に倒さないこと** —
// 申請者の枠を消費したまま閉じることになる。
//
// **`copiedURL` で照合する。** 登録 (`add` / 承認) は id が新しいので「行があるか」
// だけでも判定できるが、差し替えは行が元からあるので URL を見ないと分からない。
// どちらの経路も `emoji.originalUrl` には複製の `url` をそのまま入れる (#722) ので、
// 同じ述語で足りる。
func (h *Handler) cleanupUnreferencedEmojiCopy(ctx context.Context, emojiID, fileID, copiedURL string) {
	// **`copiedURL` が空なら照合にならない** ので触らない (空同士が「一致」に見える)。
	// **現状の storage 実装では到達しない** — `Put` は成功すれば必ず非空の URL を
	// 返す。防御的に置いてあるだけで、倒す向きは「消さない」。後始末バッチの
	// `unrepairableFromRow` が同じ曖昧さを理由に複製対象から外している。
	if fileID == "" || copiedURL == "" {
		return
	}
	after, err := h.emojiRepo.FindByID(emojiID)
	switch {
	case err != nil && !repository.IsNotFound(err):
		// 読み直せないので載ったか分からない。残す。
		slog.WarnContext(ctx, "emoji: cannot tell whether the write landed; keeping the system copy",
			"emojiId", emojiID, "systemFileId", fileID, "err", err)
		return
	case err == nil && after != nil && after.OriginalURL == copiedURL:
		// **エラーは返ったが書き込みは載っていた。** 複製は参照されているので残す。
		//
		// **ログに残す。** 呼び出し元は 5xx を返すのに絵文字は実在する、という
		// 食い違いが起きている。運用側はこれを見ないと「失敗したはずなのに
		// `DUPLICATE_NAME` になる」の原因に辿り着けない。
		slog.WarnContext(ctx, "emoji: the write landed despite the error; keeping the system copy",
			"emojiId", emojiID, "systemFileId", fileID)
		return
	}
	// 行が無い / 別の URL を指している = 載っていない。複製は誰からも参照されない。
	h.deleteSystemEmojiFile(ctx, fileID)
}

// fieldKeys は map のキー一覧を sort 済み slice で返す診断 log 用 helper。
// log 行に value を直接含めると long string が混じって読みにくいので key
// のみ列挙する。
func fieldKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// EmojiDelete handles POST /api/admin/emoji/delete.
func (h *Handler) EmojiDelete(c echo.Context) error {
	var req struct {
		ID string `json:"id"`
	}
	if err := c.Bind(&req); err != nil || req.ID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "id is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.emojiRepo == nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// log info に snapshot を含めるため削除前に取得。取得失敗は NO_SUCH_EMOJI。
	snapshot, err := h.emojiRepo.FindByID(req.ID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_EMOJI", "No such emoji.", "be83669b-773a-44b7-b1f8-e5e5170ac3c2"))
	}
	if err := h.emojiRepo.Delete(req.ID); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_EMOJI", "No such emoji.", "be83669b-773a-44b7-b1f8-e5e5170ac3c2"))
	}
	h.logModeration(c, moderationlog.LogDeleteCustomEmoji, map[string]any{
		"emojiId": req.ID,
		"emoji":   snapshot,
	})
	// 削除を broadcast stream へ流し emoji picker から消す (#2046)。
	h.publishEmojiDeleted(snapshot)
	return c.NoContent(http.StatusNoContent)
}

// jsTruthy reports whether raw, a JSON value, is truthy in JavaScript: false
// for an absent value, null, false, "" and any zero number (0, -0, 0.0, 1e-400
// after underflow), true for everything else (other numbers, true, non-empty
// strings, objects and arrays, empty ones included).
func jsTruthy(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return false
	}
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		// ParseFloatはJavaScriptと同じく、範囲外を±Inf(真)、下へ溢れた値を0(偽)にする。
		f, _ := strconv.ParseFloat(string(x), 64)
		return f != 0
	default:
		return true
	}
}

// EmojiList handles POST /api/admin/emoji/list.
func (h *Handler) EmojiList(c echo.Context) error {
	var req struct {
		Query string `json:"query"`
		// categoryは本家のparamDefに無いElythia独自の引数(#3857)。#3867で
		// string(null可)として本家のparamDefに重ねて宣言したので、文字列でない
		// 値は検証が先に400で拒否する。検証しない入力(上限を超えたbodyなど)の
		// ために、ここでは文字列でない値を絞り込まずに返す扱いを残す。
		Category  json.RawMessage `json:"category"`
		SinceID   string          `json:"sinceId"`
		UntilID   string          `json:"untilId"`
		SinceDate *jsnum.Int      `json:"sinceDate"`
		UntilDate *jsnum.Int      `json:"untilDate"`
		Limit     *jsnum.Int      `json:"limit"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.emojiRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursorIDFirst(req.SinceID, req.UntilID, req.SinceDate.Int64Ptr(), req.UntilDate.Int64Ptr())
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit.IntPtr(), 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// 本家のparamDefにoffsetは無いので読まない(#3857)。本家が無視する値で件数を
	// 飛ばし、型の違う値を400にしていた。同梱のfrontendはuntilIdで送るので影響しない。
	category, _ := rawJSONString(req.Category)
	emojis, err := h.emojiRepo.ListWithFilter(req.Query, category, true, sinceID, untilID, limit, 0)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	return c.JSON(http.StatusOK, entity.PackEmojiDetailedList(emojis))
}

// EmojiListV2 handles POST /api/v2/admin/emoji/list.
// Returns an object with pagination info (allCount, allPages) instead of a plain array.
func (h *Handler) EmojiListV2(c echo.Context) error {
	var req struct {
		Query     *emojiV2QueryReq `json:"query"`
		SinceID   string           `json:"sinceId"`
		UntilID   string           `json:"untilId"`
		SinceDate *jsnum.Int       `json:"sinceDate"`
		UntilDate *jsnum.Int       `json:"untilDate"`
		Limit     *jsnum.Int       `json:"limit"`
		Page      jsnum.Int        `json:"page"`
		SortKeys  []string         `json:"sortKeys"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.emojiRepo == nil {
		return c.JSON(http.StatusOK, emojiListV2Response{
			Emojis:   []map[string]any{},
			Count:    0,
			AllCount: 0,
			AllPages: 0,
		})
	}

	limit, limitOK := pagination.ResolveLimit(req.Limit.IntPtr(), 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}

	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate.Int64Ptr(), req.UntilDate.Int64Ptr())
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	// **`CAST("updatedAt" AS DATE)` との比較にそのまま渡る。** 日付として読めない
	// 値は PostgreSQL の型変換で落ちて 500 になる (本家も paramDef が string だけで
	// 同じ)。呼び出し側の誤りなので引く前に 400 で返す (#3518)。
	if req.Query != nil && (!repository.ValidEmojiUpdatedAtFilter(req.Query.UpdatedAtFrom) || !repository.ValidEmojiUpdatedAtFilter(req.Query.UpdatedAtTo)) {
		return apierr.JSONInvalidParam(c)
	}
	filter := model.EmojiV2Filter{
		SinceID:  sinceID,
		UntilID:  untilID,
		Limit:    limit,
		Page:     int(req.Page),
		SortKeys: req.SortKeys,
	}
	if req.Query != nil {
		filter.Query = &model.EmojiV2Query{
			Name:          req.Query.Name,
			Host:          req.Query.Host,
			HostType:      req.Query.HostType,
			Category:      req.Query.Category,
			Type:          req.Query.Type,
			Aliases:       req.Query.Aliases,
			License:       req.Query.License,
			IsSensitive:   req.Query.IsSensitive,
			LocalOnly:     req.Query.LocalOnly,
			UpdatedAtFrom: req.Query.UpdatedAtFrom,
			UpdatedAtTo:   req.Query.UpdatedAtTo,
			RoleIDs:       req.Query.RoleIDs,
			URI:           req.Query.URI,
			PublicURL:     req.Query.PublicURL,
			OriginalURL:   req.Query.OriginalURL,
		}
	}

	emojis, err := h.emojiRepo.ListV2(filter)
	if err != nil {
		slog.Error("EmojiListV2: ListV2 failed", "error", err)
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	allCount, err := h.emojiRepo.CountV2(filter)
	if err != nil {
		slog.Error("EmojiListV2: CountV2 failed", "error", err)
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	allPages := 0
	if limit > 0 {
		allPages = int((allCount + int64(limit) - 1) / int64(limit))
	}

	roleByID := h.emojiRoleByID()
	packed := make([]map[string]any, 0, len(emojis))
	for _, e := range emojis {
		packed = append(packed, packEmojiDetailedAdmin(e, roleByID))
	}

	return c.JSON(http.StatusOK, emojiListV2Response{
		Emojis:   packed,
		Count:    len(packed),
		AllCount: allCount,
		AllPages: allPages,
	})
}

type emojiV2QueryReq struct {
	Name          string   `json:"name"`
	Host          string   `json:"host"`
	HostType      string   `json:"hostType"`
	Category      string   `json:"category"`
	Type          string   `json:"type"`
	Aliases       string   `json:"aliases"`
	License       string   `json:"license"`
	IsSensitive   *bool    `json:"isSensitive"`
	LocalOnly     *bool    `json:"localOnly"`
	UpdatedAtFrom string   `json:"updatedAtFrom"`
	UpdatedAtTo   string   `json:"updatedAtTo"`
	RoleIDs       []string `json:"roleIds"`
	// URI / PublicURL / OriginalURL は upstream Misskey の v2 検索 schema
	// で受け付けている filter (#466)。frontend (custom-emojis-manager.remote
	// .vue) の検索ボックスから送られてくるため、handler 側で受けないと
	// silently 無視される。受信したら repo の WHERE 句に展開する。
	URI         string `json:"uri"`
	PublicURL   string `json:"publicUrl"`
	OriginalURL string `json:"originalUrl"`
}

type emojiListV2Response struct {
	Emojis   []map[string]any `json:"emojis"`
	Count    int              `json:"count"`
	AllCount int64            `json:"allCount"`
	AllPages int              `json:"allPages"`
}

// emojiRoleByID resolves all roles by id once, so the v2 emoji packer can embed
// {id, name} for roleIdsThatCanBeUsedThisEmojiAsReaction without an N+1 lookup
// per emoji (#1300), and sort them by displayOrder/id like upstream (#1948-13)。
// roleService 未配線 / 取得失敗時は空 map を返し、未知 role 同様 roleIds から省かれる。
func (h *Handler) emojiRoleByID() map[string]*model.Role {
	out := map[string]*model.Role{}
	if h.roleService == nil {
		return out
	}
	roles, err := h.roleService.List()
	if err != nil {
		return out
	}
	for _, r := range roles {
		out[r.ID] = r
	}
	return out
}

// packEmojiDetailedAdmin builds the golden EmojiDetailedAdmin shape for the v2
// admin emoji list. upstream packDetailedAdmin と同じく roleIds を {id, name}
// に解決する (golden は object array、生の string[] ではない)。未知 role は
// 省く (#1300)。scalar field は model.Emoji の json tag と一致する。
func packEmojiDetailedAdmin(e *model.Emoji, roleByID map[string]*model.Role) map[string]any {
	// upstream packDetailedAdmin は解決した role を displayOrder DESC, id ASC で
	// sort してから {id, name} に map する (EmojiEntityService、#1948-13)。
	resolved := make([]*model.Role, 0, len(e.RoleIDsThatCanBeUsedThisEmojiAsReaction))
	for _, rid := range e.RoleIDsThatCanBeUsedThisEmojiAsReaction {
		if r, ok := roleByID[rid]; ok {
			resolved = append(resolved, r)
		}
	}
	sort.SliceStable(resolved, func(i, j int) bool {
		if resolved[i].DisplayOrder != resolved[j].DisplayOrder {
			return resolved[i].DisplayOrder > resolved[j].DisplayOrder // displayOrder DESC
		}
		return resolved[i].ID < resolved[j].ID // id ASC
	})
	roles := make([]map[string]any, 0, len(resolved))
	for _, r := range resolved {
		roles = append(roles, map[string]any{"id": r.ID, "name": r.Name})
	}
	// golden aliases は string[] 必須 (non-null)。nil model.StringArray は null に
	// なるため [] へ coalesce する。
	aliases := []string(e.Aliases)
	if aliases == nil {
		aliases = []string{}
	}
	return map[string]any{
		"id": e.ID,
		// upstream EmojiEntityService.ts:112 は updatedAt?.toISOString() ?? null。
		// raw *time.Time だと RFC3339Nano になるため .000Z/null に揃える (#1948-10)。
		"updatedAt":   entity.ISOMillisPtr(e.UpdatedAt),
		"name":        e.Name,
		"host":        e.Host,
		"publicUrl":   e.PublicURL,
		"originalUrl": e.OriginalURL,
		"uri":         e.URI,
		"type":        e.Type,
		"aliases":     aliases,
		"category":    e.Category,
		"license":     e.License,
		"localOnly":   e.LocalOnly,
		"isSensitive": e.IsSensitive,
		"roleIdsThatCanBeUsedThisEmojiAsReaction": roles,
	}
}
